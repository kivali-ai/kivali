//! A fake `kivali-supervisor` for developing and demonstrating the shell
//! without a VM. It speaks the real supervisor's RPC (HTTP/1.1 over the
//! owner-only endpoint: the Unix socket `<dir>/supervisor.sock`, or on
//! Windows the named pipe `\\.\pipe\kivali-<hash>` of the directory,
//! `pipe.rs`; NDJSON operation streams; the shell's own `wire.rs`,
//! shared by path) and its command line, pretends to create
//! and boot an org with a stream of log lines, and serves a stand-in org
//! page plus `GET /api/v1/login` on the org's port so the shell has
//! something to load and Connect has something to check. `POST
//! /v1/handoff` answers a fixed fake token, which the stand-in page's
//! `/auth/handoff` redirects to `/` without checking.
//!
//!   kivali-supervisor [--config-dir DIR] serve [--vm-dir D]
//!   kivali-supervisor [--config-dir DIR] terminal
//!   kivali-supervisor [--config-dir DIR] backup --out FILE
//!   kivali-supervisor version
//!
//! Environment:
//!   FAKE_PORT       the org's port (default 18080)
//!   FAKE_STEP_MS    delay per log line (default 600)
//!   FAKE_CHECK      up-to-date | upgrade-available (default) |
//!                   desktop-update-required | manual-steps | failed
//!   FAKE_FAIL_UP    set to fail the first `up` part-way
//!   FAKE_SIGNED_IN  1 starts with Claude signed in, as the owner (it is
//!                   otherwise not signed in)
//!   FAKE_BILLING    what a signed-in Claude bills, as `claude auth
//!                   status` would name it (default "Claude Max")
//!   FAKE_SETUP_MISSING_MODELS  model ids (comma-separated) a sign-in
//!                   setup's model check reports missing
//!   FAKE_SETUP_REFUSED  1 makes every model check a refused sign-in
//!
//! It never opens a terminal session itself, so `terminals` stays 0.
//!
//! Each connection carries one request and closes after its answer
//! (`Connection: close`), on either transport, as the real serve's do;
//! `down --exit` and `destroy --exit` end the process once their last
//! line is out.

#[path = "../../src-tauri/src/supervisor/wire.rs"]
#[allow(dead_code)]
mod wire;

#[cfg(any(windows, test))]
mod pipe;

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;
#[cfg(unix)]
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use wire::*;

const SHIPPED: &str = "0.16.0";
const LATEST: &str = "0.17.0";

fn env_or<T: std::str::FromStr>(k: &str, d: T) -> T {
    std::env::var(k).ok().and_then(|v| v.parse().ok()).unwrap_or(d)
}

#[derive(serde::Serialize, serde::Deserialize, Clone, Default)]
struct Persisted {
    kivali: String,
    owner: String,
    port: u16,
    formatted: bool,
}

struct Fake {
    state_file: PathBuf,
    persisted: Mutex<Persisted>,
    running: AtomicBool,
    busy: AtomicBool,
    http_started: AtomicBool,
    serving: Arc<AtomicBool>,
    last_ok: Mutex<Option<CheckResult>>,
    step: Duration,
    quit: AtomicBool,
    /// Whether the org's Claude is signed in, in memory only.
    signed_in: AtomicBool,
    /// The sign-in setup applied (its id and values without secrets), in
    /// memory only; it outranks `signed_in` until a clear.
    setup: Mutex<Option<SavedSetup>>,
}

/// The models the fake's org runs, as the real catalog names them.
const MODELS: &[&str] = &["claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5-5", "claude-fable-5-1"];

/// A sign-in setup the fake accepts, as test data: what the real Claude
/// driver declares, reduced to what the fake needs to answer like it.
struct FakeSetup {
    id: &'static str,
    /// The billing line's provider words.
    provider: &'static str,
    /// The value that names the setup's target (the billing line's
    /// suffix); it must be a bare name.
    target: &'static str,
    /// Values never read back.
    secrets: &'static [&'static str],
    /// A missing model's words; {model} and {target} are filled in.
    missing: &'static str,
}

