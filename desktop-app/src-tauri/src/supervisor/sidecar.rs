//! The bundled `kivali-supervisor` binary: finding it, starting
//! `kivali-supervisor serve`, adopting one a previous shell left
//! running, and stopping it. The OS facts (who listens, who its parent
//! is, when it started, how to wait for it) come from
//! [`crate::platform`]; the rules are here.
//!
//! Tauri's bundler copies `externalBin` entries next to the app's main
//! executable, and `tauri dev` copies it next to the debug binary, so
//! both resolve the same way. `KIVALI_SUPERVISOR` overrides the path.

use crate::platform::{self, StartTime};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

pub const BINARY: &str = "kivali-supervisor";

/// How long `serve` has to start answering on its endpoint.
pub const START_DEADLINE: Duration = Duration::from_secs(15);

pub fn binary_path() -> Result<PathBuf, String> {
    if let Some(p) = std::env::var_os("KIVALI_SUPERVISOR").filter(|p| !p.is_empty()) {
        return Ok(PathBuf::from(p));
    }
    let exe = std::env::current_exe().map_err(|e| format!("cannot find the app's own binary: {e}"))?;
    let dir = exe.parent().ok_or("the app's binary has no directory")?;
    Ok(dir.join(BINARY).with_extension(std::env::consts::EXE_EXTENSION))
}

/// `kivali-supervisor --config-dir DIR serve [--vm-dir VM]`.
pub fn serve_args(dir: &Path, vm_dir: Option<&Path>) -> Vec<std::ffi::OsString> {
    let mut a: Vec<std::ffi::OsString> = vec!["--config-dir".into(), dir.into(), "serve".into()];
    if let Some(vm) = vm_dir {
        a.push("--vm-dir".into());
        a.push(vm.into());
    }
    a
}

/// The log `serve` writes to; the supervisor's own `up` uses the same.
pub fn log_path(dir: &Path) -> PathBuf {
    dir.join("logs").join("supervisor.log")
}

/// Where the shell records the pid of the `serve` it started.
pub fn pid_path(dir: &Path) -> PathBuf {
    dir.join(crate::paths::PID_FILE)
}

/// How long each step of [`Sidecar::stop`] waits for the process to end.
#[derive(Debug, Clone, Copy)]
pub struct StopTimeouts {
    /// After `down --exit` answered: serve ends right after its last line.
    pub after_down: Duration,
    /// After [`platform::request_stop`]: serve stops the VM first.
    pub after_request: Duration,
}

/// `after_request` outlasts the supervisor's own bound on a VM stop
/// (`stopTimeout`, 3 minutes, after which it powers the VM off itself)
/// with a margin: on macOS the VM runs inside `serve`, so killing `serve`
/// while the guest is still shutting down is a power cut on its data
/// disk. Only a `serve` wedged past its own bound is killed. A stop that
/// is progressing says so every 10 s, inside `after_down`'s idle limit.
pub const STOP_TIMEOUTS: StopTimeouts =
    StopTimeouts { after_down: Duration::from_secs(30), after_request: Duration::from_secs(240) };

/// How a [`Sidecar::stop`] ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stopped {
    /// `down --exit` over the RPC, and the process ended.
    Down,
    /// The platform's stop request (SIGTERM), and the process ended.
    Requested,
    /// Killed.
    Killed,
    /// An adopted process that was already gone, or whose pid now names
    /// another process: nothing was sent.
    Gone,
}

/// A supervisor this shell owns: one it started, or one a previous shell
/// started and left behind (adopted, see [`Sidecar::adopt`]). Owned
/// supervisors are stopped with the shell; others are not.
pub struct Sidecar {
    proc: Proc,
    dir: PathBuf,
}

enum Proc {
    Child(Child),
    /// Not our child, so its pid can die and be reused while we hold it:
    /// every signal and wait first checks the start time recorded at
    /// adoption (`lookup` is [`platform::start_time`], replaceable in
    /// tests).
    Adopted { pid: u32, start: StartTime, lookup: fn(u32) -> Option<StartTime> },
}

