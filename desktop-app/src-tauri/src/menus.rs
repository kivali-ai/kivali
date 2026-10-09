//! The menu bar and the tray menu, rebuilt from the shell's
//! state on every change, and the menu-bar icon. Every menu routes
//! clicks through [`handle`].
//!
//! Rules (the design's Part 9): the menu bar holds commands only, each
//! with a verb, and hides what never applies to the team in front; the
//! tray is a glance at every team (a status image and a short phrase
//! each) plus the common commands. Teams open from the tray.
//!
//! Where the menu bar lives (`platform`): macOS has the one application
//! menu ([`app_menu`]); Windows puts a menu bar inside each team window
//! ([`window_menu`]), whose Team menu acts on that window's team, so
//! there is no "team in front" to find: its items' ids carry the team.

use crate::platform;
use crate::shell::{self, Shell};
use crate::teams::Team;
use crate::trayicon::{self, TrayIconDriver, TrayState};
use crate::view::{self, TeamState};
use crate::windows;
use std::collections::BTreeMap;
use std::sync::Mutex;
use tauri::menu::{AboutMetadata, IconMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem, Submenu};
use tauri::tray::{TrayIcon, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, Wry};

pub const TRAY_ID: &str = "kivali";

type R<T> = tauri::Result<T>;

/// A team window menu's shortcuts (Windows), by command. The items show
/// them, and since WebView2 keeps a focused webview's keys from the
/// window's menu, each of the team's webviews hands its key presses to
/// [`window_shortcut`] (`platform::menu_shortcuts`), spelled as here.
const WINDOW_KEYS: &[(&str, &str)] = &[
    ("new-team", "Ctrl+N"),
    ("connect", "Ctrl+Shift+N"),
    ("settings", "Ctrl+,"),
    ("close", "Ctrl+W"),
    ("quit", "Ctrl+Q"),
    ("reload", "Ctrl+R"),
    ("zoom-reset", "Ctrl+0"),
    ("zoom-in", "Ctrl+="),
    ("zoom-out", "Ctrl+-"),
    ("browser", "Ctrl+Shift+B"),
];

/// Other presses of the same commands, not shown: Ctrl and + (Shift and
/// =) zooms in, as in a browser.
const KEY_ALIASES: &[(&str, &str)] = &[("zoom-in", "Ctrl+Shift+=")];

/// A team window's Edit menu (Windows): command, label, shortcut. The
/// shortcuts are the webview's own, so the items show them without
/// registering them (muda's predefined items would, and their action is
/// to type the shortcut, which a window holding the focus itself would
/// answer again, without end); choosing one types it into the team's
/// webview (`platform::press_keys`). An empty command is a separator.
const EDIT_KEYS: &[(&str, &str, &str)] = &[
    ("undo", "Undo", "Ctrl+Z"),
    ("redo", "Redo", "Ctrl+Y"),
    ("", "", ""),
    ("cut", "Cut", "Ctrl+X"),
    ("copy", "Copy", "Ctrl+C"),
    ("paste", "Paste", "Ctrl+V"),
    ("select-all", "Select all", "Ctrl+A"),
];

/// The shortcut a team window's menu shows for `command`.
fn window_key(command: &str) -> Option<&'static str> {
    WINDOW_KEYS.iter().find(|(c, _)| *c == command).map(|(_, k)| *k)
}

/// The menu item a key press in team `team`'s window stands for (its id,
/// as [`handle`] takes it), if the press is one of the window menu's
/// shortcuts. The items that act on the window's team carry it in their
/// ids (`reload:<team>`), as [`window_menu`] names them.
fn window_shortcut(team: &str, chord: &str) -> Option<String> {
    let command = WINDOW_KEYS.iter().chain(KEY_ALIASES).find(|(_, k)| *k == chord)?.0;
    let per_team = matches!(command, "close" | "reload" | "zoom-reset" | "zoom-in" | "zoom-out" | "browser");
    Some(if per_team { format!("{command}:{team}") } else { command.to_string() })
}

/// One team as the menus show it.
struct Row {
    team: Team,
    state: TeamState,
    phrase: String,
    /// The team's update, when one is available ("0.17").
    update: Option<String>,
}

struct View {
    rows: Vec<Row>,
    /// The team whose window is in front.
    front: Option<String>,
    app_update: Option<String>,
}