const SETUPS: &[FakeSetup] = &[FakeSetup {
    id: "microsoft-foundry",
    provider: "Microsoft Foundry",
    target: "resource",
    secrets: &["api_key", "client_secret"],
    missing: "No deployment named {model} in {target}",
}];

/// What the fake data disk "occupies" and its size, once it exists.
const DISK_USED: u64 = 3 << 30;
const DISK_SIZE: u64 = 64 << 30;

impl Fake {
    fn report(&self) -> Report {
        let p = self.persisted.lock().unwrap().clone();
        let running = self.running.load(Ordering::SeqCst);
        Report {
            running,
            forward: running.then(|| format!("127.0.0.1:{}", p.port)),
            url: running.then(|| format!("http://127.0.0.1:{}/", p.port)),
            guest: running.then(|| GuestStatus { state: "ready".into(), fatal: None, node_ready: true }),
            guest_error: None,
            state: LocalState {
                kivali: p.kivali,
                data_disk_formatted: p.formatted,
                port: p.port,
                last_check: None,
                last_check_ok: false,
                latest: self.last_ok.lock().unwrap().clone(),
                upgrade: None,
            },
            supervisor_version: Some("fake".into()),
            busy: self.busy.load(Ordering::SeqCst),
            terminals: 0,
            disk_used_bytes: if p.formatted { DISK_USED } else { 0 },
            disk_size_bytes: if p.formatted { DISK_SIZE } else { 0 },
        }
    }

