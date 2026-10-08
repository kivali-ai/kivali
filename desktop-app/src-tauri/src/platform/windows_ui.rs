//! What the Windows alerts and the team window menu's keys mean, kept
//! pure so they are tested on every OS (the calls themselves are in
//! `windows.rs`): TaskDialog's answer as an [`AlertAnswer`], and a key
//! press as the menus spell it ("Ctrl+Shift+N").
#![cfg_attr(not(windows), allow(dead_code))]

use super::{cancel_index, AlertAnswer};

/// TaskDialog's id for an alert's first button; the others follow in
/// order. Clear of the common ids (`IDOK` 1 to `IDCONTINUE` 11).
pub const FIRST_BUTTON_ID: i32 = 100;
/// `IDOK`: the lone OK of an alert that names no buttons.
pub const ID_OK: i32 = 1;
/// `IDCANCEL`: Escape, Alt+F4 or the close box
/// (`TDF_ALLOW_DIALOG_CANCELLATION`). Named for the tests: like any id
/// that is not a button's, it answers Cancel.
#[cfg_attr(not(test), allow(dead_code))]
pub const ID_CANCEL: i32 = 2;

/// The answer `TaskDialogIndirect` gave: `pressed` is the id that closed
/// it (`FIRST_BUTTON_ID + i` for button `i`, [`ID_CANCEL`] for a
/// dismissal, [`ID_OK`] for the lone OK, 0 when the dialog failed) and
/// `checked` the verification checkbox. Anything but one of the alert's
/// own buttons answers its Cancel button ([`cancel_index`]), as an NSAlert
/// closed some other way does; the checkbox counts only when the alert
/// shows one.
pub fn task_dialog_answer(buttons: &[String], pressed: i32, checked: bool, has_checkbox: bool) -> AlertAnswer {
    let i = pressed - FIRST_BUTTON_ID;
    let button = if (0..buttons.len() as i32).contains(&i) { i as usize } else { cancel_index(buttons) };
    AlertAnswer { button, suppressed: has_checkbox && checked }
}

// Virtual-key codes (winuser.h) the menus' keys use beyond 0-9 and A-Z.
pub const VK_ADD: u32 = 0x6B;
pub const VK_SUBTRACT: u32 = 0x6D;
pub const VK_OEM_PLUS: u32 = 0xBB;
pub const VK_OEM_COMMA: u32 = 0xBC;
pub const VK_OEM_MINUS: u32 = 0xBD;

/// A key press as the menus spell an accelerator: the modifiers held, in
/// the order Ctrl, Shift, Alt, then the key ("Ctrl+R", "Ctrl+Shift+N",
/// "Ctrl+=", "F5"). The keypad's digits, plus and minus count as the main
/// row's. None for a key no menu shortcut uses (a modifier alone, arrows,
/// punctuation other than `=`, `-` and `,`).
pub fn chord(vk: u32, ctrl: bool, shift: bool, alt: bool) -> Option<String> {
    let key = match vk {
        0x30..=0x39 | 0x41..=0x5A => char::from(vk as u8).to_string(),
        0x60..=0x69 => char::from(b'0' + (vk - 0x60) as u8).to_string(),
        0x70..=0x87 => format!("F{}", vk - 0x6F),
        VK_OEM_PLUS | VK_ADD => "=".into(),
        VK_OEM_MINUS | VK_SUBTRACT => "-".into(),
        VK_OEM_COMMA => ",".into(),
        _ => return None,
    };
    let mut s = String::new();
    for (held, name) in [(ctrl, "Ctrl+"), (shift, "Shift+"), (alt, "Alt+")] {
        if held {
            s.push_str(name);
        }
    }
    s.push_str(&key);
    Some(s)
}

