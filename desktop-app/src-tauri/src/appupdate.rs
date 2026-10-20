//! The app's own updates, through tauri-plugin-updater: the check, the
//! download and the install (E4, "Launch and quit" in
//! docs/developers/desktop-app.md).
//!
//! States (`AppUpdateView::state`, types.ts `AppUpdate`): `not_checked`,
//! `checking`, `up_to_date`, `available`, `downloading` (with the bytes
//! so far), `installing`; `failed` when a check could not finish,
//! `update_failed` when an update could not download or install (the
//! update stays on offer).
//!
//! The teams keep working while the update downloads; Kivali enters the
//! quitting state only once the download is done and the install starts.
//! Quit during the download stops it.

use crate::shell::{self, AppUpdateView, Shell};
use crate::{menus, platform, windows};
use std::path::Path;
use std::sync::atomic::Ordering;
use std::sync::Mutex;
use std::time::Duration;
use tauri::{AppHandle, Manager};
use tauri_plugin_updater::{Update, UpdaterBuilder, UpdaterExt};
use time::OffsetDateTime;

/// How long a request may go without receiving a byte before it is given
/// up (reqwest's read timeout, which starts again after every read). Not a
/// limit on the whole download: a slow connection keeps delivering and
/// finishes however long the ~850 MB Windows installer takes. A minute of
/// silence is a stall (Wi-Fi dropped, the computer slept, a proxy holding
/// the connection), long enough to ride out a brief outage that TCP
/// recovers from on its own, short enough not to leave the bar frozen.
pub const STALL_TIMEOUT: Duration = Duration::from_secs(60);
/// How long connecting to the release host may take.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(30);

/// The download under way, so Quit can stop it.
static DOWNLOAD: Mutex<Option<tauri::async_runtime::JoinHandle<()>>> = Mutex::new(None);

/// A check's answer: the newer version, none, or why there is no answer.
fn checked(found: Result<Option<String>, String>, checked_at: String) -> AppUpdateView {
    let checked_at = Some(checked_at);
    match found {
        Ok(Some(version)) => AppUpdateView { state: "available", version: Some(version), checked_at, ..Default::default() },
        Ok(None) => AppUpdateView { state: "up_to_date", checked_at, ..Default::default() },
        Err(e) => AppUpdateView { state: "failed", checked_at, error: Some(e), ..Default::default() },
    }
}

/// An update that did not install: still on offer, with why.
fn update_failed(before: &AppUpdateView, error: String) -> AppUpdateView {
    AppUpdateView { state: "update_failed", error: Some(error), downloaded: None, total: None, ..before.clone() }
}

/// An update is downloading or installing: a check would lose its place.
fn busy(u: &AppUpdateView) -> bool {
    matches!(u.state, "downloading" | "installing")
}

/// The download's progress after `got` more bytes, of `total` when the
/// server gave a length.
fn add_progress(u: &mut AppUpdateView, got: usize, total: Option<u64>) {
    u.downloaded = Some(u.downloaded.unwrap_or(0) + got as u64);
    if total.is_some() {
        u.total = total;
    }
}

/// The version on offer, for the tray's update item: one found by a check,
/// or one that could not install and can be tried again.
pub fn offered(u: &AppUpdateView) -> Option<String> {
    matches!(u.state, "available" | "update_failed").then(|| u.version.clone()).flatten()
}

/// A team's release needs a newer app: worth a check unless one is under
/// way or an update is already known.
fn team_wants_check(u: &AppUpdateView) -> bool {
    matches!(u.state, "not_checked" | "up_to_date" | "failed")
}

/// How often the app checks for itself while it runs.
const CHECK_EVERY: time::Duration = time::Duration::hours(24);
/// How often the daily check looks at the clock: the wall clock, not
/// the time Kivali has been awake, so a computer asleep overnight checks
/// soon after it wakes.
const CHECK_TICK: Duration = Duration::from_secs(10 * 60);

/// When the last check started, by the wall clock.
static LAST_CHECK: Mutex<Option<OffsetDateTime>> = Mutex::new(None);

