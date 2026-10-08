//! The shell's state and its flows. Every team on this computer has a
//! [`Runtime`]: its own supervisor (`kivali-supervisor --config-dir
//! <its folder> serve`), status, long operation and release check. Teams
//! elsewhere have only what the shell learns by asking their address.
//! Windows, menus and IPC commands call into here.

use crate::ownersignin;
use crate::platform;
use crate::signin::PendingSignin;
use crate::supervisor::sidecar::{self, Sidecar, STOP_TIMEOUTS};
use crate::supervisor::wire::{
    CheckReport, CredentialStatus, OrgState, Progress, Report, SetupInfo, SetupRequest, SetupResult, Status,
};
use crate::supervisor::{Error as SupError, Supervisor, UpRequest};
use crate::teams::{Kind, Place, Team, TeamsFile};
use crate::updates;
use crate::view::{self, Activity, OpKind, OpView, TeamState};
use crate::{hostinfo, menus, orgurl, teamapi, windows};
use serde::Serialize;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager};
use time::OffsetDateTime;

/// The event every bundled page listens to: "something changed, fetch
/// the snapshot again".
pub const CHANGED_EVENT: &str = "shell-changed";

/// How often the Kivali release check runs after the first one.
const CHECK_EVERY: Duration = Duration::from_secs(24 * 60 * 60);
/// The watch loop's tick, while someone is signing in to Claude.
const FAST_TICK: Duration = Duration::from_secs(3);
/// Its tick otherwise: crashes, a Claude that signed out, teams elsewhere.
const SLOW_TICK: Duration = Duration::from_secs(60);
/// How often a running team is asked whether Claude is still signed in.
const CREDENTIAL_EVERY: Duration = Duration::from_secs(5 * 60);
/// How long a Terminal sign-in is waited for before the page stops waiting.
const SIGNIN_WAIT: Duration = Duration::from_secs(15 * 60);
/// The first port a team on this computer is offered.
const FIRST_PORT: u16 = 8080;

fn now_rfc3339() -> String {
    OffsetDateTime::now_utc()
        .format(&time::format_description::well_known::Rfc3339)
        .unwrap_or_default()
}

fn unix_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// A team's long operation, current or last.
#[derive(Debug, Clone)]
pub struct TeamOp {
    pub kind: OpKind,
    pub running: bool,
    pub error: Option<String>,
    pub finished: bool,
    pub lines: Vec<String>,
    pub stage: Option<usize>,
    pub stage_since: Instant,
    pub started_ms: u64,
}

impl TeamOp {
    fn new(kind: OpKind) -> TeamOp {
        TeamOp {
            kind,
            running: true,
            error: None,
            finished: false,
            lines: Vec::new(),
            stage: None,
            stage_since: Instant::now(),
            started_ms: unix_ms(),
        }
    }

    fn progress(&mut self, p: &Progress) {
        if let Some(i) = view::stage_index(self.kind, p.stage.as_deref()) {
            if self.stage != Some(i) {
                self.stage = Some(i);
                self.stage_since = Instant::now();
            }
        }
        self.lines.push(p.label.clone());
    }

    pub fn view(&self) -> OpView {
        let stages = self.kind.stages();
        let index = if self.finished { stages.len().saturating_sub(1) } else { self.stage.unwrap_or(0) };
        let (percent, remaining) = view::progress(self.kind, index, self.stage_since.elapsed(), self.finished);
        OpView {
            kind: self.kind,
            running: self.running,
            error: self.error.clone(),
            finished: self.finished,
            lines: self.lines.clone(),
            stage: stages.get(index).map(|s| s.1.to_string()),
            stage_index: index,
            stages: stages.iter().map(|s| s.1.to_string()).collect(),
            started_ms: self.started_ms,
            percent,
            remaining: if self.running { remaining } else { None },
        }
    }
}

/// The supervisor a runtime owns, and whether it has been closed (Quit,
/// a delete, the system ending the app): after that nothing new is stored.
#[derive(Default)]
struct OwnedSidecar {
    sidecar: Option<Sidecar>,
    closed: bool,
}

/// Claude's sign-in in Terminal, as far as the shell can tell without
/// reading it: when it was opened, the sign-in it started from, whether
/// a terminal session attached, whether it closed before a credential
/// appeared.
#[derive(Debug, Default, Clone)]
pub struct SigninWatch {
    pub opened_at: Option<Instant>,
    /// The sign-in when Terminal opened ([`signin_key`]): Sign in again
    /// waits for a different one, not the one already there.
    pub from: Option<SigninKey>,
    pub saw_terminal: bool,
    pub closed: bool,
}

/// What tells two sign-ins apart: signed in, the account, the billing.
pub type SigninKey = (bool, Option<String>, Option<String>);

pub fn signin_key(c: &CredentialStatus) -> SigninKey {
    (c.signed_in, c.email.clone(), c.billing.clone())
}

/// A team on this computer while the app runs.
pub struct Runtime {
    pub id: String,
    pub dir: PathBuf,
    pub supervisor: Supervisor,
    sidecar: Mutex<OwnedSidecar>,
    /// The supervisor's last report; None when it is not running.
    pub report: Mutex<Option<Report>>,
    pub check: Mutex<Option<CheckReport>>,
    pub op: Mutex<Option<TeamOp>>,
    op_done: Condvar,
    pub credential: Mutex<Option<CredentialStatus>>,
    credential_at: Mutex<Option<Instant>>,
    /// Created in this run's setup, which has not finished (`finish_setup`):
    /// setup may still remove it (Back, Try again).
    pub provisional: AtomicBool,
    /// Why the last start failed; cleared by the next success.
    pub failure: Mutex<Option<String>>,
    pub signin: Mutex<SigninWatch>,
    check_loop: AtomicBool,
    /// What the team's own API last said, as its window's person
    /// (teamapi.rs); None until read, or when that window isn't signed in.
    pub facts: Mutex<Option<teamapi::Facts>>,
}

impl Runtime {
    fn new(id: &str, dir: PathBuf) -> Runtime {
        Runtime {
            id: id.to_string(),
            supervisor: Supervisor::new(&dir),
            dir,
            sidecar: Mutex::new(OwnedSidecar::default()),
            report: Mutex::new(None),
            check: Mutex::new(None),
            op: Mutex::new(None),
            op_done: Condvar::new(),
            credential: Mutex::new(None),
            credential_at: Mutex::new(None),
            provisional: AtomicBool::new(false),
            failure: Mutex::new(None),
            signin: Mutex::new(SigninWatch::default()),
            check_loop: AtomicBool::new(false),
            facts: Mutex::new(None),
        }
    }

    pub fn status(&self) -> Option<Status> {
        self.report.lock().unwrap().as_ref().map(Status::from_report)
    }

    pub fn op_running(&self) -> Option<OpKind> {
        self.op.lock().unwrap().as_ref().filter(|o| o.running).map(|o| o.kind)
    }

    pub fn is_running(&self) -> bool {
        self.status().is_some_and(|s| s.state == OrgState::Running)
    }

    pub fn state(&self) -> (TeamState, Option<Activity>) {
        let op = self.op_running();
        let failed = self.failure.lock().unwrap().is_some();
        view::local_state(op, failed, self.status().as_ref())
    }

    pub fn refresh_status(&self) -> Result<Report, SupError> {
        let r = self.supervisor.report();
        match &r {
            Ok(rep) => *self.report.lock().unwrap() = Some(rep.clone()),
            Err(SupError::NotRunning) => *self.report.lock().unwrap() = None,
            Err(_) => {}
        }
        r
    }

    pub fn refresh_credential(&self) -> Option<CredentialStatus> {
        let c = self.supervisor.credential().ok();
        *self.credential_at.lock().unwrap() = Some(Instant::now());
        if c.is_some() {
            *self.credential.lock().unwrap() = c.clone();
        }
        c
    }

    /// Takes ownership of a listening `serve` a previous shell left
    /// behind (sidecar.rs, `adoptable`).
    pub fn adopt_running(&self) {
        let mut owned = self.sidecar.lock().unwrap();
        if owned.sidecar.is_none() && !owned.closed {
            if let Some(sc) = Sidecar::adopt(&self.dir, self.supervisor.endpoint()) {
                eprintln!("kivali: {}: adopted the supervisor left running (pid {})", self.id, sc.pid());
                owned.sidecar = Some(sc);
            }
        }
    }

    /// Makes sure a supervisor answers on this team's endpoint, starting
    /// the bundled one when nothing does. The lock is never held across a
    /// stop or a start.
    pub fn ensure_supervisor(&self, vm_dir: Option<&std::path::Path>) -> Result<(), String> {
        if self.supervisor.is_listening() {
            self.adopt_running();
            return Ok(());
        }
        let old = self.sidecar.lock().unwrap().sidecar.take();
        if let Some(old) = old {
            old.stop(&mut || Err("not answering".into()), STOP_TIMEOUTS);
        }
        crate::paths::ensure_dir(&self.dir).map_err(|e| format!("cannot make {}: {e}", self.dir.display()))?;
        let binary = sidecar::binary_path()?;
        let sc = Sidecar::start(
            &binary,
            &self.dir,
            vm_dir,
            &|| self.supervisor.is_listening(),
            &|| std::thread::sleep(Duration::from_millis(50)),
            &Instant::now,
        )?;
        let extra = {
            let mut owned = self.sidecar.lock().unwrap();
            if owned.closed {
                Some(sc)
            } else {
                match &owned.sidecar {
                    Some(same) if same.pid() == sc.pid() => None,
                    _ => owned.sidecar.replace(sc),
                }
            }
        };
        if let Some(extra) = extra {
            self.stop_owned(extra, &mut |_| {});
            if self.sidecar.lock().unwrap().closed {
                return Err("Kivali is quitting.".into());
            }
        }
        Ok(())
    }

    fn close_sidecar(&self) -> Option<Sidecar> {
        let mut owned = self.sidecar.lock().unwrap();
        owned.closed = true;
        owned.sidecar.take()
    }

    fn stop_owned(&self, sc: Sidecar, progress: &mut dyn FnMut(Progress)) {
        let how = sc.stop(
            &mut || {
                self.supervisor.down_within(true, STOP_TIMEOUTS.after_down, &mut *progress).map_err(|e| {
                    eprintln!("kivali: {}: stopping: {e}", self.id);
                    e.to_string()
                })
            },
            STOP_TIMEOUTS,
        );
        eprintln!("kivali: {}: the supervisor stopped ({how:?})", self.id);
    }

    /// Stops the team and the supervisor this shell owns, RPC first; a
    /// supervisor started elsewhere stops its VM and keeps serving.
    pub fn stop_everything(&self, progress: &mut dyn FnMut(Progress)) {
        match self.close_sidecar() {
            Some(sc) => self.stop_owned(sc, progress),
            None if self.supervisor.is_listening() => {
                if let Err(e) = self.supervisor.down_within(false, STOP_TIMEOUTS.after_down, progress) {
                    eprintln!("kivali: {}: stopping: {e}", self.id);
                }
            }
            None => {}
        }
        *self.report.lock().unwrap() = None;
    }

    /// After a delete: the supervisor has exited (destroy with exit), so
    /// only the process is waited for.
    fn forget_sidecar(&self) {
        if let Some(sc) = self.close_sidecar() {
            sc.stop(&mut || Ok(()), STOP_TIMEOUTS);
        }
    }

    fn wait_idle(&self) {
        let mut op = self.op.lock().unwrap();
        while op.as_ref().is_some_and(|o| o.running) {
            op = self.op_done.wait(op).unwrap();
        }
    }
}

