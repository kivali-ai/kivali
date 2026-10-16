//! The windows.
//!
//! - `setup` (680×580, fixed): adding a team — first launch, File → New
//!   team, Connect to a team. Bundled pages, with IPC.
//! - `settings` (880×640): General, a page per team, Advanced. Bundled
//!   pages, with IPC.
//! - One window per team, `team-<id>`, titled with the team (on Windows
//!   with a menu bar of its own, `menus::team_window_menu`), holding two
//!   webviews in the same spot, one shown at a time:
//!   - `web-<id>`: the team's own served web app, loaded as a remote URL
//!     and pinned to the team's origin. No capability names it, so it
//!     has no IPC.
//!   - `page-<id>`: the bundled Kivali page shown when the team can't
//!     show itself (paused, starting, updating, couldn't start, can't be
//!     reached), with IPC.
//!
//! - A web subagent's browser window of a team, `browser-<hash>-<id>`
//!   (1320×900), opened when the team's web app opens
//!   `/agents/<slug>/subagents/<id>/browser`: that page alone, pinned to
//!   the team's origin as the team's web view is, with no IPC.
//!
//! A team's web app and the bundled pages never share a webview, so the
//! ACL is scoped by webview label as well as origin.

use crate::platform;
use crate::shell::{self, Shell};
use crate::signin;
use crate::teams::{Place, Team};
use crate::view::TeamState;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use std::collections::HashMap;
use tauri::webview::{NewWindowResponse, PageLoadEvent, WebviewBuilder};
use tauri::window::WindowBuilder;
use tauri::{AppHandle, LogicalPosition, LogicalSize, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder, Window};
use url::Url;

pub const SETUP: &str = "setup";
pub const SETTINGS: &str = "settings";
const TEAM_PREFIX: &str = "team-";
const WEB_PREFIX: &str = "web-";
const PAGE_PREFIX: &str = "page-";
const BROWSER_PREFIX: &str = "browser-";
/// Hex digits of the path's hash in a browser window's label.
const BROWSER_HASH_LEN: usize = 16;

pub fn team_label(id: &str) -> String {
    format!("{TEAM_PREFIX}{id}")
}
pub fn web_label(id: &str) -> String {
    format!("{WEB_PREFIX}{id}")
}
fn page_label(id: &str) -> String {
    format!("{PAGE_PREFIX}{id}")
}

/// A team's browser window for the page at `path`:
/// `browser-<hash>-<id>`, the hash the first 16 hex digits of the path's
/// SHA-256, so the same page always has the same window.
fn browser_label(id: &str, path: &str) -> String {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(path.as_bytes());
    let hash: String = digest.iter().take(BROWSER_HASH_LEN / 2).map(|b| format!("{b:02x}")).collect();
    format!("{BROWSER_PREFIX}{hash}-{id}")
}

fn is_browser_label(label: &str) -> bool {
    label.starts_with(BROWSER_PREFIX)
}

/// The team a window label belongs to.
pub fn team_of(label: &str) -> Option<&str> {
    label
        .strip_prefix(TEAM_PREFIX)
        .or_else(|| label.strip_prefix(WEB_PREFIX))
        .or_else(|| label.strip_prefix(PAGE_PREFIX))
        .or_else(|| {
            let rest = label.strip_prefix(BROWSER_PREFIX)?;
            let (hash, id) = (rest.get(..BROWSER_HASH_LEN)?, rest.get(BROWSER_HASH_LEN..)?.strip_prefix('-')?);
            (hash.bytes().all(|b| b.is_ascii_hexdigit()) && !id.is_empty()).then_some(id)
        })
}

/// A web subagent's browser page in a team's web app:
/// `/agents/<slug>/subagents/<id>/browser`, nothing before or after.
fn is_browser_path(path: &str) -> bool {
    let seg: Vec<&str> = path.split('/').collect();
    matches!(seg.as_slice(), ["", "agents", slug, "subagents", id, "browser"] if !slug.is_empty() && !id.is_empty())
}

/// Webviews that run the bundled pages and may call the app's commands.
pub fn is_page_label(label: &str) -> bool {
    label == SETUP || label == SETTINGS || label.starts_with(PAGE_PREFIX)
}

/// What a team's web view does with a navigation.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TeamNav {
    Allow,
    /// Refused here, opened in the system browser instead.
    Browser,
    /// The team's own `/auth/login` without `client=desktop`: refused,
    /// and loaded again with it, as the desktop sign-in (signin.rs).
    DesktopLogin,
    Deny,
}

fn is_app_origin(url: &Url) -> bool {
    matches!(url.host_str(), Some("tauri.localhost" | "ipc.localhost"))
        || (cfg!(debug_assertions) && url.host_str() == Some("localhost") && url.port() == Some(1420))
}

/// A team's web view is pinned to its origin (scheme, host, port). wry
/// asks this for every navigation, top-level and frames alike on macOS,
/// the page's only on Windows (frames there: [`frame_may_load`]).
/// The app's own origin and non-web schemes are refused outright; any
/// other web page (another team, a sign-in page, a link) goes to the
/// system browser. `about:blank`/`about:srcdoc` frames and the team's own
/// `blob:` URLs stay. The team's own `/auth/login` loads only with
/// `client=desktop`; without it, the shell starts the desktop sign-in.
pub fn team_nav(url: &Url, pinned: Option<&url::Origin>) -> TeamNav {
    match url.scheme() {
        "about" => {
            if url.query().is_none() && matches!(url.path(), "blank" | "srcdoc") {
                TeamNav::Allow
            } else {
                TeamNav::Deny
            }
        }
        "blob" => {
            if Some(&url.origin()) == pinned && url.origin().is_tuple() {
                TeamNav::Allow
            } else {
                TeamNav::Deny
            }
        }
        "http" | "https" => {
            if is_app_origin(url) {
                TeamNav::Deny
            } else if Some(&url.origin()) == pinned {
                if url.path() == signin::LOGIN_PATH && !signin::is_desktop_login(url) {
                    TeamNav::DesktopLogin
                } else {
                    TeamNav::Allow
                }
            } else {
                TeamNav::Browser
            }
        }
        _ => TeamNav::Deny,
    }
}

/// Whether a frame of a team's web view may load: exactly what
/// [`team_nav`] loads as it stands (the pinned origin, `about:blank`,
/// `about:srcdoc`, the origin's own `blob:`). A frame never opens the
/// browser and never starts the desktop sign-in, so the team's own
/// `/auth/login` without `client=desktop` stays out of frames too.
/// Windows asks this through [`platform::guard_frames`]; on macOS
/// [`team_nav`] sees frames itself and loads the same ones.
pub fn frame_may_load(pinned: Option<&url::Origin>, url: &Url) -> bool {
    team_nav(url, pinned) == TeamNav::Allow
}

/// A refused address as the log names it: its origin, or the scheme of
/// one that has none (`data:`, `file:`, the app's own `tauri:`).
fn origin_for_log(url: &Url) -> String {
    let origin = url.origin();
    if origin.is_tuple() {
        origin.ascii_serialization()
    } else {
        format!("{}:", url.scheme())
    }
}

/// The team web view's frame guard ([`platform::guard_frames`]). Each
/// frame is checked against the pin as it is at that moment, read from
/// the team's own cell, the one `on_navigation` reads, never through the
/// shell's map: the pin moves on the first start and when the port
/// changes. It runs on the main thread inside WebView2's callback and
/// holds the cell only to clone it. Every other holder only reads or
/// sets it and never calls into a webview meanwhile, so the guard waits
/// at most for another thread's read, and never re-enters a holder on
/// the main thread. A poisoned cell reads as no pin: only the empty
/// frames load.
fn frame_guard(label: String, pin: Arc<Mutex<Option<url::Origin>>>) -> Box<dyn Fn(&str) -> bool + Send> {
    Box::new(move |uri| {
        let pinned = pin.lock().map(|p| p.clone()).unwrap_or_default();
        let url = Url::parse(uri).ok();
        if url.as_ref().is_some_and(|u| frame_may_load(pinned.as_ref(), u)) {
            return true;
        }
        // The origin only: a refused URL may carry a sign-in's state.
        let what = url.as_ref().map_or_else(|| "an unreadable address".to_string(), origin_for_log);
        eprintln!("kivali: {label} refused a frame of {what}");
        false
    })
}

/// Adds a team's web view to its window, guards its frames and watches
/// how its loads end (`watched` set when [`platform::watch_loads`] does),
/// in the same turn of the main thread. The webview starts its first
/// navigation while it is built; nothing runs the message loop before
/// the hooks are in (building the page webview would), so no frame
/// starts unguarded and no load ends unseen, whichever thread opened the
/// window.
fn add_team_web(
    window: &Window,
    web: WebviewBuilder<tauri::Wry>,
    size: LogicalSize<f64>,
    guard: Box<dyn Fn(&str) -> bool + Send>,
    ended: Box<dyn Fn(platform::LoadEnd) + Send>,
    watched: Arc<AtomicBool>,
) -> tauri::Result<tauri::Webview> {
    let (tx, rx) = std::sync::mpsc::channel();
    let w = window.clone();
    window.run_on_main_thread(move || {
        let added = w.add_child(web, LogicalPosition::new(0.0, 0.0), size);
        if let Ok(web) = &added {
            platform::guard_frames(web, guard);
            watched.store(platform::watch_loads(web, ended), Ordering::SeqCst);
        }
        let _ = tx.send(added);
    })?;
    rx.recv().map_err(|_| tauri::Error::FailedToReceiveMessage)?
}

