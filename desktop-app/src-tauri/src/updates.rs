//! The Kivali release check, as the shell shows it.
//!
//! The supervisor fetches `release.json` and reports a [`CheckReport`];
//! this module turns that, the app's own version and the clock into one
//! [`UpdateView`]: the text on the tray's update item and Settings,
//! whether the tray icon carries a badge, and what the item does. Never a
//! modal (docs/developers/desktop-app.md, "Update states").

use crate::supervisor::wire::{check_status, BakedRelease, CheckReport};
use semver::Version;
use serde::Serialize;
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

/// Parses `v0.16.0`, `0.16.0` or `0.16.0-rc.1`.
pub fn parse_version(s: &str) -> Option<Version> {
    let s = s.trim();
    Version::parse(s.strip_prefix('v').unwrap_or(s)).ok()
}

/// Whether a team runs a development build of Kivali: the dev chart's
/// `0.0.0-dev`, or a bare `dev`. A release, a release candidate and an
/// unknown version are not.
pub fn is_dev_version(s: &str) -> bool {
    let s = s.trim();
    s == "dev" || parse_version(s).is_some_and(|v| v.pre.as_str().split('.').any(|p| p == "dev"))
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
    /// `baked`: the release on offer is the one baked into the team's VM
    /// image (a newer Kivali Desktop brought it), applied with nothing
    /// downloaded; otherwise it is the feed's.
    Available { current: String, latest: String, baked: bool },
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
    UpdateState::Available { current, latest, baked: false }
}

