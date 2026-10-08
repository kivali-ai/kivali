//! The tray icon's picture and the status dots of native menu items.
//!
//! [`TrayIconDriver`] shows one of four states in the ink the menu bar
//! needs ([`platform::menu_bar_dark`], asked again on every change and
//! every frame), and animates Starting: one timer thread swaps the pulse
//! frames ([`platform::TRAY_FRAMES`] of [`platform::TRAY_FRAME_MS`]) while
//! the state is Starting, and ends when it is not. Under Reduce motion
//! Starting holds at 50%. Every tray call runs on the main thread.

use crate::platform::{self, TrayImages, TRAY_FRAMES, TRAY_FRAME_MS};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tauri::image::Image;
use tauri::AppHandle;

/// A team's state, and the tray's.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TrayState {
    Running,
    Starting,
    Paused,
    CouldntStart,
}

impl TrayState {
    /// The tray's state for several teams: the first that applies of
    /// couldn't start, starting, running, paused. No teams is Paused.
    pub fn of_teams(states: impl IntoIterator<Item = TrayState>) -> TrayState {
        let rank = |s: &TrayState| match s {
            TrayState::CouldntStart => 0,
            TrayState::Starting => 1,
            TrayState::Running => 2,
            TrayState::Paused => 3,
        };
        states.into_iter().min_by_key(rank).unwrap_or(TrayState::Paused)
    }
}

/// The PNG for `state` in the given ink. `frame` picks the starting
/// pulse's frame; `reduce_motion` holds Starting at 50%.
fn picture(state: TrayState, frame: usize, dark: bool, reduce_motion: bool) -> &'static [u8] {
    let set: &'static TrayImages = if dark { &platform::TRAY_ICONS.dark } else { &platform::TRAY_ICONS.light };
    match state {
        TrayState::Running => set.running,
        TrayState::Paused => set.paused,
        TrayState::CouldntStart => set.couldnt_start,
        TrayState::Starting if reduce_motion => set.starting_held,
        TrayState::Starting => set.starting[frame % TRAY_FRAMES],
    }
}

struct Current {
    state: TrayState,
    frame: usize,
    /// Whether the timer thread is running.
    ticking: bool,
}

/// Drives the tray icon `tray_id` (created elsewhere, e.g. by
/// `TrayIconBuilder::with_id`). Cheap to call from any thread.
pub struct TrayIconDriver {
    app: AppHandle,
    tray_id: &'static str,
    current: Arc<Mutex<Current>>,
}

impl TrayIconDriver {
    pub fn new(app: AppHandle, tray_id: &'static str) -> Self {
        let current = Current { state: TrayState::Paused, frame: 0, ticking: false };
        TrayIconDriver { app, tray_id, current: Arc::new(Mutex::new(current)) }
    }

    /// Shows `state` with `tooltip`. Setting Starting again keeps the
    /// pulse where it is.
    pub fn set(&self, state: TrayState, tooltip: &str) {
        let Ok(mut cur) = self.current.lock() else { return };
        if cur.state != state {
            cur.frame = 0;
        }
        cur.state = state;
        apply(&self.app, self.tray_id, state, cur.frame, Some(tooltip.to_string()));
        if state == TrayState::Starting && !cur.ticking && !platform::reduce_motion() {
            cur.ticking = true;
            let (app, id, current) = (self.app.clone(), self.tray_id, self.current.clone());
            std::thread::spawn(move || tick(app, id, current));
        }
    }
}

/// The timer thread: the next frame every TRAY_FRAME_MS while Starting.
/// Reduce motion turning on stops it at the held frame.
fn tick(app: AppHandle, id: &'static str, current: Arc<Mutex<Current>>) {
    loop {
        std::thread::sleep(Duration::from_millis(TRAY_FRAME_MS));
        let Ok(mut cur) = current.lock() else { return };
        if cur.state != TrayState::Starting {
            cur.ticking = false;
            return;
        }
        if platform::reduce_motion() {
            cur.ticking = false;
            apply(&app, id, cur.state, cur.frame, None);
            return;
        }
        cur.frame = (cur.frame + 1) % TRAY_FRAMES;
        apply(&app, id, cur.state, cur.frame, None);
    }
}

/// Sets the picture (and the tooltip, when given) on the main thread,
/// choosing the ink there.
fn apply(app: &AppHandle, id: &'static str, state: TrayState, frame: usize, tooltip: Option<String>) {
    let handle = app.clone();
    let _ = app.run_on_main_thread(move || {
        let Some(tray) = handle.tray_by_id(id) else { return };
        let bytes = picture(state, frame, platform::menu_bar_dark(), platform::reduce_motion());
        if let Ok(img) = Image::from_bytes(bytes) {
            let _ = tray.set_icon(Some(img));
        }
        let _ = tray.set_icon_as_template(false);
        if let Some(t) = tooltip {
            let _ = tray.set_tooltip(Some(t));
        }
    });
}

/// The dot image's canvas and dot, in pixels (@2x: an 8 pt canvas with
/// a 4 pt dot).
const DOT_CANVAS: u32 = 16;
const DOT_DIAMETER: f64 = 8.0;

/// The dots' colours: success teal, ink-faint grey, danger red; the second of
/// each pair for dark menus.
fn dot_colour(state: TrayState, dark: bool) -> [u8; 3] {
    let (light, dark_c) = match state {
        TrayState::Running => ([0x1D, 0x6A, 0x63], [0x6C, 0xC5, 0xB8]),
        TrayState::Starting | TrayState::Paused => ([0x8D, 0x89, 0x80], [0x7B, 0x76, 0x6C]),
        TrayState::CouldntStart => ([0xB3, 0x26, 0x1E], [0xF2, 0x87, 0x7D]),
    };
    if dark {
        dark_c
    } else {
        light
    }
}