/// Whether `pid` is still the process that started at `start`. A dead
/// pid, or one now held by another process, is not.
pub fn same_process(pid: u32, start: StartTime, lookup: fn(u32) -> Option<StartTime>) -> bool {
    lookup(pid) == Some(start)
}

/// The adoption rule, kept pure for testing: the pid file must name the
/// process serving the endpoint, and that process's parent must be gone
/// (`orphaned`, as the platform judges it) or be this shell. A
/// supervisor started from a terminal, or by another running shell, is
/// not ours.
pub fn adoptable(pid_file: Option<u32>, peer: Option<u32>, parent_of_peer: Option<u32>, orphaned: bool, me: u32) -> bool {
    match (pid_file, peer, parent_of_peer) {
        (Some(recorded), Some(peer), Some(ppid)) => recorded == peer && (orphaned || ppid == me),
        _ => false,
    }
}

impl Sidecar {
    /// Takes ownership of the supervisor already serving `endpoint` when
    /// [`adoptable`] says so.
    pub fn adopt(dir: &Path, endpoint: &Path) -> Option<Sidecar> {
        let recorded = std::fs::read_to_string(pid_path(dir)).ok().and_then(|s| s.trim().parse().ok());
        let peer = platform::peer_pid(endpoint);
        let ppid = peer.and_then(platform::parent_pid);
        let orphaned = match (peer, ppid) {
            (Some(p), Some(pp)) => platform::is_orphan(p, pp),
            _ => false,
        };
        if !adoptable(recorded, peer, ppid, orphaned, std::process::id()) {
            return None;
        }
        let pid = peer?;
        let start = platform::start_time(pid)?;
        Some(Sidecar { proc: Proc::Adopted { pid, start, lookup: platform::start_time }, dir: dir.to_path_buf() })
    }

    pub fn pid(&self) -> u32 {
        match &self.proc {
            Proc::Child(c) => c.id(),
            Proc::Adopted { pid, .. } => *pid,
        }
    }

    /// For an adopted process: whether its pid still names it.
    fn still_ours(&self) -> bool {
        match &self.proc {
            Proc::Child(_) => true,
            Proc::Adopted { pid, start, lookup } => same_process(*pid, *start, *lookup),
        }
    }

    /// Starts `serve` with its output appended to the supervisor log,
    /// then waits until `is_listening` says the endpoint answers. The
    /// wait ends when it answers, when the process exits, or at
    /// [`START_DEADLINE`] by `now` (then the process is killed), so a
    /// transport that never answers cannot hang the shell.
    pub fn start(
        binary: &Path,
        dir: &Path,
        vm_dir: Option<&Path>,
        is_listening: &dyn Fn() -> bool,
        pause: &dyn Fn(),
        now: &dyn Fn() -> Instant,
    ) -> Result<Sidecar, String> {
        if !binary.exists() {
            return Err(format!("the Kivali supervisor is missing from the app ({})", binary.display()));
        }
        let log = log_path(dir);
        crate::paths::ensure_dir(log.parent().unwrap_or(dir)).map_err(|e| e.to_string())?;
        let out = crate::paths::open_append(&log, 0o600).map_err(|e| format!("cannot open {}: {e}", log.display()))?;
        let err = out.try_clone().map_err(|e| e.to_string())?;
        let mut cmd = Command::new(binary);
        cmd.args(serve_args(dir, vm_dir)).stdin(Stdio::null()).stdout(out).stderr(err);
        let mut child = platform::spawn_serve(&mut cmd).map_err(|e| format!("cannot start the Kivali supervisor: {e}"))?;
        let deadline = now() + START_DEADLINE;
        loop {
            if is_listening() {
                let _ = crate::paths::write_atomic(&pid_path(dir), format!("{}\n", child.id()).as_bytes(), 0o600);
                return Ok(Sidecar { proc: Proc::Child(child), dir: dir.to_path_buf() });
            }
            if let Some(status) = child.try_wait().map_err(|e| e.to_string())? {
                return Err(format!(
                    "the Kivali supervisor exited before it was ready ({status}); see {}",
                    log.display()
                ));
            }
            if now() >= deadline {
                let _ = child.kill();
                let _ = child.wait();
                return Err(format!(
                    "the Kivali supervisor did not answer within {} seconds; see {}",
                    START_DEADLINE.as_secs(),
                    log.display()
                ));
            }
            pause();
        }
    }

