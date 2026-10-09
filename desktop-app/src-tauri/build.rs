// The app manifest puts every app command under the ACL, so a command is
// callable only where a capability grants its `allow-*` permission:
// capabilities/shell-pages.json, which names the bundled pages' webviews
// (setup, settings, page-<team>) and local origins only. Keep this list in
// step with `commands::COMMANDS` (a test checks).
const COMMANDS: &[&str] = &[
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

/// The one desktop version is tauri.conf.json's. It becomes APP_VERSION
/// (`KIVALI_DESKTOP_VERSION`); Cargo.toml and package.json must carry
/// the same number, and the build stops when they do not.
fn desktop_version() -> String {
    let read = |p: &str| -> serde_json::Value {
        println!("cargo:rerun-if-changed={p}");
        serde_json::from_str(&std::fs::read_to_string(p).unwrap_or_else(|e| panic!("{p}: {e}")))
            .unwrap_or_else(|e| panic!("{p}: {e}"))
    };
    let conf = read("tauri.conf.json");
    let version = conf["version"].as_str().expect("tauri.conf.json has no version").to_string();
    let cargo = std::env::var("CARGO_PKG_VERSION").unwrap();
    assert_eq!(cargo, version, "Cargo.toml version {cargo} != tauri.conf.json version {version}");
    let npm = read("../package.json")["version"].as_str().unwrap_or_default().to_string();
    assert_eq!(npm, version, "package.json version {npm} != tauri.conf.json version {version}");
    version
}

fn main() {
    let version = desktop_version();
    println!("cargo:rustc-env=KIVALI_DESKTOP_VERSION={version}");
    tauri_build::try_build(
        tauri_build::Attributes::new()
            .app_manifest(tauri_build::AppManifest::new().commands(COMMANDS)),
    )
    .expect("tauri-build failed");
}
