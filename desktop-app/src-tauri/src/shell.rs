//! The shell's state and its flows. Every team on this computer has a
//! [`Runtime`]: its own supervisor (`kivali-supervisor --config-dir
//! <its folder> serve`), status, long operation and release check. Teams
//! elsewhere have only what the shell learns by asking their address.
//! Windows, menus and IPC commands call into here.

use crate::connectreturn::PendingReturn;
use crate::downloads;
use crate::ownersignin;
use crate::platform;
use crate::signin::PendingSignin;
use crate::supervisor::sidecar::{self, Sidecar, STOP_TIMEOUTS};
use crate::supervisor::wire::{
    BakedRelease, CheckReport, CredentialStatus, OrgState, Progress, Report, SetupInfo, SetupRequest, SetupResult, StageProgress, Status,
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
/// The first port of the dynamic range (49152-65535), which hints stay
/// below: Windows hands out ephemeral ports from it, and Hyper-V and
/// WinNAT reserve blocks of it anew on every boot.
const DYNAMIC_PORTS: u16 = 49152;

pub(crate) fn now_rfc3339() -> String {
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
    /// The stages the operation has been in, by index.
    pub seen: Vec<bool>,
    /// The current stage's last progress report, while it has any.
    pub measured: Option<StageProgress>,
    /// The stages' typical lengths on this computer (`view::lengths`).
    pub lengths: Vec<u64>,
    /// An update to the Kivali baked into the team's VM image: nothing is
    /// downloaded, and its first stage reads so.
    pub baked: bool,
}

impl TeamOp {
    /// `copy_bytes` is what an update's snapshot copies
    /// ([`snapshot_copy_bytes`]).
    fn new(kind: OpKind, copy_bytes: u64) -> TeamOp {
        TeamOp::with(kind, copy_bytes, false)
    }

    /// [`TeamOp::new`], an update from the VM image when `baked`.
    fn with(kind: OpKind, copy_bytes: u64, baked: bool) -> TeamOp {
        TeamOp {
            kind,
            running: true,
            error: None,
            finished: false,
            lines: Vec::new(),
            stage: None,
            stage_since: Instant::now(),
            started_ms: unix_ms(),
            seen: vec![false; kind.stages().len()],
            measured: None,
            lengths: view::lengths(kind, copy_bytes, baked),
            baked,
        }
    }

    /// Takes a line or a progress report: a stage the report names
    /// becomes the current one (with no progress yet), its progress the
    /// stage's, and its line, if it has one, the newest. The bar never
    /// goes back to a stage the operation has left: one that runs `up`
    /// twice (setup boots the machine, then installs on it once the
    /// wizard is done) hears an earlier stage again, which keeps its line
    /// but not the bar. An earlier stage not yet seen still counts (a
    /// resume says `starting` before it moves the data disk).
    fn progress(&mut self, p: &Progress) {
        let index = view::stage_index(self.kind, p.stage.as_deref())
            .filter(|&i| self.stage.is_none_or(|cur| i >= cur || !self.seen.get(i).copied().unwrap_or(false)));
        if let Some(i) = index {
            if self.stage != Some(i) {
                self.stage = Some(i);
                self.stage_since = Instant::now();
                self.measured = None;
            }
            self.seen[i] = true;
        }
        if p.progress.is_some() && index.is_some() {
            self.measured = p.progress;
        }
        if !p.label.is_empty() {
            self.lines.push(p.label.clone());
        }
    }

    pub fn view(&self) -> OpView {
        let stages = self.kind.stages();
        let index = if self.finished { stages.len().saturating_sub(1) } else { self.stage.unwrap_or(self.kind.first_stage()) };
        let shown = view::shown_stages(self.kind, &self.seen, index);
        let measured = self.measured.filter(|_| self.running && self.stage == Some(index));
        let (percent, remaining) = view::progress(&self.lengths, &shown, index, self.stage_since.elapsed(), measured, self.finished);
        OpView {
            kind: self.kind,
            running: self.running,
            error: self.error.clone(),
            finished: self.finished,
            lines: self.lines.clone(),
            stage: (index < stages.len()).then(|| view::stage_label(self.kind, index, self.baked).to_string()),
            stage_index: shown.iter().position(|&i| i == index).unwrap_or(0),
            stages: shown.iter().map(|&i| view::stage_label(self.kind, i, self.baked).to_string()).collect(),
            started_ms: self.started_ms,
            percent,
            remaining: if self.running { remaining } else { None },
            stage_progress: measured.map(|m| view::measure(self.kind, index, m)),
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
    /// Made by setup, which has not finished (`finish_setup`): setup may
    /// still remove it (Back, Try again). Mirrors teams.json's
    /// `provisional`, which keeps it across launches.
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

    /// Undoes [`Self::close_sidecar`]: a supervisor started from now on
    /// is kept again.
    fn reopen(&self) {
        self.sidecar.lock().unwrap().closed = false;
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

    /// Quit's stop of this team ([`Self::stop_everything`]). While its VM
    /// runs the stop is the team's Pause operation: its window shows the
    /// pausing page rather than its web app, the tray and Settings say
    /// pausing, and no status read mid-stop (the VM up, its forward
    /// already dropped, which reads as starting) shows through. `redraw`
    /// follows each step, and there is none once it has stopped: Kivali
    /// exits next, or resumes the team when an update's installer could
    /// not start.
    fn pause_for_exit(&self, redraw: &dyn Fn()) {
        if !self.report.lock().unwrap().as_ref().is_some_and(|r| r.running) {
            return self.stop_everything(&mut |_| {});
        }
        *self.op.lock().unwrap() = Some(TeamOp::new(OpKind::Pause, 0));
        redraw();
        self.stop_everything(&mut |p| {
            self.op_progress(&p);
            redraw();
        });
        self.end_op(&Ok(()));
    }

    /// Takes a progress report into the running operation ([`TeamOp::progress`]).
    fn op_progress(&self, p: &Progress) {
        if let Some(o) = self.op.lock().unwrap().as_mut() {
            o.progress(p);
        }
    }

    /// Ends the running operation with `result` and wakes [`Self::wait_idle`].
    fn end_op(&self, result: &Result<(), String>) {
        if let Some(o) = self.op.lock().unwrap().as_mut() {
            o.running = false;
            match result {
                Ok(()) => o.finished = true,
                Err(e) => o.error = Some(e.clone()),
            }
        }
        self.op_done.notify_all();
    }

    /// After a delete: the supervisor has exited (destroy with exit), so
    /// only the process is waited for.
    fn forget_sidecar(&self) {
        if let Some(sc) = self.close_sidecar() {
            sc.stop(&mut || Ok(()), STOP_TIMEOUTS);
        }
    }

    /// Records why `up` failed, kept until the next start succeeds. A team
    /// that can't be reached on its port has its VM stopped too: nobody
    /// can use it, and a team that couldn't start counts for no memory and
    /// has no Pause. Moving the port (`up --port`) or Try again starts it
    /// again.
    fn failed_up(&self, e: &SupError, progress: &mut dyn FnMut(Progress)) {
        *self.failure.lock().unwrap() = Some(e.to_string());
        let Some(why) = self.refresh_status().ok().as_ref().and_then(port_failure) else { return };
        *self.failure.lock().unwrap() = Some(format!("{PORT_FAILURE}{why}"));
        progress(label("Stopping the team, since it can't be reached on its port"));
        if let Err(e) = self.supervisor.down(false, progress) {
            eprintln!("kivali: {}: stopping after the port failed: {e}", self.id);
        }
        let _ = self.refresh_status();
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
    /// While downloading: the bytes so far, and the download's length
    /// when the server gave one.
    pub downloaded: Option<u64>,
    pub total: Option<u64>,
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
    /// Integrations' sign-ins handed off to the browser and not yet back
    /// (windows.rs, `open_connect_sign_in`), by team.
    pub pending_returns: Mutex<HashMap<String, PendingReturn>>,
    /// The team window in front, or last in front.
    pub front_team: Mutex<Option<String>>,
    /// Downloads from the team windows (downloads.rs), by team.
    pub downloads: Mutex<downloads::Downloads>,
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

/// A team's recorded failure when this computer can't run its VM
/// (platform::vm_support): the reason and the pages' steps come from
/// the check itself.
const VM_FAILURE: &str = "vm";

/// A team's recorded failure when it couldn't be reached on its port:
/// this prefix, then the supervisor's words (`forward_error`). It ends
/// when the supervisor reports the team running, however it got there
/// (`Shell::follow_status`): moving the port takes `kivali-supervisor up
/// --port`, outside Kivali.
const PORT_FAILURE: &str = "port:";

/// The supervisor's words when a report says the VM runs but the team's
/// recorded port could not be bound (`forward_error`).
fn port_failure(r: &Report) -> Option<String> {
    r.forward_error.as_deref().map(str::trim).filter(|e| r.running && !e.is_empty()).map(str::to_string)
}

/// What the pages render (types.ts `Snapshot`).
#[derive(Serialize)]
pub struct Snapshot {
    pub app_version: String,
    pub platform: &'static str,
    pub teams: Vec<TeamView>,
    pub host: HostView,
    /// The least memory a team needs for a web task
    /// (`teams::WEB_TASK_MEMORY_MB`): Settings warns below it.
    pub web_memory_mb: u32,
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
    /// Whether a team's VM can run here (platform::vm_support).
    pub vm_support: platform::VmSupport,
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
    /// How long an update typically takes, "about 4 minutes".
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
    /// A team here running a development build of Kivali
    /// (`updates::is_dev_version`): Settings offers Restart.
    pub dev: bool,
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
    /// Downloads from the team's window, oldest first (downloads.rs):
    /// what its downloads card draws.
    pub downloads: Vec<downloads::DownloadView>,
}

/// What an update's snapshot copies on this computer, in bytes: the
/// data disk's used bytes where the snapshot is a copy
/// (`platform::UPDATE_COPIES_DATA_DISK`), none where it is a clone or
/// the size is not known.
pub fn snapshot_copy_bytes(report: Option<&Report>) -> u64 {
    copy_bytes(platform::UPDATE_COPIES_DATA_DISK, report)
}

fn copy_bytes(copies: bool, report: Option<&Report>) -> u64 {
    if copies {
        report.map(|r| r.disk_used_bytes).unwrap_or(0)
    } else {
        0
    }
}

/// What the team's update row offers: the feed's verdict with the VM
/// image's release folded in (`updates::with_baked`). `current` is the
/// team's version from its status.
fn update_state(check: Option<&CheckReport>, baked: Option<&BakedRelease>, current: Option<&str>, app_version: &str) -> updates::UpdateState {
    // The feed's notes describe the baked release only when they name it.
    let notes = check
        .filter(|c| baked.is_some_and(|b| c.latest.as_deref().map(|l| l.trim_start_matches('v')) == Some(b.version.trim_start_matches('v'))))
        .and_then(|c| c.notes_url.as_deref());
    updates::with_baked(updates::decide(check, true, app_version), baked, current, notes)
}

/// `copy_bytes` is what an update's snapshot copies
/// ([`snapshot_copy_bytes`]), for its length.
fn update_view(
    check: Option<&CheckReport>,
    baked: Option<&BakedRelease>,
    current: Option<&str>,
    app_version: &str,
    now: OffsetDateTime,
    copy_bytes: u64,
) -> TeamUpdateView {
    use updates::UpdateState as U;
    let state = update_state(check, baked, current, app_version);
    let checked_at = check.and_then(|c| c.checked_at.clone());
    let mut from_image = false;
    let (s, current, latest, notes) = match state {
        U::NoLocalOrg | U::NotChecked | U::NotInstalled => ("not_checked", None, None, None),
        U::UpToDate { current, .. } => ("up_to_date", Some(current), None, None),
        U::Available { current, latest, baked } => {
            from_image = baked;
            // The feed's notes, when it describes the release on offer.
            let notes = check.filter(|c| c.latest.as_deref().map(|l| l.trim_start_matches('v')) == Some(latest.trim_start_matches('v'))).and_then(|c| c.notes_url.clone());
            ("available", Some(current), Some(latest), notes)
        }
        U::DesktopFirst { latest, .. } => ("desktop_first", check.map(|c| c.current.clone()), Some(latest), None),
        U::ManualSteps { latest, notes_url } => ("manual_steps", check.map(|c| c.current.clone()), Some(latest), notes_url),
        U::Failed { .. } => ("failed", check.map(|c| c.current.clone()), None, None),
    };
    let _ = now;
    let bare = |v: Option<String>| v.map(|v| v.trim_start_matches('v').to_string()).filter(|v| !v.is_empty());
    TeamUpdateView {
        state: s,
        current: bare(current),
        latest: bare(latest),
        notes_url: notes,
        summary: None,
        checked_at,
        takes: view::typical(OpKind::Update, copy_bytes, from_image),
    }
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
            pending_returns: Mutex::new(HashMap::new()),
            front_team: Mutex::new(None),
            downloads: Mutex::new(downloads::Downloads::default()),
            zoom: Mutex::new(HashMap::new()),
            last_deleted: Mutex::new(None),
            quitting: AtomicBool::new(false),
            loops: AtomicBool::new(false),
            prepared: Mutex::new(None),
        };
        let here: Vec<(String, PathBuf, bool)> = {
            let t = shell.teams.lock().unwrap();
            t.teams
                .iter()
                .filter_map(|t| t.dir(&shell.dir).map(|d| (t.id.clone(), d, t.local().is_some_and(|l| l.provisional))))
                .collect()
        };
        let mut rts = shell.runtimes.lock().unwrap();
        for (id, d, provisional) in here {
            let rt = Runtime::new(&id, d);
            rt.provisional.store(provisional, Ordering::SeqCst);
            rts.insert(id.clone(), Arc::new(rt));
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
                        Some(format!(
                            "{} is low on memory. {other_name} is using {}.",
                            view::capitalized(platform::THIS_COMPUTER),
                            view::gb(u64::from(other_mb))
                        ))
                    }
                    None if failure.as_deref() == Some(VM_FAILURE) => view::vm_reason(platform::vm_support()),
                    None if failure.as_deref().is_some_and(|f| f.starts_with(PORT_FAILURE)) => {
                        failure.as_deref().and_then(|f| f.strip_prefix(PORT_FAILURE)).map(str::to_string)
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
                    dev: status.as_ref().and_then(|s| s.kivali.as_deref()).is_some_and(updates::is_dev_version),
                    update: Some(update_view(
                        check.as_ref(),
                        report.as_ref().and_then(|r| r.baked.as_ref()),
                        status.as_ref().and_then(|s| s.kivali.as_deref()),
                        &self.app_version,
                        now,
                        snapshot_copy_bytes(report.as_ref()),
                    )),
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
                    downloads: self.downloads.lock().unwrap().view(&t.id),
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
                    dev: false,
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
                    downloads: self.downloads.lock().unwrap().view(&t.id),
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
            host: HostView { memory_mb: hostinfo::memory_mb(), cpus: hostinfo::cpus(), vm_support: platform::vm_support() },
            web_memory_mb: crate::teams::WEB_TASK_MEMORY_MB,
            settings: SettingsView { start_at_login: teams.start_at_login, ask_before_quit: teams.ask_before_quit },
            app_update,
            owner_signin: owner,
            connect,
            config_dir: self.dir.display().to_string(),
            logs_dir: self.dir.join("logs").display().to_string(),
            last_deleted: last_deleted.map(|(name, freed_bytes)| DeletedView { name, freed_bytes }),
        }
    }

    /// The team is no longer setup's: in this run and in teams.json.
    fn end_setup(&self, id: &str) {
        if let Some(rt) = self.runtime(id) {
            rt.provisional.store(false, Ordering::SeqCst);
        }
        set_provisional(&mut self.teams.lock().unwrap(), id, false);
        if let Err(e) = self.save_teams() {
            eprintln!("kivali: {id}: finishing setup: {e}");
        }
    }

    /// Records a team's port when the supervisor reports a different one;
    /// true when it did.
    fn record_port(&self, id: &str, st: &Status) -> Result<bool, String> {
        let Some(port) = st.port else { return Ok(false) };
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
        Ok(changed)
    }

    /// Follows a status read outside a start (the watch loop): the port
    /// the supervisor reports becomes the team's, and a port failure ends
    /// once the team runs, as after `kivali-supervisor up --port` moved
    /// it. Answers (port moved, failure ended).
    fn follow_status(&self, rt: &Runtime) -> (bool, bool) {
        let Some(st) = rt.status() else { return (false, false) };
        let moved = self.record_port(&rt.id, &st).unwrap_or_else(|e| {
            eprintln!("kivali: {}: recording the port: {e}", rt.id);
            false
        });
        let ended = st.state == OrgState::Running && {
            let mut failure = rt.failure.lock().unwrap();
            let port = failure.as_deref().is_some_and(|f| f.starts_with(PORT_FAILURE));
            if port {
                *failure = None;
            }
            port
        };
        if ended {
            self.set_running_at_quit(&rt.id, true);
        }
        (moved, ended)
    }

    /// A port for a new team: the first from 8080, below the dynamic
    /// range, that no other team here has recorded (the supervisor still
    /// moves off one that some other program holds or the system has
    /// reserved).
    fn port_hint_except(&self, id: &str) -> u16 {
        let used: Vec<u16> =
            self.teams.lock().unwrap().teams.iter().filter(|t| t.id != id).filter_map(|t| t.local().map(|l| l.port)).collect();
        (FIRST_PORT..DYNAMIC_PORTS).find(|p| !used.contains(p)).unwrap_or(0)
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

/// Only the pages redraw: for a change that moves no window, menu or
/// tray (a download's bytes so far).
pub fn emit_changed(app: &AppHandle) {
    let _ = app.emit(CHANGED_EVENT, ());
}

fn shell(app: &AppHandle) -> tauri::State<'_, Shell> {
    app.state::<Shell>()
}

fn label(text: &str) -> Progress {
    Progress { label: text.to_string(), ..Default::default() }
}

/// Runs one long operation of a team on its own thread. One at a time
/// per team; asking for a second while one runs is refused.
pub fn run_op<F>(app: &AppHandle, rt: Arc<Runtime>, kind: OpKind, work: F) -> Result<(), String>
where
    F: FnOnce(&AppHandle, &Shell, &Runtime, &mut dyn FnMut(Progress)) -> Result<(), String> + Send + 'static,
{
    run_op_with(app, rt, kind, false, work)
}

/// [`run_op`]; `baked` marks an update from the VM image, whose first
/// stage downloads nothing ([`TeamOp::with`]).
pub fn run_op_with<F>(app: &AppHandle, rt: Arc<Runtime>, kind: OpKind, baked: bool, work: F) -> Result<(), String>
where
    F: FnOnce(&AppHandle, &Shell, &Runtime, &mut dyn FnMut(Progress)) -> Result<(), String> + Send + 'static,
{
    if shell(app).quitting.load(Ordering::SeqCst) {
        return Err("Kivali is quitting.".into());
    }
    let copy = snapshot_copy_bytes(rt.report.lock().unwrap().as_ref());
    {
        let mut op = rt.op.lock().unwrap();
        if let Some(o) = op.as_ref().filter(|o| o.running) {
            return Err(format!("{} is busy ({:?}).", rt.id, o.kind).to_lowercase());
        }
        *op = Some(TeamOp::with(kind, copy, baked));
    }
    changed(app);
    let app = app.clone();
    std::thread::spawn(move || {
        let sh = app.state::<Shell>();
        let app2 = app.clone();
        let rt2 = rt.clone();
        let mut on_progress = |p: Progress| {
            rt2.op_progress(&p);
            changed(&app2);
        };
        let result = work(&app, &sh, &rt, &mut on_progress);
        rt.end_op(&result);
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

/// The error for a team id that names no team on this computer.
pub fn no_team_here() -> String {
    format!("no such team on {}", platform::THIS_COMPUTER)
}

/// Boots a team (creating it on the first call) and records what came
/// back. Used by create, resume and retry.
fn boot(app: &AppHandle, sh: &Shell, rt: &Runtime, req: UpRequest, progress: &mut dyn FnMut(Progress)) -> Result<(), String> {
    *rt.failure.lock().unwrap() = None;
    // No supervisor at all while this computer can't run the VM: the team
    // shows couldn't start, with the steps (types.ts `VmSupport`).
    if let Some(why) = view::vm_reason(platform::vm_support()) {
        *rt.failure.lock().unwrap() = Some(VM_FAILURE.into());
        return Err(why);
    }
    rt.ensure_supervisor(sh.vm_dir.as_deref())?;
    let result = rt.supervisor.up(&req, progress);
    let st = match result {
        Ok(st) => st,
        Err(e) => {
            eprintln!("kivali: {}: up failed: {e}", rt.id);
            rt.failed_up(&e, progress);
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
/// already preparing, would not fit in memory, or this computer can't
/// run its VM (the welcome screen says so instead).
pub fn prepare_team(app: &AppHandle) -> Result<(), String> {
    let sh = shell(app);
    if sh.quitting.load(Ordering::SeqCst) || platform::vm_support() != platform::VmSupport::Ready {
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
    if let Some(why) = view::vm_reason(platform::vm_support()) {
        return Err(why);
    }
    let (running, _) = sh.running_memory_mb("");
    let free = view::memory_free_mb(hostinfo::memory_mb(), running);
    let has_prepared = sh.prepared.lock().unwrap().is_some();
    if !has_prepared && u64::from(crate::teams::DEFAULT_MEMORY_MB) > free {
        return Err(format!(
            "{} doesn't have {} free for another team while the others run. Pause one first.",
            view::capitalized(platform::THIS_COMPUTER),
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
        set_provisional(&mut teams, &id, true);
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

/// Marks a team as setup's until setup finishes (teams.json `provisional`).
fn set_provisional(teams: &mut TeamsFile, id: &str, on: bool) {
    if let Some(l) = teams.get_mut(id).and_then(Team::local_mut) {
        l.provisional = on;
    }
}

/// Whether a team here is still setup's: its window is setup, at the
/// Claude sign-in (windows::show_team).
pub fn setup_unfinished(sh: &Shell, id: &str) -> bool {
    sh.teams.lock().unwrap().get(id).and_then(Team::local).is_some_and(|l| l.provisional)
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
        set_provisional(&mut teams, &p.id, true);
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
    shell(app).end_setup(id);
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
    let rt = sh.runtime(id).ok_or_else(no_team_here)?;
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
    let rt = shell(app).runtime(id).ok_or_else(no_team_here)?;
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

/// Restart: the team's pods recreated on the images now in its VM, for
/// a development build whose `:dev` tags moved. The team must be
/// running; the supervisor refuses otherwise.
pub fn restart_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let rt = shell(app).runtime(id).ok_or_else(no_team_here)?;
    if !rt.is_running() {
        return Err("The team isn't running.".into());
    }
    run_op(app, rt, OpKind::Restart, |_app, _sh, rt, progress| {
        let r = rt.supervisor.restart(progress).map_err(sup_err);
        let _ = rt.refresh_status();
        r
    })
}

/// Update… and Try again: the release the team's update row offers,
/// from the VM image when that is the one on offer, else the feed's.
pub fn update_team(app: &AppHandle, id: &str) -> Result<(), String> {
    let sh = shell(app);
    let rt = sh.runtime(id).ok_or_else(no_team_here)?;
    let baked = {
        let report = rt.report.lock().unwrap();
        let check = rt.check.lock().unwrap();
        let current = report.as_ref().map(|r| r.state.kivali.clone());
        matches!(
            update_state(check.as_ref(), report.as_ref().and_then(|r| r.baked.as_ref()), current.as_deref(), &sh.app_version),
            updates::UpdateState::Available { baked: true, .. }
        )
    };
    update_team_with(app, id, baked)
}

/// Updates the team to the feed's release, or with `baked` to the Kivali
/// baked into its VM image.
fn update_team_with(app: &AppHandle, id: &str, baked: bool) -> Result<(), String> {
    let rt = shell(app).runtime(id).ok_or_else(no_team_here)?;
    let id = id.to_string();
    run_op_with(app, rt, OpKind::Update, baked, move |app, sh, rt, progress| {
        let before = rt.status().and_then(|s| s.kivali);
        let name = sh.team(&id).map(|t| t.name).unwrap_or_default();
        match rt.supervisor.upgrade(baked, progress) {
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
                eprintln!("kivali: {id}: update failed: {e}");
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
    let rt = sh.runtime(id).ok_or_else(no_team_here)?;
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
    let rt = shell(app).runtime(id).ok_or_else(no_team_here)?;
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
    let rt = sh.runtime(id).ok_or_else(no_team_here)?;
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
    let rt = sh.runtime(id).ok_or_else(|| format!("only a team on {} can be deleted", platform::THIS_COMPUTER))?;
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
        return Err(format!("A team on {} is deleted, not removed.", platform::THIS_COMPUTER));
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
        return Err(format!("Only a team on {} has other devices.", platform::THIS_COMPUTER));
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
                    format!("{e} Check it's set up to reach {} on {}.", team.name, platform::THIS_COMPUTER)
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
        let l = t.get_mut(&id).and_then(Team::local_mut).ok_or_else(no_team_here)?;
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
        let l = t.get_mut(id).and_then(Team::local_mut).ok_or_else(no_team_here)?;
        l.memory_mb = memory_mb;
        l.cpus = cpus;
    }
    sh.save_teams()?;
    changed(app);
    Ok(())
}

/// The release check, once now and then daily, after a team came up.
fn after_started(app: &AppHandle, rt: &Runtime) {
    first_boot_update(app, rt);
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

/// The first start after Kivali Desktop was updated: the VM image the
/// team just booted bakes a newer Kivali than the team runs
/// (`Report::baked`), and the team moves to it without being asked, once
/// the operation that started it has ended (docs/developers/desktop-app.md,
/// "Updating a team"). Each baked version is tried once per team
/// (teams.json `first_boot_update`, [`Shell::claim_first_boot_update`]):
/// an update that fails shows in Settings with Try again, and is not
/// attempted again at every launch. An attempt that never became an
/// operation (another operation got in first, or Kivali is quitting) is
/// given back, so the next start tries again. The supervisor's verdict
/// already leaves a development build on either side alone; a release
/// whose upgrade needs steps by hand is shown, not applied; and a team
/// setup has not finished with is left to setup.
fn first_boot_update(app: &AppHandle, rt: &Runtime) {
    let Some(version) = shell(app).claim_first_boot_update(rt) else { return };
    eprintln!("kivali: {}: Kivali {version} is baked into the VM image; updating on this first start", rt.id);
    let app = app.clone();
    let id = rt.id.clone();
    std::thread::spawn(move || {
        let sh = app.state::<Shell>();
        let Some(rt) = sh.runtime(&id) else { return };
        rt.wait_idle();
        if let Err(e) = update_team_with(&app, &id, true) {
            eprintln!("kivali: {id}: first-boot update not started: {e}");
            sh.release_first_boot_update(&id, &version);
        }
    });
}

impl Shell {
    /// Whether the team's first start under this VM image should update
    /// it to the image's Kivali, and the claim of that one attempt: the
    /// report's baked release is newer and not marked manual steps, the
    /// team is not provisional, and the version is not the one already
    /// recorded; it is then recorded (teams.json `first_boot_update`) and
    /// returned.
    fn claim_first_boot_update(&self, rt: &Runtime) -> Option<String> {
        let baked = rt.report.lock().unwrap().as_ref().and_then(|r| r.baked.clone()).filter(|b| b.newer && !b.manual_steps)?;
        {
            let mut teams = self.teams.lock().unwrap();
            let l = teams.get_mut(&rt.id).and_then(Team::local_mut)?;
            if l.provisional || l.first_boot_update.as_deref() == Some(baked.version.as_str()) {
                return None;
            }
            l.first_boot_update = Some(baked.version.clone());
        }
        if let Err(e) = self.save_teams() {
            eprintln!("kivali: {}: recording the first-boot update: {e}", rt.id);
        }
        Some(baked.version)
    }

    /// Gives a claimed first-boot update back, when no operation came of
    /// it, so the next start claims it again.
    fn release_first_boot_update(&self, id: &str, version: &str) {
        {
            let mut teams = self.teams.lock().unwrap();
            let Some(l) = teams.get_mut(id).and_then(Team::local_mut) else { return };
            if l.first_boot_update.as_deref() != Some(version) {
                return;
            }
            l.first_boot_update = None;
        }
        if let Err(e) = self.save_teams() {
            eprintln!("kivali: {id}: releasing the first-boot update: {e}");
        }
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
    let desktop_first =
        matches!(updates::decide(Some(&report), true, &shell(app).app_version), updates::UpdateState::DesktopFirst { .. });
    *rt.check.lock().unwrap() = Some(report);
    changed(app);
    if desktop_first {
        crate::appupdate::check_for_team(app);
    }
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
    let (moved, ended) = shell(app).follow_status(rt);
    if ended {
        after_started(app, rt);
    }
    let mut dirty = was != now || moved || ended;
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

// ---- Launch, quit ----

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

/// Records which teams run (they resume next launch), waits for running
/// operations, and stops every team here, in parallel, each showing it
/// pauses ([`Runtime::pause_for_exit`]). Returns the teams that ran.
pub(crate) fn pause_all_for_exit(app: &AppHandle) -> Vec<String> {
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
        .map(|rt| {
            let app = app.clone();
            std::thread::spawn(move || rt.pause_for_exit(&|| changed(&app)))
        })
        .collect();
    for h in handles {
        let _ = h.join();
    }
    running
}

/// An app update paused every team for its installer, which then could
/// not start (appupdate.rs): the runtimes take supervisors again and the
/// teams that ran resume.
pub(crate) fn resume_after_failed_install(app: &AppHandle, running: &[String]) {
    let sh = shell(app);
    for rt in sh.runtimes() {
        rt.reopen();
    }
    for id in running {
        if let Err(e) = resume_or_record(app, id) {
            eprintln!("kivali: {id}: resuming after the app update: {e}");
        }
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
    crate::appupdate::cancel_download();
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
        for k in ["app_version", "platform", "teams", "host", "web_memory_mb", "settings", "app_update", "owner_signin", "connect", "config_dir", "logs_dir", "last_deleted"] {
            assert!(v.get(k).is_some(), "snapshot lacks {k}");
        }
        let t = &v["teams"][0];
        for k in [
            "id", "name", "kind", "place", "url", "device", "state", "activity", "phrase", "reason", "pause_first", "op", "version", "dev",
            "update", "ai", "owner", "memory_mb", "cpus", "disk_used_bytes", "disk_size_bytes", "public_url", "paused_since", "last_reached",
            "provisional", "agents", "working", "files", "signed_in_as", "memory_free_mb", "downloads",
        ] {
            assert!(t.get(k).is_some(), "team lacks {k}");
        }
        for k in ["provider", "signed_in", "email", "billing", "terminal_open", "signin", "signed_out_at"] {
            assert!(t["ai"].get(k).is_some(), "ai lacks {k}");
        }
        assert_eq!(t["update"]["takes"], "about 4 minutes");
        assert!(v["connect"].get("email").is_some(), "connect lacks email");
        for k in ["memory_mb", "cpus", "vm_support"] {
            assert!(v["host"].get(k).is_some(), "host lacks {k}");
        }
        assert!(v["host"]["vm_support"].is_string());
        assert_eq!(t["state"], "paused");
        assert_eq!(t["place"], "here");
        assert_eq!(v["teams"][1]["place"], "elsewhere");
        assert_eq!(v["teams"][1]["phrase"], "on studio.example.com");
    }

    #[test]
    fn op_view_stages() {
        let line = |label: &str, stage: Option<&str>| Progress { label: label.into(), stage: stage.map(Into::into), progress: None };
        let mut op = TeamOp::new(OpKind::Create, 0);
        op.progress(&line("creating the data disk", Some("making-room")));
        op.progress(&line("VM started", Some("starting")));
        op.progress(&line("no stage", None));
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Starting up"));
        assert_eq!(v.stage_index, 1);
        assert_eq!(v.stages, ["Preparing storage", "Starting up", "Setting up your team", "Ready"]);
        assert_eq!(v.lines.len(), 3);
        assert!(v.stage_progress.is_none());
        op.running = false;
        op.finished = true;
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.percent), (Some("Ready"), 100));

        // Setup's create runs `up` twice: the machine boots through
        // setting-up while the wizard is open, then the install starts
        // at `starting` again. The bar stays on the later stage, and
        // the earlier stage's line is kept.
        let mut op = TeamOp::new(OpKind::Create, 0);
        op.progress(&line("creating the data disk", Some("making-room")));
        op.progress(&line("VM started", Some("starting")));
        op.progress(&line("the VM is ready; Kivali installs once its owner is known", Some("setting-up")));
        let at = op.view().percent;
        op.progress(&line("the VM is already running", Some("starting")));
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.stage_index), (Some("Setting up your team"), 2));
        assert!(v.percent >= at, "the bar went back from {at}% to {}%", v.percent);
        assert_eq!(v.lines.last().map(String::as_str), Some("the VM is already running"));
        // A measure reported for an earlier stage does not move the bar either.
        op.progress(&Progress { label: String::new(), stage: Some("making-room".into()), progress: Some(StageProgress { done: 1, total: 10 }) });
        assert!(op.view().stage_progress.is_none());

        // A plain resume shows the stages it goes through, as before.
        let mut op = TeamOp::new(OpKind::Resume, 0);
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Starting up"));
        assert_eq!(v.stages, ["Starting up", "Setting up your team"]);
        op.progress(&line("VM started", Some("starting")));
        assert_eq!(op.view().stages, ["Starting up", "Setting up your team"]);
    }

    /// A resume that moves the data disk: the stage shows once the
    /// supervisor is in it, its reports fill the bar and make the
    /// caption's measure (in bytes), a report adds no line, and the next
    /// stage starts with no measure until it reports one.
    #[test]
    fn op_view_stage_progress() {
        let mut op = TeamOp::new(OpKind::Resume, 0);
        op.progress(&Progress { label: "the VM is not running".into(), stage: Some("starting".into()), progress: None });
        op.progress(&Progress { label: "updating the team's data".into(), stage: Some("updating-data".into()), progress: None });
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.stage_index), (Some("Updating team data"), 0));
        assert_eq!(v.stages, ["Updating team data", "Starting up", "Setting up your team"]);
        assert_eq!(v.stage_progress, None);

        let three = StageProgress { done: 3 << 30, total: 8 << 30 };
        op.progress(&Progress { label: String::new(), stage: Some("updating-data".into()), progress: Some(three) });
        let v = op.view();
        assert_eq!(v.stage_progress, Some(view::StageMeasure { done: 3 << 30, total: 8 << 30, unit: view::MeasureUnit::Bytes }));
        // 3/8 of the 120 seconds' share of 150.
        assert_eq!(v.percent, 30);
        assert_eq!(v.lines, ["the VM is not running", "updating the team's data"]);

        op.progress(&Progress { label: "the data disk's file holds 1.2 GiB of blocks the guest discarded; compacting it".into(), stage: Some("making-room".into()), progress: None });
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.stage_progress), (Some("Making room"), None));
        assert_eq!(v.stages, ["Updating team data", "Making room", "Starting up", "Setting up your team"]);
        op.progress(&Progress { label: String::new(), stage: Some("making-room".into()), progress: Some(StageProgress { done: 40, total: 100 }) });
        assert_eq!(op.view().stage_progress, Some(view::StageMeasure { done: 40, total: 100, unit: view::MeasureUnit::Share }));

        op.progress(&Progress { label: "VM started".into(), stage: Some("starting".into()), progress: None });
        let v = op.view();
        assert_eq!((v.stage.as_deref(), v.stage_index, v.stage_progress), (Some("Starting up"), 2, None));
    }

    /// An update's download and snapshot captions count bytes, its wait
    /// for the agents counts agents, and one that fails moves to a step
    /// of its own. Without it the bar guessed through both copies and the
    /// agents' start, and read "Starting up · step 4 of 4" while the
    /// update went back to the earlier version.
    #[test]
    fn op_view_update_progress_and_rollback() {
        let mut op = TeamOp::new(OpKind::Update, 4 << 30);
        let half = StageProgress { done: 256 << 20, total: 512 << 20 };
        op.progress(&Progress { label: String::new(), stage: Some("downloading".into()), progress: Some(half) });
        let v = op.view();
        assert_eq!(v.stage_progress, Some(view::StageMeasure { done: 256 << 20, total: 512 << 20, unit: view::MeasureUnit::Bytes }));
        assert_eq!(v.stages, ["Downloading", "Saving a snapshot", "Installing", "Starting up", "Starting your agents"]);
        // Half of 60 seconds' share of 304 (the 4 GB copy lengthens the snapshot).
        assert_eq!(v.percent, 9);

        let copied = StageProgress { done: 1 << 30, total: 4 << 30 };
        op.progress(&Progress { label: String::new(), stage: Some("snapshot".into()), progress: Some(copied) });
        assert_eq!(op.view().stage_progress, Some(view::StageMeasure { done: 1 << 30, total: 4 << 30, unit: view::MeasureUnit::Bytes }));

        op.progress(&Progress { label: "HelmChart now points at kivali-0.17.0.tgz".into(), stage: Some("installing".into()), progress: None });
        op.progress(&Progress { label: "rolling back to 0.16.0".into(), stage: Some("rolling-back".into()), progress: None });
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Going back to the previous version"));
        assert_eq!(v.stages, ["Downloading", "Saving a snapshot", "Installing", "Going back to the previous version"]);
        assert_eq!(v.stage_index, 3);

        // An update from the VM image downloads nothing: its first stage
        // says what it does instead, and the rest reads as any update.
        let mut op = TeamOp::with(OpKind::Update, 0, true);
        op.progress(&Progress { label: "chart kivali 0.17.0 is baked into the VM image".into(), stage: Some("downloading".into()), progress: None });
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Checking the update"));
        assert_eq!(v.stages, ["Checking the update", "Saving a snapshot", "Installing", "Starting up", "Starting your agents"]);
        assert_eq!(op.lengths[0], view::BAKED_FIRST_STAGE.1);

        // An update that gets to its agents counts them as they connect.
        let mut op = TeamOp::new(OpKind::Update, 0);
        op.progress(&Progress { label: "waiting for the agents to connect to the server".into(), stage: Some("starting-agents".into()), progress: None });
        op.progress(&Progress { label: String::new(), stage: Some("starting-agents".into()), progress: Some(StageProgress { done: 3, total: 5 }) });
        let v = op.view();
        assert_eq!(v.stage.as_deref(), Some("Starting your agents"));
        assert_eq!(v.stage_progress, Some(view::StageMeasure { done: 3, total: 5, unit: view::MeasureUnit::Agents }));
    }

    /// An update's snapshot costs time for the data disk's bytes only
    /// where it copies the disk: on macOS the estimate stays the same
    /// whatever the disk holds.
    #[test]
    fn snapshot_copy_bytes_follows_the_platform() {
        let r = Report { disk_used_bytes: 9 << 30, ..Default::default() };
        assert_eq!(copy_bytes(true, Some(&r)), 9 << 30);
        assert_eq!(copy_bytes(false, Some(&r)), 0);
        assert_eq!(copy_bytes(true, None), 0);
        assert_eq!(snapshot_copy_bytes(Some(&r)), if platform::UPDATE_COPIES_DATA_DISK { 9 << 30 } else { 0 });
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

    // An app update closes every runtime before its installer starts. On
    // Windows the installer can fail to start (the permission prompt
    // answered No, a standard user): if the runtimes stayed closed, every
    // Resume afterwards would stop the supervisor it had just started and
    // answer "Kivali is quitting." until Kivali was quit and reopened.
    #[test]
    fn a_closed_runtime_opens_again() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        let rt = sh.runtime(&a).unwrap();
        assert!(rt.close_sidecar().is_none());
        assert!(rt.sidecar.lock().unwrap().closed);
        rt.reopen();
        assert!(!rt.sidecar.lock().unwrap().closed);
    }

    /// A team setup made and never finished stays setup's across launches:
    /// the next shell reads it back, opens setup for it rather than its
    /// window, and lets setup remove it; finishing ends that on disk too.
    #[test]
    fn unfinished_setup_survives_a_restart() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let b = teams.add_here("B", Kind::Work, "a@example.com", &mut || 2);
        set_provisional(&mut teams, &a, true);
        teams.save(dir.path()).unwrap();

        let (loaded, _) = TeamsFile::load(dir.path(), 0).unwrap();
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), loaded, None);
        assert!(setup_unfinished(&sh, &a));
        assert!(!setup_unfinished(&sh, &b));
        assert!(sh.runtime(&a).unwrap().provisional.load(Ordering::SeqCst));
        assert!(!sh.runtime(&b).unwrap().provisional.load(Ordering::SeqCst));
        assert!(sh.team_view(&sh.team(&a).unwrap()).provisional);

        sh.end_setup(&a);
        assert!(!setup_unfinished(&sh, &a));
        assert!(!sh.runtime(&a).unwrap().provisional.load(Ordering::SeqCst));
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert!(!on_disk.get(&a).unwrap().local().unwrap().provisional);
    }

    /// The first-boot update's one attempt: claimed when the VM image bakes
    /// a newer release, recorded on disk so the next start does not claim
    /// it again, given back when no operation came of it, and never
    /// claimed for a provisional team, a release needing steps by hand or
    /// an image no newer than the team. Without the record every launch
    /// would retry a failed update; without the release a quit during the
    /// first boot would use the attempt up with no update run.
    #[test]
    fn first_boot_update_is_claimed_once() {
        use crate::supervisor::wire::BakedRelease;
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let p = teams.add_here("P", Kind::Work, "a@example.com", &mut || 2);
        set_provisional(&mut teams, &p, true);
        teams.save(dir.path()).unwrap();
        let (loaded, _) = TeamsFile::load(dir.path(), 0).unwrap();
        let sh = Shell::new(dir.path().to_path_buf(), "0.17.0".into(), loaded, None);
        let report = |baked: Option<BakedRelease>| Report { baked, ..running_on(8080) };
        let rt = sh.runtime(&a).unwrap();

        *rt.report.lock().unwrap() = Some(report(None));
        assert_eq!(sh.claim_first_boot_update(&rt), None);
        *rt.report.lock().unwrap() = Some(report(Some(BakedRelease { version: "0.17.0".into(), newer: false, manual_steps: false })));
        assert_eq!(sh.claim_first_boot_update(&rt), None);
        *rt.report.lock().unwrap() = Some(report(Some(BakedRelease { version: "0.18.0".into(), newer: true, manual_steps: true })));
        assert_eq!(sh.claim_first_boot_update(&rt), None);

        let newer = Some(BakedRelease { version: "0.18.0".into(), newer: true, manual_steps: false });
        *rt.report.lock().unwrap() = Some(report(newer.clone()));
        assert_eq!(sh.claim_first_boot_update(&rt).as_deref(), Some("0.18.0"));
        assert_eq!(sh.claim_first_boot_update(&rt), None);
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert_eq!(on_disk.get(&a).unwrap().local().unwrap().first_boot_update.as_deref(), Some("0.18.0"));

        // Given back: the next start claims it again; a stale release
        // (another version recorded since) changes nothing.
        sh.release_first_boot_update(&a, "0.17.9");
        assert_eq!(sh.claim_first_boot_update(&rt), None);
        sh.release_first_boot_update(&a, "0.18.0");
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert_eq!(on_disk.get(&a).unwrap().local().unwrap().first_boot_update, None);
        assert_eq!(sh.claim_first_boot_update(&rt).as_deref(), Some("0.18.0"));

        // A provisional team is setup's.
        let prt = sh.runtime(&p).unwrap();
        *prt.report.lock().unwrap() = Some(report(newer));
        assert_eq!(sh.claim_first_boot_update(&prt), None);
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

    /// A report of a running team: guest ready, Kivali installed,
    /// forwarding on `port`.
    fn running_on(port: u16) -> Report {
        Report {
            running: true,
            forward: Some(format!("127.0.0.1:{port}")),
            guest: Some(crate::supervisor::wire::GuestStatus { state: "ready".into(), ..Default::default() }),
            state: crate::supervisor::wire::LocalState { kivali: "0.17.0".into(), port, ..Default::default() },
            ..Default::default()
        }
    }

    /// Someone moves a team that couldn't start on its port with
    /// `kivali-supervisor up --port`, as the couldn't-start page says.
    /// Without the watch loop following the status, the window kept the
    /// old port and showed couldn't start until the team was paused and
    /// resumed.
    #[test]
    fn a_port_moved_by_hand_is_followed() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        teams.get_mut(&a).and_then(Team::local_mut).unwrap().port = 8080;
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        let rt = sh.runtime(&a).unwrap();

        // Still failing on its port: the failure stays, the port too.
        let failing = Report {
            running: true,
            forward: None,
            forward_error: Some("Another program is using port 8080 on this computer.".into()),
            ..running_on(8080)
        };
        assert!(port_failure(&failing).is_some());
        *rt.failure.lock().unwrap() = port_failure(&failing).map(|w| format!("{PORT_FAILURE}{w}"));
        *rt.report.lock().unwrap() = Some(failing);
        assert_eq!(sh.follow_status(&rt), (false, false));
        assert_eq!(rt.state().0, TeamState::Failed);

        // Moved and running: the new port is the team's, on disk too,
        // and the team runs.
        *rt.report.lock().unwrap() = Some(running_on(18090));
        assert_eq!(sh.follow_status(&rt), (true, true));
        assert_eq!(rt.state().0, TeamState::Running);
        assert_eq!(sh.team(&a).unwrap().origin().as_deref(), Some("http://127.0.0.1:18090"));
        let (on_disk, _) = TeamsFile::load(dir.path(), 0).unwrap();
        assert_eq!(on_disk.get(&a).unwrap().local().unwrap().port, 18090);
        assert_eq!(on_disk.running_at_quit, std::slice::from_ref(&a));
        assert_eq!(sh.follow_status(&rt), (false, false));
    }

    /// Only a port failure ends when the status says running: a start
    /// that failed some other way (the server never served) keeps its
    /// couldn't-start page and Try again, though the VM still reports
    /// running.
    #[test]
    fn other_failures_outlast_a_running_status() {
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        let rt = sh.runtime(&a).unwrap();
        *rt.failure.lock().unwrap() = Some("Kivali did not start serving in time".into());
        *rt.report.lock().unwrap() = Some(running_on(8080));
        assert_eq!(sh.follow_status(&rt), (true, false));
        assert_eq!(rt.state().0, TeamState::Failed);
        // A report with no running VM never names a port failure.
        assert_eq!(port_failure(&Report { forward_error: Some("port 8080".into()), ..Default::default() }), None);
    }

    /// A team whose recorded port another program holds: the supervisor
    /// fails `up` and keeps the VM running. Without the stop that follows,
    /// the team showed couldn't start with its VM still using its memory,
    /// uncounted when another team resumed, and with no Pause to stop it.
    /// The reason stays on the couldn't-start page after the VM stops.
    #[test]
    fn a_port_failure_leaves_nothing_running() {
        let fake = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap()
            .join("fake-supervisor/target/debug")
            .join(format!("kivali-supervisor{}", std::env::consts::EXE_SUFFIX));
        assert!(fake.exists(), "build the fake first: cargo build --manifest-path fake-supervisor/Cargo.toml");
        std::env::set_var("FAKE_STEP_MS", "0");
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut || 1);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        let rt = sh.runtime(&a).unwrap();
        std::fs::create_dir_all(&rt.dir).unwrap();
        let probe = Supervisor::new(&rt.dir);
        let sc = Sidecar::start(&fake, &rt.dir, None, &|| probe.is_listening(), &std::thread::yield_now, &Instant::now).unwrap();

        // The machine prepared on a free port, which is then recorded, and
        // another program takes the port before the team installs.
        let port = std::net::TcpListener::bind(("127.0.0.1", 0)).unwrap().local_addr().unwrap().port();
        let prepare = UpRequest { port_hint: port, prepare: true, ..Default::default() };
        assert_eq!(rt.supervisor.up(&prepare, &mut |_| {}).unwrap().port, Some(port));
        rt.supervisor.down(false, &mut |_| {}).unwrap();
        let _other = std::net::TcpListener::bind(("127.0.0.1", port)).unwrap();
        let req = UpRequest { owner: Some("a@example.com".into()), ..Default::default() };
        let e = rt.supervisor.up(&req, &mut |_| {}).unwrap_err();
        assert!(rt.refresh_status().unwrap().running, "the supervisor leaves the VM running");

        let mut lines = Vec::new();
        rt.failed_up(&e, &mut |p| lines.push(p.label));
        assert!(!rt.refresh_status().unwrap().running, "the VM still runs");
        assert!(lines.iter().any(|l| l.contains("can't be reached on its port")), "{lines:?}");
        let v = sh.team_view(&sh.team(&a).unwrap());
        assert_eq!(v.state, TeamState::Failed);
        assert!(v.reason.as_deref().is_some_and(|r| r.contains(&format!("port {port}"))), "{:?}", v.reason);
        assert_eq!(sh.running_memory_mb("").0, 0);

        sc.stop(&mut || rt.supervisor.down(true, &mut |_| {}).map_err(|e| e.to_string()), STOP_TIMEOUTS);
    }

    /// Quit stops every team at once and Kivali exits. With no operation
    /// around that stop, the team's window kept its web app until Kivali
    /// vanished, and a tray click mid-stop read the supervisor's report
    /// (the VM up, its forward already dropped) as the team waking up, so
    /// the window and the tray said it was starting. Every redraw while
    /// it stops must show it pausing, and the operation must have ended
    /// once it has stopped: when an update's installer cannot start the
    /// team resumes, which a Pause still running would refuse. A team
    /// already paused must not flash a pausing page.
    #[test]
    fn quit_shows_a_running_team_pausing() {
        let fake = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap()
            .join("fake-supervisor/target/debug")
            .join(format!("kivali-supervisor{}", std::env::consts::EXE_SUFFIX));
        assert!(fake.exists(), "build the fake first: cargo build --manifest-path fake-supervisor/Cargo.toml");
        std::env::set_var("FAKE_STEP_MS", "0");
        let dir = tempfile::tempdir().unwrap();
        let mut teams = TeamsFile::default();
        let mut n = 0u16;
        let mut next = || {
            n += 1;
            n
        };
        let a = teams.add_here("A", Kind::Work, "a@example.com", &mut next);
        let b = teams.add_here("B", Kind::Work, "b@example.com", &mut next);
        let sh = Shell::new(dir.path().to_path_buf(), "0.15.1".into(), teams, None);
        let rt = sh.runtime(&a).unwrap();
        std::fs::create_dir_all(&rt.dir).unwrap();
        let probe = Supervisor::new(&rt.dir);
        let sc = Sidecar::start(&fake, &rt.dir, None, &|| probe.is_listening(), &std::thread::yield_now, &Instant::now).unwrap();
        let port = std::net::TcpListener::bind(("127.0.0.1", 0)).unwrap().local_addr().unwrap().port();
        let req = UpRequest { owner: Some("a@example.com".into()), port_hint: port, ..Default::default() };
        rt.supervisor.up(&req, &mut |_| {}).unwrap();
        let _ = rt.refresh_status();
        assert!(rt.is_running());

        let seen = Mutex::new(Vec::new());
        rt.pause_for_exit(&|| seen.lock().unwrap().push(rt.state()));
        let seen = seen.into_inner().unwrap();
        // Once as it begins, then once for each of the stop's lines.
        assert!(seen.len() > 1, "{seen:?}");
        assert!(seen.iter().all(|s| *s == (TeamState::Starting, Some(Activity::Pausing))), "{seen:?}");
        let op = rt.op.lock().unwrap().clone().unwrap();
        assert!(op.finished && !op.running);
        assert!(op.lines.iter().any(|l| l.contains("powering off")), "{:?}", op.lines);
        assert_eq!(rt.state(), (TeamState::Paused, None));
        rt.wait_idle();

        let paused = sh.runtime(&b).unwrap();
        paused.pause_for_exit(&|| panic!("B was already paused"));
        assert!(paused.op.lock().unwrap().is_none());

        sc.stop(&mut || rt.supervisor.down(true, &mut |_| {}).map_err(|e| e.to_string()), STOP_TIMEOUTS);
    }
}
