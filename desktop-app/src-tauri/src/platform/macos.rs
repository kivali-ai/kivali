//! macOS: Application Support, LOCAL_PEERPID, proc_pidinfo, kqueue,
//! Terminal.app, coloured tray images, the application menu, NSAlert
//! sheets, LocalAuthentication, the appearance queries.

use super::{cancel_index, tray_set, AlertAnswer, AlertSpec, StartTime, TrayIcons};
use block2::RcBlock;
use objc2::rc::Retained;
use objc2::runtime::Bool;
use objc2::MainThreadMarker;
use objc2_app_kit::{
    NSAlert, NSAlertFirstButtonReturn, NSAppearance, NSAppearanceCustomization, NSAppearanceNameAqua,
    NSAppearanceNameDarkAqua, NSApplication, NSControlStateValueOn, NSModalResponse, NSRunningApplication, NSWindow,
    NSWorkspace,
};
use objc2_foundation::{ns_string, NSArray, NSError, NSString, NSUserDefaults};
use objc2_local_authentication::{LAContext, LAError, LAPolicy};
use std::cell::Cell;
use std::sync::Mutex;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

pub fn default_config_dir() -> PathBuf {
    // ~/Library/Application Support
    dirs::data_dir().unwrap_or_else(|| PathBuf::from(".")).join("Kivali")
}

/// The pid of the process listening on the socket (LOCAL_PEERPID).
pub fn peer_pid(endpoint: &Path) -> Option<u32> {
    use std::os::fd::AsRawFd;
    let s = std::os::unix::net::UnixStream::connect(endpoint).ok()?;
    let mut pid: libc::pid_t = 0;
    let mut len = std::mem::size_of::<libc::pid_t>() as libc::socklen_t;
    // SAFETY: getsockopt into a correctly sized pid_t on an open socket.
    let r = unsafe {
        libc::getsockopt(s.as_raw_fd(), libc::SOL_LOCAL, libc::LOCAL_PEERPID, &mut pid as *mut _ as *mut libc::c_void, &mut len)
    };
    (r == 0 && pid > 0).then_some(pid as u32)
}

/// proc_pidinfo PROC_PIDTBSDINFO for a live process.
fn bsdinfo(pid: u32) -> Option<libc::proc_bsdinfo> {
    // SAFETY: proc_pidinfo fills a zeroed proc_bsdinfo of the size given.
    unsafe {
        let mut info: libc::proc_bsdinfo = std::mem::zeroed();
        let size = std::mem::size_of::<libc::proc_bsdinfo>() as libc::c_int;
        let n = libc::proc_pidinfo(pid as libc::c_int, libc::PROC_PIDTBSDINFO, 0, &mut info as *mut _ as *mut libc::c_void, size);
        (n == size).then_some(info)
    }
}

pub fn parent_pid(pid: u32) -> Option<u32> {
    bsdinfo(pid).map(|i| i.pbi_ppid)
}

/// A process's start time; None when no such process exists.
pub fn start_time(pid: u32) -> Option<StartTime> {
    bsdinfo(pid).map(|i| StartTime(i.pbi_start_tvsec * 1_000_000 + i.pbi_start_tvusec))
}

/// Blocks until `pid` exits (kqueue EVFILT_PROC NOTE_EXIT), at most
/// `timeout`. True when it is gone: exited, never there, or `reused`
/// (checked after the registration, so a pid reused before it is
/// caught) says the pid now names some other process. False on timeout.
/// Unlike waitpid this returns once the exit has begun: the process's
/// sockets may still be open for a moment.
pub fn wait_exit(pid: u32, reused: &dyn Fn() -> bool, timeout: Option<Duration>) -> bool {
    // SAFETY: a private kqueue, one change and one event buffer.
    unsafe {
        let kq = libc::kqueue();
        if kq < 0 {
            return false;
        }
        let change = libc::kevent {
            ident: pid as libc::uintptr_t,
            filter: libc::EVFILT_PROC,
            flags: libc::EV_ADD | libc::EV_ONESHOT,
            fflags: libc::NOTE_EXIT,
            data: 0,
            udata: std::ptr::null_mut(),
        };
        // Registration fails (ESRCH) when the process is already gone.
        let gone = if libc::kevent(kq, &change, 1, std::ptr::null_mut(), 0, std::ptr::null()) != 0 || reused() {
            true
        } else {
            let ts = timeout.map(|t| libc::timespec { tv_sec: t.as_secs() as libc::time_t, tv_nsec: t.subsec_nanos() as libc::c_long });
            let tsp = ts.as_ref().map_or(std::ptr::null(), |t| t as *const libc::timespec);
            let mut out: libc::kevent = std::mem::zeroed();
            loop {
                let n = libc::kevent(kq, std::ptr::null(), 0, &mut out, 1, tsp);
                if n < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                break n > 0;
            }
        };
        libc::close(kq);
        gone
    }
}