/// A team elsewhere, as far as the shell has asked.
#[derive(Debug, Default, Clone)]
pub struct Reach {
    pub reachable: Option<bool>,
    pub last_reached: Option<OffsetDateTime>,
    pub checked: Option<Instant>,
    /// The team's own API, as for a team here ([`Runtime::facts`]).
    pub facts: Option<teamapi::Facts>,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct OwnerSigninView {
    pub state: &'static str,
    pub email: Option<String>,
    pub error: Option<String>,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct ConnectView {
    pub state: &'static str,
    pub origin: Option<String>,
    pub name: Option<String>,
    pub error: Option<String>,
    pub team_id: Option<String>,
    /// The Google account the team refused, from its `kivali_denied`
    /// cookie (teamapi.rs, `note_denied`).
    pub email: Option<String>,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct AppUpdateView {
    pub state: &'static str,
    pub version: Option<String>,
    pub checked_at: Option<String>,
    pub error: Option<String>,
}

pub struct Shell {
    pub dir: PathBuf,
    pub app_version: String,
    pub vm_dir: Option<PathBuf>,
    pub teams: Mutex<TeamsFile>,
    runtimes: Mutex<HashMap<String, Arc<Runtime>>>,
    pub reach: Mutex<HashMap<String, Reach>>,
    pub app_update: Mutex<AppUpdateView>,
    owner: Mutex<(OwnerSigninView, Option<ownersignin::Pending>)>,
    pub connect: Mutex<ConnectView>,
    /// Teams elsewhere being connected: listed nowhere until signed in.
    pub connecting: Mutex<Option<Team>>,
    /// The origin each team window is pinned to (windows.rs, `team_nav`).
    pub pins: Mutex<HashMap<String, Arc<Mutex<Option<url::Origin>>>>>,
    /// Sign-ins a team window started and the browser has not handed back.
    pub pending_signins: Mutex<HashMap<String, PendingSignin>>,
    /// The team window in front, or last in front.
    pub front_team: Mutex<Option<String>>,
    pub zoom: Mutex<HashMap<String, f64>>,
    pub last_deleted: Mutex<Option<(String, u64)>>,
    pub quitting: AtomicBool,
    loops: AtomicBool,
    /// A new team's machine, booting since "Create a team" was chosen and
    /// waiting for `create_team` to name its owner (`prepare_team`).
    prepared: Mutex<Option<Prepared>>,
}

/// See [`Shell::prepared`].
struct Prepared {
    id: String,
    rt: Arc<Runtime>,
    /// The install, once setup knows the owner; None (or dropping it)
    /// ends the preparation.
    go: std::sync::mpsc::Sender<Option<UpRequest>>,
}

/// Marks a prepared machine's folder until a team adopts it: one found at
/// launch is left over from a setup that never finished.
const PREPARED_MARKER: &str = ".prepared";

/// What the pages render (types.ts `Snapshot`).
#[derive(Serialize)]
pub struct Snapshot {
    pub app_version: String,
    pub platform: &'static str,
    pub teams: Vec<TeamView>,
    pub host: HostView,
    pub settings: SettingsView,
    pub app_update: AppUpdateView,
    pub owner_signin: OwnerSigninView,
    pub connect: ConnectView,
    pub config_dir: String,
    pub logs_dir: String,
    pub last_deleted: Option<DeletedView>,
}

#[derive(Serialize)]
pub struct HostView {
    pub memory_mb: u64,
    pub cpus: u64,
}

#[derive(Serialize)]
pub struct SettingsView {
    pub start_at_login: bool,
    pub ask_before_quit: bool,
}

#[derive(Serialize)]
pub struct DeletedView {
    pub name: String,
    pub freed_bytes: u64,
}

#[derive(Serialize, Clone)]
pub struct AiView {
    pub provider: &'static str,
    pub signed_in: Option<bool>,
    /// The account the CLI is signed in to, when it reports one.
    pub email: Option<String>,
    /// What model calls are billed to, in the CLI's terms ("Claude Max",
    /// "Amazon Bedrock").
    pub billing: Option<String>,
    pub terminal_open: bool,
    pub signin: &'static str,
    /// RFC 3339: when Claude signed out, while it is.
    pub signed_out_at: Option<String>,
}

#[derive(Serialize, Clone)]
pub struct TeamUpdateView {
    pub state: &'static str,
    pub current: Option<String>,
    pub latest: Option<String>,
    pub notes_url: Option<String>,
    pub summary: Option<String>,
    pub checked_at: Option<String>,
    /// How long an update typically takes, "about 3 minutes".
    pub takes: String,
}

#[derive(Serialize, Clone)]
pub struct TeamView {
    pub id: String,
    pub name: String,
    pub kind: Option<Kind>,
    pub place: &'static str,
    pub url: Option<String>,
    pub device: Option<String>,
    pub state: TeamState,
    pub activity: Option<Activity>,
    pub phrase: String,
    pub reason: Option<String>,
    pub pause_first: Option<String>,
    pub op: Option<OpView>,
    pub version: Option<String>,
    pub update: Option<TeamUpdateView>,
    pub ai: Option<AiView>,
    pub owner: Option<String>,
    pub memory_mb: u32,
    pub cpus: u32,
    pub disk_used_bytes: Option<u64>,
    pub disk_size_bytes: Option<u64>,
    /// Other devices: the https address the operator gave, or None.
    pub public_url: Option<String>,
    pub paused_since: Option<String>,
    pub last_reached: Option<String>,
    pub provisional: bool,
    /// From the team's own API (teamapi.rs), as its window's person; None
    /// until read. `working` only while the team runs.
    pub agents: Option<u64>,
    pub working: Option<u64>,
    pub files: Option<u64>,
    /// The account the team's window is signed in with.
    pub signed_in_as: Option<String>,
    /// A team here: the memory the shell would let it use beside the
    /// teams running now (`view::memory_free_mb`; the memory dialog's "2 GB free").
    pub memory_free_mb: Option<u64>,
}

fn update_view(check: Option<&CheckReport>, app_version: &str, now: OffsetDateTime) -> TeamUpdateView {
    use updates::UpdateState as U;
    let state = updates::decide(check, true, app_version);
    let checked_at = check.and_then(|c| c.checked_at.clone());
    let (s, current, latest, notes) = match state {
        U::NoLocalOrg | U::NotChecked | U::NotInstalled => ("not_checked", None, None, None),
        U::UpToDate { current, .. } => ("up_to_date", Some(current), None, None),
        U::Available { current, latest } => ("available", Some(current), Some(latest), check.and_then(|c| c.notes_url.clone())),
        U::DesktopFirst { latest, .. } => ("desktop_first", check.map(|c| c.current.clone()), Some(latest), None),
        U::ManualSteps { latest, notes_url } => ("manual_steps", check.map(|c| c.current.clone()), Some(latest), notes_url),
        U::Failed { .. } => ("failed", check.map(|c| c.current.clone()), None, None),
    };
    let _ = now;
    let bare = |v: Option<String>| v.map(|v| v.trim_start_matches('v').to_string()).filter(|v| !v.is_empty());
    TeamUpdateView { state: s, current: bare(current), latest: bare(latest), notes_url: notes, summary: None, checked_at, takes: view::typical(OpKind::Update) }
}

impl Shell {
    pub fn new(dir: PathBuf, app_version: String, teams: TeamsFile, vm_dir: Option<PathBuf>) -> Shell {
        let shell = Shell {
            dir,
            app_version,
            vm_dir,
            teams: Mutex::new(teams),
            runtimes: Mutex::new(HashMap::new()),
            reach: Mutex::new(HashMap::new()),
            app_update: Mutex::new(AppUpdateView { state: "not_checked", ..Default::default() }),
            owner: Mutex::new((OwnerSigninView { state: "idle", ..Default::default() }, None)),
            connect: Mutex::new(ConnectView { state: "idle", ..Default::default() }),
            connecting: Mutex::new(None),
            pins: Mutex::new(HashMap::new()),
            pending_signins: Mutex::new(HashMap::new()),
            front_team: Mutex::new(None),
            zoom: Mutex::new(HashMap::new()),
            last_deleted: Mutex::new(None),
            quitting: AtomicBool::new(false),
            loops: AtomicBool::new(false),
            prepared: Mutex::new(None),
        };
        let here: Vec<(String, PathBuf)> = {
            let t = shell.teams.lock().unwrap();
            t.teams.iter().filter_map(|t| t.dir(&shell.dir).map(|d| (t.id.clone(), d))).collect()
        };
        let mut rts = shell.runtimes.lock().unwrap();
        for (id, d) in here {
            rts.insert(id.clone(), Arc::new(Runtime::new(&id, d)));
        }
        drop(rts);
        shell
    }

    pub fn runtime(&self, id: &str) -> Option<Arc<Runtime>> {
        self.runtimes.lock().unwrap().get(id).cloned()
    }

    pub fn runtimes(&self) -> Vec<Arc<Runtime>> {
        self.runtimes.lock().unwrap().values().cloned().collect()
    }

    pub fn team(&self, id: &str) -> Option<Team> {
        self.teams.lock().unwrap().get(id).cloned().or_else(|| self.connecting.lock().unwrap().clone().filter(|t| t.id == id))
    }

    pub fn save_teams(&self) -> Result<(), String> {
        self.teams.lock().unwrap().save(&self.dir).map_err(|e| format!("cannot write teams.json: {e}"))
    }

    /// The state of any team, here or elsewhere.
    pub fn team_state(&self, t: &Team) -> (TeamState, Option<Activity>) {
        match t.place {
            Place::Here(_) => self.runtime(&t.id).map(|r| r.state()).unwrap_or((TeamState::Paused, None)),
            Place::Elsewhere { .. } => {
                let r = self.reach.lock().unwrap().get(&t.id).and_then(|r| r.reachable);
                (if r == Some(false) { TeamState::Paused } else { TeamState::Running }, None)
            }
        }
    }

    /// Memory the running teams here use, other than `except`.
    fn running_memory_mb(&self, except: &str) -> (u64, Option<(String, u32)>) {
        let teams = self.teams.lock().unwrap().teams.clone();
        // A machine prepared for setup holds its memory too.
        let mut sum = match self.prepared.lock().unwrap().as_ref() {
            Some(p) if p.id != except => u64::from(crate::teams::DEFAULT_MEMORY_MB),
            _ => 0,
        };
        let mut largest: Option<(String, u32)> = None;
        for t in teams.iter().filter(|t| t.id != except) {
            let Some(l) = t.local() else { continue };
            let Some(rt) = self.runtime(&t.id) else { continue };
            let (st, act) = rt.state();
            if st == TeamState::Running || (st == TeamState::Starting && act != Some(Activity::Pausing)) {
                sum += u64::from(l.memory_mb);
                if largest.as_ref().is_none_or(|(_, m)| l.memory_mb > *m) {
                    largest = Some((t.id.clone(), l.memory_mb));
                }
            }
        }
        (sum, largest)
    }

    pub fn team_view(&self, t: &Team) -> TeamView {
        let now = OffsetDateTime::now_utc();
        let (state, activity) = self.team_state(t);
        match &t.place {
            Place::Here(l) => {
                let rt = self.runtime(&t.id);
                let report = rt.as_ref().and_then(|r| r.report.lock().unwrap().clone());
                let status = report.as_ref().map(Status::from_report);
                let cred = rt.as_ref().and_then(|r| r.credential.lock().unwrap().clone());
                let signed_in = cred.as_ref().map(|c| c.signed_in);
                let op = rt.as_ref().and_then(|r| r.op.lock().unwrap().as_ref().map(TeamOp::view));
                let check = rt.as_ref().and_then(|r| r.check.lock().unwrap().clone());
                let sw = rt.as_ref().map(|r| r.signin.lock().unwrap().clone()).unwrap_or_default();
                let terminals = report.as_ref().map(|r| r.terminals).unwrap_or(0);
                let signin = if sw.opened_at.is_some() {
                    "waiting"
                } else if sw.closed && signed_in != Some(true) {
                    "closed"
                } else {
                    "idle"
                };
                let failure = rt.as_ref().and_then(|r| r.failure.lock().unwrap().clone());
                let pause_first = failure.as_deref().and_then(|f| f.strip_prefix("memory:")).map(str::to_string);
                let reason = match &pause_first {
                    Some(other) => {
                        let other_name = self.teams.lock().unwrap().get(other).map(|o| o.name.clone()).unwrap_or_default();
                        let other_mb = self.teams.lock().unwrap().get(other).and_then(|o| o.local().map(|l| l.memory_mb)).unwrap_or(0);
                        Some(format!("This Mac is low on memory. {other_name} is using {}.", view::gb(u64::from(other_mb))))
                    }
                    None => status.as_ref().and_then(|s| s.message.clone()).filter(|_| state == TeamState::Failed),
                };
                let facts = rt.as_ref().and_then(|r| r.facts.lock().unwrap().clone());
                let working = facts.as_ref().map(|f| f.working).filter(|_| state == TeamState::Running);
                let (running_mb, _) = self.running_memory_mb(&t.id);
                TeamView {
                    id: t.id.clone(),
                    name: t.name.clone(),
                    kind: t.kind,
                    place: "here",
                    url: t.origin(),
                    device: None,
                    state,
                    activity,
                    phrase: view::phrase(state, activity, signed_in, None, working),
                    reason,
                    pause_first,
                    op,
                    version: status.as_ref().and_then(|s| s.kivali.clone()),
                    update: Some(update_view(check.as_ref(), &self.app_version, now)),
                    ai: Some(AiView {
                        provider: "claude",
                        signed_in,
                        email: cred.as_ref().and_then(|c| c.email.clone()),
                        billing: cred.as_ref().and_then(|c| c.billing.clone()),
                        terminal_open: terminals > 0,
                        signin,
                        signed_out_at: l.signed_out_at.clone().filter(|_| signed_in == Some(false)),
                    }),
                    owner: l.owner.clone(),
                    memory_mb: l.memory_mb,
                    cpus: l.cpus,
                    disk_used_bytes: report.as_ref().map(|r| r.disk_used_bytes).filter(|b| *b > 0),
                    disk_size_bytes: report.as_ref().map(|r| r.disk_size_bytes).filter(|b| *b > 0),
                    public_url: l.public_url.clone(),
                    paused_since: t.paused_at.clone().filter(|_| state == TeamState::Paused),
                    last_reached: None,
                    provisional: rt.as_ref().is_some_and(|r| r.provisional.load(Ordering::SeqCst)),
                    agents: facts.as_ref().map(|f| f.agents),
                    working,
                    files: facts.as_ref().map(|f| f.files),
                    signed_in_as: facts.map(|f| f.email),
                    memory_free_mb: Some(view::memory_free_mb(hostinfo::memory_mb(), running_mb)),
                }
            }
            Place::Elsewhere { .. } => {
                let reach = self.reach.lock().unwrap().get(&t.id).cloned().unwrap_or_default();
                let device = t.device();
                let facts = reach.facts.clone();
                TeamView {
                    id: t.id.clone(),
                    name: t.name.clone(),
                    kind: t.kind,
                    place: "elsewhere",
                    url: t.origin(),
                    phrase: view::phrase(state, None, None, device.as_deref(), None),
                    device,
                    state,
                    activity: None,
                    reason: None,
                    pause_first: None,
                    op: None,
                    version: None,
                    update: None,
                    ai: None,
                    owner: None,
                    memory_mb: 0,
                    cpus: 0,
                    disk_used_bytes: None,
                    disk_size_bytes: None,
                    public_url: None,
                    paused_since: None,
                    last_reached: reach
                        .last_reached
                        .and_then(|t| t.format(&time::format_description::well_known::Rfc3339).ok()),
                    provisional: false,
                    agents: facts.as_ref().map(|f| f.agents),
                    working: facts.as_ref().map(|f| f.working).filter(|_| state == TeamState::Running),
                    files: facts.as_ref().map(|f| f.files),
                    signed_in_as: facts.map(|f| f.email),
                    memory_free_mb: None,
                }
            }
        }
    }

    pub fn snapshot(&self) -> Snapshot {
        // Each lock in its own statement: a std Mutex is not re-entrant,
        // and team_view takes several.
        let teams = self.teams.lock().unwrap().clone();
        let views = teams.teams.iter().map(|t| self.team_view(t)).collect();
        let owner = self.owner.lock().unwrap().0.clone();
        let connect = self.connect.lock().unwrap().clone();
        let app_update = self.app_update.lock().unwrap().clone();
        let last_deleted = self.last_deleted.lock().unwrap().clone();
        Snapshot {
            app_version: self.app_version.clone(),
            platform: if cfg!(target_os = "macos") {
                "macos"
            } else if cfg!(windows) {
                "windows"
            } else {
                "linux"
            },
            teams: views,
            host: HostView { memory_mb: hostinfo::memory_mb(), cpus: hostinfo::cpus() },
            settings: SettingsView { start_at_login: teams.start_at_login, ask_before_quit: teams.ask_before_quit },
            app_update,
            owner_signin: owner,
            connect,
            config_dir: self.dir.display().to_string(),
            logs_dir: self.dir.join("logs").display().to_string(),
            last_deleted: last_deleted.map(|(name, freed_bytes)| DeletedView { name, freed_bytes }),
        }
    }

    /// Records a team's port when the supervisor reports a different one.
    fn record_port(&self, id: &str, st: &Status) -> Result<(), String> {
        let Some(port) = st.port else { return Ok(()) };
        let changed = {
            let mut t = self.teams.lock().unwrap();
            match t.get_mut(id).and_then(Team::local_mut) {
                Some(l) if l.port != port => {
                    l.port = port;
                    true
                }
                _ => false,
            }
        };
        if changed {
            self.save_teams()?;
        }
        Ok(())
    }

    /// A port for a new team: the first from 8080 that no other team
    /// here has recorded (the supervisor still moves off one that some
    /// other program holds).
    fn port_hint_except(&self, id: &str) -> u16 {
        let used: Vec<u16> =
            self.teams.lock().unwrap().teams.iter().filter(|t| t.id != id).filter_map(|t| t.local().map(|l| l.port)).collect();
        (FIRST_PORT..u16::MAX).find(|p| !used.contains(p)).unwrap_or(0)
    }

    /// What `up` is sent for a team here: always the owner and the
    /// install environment (the supervisor uses them only on a team's
    /// first install, so a create cut short by a quit still installs on
    /// the next start), the port to try first while it has none, its size.
    fn up_request(&self, id: &str) -> Option<UpRequest> {
        let t = self.team(id)?;
        let l = t.local()?;
        let mut env = vec![format!("KIVALI_SEED_ORG_NAME={}", t.name)];
        if let Some(kind) = t.kind {
            env.push(format!(
                "KIVALI_TEAM_KIND={}",
                match kind {
                    Kind::Work => "work",
                    Kind::Personal => "personal",
                }
            ));
        }
        if let Some(call_me) = l.call_me.as_deref().filter(|c| !c.is_empty()) {
            env.push(format!("KIVALI_OWNER_NAME={call_me}"));
        }
        // Teams here share 127.0.0.1, and cookies ignore the port:
        // each team's sign-in cookies get their own names.
        env.push(format!("KIVALI_COOKIE_SUFFIX={}", t.id));
        Some(UpRequest {
            owner: l.owner.clone(),
            env,
            port_hint: if l.port == 0 { self.port_hint_except(id) } else { 0 },
            memory_mb: l.memory_mb,
            cpus: l.cpus,
            prepare: false,
        })
    }

    /// Keeps `running_at_quit` true to what runs, so a crash or a forced
    /// quit resumes the right teams at the next launch.
    fn set_running_at_quit(&self, id: &str, running: bool) {
        let changed = {
            let mut t = self.teams.lock().unwrap();
            let has = t.running_at_quit.iter().any(|r| r == id);
            if running && !has && t.get(id).is_some() {
                t.running_at_quit.push(id.to_string());
                true
            } else if !running && has {
                t.running_at_quit.retain(|r| r != id);
                true
            } else {
                false
            }
        };
        if changed {
            let _ = self.save_teams();
        }
    }

    fn set_paused_at(&self, id: &str) {
        if let Some(t) = self.teams.lock().unwrap().get_mut(id) {
            t.paused_at = Some(now_rfc3339());
        }
        let _ = self.save_teams();
    }

    /// The signed-out date: a credential that went from signed in to signed out
    /// stamps `signed_out_at` (once); any sign-in clears it.
    fn note_signin(&self, id: &str, before: Option<bool>, after: Option<bool>) {
        let changed = {
            let mut t = self.teams.lock().unwrap();
            match t.get_mut(id).and_then(Team::local_mut) {
                Some(l) => {
                    let next = signed_out_stamp(l.signed_out_at.clone(), before, after, now_rfc3339);
                    let changed = next != l.signed_out_at;
                    l.signed_out_at = next;
                    changed
                }
                None => false,
            }
        };
        if changed {
            let _ = self.save_teams();
        }
    }
}

/// What `signed_out_at` becomes: stamped `now` on a signed-in to
/// signed-out transition (an earlier stamp kept), cleared by a sign-in,
/// otherwise as it was.
fn signed_out_stamp(was: Option<String>, before: Option<bool>, after: Option<bool>, now: impl FnOnce() -> String) -> Option<String> {
    match (before, after) {
        (_, Some(true)) => None,
        (Some(true), Some(false)) => was.or_else(|| Some(now())),
        _ => was,
    }
}

/// Tells every surface to redraw: tray, menus, team windows (page or
/// web app), pages.
pub fn changed(app: &AppHandle) {
    let a = app.clone();
    let _ = app.run_on_main_thread(move || {
        menus::refresh(&a);
        windows::sync_all(&a);
    });
    let _ = app.emit(CHANGED_EVENT, ());
}

fn shell(app: &AppHandle) -> tauri::State<'_, Shell> {
    app.state::<Shell>()
}

fn label(text: &str) -> Progress {
    Progress { label: text.to_string(), stage: None }
}

/// Runs one long operation of a team on its own thread. One at a time
/// per team; asking for a second while one runs is refused.
pub fn run_op<F>(app: &AppHandle, rt: Arc<Runtime>, kind: OpKind, work: F) -> Result<(), String>
where
    F: FnOnce(&AppHandle, &Shell, &Runtime, &mut dyn FnMut(Progress)) -> Result<(), String> + Send + 'static,
{
    if shell(app).quitting.load(Ordering::SeqCst) {
        return Err("Kivali is quitting.".into());
    }
    {
        let mut op = rt.op.lock().unwrap();
        if let Some(o) = op.as_ref().filter(|o| o.running) {
            return Err(format!("{} is busy ({:?}).", rt.id, o.kind).to_lowercase());
        }
        *op = Some(TeamOp::new(kind));
    }
    changed(app);
    let app = app.clone();
    std::thread::spawn(move || {
        let sh = app.state::<Shell>();
        let app2 = app.clone();
        let rt2 = rt.clone();
        let mut on_progress = |p: Progress| {
            if let Some(o) = rt2.op.lock().unwrap().as_mut() {
                o.progress(&p);
            }
            changed(&app2);
        };
        let result = work(&app, &sh, &rt, &mut on_progress);
        {
            let mut op = rt.op.lock().unwrap();
            if let Some(o) = op.as_mut() {
                o.running = false;
                match &result {
                    Ok(()) => o.finished = true,
                    Err(e) => o.error = Some(e.clone()),
                }
            }
        }
        rt.op_done.notify_all();
        if result.is_ok() {
            // Running again (or still): what its own API says.
            teamapi::refresh(&app, &rt.id);
        }
        changed(&app);
    });
    Ok(())
}

fn sup_err(e: SupError) -> String {
    e.to_string()
}

/// Boots a team (creating it on the first call) and records what came
/// back. Used by create, resume and retry.
fn boot(app: &AppHandle, sh: &Shell, rt: &Runtime, req: UpRequest, progress: &mut dyn FnMut(Progress)) -> Result<(), String> {
    *rt.failure.lock().unwrap() = None;
    rt.ensure_supervisor(sh.vm_dir.as_deref())?;
    let result = rt.supervisor.up(&req, progress);
    let st = match result {
        Ok(st) => st,
        Err(e) => {
            *rt.failure.lock().unwrap() = Some(e.to_string());
            let _ = rt.refresh_status();
            return Err(sup_err(e));
        }
    };
    let _ = rt.refresh_status();
    if st.port.is_none() {
        return Err("The supervisor started the team but reported no port.".into());
    }
    sh.record_port(&rt.id, &st)?;
    sh.set_running_at_quit(&rt.id, true);
    // Other devices: the external address (or its absence) as the team's
    // own sign-in allows it; a no-op when unchanged, and how one changed
    // while the team was paused reaches it.
    let external = sh.team(&rt.id).and_then(|t| t.local().and_then(|l| l.public_url.clone()));
    if let Err(e) = rt.supervisor.set_address(external.as_deref(), progress) {
        eprintln!("kivali: {}: external address: {e}", rt.id);
    }
    let before = rt.credential.lock().unwrap().as_ref().map(|c| c.signed_in);
    let after = rt.refresh_credential().map(|c| c.signed_in);
    sh.note_signin(&rt.id, before, after);
    after_started(app, rt);
    Ok(())
}

/// The answers setup collects (types.ts `NewTeam`).
#[derive(Debug, Clone, serde::Deserialize)]
pub struct NewTeam {
    pub name: String,
    pub kind: Kind,
    pub owner: String,
    /// How the agents refer to the person; empty for none.
    #[serde(default)]
    pub call_me: String,
}

/// "Create a team" chosen: starts booting a machine for it at once, so
/// setup's own steps overlap the wait. It installs nothing until
/// `create_team` names the owner. Quietly does nothing when one is
/// already preparing or would not fit in memory.
pub fn prepare_team(app: &AppHandle) -> Result<(), String> {
    let sh = shell(app);
    if sh.quitting.load(Ordering::SeqCst) {
        return Ok(());
    }
    let (running, _) = sh.running_memory_mb("");
    if u64::from(crate::teams::DEFAULT_MEMORY_MB) > view::memory_free_mb(hostinfo::memory_mb(), running) {
        return Ok(());
    }
    // The slot is checked and filled under one hold: two quick clicks
    // prepare one machine.
    let (rt, first, wait) = {
        let mut slot = sh.prepared.lock().unwrap();
        if slot.is_some() {
            return Ok(());
        }
        let teams_dir = sh.dir.join(crate::teams::TEAMS_DIR);
        crate::paths::ensure_dir(&teams_dir).map_err(|e| format!("cannot make {}: {e}", teams_dir.display()))?;
        // An id no team has, in a folder made here and now: an existing
        // folder (another team's) is never taken over.
        let (id, dir) = loop {
            let mut b = [0u8; 2];
            let _ = getrandom::fill(&mut b);
            let id = crate::teams::new_id("team", u16::from_le_bytes(b));
            if sh.teams.lock().unwrap().get(&id).is_some() {
                continue;
            }
            let dir = teams_dir.join(&id);
            let mut builder = std::fs::DirBuilder::new();
            platform::owner_only_dir(&mut builder);
            match builder.create(&dir) {
                Ok(()) => break (id, dir),
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(e) => return Err(format!("cannot make {}: {e}", dir.display())),
            }
        };
        crate::paths::write_atomic(&dir.join(PREPARED_MARKER), b"", 0o600).map_err(|e| e.to_string())?;
        let rt = Arc::new(Runtime::new(&id, dir));
        rt.provisional.store(true, Ordering::SeqCst);
        let (go, wait) = std::sync::mpsc::channel::<Option<UpRequest>>();
        let first = UpRequest {
            owner: None,
            env: Vec::new(),
            port_hint: sh.port_hint_except(""),
            memory_mb: crate::teams::DEFAULT_MEMORY_MB,
            cpus: crate::teams::DEFAULT_CPUS,
            prepare: true,
        };
        *slot = Some(Prepared { id, rt: rt.clone(), go });
        (rt, first, wait)
    };
    run_op(app, rt, OpKind::Create, move |app, sh, rt, progress| {
        progress(label("Starting the Kivali supervisor"));
        rt.ensure_supervisor(sh.vm_dir.as_deref())?;
        rt.supervisor.up(&first, progress).map_err(sup_err)?;
        match wait.recv() {
            Ok(Some(req)) => boot(app, sh, rt, req, progress),
            _ => Err("setup was not finished".into()),
        }
    })
}

/// Ends the prepared machine nobody adopted. Blocks.
fn discard_prepared(sh: &Shell) {
    let p = sh.prepared.lock().unwrap().take();
    if let Some(p) = p {
        discard(p);
    }
}

/// Setup left the new-team path (closed, Connect chosen, back to the
/// welcome): the prepared machine goes, off the caller's thread.
pub fn discard_prepared_later(app: &AppHandle) {
    let app = app.clone();
    std::thread::spawn(move || discard_prepared(&app.state::<Shell>()));
}

/// A prepared machine's end: its operation stops waiting for an install,
/// a boot still running is cancelled (`down` cancels a running `up`), its
/// supervisor deletes what it made and exits, and its folder goes.
fn discard(p: Prepared) {
    drop(p.go);
    if p.rt.op_running().is_some() && p.rt.supervisor.is_listening() {
        if let Err(e) = p.rt.supervisor.down(false, &mut |_| {}) {
            eprintln!("kivali: {}: stopping a prepared machine: {e}", p.id);
        }
    }
    p.rt.wait_idle();
    if p.rt.supervisor.is_listening() {
        if let Err(e) = p.rt.supervisor.destroy(true, &mut |_| {}) {
            eprintln!("kivali: {}: discarding a prepared machine: {e}", p.id);
        }
    }
    p.rt.forget_sidecar();
    if let Err(e) = std::fs::remove_dir_all(&p.rt.dir) {
        eprintln!("kivali: removing {}: {e}", p.rt.dir.display());
    }
}

/// At launch: machines prepared by a setup that never finished (a crash,
/// a forced quit) are deleted, supervisor first if one still serves.
fn remove_left_over_prepared(sh: &Shell) {
    let Ok(rd) = std::fs::read_dir(sh.dir.join(crate::teams::TEAMS_DIR)) else { return };
    let known: Vec<String> = sh.teams.lock().unwrap().teams.iter().map(|t| t.id.clone()).collect();
    for e in rd.flatten() {
        let dir = e.path();
        let id = e.file_name().to_string_lossy().to_string();
        if !dir.join(PREPARED_MARKER).exists() || known.contains(&id) {
            continue;
        }
        let sup = Supervisor::new(&dir);
        if sup.is_listening() {
            if let Err(e) = sup.destroy(true, &mut |_| {}) {
                eprintln!("kivali: {id}: removing a left-over prepared machine: {e}");
                continue;
            }
            if let Some(sc) = Sidecar::adopt(&dir, sup.endpoint()) {
                sc.stop(&mut || Ok(()), STOP_TIMEOUTS);
            }
        }
        if let Err(e) = std::fs::remove_dir_all(&dir) {
            eprintln!("kivali: removing {}: {e}", dir.display());
        }
    }
}

/// Setup's step 2 Continue: records the team and gets it ready: on the
/// machine `prepare_team` started, when there is one.
pub fn create_team(app: &AppHandle, t: NewTeam) -> Result<String, String> {
    let sh = shell(app);
    let name = t.name.trim().to_string();
    if name.is_empty() {
        return Err("Give the team a name.".into());
    }
    if name.chars().count() > 64 {
        return Err("Keep the name under 64 characters.".into());
    }
    let (running, _) = sh.running_memory_mb("");
    let free = view::memory_free_mb(hostinfo::memory_mb(), running);
    let has_prepared = sh.prepared.lock().unwrap().is_some();
    if !has_prepared && u64::from(crate::teams::DEFAULT_MEMORY_MB) > free {
        return Err(format!(
            "This Mac doesn't have {} free for another team while the others run. Pause one first.",
            view::gb(u64::from(crate::teams::DEFAULT_MEMORY_MB))
        ));
    }
    let owner = t.owner.trim().to_lowercase();
    let signed_in_as = sh.owner.lock().unwrap().0.email.clone();
    if signed_in_as.as_deref() != Some(owner.as_str()) {
        return Err("Sign in with Google first.".into());
    }
    if let Some(id) = adopt_prepared(app, &sh, &name, t.kind, &owner, &t.call_me)? {
        return Ok(id);
    }
    let id = {
        let mut teams = sh.teams.lock().unwrap();
        let mut rnd = || {
            let mut b = [0u8; 2];
            let _ = getrandom::fill(&mut b);
            u16::from_le_bytes(b)
        };
        let id = teams.add_here(&name, t.kind, &owner, &mut rnd);
        set_call_me(&mut teams, &id, &t.call_me);
        teams.last_open = Some(id.clone());
        id
    };
    sh.save_teams()?;
    let dir = sh.team(&id).and_then(|t| t.dir(&sh.dir)).ok_or("the new team has no folder")?;
    let rt = Arc::new(Runtime::new(&id, dir));
    rt.provisional.store(true, Ordering::SeqCst);
    sh.runtimes.lock().unwrap().insert(id.clone(), rt.clone());
    let req = sh.up_request(&id).ok_or("the new team has no folder")?;
    run_op(app, rt, OpKind::Create, move |app, sh, rt, progress| {
        progress(label("Starting the Kivali supervisor"));
        boot(app, sh, rt, req, progress)
    })?;
    Ok(id)
}

/// Records setup's "What should your agents call you?" on a new team
/// (see clean_call_me; empty is none).
fn set_call_me(teams: &mut TeamsFile, id: &str, call_me: &str) {
    let c = clean_call_me(call_me);
    if let Some(l) = teams.get_mut(id).and_then(Team::local_mut) {
        l.call_me = (!c.is_empty()).then_some(c);
    }
}

/// The name as the server accepts it (internal/owner CleanName): trimmed,
/// one line, at most 40 characters, without `:`, `` ` `` or `>` (they
/// would break the `> Name:` marker) or invisible formatting characters.
/// A refused KIVALI_OWNER_NAME would stop the server at boot.
pub fn clean_call_me(call_me: &str) -> String {
    let invisible = |c: char| {
        matches!(c, '\u{00AD}' | '\u{061C}' | '\u{180E}' | '\u{200B}'..='\u{200F}' | '\u{2028}'..='\u{202E}'
            | '\u{2060}'..='\u{2064}' | '\u{2066}'..='\u{206F}' | '\u{FEFF}' | '\u{FFF9}'..='\u{FFFB}'
            | '\u{0600}'..='\u{0605}' | '\u{06DD}' | '\u{070F}' | '\u{0890}'..='\u{0891}' | '\u{08E2}'
            | '\u{110BD}' | '\u{110CD}' | '\u{13430}'..='\u{1343F}' | '\u{1BCA0}'..='\u{1BCA3}'
            | '\u{1D173}'..='\u{1D17A}' | '\u{E0001}' | '\u{E0020}'..='\u{E007F}')
    };
    let kept: String = call_me.chars().filter(|&c| !c.is_control() && !invisible(c) && !matches!(c, ':' | '`' | '>')).collect();
    kept.trim().chars().take(40).collect::<String>().trim_end().to_string()
}

/// Hands the prepared machine its team: listed under the machine's id,
/// and its waiting create operation goes on to install. None when there
/// is no prepared machine, or it already failed (then it is discarded,
/// in the background, and the caller creates afresh). The slot is held
/// throughout, so Quit (which discards the slot first) sees either the
/// prepared machine or the team it became, never neither.
fn adopt_prepared(app: &AppHandle, sh: &Shell, name: &str, kind: Kind, owner: &str, call_me: &str) -> Result<Option<String>, String> {
    let mut slot = sh.prepared.lock().unwrap();
    if sh.quitting.load(Ordering::SeqCst) {
        return Err("Kivali is quitting.".into());
    }
    let Some(p) = slot.take() else { return Ok(None) };
    if p.rt.op_running() != Some(OpKind::Create) {
        drop(slot);
        std::thread::spawn(move || discard(p));
        return Ok(None);
    }
    {
        let mut teams = sh.teams.lock().unwrap();
        teams.add_here_as(&p.id, name, kind, owner);
        set_call_me(&mut teams, &p.id, call_me);
        teams.last_open = Some(p.id.clone());
    }
    let req = sh.up_request(&p.id);
    let failed = match (sh.save_teams(), &req) {
        (Err(e), _) => Some(e),
        (Ok(()), None) => Some("the new team has no folder".to_string()),
        (Ok(()), Some(_)) => None,
    };
    if let Some(e) = failed {
        // Back as it was: the team unlisted, the machine still prepared.
        sh.teams.lock().unwrap().remove(&p.id);
        let _ = sh.save_teams();
        *slot = Some(p);
        return Err(e);
    }
    let _ = std::fs::remove_file(p.rt.dir.join(PREPARED_MARKER));
    sh.runtimes.lock().unwrap().insert(p.id.clone(), p.rt.clone());
    drop(slot);
    if p.go.send(req).is_err() {
        // It ended in the meantime: its op says why (setup offers Try again).
        eprintln!("kivali: {}: the prepared machine stopped before its install", p.id);
    }
    changed(app);
    Ok(Some(p.id))
}

/// Removes a team whose setup has not finished, and everything it made.
/// Blocks until it is gone. Answers setup's answers (for Try again).
pub fn abandon_team_blocking(app: &AppHandle, id: &str) -> Result<Option<NewTeam>, String> {
    let sh = shell(app);
    let rt = sh.runtime(id).ok_or("no such team")?;
    if !rt.provisional.load(Ordering::SeqCst) {
        return Err("That team is set up; delete it from Settings instead.".into());
    }
    let team = sh.team(id).ok_or("no such team")?;
    let answers = team.local().map(|l| NewTeam {
        name: team.name.clone(),
        kind: team.kind.unwrap_or(Kind::Work),
        owner: l.owner.clone().unwrap_or_default(),
        call_me: l.call_me.clone().unwrap_or_default(),
    });
    // No new supervisor from here on: a create still starting one gets an
    // error instead (and stops what it started).
    rt.sidecar.lock().unwrap().closed = true;
    // Destroy cancels a running `up` in the supervisor. A create may be
    // starting the supervisor right now; wait for it to answer, or for
    // the create to end.
    loop {
        if rt.supervisor.is_listening() {
            if let Err(e) = rt.supervisor.destroy(true, &mut |_| {}) {
                eprintln!("kivali: {id}: abandoning: {e}");
            }
            break;
        }
        if rt.op_running().is_none() {
            break;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    rt.wait_idle();
    rt.forget_sidecar();
    remove_team_files(&sh, &team);
    sh.runtimes.lock().unwrap().remove(id);
    sh.teams.lock().unwrap().remove(id);
    sh.save_teams()?;
    let a = app.clone();
    let id2 = id.to_string();
    let _ = app.run_on_main_thread(move || windows::close_team_window(&a, &id2));
    changed(app);
    Ok(answers)
}

/// Setup's Try again after a failed create: removes the failed team, then creates it again with
/// the same answers.
pub fn retry_create(app: &AppHandle, id: &str) -> Result<String, String> {
    let answers = abandon_team_blocking(app, id)?;
    create_team(app, answers.ok_or("nothing to retry")?)
}

/// "Open <team>" at the end of setup: the team is no longer setup's to
/// remove.
pub fn finish_setup(app: &AppHandle, id: &str) {
    if let Some(rt) = shell(app).runtime(id) {
        rt.provisional.store(false, Ordering::SeqCst);
    }
    changed(app);
}

/// Deletes a team folder the shell made (teams/<id>).
fn remove_team_files(sh: &Shell, team: &Team) {
    let Some(l) = team.local() else { return };
    let dir = sh.dir.join(&l.dir);
    if dir.starts_with(sh.dir.join(crate::teams::TEAMS_DIR)) && dir != sh.dir.join(crate::teams::TEAMS_DIR) {
        if let Err(e) = std::fs::remove_dir_all(&dir) {
            if e.kind() != std::io::ErrorKind::NotFound {
                eprintln!("kivali: removing {}: {e}", dir.display());
            }
        }
    }
}

/// Resume (or Try again): refused with `memory:<team id>` when the team
/// would not fit beside the ones running.
pub fn resume_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let sh = shell(app);
    let rt = sh.runtime(id).ok_or("no such team on this Mac")?;
    if rt.is_running() && rt.failure.lock().unwrap().is_none() {
        return Ok(());
    }
    let req = sh.up_request(id).ok_or("no such team")?;
    let (running, largest) = sh.running_memory_mb(id);
    if u64::from(req.memory_mb) > view::memory_free_mb(hostinfo::memory_mb(), running) {
        if let Some((other, _)) = largest {
            return Err(format!("memory:{other}"));
        }
    }
    run_op(app, rt, OpKind::Resume, move |app, sh, rt, progress| {
        progress(label("Starting the Kivali supervisor"));
        boot(app, sh, rt, req, progress)
    })
}

/// Resume where nobody is asked first (launch, the team page's Try again): a team
/// that would not fit shows as couldn't start, with the team to pause
/// (`pause_first`).
pub fn resume_or_record(app: &AppHandle, id: &str) -> Result<(), String> {
    match resume_team(app, id) {
        Err(e) if e.starts_with("memory:") => {
            if let Some(rt) = shell(app).runtime(id) {
                *rt.failure.lock().unwrap() = Some(e);
            }
            changed(app);
            Ok(())
        }
        r => r,
    }
}

/// Pause one team, then resume another.
pub fn pause_and_resume(app: &AppHandle, pause: &str, resume: &str) -> Result<(), String> {
    let sh = shell(app);
    let p = sh.runtime(pause).ok_or("no such team")?;
    let r = sh.runtime(resume).ok_or("no such team")?;
    let req = sh.up_request(resume).ok_or("no such team")?;
    let app2 = app.clone();
    let resume_id = resume.to_string();
    run_op(app, p, OpKind::Pause, move |_app, sh, rt, progress| {
        rt.supervisor.down(false, progress).map_err(sup_err)?;
        let _ = rt.refresh_status();
        sh.set_paused_at(&rt.id);
        sh.set_running_at_quit(&rt.id, false);
        let rid = resume_id.clone();
        std::thread::spawn(move || {
            let r = run_op(&app2, r, OpKind::Resume, move |app, sh, rt, progress| boot(app, sh, rt, req, progress));
            if let Err(e) = r {
                eprintln!("kivali: {rid}: {e}");
            }
        });
        Ok(())
    })
}

pub fn pause_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let rt = shell(app).runtime(id).ok_or("no such team on this Mac")?;
    run_op(app, rt, OpKind::Pause, |_app, sh, rt, progress| {
        *rt.failure.lock().unwrap() = None;
        if rt.supervisor.is_listening() {
            rt.supervisor.down(false, progress).map_err(sup_err)?;
        }
        let _ = rt.refresh_status();
        sh.set_paused_at(&rt.id);
        sh.set_running_at_quit(&rt.id, false);
        Ok(())
    })
}

pub fn update_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let rt = shell(app).runtime(id).ok_or("no such team on this Mac")?;
    let id = id.to_string();
    run_op(app, rt, OpKind::Update, move |app, sh, rt, progress| {
        let before = rt.status().and_then(|s| s.kivali);
        let name = sh.team(&id).map(|t| t.name).unwrap_or_default();
        match rt.supervisor.upgrade(progress) {
            Ok(st) => {
                let _ = rt.refresh_status();
                notify_background(
                    app,
                    &format!("{name} is updated"),
                    &format!(
                        "It's on Kivali {}. Agents have picked up where they left off.",
                        short_version(st.kivali.as_deref().unwrap_or_default())
                    ),
                    platform::NotifyTarget::Team(id.clone()),
                );
                check_now(app, rt);
                Ok(())
            }
            Err(e) => {
                let _ = rt.refresh_status();
                if rt.is_running() {
                    notify_background(
                        app,
                        &format!("{name} couldn't update"),
                        &format!(
                            "It's still on {} and running. Open Settings for details.",
                            short_version(before.as_deref().unwrap_or_default())
                        ),
                        platform::NotifyTarget::Settings(format!("team/{id}")),
                    );
                }
                Err(sup_err(e))
            }
        }
    })
}

/// "0.17" from "0.17.0", as the design names releases in sentences.
pub fn short_version(v: &str) -> String {
    let v = v.trim().trim_start_matches('v');
    match v.split('.').collect::<Vec<_>>()[..] {
        [a, b, "0"] => format!("{a}.{b}"),
        _ => v.to_string(),
    }
}

/// Opens Claude Code's sign-in in Terminal, attached to the team's
/// server container: the setup step, Sign in, and Sign in again, which
/// can switch to another account or way of billing. It first removes any
/// cloud provider's variables from the CLI's settings (a sign-in setup
/// applied here, a Bedrock or Vertex sign-in), which would otherwise
/// outrank the sign-in made in Terminal. The watch loop follows it until
/// the sign-in differs from the one there then. Blocks on the
/// supervisor: never call it on the main thread.
pub fn open_claude_signin(app: &AppHandle, id: &str) -> Result<(), String> {
    let sh = shell(app);
    let rt = sh.runtime(id).ok_or("no such team on this Mac")?;
    if !rt.is_running() {
        return Err("Claude's sign-in opens once the team is running.".into());
    }
    let cleared = rt.supervisor.clear_provider().map_err(|e| format!("Couldn't clear the previous sign-in: {}", e.sentence()))?;
    if !cleared.cleared.is_empty() {
        eprintln!("kivali: {id}: cleared {} before Claude's sign-in", cleared.cleared.join(" "));
        let before = rt.credential.lock().unwrap().as_ref().map(|c| c.signed_in);
        let after = rt.refresh_credential().map(|c| c.signed_in);
        sh.note_signin(id, before, after);
    }
    let binary = sidecar::binary_path()?;
    platform::open_terminal(&binary, &rt.dir)?;
    let from = rt.credential.lock().unwrap().as_ref().map(signin_key);
    *rt.signin.lock().unwrap() = SigninWatch { opened_at: Some(Instant::now()), from, saw_terminal: false, closed: false };
    start_loops(app);
    changed(app);
    Ok(())
}

/// What a sign-in setup's form starts from: the model ids the team runs
/// and the setup saved now, without its secrets. Blocks on the
/// supervisor.
pub fn credential_setup(app: &AppHandle, id: &str) -> Result<SetupInfo, String> {
    let rt = shell(app).runtime(id).ok_or("no such team on this Mac")?;
    if !rt.is_running() {
        return Err("This can be set up once the team is running.".into());
    }
    rt.supervisor.setup_info().map_err(|e| e.sentence())
}

/// Signs the team's Claude in with a sign-in setup (the supervisor hands
/// it to the Claude driver, writes the CLI's settings and checks each
/// model), then records the new sign-in as the watch loop would. Blocks
/// on the supervisor for as long as the checks take.
pub fn apply_credential_setup(app: &AppHandle, id: &str, req: SetupRequest) -> Result<SetupResult, String> {
    let sh = shell(app);
    let rt = sh.runtime(id).ok_or("no such team on this Mac")?;
    if !rt.is_running() {
        return Err("This can be set up once the team is running.".into());
    }
    let res = rt.supervisor.apply_setup(&req).map_err(|e| e.sentence())?;
    let before = rt.credential.lock().unwrap().as_ref().map(|c| c.signed_in);
    *rt.credential.lock().unwrap() = Some(res.credential.clone());
    sh.note_signin(id, before, Some(res.credential.signed_in));
    // A sign-in in Terminal that was still open is over: this one
    // replaced it.
    *rt.signin.lock().unwrap() = SigninWatch::default();
    changed(app);
    Ok(res)
}

/// E8, then F5: deletes a team on this computer for good.
pub fn delete_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let sh = shell(app);
    let team = sh.team(id).ok_or("no such team")?;
    let rt = sh.runtime(id).ok_or("only a team on this Mac can be deleted")?;
    let name = team.name.clone();
    let id = id.to_string();
    run_op(app, rt, OpKind::Delete, move |app, sh, rt, progress| {
        rt.ensure_supervisor(sh.vm_dir.as_deref())?;
        let freed = rt.supervisor.destroy(true, progress).map_err(sup_err)?;
        rt.forget_sidecar();
        remove_team_files(sh, &team);
        *sh.last_deleted.lock().unwrap() = Some((name, freed));
        sh.teams.lock().unwrap().remove(&id);
        sh.save_teams()?;
        sh.runtimes.lock().unwrap().remove(&id);
        let a = app.clone();
        let id2 = id.clone();
        let _ = app.run_on_main_thread(move || windows::close_team_window(&a, &id2));
        Ok(())
    })
}