fn row(sh: &Shell, t: Team) -> Row {
    let v = sh.team_view(&t);
    let update = v.update.as_ref().filter(|u| u.state == "available").and_then(|u| u.latest.clone());
    Row { state: v.state, phrase: v.phrase, update: update.map(|u| shell::short_version(&u)), team: t }
}

fn view(app: &AppHandle) -> View {
    let sh = app.state::<Shell>();
    let teams = sh.teams.lock().unwrap().teams.clone();
    let rows = teams.into_iter().map(|t| row(&sh, t)).collect();
    let front = windows::front_team_window(app).map(|(id, _)| id);
    let app_update = {
        let u = sh.app_update.lock().unwrap();
        (u.state == "available").then(|| u.version.clone()).flatten()
    };
    View { rows, front, app_update }
}

fn tray_state(s: TeamState) -> TrayState {
    match s {
        TeamState::Running => TrayState::Running,
        TeamState::Starting => TrayState::Starting,
        TeamState::Paused => TrayState::Paused,
        TeamState::Failed => TrayState::CouldntStart,
    }
}

fn item(app: &AppHandle, id: impl Into<String>, text: impl AsRef<str>, accel: Option<&str>) -> R<MenuItem<Wry>> {
    MenuItem::with_id(app, id.into(), text, true, accel)
}

fn sep(app: &AppHandle) -> R<PredefinedMenuItem<Wry>> {
    PredefinedMenuItem::separator(app)
}

/// Pause or Resume, by what the team is doing now.
fn pause_resume(app: &AppHandle, r: &Row) -> R<Option<MenuItem<Wry>>> {
    let id = &r.team.id;
    Ok(match r.state {
        TeamState::Running => Some(item(app, format!("pause:{id}"), format!("Pause {}…", r.team.name), None)?),
        TeamState::Paused => Some(item(app, format!("resume:{id}"), format!("Resume {}", r.team.name), None)?),
        TeamState::Failed => Some(item(app, format!("resume:{id}"), format!("Try starting {} again", r.team.name), None)?),
        TeamState::Starting => None,
    })
}

/// The Team menu's items for `r`: Pause, Resume or Try again for a team
/// here (none while it starts), its settings, Open in browser with
/// `browser_key`. The menu bar's (macOS) and each team window's (Windows).
fn team_items(app: &AppHandle, r: &Row, browser_key: Option<&str>) -> R<Vec<MenuItem<Wry>>> {
    let id = &r.team.id;
    let mut items = Vec::new();
    if r.team.is_here() {
        items.extend(pause_resume(app, r)?);
    }
    items.push(item(app, format!("settings:{id}"), format!("{} settings…", r.team.name), None)?);
    items.push(item(app, format!("browser:{id}"), "Open in browser", browser_key)?);
    Ok(items)
}

/// The id of a team window's Team menu, which is refilled in place.
const TEAM_MENU: &str = "team-menu";

/// A team window's own menu bar (Windows, `platform::WINDOW_MENU`): the
/// menu bar's commands in Windows' arrangement. No application menu (its
/// Settings, Check for updates and Quit are in File), no Window menu, and
/// the team commands (Close window, View, Edit, Team, Show logs) act on
/// this window's team: their ids carry it.
fn window_menu(app: &AppHandle, r: &Row) -> R<Menu<Wry>> {
    let id = &r.team.id;
    let file = Submenu::with_items(
        app,
        "&File",
        true,
        &[
            &item(app, "new-team", "New team…", window_key("new-team"))?,
            &item(app, "connect", "Connect to a team…", window_key("connect"))?,
            &sep(app)?,
            &item(app, "settings", "Settings…", window_key("settings"))?,
            &item(app, "check-updates", "Check for updates…", None)?,
            &sep(app)?,
            &item(app, format!("close:{id}"), "Close window", window_key("close"))?,
            // The app's own Quit, so it pauses teams first.
            &item(app, "quit", "Quit Kivali", window_key("quit"))?,
        ],
    )?;
    let edit = Submenu::new(app, "&Edit", true)?;
    for (command, label, key) in EDIT_KEYS {
        if command.is_empty() {
            edit.append(&sep(app)?)?;
        } else {
            // Shown after a tab, as Windows shows a shortcut, unregistered.
            edit.append(&item(app, format!("{command}:{id}"), format!("{label}\t{key}"), None)?)?;
        }
    }
    let view_menu = Submenu::with_items(
        app,
        "&View",
        true,
        &[
            &item(app, format!("reload:{id}"), "Reload", window_key("reload"))?,
            &sep(app)?,
            &item(app, format!("zoom-reset:{id}"), "Actual size", window_key("zoom-reset"))?,
            &item(app, format!("zoom-in:{id}"), "Zoom in", window_key("zoom-in"))?,
            &item(app, format!("zoom-out:{id}"), "Zoom out", window_key("zoom-out"))?,
        ],
    )?;
    let team = Submenu::with_id(app, TEAM_MENU, "&Team", true)?;
    for i in team_items(app, r, window_key("browser"))? {
        team.append(&i)?;
    }
    let help = Submenu::with_items(
        app,
        "&Help",
        true,
        &[
            &item(app, "help", "Kivali help", None)?,
            &item(app, "whats-new", "What's new", None)?,
            &item(app, format!("logs:{id}"), "Show logs", None)?,
            &item(app, "licenses", "Open source licenses", None)?,
        ],
    )?;
    Menu::with_items(app, &[&file, &edit, &view_menu, &team, &help])
}

