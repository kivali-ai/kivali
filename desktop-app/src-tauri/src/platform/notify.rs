//! System notifications and where a click on one goes.
//!
//! macOS posts through UNUserNotificationCenter (`notify_macos.rs`): each
//! notification carries its [`NotifyTarget`] in `userInfo`, and the
//! center's delegate hands a click to the handler registered with
//! [`on_notification_click`]. Where the center cannot be used (a binary
//! outside an app bundle: `cargo test`, `tauri dev`) and on Windows and
//! Linux, the notification plugin shows it instead, and a click only
//! brings Kivali forward (the plugin routes no clicks on desktop).

use std::path::Path;
use std::sync::{Arc, OnceLock};

/// Where a click on a notification goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum NotifyTarget {
    /// The team's window (`windows::show_team`).
    Team(String),
    /// Settings at a route (`windows::show_settings`): `team/<id>` or
    /// `team/<id>/<tab>`.
    Settings(String),
}

impl NotifyTarget {
    /// The string a notification carries: `team:<id>` or
    /// `settings:<route>`.
    pub fn encode(&self) -> String {
        match self {
            NotifyTarget::Team(id) => format!("team:{id}"),
            NotifyTarget::Settings(route) => format!("settings:{route}"),
        }
    }

    /// [`encode`](Self::encode)'s inverse; anything else (another app's
    /// payload, an empty id) is no target.
    pub fn decode(s: &str) -> Option<Self> {
        let (kind, rest) = s.split_once(':')?;
        if rest.is_empty() {
            return None;
        }
        match kind {
            "team" => Some(NotifyTarget::Team(rest.to_string())),
            "settings" => Some(NotifyTarget::Settings(rest.to_string())),
            _ => None,
        }
    }
}

/// The `userInfo` key that holds [`NotifyTarget::encode`].
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
pub(super) const TARGET_KEY: &str = "kivali.target";

/// Whether UNUserNotificationCenter may be asked for: only in an app
/// bundle, with a bundle identifier. Outside one (a bare binary, even
/// with an embedded Info.plist, as `tauri dev` builds it) the center
/// throws on first use.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
pub(super) fn center_usable(bundle_id: Option<&str>, bundle_path: &str) -> bool {
    bundle_id.is_some_and(|b| !b.trim().is_empty()) && Path::new(bundle_path).extension().is_some_and(|e| e == "app")
}

type ClickHandler = Arc<dyn Fn(NotifyTarget) + Send + Sync>;

static CLICKS: OnceLock<(tauri::AppHandle, ClickHandler)> = OnceLock::new();

/// Registers, once at startup, what a click on a notification does.
/// `handler` runs on the main thread. A second registration is ignored.
// Off macOS nothing follows the early return, which clippy then calls
// needless.
#[cfg_attr(not(target_os = "macos"), allow(clippy::needless_return))]
pub fn on_notification_click(app: &tauri::AppHandle, handler: Box<dyn Fn(NotifyTarget) + Send + Sync>) {
    if CLICKS.set((app.clone(), Arc::from(handler))).is_err() {
        eprintln!("kivali: notification click handler already registered");
    } else {
        #[cfg(target_os = "macos")]
        super::notify_macos::install();
    }
}

/// A click on a notification that carried `encoded`: the handler, on
/// the main thread. Unknown payloads, or no handler, do nothing (the
/// system has already brought Kivali forward).
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
pub(super) fn clicked(encoded: &str) {
    let Some(target) = NotifyTarget::decode(encoded) else { return };
    let Some((app, handler)) = CLICKS.get() else { return };
    let handler = handler.clone();
    if let Err(e) = app.run_on_main_thread(move || handler(target)) {
        eprintln!("kivali: cannot route a notification click: {e}");
    }
}

/// A system notification; clicking it opens `target` where
/// the platform routes clicks (macOS in an app bundle), else it only
/// brings Kivali forward. Callers decide whether to send one
/// ([`super::app_is_active`]: only when Kivali is not frontmost).
/// Callable from any thread. The plugin path needs
/// `tauri_plugin_notification::init()` registered.
pub fn notify(app: &tauri::AppHandle, title: &str, body: &str, target: NotifyTarget) {
    #[cfg(target_os = "macos")]
    if super::notify_macos::post(title, body, &target.encode()) {
        return;
    }
    let _ = target;
    plugin_notify(app, title, body);
}

fn plugin_notify(app: &tauri::AppHandle, title: &str, body: &str) {
    use tauri::Manager;
    let Some(n) = app.try_state::<tauri_plugin_notification::Notification<tauri::Wry>>() else {
        eprintln!("kivali: notification plugin not registered; dropped \"{title}\"");
        return;
    };
    if let Err(e) = n.builder().title(title).body(body).show() {
        eprintln!("kivali: notification failed: {e}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn targets_round_trip() {
        for t in [
            NotifyTarget::Team("plainsong".into()),
            NotifyTarget::Settings("team/plainsong".into()),
            NotifyTarget::Settings("team/plainsong/ai".into()),
            NotifyTarget::Team("id:with:colons".into()),
        ] {
            assert_eq!(NotifyTarget::decode(&t.encode()), Some(t));
        }
        assert_eq!(NotifyTarget::Team("a".into()).encode(), "team:a");
        assert_eq!(NotifyTarget::Settings("team/a/ai".into()).encode(), "settings:team/a/ai");
    }

    #[test]
    fn foreign_payloads_have_no_target() {
        for s in ["", "team", "team:", "settings:", "window:a", "Team:a", "nothing"] {
            assert_eq!(NotifyTarget::decode(s), None, "{s:?}");
        }
    }

    #[test]
    fn center_only_in_an_app_bundle() {
        assert!(center_usable(Some("ai.kivali.desktop"), "/Applications/Kivali.app"));
        assert!(center_usable(Some("ai.kivali.desktop"), "/Volumes/Kivali/Kivali.app"));
        // `tauri dev`: an embedded Info.plist names an id, but the main
        // bundle is the binary's directory.
        assert!(!center_usable(Some("ai.kivali.desktop"), "/src/desktop-app/src-tauri/target/debug"));
        // `cargo test`: no identifier.
        assert!(!center_usable(None, "/src/desktop-app/src-tauri/target/debug/deps"));
        assert!(!center_usable(Some(""), "/Applications/Kivali.app"));
    }

    /// Without a handler, a click does nothing (and never panics).
    #[test]
    fn click_without_handler_is_ignored() {
        clicked("team:a");
        clicked("garbage");
    }
}
