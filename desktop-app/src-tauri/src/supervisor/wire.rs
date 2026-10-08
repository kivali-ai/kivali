//! The supervisor's RPC, as `internal/supervisor/rpc.go` serves it.
//!
//! HTTP/1.1 over the owner-only endpoint `crate::platform::endpoint`
//! gives (the Unix socket `<config dir>/supervisor.sock`, or the named
//! pipe `\\.\pipe\kivali-<hash>` on Windows):
//!
//! | Request | Body | Answer |
//! | --- | --- | --- |
//! | `GET /v1/status` | | a [`Report`] |
//! | `POST /v1/up` | [`UpOptions`] | NDJSON [`Event`]s; result: a [`Report`] |
//! | `POST /v1/down` | [`DownRequest`] | NDJSON; no result |
//! | `POST /v1/install` | [`InstallOptions`] | NDJSON; no result |
//! | `POST /v1/check` | [`CheckRequest`] | NDJSON; result: a [`CheckResult`] |
//! | `POST /v1/upgrade` | [`UpgradeOptions`] | NDJSON; result: a [`Report`] |
//! | `GET /v1/credential` | | a [`CredentialStatus`]; 409 when the org is not running |
//! | `POST /v1/destroy` | [`DestroyRequest`] | NDJSON; result: a [`DestroyResult`] |
//! | `POST /v1/handoff` | `{}` | a [`HandoffResult`]; 409 when the org is not running |
//!
//! An operation's answer is `200` with one JSON object per line: log
//! lines `{"log": "..."}`, then exactly one final line with `"done":
//! true` and either `"error"` or `"result"`. Backup and restore (the app's
//! own zip, through its endpoints) and the terminal (an upgraded PTY stream) are used
//! through the `kivali-supervisor` command instead (see `mod.rs`).
//!
//! Only the fields the shell reads are declared; serde ignores the rest,
//! so the supervisor can grow without breaking the shell. This file is
//! shared by path with the fake supervisor, so it depends on serde alone.

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

/// One line of an operation's NDJSON answer.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Event {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub log: Option<String>,
    /// The operation's progress stage when the line was logged
    /// ([`stage`]); absent for operations without stages.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stage: Option<String>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub done: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub result: Option<serde_json::Value>,
}

fn is_false(b: &bool) -> bool {
    !*b
}

fn is_zero(n: &u16) -> bool {
    *n == 0
}

fn is_zero_u64(n: &u64) -> bool {
    *n == 0
}

fn is_zero_u32(n: &u32) -> bool {
    *n == 0
}

/// The progress stages an operation's lines carry (`Event::stage`).
pub mod stage {
    /// `up` on a fresh disk: creating it, its first boot to READY.
    pub const MAKING_ROOM: &str = "making-room";
    /// Booting to READY, the guest agent, the forward; the upgrade's wait
    /// for the new release.
    pub const STARTING: &str = "starting";
    /// Install, rollout and `/readyz`.
    pub const SETTING_UP: &str = "setting-up";
    /// Upgrade: preflight, fetch, verify, import.
    pub const DOWNLOADING: &str = "downloading";
    /// Upgrade: journal, quiesce, stop, snapshot.
    pub const SNAPSHOT: &str = "snapshot";
    /// Upgrade: boot, the new chart, the HelmChart rewrite.
    pub const INSTALLING: &str = "installing";
    pub const ROLLING_BACK: &str = "rolling-back";
    /// `down`.
    pub const PAUSING: &str = "pausing";
    /// `destroy`.
    pub const DELETING: &str = "deleting";
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct InstallOptions {
    /// The Google account that owns the org (OWNER_EMAILS); required on
    /// the first `up`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub owner: String,
    /// Extra server environment, `NAME=VALUE`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct UpOptions {
    pub install: InstallOptions,
    /// Recorded and never moved: `up` fails if another program holds it.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub port: u16,
    /// The first port of an org with none recorded yet; moved to a free
    /// one if busy, ignored once the org has a port.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub port_hint: u16,
    #[serde(default, skip_serializing_if = "is_zero_u64")]
    pub memory_mb: u64,
    #[serde(default, skip_serializing_if = "is_zero_u32")]
    pub cpus: u32,
    /// Boot and forward only; install on a later `up` (a new team whose
    /// owner setup does not know yet).
    #[serde(default, skip_serializing_if = "is_false")]
    pub prepare: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DownRequest {
    /// Also ends `serve` once the VM is down.
    #[serde(default, skip_serializing_if = "is_false")]
    pub exit: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct CheckRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub feed: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct UpgradeOptions {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub feed: String,
}

/// `GET /v1/status`, and the result of `up` and `upgrade`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Report {
    /// The VM is running.
    #[serde(default)]
    pub running: bool,
    /// The host side of the port forward, `127.0.0.1:8080`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub forward: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub guest: Option<GuestStatus>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub guest_error: Option<String>,
    #[serde(default)]
    pub state: LocalState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub supervisor_version: Option<String>,
    /// An operation is running.
    #[serde(default)]
    pub busy: bool,
    /// Open terminal sessions (the one running `claude` login, closing,
    /// drops this).
    #[serde(default)]
    pub terminals: u32,
    /// The data disk's allocated bytes and logical size; 0 with no disk.
    #[serde(default)]
    pub disk_used_bytes: u64,
    #[serde(default)]
    pub disk_size_bytes: u64,
}

/// `GET /v1/credential` (409 with a sentence when the org is not
/// running): how the team's Claude CLI is signed in, as `claude auth
/// status` in the server container reports it.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct CredentialStatus {
    #[serde(default)]
    pub signed_in: bool,
    /// The signed-in account, when the CLI reports one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub email: Option<String>,
    /// What model calls are billed to: "Claude Max", "Anthropic
    /// Console", "Amazon Bedrock", "Google Vertex AI"; after a sign-in
    /// setup, "<provider> · <target>".
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub billing: Option<String>,
    /// RFC 3339.
    #[serde(default)]
    pub checked_at: String,
}

/// `GET /v1/credential/setup`: the model ids the team runs the CLI with
/// (what a setup's form names), and the sign-in setup saved now, if any.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SetupInfo {
    #[serde(default)]
    pub models: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub current: Option<SavedSetup>,
}

