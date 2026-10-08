//! Settings → Advanced → Diagnostics: logs and settings in one
//! text file for support. No team data and no sign-ins: teams.json, each
//! team's local.json (no secrets live there) and the tail of each log.

use crate::shell::Shell;
use std::fmt::Write as _;
use std::path::Path;
use tauri::{AppHandle, Manager};

/// The most of one log the file carries.
const LOG_TAIL_LINES: usize = 2000;

fn tail(path: &Path, lines: usize) -> Option<String> {
    let text = std::fs::read_to_string(path).ok()?;
    let all: Vec<&str> = text.lines().collect();
    Some(all[all.len().saturating_sub(lines)..].join("\n"))
}

/// The file's contents for the config directory `dir`.
pub fn collect(dir: &Path, app_version: &str) -> String {
    let mut out = String::new();
    let _ = writeln!(out, "Kivali {app_version} diagnostics · {}", std::env::consts::OS);
    let mut section = |title: &str, body: Option<String>| {
        let _ = writeln!(out, "\n===== {title} =====");
        out.push_str(body.as_deref().unwrap_or("(none)"));
        out.push('\n');
    };
    section("teams.json", std::fs::read_to_string(dir.join(crate::teams::TEAMS_FILE)).ok());
    let mut team_dirs = vec![dir.to_path_buf()];
    if let Ok(rd) = std::fs::read_dir(dir.join(crate::teams::TEAMS_DIR)) {
        let mut more: Vec<_> = rd.filter_map(|e| e.ok().map(|e| e.path())).filter(|p| p.is_dir()).collect();
        more.sort();
        team_dirs.extend(more);
    }
    for d in team_dirs {
        // A label, not a path to open: its parts joined with `/` on every
        // OS, so a Windows file reads like a macOS one and never mixes the
        // path's `\` with the `/` of the suffixes below.
        let name = d
            .strip_prefix(dir)
            .map(|p| p.components().map(|c| c.as_os_str().to_string_lossy()).collect::<Vec<_>>().join("/"))
            .unwrap_or_default();
        let name = if name.is_empty() { ".".to_string() } else { name };
        if d.join("local.json").exists() {
            section(&format!("{name}/local.json"), std::fs::read_to_string(d.join("local.json")).ok());
        }
        for log in ["supervisor.log", "console.log"] {
            let p = d.join("logs").join(log);
            if p.exists() {
                section(&format!("{name}/logs/{log} (last {LOG_TAIL_LINES} lines)"), tail(&p, LOG_TAIL_LINES));
            }
        }
    }
    out
}

/// Asks where to save, then writes the file. Returns its path, or
/// "cancelled".
pub async fn save(app: AppHandle) -> Result<String, String> {
    use tauri_plugin_dialog::DialogExt;
    let name = format!("kivali-diagnostics-{}.txt", time::OffsetDateTime::now_utc().date());
    let app2 = app.clone();
    let path = tauri::async_runtime::spawn_blocking(move || {
        app2.dialog().file().set_title("Save diagnostics").set_file_name(name).blocking_save_file()
    })
    .await
    .map_err(|e| e.to_string())?;
    let Some(path) = path.and_then(|p| p.into_path().ok()) else { return Err("cancelled".into()) };
    let sh = app.state::<Shell>();
    let text = collect(&sh.dir, &sh.app_version);
    std::fs::write(&path, text).map_err(|e| format!("cannot write {}: {e}", path.display()))?;
    Ok(path.display().to_string())
}

#[cfg(test)]
mod tests {
    #[test]
    fn collects_teams_and_logs() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("teams.json"), "{\"teams\":[]}").unwrap();
        let t = dir.path().join("teams/home-0001/logs");
        std::fs::create_dir_all(&t).unwrap();
        std::fs::write(dir.path().join("teams/home-0001/local.json"), "{\"port\":8081}").unwrap();
        let log: String = (0..2500).map(|i| format!("line {i}\n")).collect();
        std::fs::write(t.join("supervisor.log"), log).unwrap();
        let out = super::collect(dir.path(), "0.16.0");
        assert!(out.contains("===== teams.json ====="));
        assert!(out.contains("teams/home-0001/local.json"));
        assert!(out.contains("line 2499") && !out.contains("line 499\n"));
    }
}