/// The bundled pages' webviews only ever show the app's origin
/// (`tauri://localhost`, or `http(s)://tauri.localhost` where the webview
/// serves the app over http, as WebView2 does), plus the Vite dev server
/// in a debug build.
fn page_may_load(url: &Url) -> bool {
    match url.scheme() {
        "tauri" => url.host_str() == Some("localhost"),
        "https" | "http" if url.host_str() == Some("tauri.localhost") => url.port().is_none(),
        "http" => cfg!(debug_assertions) && url.host_str() == Some("localhost") && url.port() == Some(1420),
        _ => false,
    }
}

/// A bundled page's webview that ends up showing anything but the app's
/// own pages goes back to the last one it showed. (Seen once on closing
/// a window: Tauri's IPC endpoint as a page, "only POST and OPTIONS are
/// allowed".) Navigations are refused by [`page_may_load`]; this covers
/// loads that never ask, and logs them.
fn heal_page_loads<R: tauri::Runtime>(
) -> impl Fn(tauri::Webview<R>, tauri::webview::PageLoadPayload<'_>) + Send + Sync + 'static {
    let last_good: Arc<Mutex<Option<Url>>> = Arc::new(Mutex::new(None));
    move |w, p| {
        if p.event() != PageLoadEvent::Finished {
            return;
        }
        let url = p.url().clone();
        if page_may_load(&url) {
            *last_good.lock().unwrap() = Some(url);
            return;
        }
        eprintln!("kivali: {} loaded {}://{} instead of a Kivali page; going back", w.label(), url.scheme(), url.host_str().unwrap_or(""));
        if let Some(good) = last_good.lock().unwrap().clone() {
            let _ = w.navigate(good);
        }
    }
}

/// How often a team window may send a page to the system browser.
pub const BROWSER_OPEN_EVERY: Duration = Duration::from_secs(1);
/// How long after a team window loses focus it may still do so.
pub const BROWSER_FOCUS_GRACE: Duration = Duration::from_secs(3);

/// Rate limit on a team window opening the system browser. Every refused
/// cross-origin navigation would otherwise open one, and a hostile page
/// could loop `location.assign` or add iframes to drive the person's
/// browser through any URL with no gesture. At most one open per
/// [`BROWSER_OPEN_EVERY`], and only while the window has focus or lost it
/// within [`BROWSER_FOCUS_GRACE`].
#[derive(Debug, Default)]
pub struct BrowserGate {
    last_open: Option<Instant>,
    focused: bool,
    unfocused_at: Option<Instant>,
}

impl BrowserGate {
    pub fn focus(&mut self, focused: bool, now: Instant) {
        if self.focused && !focused {
            self.unfocused_at = Some(now);
        }
        self.focused = focused;
    }

    pub fn try_open(&mut self, now: Instant) -> bool {
        let attended = self.focused
            || self.unfocused_at.is_some_and(|t| now.saturating_duration_since(t) <= BROWSER_FOCUS_GRACE);
        let spaced = self.last_open.is_none_or(|t| now.saturating_duration_since(t) >= BROWSER_OPEN_EVERY);
        if attended && spaced {
            self.last_open = Some(now);
            true
        } else {
            false
        }
    }
}

fn open_gated(app: &AppHandle, gate: &Mutex<BrowserGate>, url: &Url) {
    if gate.lock().unwrap().try_open(Instant::now()) {
        if matches!(url.scheme(), "http" | "https") {
            let _ = crate::open_url_in_browser(app, url.as_str());
        }
    } else {
        // The origin only: a refused URL may carry a sign-in's state.
        eprintln!(
            "kivali: not opening a page of {} in the browser (rate limit or window not focused)",
            url.origin().ascii_serialization()
        );
    }
}

fn pin_of(app: &AppHandle, id: &str) -> Arc<Mutex<Option<url::Origin>>> {
    app.state::<Shell>().pins.lock().unwrap().entry(id.to_string()).or_default().clone()
}

/// What the shell knows of a team's web view, to decide when to show it
/// and when to send it to the team's origin.
///
/// Until the web view has finished loading a page (`loaded`) the window
/// keeps showing the team's page (the setup wizard, "starting"): shown as
/// soon as the load starts, the web view is blank for the moment the app
/// takes to arrive, a flash between the two.
///
/// A web view made before the team serves loads its origin while nothing
/// answers there. That load fails: WKWebView never finishes it, WebView2
/// finishes it on an error page, which [`platform::watch_loads`] reports
/// as failed. So when the team starts serving, a web view not yet sent to
/// the origin while serving goes there, unless its last finished load
/// happened while serving.
///
/// A load the shell sends while the team serves is pending until a page
/// finishes. It has failed when WebView2 says so, or, as WKWebView never
/// says, when no page finished within [`patience`] of it starting or of a
/// page starting to arrive. A failed load is sent again after
/// [`retry_delay`], each retry waiting longer up to a bound, until a page
/// finishes or the team stops serving: a load that failed for a moment
/// (the network not yet up after waking) recovers on its own. A failed
/// signed-in handoff is sent again as a handoff, with a new token. A load
/// the web view turned away from (a desktop sign-in, a page for the
/// browser) is no longer pending, and is not retried.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct WebLoad {
    /// A page finished loading since the web view was last sent to the origin.
    loaded: bool,
    /// The team showed its web app when the window last looked.
    serving: bool,
    /// The last finished load happened while the team was serving.
    loaded_serving: bool,
    /// The web view has been sent to the origin since the team began serving.
    sent: bool,
    /// A load is pending: when it counts as failed, or, once it has
    /// failed, when it is sent again.
    retry_at: Option<Instant>,
    /// The pending load is known to have failed.
    failed: bool,
    /// A page of the pending load has started to arrive.
    arriving: bool,
    /// The pending load is the signed-in handoff.
    handoff: bool,
    /// Retries since a page last finished, for the backoff.
    tries: u32,
}

/// Where a pending load is sent again: the team's origin, or a new
/// signed-in handoff.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Resend {
    Origin,
    Handoff,
}

/// Whether the shell logs a load's `tries`-th retry: the first, then
/// every tenth, so a team that stays out of reach does not fill the log.
fn logs_retry(tries: u32) -> bool {
    tries == 1 || tries.is_multiple_of(10)
}

/// How long a pending load may go without a page finishing before it
/// counts as failed: 10 s, doubling with each retry, at most a minute.
fn patience(tries: u32) -> Duration {
    (Duration::from_secs(10) * 2u32.pow(tries.min(3))).min(Duration::from_secs(60))
}

/// How long after a failed load it is sent again: 1 s, doubling with
/// each retry, at most 30 s.
fn retry_delay(tries: u32) -> Duration {
    (Duration::from_secs(1) * 2u32.pow(tries.min(5))).min(Duration::from_secs(30))
}

impl WebLoad {
    /// A new web view, its first navigation (to the origin, if the team
    /// has one) under way; `serving` whether the team serves now.
    fn new(serving: bool, now: Instant) -> Self {
        let mut w = WebLoad { serving, ..WebLoad::default() };
        if serving {
            w.sending(now);
        }
        w
    }

    /// A page finished loading. Returns the retries it took.
    fn finished(&mut self) -> u32 {
        let tries = self.tries;
        self.loaded = true;
        self.loaded_serving = self.serving;
        self.retry_at = None;
        self.failed = false;
        self.arriving = false;
        self.handoff = false;
        self.tries = 0;
        tries
    }

    /// A page started to arrive: a pending load is under way, its
    /// patience counted from now.
    fn started(&mut self, now: Instant) {
        if self.retry_at.is_some() && !self.failed {
            self.arriving = true;
            self.retry_at = Some(now + patience(self.tries));
        }
    }

    /// The web view's load failed (WebView2 says so): a pending load is
    /// sent again after [`retry_delay`].
    fn failed(&mut self, now: Instant) {
        if self.serving && !self.loaded && self.retry_at.is_some() {
            self.failed = true;
            self.arriving = false;
            self.retry_at = Some(now + retry_delay(self.tries));
        }
    }

    /// The web view turned the pending load away, to a desktop sign-in or
    /// to the browser: it is not retried.
    fn handed_off(&mut self) {
        self.retry_at = None;
        self.failed = false;
        self.arriving = false;
        self.handoff = false;
    }

    /// The web view is being sent to the team's origin; the load is
    /// pending while the team serves.
    fn sending(&mut self, now: Instant) {
        self.loaded = false;
        self.sent = true;
        self.failed = false;
        self.arriving = false;
        self.handoff = false;
        self.retry_at = self.serving.then(|| now + patience(self.tries));
    }

    /// The web view is being sent to the signed-in handoff; the load is
    /// pending while the team serves.
    fn sending_handoff(&mut self, now: Instant) {
        self.sending(now);
        self.handoff = true;
    }

