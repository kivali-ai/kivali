//! The Windows terminal launcher's command lines, kept pure (and
//! compiled into the tests on every OS) because their quoting is the
//! part that goes wrong.
//!
//! Windows Terminal, when `wt.exe` is on PATH:
//!   wt.exe new-tab --title "Claude Code" -- <exe> --config-dir <dir> terminal
//! with every `;` escaped as `\;` (wt splits its own subcommands on `;`).
//! Otherwise a console window through cmd.exe:
//!   cmd.exe /c "start "" "<exe>" --config-dir "<dir>" terminal"
//! cmd strips the outer pair of quotes; `start` takes the empty `""` as
//! the window title.

use std::ffi::OsString;
use std::path::Path;

/// wt.exe's arguments (each one is quoted by Rust's MSVC-rule quoting).
pub fn wt_args(exe: &Path, dir: &Path) -> Vec<OsString> {
    let esc = |p: &Path| OsString::from(p.to_string_lossy().replace(';', "\\;"));
    vec![
        "new-tab".into(),
        "--title".into(),
        "Claude Code".into(),
        "--".into(),
        esc(exe),
        "--config-dir".into(),
        esc(dir),
        "terminal".into(),
    ]
}

/// One argument in double quotes for a program that parses its command
/// line by the MSVC rules (Go does): backslashes before the closing
/// quote are doubled. A `"` cannot occur in a Windows path, and a `%`
/// would be expanded by cmd.exe even inside quotes, so both are refused.
fn quote(s: &str) -> Result<String, String> {
    if s.contains('"') || s.contains('%') {
        return Err(format!("cannot pass {s:?} through cmd.exe (it contains \" or %)"));
    }
    let trailing = s.len() - s.trim_end_matches('\\').len();
    Ok(format!("\"{s}{}\"", "\\".repeat(trailing)))
}

/// cmd.exe's raw command line (passed with `raw_arg`, not re-quoted).
pub fn cmd_start_line(exe: &Path, dir: &Path) -> Result<String, String> {
    Ok(format!(
        "/c \"start \"\" {} --config-dir {} terminal\"",
        quote(&exe.to_string_lossy())?,
        quote(&dir.to_string_lossy())?
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn windows_terminal_arguments() {
        let a = wt_args(
            Path::new(r"C:\Program Files\Kivali\kivali-supervisor.exe"),
            Path::new(r"C:\Users\me\AppData\Local\Kivali"),
        );
        let a: Vec<String> = a.into_iter().map(|s| s.into_string().unwrap()).collect();
        assert_eq!(
            a,
            [
                "new-tab",
                "--title",
                "Claude Code",
                "--",
                r"C:\Program Files\Kivali\kivali-supervisor.exe",
                "--config-dir",
                r"C:\Users\me\AppData\Local\Kivali",
                "terminal"
            ]
        );
        let b = wt_args(Path::new(r"C:\a;b\k.exe"), Path::new(r"C:\x;y"));
        assert_eq!(b[4], r"C:\a\;b\k.exe");
        assert_eq!(b[6], r"C:\x\;y");
    }

    #[test]
    fn cmd_start_quoting() {
        let line = cmd_start_line(
            Path::new(r"C:\Program Files\Kivali\kivali-supervisor.exe"),
            Path::new(r"C:\Users\Jo & Al\AppData\Local\Kivali"),
        )
        .unwrap();
        assert_eq!(
            line,
            r#"/c "start "" "C:\Program Files\Kivali\kivali-supervisor.exe" --config-dir "C:\Users\Jo & Al\AppData\Local\Kivali" terminal""#
        );
        // A trailing backslash would escape the closing quote for the
        // program's own parser; it is doubled.
        let line = cmd_start_line(Path::new(r"C:\k.exe"), Path::new(r"D:\")).unwrap();
        assert!(line.contains(r#"--config-dir "D:\\" terminal"#), "{line}");
        assert!(cmd_start_line(Path::new(r"C:\k.exe"), Path::new(r"C:\%USERNAME%")).is_err());
    }
}