    /// Why the org cannot answer about its credential, if it cannot.
    fn not_ready(&self) -> Option<&'static str> {
        if !self.running.load(Ordering::SeqCst) {
            Some("the VM is not running; start the org first")
        } else if self.persisted.lock().unwrap().kivali.is_empty() {
            Some("Kivali is not installed yet; finish setting up the org first")
        } else {
            None
        }
    }

    fn credential_status(&self) -> CredentialStatus {
        if let Some(saved) = self.setup.lock().unwrap().clone() {
            let s = SETUPS.iter().find(|s| s.id == saved.setup).expect("only known setups are saved");
            let target = saved.values.get(s.target).cloned().unwrap_or_default();
            return CredentialStatus {
                signed_in: true,
                email: None,
                billing: Some(format!("{} · {target}", s.provider)),
                checked_at: now_rfc3339(),
            };
        }
        let signed_in = self.signed_in.load(Ordering::SeqCst);
        let owner = self.persisted.lock().unwrap().owner.clone();
        CredentialStatus {
            signed_in,
            email: signed_in.then_some(owner).filter(|o| !o.is_empty()),
            billing: signed_in.then(|| std::env::var("FAKE_BILLING").ok().filter(|b| !b.is_empty()).unwrap_or_else(|| "Claude Max".into())),
            checked_at: now_rfc3339(),
        }
    }

    /// Applies a sign-in setup, refusing what the real driver refuses
    /// first (an unknown setup, a target that is not a bare name).
    /// FAKE_SETUP_MISSING_MODELS (comma-separated model ids) names the
    /// models the check reports missing; FAKE_SETUP_REFUSED=1 makes the
    /// provider refuse every check.
    fn apply_setup(&self, body: &[u8]) -> Result<SetupResult, String> {
        let r: SetupRequest = serde_json::from_slice(body).map_err(|_| "bad request: the body is not a sign-in setup".to_string())?;
        let s = SETUPS.iter().find(|s| s.id == r.setup).ok_or_else(|| format!("unknown sign-in setup {:?}", r.setup))?;
        let target = r.values.get(s.target).map(|v| v.trim().to_string()).unwrap_or_default();
        if target.is_empty() || !target.chars().all(|c| c.is_ascii_alphanumeric() || c == '-') {
            return Err(format!("enter the {} only, like my-{}, not a URL", s.target, s.target));
        }
        let mut values = r.values.clone();
        values.retain(|k, v| !s.secrets.contains(&k.as_str()) && !v.trim().is_empty());
        values.insert(s.target.to_string(), target.clone());
        *self.setup.lock().unwrap() = Some(SavedSetup { setup: s.id.to_string(), values });
        let missing = std::env::var("FAKE_SETUP_MISSING_MODELS").unwrap_or_default();
        let refused = std::env::var("FAKE_SETUP_REFUSED").is_ok_and(|v| v == "1");
        let models = MODELS
            .iter()
            .map(|m| {
                let gone = missing.split(',').any(|x| x.trim() == *m);
                ModelCheck {
                    model: m.to_string(),
                    ok: !refused && !gone,
                    missing: !refused && gone,
                    problem: if refused {
                        Some("the provider refused the sign-in (HTTP 401)".into())
                    } else if gone {
                        Some(s.missing.replace("{model}", m).replace("{target}", &target))
                    } else {
                        None
                    },
                }
            })
            .collect();
        Ok(SetupResult { credential: self.credential_status(), models })
    }

    fn destroy(&self, out: &mut dyn Write) -> Result<Option<serde_json::Value>, String> {
        let mut lines = vec![];
        if self.running.load(Ordering::SeqCst) {
            lines.extend(["clean shutdown requested through the guest agent", "VM stopped; data disk unmounted cleanly"]);
        }
        let formatted = self.persisted.lock().unwrap().formatted;
        lines.extend(["deleting the data disk", "deleting local.json and the logs"]);
        let _ = self.logs(out, stage::DELETING, &lines);
        self.running.store(false, Ordering::SeqCst);
        self.serving.store(false, Ordering::SeqCst);
        if let Err(e) = std::fs::remove_file(&self.state_file) {
            if e.kind() != std::io::ErrorKind::NotFound {
                return Err(format!("delete {}: {e}", self.state_file.display()));
            }
        }
        *self.persisted.lock().unwrap() = Persisted::default();
        self.signed_in.store(false, Ordering::SeqCst);
        let freed = if formatted { DISK_USED } else { 0 };
        Ok(Some(serde_json::to_value(DestroyResult { freed_bytes: freed }).unwrap()))
    }

    fn save(&self) {
        let p = self.persisted.lock().unwrap().clone();
        let _ = std::fs::write(&self.state_file, serde_json::to_vec_pretty(&p).unwrap());
    }

    fn logs(&self, out: &mut dyn Write, stage: &str, lines: &[&str]) -> std::io::Result<()> {
        for l in lines {
            let ev = Event { log: Some(l.to_string()), stage: Some(stage.to_string()), ..Default::default() };
            out.write_all(encode_event(&ev).as_bytes())?;
            out.flush()?;
            std::thread::sleep(self.step);
        }
        Ok(())
    }

    fn start_http(&self) {
        self.serving.store(true, Ordering::SeqCst);
        if self.http_started.swap(true, Ordering::SeqCst) {
            return;
        }
        let p = self.persisted.lock().unwrap().clone();
        let serving = self.serving.clone();
        std::thread::spawn(move || serve_org(p.port, serving, p.owner));
    }

    fn up(&self, out: &mut dyn Write, o: UpOptions) -> Result<Option<serde_json::Value>, String> {
        let creating = self.persisted.lock().unwrap().kivali.is_empty();
        if creating && !o.prepare && o.install.owner.trim().is_empty() {
            return Err("Kivali is not installed and no owner was given: pass the owner's Google account".into());
        }
        let already = self.running.load(Ordering::SeqCst);
        if already && !creating {
            let _ = self.logs(out, stage::STARTING, &["the VM is already running"]);
            return Ok(Some(serde_json::to_value(self.report()).unwrap()));
        }
        let formatted = self.persisted.lock().unwrap().formatted;
        if already {
            let _ = self.logs(out, stage::STARTING, &["the VM is already running"]);
        } else if !formatted {
            let _ = self.logs(out, stage::MAKING_ROOM, &["creating the data disk", "VM started; first boot, the guest formats the data disk", "KIVALI-VM READY"]);
        } else {
            let _ = self.logs(out, stage::STARTING, &["VM started", "KIVALI-VM READY"]);
        }
        {
            let mut p = self.persisted.lock().unwrap();
            p.formatted = true;
            if p.port == 0 {
                p.port = if o.port != 0 {
                    o.port
                } else if o.port_hint != 0 {
                    o.port_hint
                } else {
                    env_or("FAKE_PORT", 18080)
                };
            } else if o.port != 0 {
                p.port = o.port;
            }
            // As the real supervisor does, move off a port another program
            // holds (the fake's own server, once started, keeps its port).
            if o.port == 0 && !self.http_started.load(Ordering::SeqCst) {
                p.port = free_port_from(p.port);
            }
        }
        self.save();
        if !already {
            let _ = self.logs(out, stage::STARTING, &["guest agent ready", "forwarding the org's port"]);
        }
        if creating && o.prepare {
            let _ = self.logs(out, stage::STARTING, &["the VM is ready; Kivali installs once its owner is known"]);
            self.running.store(true, Ordering::SeqCst);
            return Ok(Some(serde_json::to_value(self.report()).unwrap()));
        }
        if creating {
            if std::env::var_os("FAKE_FAIL_UP").is_some() {
                return Err("install: the Helm controller reported an error (FAKE_FAIL_UP)".into());
            }
            let _ = self.logs(
                out,
                stage::SETTING_UP,
                &["chart kivali 0.16.0 placed from the VM image", "HelmChart kube-system/kivali written", "waiting for the server deployment to roll out", "/readyz answers 200"],
            );
            let mut p = self.persisted.lock().unwrap();
            p.kivali = SHIPPED.into();
            p.owner = o.install.owner.clone();
            eprintln!("fake: created org for {}", p.owner);
        } else {
            let _ = self.logs(out, stage::SETTING_UP, &["waiting for the server deployment to roll out", "/readyz answers 200"]);
        }
        self.save();
        self.running.store(true, Ordering::SeqCst);
        self.start_http();
        Ok(Some(serde_json::to_value(self.report()).unwrap()))
    }

    fn check(&self) -> CheckResult {
        let current = self.persisted.lock().unwrap().kivali.clone();
        let now = now_rfc3339();
        let scenario = std::env::var("FAKE_CHECK").unwrap_or_else(|_| check_status::UPGRADE.into());
        let mut r = CheckResult {
            status: scenario.clone(),
            current: current.clone(),
            latest: Some(LATEST.into()),
            message: String::new(),
            notes_url: Some("https://github.com/kivali-ai/kivali/releases".into()),
            checked_at: Some(now),
            min_desktop_version: Some(if scenario == check_status::APP_TOO_OLD { "9.0.0".into() } else { SHIPPED.into() }),
        };
        if current.is_empty() {
            r.status = check_status::NOT_INSTALLED.into();
        } else if current == LATEST || scenario == check_status::UP_TO_DATE {
            r.status = check_status::UP_TO_DATE.into();
            r.latest = Some(current);
        }
        r.message = match r.status.as_str() {
            check_status::FAILED => {
                r.latest = None;
                "Couldn't check for updates: GET release.json: timed out after 10s".into()
            }
            s => format!("fake verdict: {s}"),
        };
        if r.status != check_status::FAILED {
            *self.last_ok.lock().unwrap() = Some(r.clone());
        }
        r
    }
}