/// The daily check is due at `now`: no check yet, a day since the last,
/// or a clock set back past it.
fn check_due(last: Option<OffsetDateTime>, now: OffsetDateTime) -> bool {
    last.is_none_or(|l| now < l || now - l >= CHECK_EVERY)
}

/// Marks, in the config directory, that the next launch is an update's
/// relaunch. The relaunch carries the arguments this process had (macOS:
/// `app.restart()` passes Tauri's `Env::args_os`; Windows: the plugin hands
/// the same arguments to the installer, whose `/ARGS` relaunches Kivali
/// with them), `--autostart` included when the login item started it, and
/// neither can be told otherwise. The mark makes that launch open the
/// window like any other ([`take_relaunch_mark`], lib.rs).
const RELAUNCH_MARK: &str = ".relaunch-after-update";

fn mark_relaunch(dir: &Path) {
    if let Err(e) = crate::paths::write_atomic(&dir.join(RELAUNCH_MARK), b"", 0o600) {
        eprintln!("kivali: marking the relaunch after the update: {e}");
    }
}

fn unmark_relaunch(dir: &Path) {
    let _ = std::fs::remove_file(dir.join(RELAUNCH_MARK));
}

/// At launch: whether this is an update's relaunch, which opens the
/// window even when started with `--autostart`. The mark goes, so the
/// next launch is itself again.
pub fn take_relaunch_mark(dir: &Path) -> bool {
    std::fs::remove_file(dir.join(RELAUNCH_MARK)).is_ok()
}

/// What the before-exit hook put aside for the installer, given back if
/// the installer could not start ([`put_back`]).
struct HandedOver {
    /// The teams that ran.
    running: Vec<String>,
    /// The windows that showed, by label.
    shown: Vec<String>,
}

static HANDED_OVER: Mutex<Option<HandedOver>> = Mutex::new(None);

/// Of the app's windows (label, showing), the ones to hide now and show
/// again if the installer cannot start: those that show.
fn shown_windows(windows: impl IntoIterator<Item = (String, bool)>) -> Vec<String> {
    windows.into_iter().filter(|(_, showing)| *showing).map(|(label, _)| label).collect()
}

/// The updater every check and download uses: the stall timeout, and
/// what runs before an installer that ends the process.
fn updater(app: &AppHandle) -> UpdaterBuilder {
    let mut builder = app
        .updater_builder()
        .configure_client(|c| c.read_timeout(STALL_TIMEOUT).connect_timeout(CONNECT_TIMEOUT));
    if platform::INSTALL_EXITS {
        let app = app.clone();
        builder = builder.on_before_exit(move || step_aside(&app));
    }
    builder
}

/// Before the installer starts (Windows: the plugin then exits the
/// process): every team pauses, as at Quit, and the tray icon and the
/// windows go, so none is left behind by the exit. Not Tauri's
/// `cleanup_before_exit`: if the installer cannot start, Kivali carries
/// on ([`put_back`]), and that cleanup leaves the app unfit to.
fn step_aside(app: &AppHandle) {
    let running = shell::pause_all_for_exit(app);
    mark_relaunch(&app.state::<Shell>().dir);
    // On the main thread, where Tauri keeps the tray, and waited for: the
    // plugin ends the process as soon as this returns.
    let (done, wait) = std::sync::mpsc::channel();
    let a = app.clone();
    let on_main = app.run_on_main_thread(move || {
        drop(a.remove_tray_by_id(menus::TRAY_ID));
        let windows = a.windows();
        let shown = shown_windows(windows.iter().map(|(label, w)| (label.clone(), w.is_visible().unwrap_or(false))));
        for label in &shown {
            if let Some(w) = windows.get(label) {
                let _ = w.hide();
            }
        }
        let _ = done.send(shown);
    });
    let shown = if on_main.is_ok() { wait.recv().unwrap_or_default() } else { Vec::new() };
    *HANDED_OVER.lock().unwrap() = Some(HandedOver { running, shown });
}