/// Forgets a team elsewhere; it keeps running there.
pub fn remove_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let sh = shell(app);
    let t = sh.team(id).ok_or("no such team")?;
    if t.is_here() {
        return Err("A team on this Mac is deleted, not removed.".into());
    }
    sh.teams.lock().unwrap().remove(id);
    sh.save_teams()?;
    sh.reach.lock().unwrap().remove(id);
    let a = app.clone();
    let id2 = id.to_string();
    let _ = app.run_on_main_thread(move || windows::close_team_window(&a, &id2));
    changed(app);
    Ok(())
}

/// Other devices: records the https address the operator put in
/// front of the team (`None` turns it off). Kivali serves no network
/// listener of its own: Tailscale or a reverse proxy on this Mac reaches
/// the team at `http://127.0.0.1:<port>`. The address must be https (the
/// connect side refuses anything else off loopback) and must answer
/// `GET /api/v1/login` as this team.
pub async fn set_public_url(app: AppHandle, id: String, address: Option<String>) -> Result<Option<String>, String> {
    let sh = app.state::<Shell>();
    let team = sh.team(&id).ok_or("no such team")?;
    if !team.is_here() {
        return Err("Only a team on this Mac has other devices.".into());
    }
    let url = match address.as_deref().map(str::trim).filter(|a| !a.is_empty()) {
        None => None,
        Some(a) => {
            let origin = orgurl::normalize_org_url(a)?;
            if origin.scheme() != "https" {
                return Err("Use the https address other computers reach it at.".into());
            }
            let info = orgurl::fetch_login(&origin).await.map_err(|e| {
                if e.starts_with("That address answers") {
                    e
                } else {
                    format!("{e} Check it's set up to reach {} on this Mac.", team.name)
                }
            })?;
            if !info.org_name.is_empty() && info.org_name != team.name {
                return Err(format!("That address answers as {}, not {}.", info.org_name, team.name));
            }
            Some(orgurl::origin_string(&origin))
        }
    };
    {
        let mut t = sh.teams.lock().unwrap();
        let l = t.get_mut(&id).and_then(Team::local_mut).ok_or("no such team on this Mac")?;
        l.public_url = url.clone();
    }
    sh.save_teams()?;
    // The team's own sign-in accepts the address only once its server
    // knows it: now, if it runs (a paused one gets it at its next start).
    if let Some(rt) = sh.runtime(&id).filter(|r| r.is_running()) {
        let apply = url.clone();
        let r = run_op(&app, rt, OpKind::Address, move |_app, _sh, rt, progress| {
            rt.supervisor.set_address(apply.as_deref(), progress).map_err(sup_err)
        });
        if let Err(e) = r {
            // Busy: saved all the same, and applied at its next start.
            eprintln!("kivali: {id}: the external address waits for the next start: {e}");
        }
    }
    changed(&app);
    Ok(url)
}

