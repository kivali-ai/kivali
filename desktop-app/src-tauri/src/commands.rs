//! IPC commands for the bundled pages (the contract is src/types.ts and
//! docs/developers/desktop-app.md, "The bundled pages").
//!
//! The ACL already confines these to the bundled pages' webviews on a
//! local origin (build.rs, capabilities/shell-pages.json). Each command
//! checks the calling webview's label again, so a mistake in the
//! capability file alone cannot hand a command to a team's web app.

use crate::shell::{self, ConnectError, ConnectFound, NewTeam, Shell, Snapshot};
use crate::supervisor::wire::{SetupInfo, SetupRequest, SetupResult};
use crate::windows;
use serde::Serialize;
use tauri::{AppHandle, Manager, State, Webview};

fn guard(webview: &Webview) -> Result<(), String> {
    // A second instance manages no Shell; nothing runs there.
    if windows::is_page_label(webview.label()) && webview.try_state::<Shell>().is_some() {
        Ok(())
    } else {
        Err("not allowed from this window".into())
    }
}

type Res<T = ()> = Result<T, String>;

#[tauri::command]
pub fn shell_snapshot(webview: Webview, shell: State<'_, Shell>) -> Res<Snapshot> {
    guard(&webview)?;
    Ok(shell.snapshot())
}

// ---- Windows ----

#[tauri::command]
pub fn set_title(webview: Webview, title: String) -> Res {
    guard(&webview)?;
    let title: String = title.chars().filter(|c| !c.is_control()).take(80).collect();
    webview.window().set_title(&title).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn close_window(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    let window = webview.window();
    window.hide().map_err(|e| e.to_string())?;
    windows::sync_dock(&app, Some(window.label()));
    Ok(())
}

#[tauri::command]
pub async fn open_settings(webview: Webview, app: AppHandle, id: Option<String>, tab: Option<String>) -> Res {
    guard(&webview)?;
    let route = match (id, tab) {
        (Some(id), Some(tab)) => format!("team/{id}/{tab}"),
        (Some(id), None) => format!("team/{id}"),
        (None, Some(tab)) if tab == "advanced" => "advanced".into(),
        _ => "general".into(),
    };
    windows::show_settings(&app, &route);
    Ok(())
}

#[tauri::command]
pub async fn open_setup(webview: Webview, app: AppHandle, route: String) -> Res {
    guard(&webview)?;
    if !matches!(route.as_str(), "welcome" | "add" | "connect") {
        return Err("no such setup page".into());
    }
    windows::show_setup(&app, &route);
    Ok(())
}

// ---- Setup: create ----

#[tauri::command]
pub fn owner_signin_start(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::owner_signin_start(&app)
}

#[tauri::command]
pub fn owner_signin_reset(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::owner_signin_reset(&app);
    Ok(())
}

/// "Create a team" chosen: the machine starts booting while setup asks.
#[tauri::command]
pub async fn prepare_team(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::prepare_team(&app)
}

/// Async: adopting the prepared machine may wait on its supervisor, which
/// must never happen on the main thread.
#[tauri::command]
pub async fn create_team(webview: Webview, app: AppHandle, team: NewTeam) -> Res<String> {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::create_team(&app, team))
        .await
        .map_err(|e| e.to_string())?
}

/// Setup left the new-team path: the machine prepared for it goes.
#[tauri::command]
pub fn discard_prepared(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::discard_prepared_later(&app);
    Ok(())
}

/// Async: clearing the previous provider waits on the supervisor.
#[tauri::command]
pub async fn open_claude_signin(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::open_claude_signin(&app, &id))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
pub async fn credential_setup(webview: Webview, app: AppHandle, id: String) -> Res<SetupInfo> {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::credential_setup(&app, &id))
        .await
        .map_err(|e| e.to_string())?
}

/// Signs a team's Claude in with a sign-in setup. The values carry
/// secrets; nothing here logs them.
#[tauri::command]
pub async fn apply_credential_setup(
    webview: Webview,
    app: AppHandle,
    id: String,
    setup: String,
    values: std::collections::BTreeMap<String, String>,
) -> Res<SetupResult> {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::apply_credential_setup(&app, &id, SetupRequest { setup, values }))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
pub async fn finish_setup(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    shell::finish_setup(&app, &id);
    windows::close_setup(&app);
    windows::show_team_signed_in(&app, &id);
    shell::owner_signin_reset(&app);
    Ok(())
}

#[tauri::command]
pub async fn abandon_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::abandon_team_blocking(&app, &id).map(|_| ()))
        .await
        .map_err(|e| e.to_string())?
}

#[tauri::command]
pub async fn retry_create(webview: Webview, app: AppHandle, id: String) -> Res<String> {
    guard(&webview)?;
    tauri::async_runtime::spawn_blocking(move || shell::retry_create(&app, &id))
        .await
        .map_err(|e| e.to_string())?
}

