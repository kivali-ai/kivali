//! Windows: %LOCALAPPDATA%, an exclusively opened lock file, the
//! supervisor's named pipe, GetNamedPipeServerProcessId,
//! GetProcessTimes, WaitForSingleObject, Windows Terminal, the 32 px
//! tray set, a menu bar in each team window instead of an application
//! menu (WebView2's key event hands it the webview's keys), TaskDialog
//! alerts, Windows Hello, the registry's theme, WebView2's frame event
//! (the team window's frame guard), the standard handles of a
//! GUI-subsystem program (the shell's log, the flags' output).
//!
//! Built and tested on Windows: the tests below run the lock, the pipe,
//! the process facts, the second-launch event and the shell's log in a
//! process started without a console. What needs a desktop
//! (the alerts, the menus, Hello's prompt, the terminal, notifications)
//! has not been clicked: see docs/developers/desktop-app.md, "Known gaps".

use super::windows_terminal::{cmd_start_line, wt_args};
use super::windows_ui::{chord, keys, task_dialog_answer, FIRST_BUTTON_ID, ID_OK};
use super::{pipe_name, tray_set, AlertAnswer, AlertSpec, StartTime, Stream, TrayIcons};
use std::ffi::OsStr;
use std::os::windows::ffi::OsStrExt;
use std::os::windows::fs::OpenOptionsExt;
use std::os::windows::io::{AsRawHandle, IntoRawHandle};
use std::os::windows::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use windows_sys::Win32::Foundation::{
    CloseHandle, GetLastError, ERROR_PIPE_BUSY, ERROR_SEM_TIMEOUT, ERROR_SHARING_VIOLATION, FILETIME, HANDLE,
    INVALID_HANDLE_VALUE, NO_ERROR, WAIT_OBJECT_0, WAIT_TIMEOUT,
};
use windows_sys::Win32::Security::{EqualSid, GetTokenInformation, TokenUser, TOKEN_QUERY, TOKEN_USER};
use windows_sys::Win32::Storage::FileSystem::{GetFileType, FILE_TYPE_UNKNOWN, SECURITY_IDENTIFICATION};
use windows_sys::Win32::System::Console::{
    AttachConsole, GetConsoleWindow, GetStdHandle, SetStdHandle, ATTACH_PARENT_PROCESS, STD_ERROR_HANDLE, STD_HANDLE,
    STD_OUTPUT_HANDLE,
};
use windows_sys::Win32::System::Diagnostics::ToolHelp::{
    CreateToolhelp32Snapshot, Process32FirstW, Process32NextW, PROCESSENTRY32W, TH32CS_SNAPPROCESS,
};
use windows_sys::Win32::System::Pipes::{GetNamedPipeServerProcessId, PeekNamedPipe, WaitNamedPipeW};
use windows_sys::Win32::System::Threading::{
    CreateEventW, GetCurrentProcess, GetProcessTimes, OpenEventW, OpenProcess, OpenProcessToken, SetEvent,
    TerminateProcess, WaitForSingleObject,
    CREATE_NO_WINDOW, DETACHED_PROCESS, EVENT_MODIFY_STATE, INFINITE, PROCESS_QUERY_LIMITED_INFORMATION,
    PROCESS_SYNCHRONIZE, PROCESS_TERMINATE,
};
use windows_sys::Win32::UI::WindowsAndMessaging::{AllowSetForegroundWindow, ASFW_ANY};

pub fn default_config_dir() -> PathBuf {
    // %LOCALAPPDATA%: local, never roaming.
    dirs::data_local_dir().unwrap_or_else(|| PathBuf::from(".")).join("Kivali")
}

fn wide(s: &OsStr) -> Vec<u16> {
    s.encode_wide().chain(std::iter::once(0)).collect()
}

/// Closes a handle when dropped.
struct Owned(HANDLE);
impl Drop for Owned {
    fn drop(&mut self) {
        // SAFETY: a handle this module opened and nobody else closes.
        unsafe { CloseHandle(self.0) };
    }
}

fn open_process(pid: u32, access: u32) -> Option<Owned> {
    // SAFETY: OpenProcess returns null on failure.
    let h = unsafe { OpenProcess(access, 0, pid) };
    (!h.is_null()).then_some(Owned(h))
}

/// The one running shell for a config directory: `<dir>/shell.lock`
/// opened with no sharing, so a second open fails with a sharing
/// violation while this one lives. Windows closes the handle however
/// the process ends.
pub struct InstanceLock {
    _file: std::fs::File,
}

impl InstanceLock {
    pub fn acquire(dir: &Path) -> std::io::Result<Option<InstanceLock>> {
        crate::paths::ensure_dir(dir)?;
        let opened = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .share_mode(0)
            .open(dir.join(crate::paths::LOCK_FILE));
        match opened {
            Ok(f) => Ok(Some(InstanceLock { _file: f })),
            Err(e) if e.raw_os_error() == Some(ERROR_SHARING_VIOLATION as i32) => Ok(None),
            Err(e) => Err(e),
        }
    }
}

/// The file that makes the bundle's `vm` resource (`<install dir>\vm`,
/// beside Kivali.exe) an image the shell passes on (`serve --vm-dir`):
/// Hyper-V's root disk, beside VERSION.
pub const VM_ROOT_FILE: &str = "root.vhdx";

/// The supervisor's named pipe for this config directory ([`pipe_name`]).
pub fn endpoint(dir: &Path) -> PathBuf {
    PathBuf::from(pipe_name(dir))
}

/// The pid of the process serving an open pipe.
fn server_pid(pipe: &std::fs::File) -> Option<u32> {
    let mut pid = 0u32;
    // SAFETY: a pipe handle we hold open; pid is written on success.
    let ok = unsafe { GetNamedPipeServerProcessId(pipe.as_raw_handle() as HANDLE, &mut pid) };
    (ok != 0 && pid != 0).then_some(pid)
}

/// A process token's TOKEN_USER, in a buffer aligned for it.
fn token_user(process: HANDLE) -> Option<Vec<u64>> {
    // SAFETY: a token handle we own (closed on drop), queried twice: for
    // the size, then into a buffer of that size.
    unsafe {
        let mut token: HANDLE = std::ptr::null_mut();
        if OpenProcessToken(process, TOKEN_QUERY, &mut token) == 0 {
            return None;
        }
        let token = Owned(token);
        let mut need = 0u32;
        GetTokenInformation(token.0, TokenUser, std::ptr::null_mut(), 0, &mut need);
        if need == 0 {
            return None;
        }
        let mut buf = vec![0u64; (need as usize).div_ceil(8)];
        if GetTokenInformation(token.0, TokenUser, buf.as_mut_ptr().cast(), need, &mut need) == 0 {
            return None;
        }
        Some(buf)
    }
}