pub fn set_resources(app: &AppHandle, id: &str, memory_mb: u32, cpus: u32) -> Result<(), String> {
    let sh = shell(app);
    let host = hostinfo::memory_mb();
    if !(2048..=host as u32).contains(&memory_mb) || !memory_mb.is_multiple_of(1024) {
        return Err("Pick a memory size from the list.".into());
    }
    if cpus == 0 || u64::from(cpus) > hostinfo::cpus() {
        return Err("Pick a number of CPUs from the list.".into());
    }
    {
        let mut t = sh.teams.lock().unwrap();
        let l = t.get_mut(id).and_then(Team::local_mut).ok_or("no such team on this Mac")?;
        l.memory_mb = memory_mb;
        l.cpus = cpus;
    }
    sh.save_teams()?;
    changed(app);
    Ok(())
}

/// The release check, once now and then daily, after a team came up.
fn after_started(app: &AppHandle, rt: &Runtime) {
    check_now_id(app, &rt.id);
    if !rt.check_loop.swap(true, Ordering::SeqCst) {
        let app = app.clone();
        let id = rt.id.clone();
        std::thread::spawn(move || loop {
            std::thread::sleep(CHECK_EVERY);
            let Some(rt) = app.state::<Shell>().runtime(&id) else { return };
            run_check_blocking(&app, &rt);
        });
    }
}