/// The first port from `start` that 127.0.0.1 can bind now; `start`
/// itself when none of the next hundred is free.
fn free_port_from(start: u16) -> u16 {
    (start..start.saturating_add(100)).find(|&p| TcpListener::bind(("127.0.0.1", p)).is_ok()).unwrap_or(start)
}

/// A key's last four characters, all the fake keeps of it.
/// RFC 3339 UTC for now, without a date crate.
fn now_rfc3339() -> String {
    let secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs() as i64;
    let (days, rem) = (secs.div_euclid(86400), secs.rem_euclid(86400));
    // civil_from_days (Howard Hinnant)
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z", rem / 3600, rem % 3600 / 60, rem % 60)
}

/// Reads one HTTP/1.1 request: (method, path, body). None when the
/// client closed without sending one (the shell's `is_listening` only
/// opens the endpoint).
fn read_request(conn: &mut dyn Read) -> std::io::Result<Option<(String, String, Vec<u8>)>> {
    let mut r = BufReader::new(conn);
    let mut first = String::new();
    if r.read_line(&mut first)? == 0 {
        return Ok(None);
    }
    let mut parts = first.split_whitespace();
    let method = parts.next().unwrap_or_default().to_string();
    let path = parts.next().unwrap_or_default().to_string();
    let mut len = 0usize;
    loop {
        let mut h = String::new();
        if r.read_line(&mut h)? == 0 || h.trim().is_empty() {
            break;
        }
        if let Some(v) = h.to_ascii_lowercase().strip_prefix("content-length:") {
            len = v.trim().parse().unwrap_or(0);
        }
    }
    let mut body = vec![0u8; len];
    r.read_exact(&mut body)?;
    Ok(Some((method, path, body)))
}