    /// Waits for the process to end, at most `timeout`; true once it
    /// has. An adopted process that is already gone (or whose pid now
    /// names another process) counts as ended. The identity check runs
    /// after the platform's wait is registered, so a pid reused before
    /// it is never waited on.
    fn wait_for(&mut self, timeout: Option<Duration>) -> bool {
        match &mut self.proc {
            Proc::Child(c) => {
                if matches!(c.try_wait(), Ok(Some(_)) | Err(_)) {
                    return true;
                }
                // An unreaped child's pid cannot be reused.
                platform::wait_exit(c.id(), &|| false, timeout) && c.wait().is_ok()
            }
            Proc::Adopted { pid, start, lookup } => {
                let (pid, start, lookup) = (*pid, *start, *lookup);
                platform::wait_exit(pid, &|| lookup(pid).is_some_and(|s| s != start), timeout)
            }
        }
    }

    /// Removes the pid file if it still names this process.
    fn forget(&self) {
        let path = pid_path(&self.dir);
        if std::fs::read_to_string(&path).ok().and_then(|s| s.trim().parse::<u32>().ok()) == Some(self.pid()) {
            let _ = std::fs::remove_file(path);
        }
    }

    /// Waits for the process to exit (after `down --exit`), however long
    /// that takes, then removes the pid file.
    pub fn wait(mut self) {
        self.wait_for(None);
        self.forget();
    }

    /// Stops `serve`, RPC first: `down` (`POST /v1/down {"exit": true}`)
    /// and up to `t.after_down` for the exit; then the platform's stop
    /// request ([`platform::request_stop`]: SIGTERM, on which `serve`
    /// stops the VM cleanly; nothing on Windows) and up to
    /// `t.after_request`; then a kill. An adopted process whose pid no
    /// longer names it is never signalled.
    pub fn stop(mut self, down: &mut dyn FnMut() -> Result<(), String>, t: StopTimeouts) -> Stopped {
        let how = if !self.still_ours() {
            Stopped::Gone
        } else if down().is_ok() && self.wait_for(Some(t.after_down)) {
            Stopped::Down
        } else if self.still_ours() && platform::request_stop(self.pid()) && self.wait_for(Some(t.after_request)) {
            Stopped::Requested
        } else {
            match &mut self.proc {
                Proc::Child(c) => {
                    let _ = c.kill();
                }
                Proc::Adopted { .. } => {
                    if self.still_ours() {
                        platform::kill(self.pid());
                    }
                }
            }
            self.wait_for(None);
            Stopped::Killed
        };
        self.forget();
        how
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn serve_arguments() {
        let a = serve_args(Path::new("/c"), None);
        assert_eq!(a, ["--config-dir", "/c", "serve"].map(std::ffi::OsString::from));
        let a = serve_args(Path::new("/c"), Some(Path::new("/r/vm")));
        assert_eq!(a, ["--config-dir", "/c", "serve", "--vm-dir", "/r/vm"].map(std::ffi::OsString::from));
    }

    #[test]
    fn binary_name_has_the_platform_suffix() {
        let p = binary_path().unwrap();
        let name = p.file_name().unwrap().to_string_lossy().into_owned();
        assert_eq!(name, if cfg!(windows) { "kivali-supervisor.exe" } else { "kivali-supervisor" });
    }

    #[test]
    fn start_reports_a_missing_binary() {
        let dir = tempfile::tempdir().unwrap();
        let err = match Sidecar::start(&dir.path().join("nope"), dir.path(), None, &|| true, &|| {}, &Instant::now) {
            Err(e) => e,
            Ok(_) => panic!("started"),
        };
        assert!(err.contains("missing"), "{err}");
    }

    #[test]
    fn adoption_rule() {
        let me = 500;
        // A serve whose shell died, or ours.
        assert!(adoptable(Some(42), Some(42), Some(1), true, me));
        assert!(adoptable(Some(42), Some(42), Some(me), false, me));
        // Started from a terminal, or by another live shell.
        assert!(!adoptable(Some(42), Some(42), Some(77), false, me));
        // The pid file names some other process.
        assert!(!adoptable(Some(41), Some(42), Some(1), true, me));
        // Anything unknown: not ours.
        assert!(!adoptable(None, Some(42), Some(1), true, me));
        assert!(!adoptable(Some(42), None, None, true, me));
        assert!(!adoptable(Some(42), Some(42), None, true, me));
    }

    fn other_start(_pid: u32) -> Option<StartTime> {
        Some(StartTime(12))
    }
    fn no_process(_pid: u32) -> Option<StartTime> {
        None
    }

    #[test]
    fn identity_check() {
        assert!(same_process(7, StartTime(12), other_start));
        assert!(!same_process(7, StartTime(13), other_start));
        assert!(!same_process(7, StartTime(12), no_process));
    }

    const FAST: StopTimeouts = StopTimeouts { after_down: Duration::from_millis(50), after_request: Duration::from_millis(50) };

    /// An adopted pid that now names another process (here: this test
    /// process itself, the worst case) is neither signalled nor waited
    /// on, and `down` is never sent: a stop would otherwise end this
    /// test run, or wait forever.
    #[test]
    fn a_reused_pid_is_left_alone() {
        let dir = tempfile::tempdir().unwrap();
        let me = std::process::id();
        let real = platform::start_time(me).unwrap();
        assert!(same_process(me, real, platform::start_time));
        let stale = Sidecar {
            proc: Proc::Adopted { pid: me, start: StartTime(real.0.wrapping_sub(1)), lookup: platform::start_time },
            dir: dir.path().to_path_buf(),
        };
        assert!(!stale.still_ours());
        let mut downs = 0;
        assert_eq!(stale.stop(&mut || { downs += 1; Ok(()) }, FAST), Stopped::Gone);
        assert_eq!(downs, 0);
        let gone = Sidecar {
            proc: Proc::Adopted { pid: me, start: real, lookup: no_process },
            dir: dir.path().to_path_buf(),
        };
        assert_eq!(gone.stop(&mut || Ok(()), FAST), Stopped::Gone);
    }
}

/// Tests that run real processes: a shell script stands in for `serve`.
#[cfg(all(test, unix))]
mod process_tests {
    use super::*;
    use std::cell::Cell;