/// The installer could not start after [`step_aside`] (the permission
/// prompt answered No, a standard user): the tray and the windows that
/// showed come back and the teams that ran resume. Waits for the windows,
/// so Settings → General, opened next with why, comes in front of them.
fn put_back(app: &AppHandle, handed: HandedOver) {
    unmark_relaunch(&app.state::<Shell>().dir);
    shell::resume_after_failed_install(app, &handed.running);
    let (done, wait) = std::sync::mpsc::channel();
    let a = app.clone();
    let on_main = app.run_on_main_thread(move || {
        menus::restore_tray(&a);
        for label in &handed.shown {
            if let Some(w) = a.get_window(label) {
                let _ = w.show();
            }
        }
        let _ = done.send(());
    });
    if on_main.is_ok() {
        let _ = wait.recv();
    }
}

/// At launch: the first check, then one a day while Kivali runs; and
/// the installers earlier updates left in the temporary directory go.
pub fn start(app: &AppHandle) {
    check_desktop_update(app);
    let app = app.clone();
    let daily = std::thread::Builder::new().name("kivali-app-update".into()).spawn(move || {
        let name = app.package_info().name.clone();
        let mut leftovers = true;
        loop {
            // The installer that relaunched Kivali may still be running
            // from its file, which cannot be removed until it ends: tried
            // again at each tick until none is left. Never while an update
            // writes its own.
            if leftovers && !busy(&app.state::<Shell>().app_update.lock().unwrap()) {
                leftovers = remove_updater_leftovers(&std::env::temp_dir(), &name);
            }
            std::thread::sleep(CHECK_TICK);
            let quitting = app.state::<Shell>().quitting.load(Ordering::SeqCst);
            let last = *LAST_CHECK.lock().unwrap();
            if !quitting && check_due(last, OffsetDateTime::now_utc()) {
                check_desktop_update(&app);
            }
        }
    });
    if let Err(e) = daily {
        eprintln!("kivali: the daily app update check: {e}");
    }
}

/// A folder the Windows updater made for an update's installer:
/// `<app>-<version>-updater-<6 letters or digits>` (tauri-plugin-updater's
/// `make_temp_dir`, a `tempfile` directory it keeps). The installer,
/// `<app>-<version>-installer.exe` (about 850 MB), is written inside it
/// and would go when dropped, but the plugin ends the process with
/// `process::exit` once the installer starts, so it stays, and the
/// folder stays either way. (macOS: the plugin extracts and swaps the
/// bundle in `tempfile` directories it removes before returning.)
fn is_updater_leftover(folder: &str, app_name: &str) -> bool {
    let Some(rest) = folder.strip_prefix(app_name).and_then(|r| r.strip_prefix('-')) else { return false };
    let Some((version, random)) = rest.rsplit_once("-updater-") else { return false };
    !version.is_empty() && random.len() == 6 && random.chars().all(|c| c.is_ascii_alphanumeric())
}

/// Removes the updater's leftover folders from `temp`. Returns whether
/// any could not be removed yet.
fn remove_updater_leftovers(temp: &Path, app_name: &str) -> bool {
    let Ok(entries) = std::fs::read_dir(temp) else { return false };
    let mut left = false;
    for e in entries.flatten() {
        let is_dir = e.file_type().is_ok_and(|t| t.is_dir());
        if !is_dir || !e.file_name().to_str().is_some_and(|n| is_updater_leftover(n, app_name)) {
            continue;
        }
        match std::fs::remove_dir_all(e.path()) {
            Ok(()) => eprintln!("kivali: removed an earlier update's installer: {}", e.path().display()),
            Err(err) => {
                eprintln!("kivali: removing {}: {err}", e.path().display());
                left = true;
            }
        }
    }
    left
}

/// A team's check found a release that needs a newer app (shell.rs,
/// `run_check_blocking`): look for one now rather than at the next
/// launch, so General and the tray offer it.
pub fn check_for_team(app: &AppHandle) {
    let wants = team_wants_check(&app.state::<Shell>().app_update.lock().unwrap());
    if wants {
        check_desktop_update(app);
    }
}