/// What a team window's Team menu depends on: it is refilled only when
/// this changes (refilling an open menu closes it).
fn team_menu_signature(r: &Row) -> String {
    format!("{}|{:?}|{}", r.team.name, r.state, r.team.is_here())
}

/// Each team window's Team menu as last filled, by team.
static WINDOW_MENUS: Mutex<BTreeMap<String, String>> = Mutex::new(BTreeMap::new());

/// A new team window's menu bar (Windows), for its builder: set as the
/// window is made, so its webviews are laid out under it from the start.
/// None where windows have no menu (`platform::WINDOW_MENU`).
pub fn team_window_menu(app: &AppHandle, t: &Team) -> Option<Menu<Wry>> {
    if !platform::WINDOW_MENU {
        return None;
    }
    let r = row(&app.state::<Shell>(), t.clone());
    match window_menu(app, &r) {
        Ok(m) => {
            WINDOW_MENUS.lock().unwrap().insert(t.id.clone(), team_menu_signature(&r));
            Some(m)
        }
        Err(e) => {
            eprintln!("kivali: {}'s menu: {e}", t.name);
            None
        }
    }
}

/// Puts each team window's Team menu in step with its team (Windows).
/// Only that menu changes with the team, and it is refilled in place:
/// replacing a window's menu bar would take it off and put it back,
/// resizing the window's webviews twice.
fn sync_window_menus(app: &AppHandle, v: &View) {
    for r in &v.rows {
        let sig = team_menu_signature(r);
        if WINDOW_MENUS.lock().unwrap().get(&r.team.id) == Some(&sig) {
            continue;
        }
        let Some(window) = app.get_window(&windows::team_label(&r.team.id)) else { continue };
        let Some(team) = window.menu().and_then(|m| m.get(TEAM_MENU)).and_then(|k| k.as_submenu().cloned()) else {
            continue;
        };
        let refill = || -> R<()> {
            while team.remove_at(0)?.is_some() {}
            for i in team_items(app, r, window_key("browser"))? {
                team.append(&i)?;
            }
            Ok(())
        };
        match refill() {
            Ok(()) => {
                WINDOW_MENUS.lock().unwrap().insert(r.team.id.clone(), sig);
            }
            Err(e) => eprintln!("kivali: {}'s menu: {e}", r.team.name),
        }
    }
}

/// Hands one of team `id`'s webviews' key presses to its window's menu
/// (Windows: [`platform::menu_shortcuts`]). A shortcut runs as a click on
/// its item would, after the webview's key event has returned.
pub fn hook_window_shortcuts(app: &AppHandle, webview: &tauri::Webview, id: &str) {
    if !platform::WINDOW_MENU {
        return;
    }
    let (app, id) = (app.clone(), id.to_string());
    platform::menu_shortcuts(
        webview,
        Box::new(move |chord| {
            let Some(item) = window_shortcut(&id, chord) else { return false };
            let app = app.clone();
            std::thread::spawn(move || {
                let a = app.clone();
                let _ = app.run_on_main_thread(move || command(&a, &item));
            });
            true
        }),
    );
}