fn respond(conn: &mut dyn Write, status: &str, ctype: &str, body: &[u8]) -> std::io::Result<()> {
    write!(conn, "HTTP/1.1 {status}\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", body.len())?;
    conn.write_all(body)
}

fn body_or_default<T: serde::de::DeserializeOwned + Default>(b: &[u8]) -> Result<T, String> {
    if b.is_empty() {
        return Ok(T::default());
    }
    serde_json::from_slice(b).map_err(|e| format!("bad request: {e}"))
}

/// One connection's request and answer, on either transport. Closing
/// the connection, and exiting after `--exit`, is the caller's
/// ([`finished`]).
fn handle(fake: &Fake, mut conn: impl Read + Write) -> std::io::Result<()> {
    let Some((method, path, body)) = read_request(&mut conn)? else {
        return Ok(());
    };
    if method == "GET" && path == "/v1/status" {
        return respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&fake.report()).unwrap());
    }
    if method == "GET" && path == "/v1/credential" {
        if let Some(why) = fake.not_ready() {
            return respond(&mut conn, "409 Conflict", "text/plain; charset=utf-8", format!("{why}\n").as_bytes());
        }
        return respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&fake.credential_status()).unwrap());
    }
    if path == "/v1/credential/setup" || path == "/v1/credential/clear-provider" {
        if let Some(why) = fake.not_ready() {
            return respond(&mut conn, "409 Conflict", "text/plain; charset=utf-8", format!("{why}\n").as_bytes());
        }
        return match (method.as_str(), path.as_str()) {
            ("GET", "/v1/credential/setup") => {
                let info = SetupInfo { models: MODELS.iter().map(|m| m.to_string()).collect(), current: fake.setup.lock().unwrap().clone() };
                respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&info).unwrap())
            }
            ("POST", "/v1/credential/setup") => match fake.apply_setup(&body) {
                Ok(r) => respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&r).unwrap()),
                Err(why) => respond(&mut conn, "400 Bad Request", "text/plain; charset=utf-8", format!("{why}\n").as_bytes()),
            },
            ("POST", _) => {
                // The fake's settings hold one variable per saved value.
                let cleared: Vec<String> = match fake.setup.lock().unwrap().take() {
                    Some(saved) => saved.values.keys().cloned().collect(),
                    None => vec![],
                };
                respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&serde_json::json!({ "cleared": cleared })).unwrap())
            }
            _ => respond(&mut conn, "404 Not Found", "text/plain", b"404 page not found\n"),
        };
    }
    if method == "POST" && path == "/v1/handoff" {
        if let Some(why) = fake.not_ready() {
            return respond(&mut conn, "409 Conflict", "text/plain; charset=utf-8", format!("{why}\n").as_bytes());
        }
        let r = HandoffResult { token: "fake-handoff-token".into() };
        return respond(&mut conn, "200 OK", "application/json", &serde_json::to_vec(&r).unwrap());
    }
    if method != "POST" || !path.starts_with("/v1/") {
        return respond(&mut conn, "404 Not Found", "text/plain", b"404 page not found\n");
    }
    // Streamed NDJSON: no length, the body ends when the connection does.
    conn.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: close\r\n\r\n")?;
    fake.busy.store(true, Ordering::SeqCst);
    let result: Result<Option<serde_json::Value>, String> = match &path[4..] {
        "up" => body_or_default::<UpOptions>(&body).and_then(|o| fake.up(&mut conn, o)),
        "down" => body_or_default::<DownRequest>(&body).map(|d| {
            if fake.running.load(Ordering::SeqCst) {
                let _ = fake.logs(&mut conn, stage::PAUSING, &["deleting the Kivali pods", "stopping k3s", "the data disk is unmounted; powering off"]);
            } else {
                let _ = fake.logs(&mut conn, stage::PAUSING, &["the VM is not running"]);
            }
            fake.running.store(false, Ordering::SeqCst);
            fake.serving.store(false, Ordering::SeqCst);
            if d.exit {
                let _ = fake.logs(&mut conn, stage::PAUSING, &["serve is exiting"]);
                fake.quit.store(true, Ordering::SeqCst);
            }
            None
        }),
        "address" => body_or_default::<wire::AddressRequest>(&body).map(|r| {
            let line = if r.external_url.is_empty() { "external address cleared".to_string() } else { format!("external address set to {}", r.external_url) };
            let _ = fake.logs(&mut conn, stage::SETTING_UP, &[line.as_str()]);
            None
        }),
        "destroy" => body_or_default::<DestroyRequest>(&body).and_then(|d| {
            let r = fake.destroy(&mut conn)?;
            if d.exit {
                let _ = fake.logs(&mut conn, stage::DELETING, &["serve is exiting"]);
                fake.quit.store(true, Ordering::SeqCst);
            }
            Ok(r)
        }),
        "install" => Ok(None),
        "check" => Ok(Some(serde_json::to_value(fake.check()).unwrap())),
        "upgrade" => {
            if !fake.running.load(Ordering::SeqCst) {
                Err("the VM is not running".into())
            } else {
                let _ = fake.logs(
                    &mut conn,
                    stage::DOWNLOADING,
                    &["fetching release.json", "downloading the chart and images; verifying sha256", "importing the images"],
                );
                let _ = fake.logs(&mut conn, stage::SNAPSHOT, &["journal: prepared", "deleting the agent pods; stopping the VM", "cloning the data disk"]);
                let _ = fake.logs(&mut conn, stage::INSTALLING, &["starting the VM", "HelmChart now points at kivali-0.17.0.tgz"]);
                let _ = fake.logs(&mut conn, stage::STARTING, &["waiting for the new server", "journal: committed; snapshot deleted"]);
                fake.persisted.lock().unwrap().kivali = LATEST.into();
                fake.save();
                Ok(Some(serde_json::to_value(fake.report()).unwrap()))
            }
        }
        other => Err(format!("unknown operation {other}")),
    };
    fake.busy.store(false, Ordering::SeqCst);
    let ev = match result {
        Ok(result) => Event { done: true, result, ..Default::default() },
        Err(e) => Event { done: true, error: Some(e), ..Default::default() },
    };
    conn.write_all(encode_event(&ev).as_bytes())
}

