//! The shell's only way to a team on this computer: the supervisor client.
//!
//! Every local operation goes through [`Supervisor`]: typed calls the
//! rest of the shell uses, mapped here onto the supervisor's RPC
//! (`wire.rs`: HTTP/1.1, spoken by `http.rs`, over the platform's
//! owner-only endpoint, a Unix socket or a named pipe). A protocol change
//! is a change to this module alone.

pub mod http;
pub mod sidecar;
pub mod wire;

use crate::platform;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use wire::{
    CheckReport, CheckRequest, CheckResult, ClearResult, CredentialStatus, DestroyRequest, DestroyResult,
    DownRequest, Event, HandoffResult, InstallOptions, Progress, Report, SetupInfo, SetupRequest, SetupResult, Status,
    UpOptions, UpgradeOptions,
};

#[derive(Debug, Clone, PartialEq)]
pub enum Error {
    /// Nothing is listening on the endpoint (no supervisor running).
    NotRunning,
    /// Talking to it failed mid-way.
    Io(String),
    /// It answered something this shell cannot read.
    Protocol(String),
    /// It answered with an error.
    Supervisor(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::NotRunning => write!(f, "the Kivali supervisor is not running"),
            Error::Io(e) => write!(f, "talking to the Kivali supervisor failed: {e}"),
            Error::Protocol(e) => write!(f, "the Kivali supervisor answered unexpectedly: {e}"),
            Error::Supervisor(e) => write!(f, "{e}"),
        }
    }
}

impl Error {
    /// The error in the supervisor's own words: an answer's body without
    /// its status line ("409 Conflict: "), for a page to show.
    pub fn sentence(&self) -> String {
        if let Error::Supervisor(s) = self {
            let b = s.as_bytes();
            if b.len() > 4 && b[..3].iter().all(u8::is_ascii_digit) && b[3] == b' ' {
                if let Some((_, rest)) = s.split_once(": ") {
                    return rest.to_string();
                }
            }
        }
        self.to_string()
    }
}

/// Reads an operation's NDJSON answer: every log line goes to
/// `on_progress`, the final line decides. Blank lines are skipped; the
/// stream ending first is a protocol error.
pub fn read_events(
    reader: impl BufRead,
    on_progress: &mut dyn FnMut(Progress),
) -> Result<Option<serde_json::Value>, Error> {
    for line in reader.lines() {
        let line = line.map_err(|e| Error::Io(e.to_string()))?;
        if line.trim().is_empty() {
            continue;
        }
        let Event { log, done, error, result, stage } = wire::parse_event(&line).map_err(Error::Protocol)?;
        if done {
            return match error.filter(|e| !e.is_empty()) {
                Some(e) => Err(Error::Supervisor(e)),
                None => Ok(result.filter(|v| !v.is_null())),
            };
        }
        if let Some(label) = log {
            on_progress(Progress { label, stage });
        }
    }
    Err(Error::Protocol("the stream closed before the operation finished".into()))
}

/// The largest error or JSON answer read whole.
const MAX_BODY: u64 = 4 << 20;

/// The protocol `GET /v1/terminal` and `GET /v1/exec` upgrade to.
pub const UPGRADE_TOKEN: &str = "kivali-stream";

pub struct Supervisor {
    endpoint: PathBuf,
}

fn decode<T: serde::de::DeserializeOwned>(v: serde_json::Value) -> Result<T, Error> {
    serde_json::from_value(v).map_err(|e| Error::Protocol(e.to_string()))
}