fn tray_menu(app: &AppHandle, v: &View) -> R<Menu<Wry>> {
    let m = Menu::new(app)?;
    m.append(&item(app, "open", "Open Kivali", None)?)?;
    if !v.rows.is_empty() {
        m.append(&sep(app)?)?;
    }
    // One row per team, its state's dot and phrase; choosing it opens the
    // team's window. Pausing, its settings and the rest are there (the
    // Team menu, the team's page) and in Settings.
    for r in &v.rows {
        let row = IconMenuItem::with_id(
            app,
            format!("open:{}", r.team.id),
            format!("{} · {}", r.team.name, r.phrase),
            true,
            Some(trayicon::dot_image(tray_state(r.state))),
            None::<&str>,
        )?;
        m.append(&row)?;
    }
    // One update at a time: the app's own first, then the first team's.
    let update: Option<MenuItem<Wry>> = if let Some(ver) = &v.app_update {
        Some(item(app, "app-update", format!("Update Kivali to {ver}…"), None)?)
    } else if let Some(r) = v.rows.iter().find(|r| r.update.is_some() && r.team.is_here()) {
        Some(item(
            app,
            format!("update:{}", r.team.id),
            format!("Update {} to {}…", r.team.name, r.update.clone().unwrap_or_default()),
            None,
        )?)
    } else {
        None
    };
    if let Some(u) = update {
        m.append(&sep(app)?)?;
        m.append(&u)?;
    }
    m.append(&sep(app)?)?;
    m.append(&item(app, "new-team", "New team…", None)?)?;
    m.append(&item(app, "connect", "Connect to a team…", None)?)?;
    m.append(&sep(app)?)?;
    m.append(&item(app, "settings", "Settings…", Some("CmdOrCtrl+,"))?)?;
    m.append(&item(app, "quit", "Quit Kivali", Some("CmdOrCtrl+Q"))?)?;
    Ok(m)
}

fn app_menu(app: &AppHandle, v: &View) -> R<Menu<Wry>> {
    let about = AboutMetadata {
        name: Some("Kivali".into()),
        version: Some(app.package_info().version.to_string()),
        ..Default::default()
    };
    let kivali = Submenu::with_items(
        app,
        "Kivali",
        true,
        &[
            &PredefinedMenuItem::about(app, Some("About Kivali"), Some(about))?,
            &item(app, "licenses", "Open source licenses", None)?,
            &sep(app)?,
            &item(app, "settings", "Settings…", Some("CmdOrCtrl+,"))?,
            &item(app, "check-updates", "Check for updates…", None)?,
            &sep(app)?,
            &PredefinedMenuItem::hide(app, Some("Hide Kivali"))?,
            &PredefinedMenuItem::hide_others(app, Some("Hide others"))?,
            &sep(app)?,
            // Our own Quit, not the predefined one, so it pauses teams first.
            &item(app, "quit", "Quit Kivali", Some("CmdOrCtrl+Q"))?,
        ],
    )?;
    let file = Submenu::with_items(
        app,
        "File",
        true,
        &[
            &item(app, "new-team", "New team…", Some("CmdOrCtrl+N"))?,
            &item(app, "connect", "Connect to a team…", Some("CmdOrCtrl+Shift+N"))?,
            &sep(app)?,
            &PredefinedMenuItem::close_window(app, Some("Close window"))?,
        ],
    )?;
    let edit = Submenu::with_items(
        app,
        "Edit",
        true,
        &[
            &PredefinedMenuItem::undo(app, None)?,
            &PredefinedMenuItem::redo(app, None)?,
            &sep(app)?,
            &PredefinedMenuItem::cut(app, None)?,
            &PredefinedMenuItem::copy(app, None)?,
            &PredefinedMenuItem::paste(app, None)?,
            &PredefinedMenuItem::select_all(app, Some("Select all"))?,
        ],
    )?;
    let view_menu = Submenu::with_items(
        app,
        "View",
        true,
        &[
            &item(app, "reload", "Reload", Some("CmdOrCtrl+R"))?,
            &sep(app)?,
            &item(app, "zoom-reset", "Actual size", Some("CmdOrCtrl+0"))?,
            &item(app, "zoom-in", "Zoom in", Some("CmdOrCtrl+="))?,
            &item(app, "zoom-out", "Zoom out", Some("CmdOrCtrl+-"))?,
            &sep(app)?,
            &item(app, "fullscreen", "Enter full screen", Some("Ctrl+CmdOrCtrl+F"))?,
        ],
    )?;
    let mut menus: Vec<Submenu<Wry>> = vec![kivali, file, edit, view_menu];
    // The Team menu acts on the team whose window is in front.
    if let Some(r) = v.front.as_ref().and_then(|f| v.rows.iter().find(|r| &r.team.id == f)) {
        let team = Submenu::new(app, "Team", true)?;
        for i in team_items(app, r, Some("CmdOrCtrl+Shift+B"))? {
            team.append(&i)?;
        }
        menus.push(team);
    }
    let window = Submenu::with_items(
        app,
        "Window",
        true,
        &[
            &PredefinedMenuItem::minimize(app, None)?,
            &PredefinedMenuItem::maximize(app, Some("Zoom"))?,
            &sep(app)?,
            &item(app, "front", "Bring all to front", None)?,
        ],
    )?;
    let help = Submenu::with_items(
        app,
        "Help",
        true,
        &[
            &item(app, "help", "Kivali help", None)?,
            &item(app, "whats-new", "What's new", None)?,
            &item(app, "logs", "Show logs", None)?,
        ],
    )?;
    platform::mark_app_menus(&window, &help);
    menus.push(window);
    menus.push(help);
    let refs: Vec<&dyn tauri::menu::IsMenuItem<Wry>> = menus.iter().map(|m| m as &dyn tauri::menu::IsMenuItem<Wry>).collect();
    Menu::with_items(app, &refs)
}