    /// Whether a pending load is in flight, for Reload to leave be: the
    /// signed-in handoff, or a load whose page has started to arrive,
    /// until it fails or its patience runs out. A load with nothing
    /// arriving yet may have failed unseen (WKWebView), so it is not.
    fn in_flight(&self, now: Instant) -> bool {
        (self.handoff || self.arriving) && !self.failed && self.retry_at.is_some_and(|t| now < t)
    }

    /// Records a send of the pending load again, as the same kind.
    fn resend(&mut self, now: Instant) -> Resend {
        if self.handoff {
            self.sending_handoff(now);
            Resend::Handoff
        } else {
            self.sending(now);
            Resend::Origin
        }
    }

    /// Where to send the web view again now, if anywhere: a pending load
    /// failed and its retry is due. Records the send.
    fn retry(&mut self, now: Instant) -> Option<Resend> {
        if !self.serving || self.loaded || !self.retry_at.is_some_and(|t| now >= t) {
            return None;
        }
        self.tries = self.tries.saturating_add(1);
        Some(self.resend(now))
    }

    /// View → Reload while the team serves: where to send the web view,
    /// as it has no page to reload, if anywhere: at once, the backoff
    /// from scratch, unless a load is in flight ([`WebLoad::in_flight`]).
    /// Records the send.
    fn reload(&mut self, now: Instant) -> Option<Resend> {
        if self.loaded || self.in_flight(now) {
            return None;
        }
        self.tries = 0;
        Some(self.resend(now))
    }

    /// Records whether the team serves now and returns whether to send the
    /// web view to the origin. `moved`: the origin is not the one the web
    /// view is pinned to. `blank`: a page has loaded and it is `about:`.
    fn sync(&mut self, serving: bool, moved: bool, blank: bool, now: Instant) -> bool {
        self.serving = serving;
        if !serving {
            self.sent = false;
            self.handed_off();
            self.tries = 0;
            return false;
        }
        let go = moved || blank || (!self.sent && !(self.loaded && self.loaded_serving));
        self.sent = true;
        if go {
            self.tries = 0;
            self.sending(now);
        }
        go
    }
}

/// Each team window's [`WebLoad`], by team id.
static WEB_LOADS: Mutex<Option<HashMap<String, WebLoad>>> = Mutex::new(None);

fn web_load<T>(id: &str, f: impl FnOnce(&mut WebLoad) -> T) -> T {
    let mut g = WEB_LOADS.lock().unwrap();
    f(g.get_or_insert_with(HashMap::new).entry(id.to_string()).or_default())
}

/// Wakes the shell when a team's pending load is due to count as failed
/// or to be sent again ([`WebLoad`]'s `retry_at`), for [`retry_load`] to
/// decide. A wake that finds nothing due does nothing: the load finished,
/// the team stopped serving, or a later event moved the time on and
/// armed its own wake.
fn arm_retry(app: &AppHandle, id: &str) {
    let Some(at) = web_load(id, |w| w.retry_at) else { return };
    let (app, id) = (app.clone(), id.to_string());
    std::thread::spawn(move || {
        std::thread::sleep(at.saturating_duration_since(Instant::now()));
        let app2 = app.clone();
        let _ = app.run_on_main_thread(move || retry_load(&app2, &id));
    });
}

/// Sends a team's web view to its origin again, or to a new signed-in
/// handoff, when its pending load failed and the retry is due
/// ([`WebLoad::retry`]).
fn retry_load(app: &AppHandle, id: &str) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    if !shows_web(&sh, &t) {
        return;
    }
    let (Some(web), Some(origin)) = (app.get_webview(&web_label(id)), team_root(&t)) else { return };
    // A web view not on its team's origin is sync_team's to move.
    if pin_of(app, id).lock().unwrap().as_ref() != Some(&origin.origin()) {
        return;
    }
    let Some((how, tries)) = web_load(id, |w| w.retry(Instant::now()).map(|how| (how, w.tries))) else { return };
    if logs_retry(tries) {
        eprintln!("kivali: {}: no page loaded; trying again (retry {tries})", web_label(id));
    }
    send_again(app, id, &web, origin, how);
}

/// Sends a team's web view where [`WebLoad`] said, the send already
/// recorded there.
fn send_again(app: &AppHandle, id: &str, web: &tauri::Webview, origin: Url, how: Resend) {
    match how {
        Resend::Origin => {
            let _ = web.navigate(origin);
        }
        Resend::Handoff => send_handoff(app, id, true),
    }
    arm_retry(app, id);
}

/// Sends a team here's web view to the signed-in handoff
/// (docs/developers/auth.md, Desktop handoff) with a token fresh from
/// the team's supervisor, fetched off the main thread. Each token works
/// once and for two minutes, so a handoff sent again gets a new one.
/// `retrying`: this resends a pending handoff, and goes only while that
/// is still pending; with no token to be had, the web view goes to the
/// origin instead. A first handoff with no token leaves the window as
/// it is.
fn send_handoff(app: &AppHandle, id: &str, retrying: bool) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    let (Some(rt), Some(origin), Some(root)) = (sh.runtime(id), t.origin(), team_root(&t)) else { return };
    let app2 = app.clone();
    let id = id.to_string();
    std::thread::spawn(move || {
        let target = match rt.supervisor.handoff() {
            Ok(token) => handoff_url(&origin, &token),
            Err(e) => {
                eprintln!("kivali: opening {id} signed in: {e}");
                None
            }
        };
        let app3 = app2.clone();
        let _ = app2.run_on_main_thread(move || {
            // Only onto the origin the window is pinned to.
            if pin_of(&app3, &id).lock().unwrap().as_ref() != Some(&root.origin()) {
                return;
            }
            if retrying && !web_load(&id, |w| !w.loaded && w.retry_at.is_some()) {
                return;
            }
            let Some(w) = app3.get_webview(&web_label(&id)) else { return };
            let now = Instant::now();
            match target {
                Some(target) => {
                    // A load in flight, which View → Reload leaves be.
                    web_load(&id, |l| l.sending_handoff(now));
                    let _ = w.navigate(target);
                }
                None if retrying => {
                    web_load(&id, |l| l.sending(now));
                    let _ = w.navigate(root);
                }
                None => return,
            }
            arm_retry(&app3, &id);
        });
    });
}

/// How a page load of a team's web view ended.
fn load_ended(app: &AppHandle, id: &str, end: platform::LoadEnd) {
    match end {
        platform::LoadEnd::Shown => {
            // The web view has something to show: the window may now
            // swap the team's page for it (sync_team).
            let (first, tries) = web_load(id, |w| {
                let first = !w.loaded;
                (first, w.finished())
            });
            if tries > 0 {
                eprintln!("kivali: {}: a page loaded after {tries} retries", web_label(id));
            }
            if first {
                sync_all(app);
            }
        }
        platform::LoadEnd::Failed => {
            web_load(id, |w| w.failed(Instant::now()));
            arm_retry(app, id);
        }
    }
}

/// The root of a team's web app, if it has an origin.
fn team_root(t: &Team) -> Option<Url> {
    t.origin().and_then(|o| Url::parse(&format!("{o}/")).ok())
}

fn team_title(t: &Team) -> String {
    match t.device() {
        Some(d) => format!("{} · {d}", t.name),
        None => t.name.clone(),
    }
}

/// Starts the desktop sign-in for a team window's own `login` URL: any
/// older pending sign-in of that team closes, a listener on `127.0.0.1`
/// takes its place, and the web view loads the same URL with
/// `client=desktop`, `return_port` and `return_token`. The load runs from
/// another thread: inside the navigation callback that is refusing this
/// one, Tauri would run it before WebKit has the refusal.
fn start_desktop_login(app: &AppHandle, id: &str, login: &Url) {
    let sh = app.state::<Shell>();
    let old = sh.pending_signins.lock().unwrap().remove(id);
    drop(old);
    let app_cb = app.clone();
    let id_cb = id.to_string();
    let pending = match signin::start(login.origin(), Arc::new(signin::SystemClock), move |r| finish_signin(&app_cb, &id_cb, r)) {
        Ok(p) => p,
        Err(e) => return eprintln!("kivali: sign-in: cannot listen on 127.0.0.1: {e}"),
    };
    let target = signin::desktop_login_url(login, pending.port, &pending.token);
    let token = pending.token.clone();
    sh.pending_signins.lock().unwrap().insert(id.to_string(), pending);
    let app2 = app.clone();
    let id = id.to_string();
    std::thread::spawn(move || {
        let pinned = pin_of(&app2, &id).lock().unwrap().clone();
        if pinned.as_ref() != Some(&target.origin()) {
            eprintln!("kivali: sign-in abandoned: the team's address changed");
            let sh = app2.state::<Shell>();
            let mut p = sh.pending_signins.lock().unwrap();
            if p.get(&id).is_some_and(|s| s.token == token) {
                p.remove(&id);
            }
            return;
        }
        if let Some(w) = app2.get_webview(&web_label(&id)) {
            let _ = w.navigate(target);
        }
    });
}

