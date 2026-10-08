//! The files Kivali Desktop keeps in its config directory
//! ([`crate::platform::config_dir`], shared with the supervisor), and
//! how it writes them.

use std::path::Path;

/// The pid of the `serve` the shell started (adoption after a crash).
pub const PID_FILE: &str = "supervisor.pid";
/// Held by the one running shell ([`crate::platform::InstanceLock`]).
pub const LOCK_FILE: &str = "shell.lock";

/// Tests that spawn a process hold this for reading; the lock test holds
/// it for writing. A spawn copies the parent's descriptor table into the
/// child, and descriptors marked close-on-exec (all of std's) are closed
/// only when the child execs. A flock belongs to the open file, so a
/// lock file open in this process while another test spawns stays locked
/// in that window even after this process drops it: dropping and
/// retaking the lock failed about one run in seven. Windows has no such
/// window (std opens every handle non-inheritable, and a child inherits
/// only inheritable ones), so only Unix tests take it.
#[cfg(all(test, unix))]
pub(crate) static SPAWNING: std::sync::RwLock<()> = std::sync::RwLock::new(());

/// Held by a test while it spawns processes (see [`SPAWNING`]).
#[cfg(all(test, unix))]
pub(crate) fn spawning() -> std::sync::RwLockReadGuard<'static, ()> {
    SPAWNING.read().unwrap_or_else(|e| e.into_inner())
}

/// Creates the directory owner-only if it is missing. An existing
/// directory is left alone: the supervisor may have made it.
pub fn ensure_dir(dir: &Path) -> std::io::Result<()> {
    if dir.is_dir() {
        return Ok(());
    }
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    crate::platform::owner_only_dir(&mut b);
    b.create(dir)
}

/// Opens `path` for appending, creating it with `mode` where the
/// platform has modes.
pub fn open_append(path: &Path, mode: u32) -> std::io::Result<std::fs::File> {
    let mut opts = std::fs::OpenOptions::new();
    opts.create(true).append(true);
    crate::platform::file_mode(&mut opts, mode);
    opts.open(path)
}

/// Writes a file atomically (temporary file + rename) with `mode` where
/// the platform has modes.
pub fn write_atomic(path: &Path, bytes: &[u8], mode: u32) -> std::io::Result<()> {
    use std::io::Write;
    let dir = path.parent().unwrap_or_else(|| Path::new("."));
    ensure_dir(dir)?;
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let tmp = dir.join(format!(".{name}.tmp-{}", std::process::id()));
    {
        let mut opts = std::fs::OpenOptions::new();
        opts.write(true).create(true).truncate(true);
        crate::platform::file_mode(&mut opts, mode);
        let mut f = opts.open(&tmp)?;
        f.write_all(bytes)?;
        f.sync_all()?;
    }
    std::fs::rename(&tmp, path)
}
