//! The app's own updates, through tauri-plugin-updater: the check and
//! the install (E4, "Launch and quit" in docs/developers/desktop-app.md).
//!
//! States (`AppUpdateView::state`, types.ts `AppUpdate`): `not_checked`,
//! `checking`, `up_to_date`, `available`, `installing`; `failed` when a
//! check could not finish, `update_failed` when an update could not
//! install (the update stays on offer).

use crate::platform;
use crate::shell::{self, AppUpdateView, Shell};
use std::sync::atomic::Ordering;
use tauri::{AppHandle, Manager};

/// A check's answer: the newer version, none, or why there is no answer.
fn checked(found: Result<Option<String>, String>, checked_at: String) -> AppUpdateView {
    let checked_at = Some(checked_at);
    match found {
        Ok(Some(version)) => AppUpdateView { state: "available", version: Some(version), checked_at, error: None },
        Ok(None) => AppUpdateView { state: "up_to_date", version: None, checked_at, error: None },
        Err(e) => AppUpdateView { state: "failed", version: None, checked_at, error: Some(e) },
    }
}

/// An update that did not install: still on offer, with why.
fn update_failed(before: &AppUpdateView, error: String) -> AppUpdateView {
    AppUpdateView { state: "update_failed", error: Some(error), ..before.clone() }
}

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
        let found = found.map(|u| u.map(|u| u.version)).map_err(|e| {
            eprintln!("kivali: app update check: {e}");
            e.to_string()
        });
        *app.state::<Shell>().app_update.lock().unwrap() = checked(found, shell::now_rfc3339());
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
                let error = match other {
                    Err(e) => e,
                    _ => "No newer Kivali is available any more.".into(),
                };
                eprintln!("kivali: app update: {error}");
                let mut u = sh.app_update.lock().unwrap();
                *u = update_failed(&u, error);
                drop(u);
                shell::changed(&app);
            }
        }
    });
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
}