/// Single-quotes for /bin/sh.
pub fn sh_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// The script Claude's sign-in hands to Terminal.app (Settings → AI,
/// and setup's "Open Claude sign-in").
pub const TERMINAL_SCRIPT: &str = "terminal.command";

/// `kivali-supervisor terminal`, which asks the running supervisor for
/// a terminal in the org's server container. The shell never sees the
/// bytes.
pub fn terminal_script(binary: &Path, dir: &Path) -> String {
    format!(
        "#!/bin/sh\n# Written by Kivali Desktop for Claude's sign-in.\nexec {} --config-dir {} terminal\n",
        sh_quote(&binary.to_string_lossy()),
        sh_quote(&dir.to_string_lossy())
    )
}

/// Writes `<dir>/terminal.command` (0700) and opens it in Terminal.app.
pub fn open_terminal(binary: &Path, dir: &Path) -> Result<(), String> {
    let script = dir.join(TERMINAL_SCRIPT);
    crate::paths::write_atomic(&script, terminal_script(binary, dir).as_bytes(), 0o700).map_err(|e| e.to_string())?;
    let status = Command::new("/usr/bin/open")
        .arg("-a")
        .arg("Terminal")
        .arg(&script)
        .status()
        .map_err(|e| format!("cannot open Terminal: {e}"))?;
    if !status.success() {
        return Err(format!("`open -a Terminal` failed ({status})"));
    }
    Ok(())
}

/// Coloured images at 40x44 (@2x of the design's 20x22 pt), light and
/// dark inks. The tray crate draws them 18 pt tall.
pub const TRAY_ICONS: TrayIcons = tray_set!("tray");
pub const TRAY_AREA: &str = "menu bar";
/// The application menu (Kivali, File, Edit, View, Team, Window, Help)
/// in the menu bar.
pub const APP_MENU: bool = true;
/// No menu inside the windows: the menu bar is the app's.
pub const WINDOW_MENU: bool = false;
/// A second launch shows a dialog; the Dock's reopen shows the window.
pub const SECOND_LAUNCH_FOCUSES: bool = false;
/// The updater installs and returns; the shell then pauses the teams and
/// restarts.
pub const INSTALL_EXITS: bool = false;

/// macOS needs no message to the running instance: a second launch is
/// refused with a dialog, and clicking the Dock icon reopens.
pub fn notify_running_instance(_dir: &Path) -> bool {
    false
}

pub fn on_second_launch(_dir: &Path, _show: Box<dyn Fn() + Send>) {}

/// The Dock icon was clicked with no window showing.
pub fn is_reopen(event: &tauri::RunEvent) -> bool {
    matches!(event, tauri::RunEvent::Reopen { .. })
}

/// The button index an NSAlert response names: the first button answers
/// `NSAlertFirstButtonReturn` and the rest follow; anything else (the
/// sheet ended some other way) answers Cancel.
fn button_of(response: NSModalResponse, buttons: &[String]) -> usize {
    let i = response - NSAlertFirstButtonReturn;
    let n = buttons.len().max(1) as isize;
    if (0..n).contains(&i) {
        i as usize
    } else {
        cancel_index(buttons)
    }
}

/// An NSAlert: a sheet on `parent` when it is showing, else app-modal.
/// Callable from any thread; built and shown on the main thread, and
/// `done` runs there once the alert is answered.
pub fn alert(app: &tauri::AppHandle, parent: Option<&tauri::Window>, spec: AlertSpec, done: Box<dyn FnOnce(AlertAnswer) + Send>) {
    let parent = parent.cloned();
    let run = move || {
        let Some(mtm) = MainThreadMarker::new() else { return };
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(&spec.title));
        alert.setInformativeText(&NSString::from_str(&spec.message));
        for (i, title) in spec.buttons.iter().enumerate() {
            let b = alert.addButtonWithTitle(&NSString::from_str(title));
            if spec.destructive == Some(i) {
                b.setHasDestructiveAction(true);
            }
            // NSAlert gives the first button Return and a "Cancel" Escape
            // itself; Escape is set again so it never depends on that.
            if i > 0 && title == "Cancel" {
                b.setKeyEquivalent(ns_string!("\u{1b}"));
            }
        }
        if let Some(label) = &spec.suppression {
            alert.setShowsSuppressionButton(true);
            if let Some(b) = alert.suppressionButton() {
                b.setTitle(&NSString::from_str(label));
            }
        }
        let wants_suppression = spec.suppression.is_some();
        let buttons = spec.buttons;
        let answer = move |alert: &NSAlert, response: NSModalResponse| AlertAnswer {
            button: button_of(response, &buttons),
            suppressed: wants_suppression && alert.suppressionButton().is_some_and(|b| b.state() == NSControlStateValueOn),
        };

        // SAFETY: tauri's NSWindow pointer, alive while `parent` is held
        // (and retained by AppKit while its sheet shows).
        let sheet_on = parent
            .as_ref()
            .and_then(|w| w.ns_window().ok())
            .map(|p| unsafe { &*(p as *const NSWindow) })
            .filter(|w| w.isVisible() && w.attachedSheet().is_none());
        // An alert often answers a click outside Kivali's windows (the
        // tray, the menu bar while another app is in front): bring Kivali,
        // and the window the sheet hangs from, to the front first, or the
        // alert opens behind whatever is there and the click seems lost.
        #[allow(deprecated)]
        NSApplication::sharedApplication(mtm).activateIgnoringOtherApps(true);
        if let Some(window) = sheet_on {
            window.makeKeyAndOrderFront(None);
        }
        match sheet_on {
            Some(window) => {
                // The block holds the alert and `done` until it runs once,
                // then lets both go (no alert -> block -> alert cycle).
                let held = Cell::new(Some((alert.clone(), done)));
                let block = RcBlock::new(move |response: NSModalResponse| {
                    if let Some((alert, done)) = held.take() {
                        done(answer(&alert, response));
                    }
                });
                alert.beginSheetModalForWindow_completionHandler(window, Some(&block));
            }
            None => {
                let response = alert.runModal();
                done(answer(&alert, response));
            }
        }
    };
    if let Err(e) = app.run_on_main_thread(run) {
        eprintln!("kivali: cannot show an alert: {e}");
    }
}

