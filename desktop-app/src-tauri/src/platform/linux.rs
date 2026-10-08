//! Linux: not a shipped platform, kept compiling so the common code is
//! never written against macOS by accident. XDG data directory,
//! SO_PEERCRED, /proc, pidfd. No terminal launcher and no app menu.

use super::{tray_set, AlertAnswer, AlertSpec, StartTime, TrayIcons};
use std::path::{Path, PathBuf};
use std::time::Duration;

pub fn default_config_dir() -> PathBuf {
    // $XDG_DATA_HOME, or ~/.local/share
    dirs::data_dir().unwrap_or_else(|| PathBuf::from(".")).join("kivali")
}

/// The pid of the process listening on the socket (SO_PEERCRED).
pub fn peer_pid(endpoint: &Path) -> Option<u32> {
    use std::os::fd::AsRawFd;
    let s = std::os::unix::net::UnixStream::connect(endpoint).ok()?;
    // SAFETY: getsockopt into a correctly sized ucred on an open socket.
    unsafe {
        let mut cred: libc::ucred = std::mem::zeroed();
        let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        let r = libc::getsockopt(s.as_raw_fd(), libc::SOL_SOCKET, libc::SO_PEERCRED, &mut cred as *mut _ as *mut libc::c_void, &mut len);
        (r == 0 && cred.pid > 0).then_some(cred.pid as u32)
    }
}

/// Fields of /proc/<pid>/stat after the command name, which may itself
/// hold spaces and parentheses: state is [0], ppid [1], starttime [19].
fn stat_fields(pid: u32) -> Option<Vec<String>> {
    let s = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let rest = &s[s.rfind(')')? + 1..];
    Some(rest.split_whitespace().map(str::to_string).collect())
}

pub fn parent_pid(pid: u32) -> Option<u32> {
    stat_fields(pid)?.get(1)?.parse().ok()
}

/// Clock ticks since boot; a zombie counts as gone.
pub fn start_time(pid: u32) -> Option<StartTime> {
    let f = stat_fields(pid)?;
    if f.first().map(String::as_str) == Some("Z") {
        return None;
    }
    f.get(19)?.parse().ok().map(StartTime)
}

/// pidfd_open + poll, at most `timeout`; see the macOS twin.
pub fn wait_exit(pid: u32, reused: &dyn Fn() -> bool, timeout: Option<Duration>) -> bool {
    // SAFETY: a pidfd we own, polled once and closed.
    unsafe {
        let fd = libc::syscall(libc::SYS_pidfd_open, pid as libc::pid_t, 0) as libc::c_int;
        if fd < 0 {
            return true;
        }
        let gone = if reused() {
            true
        } else {
            let ms = timeout.map_or(-1, |t| t.as_millis().min(i32::MAX as u128) as libc::c_int);
            let mut p = libc::pollfd { fd, events: libc::POLLIN, revents: 0 };
            loop {
                let n = libc::poll(&mut p, 1, ms);
                if n < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                break n > 0;
            }
        };
        libc::close(fd);
        gone
    }
}

pub fn open_terminal(_binary: &Path, _dir: &Path) -> Result<(), String> {
    Err("Kivali Desktop has no terminal launcher on Linux; run `kivali-supervisor terminal` yourself".into())
}

pub const TRAY_ICONS: TrayIcons = tray_set!("tray");
pub const TRAY_AREA: &str = "system tray";
pub const APP_MENU: bool = false;
pub const WINDOW_MENU: bool = false;
pub const SECOND_LAUNCH_FOCUSES: bool = false;
pub const INSTALL_EXITS: bool = false;

pub fn notify_running_instance(_dir: &Path) -> bool {
    false
}

pub fn on_second_launch(_dir: &Path, _show: Box<dyn Fn() + Send>) {}

pub fn is_reopen(_event: &tauri::RunEvent) -> bool {
    false
}

pub fn alert(app: &tauri::AppHandle, parent: Option<&tauri::Window>, spec: AlertSpec, done: Box<dyn FnOnce(AlertAnswer) + Send>) {
    super::plugin_dialog::alert(app, parent, spec, done)
}

/// No owner check on Linux.
pub fn authenticate(_app: &tauri::AppHandle, _reason: &str, done: Box<dyn FnOnce(Result<(), String>) + Send>) {
    done(Ok(()))
}

pub fn menu_bar_dark() -> bool {
    true
}

pub fn menus_dark() -> bool {
    false
}

pub fn mark_app_menus(_window: &tauri::menu::Submenu<tauri::Wry>, _help: &tauri::menu::Submenu<tauri::Wry>) {}

pub fn reduce_motion() -> bool {
    false
}

pub fn app_is_active() -> bool {
    false
}

/// Nothing to add: WebKitGTK asks wry about frames as well as the page.
pub fn guard_frames(_webview: &tauri::Webview, _may_load: Box<dyn Fn(&str) -> bool + Send>) {}

/// No menus in the windows ([`WINDOW_MENU`]).
pub fn menu_shortcuts(_webview: &tauri::Webview, _shortcut: Box<dyn Fn(&str) -> bool + Send>) {}

pub fn press_keys(_chord: &str) {}
