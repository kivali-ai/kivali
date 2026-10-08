//! Kivali Desktop: the Tauri shell. docs/developers/desktop-app.md is the
//! reference.

pub mod cli;
mod commands;
mod diagnostics;
mod hostinfo;
mod menus;
pub mod orgurl;
mod ownersignin;
pub mod paths;
pub mod platform;
mod shell;
mod signin;
pub mod supervisor;
mod teamapi;
pub mod teams;
mod trayicon;
pub mod updates;
mod view;
mod windows;

use shell::Shell;
use std::path::Path;
use std::time::Duration;
use tauri::{AppHandle, Manager, RunEvent};
use teams::TeamsFile;

/// tauri.conf.json's version, the single desktop version (build.rs).
pub const APP_VERSION: &str = env!("KIVALI_DESKTOP_VERSION");

/// An HTTP client on the rustls + ring stack the updater already uses,
/// with a 10-second limit, so a silent host never hangs a page.
pub(crate) fn http_client(redirect: reqwest::redirect::Policy) -> Result<reqwest::Client, String> {
    install_crypto_provider();
    reqwest::Client::builder()
        .redirect(redirect)
        .timeout(Duration::from_secs(10))
        .user_agent(format!("KivaliDesktop/{APP_VERSION}"))
        .build()
        .map_err(|e| format!("cannot set up HTTP: {e}"))
}

/// reqwest is built without a TLS provider of its own (the updater's
/// choice); every client needs the process default installed first.
pub(crate) fn install_crypto_provider() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}

pub(crate) fn open_url_in_browser(app: &AppHandle, url: &str) -> Result<(), String> {
    use tauri_plugin_opener::OpenerExt;
    app.opener().open_url(url, None::<&str>).map_err(|e| e.to_string())
}

/// The third-party license notices a release build bundles as a resource
/// (desktop-app/Makefile, scripts/third-party-licenses.sh).
const LICENSES_FILE: &str = "THIRD_PARTY_LICENSES.txt";

/// Kivali → Open source licenses (Windows: Help): the bundled notices, in
/// the system's viewer for text files.
pub(crate) fn show_licenses(app: &AppHandle) -> Result<(), String> {
    use tauri_plugin_opener::OpenerExt;
    let path = app.path().resource_dir().map_err(|e| e.to_string())?.join(LICENSES_FILE);
    if !path.exists() {
        return Err(format!("no {LICENSES_FILE} in this build's resources"));
    }
    app.opener().open_path(path.to_string_lossy(), None::<&str>).map_err(|e| e.to_string())
}

/// Shows a folder in the file manager (macOS: Finder).
pub(crate) fn reveal(app: &AppHandle, dir: &Path) -> Result<(), String> {
    use tauri_plugin_opener::OpenerExt;
    app.opener().open_path(dir.to_string_lossy(), None::<&str>).map_err(|e| e.to_string())
}

/// Help → Show logs, Settings → Advanced → Logs: the team in front's logs
/// folder, else the teams folder, else the config directory.
pub(crate) fn show_logs(app: &AppHandle) -> Result<(), String> {
    let front = app.state::<Shell>().front_team.lock().unwrap().clone();
    show_logs_of(app, front.as_deref())
}

/// The logs folder of `team` (a team window's Help → Show logs), else the
/// teams folder, else the config directory.
pub(crate) fn show_logs_of(app: &AppHandle, team: Option<&str>) -> Result<(), String> {
    let sh = app.state::<Shell>();
    let dir = team
        .and_then(|id| sh.team(id))
        .and_then(|t| t.dir(&sh.dir))
        .map(|d| d.join("logs"))
        .filter(|d| d.is_dir())
        .or_else(|| Some(sh.dir.join(teams::TEAMS_DIR)).filter(|d| d.is_dir()))
        .unwrap_or_else(|| sh.dir.clone());
    reveal(app, &dir)
}

/// A team's own address in the system browser.
pub(crate) fn open_team_in_browser(app: &AppHandle, id: &str) -> Result<(), String> {
    let t = app.state::<Shell>().team(id).ok_or("no such team")?;
    let origin = t.origin().ok_or("The team has no address yet; resume it first.")?;
    open_url_in_browser(app, &format!("{origin}/"))
}

pub(crate) fn set_start_at_login(app: &AppHandle, enabled: bool) -> Result<(), String> {
    use tauri_plugin_autostart::ManagerExt;
    let al = app.autolaunch();
    if enabled { al.enable() } else { al.disable() }.map_err(|e| e.to_string())?;
    let sh = app.state::<Shell>();
    sh.teams.lock().unwrap().start_at_login = enabled;
    sh.save_teams()?;
    shell::changed(app);
    Ok(())
}

/// teams.json is the record of the person's choice; the login item
/// follows it.
fn reconcile_autostart(app: &AppHandle) {
    use tauri_plugin_autostart::ManagerExt;
    let want = app.state::<Shell>().teams.lock().unwrap().start_at_login;
    let al = app.autolaunch();
    if al.is_enabled().ok() != Some(want) {
        let r = if want { al.enable() } else { al.disable() };
        if let Err(e) = r {
            eprintln!("kivali: open at login: {e}");
        }
    }
}