/// The device owner's Touch ID or login password
/// (LAPolicyDeviceOwnerAuthentication). macOS draws the prompt; `reason`
/// is the only text Kivali writes. `done` runs on a LocalAuthentication
/// queue: Ok, Err("cancelled") when the person (or the system) cancels,
/// else Err with the system's message. Callable from any thread.
pub fn authenticate(_app: &tauri::AppHandle, reason: &str, done: Box<dyn FnOnce(Result<(), String>) + Send>) {
    // SAFETY: a fresh context; LAContext is usable from any thread.
    let ctx: Retained<LAContext> = unsafe { LAContext::new() };
    // The reply may come on any queue: `done` and the context (kept
    // alive until then, or the evaluation is cancelled) sit in a Mutex.
    let held = Mutex::new(Some((ctx.clone(), done)));
    let reply = RcBlock::new(move |ok: Bool, err: *mut NSError| {
        let Some((_ctx, done)) = held.lock().ok().and_then(|mut h| h.take()) else { return };
        if ok.as_bool() {
            return done(Ok(()));
        }
        // SAFETY: LocalAuthentication passes a valid NSError or null.
        let err = unsafe { err.as_ref() };
        done(Err(match err {
            Some(e) if [LAError::UserCancel.0, LAError::SystemCancel.0, LAError::AppCancel.0].contains(&e.code()) => {
                "cancelled".into()
            }
            Some(e) => e.localizedDescription().to_string(),
            None => "authentication failed".into(),
        }));
    });
    // SAFETY: a valid policy, reason and reply block (copied by the call).
    unsafe {
        ctx.evaluatePolicy_localizedReason_reply(LAPolicy::DeviceOwnerAuthentication, &NSString::from_str(reason), &reply)
    };
}

fn is_dark(appearance: &NSAppearance) -> bool {
    // SAFETY: AppKit's constant appearance names.
    let (aqua, dark) = unsafe { (NSAppearanceNameAqua, NSAppearanceNameDarkAqua) };
    let names = NSArray::from_slice(&[aqua, dark]);
    appearance.bestMatchFromAppearancesWithNames(&names).is_some_and(|m| m.isEqualToString(dark))
}

/// The system appearance (`AppleInterfaceStyle` is "Dark" in dark mode,
/// absent in light). Safe on any thread.
fn system_dark() -> bool {
    NSUserDefaults::standardUserDefaults()
        .stringForKey(ns_string!("AppleInterfaceStyle"))
        .is_some_and(|s| s.to_string().eq_ignore_ascii_case("dark"))
}

/// Whether the menu bar is dark, so the tray needs the light ink. On the
/// main thread this asks the status bar's own window (it follows the
/// wallpaper as well as the setting); elsewhere, or when there is no
/// status item yet, it answers the system appearance. Safe on any
/// thread; call it on the main thread (where the tray is set anyway)
/// for the precise answer.
pub fn menu_bar_dark() -> bool {
    if let Some(mtm) = MainThreadMarker::new() {
        let app = NSApplication::sharedApplication(mtm);
        for w in app.windows() {
            if w.class().name().to_bytes() == b"NSStatusBarWindow" {
                if let Some(view) = w.contentView() {
                    return is_dark(&view.effectiveAppearance());
                }
            }
        }
    }
    system_dark()
}

