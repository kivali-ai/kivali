//! macOS notifications through UNUserNotificationCenter, so a click can
//! say which notification it was (`notify.rs` routes it).

use super::notify::{center_usable, clicked, TARGET_KEY};
use block2::{DynBlock, RcBlock};
use objc2::rc::Retained;
use objc2::runtime::{AnyObject, Bool, NSObject, NSObjectProtocol, ProtocolObject};
use objc2::{define_class, msg_send, AnyThread};
use objc2_foundation::{NSBundle, NSDictionary, NSError, NSString};
use objc2_user_notifications::{
    UNAuthorizationOptions, UNMutableNotificationContent, UNNotification, UNNotificationDefaultActionIdentifier,
    UNNotificationPresentationOptions, UNNotificationRequest, UNNotificationResponse, UNUserNotificationCenter,
    UNUserNotificationCenterDelegate,
};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::OnceLock;

/// The center, when this process may use it: in an app bundle, and the
/// first call did not throw. Decided once.
fn center() -> Option<Retained<UNUserNotificationCenter>> {
    static USABLE: OnceLock<bool> = OnceLock::new();
    let usable = *USABLE.get_or_init(|| {
        let bundle = NSBundle::mainBundle();
        let id = bundle.bundleIdentifier().map(|s| s.to_string());
        let path = bundle.bundlePath().to_string();
        if !center_usable(id.as_deref(), &path) {
            eprintln!("kivali: not in an app bundle ({path}); notifications use the plugin and clicks are not routed");
            return false;
        }
        match objc2::exception::catch(|| drop(UNUserNotificationCenter::currentNotificationCenter())) {
            Ok(()) => true,
            Err(e) => {
                eprintln!("kivali: UNUserNotificationCenter unavailable: {e:?}");
                false
            }
        }
    });
    usable.then(UNUserNotificationCenter::currentNotificationCenter)
}

define_class!(
    // SAFETY: NSObject has no subclassing requirements, and this class
    // does not implement Drop.
    #[unsafe(super(NSObject))]
    #[name = "KivaliNotificationDelegate"]
    struct NotificationDelegate;

    unsafe impl NSObjectProtocol for NotificationDelegate {}

    // SAFETY: the signatures are the protocol's.
    unsafe impl UNUserNotificationCenterDelegate for NotificationDelegate {
        /// Kivali posts only while it is not frontmost, but one that
        /// arrives just as it comes forward still shows.
        #[unsafe(method(userNotificationCenter:willPresentNotification:withCompletionHandler:))]
        fn will_present(
            &self,
            _center: &UNUserNotificationCenter,
            _notification: &UNNotification,
            done: &DynBlock<dyn Fn(UNNotificationPresentationOptions)>,
        ) {
            done.call((UNNotificationPresentationOptions::Banner | UNNotificationPresentationOptions::List,));
        }

        /// A click on the notification (the default action) opens its
        /// target.
        #[unsafe(method(userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:))]
        fn did_receive(&self, _center: &UNUserNotificationCenter, response: &UNNotificationResponse, done: &DynBlock<dyn Fn()>) {
            // SAFETY: an extern static NSString from UserNotifications.
            let default = unsafe { UNNotificationDefaultActionIdentifier };
            if response.actionIdentifier().isEqualToString(default) {
                let info = response.notification().request().content().userInfo();
                let key = NSString::from_str(TARGET_KEY);
                let key: &AnyObject = &key;
                if let Some(target) = info.objectForKey(key).and_then(|v| v.downcast::<NSString>().ok()) {
                    clicked(&target.to_string());
                }
            }
            done.call(());
        }
    }
);

impl NotificationDelegate {
    fn new() -> Retained<Self> {
        let this = Self::alloc().set_ivars(());
        // SAFETY: NSObject's designated initializer.
        unsafe { msg_send![super(this), init] }
    }
}

/// Makes the center's delegate one that routes clicks, for the life of
/// the process (the center holds its delegate weakly).
pub(super) fn install() {
    let Some(center) = center() else { return };
    let delegate = NotificationDelegate::new();
    center.setDelegate(Some(ProtocolObject::from_ref(&*delegate)));
    std::mem::forget(delegate);
}

/// Posts a notification whose `userInfo` carries `target`. False when
/// the center cannot be used here (the caller falls back to the plugin).
/// The first post asks for permission (alerts only, no sound); macOS
/// asks the person once and answers from their choice afterwards.
pub(super) fn post(title: &str, body: &str, target: &str) -> bool {
    let Some(center) = center() else { return false };
    let (title, body, target) = (title.to_string(), body.to_string(), target.to_string());
    let authorized = RcBlock::new(move |granted: Bool, err: *mut NSError| {
        if !granted.as_bool() {
            // SAFETY: the center passes a valid NSError or null.
            let why = unsafe { err.as_ref() }.map(|e| e.localizedDescription().to_string());
            eprintln!("kivali: notifications not allowed ({}); dropped \"{title}\"", why.unwrap_or_default());
            return;
        }
        let content = UNMutableNotificationContent::new();
        content.setTitle(&NSString::from_str(&title));
        content.setBody(&NSString::from_str(&body));
        let key = NSString::from_str(TARGET_KEY);
        let value = NSString::from_str(&target);
        let info = NSDictionary::<NSString, NSString>::from_slices(&[&*key], &[&*value]);
        // SAFETY: a dictionary of strings is a property list, which is
        // what userInfo must hold; the cast only forgets the generics.
        unsafe { content.setUserInfo(&Retained::cast_unchecked::<NSDictionary>(info)) };
        let request =
            UNNotificationRequest::requestWithIdentifier_content_trigger(&NSString::from_str(&request_id()), &content, None);
        let title = title.clone();
        let added = RcBlock::new(move |err: *mut NSError| {
            // SAFETY: as above.
            if let Some(e) = unsafe { err.as_ref() } {
                eprintln!("kivali: notification \"{title}\" failed: {}", e.localizedDescription());
            }
        });
        UNUserNotificationCenter::currentNotificationCenter().addNotificationRequest_withCompletionHandler(&request, Some(&added));
    });
    center.requestAuthorizationWithOptions_completionHandler(UNAuthorizationOptions::Alert, &authorized);
    true
}

/// A fresh request identifier: one with the same identifier would
/// replace the notification already in Notification Center.
fn request_id() -> String {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    format!("kivali-{}-{}", std::process::id(), NEXT.fetch_add(1, Ordering::Relaxed))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `cargo test` runs a bare binary: the center is never touched, and
    /// posting reports that the plugin must show it.
    #[test]
    fn bare_binary_falls_back() {
        assert!(center().is_none());
        assert!(!post("t", "b", "team:a"));
        install(); // a no-op without the center
    }

    #[test]
    fn request_ids_differ() {
        assert_ne!(request_id(), request_id());
    }

    /// The payload the delegate reads back is the one `post` writes.
    #[test]
    fn user_info_carries_the_target() {
        let key = NSString::from_str(TARGET_KEY);
        let value = NSString::from_str("settings:team/a/ai");
        let typed = NSDictionary::<NSString, NSString>::from_slices(&[&*key], &[&*value]);
        // SAFETY: as in `post`.
        let info = unsafe { Retained::cast_unchecked::<NSDictionary>(typed) };
        let k: &AnyObject = &key;
        let got = info.objectForKey(k).and_then(|v| v.downcast::<NSString>().ok()).map(|s| s.to_string());
        assert_eq!(got.as_deref(), Some("settings:team/a/ai"));
        let _ = NotificationDelegate::new();
    }
}
