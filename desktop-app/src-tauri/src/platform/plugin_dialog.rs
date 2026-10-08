//! [`alert`](super::alert) on Linux, which has neither NSAlert nor
//! TaskDialog: the dialog plugin's message box. At most three buttons,
//! no destructive styling and no suppression checkbox (always unticked).
//! Built on every OS for its tests.
#![cfg_attr(all(test, not(target_os = "linux")), allow(dead_code))]

use super::{cancel_index, AlertAnswer, AlertSpec};
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons, MessageDialogResult};

/// The plugin's buttons for `buttons`: the first three, in order.
pub fn dialog_buttons(buttons: &[String]) -> MessageDialogButtons {
    match buttons {
        [] => MessageDialogButtons::Ok,
        [a] => MessageDialogButtons::OkCustom(a.clone()),
        [a, b] => MessageDialogButtons::OkCancelCustom(a.clone(), b.clone()),
        [a, b, c, ..] => MessageDialogButtons::YesNoCancelCustom(a.clone(), b.clone(), c.clone()),
    }
}

/// The index of the button the plugin's result names.
pub fn answer_index(buttons: &[String], result: &MessageDialogResult) -> usize {
    let shown = &buttons[..buttons.len().min(3)];
    match result {
        MessageDialogResult::Custom(label) => shown.iter().position(|b| b == label).unwrap_or_else(|| cancel_index(shown)),
        MessageDialogResult::Ok | MessageDialogResult::Yes => 0,
        MessageDialogResult::No => 1.min(shown.len().saturating_sub(1)),
        MessageDialogResult::Cancel => cancel_index(shown),
    }
}

pub fn alert(app: &tauri::AppHandle, parent: Option<&tauri::Window>, spec: AlertSpec, done: Box<dyn FnOnce(AlertAnswer) + Send>) {
    let mut d = app.dialog().message(spec.message).title(spec.title).buttons(dialog_buttons(&spec.buttons));
    if let Some(w) = parent {
        d = d.parent(w);
    }
    let buttons = spec.buttons;
    d.show_with_result(move |r| done(AlertAnswer { button: answer_index(&buttons, &r), suppressed: false }));
}

#[cfg(test)]
mod tests {
    use super::*;

    fn b(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn buttons_map_in_order() {
        assert!(matches!(dialog_buttons(&[]), MessageDialogButtons::Ok));
        assert!(matches!(dialog_buttons(&b(&["Pause"])), MessageDialogButtons::OkCustom(a) if a == "Pause"));
        assert!(matches!(dialog_buttons(&b(&["Pause", "Cancel"])), MessageDialogButtons::OkCancelCustom(a, c) if a == "Pause" && c == "Cancel"));
        assert!(matches!(
            dialog_buttons(&b(&["Keep", "Cancel", "Continue", "More"])),
            MessageDialogButtons::YesNoCancelCustom(a, c, d) if a == "Keep" && c == "Cancel" && d == "Continue"
        ));
    }

    #[test]
    fn results_map_to_indices() {
        let three = b(&["Keep Plainsong", "Cancel", "Continue"]);
        assert_eq!(answer_index(&three, &MessageDialogResult::Custom("Continue".into())), 2);
        assert_eq!(answer_index(&three, &MessageDialogResult::Custom("?".into())), 1);
        assert_eq!(answer_index(&three, &MessageDialogResult::Yes), 0);
        assert_eq!(answer_index(&three, &MessageDialogResult::No), 1);
        assert_eq!(answer_index(&three, &MessageDialogResult::Cancel), 1);
        let two = b(&["Quit", "Don't Quit"]);
        assert_eq!(answer_index(&two, &MessageDialogResult::Ok), 0);
        assert_eq!(answer_index(&two, &MessageDialogResult::Cancel), 1);
        assert_eq!(answer_index(&[], &MessageDialogResult::Ok), 0);
    }
}