/// The menu-bar icon's state and tooltip across every team here.
fn icon(v: &View) -> (TrayState, String) {
    let here: Vec<&Row> = v.rows.iter().filter(|r| r.team.is_here()).collect();
    let st = TrayState::of_teams(here.iter().map(|r| tray_state(r.state)));
    let tip = match here.as_slice() {
        [] => "Kivali".to_string(),
        rows => format!(
            "Kivali · {}",
            rows.iter().map(|r| format!("{} {}", r.team.name, r.phrase)).collect::<Vec<_>>().join(" · ")
        ),
    };
    (st, tip)
}

/// Creates the tray icon and the app menu. Called once, from setup.
/// Team windows get their menus as they are made ([`team_window_menu`]).
pub fn install(app: &AppHandle) -> R<TrayIcon> {
    let v = view(app);
    // Tauri hands every menu event (the menu bar's, the tray's, a
    // window's) to every listener registered for the app, the tray's own
    // included, so `handle` is registered once, here, for all of them.
    app.on_menu_event(handle);
    if platform::APP_MENU {
        app.set_menu(app_menu(app, &v)?)?;
    }
    let tray = TrayIconBuilder::with_id(TRAY_ID)
        .menu(&tray_menu(app, &v)?)
        .show_menu_on_left_click(true)
        .on_tray_icon_event(|tray, event| {
            // Opening the menu is the moment someone looks: refresh.
            if let TrayIconEvent::Click { .. } = event {
                let app = tray.app_handle().clone();
                std::thread::spawn(move || {
                    for rt in app.state::<Shell>().runtimes() {
                        if rt.op_running().is_none() && rt.supervisor.is_listening() {
                            let _ = rt.refresh_status();
                        }
                    }
                    shell::changed(&app);
                });
            }
        })
        .build(app)?;
    let driver = TrayIconDriver::new(app.clone(), TRAY_ID);
    let (st, tip) = icon(&v);
    driver.set(st, &tip);
    app.manage(driver);
    Ok(tray)
}

/// What the menus show, as text: they are replaced only when it changes.
/// Replacing a menu that is open closes it under the person's cursor, and
/// the shell redraws on every progress line of every team.
fn signature(v: &View) -> String {
    let mut s = String::new();
    for r in &v.rows {
        s.push_str(&format!("{}|{}|{:?}|{}|{:?}|{}\n", r.team.id, r.team.name, r.state, r.phrase, r.update, r.team.is_here()));
    }
    s.push_str(&format!("front={:?} app={:?}", v.front, v.app_update));
    s
}

static LAST_MENUS: std::sync::Mutex<Option<String>> = std::sync::Mutex::new(None);