/// Folds the VM image's release into the feed's verdict. A baked Kivali
/// the team can move to (`newer`: a release above the team's release) is
/// on offer whatever the feed said or whether it was reached, since the
/// app that bakes it is by definition new enough for it; only a feed
/// release above it keeps the feed's offer. One whose upgrade needs
/// steps by hand is shown as such instead (`notes_url` from the feed when
/// it names the same release). `current` is the team's version from its
/// status, for when no check has finished.
pub fn with_baked(state: UpdateState, baked: Option<&BakedRelease>, current: Option<&str>, notes_url: Option<&str>) -> UpdateState {
    let Some(b) = baked.filter(|b| b.newer) else { return state };
    let Some(bv) = parse_version(&b.version) else { return state };
    match &state {
        UpdateState::NoLocalOrg => state,
        UpdateState::Available { latest, .. } | UpdateState::ManualSteps { latest, .. } | UpdateState::DesktopFirst { latest, .. }
            if parse_version(latest).is_some_and(|l| l > bv) =>
        {
            state
        }
        _ if b.manual_steps => UpdateState::ManualSteps { latest: b.version.clone(), notes_url: notes_url.map(Into::into) },
        _ => UpdateState::Available { current: current.unwrap_or_default().to_string(), latest: b.version.clone(), baked: true },
    }
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

    // Guards the Restart button's condition: it shows for the dev chart
    // and never for a release, a release candidate or no version.
    #[test]
    fn dev_versions() {
        assert!(is_dev_version("0.0.0-dev"));
        assert!(is_dev_version(" dev "));
        assert!(is_dev_version("1.3.0-dev.2"));
        // A development build of the server names its commit, and a dirty
        // tree's time, as build metadata (the Makefile's BUILD_VERSION).
        assert!(is_dev_version("0.0.0-dev+g2b703bf3c1e4"));
        assert!(is_dev_version("0.0.0-dev+g2b703bf3c1e4.dirty.20261027T153000Z"));
        assert!(!is_dev_version("1.2.2"));
        assert!(!is_dev_version("0.17.0-rc.2"));
        assert!(!is_dev_version(""));
        assert!(!is_dev_version("devel"));
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

    fn baked(version: &str, newer: bool) -> BakedRelease {
        BakedRelease { version: version.into(), newer, manual_steps: false }
    }

    // Guards the first-boot update's offer: the VM image's release
    // stands in for the feed's whether the feed was reached, failed or
    // named something older, and steps aside only for a feed release
    // above it. A baked Kivali the supervisor judged not newer (a dev
    // build on either side, the same or an older version) changes
    // nothing.
    #[test]
    fn baked_release_is_offered_over_the_feed() {
        let b = baked("0.17.0", true);
        let offered = |s: UpdateState| matches!(s, UpdateState::Available { ref latest, baked: true, .. } if latest == "0.17.0");
        assert!(offered(with_baked(decide(None, true, "0.17.0"), Some(&b), Some("0.16.0"), None)));
        assert!(offered(with_baked(decide(Some(&report("0.16.0", Some("0.16.0"))), true, "0.17.0"), Some(&b), Some("0.16.0"), None)));
        let mut failed = report("0.16.0", None);
        failed.error = Some("timed out".into());
        assert!(offered(with_baked(decide(Some(&failed), true, "0.17.0"), Some(&b), Some("0.16.0"), None)));
        let mut manual = report("0.16.0", Some("0.17.0"));
        manual.manual_steps = true;
        assert!(offered(with_baked(decide(Some(&manual), true, "0.17.0"), Some(&b), Some("0.16.0"), None)));
        assert_eq!(
            with_baked(decide(None, true, "0.17.0"), Some(&b), Some("0.16.0"), None),
            UpdateState::Available { current: "0.16.0".into(), latest: "0.17.0".into(), baked: true }
        );
        // The feed's own offer of the same version becomes the baked one.
        assert!(offered(with_baked(decide(Some(&report("0.16.0", Some("0.17.0"))), true, "0.17.0"), Some(&b), Some("0.16.0"), None)));
        // A feed release above the baked one keeps the feed's offer, as
        // does a feed release above it that needs steps by hand.
        assert_eq!(
            with_baked(decide(Some(&report("0.16.0", Some("0.17.1"))), true, "0.17.0"), Some(&b), Some("0.16.0"), None),
            UpdateState::Available { current: "0.16.0".into(), latest: "0.17.1".into(), baked: false }
        );
        let mut manual_above = report("0.16.0", Some("0.17.1"));
        manual_above.manual_steps = true;
        assert!(matches!(with_baked(decide(Some(&manual_above), true, "0.17.0"), Some(&b), Some("0.16.0"), None), UpdateState::ManualSteps { ref latest, .. } if latest == "0.17.1"));
        // A feed release above it that needs a newer app keeps that
        // message too: the next app brings it on its first start.
        let mut app_first = report("0.16.0", Some("0.18.0"));
        app_first.min_desktop = Some("0.18.0".into());
        assert!(matches!(with_baked(decide(Some(&app_first), true, "0.17.0"), Some(&b), Some("0.16.0"), None), UpdateState::DesktopFirst { ref latest, .. } if latest == "0.18.0"));
        // One at or below the baked release gives way to it.
        let mut app_first_below = report("0.16.0", Some("0.17.0"));
        app_first_below.min_desktop = Some("0.17.0".into());
        assert!(offered(with_baked(decide(Some(&app_first_below), true, "0.16.9"), Some(&b), Some("0.16.0"), None)));
        // A baked release whose upgrade needs steps by hand is shown as
        // such, never offered; the feed's notes attach when it names it.
        let by_hand = BakedRelease { manual_steps: true, ..baked("0.17.0", true) };
        assert_eq!(
            with_baked(decide(None, true, "0.17.0"), Some(&by_hand), Some("0.16.0"), Some("https://notes")),
            UpdateState::ManualSteps { latest: "0.17.0".into(), notes_url: Some("https://notes".into()) }
        );
        // Not newer, or no verdict yet: the feed's state stands.
        let up = decide(Some(&report("0.16.0", Some("0.16.0"))), true, "0.16.0");
        assert_eq!(with_baked(up.clone(), Some(&baked("0.16.0", false)), Some("0.16.0"), None), up);
        assert_eq!(with_baked(up.clone(), None, Some("0.16.0"), None), up);
        assert_eq!(with_baked(UpdateState::NoLocalOrg, Some(&b), None, None), UpdateState::NoLocalOrg);
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
