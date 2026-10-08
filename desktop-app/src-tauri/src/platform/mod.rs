//! Everything that differs between operating systems, behind one
//! signature each (docs/developers/desktop-app.md, "Platform"). The rest of the shell never names an OS, a socket type
//! or a system call: it calls these.
//!
//! | Concern | here | macOS (`macos.rs`, `unix.rs`) | Windows (`windows.rs`) |
//! | --- | --- | --- | --- |
//! | config directory | [`config_dir`] | Application Support | `%LOCALAPPDATA%` |
//! | instance lock | [`InstanceLock`] | flock on `shell.lock` | exclusive open of `shell.lock` |
//! | RPC transport | [`endpoint`], [`is_listening`], [`connect`], [`connect_timeout`] | Unix socket | named pipe ([`pipe_name`]) |
//! | VM image, broker | [`VM_ROOT_FILE`], [`broker_listening`] | `root.squashfs`; no broker | `root.vhdx`; whether `\\.\pipe\kivali-broker` exists |
//! | who owns serve | [`peer_pid`], [`parent_pid`], [`start_time`], [`is_orphan`] | `LOCAL_PEERPID`, `proc_pidinfo`, re-parented to launchd | `GetNamedPipeServerProcessId`, `GetProcessTimes`, parent gone |
//! | starting serve | [`spawn_serve`] | plain spawn | `CREATE_NO_WINDOW`, detached |
//! | stopping serve | [`request_stop`], [`kill`] | SIGTERM, SIGKILL | nothing (the RPC is the request), `TerminateProcess` |
//! | waiting for exit | [`wait_exit`] | kqueue | `WaitForSingleObject` |
//! | terminal | [`open_terminal`] | `open -a Terminal` on a `.command` script | Windows Terminal, else `cmd.exe /c start` |
//! | tray icons | [`TRAY_ICONS`] | 40x44 coloured, light and dark inks | 32x32, the same drawings |
//! | appearance | [`menu_bar_dark`], [`menus_dark`], [`reduce_motion`], [`app_is_active`] | NSAppearance, NSWorkspace, NSRunningApplication | the registry, `SPI_GETCLIENTAREAANIMATION`, the foreground window |
//! | alerts | [`alert`] | NSAlert, a sheet on the parent | TaskDialog, modal to the parent: any number of buttons, a verification checkbox |
//! | owner check | [`authenticate`] | LocalAuthentication (Touch ID or password) | Windows Hello, else a confirmation |
//! | notifications | [`notify`], [`on_notification_click`] | UNUserNotificationCenter, clicks routed (the plugin outside an app bundle) | the notification plugin, clicks only bring Kivali forward |
//! | team window frames | [`guard_frames`] | nothing: WKWebView asks wry about frames too | WebView2's `FrameNavigationStarting`, a refused frame cancelled |
//! | app menu | [`APP_MENU`] | the application menu | none |
//! | window menu | [`WINDOW_MENU`], [`menu_shortcuts`], [`press_keys`] | none (the application menu) | a menu bar in each team window; WebView2's `AcceleratorKeyPressed` hands it the webview's keys |
//! | wording | [`TRAY_AREA`] | menu bar | notification area |
//! | second launch | [`SECOND_LAUNCH_FOCUSES`], [`notify_running_instance`], [`on_second_launch`], [`is_reopen`] | a dialog; Dock reopen | the running instance is focused |
//! | updater | [`INSTALL_EXITS`] | stop, install, restart | stop in the updater's before-exit hook |
//! | the shell's log | [`stderr_is_seen`], [`redirect_output`], [`attach_terminal`] | stderr not a terminal: `dup2` onto fd 2 | no console and no open stderr: `SetStdHandle` (stdout and stderr); a flag run from a console attaches to it |
//!
//! The Windows pipe stream is a synchronous handle: one blocking call
//! at a time on it, and no read timeouts of its own ([`connect_timeout`]
//! bounds reads with a `PeekNamedPipe` loop). A relay over an upgraded
//! stream (the terminal or exec, `Supervisor::open_stream`) that reads
//! on one thread while writing on another must therefore open the pipe
//! for overlapped I/O, or use two handles, when that is wired; a Unix
//! socket can simply be cloned.
//!
//! Linux shares `unix.rs` and has its own process facts (`linux.rs`);
//! it is not a shipped platform.

