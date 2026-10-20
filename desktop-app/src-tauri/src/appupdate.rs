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

/// The teams that ran when the before-exit hook paused them for the
/// installer; taken back if the installer could not start.
static HANDED_OVER: Mutex<Option<Vec<String>>> = Mutex::new(None);

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
    *HANDED_OVER.lock().unwrap() = Some(running);
    // On the main thread, where Tauri keeps the tray, and waited for: the
    // plugin ends the process as soon as this returns.
    let (done, wait) = std::sync::mpsc::channel();
    let a = app.clone();
    let on_main = app.run_on_main_thread(move || {
        drop(a.remove_tray_by_id(menus::TRAY_ID));
        for w in a.windows().into_values() {
            let _ = w.hide();
        }
        let _ = done.send(());
    });
    if on_main.is_ok() {
        let _ = wait.recv();
    }
}

/// The installer could not start after [`step_aside`] (the permission
/// prompt answered No, a standard user): the tray comes back, the teams
/// that ran resume, and Settings → General says why.
fn put_back(app: &AppHandle, running: &[String]) {
    shell::resume_after_failed_install(app, running);
    let a = app.clone();
    let _ = app.run_on_main_thread(move || menus::restore_tray(&a));
}

/// At launch: the first check, then one a day while Kivali runs.
pub fn start(app: &AppHandle) {
    check_desktop_update(app);
    let app = app.clone();
    let daily = std::thread::Builder::new().name("kivali-app-update".into()).spawn(move || loop {
        std::thread::sleep(CHECK_TICK);
        let quitting = app.state::<Shell>().quitting.load(Ordering::SeqCst);
        let last = *LAST_CHECK.lock().unwrap();
        if !quitting && check_due(last, OffsetDateTime::now_utc()) {
            check_desktop_update(&app);
        }
    });
    if let Err(e) = daily {
        eprintln!("kivali: the daily app update check: {e}");
    }
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
            app.restart();
        }
        Err(e) => {
            sh.quitting.store(false, Ordering::SeqCst);
            let handed_over = HANDED_OVER.lock().unwrap().take();
            if let Some(running) = handed_over {
                put_back(app, &running);
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