impl Supervisor {
    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { endpoint: platform::endpoint(&dir.into()) }
    }

    /// The RPC endpoint: a socket path, or a named pipe's name.
    pub fn endpoint(&self) -> &Path {
        &self.endpoint
    }

    pub fn is_listening(&self) -> bool {
        platform::is_listening(&self.endpoint)
    }

    /// One request on a fresh connection; a status outside 2xx (and
    /// other than 101) is the supervisor's error, with its body as the
    /// message. Operations take as long as they take (a first boot, an
    /// upgrade): the supervisor bounds its own steps.
    fn send(&self, req: &http::Request) -> Result<http::Response, Error> {
        self.send_within(req, None)
    }

    /// [`Self::send`], where `idle` (when set) bounds every wait for the
    /// supervisor: a connection on which nothing moves for that long
    /// fails instead of hanging.
    fn send_within(&self, req: &http::Request, idle: Option<std::time::Duration>) -> Result<http::Response, Error> {
        let connected = match idle {
            Some(t) => platform::connect_timeout(&self.endpoint, t),
            None => platform::connect(&self.endpoint),
        };
        let stream = connected.map_err(|e| match e.kind() {
            // No socket or pipe, or a stale socket nobody accepts on.
            std::io::ErrorKind::NotFound | std::io::ErrorKind::ConnectionRefused => Error::NotRunning,
            _ => Error::Io(e.to_string()),
        })?;
        let resp = http::send(stream, req).map_err(|e| Error::Io(e.to_string()))?;
        if !resp.is_success() && resp.status != 101 {
            let line = resp.status_line();
            let body = resp.bytes(MAX_BODY).unwrap_or_default();
            return Err(Error::Supervisor(format!("{line}: {}", String::from_utf8_lossy(&body).trim())));
        }
        Ok(resp)
    }

    /// `POST /v1/<name>` with a JSON body; streams its log lines.
    fn op(
        &self,
        name: &str,
        body: &impl serde::Serialize,
        on_progress: &mut dyn FnMut(Progress),
    ) -> Result<Option<serde_json::Value>, Error> {
        self.op_within(name, body, None, on_progress)
    }

    fn op_within(
        &self,
        name: &str,
        body: &impl serde::Serialize,
        idle: Option<std::time::Duration>,
        on_progress: &mut dyn FnMut(Progress),
    ) -> Result<Option<serde_json::Value>, Error> {
        let target = format!("/v1/{name}");
        let req = http::Request::new("POST", &target).json(body).map_err(|e| Error::Protocol(e.to_string()))?;
        let resp = self.send_within(&req, idle)?;
        read_events(BufReader::new(resp.body()), on_progress)
    }

    /// `GET <target>` upgraded to the supervisor's stream protocol
    /// (`/v1/terminal?…`, `/v1/exec?argv=…`): the raw stream, whose
    /// frames are the guest agent's.
    pub fn open_stream(&self, target: &str) -> Result<http::Upgraded, Error> {
        let req = http::Request::new("GET", target).header("Connection", "Upgrade").header("Upgrade", UPGRADE_TOKEN);
        let resp = self.send(&req)?;
        if resp.status != 101 {
            return Err(Error::Protocol(format!("expected 101 Switching Protocols, got {}", resp.status_line())));
        }
        Ok(resp.upgraded())
    }

    pub fn report(&self) -> Result<Report, Error> {
        let resp = self.send(&http::Request::new("GET", "/v1/status"))?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))
    }

    pub fn status(&self) -> Result<Status, Error> {
        Ok(Status::from_report(&self.report()?))
    }

    fn status_from(&self, result: Option<serde_json::Value>) -> Result<Status, Error> {
        match result {
            Some(v) => Ok(Status::from_report(&decode::<Report>(v)?)),
            None => self.status(),
        }
    }

    /// Boots the team, creating and installing it on the first call (the
    /// owner is required then and ignored afterwards).
    pub fn up(&self, req: &UpRequest, on_progress: &mut dyn FnMut(Progress)) -> Result<Status, Error> {
        let body = UpOptions {
            install: InstallOptions { owner: req.owner.clone().unwrap_or_default(), env: req.env.clone() },
            port: 0,
            port_hint: req.port_hint,
            memory_mb: u64::from(req.memory_mb),
            cpus: req.cpus,
            prepare: req.prepare,
        };
        let r = self.op("up", &body, on_progress)?;
        self.status_from(r)
    }

    /// Deletes the team's VM, data disk, snapshots, downloads and logs;
    /// with `exit`, `serve` ends afterwards. Answers the bytes freed.
    pub fn destroy(&self, exit: bool, on_progress: &mut dyn FnMut(Progress)) -> Result<u64, Error> {
        let r = self.op("destroy", &DestroyRequest { exit }, on_progress)?;
        Ok(r.map(decode::<DestroyResult>).transpose()?.map(|d| d.freed_bytes).unwrap_or(0))
    }

    /// How the team's Claude CLI is signed in: whether it is, the account
    /// and what calls are billed to; never the credential.
    pub fn credential(&self) -> Result<CredentialStatus, Error> {
        let resp = self.send(&http::Request::new("GET", "/v1/credential"))?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))
    }

    /// The model ids the team runs and the sign-in setup saved now,
    /// without its secrets.
    pub fn setup_info(&self) -> Result<SetupInfo, Error> {
        let resp = self.send(&http::Request::new("GET", "/v1/credential/setup"))?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))
    }

    /// Signs the team's Claude CLI in with a sign-in setup and checks each
    /// model; takes as long as the slowest check (the supervisor bounds
    /// each). The values go to the supervisor alone; the answer holds
    /// none.
    pub fn apply_setup(&self, req: &SetupRequest) -> Result<SetupResult, Error> {
        let req = http::Request::new("POST", "/v1/credential/setup").json(req).map_err(|e| Error::Protocol(e.to_string()))?;
        let resp = self.send(&req)?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))
    }

    /// Removes every cloud provider's variables from the CLI's settings,
    /// so the next sign-in in Terminal is the one the CLI uses.
    pub fn clear_provider(&self) -> Result<ClearResult, Error> {
        let req = http::Request::new("POST", "/v1/credential/clear-provider").json(&serde_json::json!({})).map_err(|e| Error::Protocol(e.to_string()))?;
        let resp = self.send(&req)?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))
    }

    /// A one-time token that signs the team's owner in at
    /// `<origin>/auth/handoff?t=<token>` within two minutes. The token is
    /// a secret: callers never log it.
    pub fn handoff(&self) -> Result<String, Error> {
        let req = http::Request::new("POST", "/v1/handoff").json(&serde_json::json!({})).map_err(|e| Error::Protocol(e.to_string()))?;
        let resp = self.send(&req)?;
        let body = resp.bytes(MAX_BODY).map_err(|e| Error::Io(e.to_string()))?;
        let r: HandoffResult = serde_json::from_slice(&body).map_err(|e| Error::Protocol(e.to_string()))?;
        if r.token.is_empty() {
            return Err(Error::Protocol("the supervisor answered no handoff token".into()));
        }
        Ok(r.token)
    }

    /// Records the team's external https origin (Other devices), or
    /// clears it; the server then accepts sign-ins at that name as well as
    /// on loopback. Waits until it serves again when that changed.
    pub fn set_address(&self, external_url: Option<&str>, on_progress: &mut dyn FnMut(Progress)) -> Result<(), Error> {
        let body = wire::AddressRequest { external_url: external_url.unwrap_or_default().to_string() };
        self.op("address", &body, on_progress).map(|_| ())
    }

    /// Stops the VM cleanly; with `exit`, `serve` ends afterwards.
    pub fn down(&self, exit: bool, on_progress: &mut dyn FnMut(Progress)) -> Result<(), Error> {
        self.op("down", &DownRequest { exit }, on_progress).map(|_| ())
    }

    /// [`Self::down`] for stopping `serve`: an error once the supervisor
    /// has said nothing for `idle` (it accepted the connection but is
    /// wedged), so the stop can go on to the platform's stop request and
    /// a kill (`Sidecar::stop`). A shutdown that keeps logging keeps
    /// going.
    pub fn down_within(
        &self,
        exit: bool,
        idle: std::time::Duration,
        on_progress: &mut dyn FnMut(Progress),
    ) -> Result<(), Error> {
        self.op_within("down", &DownRequest { exit }, Some(idle), on_progress).map(|_| ())
    }

    pub fn install(&self, on_progress: &mut dyn FnMut(Progress)) -> Result<Status, Error> {
        self.op("install", &InstallOptions::default(), on_progress)?;
        self.status()
    }

    /// The release check. A failed fetch is a successful call whose
    /// report says so; the last successful check's time comes from the
    /// status report.
    pub fn check(&self) -> Result<CheckReport, Error> {
        let r = self.op("check", &CheckRequest::default(), &mut |_| {})?;
        let result: CheckResult = decode(r.ok_or_else(|| Error::Protocol("check returned no result".into()))?)?;
        let last_ok = self.report().ok().and_then(|r| r.state.latest).and_then(|l| l.checked_at);
        Ok(CheckReport::from_result(&result, last_ok))
    }

    pub fn upgrade(&self, on_progress: &mut dyn FnMut(Progress)) -> Result<Status, Error> {
        let r = self.op("upgrade", &UpgradeOptions::default(), on_progress)?;
        self.status_from(r)
    }
}