#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "macos")]
mod macos;
mod notify;
#[cfg(target_os = "macos")]
mod notify_macos;
#[cfg(any(target_os = "linux", test))]
mod plugin_dialog;
#[cfg(unix)]
mod unix;
#[cfg(windows)]
mod windows;
#[cfg(any(windows, test))]
mod windows_terminal;
#[cfg(any(windows, test))]
pub(crate) mod windows_ui;

// `os` is the one OS this build is for; `shared` is what it shares with
// its family (Unix for macOS and Linux).
#[cfg(target_os = "linux")]
use linux as os;
#[cfg(target_os = "macos")]
use macos as os;
#[cfg(unix)]
use unix as shared;
#[cfg(windows)]
use windows as os;
#[cfg(windows)]
use windows as shared;

pub use os::{
    alert, app_is_active, authenticate, guard_frames, is_reopen, mark_app_menus, menu_bar_dark, menu_shortcuts,
    menus_dark, notify_running_instance, on_second_launch, open_terminal, parent_pid, peer_pid, press_keys,
    reduce_motion, start_time, wait_exit, APP_MENU, INSTALL_EXITS, SECOND_LAUNCH_FOCUSES, TRAY_AREA, TRAY_ICONS,
    WINDOW_MENU,
};
pub use notify::{notify, on_notification_click, NotifyTarget};
pub use shared::{
    attach_terminal, broker_listening, connect, connect_timeout, endpoint, file_mode, is_listening, is_orphan, kill,
    owner_only_dir, redirect_output, request_stop, spawn_serve, stderr_is_seen, InstanceLock, VM_ROOT_FILE,
};

use std::io::{Read, Write};
use std::path::{Path, PathBuf};

/// A connection to the supervisor's RPC endpoint.
pub trait Stream: Read + Write + Send {}
impl<T: Read + Write + Send + ?Sized> Stream for T {}

/// When a process started, which with its pid names one process for
/// good: microseconds since the Unix epoch on Unix, the creation
/// FILETIME (100 ns ticks since 1601) on Windows. Compared, never read.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct StartTime(pub u64);

/// How many frames the starting pulse has, and how long each shows: one
/// 45% -> 100% -> 45% cycle in 1.6 s (scripts/make-icons.mjs draws them).
pub const TRAY_FRAMES: usize = 10;
pub const TRAY_FRAME_MS: u64 = 160;

/// One ink's tray pictures (PNG), one per state.
pub struct TrayImages {
    /// Solid bars and dots.
    pub running: &'static [u8],
    /// Outlined, at 45%.
    pub paused: &'static [u8],
    /// Solid bars, red dots.
    pub couldnt_start: &'static [u8],
    /// The paused drawing held at 50%: starting under Reduce motion.
    pub starting_held: &'static [u8],
    /// The starting pulse, [`TRAY_FRAMES`] frames of [`TRAY_FRAME_MS`].
    pub starting: [&'static [u8]; TRAY_FRAMES],
}

/// The tray pictures in the ink for a light menu bar (`#1D1D1F`) and a
/// dark one (`#F5F5F5`). Coloured images, never templates, so the red
/// survives: the shell picks the ink ([`menu_bar_dark`]).
pub struct TrayIcons {
    pub light: TrayImages,
    pub dark: TrayImages,
}

/// The tray set whose files are `icons/<prefix>-<state>-<light|dark>.png`.
macro_rules! tray_set {
    ($prefix:literal) => {
        $crate::platform::TrayIcons {
            light: tray_set!(@ink $prefix, "light"),
            dark: tray_set!(@ink $prefix, "dark"),
        }
    };
    (@ink $prefix:literal, $ink:literal) => {
        $crate::platform::TrayImages {
            running: include_bytes!(concat!("../../icons/", $prefix, "-running-", $ink, ".png")),
            paused: include_bytes!(concat!("../../icons/", $prefix, "-paused-", $ink, ".png")),
            couldnt_start: include_bytes!(concat!("../../icons/", $prefix, "-couldnt-start-", $ink, ".png")),
            starting_held: include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, ".png")),
            starting: [
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-0.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-1.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-2.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-3.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-4.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-5.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-6.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-7.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-8.png")),
                include_bytes!(concat!("../../icons/", $prefix, "-starting-", $ink, "-9.png")),
            ],
        }
    };
}
use tray_set;