pub fn run_check_blocking(app: &AppHandle, rt: &Runtime) {
    let report = match rt.supervisor.check() {
        Ok(r) => r,
        Err(e) => {
            let previous_ok = rt.check.lock().unwrap().as_ref().and_then(|r| r.last_ok_at.clone());
            CheckReport {
                error: Some(e.to_string()),
                last_ok_at: previous_ok,
                current: rt.status().and_then(|s| s.kivali).unwrap_or_default(),
                ..Default::default()
            }
        }
    };
    *rt.check.lock().unwrap() = Some(report);
    changed(app);
}

fn check_now(app: &AppHandle, rt: &Runtime) {
    check_now_id(app, &rt.id);
}

pub fn check_now_id(app: &AppHandle, id: &str) {
    let app = app.clone();
    let id = id.to_string();
    std::thread::spawn(move || {
        if let Some(rt) = app.state::<Shell>().runtime(&id) {
            run_check_blocking(&app, &rt);
        }
    });
}

/// A notification, only while Kivali is not the app in front;
/// clicking it opens `target` (lib.rs routes it).
pub fn notify_background(app: &AppHandle, title: &str, body: &str, target: platform::NotifyTarget) {
    if !platform::app_is_active() {
        platform::notify(app, title, body, target);
    }
}