/// After a connection has been closed: its error logged, and the process
/// ended when the request asked serve to exit (`--exit`), `endpoint_gone`
/// first. Closing first lets the last line reach the client.
fn finished(fake: &Fake, handled: std::io::Result<()>, endpoint_gone: impl FnOnce()) {
    if let Err(e) = handled {
        eprintln!("fake: connection: {e}");
    }
    if fake.quit.load(Ordering::SeqCst) {
        eprintln!("fake: exiting (--exit)");
        endpoint_gone();
        std::process::exit(0);
    }
}

fn serve_org(port: u16, serving: Arc<AtomicBool>, owner: String) {
    let listener = match TcpListener::bind(("127.0.0.1", port)) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("fake: cannot bind 127.0.0.1:{port}: {e}");
            return;
        }
    };
    for conn in listener.incoming().flatten() {
        if !serving.load(Ordering::SeqCst) {
            continue; // dropped: the org is "stopped"
        }
        let owner = owner.clone();
        std::thread::spawn(move || {
            let mut conn = conn;
            let mut reader = BufReader::new(conn.try_clone().unwrap());
            let mut first = String::new();
            let _ = reader.read_line(&mut first);
            loop {
                let mut h = String::new();
                if reader.read_line(&mut h).unwrap_or(0) == 0 || h.trim().is_empty() {
                    break;
                }
            }
            let path = first.split_whitespace().nth(1).unwrap_or("/").to_string();
            // The handoff signs nobody in here; like the real one, it
            // lands on "/".
            if path.starts_with("/auth/handoff?") {
                let _ = write!(conn, "HTTP/1.1 302 Found\r\nlocation: /\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
                return;
            }
            let (ctype, body) = if path == "/api/v1/login" {
                ("application/json", r#"{"org":{"name":"Fake Org","has_logo":false},"auth_ready":true,"dev_mode":false}"#.to_string())
            } else {
                (
                    "text/html; charset=utf-8",
                    format!(
                        "<!doctype html><meta charset=utf-8><title>Fake Org</title>\
                         <body style=\"font:16px system-ui;padding:2rem\"><h1>Fake Org</h1>\
                         <p>Served by the fake supervisor on port {port} for {owner}. A real org's UI loads here.</p>\
                         <p><a href=\"https://example.com\" target=\"_blank\">An external link</a> opens in the browser.</p>"
                    ),
                )
            };
            let _ = write!(
                conn,
                "HTTP/1.1 200 OK\r\ncontent-type: {ctype}\r\ncontent-length: {}\r\ncache-control: no-store\r\nconnection: close\r\n\r\n{body}",
                body.len()
            );
        });
    }
}

fn new_fake(dir: &Path) -> Arc<Fake> {
    let state_file = dir.join("fake-local.json");
    let persisted: Persisted = std::fs::read(&state_file).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default();
    Arc::new(Fake {
        state_file,
        persisted: Mutex::new(persisted),
        running: AtomicBool::new(false),
        busy: AtomicBool::new(false),
        http_started: AtomicBool::new(false),
        serving: Arc::new(AtomicBool::new(false)),
        last_ok: Mutex::new(None),
        step: Duration::from_millis(env_or("FAKE_STEP_MS", 600)),
        quit: AtomicBool::new(false),
        signed_in: AtomicBool::new(std::env::var("FAKE_SIGNED_IN").is_ok_and(|v| v == "1")),
        setup: Mutex::new(None),
    })
}

/// The RPC on the owner-only Unix socket `<dir>/supervisor.sock`.
#[cfg(unix)]
fn serve(dir: &Path) -> i32 {
    std::fs::create_dir_all(dir).expect("config dir");
    let sock = dir.join("supervisor.sock");
    if UnixStream::connect(&sock).is_ok() {
        eprintln!("fake: a supervisor is already serving on {}", sock.display());
        return 1;
    }
    let _ = std::fs::remove_file(&sock);
    let listener = UnixListener::bind(&sock).expect("bind socket");
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&sock, std::fs::Permissions::from_mode(0o600));
    }
    let fake = new_fake(dir);
    eprintln!("fake: serving on {}", sock.display());
    for mut conn in listener.incoming().flatten() {
        let (fake, sock) = (fake.clone(), sock.clone());
        std::thread::spawn(move || {
            let handled = handle(&fake, &mut conn);
            drop(conn);
            // As the real serve does on exit: the socket goes with it.
            finished(&fake, handled, || {
                let _ = std::fs::remove_file(&sock);
            });
        });
    }
    0
}

