//! The Kivali release check, as the shell shows it.
//!
//! The supervisor fetches `release.json` and reports a [`CheckReport`];
//! this module turns that, the app's own version and the clock into one
//! [`UpdateView`]: the text on the tray's update item and Settings,
//! whether the tray icon carries a badge, and what the item does. Never a
//! modal (docs/developers/desktop-app.md, "Update states").

use crate::supervisor::wire::{check_status, CheckReport};
use semver::Version;
use serde::Serialize;
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

/// Parses `v0.16.0`, `0.16.0` or `0.16.0-rc.1`.
pub fn parse_version(s: &str) -> Option<Version> {
    let s = s.trim();
    Version::parse(s.strip_prefix('v').unwrap_or(s)).ok()
}

/// "0.16.0" without a leading v, for display after "v".
fn bare(s: &str) -> &str {
    let s = s.trim();
    s.strip_prefix('v').unwrap_or(s)
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum UpdateState {
    /// No local org, so there is no Kivali release to check against.
    NoLocalOrg,
    /// A local org exists but no check has finished yet.
    NotChecked,
    /// The local org has no Kivali installed yet (a first boot that
    /// stopped short), so there is nothing to compare.
    NotInstalled,
    UpToDate { current: String, checked_at: Option<String> },
    Available { current: String, latest: String },
    DesktopFirst { latest: String, min_desktop: String },
    ManualSteps { latest: String, notes_url: Option<String> },
    Failed { error: String, last_ok_at: Option<String> },
}

/// Everything a surface needs to show the update item.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct UpdateView {
    #[serde(flatten)]
    pub state: UpdateState,
    /// The one line shown on the tray item and in Settings.
    pub label: String,
    /// The tray icon carries a badge.
    pub badge: bool,
    /// What clicking does: "upgrade", "retry", "update_desktop",
    /// "open_notes", or none (informational).
    pub action: Option<&'static str>,
}

/// Decides the state. Order matters and follows the upgrade preflight:
/// a failed check first (its report has nothing newer to compare), then
/// "nothing newer", then the minimum desktop version, then manual steps.
pub fn decide(report: Option<&CheckReport>, has_local: bool, app_version: &str) -> UpdateState {
    if !has_local {
        return UpdateState::NoLocalOrg;
    }
    let Some(r) = report else {
        return UpdateState::NotChecked;
    };
    if let Some(err) = r.error.as_ref().filter(|e| !e.is_empty()) {
        return UpdateState::Failed { error: err.clone(), last_ok_at: r.last_ok_at.clone() };
    }
    let current = r.current.clone();
    let latest_or_empty = || r.latest.clone().unwrap_or_default();
    // The shell's own judgement first: release.json's minimum desktop
    // version against this app's version (APP_VERSION, from
    // tauri.conf.json). It applies whenever the release is newer than
    // the org.
    let release_is_newer = match r.status.as_deref() {
        Some(check_status::UPGRADE | check_status::APP_TOO_OLD | check_status::MANUAL_STEPS) => true,
        Some(_) => false,
        None => matches!(
            (r.latest.as_deref().and_then(parse_version), parse_version(&current)),
            (Some(l), Some(c)) if l > c
        ),
    };
    if release_is_newer {
        if let Some(min) = r.min_desktop.as_deref().filter(|m| !m.trim().is_empty()) {
            if needs_newer_app(min, app_version) {
                return UpdateState::DesktopFirst { latest: latest_or_empty(), min_desktop: min.to_string() };
            }
        }
    }
    // Then the supervisor's verdict, as a second opinion: it alone says
    // "failed" and "not installed", and its "app too old" and "manual
    // steps" stand even when release.json's fields did not reach the
    // shell. "Upgrade available" still goes through the checks below.
    match r.status.as_deref() {
        Some(check_status::FAILED) => {
            return UpdateState::Failed { error: "the check failed".into(), last_ok_at: r.last_ok_at.clone() }
        }
        Some(check_status::NOT_INSTALLED) => return UpdateState::NotInstalled,
        Some(check_status::UP_TO_DATE) => {
            return UpdateState::UpToDate { current, checked_at: r.checked_at.clone() }
        }
        Some(check_status::APP_TOO_OLD) => {
            return UpdateState::DesktopFirst {
                latest: latest_or_empty(),
                min_desktop: r.min_desktop.clone().unwrap_or_default(),
            }
        }
        Some(check_status::MANUAL_STEPS) => {
            return UpdateState::ManualSteps { latest: latest_or_empty(), notes_url: r.notes_url.clone() }
        }
        _ => {}
    }
    let newer = match (r.latest.as_deref(), parse_version(&current)) {
        (Some(latest), Some(cur)) => match parse_version(latest) {
            Some(l) => l > cur,
            None => {
                return UpdateState::Failed {
                    error: format!("release.json names an unreadable version {latest:?}"),
                    last_ok_at: r.last_ok_at.clone(),
                }
            }
        },
        // The org reports no readable version (not installed yet, or a
        // dev build): offer what the feed has, if anything.
        (Some(latest), None) => parse_version(latest).is_some() && current.trim().is_empty(),
        (None, _) => false,
    };
    if !newer {
        return UpdateState::UpToDate { current, checked_at: r.checked_at.clone() };
    }
    let latest = r.latest.clone().unwrap_or_default();
    if r.manual_steps {
        return UpdateState::ManualSteps { latest, notes_url: r.notes_url.clone() };
    }
    UpdateState::Available { current, latest }
}