/// A native alert. `buttons` are in order: the first is
/// the default (Return), one titled exactly "Cancel" answers Escape.
/// An empty list shows a single OK.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AlertSpec {
    pub title: String,
    pub message: String,
    pub buttons: Vec<String>,
    /// The button drawn as destructive (macOS: `hasDestructiveAction`;
    /// Windows has no such button, so the alert shows the warning icon).
    pub destructive: Option<usize>,
    /// A checkbox under the message ("Don't ask again"): macOS's
    /// suppression button, Windows' verification checkbox.
    pub suppression: Option<String>,
}

/// Which button closed an alert (an index into [`AlertSpec::buttons`];
/// closing it some other way answers the Cancel button, else the last),
/// and whether the suppression checkbox was ticked.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AlertAnswer {
    pub button: usize,
    pub suppressed: bool,
}

/// The button an alert's dismissal without a button means: "Cancel"
/// when there is one, else the last.
pub(crate) fn cancel_index(buttons: &[String]) -> usize {
    buttons.iter().position(|b| b == "Cancel").unwrap_or(buttons.len().saturating_sub(1))
}

/// `p` without the `\\?\` prefix Windows puts on a canonical local
/// path (Tauri's resource directory arrives that way), which the
/// supervisor and the broker take as another directory. Only a local
/// drive's prefix goes; a UNC or device path, and every path elsewhere,
/// is returned as it is.
pub fn plain_path(p: PathBuf) -> PathBuf {
    if !cfg!(windows) {
        return p;
    }
    let s = p.to_string_lossy();
    match s.strip_prefix(r"\\?\") {
        Some(rest) if rest.len() >= 3 && rest.as_bytes()[1] == b':' && rest.as_bytes()[2] == b'\\' => PathBuf::from(rest),
        _ => p,
    }
}

/// The directory the shell and the supervisor share. `KIVALI_CONFIG_DIR`
/// overrides it everywhere (the supervisor reads the same variable);
/// otherwise `~/Library/Application Support/Kivali` on macOS,
/// `%LOCALAPPDATA%\Kivali` on Windows (local, never roaming: the data
/// disk is tens of gigabytes) and `$XDG_DATA_HOME/kivali` on Linux. It is
/// deliberately not Tauri's bundle-id directory, because the
/// command-line supervisor uses it without the shell, and the shell
/// always passes it to the supervisor explicitly (`--config-dir`).
pub fn config_dir() -> PathBuf {
    config_dir_from(std::env::var_os("KIVALI_CONFIG_DIR"))
}

fn config_dir_from(over: Option<std::ffi::OsString>) -> PathBuf {
    match over.filter(|d| !d.is_empty()) {
        Some(dir) => PathBuf::from(dir),
        None => os::default_config_dir(),
    }
}