// ---- Owner sign-in ----

/// Which owner sign-in is current: an answer from an older one (the
/// person pressed again, or reset, while it was being checked) is dropped.
static OWNER_GEN: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

pub fn owner_signin_start(app: &AppHandle) -> Result<(), String> {
    let sh = shell(app);
    let gen = OWNER_GEN.fetch_add(1, Ordering::SeqCst) + 1;
    // Dropping the previous one closes its listener without an answer.
    let old = sh.owner.lock().unwrap().1.take();
    drop(old);
    let app2 = app.clone();
    let (pending, url) = ownersignin::start(move |r| {
        let sh = app2.state::<Shell>();
        {
            let mut g = sh.owner.lock().unwrap();
            if OWNER_GEN.load(Ordering::SeqCst) != gen {
                return;
            }
            g.1 = None;
            g.0 = match r {
                Ok(email) => OwnerSigninView { state: "signed_in", email: Some(email), error: None },
                Err(e) => OwnerSigninView { state: "failed", email: None, error: Some(e) },
            };
        }
        changed(&app2);
    })
    .map_err(|e| format!("cannot listen for the browser: {e}"))?;
    *sh.owner.lock().unwrap() = (OwnerSigninView { state: "waiting", email: None, error: None }, Some(pending));
    crate::open_url_in_browser(app, url.as_str())?;
    changed(app);
    Ok(())
}

pub fn owner_signin_reset(app: &AppHandle) {
    OWNER_GEN.fetch_add(1, Ordering::SeqCst);
    let old = std::mem::replace(&mut *shell(app).owner.lock().unwrap(), (OwnerSigninView { state: "idle", ..Default::default() }, None));
    drop(old);
    changed(app);
}

// ---- Connect ----

/// `connect_check`'s answer and error (types.ts).
#[derive(Serialize)]
pub struct ConnectFound {
    pub origin: String,
    pub name: String,
    pub host: String,
}

#[derive(Serialize, Debug)]
pub struct ConnectError {
    pub kind: &'static str,
    pub message: String,
}

pub async fn connect_check(address: &str) -> Result<ConnectFound, ConnectError> {
    let origin = orgurl::normalize_org_url(address).map_err(|m| ConnectError { kind: "invalid", message: m })?;
    let info = orgurl::fetch_login(&origin).await.map_err(|m| ConnectError {
        kind: if m.starts_with("That address answers") { "not_kivali" } else { "unreachable" },
        message: m,
    })?;
    let host = origin.host_str().unwrap_or_default().to_string();
    let name = if info.org_name.is_empty() { host.clone() } else { info.org_name };
    Ok(ConnectFound { origin: orgurl::origin_string(&origin), name, host })
}

/// Runs the team's own sign-in in a window that shows once it worked.
pub fn connect_signin(app: &AppHandle, origin: &str, name: &str) -> Result<(), String> {
    let sh = shell(app);
    let url = orgurl::normalize_org_url(origin)?;
    let existing = sh
        .teams
        .lock()
        .unwrap()
        .teams
        .iter()
        .find(|t| t.origin().as_deref() == Some(orgurl::origin_string(&url).as_str()))
        .map(|t| t.id.clone());
    if let Some(id) = existing {
        windows::close_setup(app);
        windows::show_team(app, &id);
        return Ok(());
    }
    let mut scratch = TeamsFile::default();
    let id = scratch.add_elsewhere(&url, name);
    let team = scratch.teams.remove(0);
    *sh.connecting.lock().unwrap() = Some(team);
    *sh.connect.lock().unwrap() =
        ConnectView { state: "signing_in", origin: Some(orgurl::origin_string(&url)), name: Some(name.to_string()), ..Default::default() };
    windows::start_connect_signin(app, &id);
    changed(app);
    Ok(())
}