/// Whether the process serving the pipe runs as this user. Another
/// local account could create the pipe name first (squat it); its
/// server is never talked to.
fn served_by_this_user(pipe: &std::fs::File) -> bool {
    let Some(pid) = server_pid(pipe) else { return false };
    let Some(server) = open_process(pid, PROCESS_QUERY_LIMITED_INFORMATION) else { return false };
    // SAFETY: GetCurrentProcess is a pseudo-handle that needs no closing.
    let (Some(theirs), Some(ours)) = (token_user(server.0), token_user(unsafe { GetCurrentProcess() })) else {
        return false;
    };
    // SAFETY: both buffers hold a TOKEN_USER whose SID points into them.
    unsafe {
        let sid = |b: &Vec<u64>| (*(b.as_ptr() as *const TOKEN_USER)).User.Sid;
        EqualSid(sid(&theirs), sid(&ours)) != 0
    }
}

static SQUAT_LOGGED: AtomicBool = AtomicBool::new(false);

/// Opens the pipe and checks who serves it. A pipe held by another
/// account answers `ConnectionRefused`, as if nothing listened, and one
/// fixed line is logged (once).
fn open_pipe(endpoint: &Path) -> std::io::Result<std::fs::File> {
    let f = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        // The server may identify this client but never impersonate it.
        .security_qos_flags(SECURITY_IDENTIFICATION)
        .open(endpoint)?;
    if served_by_this_user(&f) {
        return Ok(f);
    }
    if !SQUAT_LOGGED.swap(true, Ordering::Relaxed) {
        eprintln!("kivali: the supervisor pipe is held by another account");
    }
    Err(std::io::Error::new(std::io::ErrorKind::ConnectionRefused, "the supervisor pipe is held by another account"))
}

/// Whether this user's supervisor serves the pipe now. Never blocks: a
/// pipe whose instances are all busy exists and counts as listening
/// (its server is checked when [`connect`] gets through).
pub fn is_listening(endpoint: &Path) -> bool {
    match open_pipe(endpoint) {
        Ok(_) => true,
        Err(e) => e.raw_os_error() == Some(ERROR_PIPE_BUSY as i32),
    }
}

/// The broker's pipe, one per machine (docs/developers/supervisor.md, "The
/// broker"; `internal/supervisor/broker`, `PipeName`).
const BROKER_PIPE: &str = r"\\.\pipe\kivali-broker";

/// Whether the broker's pipe exists. Its server is LocalSystem, never
/// this user, so [`is_listening`]'s owner check does not apply; who
/// serves it is the supervisor's check.
pub fn broker_listening() -> Option<bool> {
    Some(pipe_exists(Path::new(BROKER_PIPE)))
}

/// Whether a pipe of that name exists, asked without connecting, so the
/// probe takes none of the server's instances: `WaitNamedPipeW` answers
/// at once when there is no such pipe, and times out when every
/// instance is busy (it exists).
fn pipe_exists(name: &Path) -> bool {
    let name = wide(name.as_os_str());
    // SAFETY: a NUL-terminated wide string; the error is read right after.
    unsafe { WaitNamedPipeW(name.as_ptr(), 1) != 0 || GetLastError() == ERROR_SEM_TIMEOUT }
}

/// How long a connect waits for a busy pipe, in all.
const BUSY_WAIT_MS: u32 = 5_000;