fn now_unix() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Started from Finder, Explorer, the Start menu or the login item, the
/// shell's stderr goes nowhere (a Windows release build has no console
/// at all): send it to `logs/shell.log` in the config directory, so what
/// it reports (every `kivali:` line) can be read afterwards. Run from a
/// terminal it stays there, as does a redirected or piped stderr on
/// Windows ([`platform::stderr_is_seen`]).
fn log_to_file(dir: &Path) {
    if platform::stderr_is_seen() {
        return;
    }
    let logs = dir.join("logs");
    if paths::ensure_dir(&logs).is_err() {
        return;
    }
    let Ok(f) = paths::open_append(&logs.join("shell.log"), 0o600) else { return };
    if platform::redirect_output(f).is_ok() {
        eprintln!("kivali: started {} (Kivali {APP_VERSION})", now_unix());
    }
}

pub fn run() {
    let dir = platform::config_dir();
    log_to_file(&dir);
    // The login item starts the app with --autostart: resume the teams
    // that were running and sit in the tray without opening a window.
    let autostarted = std::env::args().any(|a| a == "--autostart");

    let app = tauri::Builder::default()
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            Some(vec!["--autostart"]),
        ))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        .invoke_handler(commands::handler())
        .setup(move |app| {
            paths::ensure_dir(&dir)?;
            // One shell per config directory. A second launch exits
            // without touching teams.json or any supervisor; where the
            // platform can, it asks the running shell to show itself,
            // otherwise it says so in a dialog.
            let Some(lock) = platform::InstanceLock::acquire(&dir)? else {
                if platform::SECOND_LAUNCH_FOCUSES && platform::notify_running_instance(&dir) {
                    app.handle().exit(0);
                    return Ok(());
                }
                use tauri_plugin_dialog::{DialogExt, MessageDialogKind};
                let handle = app.handle().clone();
                app.dialog()
                    .message(format!("Kivali is already running. Use the Kivali icon in the {}.", platform::TRAY_AREA))
                    .title("Kivali")
                    .kind(MessageDialogKind::Info)
                    .show(move |_| handle.exit(0));
                return Ok(());
            };
            app.manage(lock);
            let (teams, report) = TeamsFile::load(&dir, now_unix())?;
            for r in &report.repairs {
                eprintln!("kivali: teams.json: {r}");
            }
            if !report.repairs.is_empty() && report.moved_aside.is_none() {
                teams.save(&dir)?;
            }
            let has_teams = !teams.teams.is_empty();
            // The VM image ships as the bundle's `vm` resource when the
            // build had one (macOS: Contents/Resources/vm; Windows: <install
            // dir>\vm, since Tauri's resource directory there is the
            // executable's); otherwise the supervisor looks for it.
            let vm_dir = app
                .path()
                .resource_dir()
                .ok()
                .map(|r| r.join("vm"))
                .filter(|d| d.join(platform::VM_ROOT_FILE).exists());
            app.manage(Shell::new(dir.clone(), APP_VERSION.to_string(), teams, vm_dir));
            let handle = app.handle();
            reconcile_autostart(handle);
            menus::install(handle)?;
            let shown = handle.clone();
            platform::on_second_launch(
                &dir,
                Box::new(move || {
                    let app = shown.clone();
                    let _ = shown.run_on_main_thread(move || windows::open_kivali(&app));
                }),
            );
            // A click on a notification opens what it is about.
            let clicked = handle.clone();
            platform::on_notification_click(
                handle,
                Box::new(move |target| match target {
                    platform::NotifyTarget::Team(id) => windows::show_team(&clicked, &id),
                    platform::NotifyTarget::Settings(route) => windows::show_settings(&clicked, &route),
                }),
            );
            // Adopting supervisors and resuming teams talks to sockets and
            // may start processes: never on the main thread.
            let h2 = handle.clone();
            std::thread::spawn(move || shell::launch(&h2));
            if !autostarted {
                if has_teams {
                    windows::open_kivali(handle);
                } else {
                    windows::show_setup(handle, "welcome");
                }
            }
            shell::check_desktop_update(handle);
            // Started at login, nothing opens: Kivali starts in the menu
            // bar alone (a window shown later puts it in the Dock).
            if autostarted {
                windows::sync_dock(handle, None);
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("Kivali Desktop failed to start");

    app.run(|app, event| {
        // No Shell: this is a second instance on its way out.
        let Some(sh) = app.try_state::<Shell>() else { return };
        match event {
            RunEvent::ExitRequested { api, .. } => {
                // Closing the last window leaves Kivali in the tray; only
                // Quit (which sets `quitting` once teams are paused) exits.
                if !sh.quitting.load(std::sync::atomic::Ordering::SeqCst) {
                    api.prevent_exit();
                }
            }
            RunEvent::Exit => shell::on_exit(&sh),
            ref e if platform::is_reopen(e) => windows::open_kivali(app),
            _ => {}
        }
    });
}