/// The one signature each OS module must offer. Compiled on every
/// target, so an OS module that drifts fails its own build here rather
/// than in the common code. The signatures are spelled out in full on
/// purpose, so clippy's type_complexity does not apply.
#[allow(clippy::type_complexity)]
const _: () = {
    let _: fn() -> PathBuf = os::default_config_dir;
    let _: fn(&Path) -> std::io::Result<Option<InstanceLock>> = InstanceLock::acquire;
    let _: fn(&Path) -> PathBuf = endpoint;
    let _: fn(&Path) -> bool = is_listening;
    let _: fn() -> Option<bool> = broker_listening;
    let _: &str = VM_ROOT_FILE;
    let _: fn(&Path) -> std::io::Result<Box<dyn Stream>> = connect;
    let _: fn(&Path, std::time::Duration) -> std::io::Result<Box<dyn Stream>> = connect_timeout;
    let _: fn(&Path) -> Option<u32> = peer_pid;
    let _: fn(u32) -> Option<u32> = parent_pid;
    let _: fn(u32) -> Option<StartTime> = start_time;
    let _: fn(u32, u32) -> bool = is_orphan;
    let _: fn(&mut std::process::Command) -> std::io::Result<std::process::Child> = spawn_serve;
    let _: fn(u32) -> bool = request_stop;
    let _: fn(u32) -> bool = kill;
    let _: fn(u32, &dyn Fn() -> bool, Option<std::time::Duration>) -> bool = wait_exit;
    let _: fn(&Path, &Path) -> Result<(), String> = open_terminal;
    let _: fn(&mut std::fs::DirBuilder) = owner_only_dir;
    let _: fn(&mut std::fs::OpenOptions, u32) = file_mode;
    let _: fn() -> bool = stderr_is_seen;
    let _: fn(std::fs::File) -> std::io::Result<()> = redirect_output;
    let _: fn() = attach_terminal;
    let _: fn(&Path) -> bool = notify_running_instance;
    let _: fn(&Path, Box<dyn Fn() + Send>) = on_second_launch;
    let _: fn(&tauri::RunEvent) -> bool = is_reopen;
    let _: &TrayIcons = &TRAY_ICONS;
    let _: fn(&tauri::AppHandle, Option<&tauri::Window>, AlertSpec, Box<dyn FnOnce(AlertAnswer) + Send>) = alert;
    let _: fn(&tauri::AppHandle, &str, Box<dyn FnOnce(Result<(), String>) + Send>) = authenticate;
    let _: fn(&tauri::AppHandle, &str, &str, NotifyTarget) = notify;
    let _: fn(&tauri::AppHandle, Box<dyn Fn(NotifyTarget) + Send + Sync>) = on_notification_click;
    let _: fn() -> bool = menu_bar_dark;
    let _: fn() -> bool = menus_dark;
    let _: fn(&tauri::menu::Submenu<tauri::Wry>, &tauri::menu::Submenu<tauri::Wry>) = mark_app_menus;
    let _: fn() -> bool = reduce_motion;
    let _: fn() -> bool = app_is_active;
    let _: fn(&tauri::Webview, Box<dyn Fn(&str) -> bool + Send>) = guard_frames;
    let _: fn(&tauri::Webview, Box<dyn Fn(&str) -> bool + Send>) = menu_shortcuts;
    let _: fn(&str) = press_keys;
    let _: bool = APP_MENU || WINDOW_MENU || SECOND_LAUNCH_FOCUSES || INSTALL_EXITS;
    let _: &str = TRAY_AREA;
};

/// The named pipe the supervisor serves for config directory `dir` on
/// Windows: `\\.\pipe\kivali-<h>`, where `<h>` is the first 8 lower-case
/// hex characters of the SHA-256 of the directory's absolute path,
/// cleaned and lower-cased, as UTF-8. This is the supervisor's rule
/// (`internal/supervisor/host/host_windows.go`, `Endpoint`):
///
/// ```go
/// abs, _ := filepath.Abs(dir)
/// sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
/// name := `\\.\pipe\kivali-` + hex.EncodeToString(sum[:])[:8]
/// ```
///
/// Shared vector: `C:\Users\Maya\AppData\Local\Kivali` is
/// `\\.\pipe\kivali-96e3ef74`.
///
/// Both sides make the path absolute with `GetFullPathNameW` (Rust's
/// `std::path::absolute`, Go's `filepath.Abs`), which also resolves `.`
/// and `..` and turns `/` into `\`; [`pipe_key`] does the rest of
/// `filepath.Clean` and `strings.ToLower`. The shell passes the same
/// `--config-dir` string the supervisor hashes, so the two only have to
/// agree on spelling, not on what the file system thinks.
pub fn pipe_name(dir: &Path) -> String {
    let abs = std::path::absolute(dir).unwrap_or_else(|_| dir.to_path_buf());
    pipe_name_of_absolute(&abs.to_string_lossy())
}

/// [`pipe_name`] for a path already absolute. Pure, so the rule is
/// tested on every OS with Windows spellings.
fn pipe_name_of_absolute(abs: &str) -> String {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(pipe_key(abs).as_bytes());
    let hex: String = digest.iter().map(|b| format!("{b:02x}")).collect();
    format!(r"\\.\pipe\kivali-{}", &hex[..8])
}