/// The connecting team's window finished a page: decides the connect outcome.
pub fn connect_page_loaded(app: &AppHandle, id: &str, path: &str) {
    let sh = shell(app);
    let Some(team) = sh.connecting.lock().unwrap().clone().filter(|t| t.id == id) else { return };
    let outcome = if path == "/auth/not-invited" {
        "not_invited"
    } else if path.starts_with("/auth/") || path == "/login" {
        // /auth/login before the browser leg, or a callback's error page.
        if path == "/auth/callback" {
            "failed"
        } else {
            return;
        }
    } else {
        "done"
    };
    *sh.connecting.lock().unwrap() = None;
    if outcome == "done" {
        {
            let mut t = sh.teams.lock().unwrap();
            t.teams.push(team.clone());
            t.last_open = Some(team.id.clone());
        }
        let _ = sh.save_teams();
        sh.reach.lock().unwrap().insert(
            team.id.clone(),
            Reach { reachable: Some(true), last_reached: Some(OffsetDateTime::now_utc()), checked: Some(Instant::now()), facts: None },
        );
        *sh.connect.lock().unwrap() =
            ConnectView { state: "done", team_id: Some(team.id.clone()), name: Some(team.name.clone()), ..Default::default() };
        let a = app.clone();
        let tid = team.id.clone();
        let _ = app.run_on_main_thread(move || {
            windows::close_setup(&a);
            windows::show_team(&a, &tid);
        });
    } else {
        let mut c = sh.connect.lock().unwrap();
        c.state = outcome;
        c.error = (outcome == "failed").then(|| "Sign-in didn't finish. Try again.".to_string());
        c.email = None;
        drop(c);
        if outcome == "not_invited" {
            // The not-invited page names the refused account: read from the window's
            // cookies (off this thread) before the window goes.
            teamapi::note_denied(app, id, team.origin());
        } else {
            let a = app.clone();
            let tid = id.to_string();
            let _ = app.run_on_main_thread(move || windows::close_team_window(&a, &tid));
        }
    }
    changed(app);
}

pub fn connect_reset(app: &AppHandle) {
    let sh = shell(app);
    if let Some(t) = sh.connecting.lock().unwrap().take() {
        sh.pending_signins.lock().unwrap().remove(&t.id);
        let a = app.clone();
        let _ = app.run_on_main_thread(move || windows::close_team_window(&a, &t.id));
    }
    *sh.connect.lock().unwrap() = ConnectView { state: "idle", ..Default::default() };
    changed(app);
}

// ---- Teams elsewhere ----

/// Asks a team elsewhere whether it answers (and picks up a rename).
pub fn check_reach(app: &AppHandle, id: &str) {
    let Some(team) = shell(app).team(id) else { return };
    let Some(origin) = team.origin() else { return };
    let app = app.clone();
    let id = id.to_string();
    tauri::async_runtime::spawn(async move {
        let ok = match orgurl::normalize_org_url(&origin) {
            Ok(u) => orgurl::fetch_login(&u).await.ok(),
            Err(_) => None,
        };
        let sh = app.state::<Shell>();
        let before = sh.reach.lock().unwrap().get(&id).and_then(|r| r.reachable);
        {
            let mut reach = sh.reach.lock().unwrap();
            let r = reach.entry(id.clone()).or_default();
            r.reachable = Some(ok.is_some());
            r.checked = Some(Instant::now());
            if ok.is_some() {
                r.last_reached = Some(OffsetDateTime::now_utc());
            }
        }
        if let Some(info) = ok {
            if sh.teams.lock().unwrap().rename(&id, &info.org_name) {
                let _ = sh.save_teams();
            }
        }
        if before != sh.reach.lock().unwrap().get(&id).and_then(|r| r.reachable) {
            changed(&app);
        }
    });
}

// ---- The watch loop ----

/// One background thread: while someone signs in to Claude it asks every
/// few seconds whether a credential exists and whether Terminal is still
/// open; otherwise once a minute it notices a team that stopped on its
/// own, a Claude that signed out, and teams elsewhere coming and going.
pub fn start_loops(app: &AppHandle) {
    if shell(app).loops.swap(true, Ordering::SeqCst) {
        return;
    }
    let app = app.clone();
    std::thread::Builder::new()
        .name("kivali-watch".into())
        .spawn(move || {
            let mut last_slow = Instant::now() - SLOW_TICK;
            loop {
                let sh = app.state::<Shell>();
                // Quitting (or installing an update, which can fail and
                // carry on): skip the work, keep the loop.
                if sh.quitting.load(Ordering::SeqCst) {
                    std::thread::sleep(FAST_TICK);
                    continue;
                }
                let mut dirty = false;
                let signing: Vec<Arc<Runtime>> =
                    sh.runtimes().into_iter().filter(|r| r.signin.lock().unwrap().opened_at.is_some()).collect();
                for rt in &signing {
                    dirty |= watch_signin(&app, rt);
                }
                if last_slow.elapsed() >= SLOW_TICK {
                    last_slow = Instant::now();
                    for rt in sh.runtimes() {
                        dirty |= watch_team(&app, &rt);
                    }
                    let elsewhere: Vec<String> =
                        sh.teams.lock().unwrap().teams.iter().filter(|t| !t.is_here()).map(|t| t.id.clone()).collect();
                    for id in elsewhere {
                        let due = sh.reach.lock().unwrap().get(&id).and_then(|r| {
                            let every = if r.reachable == Some(false) { SLOW_TICK } else { 5 * SLOW_TICK };
                            r.checked.map(|c| c.elapsed() >= every)
                        });
                        if due != Some(false) {
                            check_reach(&app, &id);
                        }
                    }
                    // Agents working, the signed-in account: for each
                    // running team with a window (teamapi.rs skips the rest).
                    let ids: Vec<String> = sh.teams.lock().unwrap().teams.iter().map(|t| t.id.clone()).collect();
                    for id in ids {
                        teamapi::refresh(&app, &id);
                    }
                }
                if dirty {
                    changed(&app);
                }
                std::thread::sleep(if signing.is_empty() { Duration::from_secs(5) } else { FAST_TICK });
            }
        })
        .expect("the watch thread starts");
}

/// The sign-in watch after one look at the team: done once a sign-in
/// other than the one Terminal opened on appears; following while a
/// terminal session is attached; closed when that session ends with no
/// sign-in (Sign in again that kept the one already there is simply
/// done); given up after [`SIGNIN_WAIT`] with no session.
fn next_signin(w: &SigninWatch, cred: Option<&CredentialStatus>, terminals: u32, waited_out: bool) -> SigninWatch {
    let signed_in = cred.is_some_and(|c| c.signed_in);
    if cred.is_some_and(|c| c.signed_in && Some(signin_key(c)) != w.from) {
        SigninWatch::default()
    } else if terminals > 0 {
        SigninWatch { saw_terminal: true, ..w.clone() }
    } else if w.saw_terminal {
        SigninWatch { closed: !signed_in, ..SigninWatch::default() }
    } else if waited_out {
        SigninWatch::default()
    } else {
        w.clone()
    }
}

fn watch_signin(app: &AppHandle, rt: &Runtime) -> bool {
    let cred_before = rt.credential.lock().unwrap().as_ref().map(signin_key);
    let terminals_before = rt.report.lock().unwrap().as_ref().map(|r| r.terminals).unwrap_or(0);
    let rep = rt.refresh_status().ok();
    let cred = rt.refresh_credential();
    let terminals = rep.map(|r| r.terminals).unwrap_or(0);
    let mut w = rt.signin.lock().unwrap();
    let before = w.clone();
    let waited_out = w.opened_at.is_some_and(|t| t.elapsed() > SIGNIN_WAIT);
    *w = next_signin(&w, cred.as_ref(), terminals, waited_out);
    drop(w);
    shell(app).note_signin(&rt.id, cred_before.as_ref().map(|c| c.0), cred.as_ref().map(|c| c.signed_in));
    let w = rt.signin.lock().unwrap();
    let cred_now = cred.as_ref().map(signin_key);
    before.opened_at != w.opened_at || before.closed != w.closed || before.saw_terminal != w.saw_terminal || cred_before != cred_now
        || terminals_before != terminals
}

fn watch_team(app: &AppHandle, rt: &Runtime) -> bool {
    if rt.op_running().is_some() {
        return false;
    }
    let was = rt.status().map(|s| s.state);
    let listening = rt.supervisor.is_listening();
    if !listening && was.is_none() {
        return false;
    }
    let _ = rt.refresh_status();
    let now = rt.status().map(|s| s.state);
    let name = shell(app).team(&rt.id).map(|t| t.name).unwrap_or_default();
    let mut dirty = was != now;
    if was == Some(OrgState::Running) && now != Some(OrgState::Running) && !shell(app).quitting.load(Ordering::SeqCst) {
        notify_background(
            app,
            &format!("{name} stopped unexpectedly"),
            "Open Kivali to start it again.",
            platform::NotifyTarget::Team(rt.id.clone()),
        );
    }
    if now == Some(OrgState::Running) {
        let due = rt.credential_at.lock().unwrap().is_none_or(|t| t.elapsed() >= CREDENTIAL_EVERY);
        if due {
            let before = rt.credential.lock().unwrap().as_ref().map(|c| c.signed_in);
            let after = rt.refresh_credential().map(|c| c.signed_in);
            shell(app).note_signin(&rt.id, before, after);
            if before == Some(true) && after == Some(false) {
                notify_background(
                    app,
                    &format!("{name} needs you"),
                    "Claude signed out. Sign in again so agents can work.",
                    platform::NotifyTarget::Settings(format!("team/{}/ai", rt.id)),
                );
            }
            dirty |= before != after;
        }
    }
    dirty
}

// ---- Launch, app updates, quit ----

/// At launch: adopt supervisors a previous shell left running, and
/// resume the teams that were running when Kivali quit.
pub fn launch(app: &AppHandle) {
    let sh = shell(app);
    remove_left_over_prepared(&sh);
    for rt in sh.runtimes() {
        rt.adopt_running();
        if rt.supervisor.is_listening() {
            let _ = rt.refresh_status();
            if rt.is_running() {
                rt.refresh_credential();
                after_started(app, &rt);
            }
        }
    }
    let resume: Vec<String> = sh.teams.lock().unwrap().running_at_quit.clone();
    for id in resume {
        if sh.runtime(&id).is_some_and(|r| !r.is_running()) {
            if let Err(e) = resume_or_record(app, &id) {
                eprintln!("kivali: {id}: resuming at launch: {e}");
            }
        }
    }
    start_loops(app);
}

pub fn check_desktop_update(app: &AppHandle) {
    use tauri_plugin_updater::UpdaterExt;
    *shell(app).app_update.lock().unwrap() = AppUpdateView { state: "checking", ..Default::default() };
    changed(app);
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let found = match app.updater() {
            Ok(u) => u.check().await,
            Err(e) => Err(e),
        };
        let checked_at = Some(now_rfc3339());
        let v = match found {
            Ok(Some(update)) => AppUpdateView { state: "available", version: Some(update.version.clone()), checked_at, error: None },
            Ok(None) => AppUpdateView { state: "up_to_date", version: None, checked_at, error: None },
            Err(e) => {
                eprintln!("kivali: app update check: {e}");
                AppUpdateView { state: "failed", version: None, checked_at, error: Some(e.to_string()) }
            }
        };
        *app.state::<Shell>().app_update.lock().unwrap() = v;
        changed(&app);
    });
}

/// Restart to update answered: installs the new app, pauses the teams (they
/// resume after the restart, as after Quit), restarts.
pub fn install_app_update(app: &AppHandle) {
    use tauri_plugin_updater::UpdaterExt;
    let sh = shell(app);
    if sh.quitting.swap(true, Ordering::SeqCst) {
        return;
    }
    sh.app_update.lock().unwrap().state = "installing";
    changed(app);
    let app = app.clone();
    std::thread::spawn(move || {
        let app2 = app.clone();
        let installed: Result<bool, String> = tauri::async_runtime::block_on(async move {
            let mut builder = app2.updater_builder();
            if platform::INSTALL_EXITS {
                let app3 = app2.clone();
                builder = builder.on_before_exit(move || {
                    pause_all_for_exit(&app3);
                    app3.cleanup_before_exit();
                });
            }
            let updater = builder.build().map_err(|e| e.to_string())?;
            let Some(update) = updater.check().await.map_err(|e| e.to_string())? else { return Ok(false) };
            update.download_and_install(|_, _| {}, || {}).await.map_err(|e| e.to_string())?;
            Ok(true)
        });
        match installed {
            Ok(true) => {
                pause_all_for_exit(&app);
                app.restart();
            }
            other => {
                let sh = app.state::<Shell>();
                sh.quitting.store(false, Ordering::SeqCst);
                *sh.app_update.lock().unwrap() = AppUpdateView {
                    state: "failed",
                    error: Some(match other {
                        Err(e) => e,
                        _ => "No newer Kivali is available any more.".into(),
                    }),
                    ..Default::default()
                };
                changed(&app);
            }
        }
    });
}