pub fn check_desktop_update(app: &AppHandle) {
    {
        let sh = app.state::<Shell>();
        let mut u = sh.app_update.lock().unwrap();
        if busy(&u) {
            return;
        }
        *u = AppUpdateView { state: "checking", ..Default::default() };
    }
    *LAST_CHECK.lock().unwrap() = Some(OffsetDateTime::now_utc());
    shell::changed(app);
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let found = match updater(&app).build() {
            Ok(u) => u.check().await,
            Err(e) => Err(e),
        };
        let found = found.map(|u| u.map(|u| u.version)).map_err(|e| {
            eprintln!("kivali: app update check: {e}");
            e.to_string()
        });
        *app.state::<Shell>().app_update.lock().unwrap() = checked(found, shell::now_rfc3339());
        shell::changed(&app);
    });
}

/// Update answered (E4): downloads the new app while the teams keep
/// working, then installs it, pauses the teams (they resume after the
/// restart, as after Quit) and restarts.
pub fn install_app_update(app: &AppHandle) {
    let sh = app.state::<Shell>();
    if sh.quitting.load(Ordering::SeqCst) {
        return;
    }
    {
        let mut u = sh.app_update.lock().unwrap();
        if busy(&u) {
            return;
        }
        *u = AppUpdateView { state: "downloading", downloaded: Some(0), total: None, error: None, ..u.clone() };
    }
    shell::changed(app);
    let app = app.clone();
    let task = tauri::async_runtime::spawn(async move {
        let got = download(&app).await;
        // Installing blocks (Windows writes the installer to disk first):
        // off the async runtime.
        std::thread::spawn(move || install(&app, got));
    });
    *DOWNLOAD.lock().unwrap() = Some(task);
}

/// Quit: stop a download under way. Its install never starts.
pub fn cancel_download() {
    if let Some(task) = DOWNLOAD.lock().unwrap().take() {
        task.abort();
    }
}

async fn download(app: &AppHandle) -> Result<Option<(Update, Vec<u8>)>, String> {
    let updater = updater(app).build().map_err(|e| e.to_string())?;
    let Some(update) = updater.check().await.map_err(|e| e.to_string())? else { return Ok(None) };
    app.state::<Shell>().app_update.lock().unwrap().version = Some(update.version.clone());
    let progress = app.clone();
    let bytes = update
        .download(
            move |got, total| add_progress(&mut progress.state::<Shell>().app_update.lock().unwrap(), got, total),
            || {},
        )
        .await
        .map_err(|e| e.to_string())?;
    Ok(Some((update, bytes)))
}

fn install(app: &AppHandle, got: Result<Option<(Update, Vec<u8>)>, String>) {
    DOWNLOAD.lock().unwrap().take();
    let sh = app.state::<Shell>();
    let (update, bytes) = match got {
        Ok(Some(u)) => u,
        Ok(None) => return fail(app, "No newer Kivali is available any more.".into()),
        Err(e) => return fail(app, e),
    };
    // Quit chose first: it ends the app.
    if sh.quitting.swap(true, Ordering::SeqCst) {
        return;
    }
    {
        let mut u = sh.app_update.lock().unwrap();
        u.state = "installing";
        u.downloaded = None;
        u.total = None;
    }
    shell::changed(app);
    // On Windows the install writes the installer to a file, runs the
    // before-exit hook ([`step_aside`]) and starts the installer, which
    // ends the process inside this call; it returns only with an error:
    // before the hook (writing the file) or after it (the installer did
    // not start, ShellExecuteW's error).
    match update.install(bytes) {
        Ok(()) => {
            shell::pause_all_for_exit(app);
            mark_relaunch(&sh.dir);
            app.restart();
        }
        Err(e) => {
            sh.quitting.store(false, Ordering::SeqCst);
            let handed_over = HANDED_OVER.lock().unwrap().take();
            if let Some(handed) = handed_over {
                put_back(app, handed);
            }
            fail(app, e.to_string());
            windows::show_settings(app, "general");
        }
    }
}

fn fail(app: &AppHandle, error: String) {
    eprintln!("kivali: app update: {error}");
    let sh = app.state::<Shell>();
    {
        let mut u = sh.app_update.lock().unwrap();
        *u = update_failed(&u, error);
    }
    shell::changed(app);
}