/// The browser came back to the listener: the team window loads its
/// `/auth/callback` with the browser's query, where the server checks the
/// state against the webview's cookie and sets the session.
fn finish_signin(app: &AppHandle, id: &str, ret: signin::Return) {
    let sh = app.state::<Shell>();
    let pending = {
        let mut g = sh.pending_signins.lock().unwrap();
        if g.get(id).is_some_and(|p| p.token == ret.token) {
            g.remove(id)
        } else {
            None
        }
    };
    if pending.is_none() {
        return eprintln!("kivali: sign-in return dropped: a newer sign-in replaced it");
    }
    let app2 = app.clone();
    let id = id.to_string();
    let _ = app.run_on_main_thread(move || {
        *pin_of(&app2, &id).lock().unwrap() = Some(ret.callback.origin());
        if let Some(w) = app2.get_webview(&web_label(&id)) {
            let _ = w.navigate(ret.callback);
        }
        // A connecting team's window shows once the sign-in worked
        // (shell::connect_page_loaded); any other comes forward now.
        let connecting = app2.state::<Shell>().connecting.lock().unwrap().as_ref().is_some_and(|t| t.id == id);
        if !connecting {
            if let Some(w) = app2.get_window(&team_label(&id)) {
                let _ = w.show();
                let _ = w.set_focus();
            }
        }
    });
}

/// Builds a team's window with its two webviews; `visible` false for a
/// team being connected, whose window shows once it is signed in.
fn create_team_window(app: &AppHandle, t: &Team, visible: bool) -> Option<Window> {
    let id = t.id.clone();
    let pin = pin_of(app, &id);
    let start = team_root(t);
    *pin.lock().unwrap() = start.as_ref().map(Url::origin);
    // A new web view has loaded nothing, whatever an earlier window of
    // this team did.
    let serving = shows_web(&app.state::<Shell>(), t);
    web_load(&id, |w| *w = WebLoad::new(serving, Instant::now()));
    let mut builder = WindowBuilder::new(app, team_label(&id))
        .title(team_title(t))
        .inner_size(1200.0, 760.0)
        .min_inner_size(900.0, 600.0)
        .visible(visible);
    // Windows: the menu bar is inside the window, set as it is made.
    if let Some(menu) = crate::menus::team_window_menu(app, t) {
        builder = builder.menu(menu);
    }
    let window = match builder.build() {
        Ok(w) => w,
        Err(e) => {
            eprintln!("kivali: cannot open {}'s window: {e}", t.name);
            return None;
        }
    };
    // The webviews fill the client area, which a menu bar shortens; with
    // auto_resize they keep their share of it from here on, so a size
    // taller than it would stay cut off at the bottom.
    let size = window
        .inner_size()
        .and_then(|s| Ok(s.to_logical::<f64>(window.scale_factor()?)))
        .unwrap_or(LogicalSize::new(1200.0, 760.0));
    // The window is created focused; its focus events keep this true.
    let gate = Arc::new(Mutex::new(BrowserGate::default()));
    gate.lock().unwrap().focus(true, Instant::now());
    let (gate_nav, gate_new, gate_focus) = (gate.clone(), gate.clone(), gate);
    let (app_nav, app_new, app_load) = (app.clone(), app.clone(), app.clone());
    let (id_nav, id_new, id_load) = (id.clone(), id.clone(), id.clone());
    let (pin_nav, pin_new) = (pin.clone(), pin.clone());
    let watched = Arc::new(AtomicBool::new(false));
    let watched_load = watched.clone();
    let blank = Url::parse("about:blank").expect("a constant URL");
    let web = WebviewBuilder::new(web_label(&id), WebviewUrl::External(start.unwrap_or(blank)))
        .on_navigation(move |url| {
            let pinned = pin_nav.lock().unwrap().clone();
            match team_nav(url, pinned.as_ref()) {
                TeamNav::Allow => true,
                // The web view turns away: a load it was waiting for
                // ends here on purpose, and is not retried.
                TeamNav::Browser => {
                    web_load(&id_nav, WebLoad::handed_off);
                    open_gated(&app_nav, &gate_nav, url);
                    false
                }
                TeamNav::DesktopLogin => {
                    web_load(&id_nav, WebLoad::handed_off);
                    start_desktop_login(&app_nav, &id_nav, url);
                    false
                }
                TeamNav::Deny => false,
            }
        })
        .on_new_window(move |url, _features| {
            open_new_window(&app_new, &id_new, &pin_new, &gate_new, url);
            NewWindowResponse::Deny
        })
        .on_page_load(move |_w, payload| {
            if payload.event() == PageLoadEvent::Started {
                web_load(&id_load, |w| w.started(Instant::now()));
                arm_retry(&app_load, &id_load);
            }
            if payload.event() == PageLoadEvent::Finished {
                let path = payload.url().path().to_string();
                // Where a failed load finishes too, platform::watch_loads
                // says how each one ended.
                if !watched_load.load(Ordering::SeqCst) {
                    load_ended(&app_load, &id_load, platform::LoadEnd::Shown);
                }
                shell::connect_page_loaded(&app_load, &id_load, &path);
                // A page of the team's app (a sign-in just finished, say):
                // what the shell shows about it may have changed.
                if !path.starts_with("/auth/") && path != "/login" {
                    crate::teamapi::refresh(&app_load, &id_load);
                }
            }
        })
        .auto_resize();
    // Frames: on macOS WKWebView asks wry about them and `on_navigation`
    // above decides; on Windows wry asks about the page only, and the
    // guard hooks WebView2's own frame event.
    let (app_end, id_end) = (app.clone(), id.clone());
    let ended = Box::new(move |end| load_ended(&app_end, &id_end, end));
    match add_team_web(&window, web, size, frame_guard(web_label(&id), pin), ended, watched) {
        Ok(web) => crate::menus::hook_window_shortcuts(app, &web, &id),
        Err(e) => eprintln!("kivali: cannot open {}'s web app: {e}", t.name),
    }
    arm_retry(app, &id);
    let page = WebviewBuilder::new(page_label(&id), WebviewUrl::App(format!("index.html#/team/{id}").into()))
        .on_navigation(page_may_load)
        .on_page_load(heal_page_loads())
        .auto_resize();
    match window.add_child(page, LogicalPosition::new(0.0, 0.0), size) {
        Ok(page) => crate::menus::hook_window_shortcuts(app, &page, &id),
        Err(e) => eprintln!("kivali: cannot open {}'s page: {e}", t.name),
    }
    let app2 = app.clone();
    let w2 = window.clone();
    window.on_window_event(move |event| match event {
        tauri::WindowEvent::CloseRequested { api, .. } => {
            // Closing a window hides it; the team keeps running. Its
            // browser windows close with it.
            api.prevent_close();
            let _ = w2.hide();
            close_browsers(&app2, &id);
            sync_dock(&app2, Some(w2.label()));
        }
        tauri::WindowEvent::Focused(f) => {
            gate_focus.lock().unwrap().focus(*f, Instant::now());
            if *f {
                *app2.state::<Shell>().front_team.lock().unwrap() = Some(id.clone());
                crate::menus::refresh(&app2);
                crate::teamapi::refresh(&app2, &id);
            }
        }
        _ => {}
    });
    Some(window)
}

/// A new window a team's page asks for (`window.open`, a link with a
/// target), always refused as a new window. A web subagent's browser
/// page ([`is_browser_path`]) opens in its own window ([`show_browser`]);
/// any other page of the team's own origin (an attachment the app opens
/// in a new tab, say) loads in the team's web view, where the session
/// is; the browser has none. Both from another thread, as the desktop
/// sign-in is: inside the callback that is refusing the new window, the
/// load would run first. Any other link goes to the system browser.
fn open_new_window(app: &AppHandle, id: &str, pin: &Mutex<Option<url::Origin>>, gate: &Mutex<BrowserGate>, url: Url) {
    if team_nav(&url, pin.lock().unwrap().as_ref()) != TeamNav::Allow {
        open_gated(app, gate, &url);
        return;
    }
    let (app, id) = (app.clone(), id.to_string());
    std::thread::spawn(move || {
        if is_browser_path(url.path()) {
            show_browser(&app, &id, url);
        } else if let Some(w) = app.get_webview(&web_label(&id)) {
            let _ = w.navigate(url);
        }
    });
}