/// The RPC on the owner-only named pipe of `dir` (pipe.rs): one
/// instance per connection, a new one waiting as each is taken.
#[cfg(windows)]
fn serve(dir: &Path) -> i32 {
    std::fs::create_dir_all(dir).expect("config dir");
    let name = pipe::pipe_name(dir);
    let mut listener = match pipe::Listener::bind(&name) {
        Ok(l) => l,
        Err(e) if e.raw_os_error() == Some(windows_sys::Win32::Foundation::ERROR_ACCESS_DENIED as i32) => {
            eprintln!("fake: {name} exists already: a supervisor (or another account) is serving it");
            return 1;
        }
        Err(e) => {
            eprintln!("fake: cannot create {name}: {e}");
            return 1;
        }
    };
    let fake = new_fake(dir);
    eprintln!("fake: serving on {name} for {}", dir.display());
    loop {
        let mut conn = match listener.accept() {
            Ok(c) => c,
            Err(e) => {
                eprintln!("fake: {name}: {e}");
                return 1;
            }
        };
        let fake = fake.clone();
        std::thread::spawn(move || {
            let handled = handle(&fake, &mut conn);
            // Closed: the client reads what is left, then the end. The
            // pipe itself goes with the process.
            drop(conn);
            finished(&fake, handled, || {});
        });
    }
}