/// Rebuilds the menus (when what they show changed) and sets the tray
/// icon from the current state. Main thread only.
pub fn refresh(app: &AppHandle) {
    let v = view(app);
    if let Some(driver) = app.try_state::<TrayIconDriver>() {
        let (st, tip) = icon(&v);
        driver.set(st, &tip);
    }
    let sig = signature(&v);
    {
        let mut last = LAST_MENUS.lock().unwrap();
        if last.as_deref() == Some(sig.as_str()) {
            return;
        }
        *last = Some(sig);
    }
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        match tray_menu(app, &v) {
            Ok(m) => {
                let _ = tray.set_menu(Some(m));
            }
            Err(e) => eprintln!("kivali: tray menu: {e}"),
        }
    }
    if platform::APP_MENU {
        match app_menu(app, &v) {
            Ok(m) => {
                let _ = app.set_menu(m);
            }
            Err(e) => eprintln!("kivali: app menu: {e}"),
        }
    }
    if platform::WINDOW_MENU {
        sync_window_menus(app, &v);
    }
}

/// Asks, then pauses: from a menu (`on_team_window`) as a sheet on the
/// team's window when it shows, from Settings as an alert of its own.
pub fn confirm_pause(app: &AppHandle, id: &str, on_team_window: bool) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    // "3 agents are working. They stop mid-task…" when the team said.
    let working = sh.team_view(&t).working;
    let spec = platform::AlertSpec {
        title: format!("Pause {}?", t.name),
        message: view::pause_message(working),
        buttons: vec!["Pause".into(), "Cancel".into()],
        destructive: None,
        suppression: None,
    };
    let parent = app
        .get_window(&windows::team_label(id))
        .filter(|w| on_team_window && w.is_visible().unwrap_or(false));
    let app2 = app.clone();
    let id = id.to_string();
    platform::alert(
        app,
        parent.as_ref(),
        spec,
        Box::new(move |a| {
            if a.button == 0 {
                report(shell::pause_team(&app2, &id));
            }
        }),
    );
}

/// Resume from a menu; the memory dialog when it would not fit.
pub fn resume_or_ask(app: &AppHandle, id: &str) {
    match shell::resume_team(app, id) {
        Err(e) if e.starts_with("memory:") => {
            let other = e.trim_start_matches("memory:").to_string();
            let sh = app.state::<Shell>();
            let (Some(t), Some(o)) = (sh.team(id), sh.team(&other)) else { return };
            let need = t.local().map(|l| u64::from(l.memory_mb)).unwrap_or(0);
            // The shell's own estimate, as resume_team measured it (and
            // as the pages' memory dialog shows it).
            let free = sh.team_view(&t).memory_free_mb.unwrap_or(0);
            let spec = platform::AlertSpec {
                title: format!("Not enough memory for {}", t.name),
                message: format!(
                    "{} needs {}, and this Mac has {} free while {} runs. Pause {} to resume {}?",
                    t.name,
                    view::gb(need),
                    view::gb(free),
                    o.name,
                    o.name,
                    t.name
                ),
                buttons: vec![format!("Pause {} and resume", o.name), "Cancel".into()],
                destructive: None,
                suppression: None,
            };
            let parent = app.get_window(&windows::team_label(id)).filter(|w| w.is_visible().unwrap_or(false));
            let app2 = app.clone();
            let id = id.to_string();
            platform::alert(
                app,
                parent.as_ref(),
                spec,
                Box::new(move |a| {
                    if a.button == 0 {
                        report(shell::pause_and_resume(&app2, &other, &id));
                    }
                }),
            );
        }
        r => report(r),
    }
}

/// Update from the tray: confirm, then update.
fn confirm_update(app: &AppHandle, id: &str) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    let v = sh.team_view(&t);
    let Some(u) = v.update.filter(|u| u.state == "available") else { return };
    let to = shell::short_version(u.latest.as_deref().unwrap_or_default());
    let from = shell::short_version(u.current.as_deref().unwrap_or_default());
    let spec = platform::AlertSpec {
        title: format!("Update {} to {to}?", t.name),
        message: format!(
            "It takes {}. Agents pause while it updates. If anything goes wrong, {} goes back to {from}.",
            u.takes, t.name
        ),
        buttons: vec!["Update".into(), "Cancel".into(), "What's new".into()],
        destructive: None,
        suppression: None,
    };
    let app2 = app.clone();
    let id = id.to_string();
    let notes = u.notes_url.clone();
    platform::alert(
        app,
        None,
        spec,
        Box::new(move |a| match a.button {
            0 => report(shell::update_team(&app2, &id)),
            2 => {
                if let Some(n) = notes {
                    report(crate::open_url_in_browser(&app2, &n));
                }
            }
            _ => {}
        }),
    );
}