/// A saved sign-in setup: its id and its values, without secrets.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SavedSetup {
    pub setup: String,
    #[serde(default)]
    pub values: BTreeMap<String, String>,
}

/// The body of `POST /v1/credential/setup`: a sign-in setup the Claude
/// driver declares, by id, and its values. Values carry secrets: its
/// `Debug` shows only the setup and the value names.
#[derive(Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SetupRequest {
    pub setup: String,
    #[serde(default)]
    pub values: BTreeMap<String, String>,
}

impl std::fmt::Debug for SetupRequest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SetupRequest").field("setup", &self.setup).field("values", &self.values.keys().collect::<Vec<_>>()).finish()
    }
}

/// One model's answer after a setup: `missing` when the provider has no
/// model by that name, otherwise `problem` is why it did not answer.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ModelCheck {
    pub model: String,
    #[serde(default)]
    pub ok: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub missing: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub problem: Option<String>,
}

/// `POST /v1/credential/setup`'s answer: the sign-in the CLI reports
/// after the setup, and each model's check.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SetupResult {
    pub credential: CredentialStatus,
    #[serde(default)]
    pub models: Vec<ModelCheck>,
}

/// `POST /v1/credential/clear-provider`'s answer: the cloud provider
/// variables removed from the CLI's settings.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ClearResult {
    #[serde(default)]
    pub cleared: Vec<String>,
}

/// The body of `POST /v1/address`: the team's external https origin
/// (the chart's externalURL), or empty for none.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct AddressRequest {
    #[serde(default)]
    pub external_url: String,
}

/// `POST /v1/handoff`: a one-time token that signs the team's owner in
/// at `<origin>/auth/handoff?t=<token>` for two minutes (docs/developers/auth.md,
/// Desktop handoff). A secret: its `Debug` redacts it.
#[derive(Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct HandoffResult {
    #[serde(default)]
    pub token: String,
}

impl std::fmt::Debug for HandoffResult {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HandoffResult").field("token", &"<redacted>").finish()
    }
}

/// The body of `POST /v1/destroy`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DestroyRequest {
    /// Also ends `serve` once the org is deleted.
    #[serde(default, skip_serializing_if = "is_false")]
    pub exit: bool,
}

/// The result of `POST /v1/destroy`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct DestroyResult {
    /// What the deleted files occupied on disk.
    #[serde(default)]
    pub freed_bytes: u64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct GuestStatus {
    /// booting, ready or fatal.
    #[serde(default)]
    pub state: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fatal: Option<String>,
    #[serde(default)]
    pub node_ready: bool,
}

/// local.json, the supervisor's record (the parts the shell reads).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct LocalState {
    /// The installed Kivali version, "" until installed.
    #[serde(default)]
    pub kivali: String,
    #[serde(default)]
    pub data_disk_formatted: bool,
    #[serde(default)]
    pub port: u16,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_check: Option<String>,
    #[serde(default)]
    pub last_check_ok: bool,
    /// The last successful check.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latest: Option<CheckResult>,
    /// The upgrade journal; present while an upgrade is under way.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub upgrade: Option<serde_json::Value>,
}