/// Records which teams run (they resume next launch), waits for running
/// operations, and stops every team here, in parallel.
fn pause_all_for_exit(app: &AppHandle) {
    let sh = shell(app);
    discard_prepared(&sh);
    let rts = sh.runtimes();
    for rt in &rts {
        rt.wait_idle();
    }
    let running: Vec<String> = rts.iter().filter(|r| r.is_running()).map(|r| r.id.clone()).collect();
    {
        let mut t = sh.teams.lock().unwrap();
        t.running_at_quit = running.clone();
        let stamp = now_rfc3339();
        for id in &running {
            if let Some(team) = t.get_mut(id) {
                team.paused_at = Some(stamp.clone());
            }
        }
    }
    let _ = sh.save_teams();
    let handles: Vec<_> = rts
        .into_iter()
        .map(|rt| std::thread::spawn(move || rt.stop_everything(&mut |_| {})))
        .collect();
    for h in handles {
        let _ = h.join();
    }
}

/// Quit (a dialog asks first when a team here is running and the setting says to).
pub fn quit(app: &AppHandle) {
    let sh = shell(app);
    if sh.quitting.load(Ordering::SeqCst) {
        return;
    }
    let (ask, running): (bool, Vec<String>) = {
        let t = sh.teams.lock().unwrap();
        let running = t
            .teams
            .iter()
            .filter(|tm| sh.runtime(&tm.id).is_some_and(|r| r.is_running() || r.op_running().is_some()))
            .map(|tm| tm.name.clone())
            .collect();
        (t.ask_before_quit, running)
    };
    if !ask || running.is_empty() {
        return do_quit(app);
    }
    let who = match running.as_slice() {
        [one] => one.clone(),
        [a, b] => format!("{a} and {b}"),
        more => format!("{} and {}", more[..more.len() - 1].join(", "), more[more.len() - 1]),
    };
    let spec = platform::AlertSpec {
        title: "Quit Kivali?".into(),
        message: format!("{who} pause{} until you open Kivali again.", if running.len() == 1 { "s" } else { "" }),
        buttons: vec!["Quit".into(), "Cancel".into()],
        destructive: None,
        suppression: Some("Don't ask again".into()),
    };
    let parent = windows::front_window(app);
    let app2 = app.clone();
    platform::alert(
        app,
        parent.as_ref(),
        spec,
        Box::new(move |a| {
            if a.button != 0 {
                return;
            }
            if a.suppressed {
                let sh = app2.state::<Shell>();
                sh.teams.lock().unwrap().ask_before_quit = false;
                let _ = sh.save_teams();
            }
            do_quit(&app2);
        }),
    );
}

fn do_quit(app: &AppHandle) {
    let sh = shell(app);
    if sh.quitting.swap(true, Ordering::SeqCst) {
        return;
    }
    changed(app);
    let app = app.clone();
    std::thread::spawn(move || {
        pause_all_for_exit(&app);
        app.exit(0);
    });
}

/// The system is ending the app without Quit (logout, shutdown): stop
/// every supervisor this shell owns.
pub fn on_exit(sh: &Shell) {
    let mut rts = sh.runtimes();
    if let Some(p) = sh.prepared.lock().unwrap().take() {
        drop(p.go);
        rts.push(p.rt);
    }
    // As at Quit: the teams running now resume at the next launch (a
    // logout's login item, say). Quit has already recorded them.
    if !sh.quitting.load(Ordering::SeqCst) {
        let running: Vec<String> = rts.iter().filter(|r| r.is_running()).map(|r| r.id.clone()).collect();
        let stamp = now_rfc3339();
        {
            let mut t = sh.teams.lock().unwrap();
            for id in &running {
                if let Some(team) = t.get_mut(id) {
                    team.paused_at = Some(stamp.clone());
                }
            }
            t.running_at_quit = running;
        }
        let _ = sh.save_teams();
    }
    let handles: Vec<_> = rts
        .into_iter()
        .map(|rt| {
            std::thread::spawn(move || {
                if let Some(sc) = rt.close_sidecar() {
                    rt.stop_owned(sc, &mut |_| {});
                }
            })
        })
        .collect();
    for h in handles {
        let _ = h.join();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn snapshot_returns_without_deadlock() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let mut n = 0u16;
        teams.add_here("Plainsong", Kind::Work, "dana@example.com", &mut || {
            n += 1;
            n
        });
        teams.add_elsewhere(&orgurl::normalize_org_url("studio.example.com").unwrap(), "Studio");
        let shell = Arc::new(Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None));
        let (tx, rx) = std::sync::mpsc::channel();
        let s = shell.clone();
        std::thread::spawn(move || {
            let _ = tx.send(serde_json::to_value(s.snapshot()).unwrap());
        });
        let v = rx.recv_timeout(Duration::from_secs(10)).expect("snapshot() did not return: a lock is taken twice");
        // The keys src/types.ts reads.
        for k in ["app_version", "platform", "teams", "host", "settings", "app_update", "owner_signin", "connect", "config_dir", "logs_dir", "last_deleted"] {
            assert!(v.get(k).is_some(), "snapshot lacks {k}");
        }
        let t = &v["teams"][0];
        for k in [
            "id", "name", "kind", "place", "url", "device", "state", "activity", "phrase", "reason", "pause_first", "op", "version",
            "update", "ai", "owner", "memory_mb", "cpus", "disk_used_bytes", "disk_size_bytes", "public_url", "paused_since", "last_reached",
            "provisional", "agents", "working", "files", "signed_in_as", "memory_free_mb",
        ] {
            assert!(t.get(k).is_some(), "team lacks {k}");
        }
        for k in ["provider", "signed_in", "email", "billing", "terminal_open", "signin", "signed_out_at"] {
            assert!(t["ai"].get(k).is_some(), "ai lacks {k}");
        }
        assert_eq!(t["update"]["takes"], "about 3 minutes");
        assert!(v["connect"].get("email").is_some(), "connect lacks email");
        assert_eq!(t["state"], "paused");
        assert_eq!(t["place"], "here");
        assert_eq!(v["teams"][1]["place"], "elsewhere");
        assert_eq!(v["teams"][1]["phrase"], "on studio.example.com");
    }

    #[test]
    fn op_view_stages() {
        let mut op = TeamOp::new(OpKind::Create);
        op.progress(&Progress { label: "creating the data disk".into(), stage: Some("making-room".into()) });
        op.progress(&Progress { label: "VM started".into(), stage: Some("starting".into()) });
        op.progress(&Progress { label: "no stage".into(), stage: None });
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Starting up"));
        assert_eq!(v.stage_index, 1);
        assert_eq!(v.stages, ["Making room", "Starting up", "Setting up your team", "Ready"]);
        assert_eq!(v.lines.len(), 3);
        op.running = false;
        op.finished = true;
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.percent), (Some("Ready"), 100));
    }

    /// What resumes at the next launch follows what runs: added when a
    /// team comes up, removed when it is paused, never an unknown id.
    #[test]
    fn running_at_quit_follows_the_teams() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        sh.set_running_at_quit(&a, true);
        sh.set_running_at_quit(&a, true);
        sh.set_running_at_quit("gone", true);
        assert_eq!(sh.teams.lock().unwrap().running_at_quit, std::slice::from_ref(&a));
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert_eq!(on_disk.running_at_quit, std::slice::from_ref(&a));
        sh.set_running_at_quit(&a, false);
        assert!(sh.teams.lock().unwrap().running_at_quit.is_empty());
    }

    /// The signed-out date: stamped when Claude goes from signed in to signed out,
    /// kept while it stays out, cleared by a sign-in.
    #[test]
    fn signed_out_stamp_follows_the_transition() {
        let now = || "2026-10-02T09:00:00Z".to_string();
        let was = Some("2026-09-30T08:00:00Z".to_string());
        assert_eq!(signed_out_stamp(None, Some(true), Some(false), now).as_deref(), Some("2026-10-02T09:00:00Z"));
        assert_eq!(signed_out_stamp(was.clone(), Some(true), Some(false), now), was);
        assert_eq!(signed_out_stamp(was.clone(), Some(false), Some(false), now), was);
        assert_eq!(signed_out_stamp(was.clone(), None, None, now), was);
        // Never signed in (a new team): nothing to date.
        assert_eq!(signed_out_stamp(None, None, Some(false), now), None);
        assert_eq!(signed_out_stamp(was, Some(false), Some(true), now), None);
    }

    /// Stamped in teams.json, and cleared again, by the shell.
    #[test]
    fn note_signin_persists() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        sh.note_signin(&a, Some(true), Some(false));
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert!(on_disk.get(&a).unwrap().local().unwrap().signed_out_at.is_some());
        sh.note_signin(&a, Some(false), Some(true));
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert_eq!(on_disk.get(&a).unwrap().local().unwrap().signed_out_at, None);
    }

    #[test]
    fn sign_in_again_waits_for_a_different_sign_in() {
        let max = CredentialStatus { signed_in: true, email: Some("a@example.com".into()), billing: Some("Claude Max".into()), checked_at: String::new() };
        let bedrock = CredentialStatus { signed_in: true, email: None, billing: Some("Amazon Bedrock".into()), checked_at: String::new() };
        let out = CredentialStatus::default();
        let opened = |from: Option<&CredentialStatus>| SigninWatch { opened_at: Some(Instant::now()), from: from.map(signin_key), saw_terminal: false, closed: false };

        // Setup: nothing signed in, then a sign-in ends the wait.
        let w = opened(Some(&out));
        let w = next_signin(&w, Some(&out), 1, false);
        assert!(w.opened_at.is_some() && w.saw_terminal);
        let w = next_signin(&w, Some(&max), 1, false);
        assert!(w.opened_at.is_none() && !w.closed);

        // Sign in again: the same sign-in keeps waiting, a new one ends it.
        let w = opened(Some(&max));
        let w = next_signin(&w, Some(&max), 1, false);
        assert!(w.opened_at.is_some());
        let w = next_signin(&w, Some(&bedrock), 1, false);
        assert!(w.opened_at.is_none() && !w.closed);

        // Terminal closed: with the old sign-in kept, done; with none, closed.
        let w = next_signin(&SigninWatch { saw_terminal: true, ..opened(Some(&max)) }, Some(&max), 0, false);
        assert!(w.opened_at.is_none() && !w.closed);
        let w = next_signin(&SigninWatch { saw_terminal: true, ..opened(Some(&out)) }, Some(&out), 0, false);
        assert!(w.opened_at.is_none() && w.closed);

        // No terminal ever attached: given up only after the wait.
        assert!(next_signin(&opened(None), None, 0, false).opened_at.is_some());
        assert!(next_signin(&opened(None), None, 0, true).opened_at.is_none());
    }

    #[test]
    fn versions_read_short() {
        assert_eq!(short_version("0.17.0"), "0.17");
        assert_eq!(short_version("v0.16.2"), "0.16.2");
    }

    #[test]
    fn call_me_is_what_the_server_accepts() {
        assert_eq!(clean_call_me("  Mom  "), "Mom");
        assert_eq!(clean_call_me("Dr. Patel: CEO"), "Dr. Patel CEO");
        assert_eq!(clean_call_me("`>Jane\u{202E}\u{200D}\n"), "Jane");
        assert_eq!(clean_call_me(&"x".repeat(50)).len(), 40);
        assert_eq!(clean_call_me(" : "), "");
    }
}