fn terminal() -> i32 {
    println!("Kivali (fake supervisor): the real one attaches this terminal to the org's server container,");
    println!("where Claude Code's sign-in runs. Here is a plain shell instead; exit to close.");
    let default = if cfg!(windows) { "cmd.exe" } else { "/bin/sh" };
    let sh = std::env::var(if cfg!(windows) { "COMSPEC" } else { "SHELL" }).unwrap_or_else(|_| default.into());
    let _ = std::process::Command::new(sh).status();
    0
}

fn backup(args: &[String]) -> i32 {
    let Some(out) = args.iter().position(|a| a == "--out").and_then(|i| args.get(i + 1)) else {
        eprintln!("kivali-supervisor: backup: --out is required");
        return 1;
    };
    let body = b"fake kivali backup\n";
    match std::fs::write(out, body) {
        Ok(()) => {
            println!("backup written to {out} ({} bytes)", body.len());
            0
        }
        Err(e) => {
            eprintln!("kivali-supervisor: {e}");
            1
        }
    }
}

fn main() {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    let mut dir = std::env::var_os("KIVALI_CONFIG_DIR").filter(|d| !d.is_empty()).map(PathBuf::from).unwrap_or_else(|| {
        if cfg!(windows) {
            std::env::var_os("LOCALAPPDATA").map(PathBuf::from).unwrap_or_default().join("Kivali")
        } else {
            let home = std::env::var_os("HOME").map(PathBuf::from).unwrap_or_default();
            home.join("Library/Application Support/Kivali")
        }
    });
    if args.first().map(String::as_str) == Some("--config-dir") && args.len() > 1 {
        dir = PathBuf::from(args[1].clone());
        args.drain(..2);
    }
    let code = match args.first().map(String::as_str) {
        Some("serve") => serve(&dir),
        Some("terminal") => terminal(),
        Some("backup") => backup(&args[1..]),
        Some("version") => {
            println!("fake");
            0
        }
        _ => {
            eprintln!("usage: kivali-supervisor [--config-dir DIR] serve | terminal | backup --out FILE | version");
            2
        }
    };
    std::process::exit(code);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A port another program holds is passed over, as the real
    /// supervisor passes it over; a free one is kept.
    #[test]
    fn a_held_port_is_passed_over() {
        // A port with room above it.
        let (held, port) = loop {
            let l = TcpListener::bind(("127.0.0.1", 0)).unwrap();
            let p = l.local_addr().unwrap().port();
            if p < 65_000 {
                break (l, p);
            }
        };
        assert!(free_port_from(port) > port);
        drop(held);
        assert_eq!(free_port_from(port), port);
    }
}