/// Whether release.json's minimum desktop version is above this app's.
/// An unreadable minimum (or app version) counts as unmet: upgrading
/// under an app that may be too old is the one wrong move.
fn needs_newer_app(min: &str, app_version: &str) -> bool {
    match (parse_version(min), parse_version(app_version)) {
        (Some(min_v), Some(app_v)) => min_v > app_v,
        _ => true,
    }
}

/// "just now", "5m ago", "3h ago", "yesterday", "4 days ago". `when` is
/// RFC 3339; an unreadable or missing time reads "never".
pub fn ago(when: Option<&str>, now: OffsetDateTime) -> String {
    let Some(t) = when.and_then(|w| OffsetDateTime::parse(w, &Rfc3339).ok()) else {
        return "never".into();
    };
    let secs = (now - t).whole_seconds();
    if secs < 60 {
        "just now".into()
    } else if secs < 3600 {
        format!("{}m ago", secs / 60)
    } else if secs < 24 * 3600 {
        format!("{}h ago", secs / 3600)
    } else if secs < 48 * 3600 {
        "yesterday".into()
    } else {
        format!("{} days ago", secs / 86400)
    }
}

pub fn view(state: UpdateState, now: OffsetDateTime) -> UpdateView {
    let (label, badge, action) = match &state {
        UpdateState::NoLocalOrg => ("No org on this computer to update".to_string(), false, None),
        UpdateState::NotChecked => ("Checking for Kivali updates…".to_string(), false, None),
        UpdateState::NotInstalled => ("Kivali isn't installed in the local org yet".to_string(), false, None),
        UpdateState::UpToDate { current, checked_at } => {
            let v = if current.trim().is_empty() { "Kivali".to_string() } else { format!("Kivali v{}", bare(current)) };
            (format!("{v} is up to date · checked {}", ago(checked_at.as_deref(), now)), false, Some("retry"))
        }
        UpdateState::Available { latest, .. } => {
            (format!("Upgrade to Kivali v{}…", bare(latest)), true, Some("upgrade"))
        }
        UpdateState::DesktopFirst { .. } => ("Update Kivali Desktop first".to_string(), true, Some("update_desktop")),
        UpdateState::ManualSteps { notes_url, .. } => (
            "This release needs steps the app can't do yet".to_string(),
            false,
            notes_url.as_ref().map(|_| "open_notes"),
        ),
        UpdateState::Failed { last_ok_at, .. } => (
            format!("Couldn't check for updates · last checked {} · Retry", ago(last_ok_at.as_deref(), now)),
            false,
            Some("retry"),
        ),
    };
    UpdateView { state, label, badge, action }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn now() -> OffsetDateTime {
        OffsetDateTime::parse("2026-10-01T12:00:00Z", &Rfc3339).unwrap()
    }

    fn report(current: &str, latest: Option<&str>) -> CheckReport {
        CheckReport {
            status: None,
            current: current.into(),
            latest: latest.map(Into::into),
            min_desktop: None,
            manual_steps: false,
            notes_url: None,
            checked_at: Some("2026-10-01T09:00:00Z".into()),
            last_ok_at: Some("2026-10-01T09:00:00Z".into()),
            error: None,
        }
    }

    #[test]
    fn versions() {
        assert!(parse_version("v0.17.0").unwrap() > parse_version("0.16.9").unwrap());
        assert!(parse_version("0.16.10").unwrap() > parse_version("0.16.9").unwrap());
        assert!(parse_version("0.17.0").unwrap() > parse_version("0.17.0-rc.2").unwrap());
        assert!(parse_version("0.17.0-rc.10").unwrap() > parse_version("0.17.0-rc.2").unwrap());
        assert_eq!(parse_version(" v1.2.3 ").unwrap(), parse_version("1.2.3").unwrap());
        assert!(parse_version("0.16").is_none());
        assert!(parse_version("dev").is_none());
    }

    #[test]
    fn up_to_date() {
        let s = decide(Some(&report("0.16.0", Some("0.16.0"))), true, "0.16.0");
        let v = view(s, now());
        assert_eq!(v.label, "Kivali v0.16.0 is up to date · checked 3h ago");
        assert!(!v.badge);
        // Older feed than the org (a rollback of the release) is not an upgrade.
        let s = decide(Some(&report("v0.17.0", Some("0.16.0"))), true, "0.16.0");
        assert_eq!(view(s, now()).label, "Kivali v0.17.0 is up to date · checked 3h ago");
    }

    #[test]
    fn available_badges() {
        let v = view(decide(Some(&report("0.16.0", Some("v0.17.0"))), true, "0.16.0"), now());
        assert_eq!(v.label, "Upgrade to Kivali v0.17.0…");
        assert!(v.badge);
        assert_eq!(v.action, Some("upgrade"));
    }

    #[test]
    fn desktop_first_beats_manual_steps() {
        let mut r = report("0.16.0", Some("0.17.0"));
        r.min_desktop = Some("0.17.0".into());
        r.manual_steps = true;
        let v = view(decide(Some(&r), true, "0.16.0"), now());
        assert_eq!(v.label, "Update Kivali Desktop first");
        assert_eq!(v.action, Some("update_desktop"));
        // A new enough app passes the minimum and reaches manual steps.
        let v = view(decide(Some(&r), true, "0.17.0"), now());
        assert_eq!(v.label, "This release needs steps the app can't do yet");
        assert_eq!(v.action, None);
        assert!(!v.badge);
    }

    #[test]
    fn unreadable_minimum_counts_as_unmet() {
        let mut r = report("0.16.0", Some("0.17.0"));
        r.min_desktop = Some("soon".into());
        assert!(matches!(decide(Some(&r), true, "9.9.9"), UpdateState::DesktopFirst { .. }));
        r.min_desktop = Some("".into());
        assert!(matches!(decide(Some(&r), true, "0.16.0"), UpdateState::Available { .. }));
    }

    #[test]
    fn shell_compares_min_desktop_even_when_the_supervisor_says_upgrade() {
        // The supervisor (a "dev" build, say) thinks the app is new
        // enough; release.json's minimum says otherwise.
        let mut r = report("0.16.0", Some("0.17.0"));
        r.status = Some(check_status::UPGRADE.into());
        r.min_desktop = Some("0.17.0".into());
        assert_eq!(
            decide(Some(&r), true, "0.16.0"),
            UpdateState::DesktopFirst { latest: "0.17.0".into(), min_desktop: "0.17.0".into() }
        );
        assert!(matches!(decide(Some(&r), true, "0.17.0"), UpdateState::Available { .. }));
        // Manual steps behind an unmet minimum: the app comes first.
        r.status = Some(check_status::MANUAL_STEPS.into());
        assert!(matches!(decide(Some(&r), true, "0.16.0"), UpdateState::DesktopFirst { .. }));
        // Nothing newer: the minimum is irrelevant.
        r.status = Some(check_status::UP_TO_DATE.into());
        assert!(matches!(decide(Some(&r), true, "0.16.0"), UpdateState::UpToDate { .. }));
    }

    #[test]
    fn supervisor_verdict_is_a_second_opinion() {
        // No minimum reached the shell; the supervisor still says the
        // app is too old.
        let mut r = report("0.16.0", Some("0.17.0"));
        r.status = Some(check_status::APP_TOO_OLD.into());
        assert!(matches!(decide(Some(&r), true, "0.16.0"), UpdateState::DesktopFirst { .. }));
    }

    #[test]
    fn manual_steps_links_notes() {
        let mut r = report("0.16.0", Some("0.17.0"));
        r.manual_steps = true;
        r.notes_url = Some("https://github.com/kivali-ai/kivali/releases".into());
        let v = view(decide(Some(&r), true, "0.17.0"), now());
        assert_eq!(v.action, Some("open_notes"));
    }

    #[test]
    fn failed_check() {
        let mut r = report("0.16.0", None);
        r.error = Some("timeout".into());
        r.last_ok_at = Some("2026-09-30T08:00:00Z".into());
        let v = view(decide(Some(&r), true, "0.16.0"), now());
        assert_eq!(v.label, "Couldn't check for updates · last checked yesterday · Retry");
        assert_eq!(v.action, Some("retry"));
        r.last_ok_at = None;
        let v = view(decide(Some(&r), true, "0.16.0"), now());
        assert_eq!(v.label, "Couldn't check for updates · last checked never · Retry");
    }

    #[test]
    fn unreadable_latest_is_a_failure() {
        let r = report("0.16.0", Some("latest"));
        assert!(matches!(decide(Some(&r), true, "0.16.0"), UpdateState::Failed { .. }));
    }

    #[test]
    fn no_local_and_not_checked() {
        assert_eq!(decide(None, false, "0.16.0"), UpdateState::NoLocalOrg);
        assert_eq!(decide(None, true, "0.16.0"), UpdateState::NotChecked);
    }

    #[test]
    fn ago_buckets() {
        let n = now();
        assert_eq!(ago(Some("2026-10-01T11:59:30Z"), n), "just now");
        assert_eq!(ago(Some("2026-10-01T11:55:00Z"), n), "5m ago");
        assert_eq!(ago(Some("2026-10-01T09:00:00+00:00"), n), "3h ago");
        assert_eq!(ago(Some("2026-10-01T05:00:00-04:00"), n), "3h ago");
        assert_eq!(ago(Some("2026-09-30T11:00:00Z"), n), "yesterday");
        assert_eq!(ago(Some("2026-09-27T12:00:00.5Z"), n), "3 days ago");
        assert_eq!(ago(Some("garbage"), n), "never");
        assert_eq!(ago(None, n), "never");
        // A clock that ran backwards reads as just now, not negative.
        assert_eq!(ago(Some("2026-10-01T13:00:00Z"), n), "just now");
    }
}
