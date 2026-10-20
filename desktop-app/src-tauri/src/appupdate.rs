//! The app's own updates, through tauri-plugin-updater: the check and
//! the install (E4, "Launch and quit" in docs/developers/desktop-app.md).

use crate::platform;
use crate::shell::{self, AppUpdateView, Shell};
use std::sync::atomic::Ordering;
use tauri::{AppHandle, Manager};

pub fn check_desktop_update(app: &AppHandle) {
    use tauri_plugin_updater::UpdaterExt;
    *app.state::<Shell>().app_update.lock().unwrap() = AppUpdateView { state: "checking", ..Default::default() };
    shell::changed(app);
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let found = match app.updater() {
            Ok(u) => u.check().await,
            Err(e) => Err(e),
        };
        let checked_at = Some(shell::now_rfc3339());
        let v = match found {
            Ok(Some(update)) => AppUpdateView { state: "available", version: Some(update.version.clone()), checked_at, error: None },
            Ok(None) => AppUpdateView { state: "up_to_date", version: None, checked_at, error: None },
            Err(e) => {
                eprintln!("kivali: app update check: {e}");
                AppUpdateView { state: "failed", version: None, checked_at, error: Some(e.to_string()) }
            }
        };
        *app.state::<Shell>().app_update.lock().unwrap() = v;
        shell::changed(&app);
    });
}

/// Restart to update answered: installs the new app, pauses the teams (they
/// resume after the restart, as after Quit), restarts.
pub fn install_app_update(app: &AppHandle) {
    use tauri_plugin_updater::UpdaterExt;
    let sh = app.state::<Shell>();
    if sh.quitting.swap(true, Ordering::SeqCst) {
        return;
    }
    sh.app_update.lock().unwrap().state = "installing";
    shell::changed(app);
    let app = app.clone();
    std::thread::spawn(move || {
        let app2 = app.clone();
        let installed: Result<bool, String> = tauri::async_runtime::block_on(async move {
            let mut builder = app2.updater_builder();
            if platform::INSTALL_EXITS {
                let app3 = app2.clone();
                builder = builder.on_before_exit(move || {
                    shell::pause_all_for_exit(&app3);
                    app3.cleanup_before_exit();
                });
            }
            let updater = builder.build().map_err(|e| e.to_string())?;
            let Some(update) = updater.check().await.map_err(|e| e.to_string())? else { return Ok(false) };
            update.download_and_install(|_, _| {}, || {}).await.map_err(|e| e.to_string())?;
            Ok(true)
        });
        match installed {
            Ok(true) => {
                shell::pause_all_for_exit(&app);
                app.restart();
            }
            other => {
                let sh = app.state::<Shell>();
                sh.quitting.store(false, Ordering::SeqCst);
                *sh.app_update.lock().unwrap() = AppUpdateView {
                    state: "failed",
                    error: Some(match other {
                        Err(e) => e,
                        _ => "No newer Kivali is available any more.".into(),
                    }),
                    ..Default::default()
                };
                shell::changed(&app);
            }
        }
    });
}