/// What `up` is asked: the owner and server environment on the first
/// call, the port to try first, and the VM's size.
#[derive(Debug, Clone, Default)]
pub struct UpRequest {
    pub owner: Option<String>,
    pub env: Vec<String>,
    pub port_hint: u16,
    pub memory_mb: u32,
    pub cpus: u32,
    /// Boot and forward only (a new team's machine, before setup knows
    /// its owner); a later `up` installs.
    pub prepare: bool,
}

#[cfg(test)]
mod tests {
    use super::wire::*;
    use super::*;
    use std::io::Cursor;
    // The socket tests below are Unix-only (the fake server binds a Unix
    // socket); only they read and write a stream.
    #[cfg(unix)]
    use std::io::{Read, Write};

    #[test]
    fn up_body_matches_the_supervisor() {
        let body = UpOptions {
            install: InstallOptions { owner: "owner@example.com".into(), ..Default::default() },
            ..Default::default()
        };
        assert_eq!(serde_json::to_string(&body).unwrap(), r#"{"install":{"owner":"owner@example.com"}}"#);
        assert_eq!(serde_json::to_string(&DownRequest { exit: true }).unwrap(), r#"{"exit":true}"#);
        assert_eq!(serde_json::to_string(&DownRequest { exit: false }).unwrap(), "{}");
    }

    #[test]
    fn events_stream_logs_then_result() {
        let input = "{\"log\":\"booting\"}\n\n{\"log\":\"installing\"}\n{\"done\":true,\"result\":{\"running\":true}}\n{\"log\":\"after\"}\n";
        let mut seen = Vec::new();
        let v = read_events(Cursor::new(input), &mut |p| seen.push(p.label)).unwrap().unwrap();
        assert_eq!(seen, ["booting", "installing"]);
        assert_eq!(v["running"], true);
        let none = read_events(Cursor::new("{\"done\":true}\n"), &mut |_| {}).unwrap();
        assert_eq!(none, None);
    }

    #[test]
    fn events_errors() {
        let e = read_events(Cursor::new("{\"done\":true,\"error\":\"disk full\"}\n"), &mut |_| {}).unwrap_err();
        assert_eq!(e, Error::Supervisor("disk full".into()));
        let e = read_events(Cursor::new("{\"log\":\"x\"}\n"), &mut |_| {}).unwrap_err();
        assert!(matches!(e, Error::Protocol(_)));
        let e = read_events(Cursor::new("garbage\n"), &mut |_| {}).unwrap_err();
        assert!(matches!(e, Error::Protocol(_)));
    }

    fn report(json: &str) -> Report {
        serde_json::from_str(json).unwrap()
    }

    #[test]
    fn status_from_reports() {
        // Fresh computer: nothing installed, no disk.
        let s = Status::from_report(&report(r#"{"running":false,"state":{"kivali":"","data_disk_formatted":false,"port":0},"busy":false}"#));
        assert_eq!(s, Status { state: OrgState::Absent, port: None, kivali: None, message: None });
        // Installed, VM stopped.
        let s = Status::from_report(&report(r#"{"running":false,"state":{"kivali":"0.16.0","data_disk_formatted":true,"port":8080}}"#));
        assert_eq!((s.state, s.port, s.kivali.as_deref()), (OrgState::Stopped, Some(8080), Some("0.16.0")));
        // Running and ready, with the full report's extra fields ignored.
        let s = Status::from_report(&report(
            r#"{"running":true,"forward":"127.0.0.1:8123","url":"http://127.0.0.1:8123/",
                "guest":{"boot_id":"b","state":"ready","node_ready":true,"versions":{"agent":"x"},"time":"2026-10-01T12:00:00Z"},
                "state":{"vm_image":"0.16.0","kivali":"0.16.0","data_disk":"data.img","data_disk_formatted":true,"memory_mb":4096,"cpus":4,
                         "port":8080,"feed":"f","last_check":null,"last_check_ok":false,"upgrade":null},
                "config_dir":"/x","supervisor_version":"dev","busy":false}"#,
        ));
        assert_eq!((s.state, s.port), (OrgState::Running, Some(8123)));
        // Booting.
        let s = Status::from_report(&report(r#"{"running":true,"guest":{"state":"booting"},"state":{"kivali":"0.16.0"},"busy":true}"#));
        assert_eq!(s.state, OrgState::Starting);
        // A backup in flight (busy) leaves a ready org running.
        let s = Status::from_report(&report(r#"{"running":true,"forward":"127.0.0.1:8080","guest":{"state":"ready"},"state":{"kivali":"0.16.0"},"busy":true}"#));
        assert_eq!(s.state, OrgState::Running);
        // Guest fatal.
        let s = Status::from_report(&report(r#"{"running":true,"guest":{"state":"fatal","fatal":"no user namespaces"},"state":{}}"#));
        assert_eq!((s.state, s.message.as_deref()), (OrgState::Failed, Some("no user namespaces")));
        // Upgrade journal open.
        let s = Status::from_report(&report(r#"{"running":true,"guest":{"state":"ready"},"forward":"127.0.0.1:8080","state":{"kivali":"0.16.0","upgrade":{"from":"0.16.0","to":"0.17.0","step":"applying"}}}"#));
        assert_eq!(s.state, OrgState::Upgrading);
    }

    #[test]
    fn check_reports_from_results() {
        let ok: CheckResult = serde_json::from_str(
            r#"{"status":"upgrade-available","current":"0.16.0","latest":"0.17.0","message":"Upgrade to Kivali 0.17.0 available",
                "notes_url":"https://n","checked_at":"2026-10-01T12:00:00.123456Z","feed":"https://f"}"#,
        )
        .unwrap();
        let r = CheckReport::from_result(&ok, Some("2026-09-01T00:00:00Z".into()));
        assert_eq!(r.status.as_deref(), Some(check_status::UPGRADE));
        assert_eq!(r.latest.as_deref(), Some("0.17.0"));
        assert_eq!(r.last_ok_at.as_deref(), Some("2026-10-01T12:00:00.123456Z"));
        assert_eq!(r.error, None);

        let failed: CheckResult = serde_json::from_str(
            r#"{"status":"failed","current":"0.16.0","message":"Couldn't check for updates: timeout","checked_at":"2026-10-01T12:00:00Z","feed":"f"}"#,
        )
        .unwrap();
        let r = CheckReport::from_result(&failed, Some("2026-09-30T08:00:00Z".into()));
        assert_eq!(r.error.as_deref(), Some("Couldn't check for updates: timeout"));
        assert_eq!(r.last_ok_at.as_deref(), Some("2026-09-30T08:00:00Z"));

        let with_min: CheckResult = serde_json::from_str(
            r#"{"status":"upgrade-available","current":"0.16.0","latest":"0.17.0","message":"m","min_desktop_version":"0.17.0"}"#,
        )
        .unwrap();
        assert_eq!(CheckReport::from_result(&with_min, None).min_desktop.as_deref(), Some("0.17.0"));
        assert_eq!(CheckReport::from_result(&ok, None).min_desktop, None);

        let manual: CheckResult = serde_json::from_str(r#"{"status":"manual-steps","current":"0.16.0","latest":"0.17.0","message":"m"}"#).unwrap();
        assert!(CheckReport::from_result(&manual, None).manual_steps);
    }

    /// A one-request HTTP server on a Unix socket, answering `answer`.
    #[cfg(unix)]
    fn serve_once(sock: &Path, answer: &'static str) -> std::thread::JoinHandle<String> {
        let listener = std::os::unix::net::UnixListener::bind(sock).unwrap();
        std::thread::spawn(move || {
            let (mut conn, _) = listener.accept().unwrap();
            let mut req = Vec::new();
            let mut buf = [0u8; 4096];
            // Read the head, then the body by content-length.
            loop {
                let n = conn.read(&mut buf).unwrap();
                req.extend_from_slice(&buf[..n]);
                let text = String::from_utf8_lossy(&req).to_string();
                if let Some(head_end) = text.find("\r\n\r\n") {
                    let len = text[..head_end]
                        .lines()
                        .find_map(|l| l.to_ascii_lowercase().strip_prefix("content-length:").map(|v| v.trim().parse::<usize>().unwrap()))
                        .unwrap_or(0);
                    if req.len() >= head_end + 4 + len {
                        break;
                    }
                }
                if n == 0 {
                    break;
                }
            }
            conn.write_all(answer.as_bytes()).unwrap();
            String::from_utf8(req).unwrap()
        })
    }

    #[cfg(unix)]
    #[test]
    fn up_over_the_socket() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(
            sup.endpoint(),
            "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: close\r\n\r\n\
             {\"log\":\"booting the VM\"}\n\
             {\"done\":true,\"result\":{\"running\":true,\"forward\":\"127.0.0.1:8123\",\"guest\":{\"state\":\"ready\"},\"state\":{\"kivali\":\"0.16.0\",\"port\":8123}}}\n",
        );
        let mut logs = Vec::new();
        let req = UpRequest {
            owner: Some("owner@example.com".into()),
            env: vec!["KIVALI_TEAM_KIND=work".into()],
            port_hint: 8081,
            memory_mb: 4096,
            cpus: 4,
            prepare: false,
        };
        let st = sup.up(&req, &mut |p| logs.push(p.label)).unwrap();
        assert_eq!((st.state, st.port), (OrgState::Running, Some(8123)));
        assert_eq!(logs, ["booting the VM"]);
        let req = server.join().unwrap();
        assert!(req.starts_with("POST /v1/up HTTP/1.1\r\n"), "{req}");
        assert!(
            req.ends_with(r#"{"install":{"owner":"owner@example.com","env":["KIVALI_TEAM_KIND=work"]},"port_hint":8081,"memory_mb":4096,"cpus":4}"#),
            "{req}"
        );
    }

    #[cfg(unix)]
    #[test]
    fn http_error_status_is_a_supervisor_error() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(sup.endpoint(), "HTTP/1.1 400 Bad Request\r\nContent-Length: 17\r\nConnection: close\r\n\r\nbad request: nope");
        let e = sup.down(false, &mut |_| {}).unwrap_err();
        assert_eq!(e, Error::Supervisor("400 Bad Request: bad request: nope".into()));
        server.join().unwrap();
    }

    /// Go's net/http answers a JSON status with a length and an
    /// operation chunked; both over the real socket.
    #[cfg(unix)]
    #[test]
    fn chunked_status_and_operation() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(
            sup.endpoint(),
            "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n\
             17\r\n{\"log\":\"stopping k3s\"}\n\r\n\
             e\r\n{\"done\":true}\n\r\n\
             0\r\n\r\n",
        );
        let mut logs = Vec::new();
        sup.down(true, &mut |p| logs.push(p.label)).unwrap();
        assert_eq!(logs, ["stopping k3s"]);
        let req = server.join().unwrap();
        assert!(req.starts_with("POST /v1/down HTTP/1.1\r\nHost: kivali\r\n"), "{req}");
        assert!(req.ends_with("\r\n\r\n{\"exit\":true}"), "{req}");
    }

    #[cfg(unix)]
    #[test]
    fn credential_setup_over_the_socket() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(
            sup.endpoint(),
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n\
             {\"credential\":{\"signed_in\":true,\"billing\":\"P · r\",\"checked_at\":\"t\"},\"models\":[{\"model\":\"m\",\"ok\":false,\"missing\":true,\"problem\":\"No m in r\"}]}\n",
        );
        let req = SetupRequest { setup: "s".into(), values: [("name".to_string(), "r".to_string())].into() };
        let res = sup.apply_setup(&req).unwrap();
        assert!(res.models[0].missing);
        let req = server.join().unwrap();
        assert!(req.starts_with("POST /v1/credential/setup HTTP/1.1\r\n"), "{req}");
        assert!(req.ends_with(r#"{"setup":"s","values":{"name":"r"}}"#), "{req}");

        std::fs::remove_file(sup.endpoint()).unwrap();
        let server = serve_once(sup.endpoint(), "HTTP/1.1 400 Bad Request\r\nContent-Length: 19\r\n\r\nenter the name only");
        let e = sup.apply_setup(&SetupRequest::default()).unwrap_err();
        assert_eq!(e.sentence(), "enter the name only");
        server.join().unwrap();

        std::fs::remove_file(sup.endpoint()).unwrap();
        let server = serve_once(sup.endpoint(), "HTTP/1.1 200 OK\r\nContent-Length: 17\r\n\r\n{\"cleared\":[\"X\"]}");
        assert_eq!(sup.clear_provider().unwrap().cleared, ["X"]);
        let req = server.join().unwrap();
        assert!(req.starts_with("POST /v1/credential/clear-provider HTTP/1.1\r\n"), "{req}");

        assert_eq!(Error::Supervisor("409 Conflict: the VM is not running".into()).sentence(), "the VM is not running");
        assert_eq!(Error::Supervisor("plain".into()).sentence(), "plain");
        assert_eq!(Error::NotRunning.sentence(), "the Kivali supervisor is not running");
    }

    #[cfg(unix)]
    #[test]
    fn upgrade_over_the_socket() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(
            sup.endpoint(),
            "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: kivali-stream\r\n\r\nhello from the guest",
        );
        let mut s = sup.open_stream("/v1/exec?argv=%5B%22true%22%5D").unwrap();
        let mut got = String::new();
        s.read_to_string(&mut got).unwrap();
        assert_eq!(got, "hello from the guest");
        let req = server.join().unwrap();
        assert!(req.starts_with("GET /v1/exec?argv=%5B%22true%22%5D HTTP/1.1\r\n"), "{req}");
        assert!(req.contains("Upgrade: kivali-stream\r\n"), "{req}");

        std::fs::remove_file(sup.endpoint()).unwrap();
        let server = serve_once(sup.endpoint(), "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 13\r\n\r\nno guest yet\n");
        assert_eq!(sup.open_stream("/v1/terminal").err().unwrap(), Error::Supervisor("502 Bad Gateway: no guest yet".into()));
        server.join().unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn handoff_over_the_socket() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        let server = serve_once(
            sup.endpoint(),
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 18\r\nConnection: close\r\n\r\n{\"token\":\"p.sig\"}\n",
        );
        assert_eq!(sup.handoff().unwrap(), "p.sig");
        let req = server.join().unwrap();
        assert!(req.starts_with("POST /v1/handoff HTTP/1.1\r\n"), "{req}");
        assert!(req.ends_with("\r\n\r\n{}"), "{req}");

        std::fs::remove_file(sup.endpoint()).unwrap();
        let server = serve_once(sup.endpoint(), "HTTP/1.1 409 Conflict\r\nContent-Length: 22\r\n\r\nthe VM is not running\n");
        assert!(matches!(sup.handoff().unwrap_err(), Error::Supervisor(m) if m.contains("not running")));
        server.join().unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn not_running() {
        let dir = tempfile::tempdir().unwrap();
        let sup = Supervisor::new(dir.path());
        assert!(!sup.is_listening());
        assert_eq!(sup.status().unwrap_err(), Error::NotRunning);
    }
}