/// Restart to update the app.
pub fn confirm_app_update(app: &AppHandle) {
    let ver = app.state::<Shell>().app_update.lock().unwrap().version.clone().unwrap_or_default();
    let spec = platform::AlertSpec {
        title: "Restart to update Kivali?".into(),
        message: format!("Kivali {ver} is ready. Running teams pause and resume after the restart."),
        buttons: vec!["Restart".into(), "Later".into()],
        destructive: None,
        suppression: None,
    };
    let parent = windows::front_window(app);
    let app2 = app.clone();
    platform::alert(
        app,
        parent.as_ref(),
        spec,
        Box::new(move |a| {
            if a.button == 0 {
                shell::install_app_update(&app2);
            }
        }),
    );
}

fn report(r: Result<(), String>) {
    if let Err(e) = r {
        eprintln!("kivali: {e}");
    }
}

pub const HELP_URL: &str = "https://github.com/kivali-ai/kivali/tree/main/docs";
pub const RELEASES_URL: &str = "https://github.com/kivali-ai/kivali/releases";

/// Opens a window off the menu's own event: creating a window inside a
/// menu handler on the main thread deadlocks on Windows (tauri's
/// `WindowBuilder` docs), and Tauri hands creation to the main thread
/// itself.
fn open_off_main(app: &AppHandle, open: impl FnOnce(&AppHandle) + Send + 'static) {
    let app = app.clone();
    std::thread::spawn(move || open(&app));
}

pub fn handle(app: &AppHandle, event: MenuEvent) {
    command(app, event.id().as_ref());
}