/// `strings.ToLower(filepath.Clean(abs))` for an absolute path that
/// `GetFullPathNameW` produced: runs of separators collapse to one (a UNC
/// path keeps its leading two), a trailing separator goes unless it ends
/// the root (`C:\`, `/`), and each character is lower-cased on its own,
/// as Go's `unicode.ToLower` does (so `Σ` is always `σ`, and `İ` is `i`).
fn pipe_key(abs: &str) -> String {
    let is_sep = |c: char| c == '/' || c == '\\';
    let mut out = String::with_capacity(abs.len());
    let mut prev_sep = false;
    for (i, c) in abs.chars().enumerate() {
        let sep = is_sep(c);
        if !(sep && prev_sep && i > 1) {
            out.push(c);
        }
        prev_sep = sep;
    }
    while out.chars().count() > 1 && out.ends_with(is_sep) && !out[..out.len() - 1].ends_with(':') {
        out.pop();
    }
    out.chars()
        .flat_map(|c| if c == '\u{130}' { 'i'.to_lowercase() } else { c.to_lowercase() })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn plain_path_strips_only_a_local_drive_prefix() {
        let plain = |s: &str| plain_path(PathBuf::from(s)).to_string_lossy().into_owned();
        if cfg!(windows) {
            assert_eq!(plain(r"\\?\C:\Program Files\Kivali"), r"C:\Program Files\Kivali");
            assert_eq!(plain(r"\\?\UNC\server\share"), r"\\?\UNC\server\share");
            assert_eq!(plain(r"\\?\C:"), r"\\?\C:");
        }
        assert_eq!(plain(r"C:\Program Files\Kivali"), r"C:\Program Files\Kivali");
        assert_eq!(plain("/Applications/Kivali.app"), "/Applications/Kivali.app");
    }

    #[test]
    fn config_dir_override() {
        assert_eq!(config_dir_from(Some("/x/kivali-dev".into())), PathBuf::from("/x/kivali-dev"));
        assert_eq!(config_dir_from(Some("".into())), os::default_config_dir());
        assert_eq!(config_dir_from(None), os::default_config_dir());
    }

    #[test]
    fn default_config_dir_per_os() {
        let d = os::default_config_dir();
        let name = d.file_name().unwrap().to_string_lossy().into_owned();
        if cfg!(target_os = "macos") {
            assert!(d.ends_with("Library/Application Support/Kivali"), "{}", d.display());
        } else if cfg!(windows) {
            assert_eq!(name, "Kivali");
            assert_eq!(d.parent(), dirs::data_local_dir().as_deref());
        } else {
            assert_eq!(name, "kivali");
        }
    }

    #[test]
    fn pipe_names_follow_the_shared_rule() {
        // The vector the supervisor's tests share:
        // sha256(r"c:\users\maya\appdata\local\kivali") = 96e3ef747ec8bfd4…
        let v = r"\\.\pipe\kivali-96e3ef74";
        assert_eq!(pipe_name_of_absolute(r"C:\Users\Maya\AppData\Local\Kivali"), v);
        if cfg!(windows) {
            assert_eq!(pipe_name(Path::new(r"C:\Users\Maya\AppData\Local\Kivali")), v);
            assert_eq!(pipe_name(Path::new(r"c:/users/maya/appdata/local/kivali/")), v);
        }
        // sha256("/users/me/library/application support/kivali")
        //   = 568f6dfd…, as Go's sha256.Sum256 and shasum give it.
        let a = pipe_name(Path::new("/Users/me/Library/Application Support/Kivali"));
        if !cfg!(windows) {
            assert_eq!(a, r"\\.\pipe\kivali-568f6dfd");
        }
        // A trailing or doubled separator, or another case, names the
        // same directory.
        assert_eq!(pipe_name(Path::new("/Users/me/Library/Application Support/Kivali/")), a);
        assert_eq!(pipe_name(Path::new("/Users//me/library/APPLICATION SUPPORT/Kivali")), a);
        assert_ne!(pipe_name(Path::new("/Users/me/Library/Application Support/Kivali-dev")), a);
        assert_eq!(a.len(), r"\\.\pipe\kivali-".len() + 8);
    }

    /// The Windows spellings, as `GetFullPathNameW` hands them over.
    #[test]
    fn pipe_keys_clean_and_fold_like_go() {
        assert_eq!(pipe_key(r"C:\Users\Me\AppData\Local\Kivali"), r"c:\users\me\appdata\local\kivali");
        assert_eq!(pipe_key(r"C:\Users\Me\AppData\Local\Kivali\"), r"c:\users\me\appdata\local\kivali");
        assert_eq!(pipe_key(r"C:\Users\\Me\\\Kivali"), r"c:\users\me\kivali");
        assert_eq!(pipe_key(r"C:\"), r"c:\");
        assert_eq!(pipe_key("/"), "/");
        assert_eq!(pipe_key(r"\\Server\Share\Kivali"), r"\\server\share\kivali");
        // Per-character folding, as Go's unicode.ToLower.
        assert_eq!(pipe_key(r"C:\ΟΔΟΣ\İ"), r"c:\οδοσ\i");
        // sha256(r"c:\users\me\appdata\local\kivali") = daac4236…
        assert_eq!(pipe_name_of_absolute(r"C:\Users\Me\AppData\Local\Kivali\"), r"\\.\pipe\kivali-daac4236");
    }

    #[test]
    fn endpoint_lives_with_the_config_dir() {
        let e = endpoint(Path::new("/c"));
        if cfg!(windows) {
            assert_eq!(e, PathBuf::from(pipe_name(Path::new("/c"))));
        } else {
            assert_eq!(e, Path::new("/c/supervisor.sock"));
        }
    }

    /// The most opaque pixel's colour and alpha.
    fn strongest(img: &tauri::image::Image<'_>) -> [u8; 4] {
        let p = img.rgba().chunks(4).max_by_key(|p| p[3]).unwrap();
        [p[0], p[1], p[2], p[3]]
    }

    fn has_colour(img: &tauri::image::Image<'_>, rgb: [u8; 3]) -> bool {
        img.rgba().chunks(4).any(|p| p[3] == 255 && p[..3] == rgb)
    }

    /// Both sets are committed at their sizes (menu bar 40x44, the
    /// notification area 32x32), in the design's two inks; couldn't start
    /// carries the danger red; the starting frames pulse 45% -> 100% ->
    /// 45%; the held frame is at 50%.
    #[test]
    fn tray_icon_sets_decode() {
        let sets = [(tray_set!("tray"), (40, 44)), (tray_set!("tray-win"), (32, 32))];
        for (set, size) in &sets {
            for (images, ink, red) in
                [(&set.light, [0x1D, 0x1D, 0x1F], [0xB3, 0x26, 0x1E]), (&set.dark, [0xF5, 0xF5, 0xF5], [0xF2, 0x87, 0x7D])]
            {
                let decode = |b: &[u8]| tauri::image::Image::from_bytes(b).unwrap().to_owned();
                let running = decode(images.running);
                assert_eq!((running.width(), running.height()), *size);
                assert_eq!(strongest(&running), [ink[0], ink[1], ink[2], 255]);
                assert!(!has_colour(&running, red));

                let broken = decode(images.couldnt_start);
                assert!(has_colour(&broken, ink) && has_colour(&broken, red));

                let alpha = |b: &[u8]| strongest(&decode(b))[3];
                let paused = decode(images.paused);
                assert_eq!((paused.width(), paused.height()), *size);
                // Un-premultiplied at 45%: within one step of the ink.
                assert!(strongest(&paused)[..3].iter().zip(ink).all(|(a, b)| a.abs_diff(b) <= 1), "{:?}", strongest(&paused));
                assert!(alpha(images.paused).abs_diff(115) <= 1, "{}", alpha(images.paused)); // 45%
                assert!(alpha(images.starting_held).abs_diff(128) <= 1); // 50%

                let pulse: Vec<u8> = images.starting.iter().map(|b| alpha(b)).collect();
                assert_eq!(pulse[0], alpha(images.paused));
                assert_eq!(pulse[TRAY_FRAMES / 2], 255);
                assert!(pulse[..=TRAY_FRAMES / 2].windows(2).all(|w| w[0] < w[1]), "{pulse:?}");
                assert!(pulse[TRAY_FRAMES / 2..].windows(2).all(|w| w[0] > w[1]), "{pulse:?}");
            }
        }
        assert_eq!(TRAY_FRAMES as u64 * TRAY_FRAME_MS, 1600);
        // This platform's set is one of the two.
        assert!(sets.iter().any(|(s, _)| s.light.running == TRAY_ICONS.light.running));
    }

    #[test]
    fn cancel_answers_dismissal() {
        let b = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert_eq!(cancel_index(&b(&["Keep Plainsong", "Cancel", "Continue"])), 1);
        assert_eq!(cancel_index(&b(&["Quit", "Don't Quit"])), 1);
        assert_eq!(cancel_index(&[]), 0);
    }
}