// ---- Setup: connect ----

#[tauri::command]
pub async fn connect_check(webview: Webview, address: String) -> Result<ConnectFound, ConnectError> {
    guard(&webview).map_err(|m| ConnectError { kind: "invalid", message: m })?;
    shell::connect_check(&address).await
}

#[tauri::command]
pub async fn connect_signin(webview: Webview, app: AppHandle, origin: String, name: String) -> Res {
    guard(&webview)?;
    shell::connect_signin(&app, &origin, &name)
}

#[tauri::command]
pub fn connect_reset(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::connect_reset(&app);
    Ok(())
}

// ---- Teams ----

#[tauri::command]
pub async fn open_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    windows::show_team(&app, &id);
    Ok(())
}

#[tauri::command]
pub async fn open_team_page(webview: Webview, app: AppHandle, id: String, path: String) -> Res {
    guard(&webview)?;
    windows::open_team_path(&app, &id, &path);
    Ok(())
}

#[tauri::command]
pub fn confirm_pause(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    crate::menus::confirm_pause(&app, &id, false);
    Ok(())
}

#[tauri::command]
pub fn resume_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    shell::resume_team(&app, &id)
}

#[tauri::command]
pub fn retry_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    let sh = app.state::<Shell>();
    if sh.team(&id).is_some_and(|t| !t.is_here()) {
        shell::check_reach(&app, &id);
        return Ok(());
    }
    shell::resume_or_record(&app, &id)
}

#[tauri::command]
pub fn pause_and_resume(webview: Webview, app: AppHandle, pause: String, resume: String) -> Res {
    guard(&webview)?;
    shell::pause_and_resume(&app, &pause, &resume)
}

#[tauri::command]
pub fn update_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    shell::update_team(&app, &id)
}

#[tauri::command]
pub fn open_in_browser(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    crate::open_team_in_browser(&app, &id)
}

#[tauri::command]
pub fn remove_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    shell::remove_team(&app, &id)
}

/// E8 then F5: the typed name must match exactly; then the system's own
/// password or Touch ID prompt; then the delete runs (its progress is the
/// team's operation).
#[tauri::command]
pub async fn delete_team(webview: Webview, app: AppHandle, id: String, typed: String) -> Res {
    guard(&webview)?;
    let name = app.state::<Shell>().team(&id).map(|t| t.name).ok_or("no such team")?;
    if typed != name {
        return Err("Type the team's name exactly.".into());
    }
    let (tx, rx) = std::sync::mpsc::channel();
    crate::platform::authenticate(
        &app,
        // macOS reads it as "Kivali is trying to <reason>." (F5).
        &format!("delete the team {name}"),
        Box::new(move |r| {
            let _ = tx.send(r);
        }),
    );
    tauri::async_runtime::spawn_blocking(move || rx.recv())
        .await
        .map_err(|e| e.to_string())?
        .map_err(|_| "cancelled".to_string())??;
    shell::delete_team(&app, &id)
}

#[derive(Serialize)]
pub struct TeamFacts {
    pub agents: Option<u64>,
    pub files: Option<u64>,
    pub disk_used_bytes: Option<u64>,
}

/// The delete dialog's list: the agents and files read fresh from the team's own API as
/// its window's person (teamapi.rs; the last numbers when it can't be
/// asked now, null when never), the disk from the supervisor's last
/// report. Async: reading the window's cookies waits on the main thread.
#[tauri::command]
pub async fn team_facts(webview: Webview, app: AppHandle, id: String) -> Res<TeamFacts> {
    guard(&webview)?;
    let rt = app.state::<Shell>().runtime(&id).ok_or("no such team on this Mac")?;
    let facts = match crate::teamapi::load(&app, &id).await {
        Some(f) => Some(f),
        None => rt.facts.lock().unwrap().clone(),
    };
    let used = rt.report.lock().unwrap().as_ref().map(|r| r.disk_used_bytes).filter(|b| *b > 0);
    Ok(TeamFacts { agents: facts.as_ref().map(|f| f.agents), files: facts.as_ref().map(|f| f.files), disk_used_bytes: used })
}

#[tauri::command(rename_all = "snake_case")]
pub fn set_resources(webview: Webview, app: AppHandle, id: String, memory_mb: u32, cpus: u32) -> Res {
    guard(&webview)?;
    shell::set_resources(&app, &id, memory_mb, cpus)
}

/// Other devices: records (or with `address` null, clears) the https address the
/// operator put in front of the team.
#[tauri::command]
pub async fn set_public_url(webview: Webview, app: AppHandle, id: String, address: Option<String>) -> Res<Option<String>> {
    guard(&webview)?;
    shell::set_public_url(app, id, address).await
}