    /// Writes an executable script under the lock tests write under (a
    /// file open for writing while another test spawns would leak into
    /// that child until it execs).
    fn script(dir: &Path, body: &str) -> PathBuf {
        let _no_spawns = crate::paths::SPAWNING.write().unwrap_or_else(|e| e.into_inner());
        let p = dir.join("fake-serve");
        crate::paths::write_atomic(&p, format!("#!/bin/sh\n{body}\n").as_bytes(), 0o700).unwrap();
        p
    }

    #[test]
    fn start_reports_an_early_exit() {
        let dir = tempfile::tempdir().unwrap();
        let bin = script(dir.path(), "exit 3");
        let _spawning = crate::paths::spawning();
        let err = match Sidecar::start(&bin, dir.path(), None, &|| false, &std::thread::yield_now, &Instant::now) {
            Err(e) => e,
            Ok(_) => panic!("started"),
        };
        assert!(err.contains("exited before it was ready"), "{err}");
        assert!(log_path(dir.path()).exists());
    }

    /// A process that never answers: the deadline (by an injected clock
    /// that moves a second per look) ends the wait and the process.
    #[test]
    fn start_gives_up_at_the_deadline() {
        let dir = tempfile::tempdir().unwrap();
        let bin = script(dir.path(), "exec sleep 600");
        let _spawning = crate::paths::spawning();
        let t0 = Instant::now();
        let looks = Cell::new(0u64);
        let clock = || {
            looks.set(looks.get() + 1);
            t0 + Duration::from_secs(looks.get())
        };
        let err = match Sidecar::start(&bin, dir.path(), None, &|| false, &|| {}, &clock) {
            Err(e) => e,
            Ok(_) => panic!("started"),
        };
        assert!(err.contains("did not answer within 15 seconds"), "{err}");
        // One look to set the deadline, then one per round until it passed.
        assert_eq!(looks.get(), 1 + START_DEADLINE.as_secs());
        assert!(!pid_path(dir.path()).exists());
    }