/// The supervisor's verdicts on a check (`release.go`).
pub mod check_status {
    pub const UP_TO_DATE: &str = "up-to-date";
    pub const UPGRADE: &str = "upgrade-available";
    pub const APP_TOO_OLD: &str = "desktop-update-required";
    pub const MANUAL_STEPS: &str = "manual-steps";
    pub const FAILED: &str = "failed";
    pub const NOT_INSTALLED: &str = "not-installed";
}

/// The result of `POST /v1/check`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct CheckResult {
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub current: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latest: Option<String>,
    #[serde(default)]
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub notes_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub checked_at: Option<String>,
    /// release.json's minimum Kivali Desktop version, passed through so
    /// the shell can compare it with its own version.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub min_desktop_version: Option<String>,
}

// ---- What the rest of the shell sees ----

/// A step of a long operation, as the progress page lists it.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Progress {
    pub label: String,
    /// The supervisor's stage id for the line (`stage`), when it gave one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stage: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OrgState {
    /// No local org on this computer.
    Absent,
    Stopped,
    Starting,
    Running,
    Upgrading,
    Failed,
}

/// The local org, reduced from a [`Report`].
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Status {
    pub state: OrgState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub port: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kivali: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

/// A release check, reduced from a [`CheckResult`] (plus the time of the
/// last successful check, which lives in the status report).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct CheckReport {
    /// The supervisor's verdict (`check_status`), when it gave one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub status: Option<String>,
    #[serde(default)]
    pub current: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latest: Option<String>,
    /// release.json's minimum desktop version, when known to the shell.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub min_desktop: Option<String>,
    #[serde(default)]
    pub manual_steps: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub notes_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub checked_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_ok_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

fn port_of(addr: &str) -> Option<u16> {
    addr.rsplit_once(':').and_then(|(_, p)| p.parse().ok())
}

fn non_empty(s: &str) -> Option<String> {
    let s = s.trim();
    (!s.is_empty()).then(|| s.to_string())
}

impl Status {
    pub fn from_report(r: &Report) -> Status {
        let port = r
            .forward
            .as_deref()
            .and_then(port_of)
            .or((r.state.port != 0).then_some(r.state.port));
        let kivali = non_empty(&r.state.kivali);
        let fatal = r.guest.as_ref().and_then(|g| g.fatal.clone()).filter(|f| !f.is_empty());
        let (state, message) = if !r.running {
            if kivali.is_none() && !r.state.data_disk_formatted && r.state.upgrade.is_none() {
                (OrgState::Absent, None)
            } else {
                (OrgState::Stopped, None)
            }
        } else if let Some(f) = fatal {
            (OrgState::Failed, Some(f))
        } else if r.state.upgrade.is_some() {
            (OrgState::Upgrading, None)
        } else if r.guest.as_ref().is_some_and(|g| g.state == "ready") && kivali.is_some() && r.forward.is_some() {
            // `busy` is not consulted: a backup or a check in flight
            // leaves the org running.
            (OrgState::Running, None)
        } else {
            (OrgState::Starting, r.guest_error.clone())
        };
        Status { state, port, kivali, message }
    }
}

impl CheckReport {
    pub fn from_result(c: &CheckResult, last_ok_at: Option<String>) -> CheckReport {
        let failed = c.status == check_status::FAILED;
        CheckReport {
            status: non_empty(&c.status),
            current: c.current.clone(),
            latest: c.latest.as_deref().and_then(non_empty),
            min_desktop: c.min_desktop_version.as_deref().and_then(non_empty),
            manual_steps: c.status == check_status::MANUAL_STEPS,
            notes_url: c.notes_url.as_deref().and_then(non_empty),
            checked_at: c.checked_at.clone(),
            last_ok_at: if failed { last_ok_at } else { c.checked_at.clone() },
            error: failed.then(|| non_empty(&c.message).unwrap_or_else(|| "the check failed".into())),
        }
    }
}

/// One NDJSON line.
pub fn parse_event(line: &str) -> Result<Event, String> {
    serde_json::from_str(line.trim_end_matches(['\r', '\n'])).map_err(|e| format!("bad event {line:?}: {e}"))
}

pub fn encode_event(ev: &Event) -> String {
    let mut s = serde_json::to_string(ev).expect("events serialise");
    s.push('\n');
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    // The same JSON internal/supervisor's TestWireShapes asserts the Go
    // side emits and reads.
    #[test]
    fn desktop_additions_match_the_supervisor() {
        let ev = parse_event(r#"{"log":"creating the data disk","stage":"making-room"}"#).unwrap();
        assert_eq!(ev.stage.as_deref(), Some(stage::MAKING_ROOM));
        assert_eq!(parse_event(r#"{"done":true}"#).unwrap().stage, None);

        let r: Report = serde_json::from_str(
            r#"{"running":true,"state":{},"busy":false,"terminals":1,"disk_used_bytes":3221225472,"disk_size_bytes":68719476736}"#,
        )
        .unwrap();
        assert_eq!((r.terminals, r.disk_used_bytes, r.disk_size_bytes), (1, 3 << 30, 64 << 30));

        let up = UpOptions { port_hint: 18081, memory_mb: 6144, cpus: 2, ..Default::default() };
        assert_eq!(serde_json::to_string(&up).unwrap(), r#"{"install":{},"port_hint":18081,"memory_mb":6144,"cpus":2}"#);
        let env = InstallOptions { env: vec!["A=b".into()], ..Default::default() };
        assert_eq!(serde_json::to_string(&env).unwrap(), r#"{"env":["A=b"]}"#);

        let c: CredentialStatus = serde_json::from_str(
            r#"{"signed_in":true,"email":"owner@example.com","billing":"Claude Max","checked_at":"2026-10-01T12:00:00Z"}"#,
        )
        .unwrap();
        assert_eq!((c.signed_in, c.email.as_deref(), c.billing.as_deref()), (true, Some("owner@example.com"), Some("Claude Max")));
        let c: CredentialStatus = serde_json::from_str(r#"{"signed_in":false,"checked_at":"2026-10-01T12:00:00Z"}"#).unwrap();
        assert_eq!((c.signed_in, c.email, c.billing), (false, None, None));
        assert_eq!(
            serde_json::to_string(&CredentialStatus { signed_in: true, billing: Some("Amazon Bedrock".into()), checked_at: "t".into(), ..Default::default() }).unwrap(),
            r#"{"signed_in":true,"billing":"Amazon Bedrock","checked_at":"t"}"#
        );
        assert_eq!(serde_json::to_string(&DestroyRequest { exit: true }).unwrap(), r#"{"exit":true}"#);
        let d: DestroyResult = serde_json::from_str(r#"{"freed_bytes":3221225472}"#).unwrap();
        assert_eq!(d.freed_bytes, 3 << 30);
        let h: HandoffResult = serde_json::from_str(r#"{"token":"p.s"}"#).unwrap();
        assert_eq!(h.token, "p.s");
        assert!(!format!("{h:?}").contains("p.s"));
    }

    // The same JSON internal/supervisor's TestSetupWireShapes asserts.
    #[test]
    fn credential_setup_matches_the_supervisor() {
        let i: SetupInfo = serde_json::from_str(r#"{"models":["claude-haiku-4-5"],"current":{"setup":"s","values":{"resource":"r"}}}"#).unwrap();
        assert_eq!(i.models, vec!["claude-haiku-4-5".to_string()]);
        let cur = i.current.unwrap();
        assert_eq!((cur.setup.as_str(), cur.values.get("resource").map(String::as_str)), ("s", Some("r")));
        assert_eq!(serde_json::from_str::<SetupInfo>(r#"{"models":[]}"#).unwrap().current, None);

        let r: SetupResult = serde_json::from_str(
            r#"{"credential":{"signed_in":true,"billing":"P · r","checked_at":"2026-10-01T12:00:00Z"},"models":[{"model":"a","ok":true},{"model":"b","ok":false,"missing":true,"problem":"No b in r"},{"model":"c","ok":false,"problem":"refused"}]}"#,
        )
        .unwrap();
        assert_eq!(r.credential.billing.as_deref(), Some("P · r"));
        assert_eq!(r.models[0], ModelCheck { model: "a".into(), ok: true, ..Default::default() });
        assert_eq!(r.models[1], ModelCheck { model: "b".into(), ok: false, missing: true, problem: Some("No b in r".into()) });
        assert_eq!(r.models[2].problem.as_deref(), Some("refused"));

        let req = SetupRequest { setup: "s".into(), values: [("a".to_string(), "1".to_string()), ("secret".to_string(), "s3cret".to_string())].into() };
        assert_eq!(serde_json::to_string(&req).unwrap(), r#"{"setup":"s","values":{"a":"1","secret":"s3cret"}}"#);
        assert!(!format!("{req:?}").contains("s3cret"));

        let c: ClearResult = serde_json::from_str(r#"{"cleared":["X"]}"#).unwrap();
        assert_eq!(c.cleared, vec!["X".to_string()]);
    }
}