/// Whether native menus draw dark: they follow the system appearance.
/// Safe on any thread.
pub fn menus_dark() -> bool {
    system_dark()
}

/// Hands the app menu's Window and Help menus to AppKit, which lists the
/// open windows (one per team) in the first and a search field in the
/// second.
pub fn mark_app_menus(window: &tauri::menu::Submenu<tauri::Wry>, help: &tauri::menu::Submenu<tauri::Wry>) {
    let _ = window.set_as_windows_menu_for_nsapp();
    let _ = help.set_as_help_menu_for_nsapp();
}

/// System Settings → Accessibility → Display → Reduce motion. Safe on
/// any thread.
pub fn reduce_motion() -> bool {
    NSWorkspace::sharedWorkspace().accessibilityDisplayShouldReduceMotion()
}

/// Whether Kivali is the active (frontmost) app. Safe on any thread.
pub fn app_is_active() -> bool {
    NSRunningApplication::currentApplication().isActive()
}

/// Nothing to add: WKWebView asks wry about every navigation, a frame's
/// as well as the page's, so the team web view's `on_navigation`
/// already decides its frames.
pub fn guard_frames(_webview: &tauri::Webview, _may_load: Box<dyn Fn(&str) -> bool + Send>) {}

/// Nothing to hand over: no window has a menu of its own
/// ([`WINDOW_MENU`]), and AppKit gives the menu bar its key equivalents
/// before a webview sees them.
pub fn menu_shortcuts(_webview: &tauri::Webview, _shortcut: Box<dyn Fn(&str) -> bool + Send>) {}

/// Unused: the Edit menu's items are AppKit's own.
pub fn press_keys(_chord: &str) {}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::Stdio;

    #[test]
    fn alert_responses_name_buttons() {
        let b: Vec<String> = ["Keep Plainsong", "Cancel", "Continue"].iter().map(|s| s.to_string()).collect();
        assert_eq!(button_of(1000, &b), 0);
        assert_eq!(button_of(1002, &b), 2);
        assert_eq!(button_of(1003, &b), 1); // out of range: Cancel
        assert_eq!(button_of(objc2_app_kit::NSModalResponseAbort, &b), 1);
        assert_eq!(button_of(1000, &[]), 0); // the default OK
    }

    /// Off the main thread these answer without AppKit's main-thread
    /// objects (cargo runs tests on worker threads).
    #[test]
    fn appearance_queries_answer_off_main() {
        assert!(MainThreadMarker::new().is_none());
        assert_eq!(menu_bar_dark(), system_dark());
        let _ = (reduce_motion(), app_is_active(), menus_dark());
    }

    #[test]
    fn quoting() {
        assert_eq!(sh_quote("/Applications/Kivali.app"), "'/Applications/Kivali.app'");
        assert_eq!(sh_quote("it's"), "'it'\\''s'");
    }

    #[test]
    fn script_runs_terminal_verb() {
        let s = terminal_script(
            Path::new("/Applications/Kivali.app/Contents/MacOS/kivali-supervisor"),
            Path::new("/Users/me/Library/Application Support/Kivali"),
        );
        assert!(s.starts_with("#!/bin/sh\n"));
        assert!(s.ends_with(
            "exec '/Applications/Kivali.app/Contents/MacOS/kivali-supervisor' --config-dir '/Users/me/Library/Application Support/Kivali' terminal\n"
        ));
    }

    #[test]
    fn peer_parent_and_start() {
        let dir = tempfile::tempdir().unwrap();
        let sock = dir.path().join("s.sock");
        let _l = std::os::unix::net::UnixListener::bind(&sock).unwrap();
        let me = std::process::id();
        assert_eq!(peer_pid(&sock), Some(me));
        assert_eq!(parent_pid(me), Some(unsafe { libc::getppid() } as u32));
        assert_eq!(peer_pid(&dir.path().join("none.sock")), None);
        assert_eq!(start_time(me), start_time(me));
        assert!(start_time(me).is_some());
    }

    #[test]
    fn wait_exit_returns_for_an_exited_process() {
        let _spawning = crate::paths::spawning();
        let mut c = Command::new("/bin/cat").stdin(Stdio::piped()).spawn().unwrap();
        drop(c.stdin.take()); // cat sees EOF and exits
        assert!(wait_exit(c.id(), &|| false, None));
        c.wait().unwrap();
    }

    #[test]
    fn wait_exit_times_out_on_a_live_process() {
        let _spawning = crate::paths::spawning();
        let mut c = Command::new("/bin/cat").stdin(Stdio::piped()).spawn().unwrap();
        assert!(!wait_exit(c.id(), &|| false, Some(Duration::ZERO)));
        // A pid that names another process now is treated as gone.
        assert!(wait_exit(c.id(), &|| true, None));
        drop(c.stdin.take());
        c.wait().unwrap();
    }
}