    #[test]
    fn start_records_the_pid_and_down_stops_it() {
        let dir = tempfile::tempdir().unwrap();
        // A marker file the script waits for plays `down --exit`.
        let marker = dir.path().join("down");
        let bin = script(dir.path(), &format!("while [ ! -e '{}' ]; do sleep 0.01; done", marker.display()));
        let _spawning = crate::paths::spawning();
        let sc = Sidecar::start(&bin, dir.path(), None, &|| true, &|| {}, &Instant::now).unwrap();
        let recorded: u32 = std::fs::read_to_string(pid_path(dir.path())).unwrap().trim().parse().unwrap();
        assert_eq!(recorded, sc.pid());
        let how = sc.stop(&mut || std::fs::write(&marker, b"").map_err(|e| e.to_string()), STOP_TIMEOUTS);
        assert_eq!(how, Stopped::Down);
        assert!(!pid_path(dir.path()).exists());
    }

    /// The RPC fails (nothing answers): the platform's stop request ends
    /// it.
    #[test]
    fn a_failed_down_falls_back_to_the_stop_request() {
        let dir = tempfile::tempdir().unwrap();
        let bin = script(dir.path(), "exec sleep 600");
        let _spawning = crate::paths::spawning();
        let sc = Sidecar::start(&bin, dir.path(), None, &|| true, &|| {}, &Instant::now).unwrap();
        let how = sc.stop(&mut || Err("not running".into()), STOP_TIMEOUTS);
        assert_eq!(how, Stopped::Requested);
        assert!(!pid_path(dir.path()).exists());
    }

    /// serve accepts the RPC but never answers (here: a listener that
    /// never accepts, so the request sits in its backlog): `down` gives
    /// up after `after_down` of silence instead of blocking the stop,
    /// and the stop request ends serve. The waits are real but short.
    #[test]
    fn a_wedged_down_times_out_and_escalates() {
        let dir = tempfile::tempdir().unwrap();
        let bin = script(dir.path(), "exec sleep 600");
        let sup = crate::supervisor::Supervisor::new(dir.path());
        let _wedged = std::os::unix::net::UnixListener::bind(sup.endpoint()).unwrap();
        let _spawning = crate::paths::spawning();
        let sc = Sidecar::start(&bin, dir.path(), None, &|| true, &|| {}, &Instant::now).unwrap();
        let t = StopTimeouts { after_down: Duration::from_millis(50), after_request: STOP_TIMEOUTS.after_request };
        let mut down_err = None;
        let begun = Instant::now();
        let how = sc.stop(
            &mut || {
                let r = sup.down_within(true, t.after_down, &mut |_| {}).map_err(|e| e.to_string());
                down_err = r.clone().err();
                r
            },
            t,
        );
        assert_eq!(how, Stopped::Requested);
        assert!(down_err.is_some_and(|e| e.contains("talking to the Kivali supervisor failed")));
        assert!(begun.elapsed() < STOP_TIMEOUTS.after_down, "{:?}", begun.elapsed());
        assert!(!pid_path(dir.path()).exists());
    }

    /// `down` answers but serve never ends, and it ignores the stop
    /// request: it is killed once both waits run out.
    #[test]
    fn a_stuck_serve_is_killed() {
        let dir = tempfile::tempdir().unwrap();
        // "Listening" only once the trap is set, so the stop request
        // cannot arrive before it.
        let ready = dir.path().join("ready");
        let bin = script(dir.path(), &format!("trap '' TERM\n: > '{}'\nwhile :; do sleep 1; done", ready.display()));
        let _spawning = crate::paths::spawning();
        let sc = Sidecar::start(&bin, dir.path(), None, &|| ready.exists(), &std::thread::yield_now, &Instant::now).unwrap();
        let mut downs = 0;
        let how = sc.stop(&mut || { downs += 1; Ok(()) }, FAST);
        assert_eq!((how, downs), (Stopped::Killed, 1));
    }

    const FAST: StopTimeouts = StopTimeouts { after_down: Duration::from_millis(50), after_request: Duration::from_millis(50) };
}