/// Opens team `id`'s browser window for `url`, a web subagent's browser
/// page of the team's origin, or brings the one open for that page to
/// the front. Its webview uses the app's one website data store, as the
/// team's web view does, so the team's session cookie signs it in. It is
/// pinned to the team's origin as the team's web view is (the same pin
/// cell, `team_nav`, `frame_guard`; a gate of its own for the system
/// browser), has no IPC (no capability names `browser-*`), and closes
/// with the team's window. Called off the main thread.
fn show_browser(app: &AppHandle, id: &str, url: Url) {
    let label = browser_label(id, url.path());
    if let Some(w) = app.get_webview_window(&label) {
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
        return;
    }
    to_dock(app);
    let pin = pin_of(app, id);
    let gate = Arc::new(Mutex::new(BrowserGate::default()));
    gate.lock().unwrap().focus(true, Instant::now());
    let (gate_nav, gate_new, gate_focus) = (gate.clone(), gate.clone(), gate);
    let (app_nav, app_new) = (app.clone(), app.clone());
    let (pin_nav, pin_new) = (pin.clone(), pin.clone());
    let (label_nav, id_new) = (label.clone(), id.to_string());
    let blank = Url::parse("about:blank").expect("a constant URL");
    let built = WebviewWindowBuilder::new(app, &label, WebviewUrl::External(blank))
        .title("Browser")
        .inner_size(1320.0, 900.0)
        .resizable(true)
        .on_navigation(move |url| {
            let pinned = pin_nav.lock().unwrap().clone();
            match team_nav(url, pinned.as_ref()) {
                TeamNav::Allow => true,
                TeamNav::Browser => {
                    open_gated(&app_nav, &gate_nav, url);
                    false
                }
                // Signing in is the team window's: refused here.
                TeamNav::DesktopLogin => {
                    eprintln!("kivali: {label_nav} refused the team's sign-in page");
                    false
                }
                TeamNav::Deny => false,
            }
        })
        .on_new_window(move |url, _features| {
            open_new_window(&app_new, &id_new, &pin_new, &gate_new, url);
            NewWindowResponse::Deny
        })
        .build();
    let w = match built {
        Ok(w) => w,
        Err(e) => return eprintln!("kivali: cannot open the {label} window: {e}"),
    };
    let (app_ev, label_ev) = (app.clone(), label.clone());
    w.on_window_event(move |event| match event {
        tauri::WindowEvent::Focused(f) => gate_focus.lock().unwrap().focus(*f, Instant::now()),
        tauri::WindowEvent::Destroyed => sync_dock(&app_ev, Some(&label_ev)),
        _ => {}
    });
    // The page loads once its frames are guarded (Windows): on the main
    // thread the guard is in before the navigation is sent.
    let web = w.as_ref().clone();
    let guard = frame_guard(label, pin);
    let _ = app.run_on_main_thread(move || {
        platform::guard_frames(&web, guard);
        let _ = web.navigate(url);
    });
}

/// Closes team `id`'s browser windows.
fn close_browsers(app: &AppHandle, id: &str) {
    for (label, w) in app.webview_windows() {
        if is_browser_label(&label) && team_of(&label) == Some(id) {
            let _ = w.destroy();
        }
    }
}

/// Kivali is in the Dock only while one of its windows shows; with none,
/// it lives in the menu bar alone (macOS's accessory policy). `hiding` is
/// a window on its way out, not counted.
pub fn sync_dock(app: &AppHandle, hiding: Option<&str>) {
    #[cfg(target_os = "macos")]
    {
        let any = app
            .windows()
            .into_iter()
            // A minimised window reports itself not visible, yet lives in
            // the Dock: it keeps the app there.
            .any(|(label, w)| {
                Some(label.as_str()) != hiding && (w.is_visible().unwrap_or(false) || w.is_minimized().unwrap_or(false))
            });
        let policy = if any { tauri::ActivationPolicy::Regular } else { tauri::ActivationPolicy::Accessory };
        let _ = app.set_activation_policy(policy);
    }
    #[cfg(not(target_os = "macos"))]
    let _ = (app, hiding);
}

/// Before a window shows: back in the Dock.
fn to_dock(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    let _ = app.set_activation_policy(tauri::ActivationPolicy::Regular);
    #[cfg(not(target_os = "macos"))]
    let _ = app;
}

/// Opens a team's window, or brings it to the front.
pub fn show_team(app: &AppHandle, id: &str) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    {
        let mut teams = sh.teams.lock().unwrap();
        if teams.get(id).is_some() && teams.last_open.as_deref() != Some(id) {
            teams.last_open = Some(id.to_string());
            drop(teams);
            let _ = sh.save_teams();
        }
    }
    if !t.is_here() {
        shell::check_reach(app, id);
    }
    to_dock(app);
    let w = match app.get_window(&team_label(id)) {
        Some(w) => w,
        None => match create_team_window(app, &t, true) {
            Some(w) => w,
            None => return,
        },
    };
    *sh.front_team.lock().unwrap() = Some(id.to_string());
    sync_team(app, &t);
    let _ = w.show();
    let _ = w.unminimize();
    let _ = w.set_focus();
}

/// The server's one-time sign-in route (internal/auth/handoff.go).
const HANDOFF_PATH: &str = "/auth/handoff";

/// `<origin>/auth/handoff?t=<token>`. The token is a secret: never logged.
fn handoff_url(origin: &str, token: &str) -> Option<Url> {
    let mut u = Url::parse(&format!("{origin}{HANDOFF_PATH}")).ok()?;
    u.query_pairs_mut().append_pair("t", token);
    Some(u)
}

/// Setup's "Open <team>": [`show_team`], then, for a team here that
/// shows its web app, the web view trades a one-time token from the
/// team's supervisor for a session as the owner who signed in during
/// setup (docs/developers/auth.md, Desktop handoff), so the first open needs no
/// second sign-in ([`send_handoff`]). On any failure to get a token the
/// window simply stays as `show_team` left it.
pub fn show_team_signed_in(app: &AppHandle, id: &str) {
    show_team(app, id);
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    if !t.is_here() || !shows_web(&sh, &t) {
        return;
    }
    send_handoff(app, id, false);
}

/// A hidden window for the team being connected, loading its own
/// sign-in; it shows once the sign-in worked.
pub fn start_connect_signin(app: &AppHandle, id: &str) {
    let sh = app.state::<Shell>();
    let Some(t) = sh.team(id) else { return };
    if app.get_window(&team_label(id)).is_none() && create_team_window(app, &t, false).is_none() {
        return;
    }
    let Some(origin) = t.origin() else { return };
    let Ok(login) = Url::parse(&format!("{origin}{}?next=%2F", signin::LOGIN_PATH)) else { return };
    let app2 = app.clone();
    let id = id.to_string();
    std::thread::spawn(move || {
        *pin_of(&app2, &id).lock().unwrap() = Some(login.origin());
        if let Some(w) = app2.get_webview(&web_label(&id)) {
            let _ = w.navigate(login);
        }
    });
}

/// Whether a team shows its own web app now; otherwise its Kivali page.
fn shows_web(sh: &Shell, t: &Team) -> bool {
    let (state, _) = sh.team_state(t);
    match t.place {
        Place::Here(_) => state == TeamState::Running && t.origin().is_some(),
        Place::Elsewhere { .. } => state != TeamState::Paused || sh.connecting.lock().unwrap().as_ref().is_some_and(|c| c.id == t.id),
    }
}

/// Puts a team's window in step with the team: the web app or the page,
/// on the right origin, under the right title. Returns whether it sent
/// the web view to the team's origin.
pub fn sync_team(app: &AppHandle, t: &Team) -> bool {
    let Some(window) = app.get_window(&team_label(&t.id)) else { return false };
    let sh = app.state::<Shell>();
    let _ = window.set_title(&team_title(t));
    let (Some(web), Some(page)) = (app.get_webview(&web_label(&t.id)), app.get_webview(&page_label(&t.id))) else {
        return false;
    };
    let mut sent = false;
    if shows_web(&sh, t) {
        if let Some(origin) = team_root(t) {
            let pin = pin_of(app, &t.id);
            let moved = pin.lock().unwrap().as_ref() != Some(&origin.origin());
            // Only a web view that has loaded a page has a URL to read: one
            // made for a paused team never loads its `about:blank` (Tauri
            // skips it), a load the server refuses never finishes on macOS,
            // and a sign-in that refuses the first page leaves it with
            // none; on macOS wry panics reading a URL that is not there.
            // The URL is read outside the lock: reading it runs on the
            // main thread, where page loads take the lock.
            let blank = web_load(&t.id, |w| w.loaded) && web.url().map(|u| u.scheme() == "about").unwrap_or(false);
            if web_load(&t.id, |w| w.sync(true, moved, blank, Instant::now())) {
                // The first start, a port that changed, a load from before
                // the team served (WebLoad), or an `about:` page.
                *pin.lock().unwrap() = Some(origin.origin());
                let _ = web.navigate(origin);
                arm_retry(app, &t.id);
                sent = true;
            }
        }
        // The page stays until the web view has a page to show (WebLoad);
        // its load calls back here.
        if web_load(&t.id, |w| w.loaded) {
            let _ = web.show();
            let _ = page.hide();
        }
    } else {
        web_load(&t.id, |w| w.sync(false, false, false, Instant::now()));
        let _ = page.show();
        let _ = web.hide();
    }
    sent
}

/// Every team window in step; windows of teams that are gone, closed.
pub fn sync_all(app: &AppHandle) {
    let sh = app.state::<Shell>();
    let mut teams = sh.teams.lock().unwrap().teams.clone();
    if let Some(c) = sh.connecting.lock().unwrap().clone() {
        teams.push(c);
    }
    for t in &teams {
        sync_team(app, t);
    }
    for (label, w) in app.windows() {
        if let Some(id) = label.strip_prefix(TEAM_PREFIX) {
            if !teams.iter().any(|t| t.id == id) {
                let _ = w.destroy();
            }
        }
    }
}

