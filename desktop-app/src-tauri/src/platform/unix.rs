//! What macOS and Linux share: the flock instance lock, the Unix-socket
//! transport, plain spawns, signals, owner-only file modes, the shell's
//! log on fd 2.

use super::Stream;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Child, Command};

/// The supervisor's owner-only socket in the config directory.
pub const SOCKET_FILE: &str = "supervisor.sock";

/// The file that makes the bundle's `vm` resource an image the shell
/// passes on (`serve --vm-dir`): Virtualization.framework's root disk,
/// beside the kernel, the initramfs and VERSION.
pub const VM_ROOT_FILE: &str = "root.squashfs";

/// The one running shell for a config directory: an exclusive,
/// non-blocking flock on `<dir>/shell.lock`, held as long as this value
/// lives. The kernel drops it when the process ends, however it ends.
pub struct InstanceLock {
    _file: std::fs::File,
}

impl InstanceLock {
    /// `Ok(None)` means another shell holds the lock. flock belongs to
    /// the open file, so a second open in this same process is refused
    /// just as another process would be.
    pub fn acquire(dir: &Path) -> std::io::Result<Option<InstanceLock>> {
        crate::paths::ensure_dir(dir)?;
        let f = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .mode(0o600)
            .open(dir.join(crate::paths::LOCK_FILE))?;
        // SAFETY: flock on a file descriptor we own.
        if unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } == 0 {
            return Ok(Some(InstanceLock { _file: f }));
        }
        let e = std::io::Error::last_os_error();
        if e.kind() == std::io::ErrorKind::WouldBlock {
            Ok(None)
        } else {
            Err(e)
        }
    }
}

/// The RPC endpoint for a config directory: `<dir>/supervisor.sock`.
pub fn endpoint(dir: &Path) -> PathBuf {
    dir.join(SOCKET_FILE)
}

/// Whether something accepts connections on the endpoint now. Never
/// blocks: a connect to a Unix socket succeeds into the backlog or
/// fails at once.
pub fn is_listening(endpoint: &Path) -> bool {
    UnixStream::connect(endpoint).is_ok()
}

/// No broker here: the supervisor runs its VM itself.
pub fn broker_listening() -> Option<bool> {
    None
}

/// A connection to the endpoint. No socket file, or one nobody accepts
/// on, is `NotFound` or `ConnectionRefused`.
pub fn connect(endpoint: &Path) -> std::io::Result<Box<dyn Stream>> {
    Ok(Box::new(UnixStream::connect(endpoint)?))
}

/// [`connect`], with every read and write on the connection failing
/// (`WouldBlock`/`TimedOut`) once it has waited `idle` without progress.
pub fn connect_timeout(endpoint: &Path, idle: std::time::Duration) -> std::io::Result<Box<dyn Stream>> {
    let s = UnixStream::connect(endpoint)?;
    s.set_read_timeout(Some(idle))?;
    s.set_write_timeout(Some(idle))?;
    Ok(Box::new(s))
}

/// Starts `serve` as an ordinary child.
pub fn spawn_serve(cmd: &mut Command) -> std::io::Result<Child> {
    cmd.spawn()
}

/// SIGTERM: `serve` stops the VM cleanly on it. True when sent.
pub fn request_stop(pid: u32) -> bool {
    // SAFETY: kill(2); the caller has checked the pid still names its
    // process (an unreaped child, or an adopted one by start time).
    unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) == 0 }
}

/// SIGKILL, the last resort.
pub fn kill(pid: u32) -> bool {
    // SAFETY: as for request_stop.
    unsafe { libc::kill(pid as libc::pid_t, libc::SIGKILL) == 0 }
}

/// Whether a process whose parent is `ppid` has lost the shell that
/// started it: an orphan is re-parented to pid 1 (launchd, init).
pub fn is_orphan(_pid: u32, ppid: u32) -> bool {
    ppid == 1
}

/// Directories the shell creates are owner-only (0700).
pub fn owner_only_dir(b: &mut std::fs::DirBuilder) {
    b.mode(0o700);
}

/// Files the shell creates get `mode`.
pub fn file_mode(opts: &mut std::fs::OpenOptions, mode: u32) {
    opts.mode(mode);
}

/// Whether standard error is a terminal. Started from Finder or the
/// login item it is not, and the shell logs to a file instead.
pub fn stderr_is_seen() -> bool {
    // SAFETY: isatty on the process's own stderr.
    unsafe { libc::isatty(2) == 1 }
}

/// Makes `file` the process's standard error for the rest of its life:
/// `dup2` onto fd 2, which keeps the file open after `file` closes its
/// own descriptor. Standard output is left as it was.
pub fn redirect_output(file: std::fs::File) -> std::io::Result<()> {
    // SAFETY: dup2 of a descriptor this File holds open onto fd 2.
    if unsafe { libc::dup2(file.as_raw_fd(), 2) } < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// Before a flag prints: a terminal's output already reaches it.
pub fn attach_terminal() {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_instance_at_a_time() {
        let _no_spawns = crate::paths::SPAWNING.write().unwrap_or_else(|e| e.into_inner());
        let dir = tempfile::tempdir().unwrap();
        let first = InstanceLock::acquire(dir.path()).unwrap();
        assert!(first.is_some());
        assert!(InstanceLock::acquire(dir.path()).unwrap().is_none());
        drop(first);
        assert!(InstanceLock::acquire(dir.path()).unwrap().is_some());
    }

    #[test]
    fn listening_and_connecting() {
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        assert!(!is_listening(&ep));
        let e = connect(&ep).err().unwrap();
        assert!(matches!(e.kind(), std::io::ErrorKind::NotFound | std::io::ErrorKind::ConnectionRefused), "{e:?}");
        let _l = std::os::unix::net::UnixListener::bind(&ep).unwrap();
        assert!(is_listening(&ep));
        assert!(connect(&ep).is_ok());
    }

    /// A server that accepts and never answers: the read gives up.
    #[test]
    fn a_silent_server_times_out() {
        use std::io::Read;
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        let _l = std::os::unix::net::UnixListener::bind(&ep).unwrap();
        let mut s = connect_timeout(&ep, std::time::Duration::from_millis(20)).unwrap();
        let e = s.read(&mut [0u8; 8]).unwrap_err();
        assert!(matches!(e.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut), "{e:?}");
    }

    #[test]
    fn orphans() {
        assert!(is_orphan(42, 1));
        assert!(!is_orphan(42, 77));
    }
}