/// Runs the menu item `id`: a bare command (`settings`), which on macOS
/// acts on the team in front where it acts on a team, or a command and
/// the team it acts on (`reload:<team>`), as a team window's own menu
/// (Windows) and the tray name theirs.
fn command(app: &AppHandle, id: &str) {
    match id {
        "open" => open_off_main(app, windows::open_kivali),
        "new-team" => open_off_main(app, |app| {
            let first = app.state::<Shell>().teams.lock().unwrap().teams.is_empty();
            windows::show_setup(app, if first { "welcome" } else { "add" })
        }),
        "connect" => open_off_main(app, |app| windows::show_setup(app, "connect")),
        "settings" => open_off_main(app, |app| windows::show_settings(app, "general")),
        "check-updates" => {
            shell::check_desktop_update(app);
            open_off_main(app, |app| windows::show_settings(app, "general"));
        }
        "app-update" => confirm_app_update(app),
        "reload" => windows::reload_front(app),
        "zoom-reset" => windows::zoom_front(app, None),
        "zoom-in" => windows::zoom_front(app, Some(1)),
        "zoom-out" => windows::zoom_front(app, Some(-1)),
        "fullscreen" => windows::toggle_fullscreen(app),
        "front" => {
            for w in app.windows().into_values() {
                if w.is_visible().unwrap_or(false) {
                    let _ = w.set_focus();
                }
            }
        }
        "help" => report(crate::open_url_in_browser(app, HELP_URL)),
        "whats-new" => report(crate::open_url_in_browser(app, RELEASES_URL)),
        "licenses" => report(crate::show_licenses(app)),
        "logs" => report(crate::show_logs(app)),
        "quit" => shell::quit(app),
        other => {
            let Some((verb, team)) = other.split_once(':') else { return };
            match verb {
                "open" => {
                    let team = team.to_string();
                    open_off_main(app, move |app| windows::show_team(app, &team))
                }
                "pause" => confirm_pause(app, team, true),
                "resume" => resume_or_ask(app, team),
                "settings" => {
                    let route = format!("team/{team}");
                    open_off_main(app, move |app| windows::show_settings(app, &route))
                }
                "browser" => report(crate::open_team_in_browser(app, team)),
                "update" => confirm_update(app, team),
                "close" => windows::hide_team(app, team),
                "reload" => windows::reload_team(app, team),
                "zoom-reset" => windows::zoom_team(app, team, None),
                "zoom-in" => windows::zoom_team(app, team, Some(1)),
                "zoom-out" => windows::zoom_team(app, team, Some(-1)),
                "logs" => report(crate::show_logs_of(app, Some(team))),
                edit => {
                    if let Some((_, _, key)) = EDIT_KEYS.iter().find(|(c, ..)| !c.is_empty() && *c == edit) {
                        windows::focus_team_view(app, team);
                        platform::press_keys(key);
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::teams::{Place, Team};

    fn row(name: &str, here: bool, state: TeamState, phrase: &str) -> Row {
        let place = if here {
            Place::Here(crate::teams::Local { dir: format!("teams/{name}"), port: 0, owner: None, memory_mb: 4096, cpus: 4, public_url: None, call_me: None, signed_out_at: None })
        } else {
            Place::Elsewhere { url: format!("https://{name}.example.com") }
        };
        Row { team: Team { id: name.into(), name: name.into(), kind: None, place, paused_at: None }, state, phrase: phrase.into(), update: None }
    }

    #[test]
    fn icon_follows_teams_here_only() {
        let v = View {
            rows: vec![row("Plainsong", true, TeamState::Running, "running"), row("Home", true, TeamState::Paused, "paused"), row("Studio", false, TeamState::Paused, "can't reach studio.example.com")],
            front: None,
            app_update: None,
        };
        let (st, tip) = icon(&v);
        assert_eq!(st, TrayState::Running);
        assert_eq!(tip, "Kivali · Plainsong running · Home paused");
        let none = View { rows: vec![], front: None, app_update: None };
        assert_eq!(icon(&none), (TrayState::Paused, "Kivali".to_string()));
    }

    /// A key press in a team window names the item of that window's menu:
    /// the team's own for its commands, the app's for the rest; the Edit
    /// keys stay the webview's.
    #[test]
    fn window_shortcuts_name_the_window_menus_items() {
        let s = |chord| window_shortcut("plainsong", chord);
        assert_eq!(s("Ctrl+R").as_deref(), Some("reload:plainsong"));
        assert_eq!(s("Ctrl+W").as_deref(), Some("close:plainsong"));
        assert_eq!(s("Ctrl+0").as_deref(), Some("zoom-reset:plainsong"));
        assert_eq!(s("Ctrl+=").as_deref(), Some("zoom-in:plainsong"));
        assert_eq!(s("Ctrl+Shift+=").as_deref(), Some("zoom-in:plainsong"));
        assert_eq!(s("Ctrl+-").as_deref(), Some("zoom-out:plainsong"));
        assert_eq!(s("Ctrl+Shift+B").as_deref(), Some("browser:plainsong"));
        assert_eq!(s("Ctrl+N").as_deref(), Some("new-team"));
        assert_eq!(s("Ctrl+Shift+N").as_deref(), Some("connect"));
        assert_eq!(s("Ctrl+,").as_deref(), Some("settings"));
        assert_eq!(s("Ctrl+Q").as_deref(), Some("quit"));
        for webview_own in ["Ctrl+C", "Ctrl+V", "Ctrl+X", "Ctrl+Z", "Ctrl+Y", "Ctrl+A", "Ctrl+F", "F5", "Ctrl+Shift+R"] {
            assert_eq!(s(webview_own), None, "{webview_own}");
        }
    }

    /// Every shortcut is spelled as a key press is
    /// (`platform::windows_ui::chord`), or it could never be pressed, and
    /// each press means one thing.
    #[test]
    fn window_keys_are_pressable_and_distinct() {
        use crate::platform::windows_ui::{chord, keys};
        let edit = EDIT_KEYS.iter().filter(|(c, ..)| !c.is_empty()).map(|(c, _, k)| (*c, *k));
        let all: Vec<(&str, &str)> = WINDOW_KEYS.iter().chain(KEY_ALIASES).copied().chain(edit).collect();
        for (command, k) in &all {
            let (ctrl, shift, alt, vk) = keys(k).unwrap_or_else(|| panic!("{command}: {k}"));
            assert_eq!(chord(u32::from(vk), ctrl, shift, alt).as_deref(), Some(*k), "{command}");
        }
        let mut chords: Vec<&str> = all.iter().map(|(_, k)| *k).collect();
        chords.sort_unstable();
        chords.dedup();
        assert_eq!(chords.len(), all.len());
    }
}