#[cfg(test)]
mod tests {
    use super::*;

    // Settings → General words a failed check and a failed install
    // differently ("Couldn't check for updates" against "Couldn't update
    // to 1.2.0"). If both went back to one state, a download that broke
    // off would read as a check that never ran, and the person would not
    // learn that the update is still there to try again.
    #[test]
    fn a_failed_install_is_not_a_failed_check() {
        let at = "2026-10-20T09:00:00Z".to_string();
        let failed_check = checked(Err("offline".into()), at.clone());
        assert_eq!(failed_check.state, "failed");
        assert_eq!(failed_check.error.as_deref(), Some("offline"));

        let available = checked(Ok(Some("1.2.0".into())), at.clone());
        assert_eq!(available.state, "available");
        let failed = update_failed(&available, "The operation was canceled by the user. (os error 1223)".into());
        assert_eq!(failed.state, "update_failed");
        assert_eq!(failed.version.as_deref(), Some("1.2.0"), "the update stays on offer");
        assert_eq!(failed.checked_at.as_deref(), Some(at.as_str()));
        assert_eq!(failed.error.as_deref(), Some("The operation was canceled by the user. (os error 1223)"));

        assert_eq!(checked(Ok(None), at).state, "up_to_date");
    }

    // Settings → General draws the download's bar from these bytes (types.ts
    // `AppUpdate.downloaded` and `total`). Without them the ~850 MB Windows
    // download shows nothing moving for minutes, and the person cannot
    // tell a slow download from a stuck one.
    #[test]
    fn the_download_counts_its_bytes() {
        let mut u = AppUpdateView { state: "downloading", version: Some("1.2.0".into()), downloaded: Some(0), ..Default::default() };
        add_progress(&mut u, 1000, Some(5000));
        add_progress(&mut u, 500, None);
        assert_eq!((u.downloaded, u.total), (Some(1500), Some(5000)));
        let v = serde_json::to_value(&u).unwrap();
        assert_eq!(v["downloaded"], 1500);
        assert_eq!(v["total"], 5000);
        // A failure drops the bar and keeps the version on offer.
        let f = update_failed(&u, "timed out".into());
        assert_eq!((f.downloaded, f.total, f.version.as_deref()), (None, None, Some("1.2.0")));
    }

    // Kivali often stays in the tray for weeks; checking only at launch
    // left a new app unnoticed for as long. The daily check comes a day
    // after the last check of any kind (launch, Check for updates, a
    // team's), by the wall clock, so a computer asleep past the day checks
    // at the next look; a clock set back does not hold it off.
    #[test]
    fn the_app_checks_once_a_day() {
        let t0 = OffsetDateTime::from_unix_timestamp(1_792_000_000).unwrap();
        let h = time::Duration::hours;
        assert!(check_due(None, t0));
        assert!(!check_due(Some(t0), t0));
        assert!(!check_due(Some(t0), t0 + h(24) - time::Duration::seconds(1)));
        assert!(check_due(Some(t0), t0 + h(24)));
        assert!(check_due(Some(t0), t0 + h(72)));
        assert!(check_due(Some(t0), t0 - h(1)));
    }

    // The tray's update item: an update found by any check, the daily one
    // included, and one that could not install (it can be tried again).
    // Without it an update found while Kivali sits in the tray shows only
    // in Settings, where nobody is looking.
    #[test]
    fn the_tray_offers_a_known_update() {
        let v = |state| AppUpdateView { state, version: Some("1.2.0".into()), ..Default::default() };
        assert_eq!(offered(&v("available")).as_deref(), Some("1.2.0"));
        assert_eq!(offered(&v("update_failed")).as_deref(), Some("1.2.0"));
        for s in ["not_checked", "checking", "up_to_date", "downloading", "installing", "failed"] {
            assert_eq!(offered(&v(s)), None, "{s}");
        }
    }