pub fn close_team_window(app: &AppHandle, id: &str) {
    if let Some(w) = app.get_window(&team_label(id)) {
        let _ = w.destroy();
    }
    close_browsers(app, id);
    sync_dock(app, Some(&team_label(id)));
    let sh = app.state::<Shell>();
    sh.pins.lock().unwrap().remove(id);
    sh.pending_signins.lock().unwrap().remove(id);
    let mut front = sh.front_team.lock().unwrap();
    if front.as_deref() == Some(id) {
        *front = None;
    }
}

/// The page window title for a setup route.
fn setup_title(route: &str) -> &'static str {
    match route {
        "welcome" => "Welcome to Kivali",
        "add" => "Add a team",
        "connect" => "Connect to a team",
        _ => "New team",
    }
}

fn page_window(app: &AppHandle, label: &str, hash: &str, title: &str, size: (f64, f64), resizable: bool) {
    to_dock(app);
    if let Some(w) = app.get_webview_window(label) {
        if let Ok(mut u) = w.url() {
            u.set_fragment(Some(hash));
            let _ = w.navigate(u);
        }
        let _ = w.set_title(title);
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
        return;
    }
    let mut b = WebviewWindowBuilder::new(app, label, WebviewUrl::App(format!("index.html#{hash}").into()))
        .title(title)
        .inner_size(size.0, size.1)
        .resizable(resizable)
        .on_navigation(page_may_load)
        .on_page_load({
            let heal = heal_page_loads();
            move |w: WebviewWindow, p| heal(w.as_ref().clone(), p)
        });
    if resizable {
        b = b.min_inner_size(size.0, size.1).max_inner_size(size.0, 4000.0);
    }
    match b.build() {
        Ok(w) => {
            hide_on_close(&w);
            if label == SETUP {
                // Setup closed before it finished: the machine it was
                // preparing goes (a team it already made stays, its own
                // Back and Try again remove that).
                let app2 = app.clone();
                w.on_window_event(move |event| {
                    if let tauri::WindowEvent::CloseRequested { .. } = event {
                        shell::discard_prepared_later(&app2);
                    }
                });
            }
        }
        Err(e) => eprintln!("kivali: cannot open the {label} window: {e}"),
    }
}

/// Opens the setup window at `route`: `welcome`, `add` or `connect`.
pub fn show_setup(app: &AppHandle, route: &str) {
    let hash = match route {
        "connect" => "/connect".to_string(),
        r => format!("/setup/{r}"),
    };
    page_window(app, SETUP, &hash, setup_title(route), (720.0, 640.0), false);
}

pub fn close_setup(app: &AppHandle) {
    if let Some(w) = app.get_webview_window(SETUP) {
        let _ = w.hide();
    }
    sync_dock(app, Some(SETUP));
}

/// Opens Settings at `route` (`general`, `team/<id>[/<tab>]`, `advanced`).
pub fn show_settings(app: &AppHandle, route: &str) {
    page_window(app, SETTINGS, &format!("/settings/{route}"), "Settings", (880.0, 640.0), true);
}

/// Closing a page window hides it; Kivali keeps running in the tray.
fn hide_on_close(w: &WebviewWindow) {
    let w2 = w.clone();
    w.on_window_event(move |event| {
        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = w2.hide();
            sync_dock(w2.app_handle(), Some(w2.label()));
        }
    });
}

/// The team window in front, if a team window is.
pub fn front_team_window(app: &AppHandle) -> Option<(String, Window)> {
    let id = app.state::<Shell>().front_team.lock().unwrap().clone()?;
    let w = app.get_window(&team_label(&id))?;
    w.is_visible().ok().filter(|v| *v)?;
    Some((id, w))
}

/// The window a sheet should hang from: the focused one of ours.
pub fn front_window(app: &AppHandle) -> Option<Window> {
    app.windows().into_values().find(|w| w.is_focused().unwrap_or(false))
}

/// Opens what "Open Kivali" means: the last team's window, or setup.
pub fn open_kivali(app: &AppHandle) {
    // Setup under way (showing, or hidden with no team to open): bring it
    // back as it is. Navigating it would start it over, say when the Dock
    // icon is clicked on the way back from the browser's Google sign-in.
    if let Some(w) = app.get_webview_window(SETUP) {
        let no_teams = app.state::<Shell>().teams.lock().unwrap().teams.is_empty();
        if w.is_visible().unwrap_or(false) || no_teams {
            to_dock(app);
            let _ = w.show();
            let _ = w.unminimize();
            let _ = w.set_focus();
            return;
        }
    }
    let sh = app.state::<Shell>();
    let last = {
        let t = sh.teams.lock().unwrap();
        t.last_open.clone().filter(|id| t.get(id).is_some()).or_else(|| t.teams.first().map(|t| t.id.clone()))
    };
    match last {
        Some(id) => show_team(app, &id),
        None => show_setup(app, "welcome"),
    }
}

/// The webview a team's window shows now: its web app or its page.
fn shown_view(app: &AppHandle, id: &str) -> Option<tauri::Webview> {
    [web_label(id), page_label(id)].iter().filter_map(|l| app.get_webview(l)).find(|w| w.is_visible_hint())
}

/// View menu (macOS): reload the team in front's web app (or its page).
pub fn reload_front(app: &AppHandle) {
    if let Some((id, _)) = front_team_window(app) {
        reload_team(app, &id);
    }
}

/// View → Reload in a team's window: its web app, or its page. A team
/// that serves but whose web view has no page to show has its web view
/// sent to the origin (or a failed handoff sent again) at once, unless
/// `sync_team` just sent it or a load is in flight (the signed-in
/// handoff, a page arriving), which is left to finish
/// ([`WebLoad::reload`]).
pub fn reload_team(app: &AppHandle, id: &str) {
    let sh = app.state::<Shell>();
    if let Some(t) = sh.team(id) {
        if sync_team(app, &t) {
            return;
        }
        if shows_web(&sh, &t) && !web_load(id, |w| w.loaded) {
            let pinned = pin_of(app, id).lock().unwrap().clone();
            if let (Some(web), Some(origin)) = (app.get_webview(&web_label(id)), team_root(&t)) {
                if pinned.as_ref() == Some(&origin.origin()) {
                    if let Some(how) = web_load(id, |w| w.reload(Instant::now())) {
                        send_again(app, id, &web, origin, how);
                    }
                    return;
                }
            }
        }
    }
    if let Some(w) = shown_view(app, id) {
        let _ = w.reload();
    }
}

/// The keyboard focus into what a team's window shows, before its Edit
/// menu types a shortcut there (Windows).
pub fn focus_team_view(app: &AppHandle, id: &str) {
    if let Some(w) = shown_view(app, id) {
        let _ = w.set_focus();
    }
}

/// File → Close window in a team's window: hidden, as its close box
/// does; the team keeps running.
pub fn hide_team(app: &AppHandle, id: &str) {
    if let Some(w) = app.get_window(&team_label(id)) {
        let _ = w.close();
    }
}

trait VisibleHint {
    fn is_visible_hint(&self) -> bool;
}

impl VisibleHint for tauri::Webview {
    /// Webviews report no visibility; the team's state says which shows.
    fn is_visible_hint(&self) -> bool {
        let label = self.label().to_string();
        let app = self.app_handle();
        let Some(id) = team_of(&label) else { return true };
        let sh = app.state::<Shell>();
        let Some(t) = sh.team(id) else { return false };
        shows_web(&sh, &t) == label.starts_with(WEB_PREFIX)
    }
}

/// View menu (macOS): Actual size (None), Zoom in (+1), Zoom out (-1)
/// for the team in front.
pub fn zoom_front(app: &AppHandle, step: Option<i32>) {
    if let Some((id, _)) = front_team_window(app) {
        zoom_team(app, &id, step);
    }
}

/// View menu: Actual size (None), Zoom in (+1), Zoom out (-1) for a
/// team's window, both its webviews.
pub fn zoom_team(app: &AppHandle, id: &str, step: Option<i32>) {
    let sh = app.state::<Shell>();
    let mut z = sh.zoom.lock().unwrap();
    let level = z.entry(id.to_string()).or_insert(1.0);
    *level = match step {
        None => 1.0,
        Some(s) => (*level + 0.1 * f64::from(s)).clamp(0.5, 2.0),
    };
    let level = *level;
    drop(z);
    for label in [web_label(id), page_label(id)] {
        if let Some(w) = app.get_webview(&label) {
            let _ = w.set_zoom(level);
        }
    }
}

pub fn toggle_fullscreen(app: &AppHandle) {
    if let Some(w) = front_window(app) {
        let full = w.is_fullscreen().unwrap_or(false);
        let _ = w.set_fullscreen(!full);
    }
}

/// Opens a path of a team's web app in its window (Other devices' "Open Team page").
pub fn open_team_path(app: &AppHandle, id: &str, path: &str) {
    show_team(app, id);
    let Some(t) = app.state::<Shell>().team(id) else { return };
    let Some(origin) = t.origin() else { return };
    if !path.starts_with('/') || path.starts_with("//") {
        return;
    }
    if let (Ok(u), Some(w)) = (Url::parse(&format!("{origin}{path}")), app.get_webview(&web_label(id))) {
        let _ = w.navigate(u);
    }
}

/// Sign out: the team's own sign-out, in its window.
pub fn sign_out_team(app: &AppHandle, id: &str) {
    open_team_path(app, id, "/auth/logout");
}