/// A connection to the pipe, served by this user. A missing pipe is
/// `NotFound`, another account's `ConnectionRefused`, one whose
/// instances stay busy for five seconds `TimedOut`.
pub fn connect(endpoint: &Path) -> std::io::Result<Box<dyn Stream>> {
    let name = wide(endpoint.as_os_str());
    for _ in 0..5 {
        match open_pipe(endpoint) {
            Ok(f) => return Ok(Box::new(f)),
            Err(e) if e.raw_os_error() == Some(ERROR_PIPE_BUSY as i32) => {
                // SAFETY: a NUL-terminated wide string.
                unsafe { WaitNamedPipeW(name.as_ptr(), BUSY_WAIT_MS / 5) };
            }
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::new(std::io::ErrorKind::TimedOut, "the supervisor's pipe stayed busy"))
}

/// A pipe connection whose reads give up after `idle` without data. The
/// handle is synchronous (no read timeouts of its own), so a read first
/// waits, in a bounded loop, for `PeekNamedPipe` to report bytes or a
/// closed pipe. Writes go straight through: the RPC's requests are a few
/// hundred bytes, well inside the pipe's buffer.
struct TimedPipe {
    file: std::fs::File,
    idle: Duration,
}

impl std::io::Read for TimedPipe {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let deadline = std::time::Instant::now() + self.idle;
        loop {
            let mut avail = 0u32;
            // SAFETY: a peek of the byte count only, on a handle we hold.
            let ok = unsafe {
                PeekNamedPipe(
                    self.file.as_raw_handle() as HANDLE,
                    std::ptr::null_mut(),
                    0,
                    std::ptr::null_mut(),
                    &mut avail,
                    std::ptr::null_mut(),
                )
            };
            // Bytes waiting, or a broken pipe the read itself reports.
            if ok == 0 || avail > 0 {
                return self.file.read(buf);
            }
            if std::time::Instant::now() >= deadline {
                return Err(std::io::Error::new(std::io::ErrorKind::TimedOut, "the supervisor did not answer"));
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}

impl std::io::Write for TimedPipe {
    fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
        self.file.write(b)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        self.file.flush()
    }
}

/// [`connect`], with every read failing (`TimedOut`) once it has waited
/// `idle` without data.
pub fn connect_timeout(endpoint: &Path, idle: Duration) -> std::io::Result<Box<dyn Stream>> {
    let name = wide(endpoint.as_os_str());
    for _ in 0..5 {
        match open_pipe(endpoint) {
            Ok(file) => return Ok(Box::new(TimedPipe { file, idle })),
            Err(e) if e.raw_os_error() == Some(ERROR_PIPE_BUSY as i32) => {
                // SAFETY: a NUL-terminated wide string.
                unsafe { WaitNamedPipeW(name.as_ptr(), BUSY_WAIT_MS / 5) };
            }
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::new(std::io::ErrorKind::TimedOut, "the supervisor's pipe stayed busy"))
}

/// The pid of this user's process serving the pipe
/// (GetNamedPipeServerProcessId).
pub fn peer_pid(endpoint: &Path) -> Option<u32> {
    server_pid(&open_pipe(endpoint).ok()?)
}

/// A process's parent pid, from a process snapshot.
pub fn parent_pid(pid: u32) -> Option<u32> {
    // SAFETY: a snapshot handle we own, walked with a sized entry.
    unsafe {
        let snap = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
        if snap == INVALID_HANDLE_VALUE {
            return None;
        }
        let snap = Owned(snap);
        let mut e = PROCESSENTRY32W { dwSize: std::mem::size_of::<PROCESSENTRY32W>() as u32, ..Default::default() };
        let mut ok = Process32FirstW(snap.0, &mut e);
        while ok != 0 {
            if e.th32ProcessID == pid {
                return Some(e.th32ParentProcessID);
            }
            ok = Process32NextW(snap.0, &mut e);
        }
        None
    }
}

/// A live process's creation time; None when it is gone (or exited and
/// only held open by a handle).
pub fn start_time(pid: u32) -> Option<StartTime> {
    let h = open_process(pid, PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_SYNCHRONIZE)?;
    // SAFETY: a process handle we own; four FILETIMEs written on success.
    unsafe {
        if WaitForSingleObject(h.0, 0) != WAIT_TIMEOUT {
            return None;
        }
        let zero = FILETIME { dwLowDateTime: 0, dwHighDateTime: 0 };
        let (mut created, mut exited, mut kernel, mut user) = (zero, zero, zero, zero);
        if GetProcessTimes(h.0, &mut created, &mut exited, &mut kernel, &mut user) == 0 {
            return None;
        }
        Some(StartTime((u64::from(created.dwHighDateTime) << 32) | u64::from(created.dwLowDateTime)))
    }
}

/// Windows does not re-parent: an orphan's recorded parent pid is gone,
/// or names a process started after the child (a reused pid).
pub fn is_orphan(pid: u32, ppid: u32) -> bool {
    match (start_time(ppid), start_time(pid)) {
        (None, _) => true,
        (Some(parent), Some(child)) => parent > child,
        (Some(_), None) => false,
    }
}

/// `serve` without a console window, detached from the shell's.
pub fn spawn_serve(cmd: &mut Command) -> std::io::Result<Child> {
    cmd.creation_flags(CREATE_NO_WINDOW | DETACHED_PROCESS).spawn()
}

/// Windows has no polite signal for a windowless process: the RPC's
/// `down --exit` is the request. Nothing is sent.
pub fn request_stop(_pid: u32) -> bool {
    false
}

/// TerminateProcess, the last resort.
pub fn kill(pid: u32) -> bool {
    let Some(h) = open_process(pid, PROCESS_TERMINATE) else { return false };
    // SAFETY: a process handle we own.
    unsafe { TerminateProcess(h.0, 1) != 0 }
}

/// Waits for `pid` to exit (WaitForSingleObject on its handle), at most
/// `timeout`; see `wait_exit` on macOS for the meaning of the answer.
/// The handle keeps the pid from being reused while it is open, and
/// `reused` is checked once it is.
pub fn wait_exit(pid: u32, reused: &dyn Fn() -> bool, timeout: Option<Duration>) -> bool {
    let Some(h) = open_process(pid, PROCESS_SYNCHRONIZE) else { return true };
    if reused() {
        return true;
    }
    let ms = timeout.map_or(INFINITE, |t| t.as_millis().min(u128::from(INFINITE - 1)) as u32);
    // SAFETY: a process handle we own.
    unsafe { WaitForSingleObject(h.0, ms) == WAIT_OBJECT_0 }
}

/// Directories under %LOCALAPPDATA% inherit the user's own ACL.
pub fn owner_only_dir(_b: &mut std::fs::DirBuilder) {}

/// Files inherit the directory's ACL; there are no mode bits.
pub fn file_mode(_opts: &mut std::fs::OpenOptions, _mode: u32) {}

/// What a standard handle leads to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum StdHandle {
    /// Null or `INVALID_HANDLE_VALUE`: a GUI-subsystem program started
    /// from Explorer, the Start menu, the login item or the installer has
    /// no standard handles at all.
    Unset,
    /// A value that names nothing open in this process (a parent can
    /// pass handles it never made inheritable).
    Stale,
    /// An open console, file or pipe.
    Open,
}

/// What `h` is. `GetFileType` only queries: a value that names nothing
/// answers `FILE_TYPE_UNKNOWN` with an error, an open handle of a type it
/// has no name for answers it with `NO_ERROR`.
fn handle_state(h: HANDLE) -> StdHandle {
    if h.is_null() || h == INVALID_HANDLE_VALUE {
        return StdHandle::Unset;
    }
    // SAFETY: plain queries on a handle value; no handle is used.
    if unsafe { GetFileType(h) == FILE_TYPE_UNKNOWN && GetLastError() != NO_ERROR } {
        StdHandle::Stale
    } else {
        StdHandle::Open
    }
}

fn std_handle(id: STD_HANDLE) -> StdHandle {
    // SAFETY: GetStdHandle reads this process's own table.
    handle_state(unsafe { GetStdHandle(id) })
}

/// Whether the shell's output reaches anything without a log file: a
/// console, or a standard error the parent passed (a redirect or a
/// pipe). Pure, so the rule is tested apart from how a process started.
fn output_seen(console_window: bool, stderr: StdHandle) -> bool {
    console_window || stderr == StdHandle::Open
}

/// Whether the process's standard error reaches someone. A release build
/// is a GUI-subsystem program (main.rs): started from Explorer, the Start
/// menu or the login item it has no console and no standard handles, so
/// every `eprintln!` vanishes (std counts a write to a missing handle as
/// done). A debug build run from a terminal has a console, and a release
/// build whose output is redirected or piped has the parent's handles.
pub fn stderr_is_seen() -> bool {
    // SAFETY: GetConsoleWindow only queries.
    let console_window = !unsafe { GetConsoleWindow() }.is_null();
    output_seen(console_window, std_handle(STD_ERROR_HANDLE))
}

/// Makes `file` the process's standard output and error for the rest of
/// its life (`SetStdHandle`; the handle is never closed). std looks the
/// standard handle up with `GetStdHandle` on every write (`get_handle` in
/// its `sys/stdio/windows.rs`), so `eprintln!` and `println!` land in the
/// file from here on, and a child started with inherited stdio gets it.
pub fn redirect_output(file: std::fs::File) -> std::io::Result<()> {
    let h = file.into_raw_handle() as HANDLE;
    // SAFETY: an open handle this process owns and never closes.
    let set = unsafe { SetStdHandle(STD_ERROR_HANDLE, h) != 0 && SetStdHandle(STD_OUTPUT_HANDLE, h) != 0 };
    if set {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

/// Before a flag prints (`cli::answer`). A release build run from a
/// console has no console of its own, being a GUI-subsystem program, so
/// `Kivali --version` would print nothing. When standard output leads
/// nowhere, this attaches to the console of the process that started
/// this one (`AttachConsole(ATTACH_PARENT_PROCESS)`), which fills in the
/// unset standard handles; any still not open get `CONOUT$`. An output
/// that is redirected or piped is left alone, and so is a process with
/// no parent console (started from Explorer): there is nowhere to print.
///
/// The wrinkle: cmd.exe and PowerShell do not wait for a GUI-subsystem
/// program, so the prompt is back before the output, which lands under
/// it, and `%ERRORLEVEL%` or `$LASTEXITCODE` never see the exit code
/// (`start /wait Kivali --check` waits and sees it, and so does a pipe).
/// That is acceptable for a diagnostic command; the one fix, a
/// console-subsystem binary, would open a console window beside the app
/// at every launch.
pub fn attach_terminal() {
    if std_handle(STD_OUTPUT_HANDLE) == StdHandle::Open {
        return;
    }
    // SAFETY: AttachConsole fails harmlessly when the parent has no
    // console or this process has one already.
    if unsafe { AttachConsole(ATTACH_PARENT_PROCESS) } == 0 {
        return;
    }
    for id in [STD_OUTPUT_HANDLE, STD_ERROR_HANDLE] {
        if std_handle(id) == StdHandle::Open {
            continue;
        }
        if let Ok(console) = std::fs::OpenOptions::new().read(true).write(true).open("CONOUT$") {
            // SAFETY: an open console handle, owned here and never closed.
            unsafe { SetStdHandle(id, console.into_raw_handle() as HANDLE) };
        }
    }
}

/// Windows Terminal (`wt.exe`) when it is on PATH (it is an app
/// execution alias, which only `symlink_metadata` sees), else a console
/// window through `cmd.exe /c start`.
pub fn open_terminal(binary: &Path, dir: &Path) -> Result<(), String> {
    let wt = std::env::var_os("PATH")
        .into_iter()
        .flat_map(|p| std::env::split_paths(&p).collect::<Vec<_>>())
        .map(|d| d.join("wt.exe"))
        .find(|p| std::fs::symlink_metadata(p).is_ok());
    let spawned = match wt {
        Some(wt) => Command::new(wt).args(wt_args(binary, dir)).spawn(),
        None => {
            let line = cmd_start_line(binary, dir)?;
            Command::new("cmd.exe").raw_arg(line).creation_flags(CREATE_NO_WINDOW).spawn()
        }
    };
    spawned.map(drop).map_err(|e| format!("cannot open a terminal: {e}"))
}

/// 32x32, the same drawings as the menu bar's; the taskbar is dark
/// unless the person chose light ([`menu_bar_dark`]).
pub const TRAY_ICONS: TrayIcons = tray_set!("tray-win");
pub const TRAY_AREA: &str = "notification area";
/// No application menu: each team window has a menu bar of its own
/// ([`WINDOW_MENU`]).
pub const APP_MENU: bool = false;
/// A menu bar inside each team window (Tauri's per-window menu), its
/// shortcuts handed over from the webviews ([`menu_shortcuts`]).
pub const WINDOW_MENU: bool = true;
/// A second launch is refused and the running instance shows itself.
pub const SECOND_LAUNCH_FOCUSES: bool = true;
/// The updater runs the installer and exits the process inside
/// `install`: the org is stopped in its before-exit hook.
pub const INSTALL_EXITS: bool = true;

/// The event a second launch sets to wake the first:
/// `Local\kivali-shell-<the pipe's hash>`.
fn launch_event(dir: &Path) -> Vec<u16> {
    let pipe = pipe_name(dir);
    let hash = pipe.rsplit('-').next().unwrap_or_default();
    wide(OsStr::new(&format!(r"Local\kivali-shell-{hash}")))
}

/// Asks the running shell to show itself, and lets it take the
/// foreground. False when no shell listens.
pub fn notify_running_instance(dir: &Path) -> bool {
    let name = launch_event(dir);
    // SAFETY: a NUL-terminated name; the handle is closed on drop.
    unsafe {
        let h = OpenEventW(EVENT_MODIFY_STATE, 0, name.as_ptr());
        if h.is_null() {
            return false;
        }
        let h = Owned(h);
        AllowSetForegroundWindow(ASFW_ANY);
        SetEvent(h.0) != 0
    }
}

/// Runs `show` (on its own thread) each time a second launch asks.
pub fn on_second_launch(dir: &Path, show: Box<dyn Fn() + Send>) {
    let name = launch_event(dir);
    // SAFETY: an auto-reset event, created once and held for the life of
    // the process (never closed).
    let h = unsafe { CreateEventW(std::ptr::null(), 0, 0, name.as_ptr()) };
    if h.is_null() {
        eprintln!("kivali: cannot create the second-launch event: {}", std::io::Error::last_os_error());
        return;
    }
    let raw = h as usize;
    std::thread::spawn(move || loop {
        // SAFETY: the event handle above, alive for the process.
        if unsafe { WaitForSingleObject(raw as HANDLE, INFINITE) } != WAIT_OBJECT_0 {
            return;
        }
        show();
    });
}

pub fn is_reopen(_event: &tauri::RunEvent) -> bool {
    false
}

/// One alert at a time: a second waits until the first is answered, as
/// it would behind an app-modal NSAlert.
static ALERT: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// A TaskDialog (comctl32 v6, which the executable's manifest asks for:
/// tauri-build embeds the Common-Controls 6.0 dependency). The window
/// title is the product name, the main instruction the alert's title,
/// the content its message; the buttons are the spec's, in order, any
/// number of them, the first the default; Escape, Alt+F4 and the close
/// box answer the Cancel button; `suppression` is the verification
/// checkbox. TaskDialog has no destructive button style: an alert with a
/// destructive button shows the warning icon instead.
///
/// It runs on a thread of its own, not the main thread. On the main
/// thread TaskDialog's modal loop would run inside a tao event handler,
/// which holds back every other event (menus, the tray's picture,
/// progress) until the dialog closes, and spins a core meanwhile (tao
/// re-posts its paint message while its handler is busy); called from a
/// command or a webview's event, it would run inside that callback. With
/// a parent that shows, the parent comes to the front first and owns the
/// dialog, which disables it while the dialog shows and centres the
/// dialog on it (a sheet, more or less); without one the dialog has no
/// owner and takes the foreground itself. Alerts queue ([`ALERT`]);
/// `done` runs on the dialog's thread.
pub fn alert(app: &tauri::AppHandle, parent: Option<&tauri::Window>, spec: AlertSpec, done: Box<dyn FnOnce(AlertAnswer) + Send>) {
    let product = app.package_info().name.clone();
    let parent = parent.cloned();
    std::thread::spawn(move || {
        let _one = ALERT.lock().unwrap_or_else(|e| e.into_inner());
        // An alert often answers a click in the tray while another app is
        // in front: the parent comes forward first. Its HWND is asked for
        // after set_focus, a round trip through the main thread's queue,
        // so the dialog opens once the parent is in front.
        let owner = parent.filter(|w| w.is_visible().unwrap_or(false)).and_then(|w| {
            let _ = w.unminimize();
            let _ = w.set_focus();
            w.hwnd().ok().map(|h| h.0 as usize)
        });
        done(task_dialog(&product, &spec, owner));
    });
}

/// Shows one TaskDialog, modal to `owner` (an HWND) when there is one,
/// and waits for its answer.
fn task_dialog(product: &str, spec: &AlertSpec, owner: Option<usize>) -> AlertAnswer {
    use windows_sys::Win32::UI::Controls::{
        TaskDialogIndirect, TASKDIALOGCONFIG, TASKDIALOGCONFIG_0, TASKDIALOG_BUTTON, TDCBF_OK_BUTTON,
        TDF_ALLOW_DIALOG_CANCELLATION, TDF_POSITION_RELATIVE_TO_WINDOW, TD_WARNING_ICON,
    };
    let text = |s: &str| wide(OsStr::new(s));
    let (window_title, instruction, content) = (text(product), text(&spec.title), text(&spec.message));
    let labels: Vec<Vec<u16>> = spec.buttons.iter().map(|b| text(b)).collect();
    let buttons: Vec<TASKDIALOG_BUTTON> = labels
        .iter()
        .zip(FIRST_BUTTON_ID..)
        .map(|(label, id)| TASKDIALOG_BUTTON { nButtonID: id, pszButtonText: label.as_ptr() })
        .collect();
    let checkbox = spec.suppression.as_deref().map(text);
    let mut flags = TDF_ALLOW_DIALOG_CANCELLATION;
    if owner.is_some() {
        flags |= TDF_POSITION_RELATIVE_TO_WINDOW;
    }
    let config = TASKDIALOGCONFIG {
        cbSize: std::mem::size_of::<TASKDIALOGCONFIG>() as u32,
        hwndParent: owner.map_or(std::ptr::null_mut(), |h| h as _),
        dwFlags: flags,
        // An alert that names no buttons shows a lone OK.
        dwCommonButtons: if buttons.is_empty() { TDCBF_OK_BUTTON } else { 0 },
        pszWindowTitle: window_title.as_ptr(),
        Anonymous1: TASKDIALOGCONFIG_0 {
            pszMainIcon: if spec.destructive.is_some() { TD_WARNING_ICON } else { std::ptr::null() },
        },
        pszMainInstruction: instruction.as_ptr(),
        pszContent: content.as_ptr(),
        cButtons: buttons.len() as u32,
        pButtons: if buttons.is_empty() { std::ptr::null() } else { buttons.as_ptr() },
        nDefaultButton: if buttons.is_empty() { ID_OK } else { FIRST_BUTTON_ID },
        pszVerificationText: checkbox.as_ref().map_or(std::ptr::null(), |c| c.as_ptr()),
        ..Default::default()
    };
    let (mut pressed, mut checked): (i32, windows_sys::core::BOOL) = (0, 0);
    // SAFETY: the strings and the button array outlive the call, which
    // returns once the dialog has closed; the radio button's out-pointer
    // may be null.
    let hr = unsafe { TaskDialogIndirect(&config, &mut pressed, std::ptr::null_mut(), &mut checked) };
    if hr < 0 {
        eprintln!("kivali: cannot show an alert ({:#010x})", hr as u32);
    }
    task_dialog_answer(&spec.buttons, pressed, checked != 0, checkbox.is_some())
}

/// Hands a team window's menu the keys pressed in `webview`. WebView2
/// takes a focused webview's keys in its own process, so the event
/// loop's accelerator table never sees them; its `AcceleratorKeyPressed`
/// event, raised on the main thread for a key pressed with Ctrl or Alt
/// held or one that types no character, is asked instead. `shortcut`
/// gets each press as the menus spell it ([`chord`]: Ctrl and Shift from
/// `GetKeyState`, which WebView2's child window shares with this
/// thread, Alt from the event) and, when it is a menu shortcut, acts on
/// it and answers true: the webview then leaves the key alone. A held
/// key's repeats are not passed on, but stay away from the webview when
/// its first press was a shortcut.
pub fn menu_shortcuts(webview: &tauri::Webview, shortcut: Box<dyn Fn(&str) -> bool + Send>) {
    use webview2_com::AcceleratorKeyPressedEventHandler;
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        COREWEBVIEW2_KEY_EVENT_KIND, COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN, COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN,
        COREWEBVIEW2_PHYSICAL_KEY_STATUS,
    };
    use windows_sys::Win32::UI::Input::KeyboardAndMouse::{GetKeyState, VK_CONTROL, VK_SHIFT};
    let label = webview.label().to_string();
    let hooked = webview.with_webview(move |w| {
        // The key whose first press was a shortcut, while it repeats.
        let held = std::cell::Cell::new(None::<u32>);
        let handler = AcceleratorKeyPressedEventHandler::create(Box::new(move |_, args| {
            let Some(args) = args else { return Ok(()) };
            let mut kind = COREWEBVIEW2_KEY_EVENT_KIND::default();
            let mut vk = 0u32;
            let mut status = COREWEBVIEW2_PHYSICAL_KEY_STATUS::default();
            // SAFETY: the arguments of the event being raised.
            unsafe {
                args.KeyEventKind(&mut kind)?;
                args.VirtualKey(&mut vk)?;
                args.PhysicalKeyStatus(&mut status)?;
            }
            if kind != COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN && kind != COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN {
                return Ok(());
            }
            let handled = if status.WasKeyDown.as_bool() {
                held.get() == Some(vk)
            } else {
                // SAFETY: GetKeyState has no preconditions.
                let down = |key: u16| unsafe { GetKeyState(i32::from(key)) } < 0;
                let hit = chord(vk, down(VK_CONTROL), down(VK_SHIFT), status.IsMenuKeyDown.as_bool())
                    .is_some_and(|c| shortcut(&c));
                held.set(hit.then_some(vk));
                hit
            };
            if handled {
                // SAFETY: as above.
                unsafe { args.SetHandled(true)? };
            }
            Ok(())
        }));
        let mut token = 0i64;
        // SAFETY: the live controller of this webview, on the main thread.
        if let Err(e) = unsafe { w.controller().add_AcceleratorKeyPressed(&handler, &mut token) } {
            eprintln!("kivali: {label}: cannot hand its keys to the menu: {e}");
        }
    });
    if let Err(e) = hooked {
        eprintln!("kivali: cannot hand a team web view's keys to the menu: {e}");
    }
}

/// Presses `chord` (the menus' spelling, "Ctrl+C") as keyboard input,
/// which the focused window gets: a team window's Edit menu, whose
/// shortcuts are the webview's own.
pub fn press_keys(chord: &str) {
    use windows_sys::Win32::UI::Input::KeyboardAndMouse::{
        SendInput, INPUT, INPUT_0, INPUT_KEYBOARD, KEYBDINPUT, KEYEVENTF_KEYUP, VK_CONTROL, VK_MENU, VK_SHIFT,
    };
    let Some((ctrl, shift, alt, vk)) = keys(chord) else { return };
    let held: Vec<u16> = [(ctrl, VK_CONTROL), (shift, VK_SHIFT), (alt, VK_MENU)]
        .into_iter()
        .filter_map(|(on, key)| on.then_some(key))
        .collect();
    let key = |vk: u16, up: bool| INPUT {
        r#type: INPUT_KEYBOARD,
        Anonymous: INPUT_0 {
            ki: KEYBDINPUT { wVk: vk, dwFlags: if up { KEYEVENTF_KEYUP } else { 0 }, ..Default::default() },
        },
    };
    let mut inputs: Vec<INPUT> = held.iter().map(|&m| key(m, false)).collect();
    inputs.extend([key(vk, false), key(vk, true)]);
    inputs.extend(held.iter().rev().map(|&m| key(m, true)));
    // SAFETY: an array of keyboard inputs and its length.
    unsafe { SendInput(inputs.len() as u32, inputs.as_ptr(), std::mem::size_of::<INPUT>() as i32) };
}

/// What Windows Hello said, or None when it is not set up here.
fn hello(reason: &str) -> windows::core::Result<Option<Result<(), String>>> {
    use windows::core::{factory, HSTRING};
    use windows::Security::Credentials::UI::{
        UserConsentVerificationResult as R, UserConsentVerifier, UserConsentVerifierAvailability,
    };
    use windows::Win32::System::WinRT::IUserConsentVerifierInterop;
    use windows_future::IAsyncOperation;
    use windows_sys::Win32::UI::WindowsAndMessaging::GetForegroundWindow;

    if UserConsentVerifier::CheckAvailabilityAsync()?.join()? != UserConsentVerifierAvailability::Available {
        return Ok(None);
    }
    // The prompt is owned by the window in front (Kivali's, which asked),
    // so it shows over it rather than behind.
    // SAFETY: GetForegroundWindow has no preconditions.
    let hwnd = windows::Win32::Foundation::HWND(unsafe { GetForegroundWindow() });
    let interop = factory::<UserConsentVerifier, IUserConsentVerifierInterop>()?;
    // SAFETY: a window handle (or null) and a message string.
    let op: IAsyncOperation<R> = unsafe { interop.RequestVerificationForWindowAsync(hwnd, &HSTRING::from(reason))? };
    Ok(Some(match op.join()? {
        R::Verified => Ok(()),
        R::Canceled => Err("cancelled".into()),
        R::DeviceBusy => Err("Windows Hello is busy".into()),
        R::RetriesExhausted => Err("too many attempts".into()),
        R::DisabledByPolicy => Err("Windows Hello is turned off by policy".into()),
        _ => Err("Windows Hello is not available".into()),
    }))
}

/// Windows Hello (face, fingerprint or PIN), on its own thread. Where
/// Hello is not set up, a confirmation alert stands in (its first button
/// confirms). `done` runs on that thread, or the dialog's.
pub fn authenticate(app: &tauri::AppHandle, reason: &str, done: Box<dyn FnOnce(Result<(), String>) + Send>) {
    let (app, reason) = (app.clone(), reason.to_string());
    std::thread::spawn(move || match hello(&reason) {
        Ok(Some(answer)) => done(answer),
        Ok(None) => {
            let spec = AlertSpec {
                title: "Confirm".into(),
                message: reason,
                buttons: vec!["Continue".into(), "Cancel".into()],
                destructive: Some(0),
                suppression: None,
            };
            alert(&app, None, spec, Box::new(move |a| done(if a.button == 0 { Ok(()) } else { Err("cancelled".into()) })));
        }
        Err(e) => done(Err(e.message())),
    });
}

/// A DWORD under HKCU\…\Themes\Personalize, if set.
fn personalize(value: &str) -> Option<u32> {
    use windows_sys::Win32::System::Registry::{RegGetValueW, HKEY_CURRENT_USER, RRF_RT_REG_DWORD};
    let key = wide(OsStr::new(r"Software\Microsoft\Windows\CurrentVersion\Themes\Personalize"));
    let name = wide(OsStr::new(value));
    let mut data = 0u32;
    let mut size = std::mem::size_of::<u32>() as u32;
    // SAFETY: NUL-terminated names and a DWORD-sized buffer.
    let r = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            key.as_ptr(),
            name.as_ptr(),
            RRF_RT_REG_DWORD,
            std::ptr::null_mut(),
            (&mut data as *mut u32).cast(),
            &mut size,
        )
    };
    (r == 0).then_some(data)
}

/// Whether the taskbar is dark (`SystemUsesLightTheme` is 0, or unset:
/// Windows' default). Safe on any thread.
pub fn menu_bar_dark() -> bool {
    personalize("SystemUsesLightTheme") != Some(1)
}

/// Whether apps draw dark (`AppsUseLightTheme` is 0). Safe on any thread.
pub fn menus_dark() -> bool {
    personalize("AppsUseLightTheme") == Some(0)
}

/// No app menu here (APP_MENU is false).
pub fn mark_app_menus(_window: &tauri::menu::Submenu<tauri::Wry>, _help: &tauri::menu::Submenu<tauri::Wry>) {}

/// Settings → Accessibility → Visual effects → Animation effects, off.
pub fn reduce_motion() -> bool {
    use windows_sys::Win32::UI::WindowsAndMessaging::{SystemParametersInfoW, SPI_GETCLIENTAREAANIMATION};
    let mut on: windows_sys::core::BOOL = 1;
    // SAFETY: SPI_GETCLIENTAREAANIMATION writes one BOOL.
    let ok = unsafe {
        SystemParametersInfoW(SPI_GETCLIENTAREAANIMATION, 0, (&mut on as *mut windows_sys::core::BOOL).cast(), 0)
    };
    ok != 0 && on == 0
}

/// Whether the foreground window belongs to this process.
pub fn app_is_active() -> bool {
    use windows_sys::Win32::System::Threading::GetCurrentProcessId;
    use windows_sys::Win32::UI::WindowsAndMessaging::{GetForegroundWindow, GetWindowThreadProcessId};
    // SAFETY: plain queries; a null window yields pid 0.
    unsafe {
        let w = GetForegroundWindow();
        if w.is_null() {
            return false;
        }
        let mut pid = 0u32;
        GetWindowThreadProcessId(w, &mut pid);
        pid == GetCurrentProcessId()
    }
}

/// Cancels every frame navigation in `webview` that `may_load` refuses:
/// an iframe at any depth, and each of its redirects. wry hands the
/// shell only WebView2's top-level `NavigationStarting`, so this hooks
/// `FrameNavigationStarting` on the controller Tauri exposes. Called on
/// the main thread, Tauri runs `with_webview` in place, so the hook is
/// in before the message loop runs again. `may_load` runs on the main
/// thread (WebView2's UI thread) for each frame; an address WebView2
/// cannot hand over is refused.
pub fn guard_frames(webview: &tauri::Webview, may_load: Box<dyn Fn(&str) -> bool + Send>) {
    use webview2_com::{take_pwstr, NavigationStartingEventHandler};
    let label = webview.label().to_string();
    let hooked = webview.with_webview(move |w| {
        let handler = NavigationStartingEventHandler::create(Box::new(move |_, args| {
            let Some(args) = args else { return Ok(()) };
            let mut raw = windows::core::PWSTR::null();
            // SAFETY: the arguments of the event being raised; Uri hands
            // over a CoTaskMem string, which take_pwstr copies and frees.
            unsafe {
                let uri = args.Uri(&mut raw).map(|()| take_pwstr(raw));
                if !uri.is_ok_and(|u| may_load(&u)) {
                    args.SetCancel(true)?;
                }
            }
            Ok(())
        }));
        let mut token = 0i64;
        // SAFETY: the live controller of this webview, on the main thread.
        let added =
            unsafe { w.controller().CoreWebView2().and_then(|c| c.add_FrameNavigationStarting(&handler, &mut token)) };
        if let Err(e) = added {
            eprintln!("kivali: {label}: cannot guard its frames: {e}");
        }
    });
    if let Err(e) = hooked {
        eprintln!("kivali: cannot guard a team web view's frames: {e}");
    }
}

/// The Windows counterparts of the Unix and macOS tests: the lock, the
/// pipe (against a server this process makes, so the owner check sees
/// this user), the timed reads, the process facts and the second-launch
/// event; and the shell's log, in a child process started the way
/// Explorer starts a GUI program. What needs a desktop (the alerts,
/// Hello's prompt, the terminal) is not here, and neither is
/// [`attach_terminal`]: a child attached to this process's console would
/// write into the test run's own output.
#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::process::Stdio;

    #[test]
    fn one_instance_at_a_time() {
        let dir = tempfile::tempdir().unwrap();
        let first = InstanceLock::acquire(dir.path()).unwrap();
        assert!(first.is_some());
        assert!(InstanceLock::acquire(dir.path()).unwrap().is_none());
        drop(first);
        assert!(InstanceLock::acquire(dir.path()).unwrap().is_some());
    }

    /// `count` instances of a pipe this process serves at `endpoint`. A
    /// client takes one instance for good (the supervisor's listener makes
    /// a new one for each), so a test makes as many as it connects.
    fn serve(endpoint: &Path, count: usize) -> Vec<std::fs::File> {
        use std::os::windows::io::FromRawHandle;
        use windows_sys::Win32::Storage::FileSystem::PIPE_ACCESS_DUPLEX;
        use windows_sys::Win32::System::Pipes::{CreateNamedPipeW, PIPE_TYPE_BYTE, PIPE_UNLIMITED_INSTANCES};
        let name = wide(endpoint.as_os_str());
        (0..count)
            .map(|_| {
                // SAFETY: a NUL-terminated name and the default security
                // (this user's); the File owns the handle from here on.
                unsafe {
                    let h = CreateNamedPipeW(
                        name.as_ptr(),
                        PIPE_ACCESS_DUPLEX,
                        PIPE_TYPE_BYTE,
                        PIPE_UNLIMITED_INSTANCES,
                        4096,
                        4096,
                        0,
                        std::ptr::null(),
                    );
                    assert_ne!(h, INVALID_HANDLE_VALUE, "{}", std::io::Error::last_os_error());
                    std::fs::File::from_raw_handle(h)
                }
            })
            .collect()
    }

    #[test]
    fn listening_and_connecting() {
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        assert!(!is_listening(&ep));
        let e = connect(&ep).err().unwrap();
        assert_eq!(e.kind(), std::io::ErrorKind::NotFound, "{e:?}");
        assert_eq!(peer_pid(&ep), None);
        // is_listening and peer_pid each take an instance; connect a third.
        let _server = serve(&ep, 3);
        assert!(is_listening(&ep));
        assert_eq!(peer_pid(&ep), Some(std::process::id()));
        assert!(connect(&ep).is_ok());
        // Every instance busy: the pipe still counts as listening.
        assert!(is_listening(&ep));
    }

    /// The broker's probe: a pipe exists or not, and asking takes no
    /// instance (the one instance is still there for a client after).
    #[test]
    fn a_pipe_exists_without_a_connection() {
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        assert!(!pipe_exists(&ep));
        let _server = serve(&ep, 1);
        assert!(pipe_exists(&ep));
        assert!(pipe_exists(&ep));
        let _client = connect(&ep).unwrap();
        // Its one instance busy: it still exists.
        assert!(pipe_exists(&ep));
    }

    /// A server that accepts and never answers: the read gives up.
    #[test]
    fn a_silent_server_times_out() {
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        let _server = serve(&ep, 1);
        let mut s = connect_timeout(&ep, Duration::from_millis(20)).unwrap();
        let e = s.read(&mut [0u8; 8]).unwrap_err();
        assert_eq!(e.kind(), std::io::ErrorKind::TimedOut, "{e:?}");
    }

    /// Bytes both ways through the timed pipe, then the server closing:
    /// the read sees the end, which a close-delimited body relies on.
    #[test]
    fn timed_reads_get_data_then_the_end() {
        let dir = tempfile::tempdir().unwrap();
        let ep = endpoint(dir.path());
        let mut server = serve(&ep, 1).pop().unwrap();
        let mut c = connect_timeout(&ep, Duration::from_secs(5)).unwrap();
        c.write_all(b"ping").unwrap();
        let mut got = [0u8; 4];
        server.read_exact(&mut got).unwrap();
        assert_eq!(&got, b"ping");
        server.write_all(b"pong").unwrap();
        c.read_exact(&mut got).unwrap();
        assert_eq!(&got, b"pong");
        drop(server);
        assert_eq!(c.read(&mut got).unwrap(), 0);
    }

    /// A process that lives until its stdin closes (findstr reads to the
    /// end of its input).
    fn child(spawn: fn(&mut Command) -> std::io::Result<Child>) -> Child {
        let mut cmd = Command::new("findstr.exe");
        cmd.arg("x").stdin(Stdio::piped()).stdout(Stdio::null());
        spawn(&mut cmd).unwrap()
    }

    #[test]
    fn parent_start_and_exit() {
        let me = std::process::id();
        let mut c = child(Command::spawn);
        let pid = c.id();
        assert_eq!(parent_pid(pid), Some(me));
        let (mine, theirs) = (start_time(me).unwrap(), start_time(pid).unwrap());
        assert!(mine <= theirs);
        assert_eq!(start_time(me), Some(mine));
        assert!(!is_orphan(pid, me));
        assert!(!wait_exit(pid, &|| false, Some(Duration::ZERO)));
        // A pid that names another process now is treated as gone.
        assert!(wait_exit(pid, &|| true, None));
        drop(c.stdin.take());
        assert!(wait_exit(pid, &|| false, Some(Duration::from_secs(10))));
        // Exited, though the Child's handle still holds it: no start time,
        // and a process it was the parent of is an orphan.
        assert_eq!(start_time(pid), None);
        assert!(is_orphan(me, pid));
        c.wait().unwrap();
    }

    #[test]
    fn serve_spawns_detached_and_is_killed() {
        let mut c = child(spawn_serve);
        assert!(kill(c.id()));
        assert_eq!(c.wait().unwrap().code(), Some(1));
    }

    #[test]
    fn a_second_launch_wakes_the_first() {
        let dir = tempfile::tempdir().unwrap();
        assert!(!notify_running_instance(dir.path()));
        let (tx, rx) = std::sync::mpsc::channel();
        on_second_launch(
            dir.path(),
            Box::new(move || {
                let _ = tx.send(());
            }),
        );
        assert!(notify_running_instance(dir.path()));
        rx.recv_timeout(Duration::from_secs(5)).unwrap();
    }

    /// A console, or an open standard error, is seen; without either the
    /// shell logs to a file.
    #[test]
    fn output_is_seen_with_a_console_or_an_open_stderr() {
        use StdHandle::*;
        assert!(!output_seen(false, Unset)); // Explorer, the Start menu, the login item
        assert!(!output_seen(false, Stale));
        assert!(output_seen(false, Open)); // redirected or piped
        assert!(output_seen(true, Unset));
        assert!(output_seen(true, Open));
    }

    #[test]
    fn standard_handle_states() {
        assert_eq!(handle_state(std::ptr::null_mut()), StdHandle::Unset);
        assert_eq!(handle_state(INVALID_HANDLE_VALUE), StdHandle::Unset);
        let f = tempfile::tempfile().unwrap();
        assert_eq!(handle_state(f.as_raw_handle() as HANDLE), StdHandle::Open);
        // Far above any handle this process has open.
        assert_eq!(handle_state(std::ptr::without_provenance_mut(0x7fff_fff0)), StdHandle::Stale);
        // cargo gives the test binary a standard error (a console or a pipe).
        assert!(stderr_is_seen());
    }

    /// Where the child test below takes its log: set only for that child.
    const LOG_CHILD_DIR: &str = "KIVALI_TEST_LOG_CHILD_DIR";

    /// Runs this test binary's `test` alone, as Explorer starts a GUI
    /// program: no console (`DETACHED_PROCESS`) and no standard handles
    /// (no `STARTF_USESTDHANDLES`), with `var` set to `value`. Its exit
    /// code.
    fn run_test_detached(test: &str, var: &str, value: &OsStr) -> u32 {
        use windows_sys::Win32::System::Threading::{
            CreateProcessW, GetExitCodeProcess, CREATE_UNICODE_ENVIRONMENT, PROCESS_INFORMATION, STARTUPINFOW,
        };
        let exe = std::env::current_exe().unwrap();
        let line = format!("\"{}\" {test} --exact --ignored --no-capture --test-threads=1", exe.display());
        let mut line = wide(OsStr::new(&line));
        let mut env: Vec<u16> = Vec::new();
        for (k, v) in std::env::vars_os().filter(|(k, _)| k != var).chain([(var.into(), value.to_owned())]) {
            env.extend(k.encode_wide().chain([u16::from(b'=')]).chain(v.encode_wide()).chain([0]));
        }
        env.push(0);
        let si = STARTUPINFOW { cb: std::mem::size_of::<STARTUPINFOW>() as u32, ..Default::default() };
        let mut pi = PROCESS_INFORMATION::default();
        // SAFETY: a writable NUL-terminated command line, a UTF-16
        // environment block ending in two NULs, and the two handles
        // CreateProcessW returns, closed on drop from here on.
        unsafe {
            let ok = CreateProcessW(
                std::ptr::null(),
                line.as_mut_ptr(),
                std::ptr::null(),
                std::ptr::null(),
                0,
                DETACHED_PROCESS | CREATE_UNICODE_ENVIRONMENT,
                env.as_ptr().cast(),
                std::ptr::null(),
                &si,
                &mut pi,
            );
            assert_ne!(ok, 0, "{}", std::io::Error::last_os_error());
            let (process, _thread) = (Owned(pi.hProcess), Owned(pi.hThread));
            assert_eq!(WaitForSingleObject(process.0, 60_000), WAIT_OBJECT_0, "the child test did not finish");
            let mut code = 0;
            assert_ne!(GetExitCodeProcess(process.0, &mut code), 0);
            code
        }
    }

    /// The shell's log as a release build takes it: `log_child` runs in a
    /// process with no console and no standard handles, calls the shell's
    /// own `log_to_file`, then writes with `eprintln!` and `println!`.
    /// Their lines reach `logs/shell.log` only because the standard
    /// handles were replaced and std looks them up on every write. This
    /// process's own output is never touched.
    #[test]
    fn a_process_without_a_console_logs_to_shell_log() {
        let dir = tempfile::tempdir().unwrap();
        let code = run_test_detached("platform::windows::tests::log_child", LOG_CHILD_DIR, dir.path().as_os_str());
        let log = std::fs::read_to_string(dir.path().join("logs").join("shell.log")).unwrap_or_default();
        assert_eq!(code, 0, "log_child failed; shell.log:\n{log}");
        assert!(log.contains(&format!("(Kivali {})\n", crate::APP_VERSION)), "{log}");
        assert!(log.contains("kivali: eprintln reached the log\n"), "{log}");
        assert!(log.contains("println reached the log\n"), "{log}");
    }

    /// Run by the test above, in a process of its own; anywhere else
    /// (`cargo test -- --ignored`) it does nothing.
    #[test]
    #[ignore = "run by a_process_without_a_console_logs_to_shell_log, in a process of its own"]
    fn log_child() {
        let Some(dir) = std::env::var_os(LOG_CHILD_DIR) else { return };
        assert!(!stderr_is_seen(), "started with a console or a standard error");
        crate::log_to_file(Path::new(&dir));
        eprintln!("kivali: eprintln reached the log");
        println!("println reached the log");
        assert!(stderr_is_seen());
    }

    /// Off the main thread these answer; Hello's availability is a query
    /// that shows nothing (its prompt is not tested here).
    #[test]
    fn system_queries_answer_off_main() {
        use windows::Security::Credentials::UI::UserConsentVerifier;
        let _ = (menu_bar_dark(), menus_dark(), reduce_motion(), app_is_active());
        assert!(UserConsentVerifier::CheckAvailabilityAsync().and_then(|op| op.join()).is_ok());
    }

    /// `windows_ui` builds on every OS, so it spells the SDK's numbers
    /// itself; these are the SDK's.
    #[test]
    fn alert_and_key_numbers_are_the_sdks() {
        use super::super::windows_ui as ui;
        use windows_sys::Win32::UI::Input::KeyboardAndMouse as kb;
        use windows_sys::Win32::UI::WindowsAndMessaging::{IDCANCEL, IDCONTINUE, IDOK};
        assert_eq!((ui::ID_OK, ui::ID_CANCEL), (IDOK, IDCANCEL));
        const { assert!(ui::FIRST_BUTTON_ID > IDCONTINUE) };
        let vks = [ui::VK_ADD, ui::VK_SUBTRACT, ui::VK_OEM_PLUS, ui::VK_OEM_COMMA, ui::VK_OEM_MINUS];
        let sdk = [kb::VK_ADD, kb::VK_SUBTRACT, kb::VK_OEM_PLUS, kb::VK_OEM_COMMA, kb::VK_OEM_MINUS];
        assert_eq!(vks, sdk.map(u32::from));
        assert_eq!(ui::keys("Ctrl+F5"), Some((true, false, false, kb::VK_F5)));
        assert_eq!(ui::keys("Ctrl+Z"), Some((true, false, false, kb::VK_Z)));
    }
}