#[tauri::command]
pub async fn sign_out_team(webview: Webview, app: AppHandle, id: String) -> Res {
    guard(&webview)?;
    windows::sign_out_team(&app, &id);
    Ok(())
}

#[tauri::command]
pub fn reorder_teams(webview: Webview, app: AppHandle, ids: Vec<String>) -> Res {
    guard(&webview)?;
    let sh = app.state::<Shell>();
    sh.teams.lock().unwrap().reorder(&ids);
    sh.save_teams()?;
    shell::changed(&app);
    Ok(())
}

// ---- App ----

#[tauri::command]
pub fn set_setting(webview: Webview, app: AppHandle, key: String, value: bool) -> Res {
    guard(&webview)?;
    match key.as_str() {
        "start_at_login" => crate::set_start_at_login(&app, value),
        "ask_before_quit" => {
            let sh = app.state::<Shell>();
            sh.teams.lock().unwrap().ask_before_quit = value;
            sh.save_teams()?;
            shell::changed(&app);
            Ok(())
        }
        _ => Err("no such setting".into()),
    }
}

#[tauri::command]
pub fn check_app_update(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    shell::check_desktop_update(&app);
    Ok(())
}

#[tauri::command]
pub fn install_app_update(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    crate::menus::confirm_app_update(&app);
    Ok(())
}

#[tauri::command]
pub fn open_logs(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    crate::show_logs(&app)
}

#[tauri::command]
pub fn open_config_dir(webview: Webview, app: AppHandle) -> Res {
    guard(&webview)?;
    let dir = app.state::<Shell>().dir.clone();
    crate::reveal(&app, &dir)
}

#[tauri::command]
pub async fn save_diagnostics(webview: Webview, app: AppHandle) -> Res<String> {
    guard(&webview)?;
    crate::diagnostics::save(app).await
}

/// Opens an https link (What's new, release notes) in the system browser.
#[tauri::command]
pub fn open_external(webview: Webview, app: AppHandle, url: String) -> Res {
    guard(&webview)?;
    let parsed = url::Url::parse(&url).map_err(|e| e.to_string())?;
    if parsed.scheme() != "https" {
        return Err("only https links open from here".into());
    }
    crate::open_url_in_browser(&app, parsed.as_str())
}

pub fn handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        shell_snapshot,
        set_title,
        close_window,
        open_settings,
        open_setup,
        owner_signin_start,
        owner_signin_reset,
        prepare_team,
        discard_prepared,
        create_team,
        open_claude_signin,
        credential_setup,
        apply_credential_setup,
        finish_setup,
        abandon_team,
        retry_create,
        connect_check,
        connect_signin,
        connect_reset,
        open_team,
        open_team_page,
        confirm_pause,
        resume_team,
        retry_team,
        pause_and_resume,
        update_team,
        open_in_browser,
        remove_team,
        delete_team,
        team_facts,
        set_resources,
        set_public_url,
        sign_out_team,
        reorder_teams,
        set_setting,
        check_app_update,
        install_app_update,
        open_logs,
        open_config_dir,
        save_diagnostics,
        open_external,
    ]
}

/// Every command, for build.rs's app manifest and the capability (a
/// test checks both list them).
#[cfg(test)]
pub const COMMANDS: &[&str] = &[
    "shell_snapshot",
    "set_title",
    "close_window",
    "open_settings",
    "open_setup",
    "owner_signin_start",
    "owner_signin_reset",
    "prepare_team",
    "discard_prepared",
    "create_team",
    "open_claude_signin",
    "credential_setup",
    "apply_credential_setup",
    "finish_setup",
    "abandon_team",
    "retry_create",
    "connect_check",
    "connect_signin",
    "connect_reset",
    "open_team",
    "open_team_page",
    "confirm_pause",
    "resume_team",
    "retry_team",
    "pause_and_resume",
    "update_team",
    "open_in_browser",
    "remove_team",
    "delete_team",
    "team_facts",
    "set_resources",
    "set_public_url",
    "sign_out_team",
    "reorder_teams",
    "set_setting",
    "check_app_update",
    "install_app_update",
    "open_logs",
    "open_config_dir",
    "save_diagnostics",
    "open_external",
];

#[cfg(test)]
mod tests {
    /// build.rs and the capability list the same commands as the handler.
    #[test]
    fn manifest_and_capability_match() {
        let build = include_str!("../build.rs");
        let cap = include_str!("../capabilities/shell-pages.json");
        for c in super::COMMANDS {
            assert!(build.contains(&format!("\"{c}\"")), "build.rs lacks {c}");
            assert!(cap.contains(&format!("\"allow-{}\"", c.replace('_', "-"))), "capability lacks {c}");
        }
    }
}