#[cfg(test)]
mod tests {
    use super::*;

    const ORIGIN: Option<Resend> = Some(Resend::Origin);

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    #[test]
    fn web_view_made_before_the_team_serves_goes_to_the_origin_when_it_does() {
        // macOS: the refused load never finishes, and nothing retries it
        // while the team does not serve.
        let t0 = Instant::now();
        let mut w = WebLoad::new(false, t0);
        assert_eq!(w.retry_at, None);
        assert!(!w.sync(false, false, false, t0));
        assert_eq!(w.retry(t0 + secs(3600)), None);
        assert!(w.sync(true, false, false, t0));
        assert!(!w.loaded);
        assert_eq!(w.retry_at, Some(t0 + patience(0)));
        // Syncing again leaves the load to its retries.
        assert!(!w.sync(true, false, false, t0 + secs(1)));
        // Stopped and started again: sent again, its retries from scratch.
        assert!(!w.sync(false, false, false, t0 + secs(2)));
        assert_eq!(w.retry_at, None);
        assert!(w.sync(true, false, false, t0 + secs(3)));
        w.finished();
        assert!(w.loaded && w.loaded_serving);
        assert_eq!(w.retry_at, None);
        assert!(!w.sync(true, false, false, t0 + secs(4)));
    }

    #[test]
    fn page_from_before_the_team_served_is_replaced() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(false, t0);
        w.finished();
        assert!(w.loaded && !w.loaded_serving);
        assert!(!w.sync(false, false, false, t0));
        assert!(w.sync(true, false, false, t0));
        assert!(!w.loaded);
        assert!(!w.sync(true, false, false, t0));
    }

    #[test]
    fn error_page_from_before_the_team_served_is_replaced() {
        // Windows: the refused load finishes on an error page, reported
        // failed; with the team not serving, nothing is retried.
        let t0 = Instant::now();
        let mut w = WebLoad::new(false, t0);
        w.failed(t0);
        assert!(!w.loaded && w.retry_at.is_none());
        assert!(w.sync(true, false, false, t0 + secs(5)));
        assert_eq!(w.retry_at, Some(t0 + secs(5) + patience(0)));
    }

    #[test]
    fn web_view_made_while_serving_is_left_to_load() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        assert!(w.retry_at.is_some() && !w.in_flight(t0));
        assert!(!w.sync(true, false, false, t0));
        w.started(t0 + secs(1));
        w.finished();
        assert!(!w.sync(true, false, false, t0 + secs(2)));
        // A page loaded while serving survives a pause on the same port.
        assert!(!w.sync(false, false, false, t0 + secs(3)));
        assert!(!w.sync(true, false, false, t0 + secs(4)));
        assert!(w.loaded);
        assert_eq!(w.retry(t0 + secs(3600)), None);
    }

    #[test]
    fn moved_or_blank_web_view_goes_to_the_origin() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.finished();
        assert!(w.sync(true, true, false, t0));
        w.finished();
        assert!(w.sync(true, false, true, t0));
        assert!(!w.loaded);
    }

    #[test]
    fn load_that_never_finishes_is_sent_again_with_backoff() {
        // macOS: a window made while the team serves (reachability not
        // yet known), its first load failing silently; the team keeps
        // serving, so only the retries bring the page.
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        assert!(!w.sync(true, false, false, t0));
        assert_eq!(w.retry(t0 + patience(0) - secs(1)), None);
        let mut at = t0 + patience(0);
        assert_eq!(w.retry(at), ORIGIN);
        assert_eq!(w.tries, 1);
        // Each wait is longer, up to a minute.
        let mut waits = vec![];
        for _ in 0..6 {
            let next = w.retry_at.unwrap();
            waits.push(next - at);
            assert_eq!(w.retry(next - secs(1)), None);
            assert_eq!(w.retry(next), ORIGIN);
            at = next;
        }
        assert_eq!(waits, vec![secs(20), secs(40), secs(60), secs(60), secs(60), secs(60)]);
        // A page arrives: no more retries, and the next failure starts
        // the backoff again.
        w.finished();
        assert_eq!(w.retry(at + secs(3600)), None);
        assert_eq!(w.tries, 0);
    }

    #[test]
    fn page_arriving_restarts_the_patience() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.started(t0 + secs(8));
        assert_eq!(w.retry(t0 + patience(0)), None);
        assert!(w.in_flight(t0 + patience(0)));
        assert_eq!(w.retry(t0 + secs(8) + patience(0)), ORIGIN);
    }

    #[test]
    fn reported_failure_is_retried_soon_with_backoff() {
        // Windows: the load finishes on an error page, reported failed.
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.started(t0);
        w.failed(t0 + secs(1));
        assert!(!w.in_flight(t0 + secs(1)));
        assert_eq!(w.retry_at, Some(t0 + secs(1) + retry_delay(0)));
        let mut at = t0 + secs(1);
        let mut waits = vec![];
        for _ in 0..7 {
            let next = w.retry_at.unwrap();
            waits.push(next - at);
            assert_eq!(w.retry(next - Duration::from_millis(1)), None);
            assert_eq!(w.retry(next), ORIGIN);
            w.failed(next);
            at = next;
        }
        assert_eq!(waits, vec![secs(1), secs(2), secs(4), secs(8), secs(16), secs(30), secs(30)]);
        w.finished();
        assert!(w.loaded && w.retry_at.is_none());
    }

    #[test]
    fn retries_stop_when_the_team_stops_serving() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.failed(t0);
        assert!(!w.sync(false, false, false, t0));
        assert_eq!(w.retry(t0 + secs(3600)), None);
        // A failure reported while not serving schedules nothing.
        w.failed(t0 + secs(1));
        assert_eq!(w.retry_at, None);
    }

    #[test]
    fn failure_of_a_shown_page_is_left_alone() {
        // A page the person navigated to failing is theirs to reload.
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.finished();
        w.failed(t0 + secs(1));
        assert!(w.loaded && w.retry_at.is_none());
    }

    #[test]
    fn load_handed_off_to_a_sign_in_is_not_retried() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.handed_off();
        w.started(t0 + secs(1));
        w.failed(t0 + secs(2));
        assert_eq!(w.retry(t0 + secs(3600)), None);
        // Reload sends it again.
        assert_eq!(w.reload(t0 + secs(3600)), ORIGIN);
    }

    #[test]
    fn reload_sends_at_once_unless_a_load_is_in_flight() {
        let t0 = Instant::now();
        // Nothing arriving yet (macOS: it may have failed unseen): Reload
        // sends it at once, the backoff from scratch.
        let mut w = WebLoad::new(true, t0);
        w.tries = 4;
        assert_eq!(w.reload(t0 + secs(1)), ORIGIN);
        assert_eq!(w.tries, 0);
        assert_eq!(w.retry_at, Some(t0 + secs(1) + patience(0)));
        // A page arriving is left to finish, until its patience runs out.
        w.started(t0 + secs(2));
        assert_eq!(w.reload(t0 + secs(3)), None);
        assert_eq!(w.reload(t0 + secs(2) + patience(0)), ORIGIN);
        // Failed (WebView2 says so): sent at once.
        w.started(t0 + secs(30));
        w.failed(t0 + secs(31));
        assert_eq!(w.reload(t0 + secs(31)), ORIGIN);
        // A page shows: Reload reloads it, not the web view's origin.
        w.finished();
        assert_eq!(w.reload(t0 + secs(3600)), None);
    }

    #[test]
    fn handoff_is_left_to_finish_and_sent_again_as_a_handoff() {
        let t0 = Instant::now();
        let mut w = WebLoad::new(true, t0);
        w.sending_handoff(t0 + secs(1));
        // In flight before anything arrives: Reload leaves it.
        assert_eq!(w.reload(t0 + secs(2)), None);
        // It never finishes: retried as a handoff, with a new token.
        assert_eq!(w.retry(t0 + secs(1) + patience(0)), Some(Resend::Handoff));
        assert!(w.handoff && w.tries == 1);
        // Reported failed: Reload sends a handoff at once.
        w.failed(t0 + secs(20));
        assert_eq!(w.reload(t0 + secs(20)), Some(Resend::Handoff));
        // Sent to the origin instead (no token to be had): plain retries.
        w.sending(t0 + secs(21));
        assert_eq!(w.retry(t0 + secs(21) + patience(0)), ORIGIN);
        assert_eq!(w.finished(), 1);
        assert!(!w.handoff);
    }

    #[test]
    fn retries_are_logged_now_and_then() {
        let logged: Vec<u32> = (1..=40).filter(|&n| logs_retry(n)).collect();
        assert_eq!(logged, vec![1, 10, 20, 30, 40]);
    }

    fn u(s: &str) -> Url {
        Url::parse(s).unwrap()
    }

    #[test]
    fn team_window_is_pinned_to_its_team() {
        let local = u("http://127.0.0.1:8080/").origin();
        let pin = Some(&local);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/agents?x=1#y"), pin), TeamNav::Allow);
        assert_eq!(team_nav(&u("blob:http://127.0.0.1:8080/4f1c"), pin), TeamNav::Allow);
        assert_eq!(team_nav(&u("about:blank"), pin), TeamNav::Allow);
        assert_eq!(team_nav(&u("about:srcdoc"), pin), TeamNav::Allow);
        // Same host, other port or scheme: another origin (another team).
        assert_eq!(team_nav(&u("http://127.0.0.1:8081/"), pin), TeamNav::Browser);
        assert_eq!(team_nav(&u("https://127.0.0.1:8080/"), pin), TeamNav::Browser);
        assert_eq!(team_nav(&u("http://localhost:8080/"), pin), TeamNav::Browser);
        assert_eq!(team_nav(&u("https://kivali.example.com/"), pin), TeamNav::Browser);
        assert_eq!(team_nav(&u("https://accounts.google.com/o/oauth2/v2/auth"), pin), TeamNav::Browser);
        // Never anywhere: the app's origin, other schemes, foreign blobs.
        assert_eq!(team_nav(&u("tauri://localhost/index.html"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("https://tauri.localhost/index.html"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("http://ipc.localhost/open_team"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("file:///etc/passwd"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("data:text/html,hi"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("about:config"), pin), TeamNav::Deny);
        assert_eq!(team_nav(&u("blob:https://evil.example.com/1"), pin), TeamNav::Deny);
        if cfg!(debug_assertions) {
            assert_eq!(team_nav(&u("http://localhost:1420/"), pin), TeamNav::Deny);
        }
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/auth/login"), pin), TeamNav::DesktopLogin);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/auth/login?next=%2Fagents"), pin), TeamNav::DesktopLogin);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/auth/login?client=web&client=desktop"), pin), TeamNav::DesktopLogin);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/auth/login?client=desktop&client=web"), pin), TeamNav::Allow);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/auth/callback?code=x&state=desktop~y"), pin), TeamNav::Allow);
        assert_eq!(team_nav(&u("https://kivali.example.com/auth/login"), pin), TeamNav::Browser);
        assert_eq!(team_nav(&u("http://127.0.0.1:8080/"), None), TeamNav::Browser);
    }

    #[test]
    fn team_frames_load_only_from_the_pin() {
        let local = u("http://127.0.0.1:8080/").origin();
        let pin = Some(&local);
        for ok in [
            "http://127.0.0.1:8080/",
            "http://127.0.0.1:8080/files/view?id=3#p2",
            "http://127.0.0.1:8080/auth/login?client=desktop",
            "about:blank",
            "about:srcdoc",
            "blob:http://127.0.0.1:8080/4f1c",
        ] {
            assert!(frame_may_load(pin, &u(ok)), "{ok} should load in a frame");
        }
        for refused in [
            // Another team here, the same host by another name or scheme.
            "http://127.0.0.1:8081/",
            "https://127.0.0.1:8080/",
            "http://localhost:8080/",
            "https://kivali.example.com/",
            "https://accounts.google.com/o/oauth2/v2/auth",
            // A frame never starts the desktop sign-in.
            "http://127.0.0.1:8080/auth/login?next=%2F",
            "file:///C:/Windows/win.ini",
            "data:text/html,<p>hi",
            "tauri://localhost/index.html",
            "http://tauri.localhost/index.html",
            "http://ipc.localhost/open_team",
            "blob:https://evil.example.com/1",
            "about:config",
            "javascript:alert(1)",
        ] {
            assert!(!frame_may_load(pin, &u(refused)), "{refused} should be refused in a frame");
        }
        // No pin yet: only the empty frames.
        assert!(frame_may_load(None, &u("about:srcdoc")));
        assert!(!frame_may_load(None, &u("http://127.0.0.1:8080/")));
    }

    #[test]
    fn frame_guard_reads_the_pin_as_it_is_now() {
        let pin: Arc<Mutex<Option<url::Origin>>> = Arc::default();
        let guard = frame_guard("web-x".into(), pin.clone());
        assert!(!guard("http://127.0.0.1:8080/"));
        assert!(guard("about:blank"));
        *pin.lock().unwrap() = Some(u("http://127.0.0.1:8080/").origin());
        assert!(guard("http://127.0.0.1:8080/agents"));
        // The port moved: the old origin is another team's now.
        *pin.lock().unwrap() = Some(u("http://127.0.0.1:8081/").origin());
        assert!(!guard("http://127.0.0.1:8080/agents"));
        assert!(guard("http://127.0.0.1:8081/agents"));
        assert!(!guard("not a url"));
        assert!(!guard(""));
        assert_eq!(origin_for_log(&u("https://accounts.google.com/o?state=s")), "https://accounts.google.com");
        assert_eq!(origin_for_log(&u("data:text/html,secret")), "data:");
        assert_eq!(origin_for_log(&u("tauri://localhost/index.html")), "tauri:");
    }

    #[test]
    fn handoff_loads_in_the_team_window() {
        let target = handoff_url("http://127.0.0.1:8080", "eyJl+/=.c2ln").unwrap();
        assert_eq!(target.as_str(), "http://127.0.0.1:8080/auth/handoff?t=eyJl%2B%2F%3D.c2ln");
        let pin = Some(target.origin());
        assert_eq!(team_nav(&target, pin.as_ref()), TeamNav::Allow);
    }

    #[test]
    fn browser_gate() {
        let t0 = Instant::now();
        let s = Duration::from_millis;
        let mut g = BrowserGate::default();
        assert!(!g.try_open(t0));
        g.focus(true, t0);
        assert!(g.try_open(t0));
        assert!(!g.try_open(t0 + s(999)));
        assert!(g.try_open(t0 + s(1000)));
        g.focus(false, t0 + s(1500));
        assert!(g.try_open(t0 + s(1500) + BROWSER_FOCUS_GRACE));
        assert!(!g.try_open(t0 + s(1500) + BROWSER_FOCUS_GRACE + s(1001)));
    }

    #[test]
    fn pages_load_bundled_pages_only() {
        assert!(page_may_load(&u("tauri://localhost/index.html#/setup/welcome")));
        assert!(page_may_load(&u("http://tauri.localhost/index.html#/team/x")));
        assert!(!page_may_load(&u("https://kivali.example.com/")));
        assert!(!page_may_load(&u("http://127.0.0.1:8080/")));
        assert!(!page_may_load(&u("tauri://evil/")));
        assert!(!page_may_load(&u("http://tauri.localhost:8080/")));
        assert_eq!(page_may_load(&u("http://localhost:1420/")), cfg!(debug_assertions));
    }

    #[test]
    fn labels() {
        assert_eq!(team_of("team-plainsong-3f2a"), Some("plainsong-3f2a"));
        assert_eq!(team_of("web-x"), Some("x"));
        assert_eq!(team_of("page-x"), Some("x"));
        assert_eq!(team_of("settings"), None);
        assert!(is_page_label("setup") && is_page_label("settings") && is_page_label("page-x"));
        assert!(!is_page_label("web-x") && !is_page_label("team-x"));
        assert_eq!(setup_title("welcome"), "Welcome to Kivali");
        assert_eq!(setup_title("new"), "New team");
    }

    #[test]
    fn browser_paths() {
        let is = |s: &str| is_browser_path(u(s).path());
        assert!(is("http://127.0.0.1:8080/agents/a/subagents/b/browser"));
        assert!(is("http://127.0.0.1:8080/agents/a/subagents/b/browser?tab=1#top"));
        assert!(!is("http://127.0.0.1:8080/agents/a/subagents/b/browser/x"));
        assert!(!is("http://127.0.0.1:8080/agents/a/subagents/b/browser/"));
        assert!(!is("http://127.0.0.1:8080/agents/a/subagents/browser"));
        assert!(!is("http://127.0.0.1:8080/agents//subagents/b/browser"));
        assert!(!is("http://127.0.0.1:8080/agents/a/subagents//browser"));
        assert!(!is("http://127.0.0.1:8080/x/agents/a/subagents/b/browser"));
        assert!(!is("http://127.0.0.1:8080/agents/a/subagents/b"));
        assert!(!is("http://127.0.0.1:8080/"));
        assert!(!is("about:blank"));
    }

    #[test]
    fn browser_labels() {
        let path = "/agents/a/subagents/b/browser";
        let l = browser_label("plainsong-3f2a", path);
        assert_eq!(l, browser_label("plainsong-3f2a", path));
        assert!(l.starts_with("browser-") && l.ends_with("-plainsong-3f2a"));
        assert_eq!(l.len(), "browser-".len() + BROWSER_HASH_LEN + "-plainsong-3f2a".len());
        assert_eq!(team_of(&l), Some("plainsong-3f2a"));
        assert!(is_browser_label(&l) && !is_page_label(&l));
        assert_ne!(l, browser_label("other-1b2c", path));
        assert_ne!(l, browser_label("plainsong-3f2a", "/agents/a/subagents/c/browser"));
        // Tauri allows alphanumerics, `-`, `/`, `:` and `_` in a label.
        assert!(l.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-'));
        assert_eq!(team_of("browser-x"), None);
        assert_eq!(team_of("browser-0123456789abcdef"), None);
        assert_eq!(team_of("browser-0123456789abcdef-"), None);
        assert_eq!(team_of("browser-0123456789abcdeg-x"), None);
        assert_eq!(team_of("browser-0123456789abcdef-x"), Some("x"));
    }
}
