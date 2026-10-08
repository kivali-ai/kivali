//! Flags handled before any window exists, so the binary can be checked
//! from a terminal or a build script:
//!
//! - `--version`: prints `Kivali Desktop <version>` and exits 0.
//! - `--check`: reads (never writes) the config directory and reports
//!   teams.json, the bundled supervisor, the broker (Windows: whether its
//!   pipe exists) and each team's RPC endpoint; exits 0 when the
//!   file reads and the supervisor binary is present, 1 otherwise.
//!
//! On Windows a release build is a GUI-subsystem program with no console
//! of its own: run from one, it attaches to it before printing
//! ([`platform::attach_terminal`], which explains the prompt coming back
//! first).

use crate::teams::{Place, TeamsFile};
use crate::supervisor::sidecar;
use crate::{platform, APP_VERSION};
use std::io::Write;

/// A flag answered before any window exists.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Flag {
    Version,
    Check,
}

/// The first flag among the arguments. Any other argument is the app's
/// (`--autostart`) or the OS's (macOS: `-psn_…` from Finder).
fn flag(mut args: impl Iterator<Item = String>) -> Option<Flag> {
    args.find_map(|a| match a.as_str() {
        "--version" | "-V" => Some(Flag::Version),
        "--check" => Some(Flag::Check),
        _ => None,
    })
}

/// The binary's entry (main.rs): answers a flag on standard output, once
/// that output reaches the terminal the binary was run from. Returns the
/// exit code when a flag was handled; None means run the app.
pub fn answer(args: impl Iterator<Item = String>) -> Option<i32> {
    let f = flag(args)?;
    platform::attach_terminal();
    Some(reply(f, &mut std::io::stdout()))
}

/// [`answer`] into `out`, with no terminal to attach. Returns an exit
/// code when a flag was handled; None means run the app.
pub fn handle(args: impl Iterator<Item = String>, out: &mut dyn Write) -> Option<i32> {
    flag(args).map(|f| reply(f, out))
}

fn reply(f: Flag, out: &mut dyn Write) -> i32 {
    match f {
        Flag::Version => {
            let _ = writeln!(out, "Kivali Desktop {APP_VERSION}");
            0
        }
        Flag::Check => check(out),
    }
}

fn check(out: &mut dyn Write) -> i32 {
    let mut ok = true;
    let dir = platform::config_dir();
    let _ = writeln!(out, "Kivali Desktop {APP_VERSION} (ai.kivali.desktop)");
    let _ = writeln!(out, "config dir: {}", dir.display());
    let teams_path = dir.join(crate::teams::TEAMS_FILE);
    let mut endpoints = Vec::new();
    match std::fs::read_to_string(&teams_path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            let _ = writeln!(out, "teams.json: absent (first launch shows the welcome)");
        }
        Err(e) => {
            ok = false;
            let _ = writeln!(out, "teams.json: unreadable: {e}");
        }
        Ok(text) => match TeamsFile::parse(&text) {
            Err(e) => {
                ok = false;
                let _ = writeln!(out, "teams.json: not valid JSON: {e}");
            }
            Ok((f, repairs)) => {
                let _ = writeln!(out, "teams.json: {} team(s)", f.teams.len());
                for t in &f.teams {
                    let what = match &t.place {
                        Place::Here(l) => format!("here, folder {}, port {}", l.dir, l.port),
                        Place::Elsewhere { url } => format!("elsewhere, {url}"),
                    };
                    let _ = writeln!(out, "  {} {:?} ({what})", t.id, t.name);
                    if let Some(d) = t.dir(&dir) {
                        endpoints.push((t.id.clone(), d));
                    }
                }
                for r in repairs {
                    let _ = writeln!(out, "  would repair: {r}");
                }
            }
        },
    }
    match sidecar::binary_path() {
        Ok(p) if p.exists() => {
            let _ = writeln!(out, "supervisor: {}", p.display());
        }
        Ok(p) => {
            ok = false;
            let _ = writeln!(out, "supervisor: MISSING at {}", p.display());
        }
        Err(e) => {
            ok = false;
            let _ = writeln!(out, "supervisor: {e}");
        }
    }
    if let Some(up) = platform::broker_listening() {
        let _ = writeln!(out, "broker: {}", if up { "answering" } else { "not running" });
    }
    for (id, d) in endpoints {
        let endpoint = platform::endpoint(&d);
        let listening = platform::is_listening(&endpoint);
        let _ = writeln!(
            out,
            "{id}: endpoint {} ({})",
            endpoint.display(),
            if listening { "a supervisor is listening" } else { "nothing listening" }
        );
    }
    if ok {
        0
    } else {
        1
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn version_flag() {
        let mut out = Vec::new();
        assert_eq!(handle(["--version".to_string()].into_iter(), &mut out), Some(0));
        assert_eq!(String::from_utf8(out).unwrap(), format!("Kivali Desktop {APP_VERSION}\n"));
    }

    #[test]
    fn the_first_flag_is_answered() {
        let f = |args: &[&str]| flag(args.iter().map(|a| a.to_string()));
        assert_eq!(f(&["-V"]), Some(Flag::Version));
        assert_eq!(f(&["--autostart", "--check"]), Some(Flag::Check));
        assert_eq!(f(&["--check", "--version"]), Some(Flag::Check));
        assert_eq!(f(&["--autostart"]), None);
        assert_eq!(f(&[]), None);
    }

    #[test]
    fn no_flag_runs_the_app() {
        let mut out = Vec::new();
        // The OS may pass flags of its own (macOS: -psn_… from Finder).
        assert_eq!(handle(["-psn_0_12345".to_string(), "--autostart".to_string()].into_iter(), &mut out), None);
        assert!(out.is_empty());
    }
}