    // A team's check that says the app must update first asks the app's
    // updater too, so General and the tray offer the app's update at once
    // instead of saying "Up to date · checked 4 days ago" beside it. Not
    // while a check or the update is under way, nor when it is known.
    #[test]
    fn a_team_that_needs_a_newer_app_starts_a_check() {
        let v = |state| AppUpdateView { state, ..Default::default() };
        for s in ["not_checked", "up_to_date", "failed"] {
            assert!(team_wants_check(&v(s)), "{s}");
        }
        for s in ["checking", "available", "downloading", "installing", "update_failed"] {
            assert!(!team_wants_check(&v(s)), "{s}");
        }
    }

    // When the Windows installer cannot start, the windows the update hid
    // come back with the tray. Only those that showed: a window the person
    // had closed (Kivali hides it) stays closed rather than popping up.
    // Without the record a team window open before the attempt stayed
    // hidden until reopened from the tray.
    #[test]
    fn the_windows_that_showed_come_back() {
        let windows = [("team-a".to_string(), true), ("settings".to_string(), false), ("team-b".to_string(), true)];
        assert_eq!(shown_windows(windows), ["team-a", "team-b"]);
        assert!(shown_windows(Vec::new()).is_empty());
    }

    // Kivali started at login relaunches after an update with --autostart
    // still in its arguments (on Windows the installer passes them on as
    // they were), which alone would bring back only the tray icon, on
    // Windows 11 perhaps hidden in the overflow. The mark makes that launch
    // open the window, once.
    #[test]
    fn the_relaunch_after_an_update_opens_the_window() {
        let dir = tempfile::tempdir().unwrap();
        assert!(!take_relaunch_mark(dir.path()), "an ordinary launch");
        mark_relaunch(dir.path());
        assert!(take_relaunch_mark(dir.path()), "the relaunch");
        assert!(!take_relaunch_mark(dir.path()), "the launch after it is ordinary again");
        // An installer that could not start takes the mark back.
        mark_relaunch(dir.path());
        unmark_relaunch(dir.path());
        assert!(!take_relaunch_mark(dir.path()));
    }

    // On Windows every update left its installer, about 850 MB, in %TEMP%:
    // the updater ends the process before its own cleanup runs. Without
    // this the space is lost for good, one installer per update. Only the
    // updater's own folders go; anything else in the temporary directory
    // stays.
    #[test]
    fn an_earlier_updates_installer_is_removed() {
        let temp = tempfile::tempdir().unwrap();
        let make = |name: &str| {
            let d = temp.path().join(name);
            std::fs::create_dir(&d).unwrap();
            std::fs::write(d.join("Kivali-1.2.0-installer.exe"), b"MZ").unwrap();
        };
        make("Kivali-1.2.0-updater-a1B2c3");
        make("Kivali-1.3.0-rc.1-updater-ZZ9zz9");
        let keep = ["Kivali-notes", "Other-1.2.0-updater-a1B2c3", "Kivali-1.2.0-updater-a1B2c", "Kivali--updater-a1B2c3"];
        for k in keep {
            make(k);
        }
        std::fs::write(temp.path().join("Kivali-1.1.0-updater-q1w2e3"), b"a file, not the updater's folder").unwrap();

        assert!(!remove_updater_leftovers(temp.path(), "Kivali"), "nothing left over");
        let mut after: Vec<String> =
            std::fs::read_dir(temp.path()).unwrap().map(|e| e.unwrap().file_name().into_string().unwrap()).collect();
        after.sort();
        let mut want: Vec<String> = keep.iter().map(|k| k.to_string()).collect();
        want.push("Kivali-1.1.0-updater-q1w2e3".into());
        want.sort();
        assert_eq!(after, want);
    }

    // A check started while the update downloads (Check for updates in
    // the menu, the daily check) would put the state back to `checking`
    // and hide the download's progress, though the download goes on and
    // installs. Downloading and installing are left alone.
    #[test]
    fn a_check_leaves_a_download_alone() {
        for s in ["downloading", "installing"] {
            assert!(busy(&AppUpdateView { state: s, ..Default::default() }), "{s}");
        }
        for s in ["not_checked", "checking", "up_to_date", "available", "failed", "update_failed"] {
            assert!(!busy(&AppUpdateView { state: s, ..Default::default() }), "{s}");
        }
    }
}