/// The key presses that make `chord` (the spelling [`chord`] gives):
/// whether Ctrl, Shift and Alt are held, and the key's virtual-key code
/// (the main row's, for the keys the keypad repeats).
pub fn keys(chord: &str) -> Option<(bool, bool, bool, u16)> {
    let (mut ctrl, mut shift, mut alt) = (false, false, false);
    let mut rest = chord;
    loop {
        if let Some(r) = rest.strip_prefix("Ctrl+") {
            ctrl = true;
            rest = r;
        } else if let Some(r) = rest.strip_prefix("Shift+") {
            shift = true;
            rest = r;
        } else if let Some(r) = rest.strip_prefix("Alt+") {
            alt = true;
            rest = r;
        } else {
            break;
        }
    }
    let vk = match rest.as_bytes() {
        [c @ (b'0'..=b'9' | b'A'..=b'Z')] => u32::from(*c),
        [b'='] => VK_OEM_PLUS,
        [b'-'] => VK_OEM_MINUS,
        [b','] => VK_OEM_COMMA,
        [b'F', n @ ..] => match std::str::from_utf8(n).ok()?.parse::<u32>().ok()? {
            n @ 1..=24 => 0x6F + n,
            _ => return None,
        },
        _ => return None,
    };
    Some((ctrl, shift, alt, vk as u16))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn b(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    /// Ids from 100 name the buttons in order; a dismissal (IDCANCEL), the
    /// lone OK, a failure (0) and an id out of range answer Cancel, as
    /// `cancel_answers_dismissal` has it; the checkbox only where shown.
    #[test]
    fn task_dialog_answers_map_to_buttons() {
        let three = b(&["Keep Plainsong", "Cancel", "Continue"]);
        let a = |pressed, checked| task_dialog_answer(&three, pressed, checked, false);
        assert_eq!(a(100, false), AlertAnswer { button: 0, suppressed: false });
        assert_eq!(a(102, false).button, 2);
        assert_eq!(a(ID_CANCEL, false).button, 1);
        assert_eq!(a(103, false).button, 1);
        assert_eq!(a(0, false).button, 1);
        assert_eq!(a(99, false).button, 1);
        assert!(!a(100, true).suppressed, "no checkbox shown");

        let quit = b(&["Quit", "Don't Quit"]);
        assert_eq!(task_dialog_answer(&quit, ID_CANCEL, true, true), AlertAnswer { button: 1, suppressed: true });
        assert_eq!(task_dialog_answer(&quit, 100, true, true), AlertAnswer { button: 0, suppressed: true });
        assert_eq!(task_dialog_answer(&quit, 100, false, true), AlertAnswer { button: 0, suppressed: false });

        // No buttons: a lone OK, which is button 0, however it closes.
        assert_eq!(task_dialog_answer(&[], ID_OK, false, false).button, 0);
        assert_eq!(task_dialog_answer(&[], ID_CANCEL, false, false).button, 0);
    }

    #[test]
    fn key_presses_spell_like_the_menus() {
        assert_eq!(chord(0x52, true, false, false).as_deref(), Some("Ctrl+R"));
        assert_eq!(chord(0x4E, true, true, false).as_deref(), Some("Ctrl+Shift+N"));
        assert_eq!(chord(0x42, true, true, false).as_deref(), Some("Ctrl+Shift+B"));
        assert_eq!(chord(VK_OEM_COMMA, true, false, false).as_deref(), Some("Ctrl+,"));
        assert_eq!(chord(VK_OEM_PLUS, true, false, false).as_deref(), Some("Ctrl+="));
        assert_eq!(chord(VK_OEM_PLUS, true, true, false).as_deref(), Some("Ctrl+Shift+="));
        assert_eq!(chord(VK_ADD, true, false, false).as_deref(), Some("Ctrl+="));
        assert_eq!(chord(VK_OEM_MINUS, true, false, false).as_deref(), Some("Ctrl+-"));
        assert_eq!(chord(VK_SUBTRACT, true, false, false).as_deref(), Some("Ctrl+-"));
        assert_eq!(chord(0x30, true, false, false).as_deref(), Some("Ctrl+0"));
        assert_eq!(chord(0x60, true, false, false).as_deref(), Some("Ctrl+0"));
        assert_eq!(chord(0x74, false, false, false).as_deref(), Some("F5"));
        assert_eq!(chord(0x46, false, false, true).as_deref(), Some("Alt+F"));
        assert_eq!(chord(0x51, true, true, true).as_deref(), Some("Ctrl+Shift+Alt+Q"));
        // Modifiers alone, arrows, other punctuation: no shortcut.
        for vk in [0x10, 0x11, 0x12, 0x25, 0xBA, 0xBE] {
            assert_eq!(chord(vk, true, false, false), None, "{vk:#x}");
        }
    }

    #[test]
    fn chords_round_trip_to_key_presses() {
        for c in ["Ctrl+C", "Ctrl+Shift+N", "Ctrl+,", "Ctrl+=", "Ctrl+-", "Ctrl+0", "F5", "Alt+F", "Ctrl+Shift+Alt+F24"] {
            let (ctrl, shift, alt, vk) = keys(c).unwrap_or_else(|| panic!("{c}"));
            assert_eq!(chord(u32::from(vk), ctrl, shift, alt).as_deref(), Some(c));
        }
        assert_eq!(keys("Ctrl+C"), Some((true, false, false, 0x43)));
        for bad in ["", "Ctrl+", "Ctrl+Up", "Cmd+C", "Ctrl+c", "F0", "F25", "Ctrl+CC"] {
            assert_eq!(keys(bad), None, "{bad}");
        }
    }
}