/// An anti-aliased filled circle, centred, in straight RGBA (4x4
/// supersampled coverage as alpha).
fn dot_rgba(rgb: [u8; 3]) -> Vec<u8> {
    const SS: u32 = 4;
    let (c, r) = (DOT_CANVAS as f64 / 2.0, DOT_DIAMETER / 2.0);
    let mut out = Vec::with_capacity((DOT_CANVAS * DOT_CANVAS * 4) as usize);
    for py in 0..DOT_CANVAS {
        for px in 0..DOT_CANVAS {
            let mut hits = 0;
            for sy in 0..SS {
                for sx in 0..SS {
                    let x = px as f64 + (sx as f64 + 0.5) / SS as f64 - c;
                    let y = py as f64 + (sy as f64 + 0.5) / SS as f64 - c;
                    if x * x + y * y <= r * r {
                        hits += 1;
                    }
                }
            }
            let a = (hits * 255 + (SS * SS) / 2) / (SS * SS);
            out.extend_from_slice(&[rgb[0], rgb[1], rgb[2], a as u8]);
        }
    }
    out
}

/// A status dot for a native menu item, in the colours for the
/// menus' appearance ([`platform::menus_dark`]). The starting dot does
/// not pulse here: a menu's picture is fixed while it is open.
pub fn dot_image(state: TrayState) -> Image<'static> {
    Image::new_owned(dot_rgba(dot_colour(state, platform::menus_dark())), DOT_CANVAS, DOT_CANVAS)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn size(bytes: &[u8]) -> (u32, u32) {
        let img = Image::from_bytes(bytes).unwrap();
        (img.width(), img.height())
    }

    #[test]
    fn states_map_to_their_pictures() {
        for dark in [false, true] {
            let set = if dark { &platform::TRAY_ICONS.dark } else { &platform::TRAY_ICONS.light };
            assert_eq!(picture(TrayState::Running, 3, dark, false), set.running);
            assert_eq!(picture(TrayState::Paused, 0, dark, false), set.paused);
            assert_eq!(picture(TrayState::CouldntStart, 0, dark, true), set.couldnt_start);
            assert_eq!(picture(TrayState::Starting, 4, dark, true), set.starting_held);
            for f in 0..2 * TRAY_FRAMES {
                assert_eq!(picture(TrayState::Starting, f, dark, false), set.starting[f % TRAY_FRAMES]);
            }
        }
        assert_ne!(picture(TrayState::Running, 0, false, false), picture(TrayState::Running, 0, true, false));
        let expected = if cfg!(windows) { (32, 32) } else { (40, 44) };
        for s in [TrayState::Running, TrayState::Starting, TrayState::Paused, TrayState::CouldntStart] {
            assert_eq!(size(picture(s, 0, true, false)), expected);
        }
    }

    #[test]
    fn several_teams_show_the_first_that_applies() {
        use TrayState::*;
        assert_eq!(TrayState::of_teams([]), Paused);
        assert_eq!(TrayState::of_teams([Paused, Running, Paused]), Running);
        assert_eq!(TrayState::of_teams([Running, Starting]), Starting);
        assert_eq!(TrayState::of_teams([Starting, CouldntStart, Running]), CouldntStart);
    }

    #[test]
    fn dots_are_c9_colours() {
        assert_eq!(dot_colour(TrayState::Running, false), [0x1D, 0x6A, 0x63]);
        assert_eq!(dot_colour(TrayState::Running, true), [0x6C, 0xC5, 0xB8]);
        assert_eq!(dot_colour(TrayState::Paused, false), [0x8D, 0x89, 0x80]);
        assert_eq!(dot_colour(TrayState::Starting, true), [0x7B, 0x76, 0x6C]);
        assert_eq!(dot_colour(TrayState::CouldntStart, false), [0xB3, 0x26, 0x1E]);
        assert_eq!(dot_colour(TrayState::CouldntStart, true), [0xF2, 0x87, 0x7D]);
    }

    #[test]
    fn dot_is_an_antialiased_centred_circle() {
        let rgba = dot_rgba([1, 2, 3]);
        let img = Image::new_owned(rgba.clone(), DOT_CANVAS, DOT_CANVAS);
        assert_eq!((img.width(), img.height()), (16, 16));
        let alpha = |x: u32, y: u32| rgba[((y * DOT_CANVAS + x) * 4 + 3) as usize];
        assert_eq!(alpha(7, 7), 255);
        assert_eq!(alpha(8, 8), 255);
        assert_eq!(alpha(0, 0), 0);
        assert_eq!(alpha(15, 8), 0);
        // The rim is partly covered, and the picture is symmetric.
        assert!((1..255).contains(&alpha(4, 6)), "{}", alpha(4, 6));
        for y in 0..16 {
            for x in 0..16 {
                assert_eq!(alpha(x, y), alpha(15 - x, y));
                assert_eq!(alpha(x, y), alpha(y, x));
            }
        }
        // Covered area ~ pi r^2 = 50.3 px.
        let area: f64 = (0..16 * 16).map(|i| rgba[i * 4 + 3] as f64 / 255.0).sum();
        assert!((area - std::f64::consts::PI * 16.0).abs() < 1.5, "{area}");
        assert!(rgba.chunks(4).all(|p| p[..3] == [1, 2, 3]));
        assert_eq!(dot_image(TrayState::Running).rgba().len(), 16 * 16 * 4);
    }
}
