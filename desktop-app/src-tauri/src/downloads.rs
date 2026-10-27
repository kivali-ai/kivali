//! Downloads from a team's window (docs/developers/desktop-app.md,
//! "Downloads").
//!
//! A team's web view never saves a file on its own. When one of its
//! navigations turns into a download (an attachment, a project file, a
//! backup: anything the server answers as an attachment), the web view
//! is told no ([`on_download`]) and the shell takes over: it asks where
//! to save in the system's save dialog, a sheet on the team's window
//! with the server's file name filled in, then fetches the URL itself
//! as the window's person, with the session cookie the web view holds
//! (teamapi.rs), and writes the file. While it does, a card over the
//! top right of the team's web app (`downloads-<id>`, the bundled page
//! `#/downloads/<id>`, windows.rs `sync_downloads`) shows the file, how
//! far it has got and Cancel; once saved, "Saved in Downloads" with
//! Show in folder, for [`KEEP_DONE`]; a failure stays until dismissed.
//! A download that finishes while Kivali is not the app in front is
//! also a notification.
//!
//! Only a URL of the team's own origin is fetched, and only while the
//! window is signed in there. A download the shell cannot fetch again
//! (a `blob:` URL the page made) is left to the web view, saved where
//! it would have been (the downloads folder), and only its finish shows
//! on the card.
//!
//! The file is written beside its final name as `<name>.kivali-partial`
//! and renamed into place when complete, so a cut download leaves no
//! file that looks whole; a failed or cancelled one leaves nothing.
//!
//! [`Downloads`] is the state, pure and tested; the rest drives the
//! dialog, the fetch and the card.

use crate::shell::{self, Shell};
use crate::{platform, teamapi, windows};
use serde::Serialize;
use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tauri::webview::DownloadEvent;
use tauri::{AppHandle, Manager};
use url::Url;

/// How long a saved download stays on the card after it finished.
pub const KEEP_DONE: Duration = Duration::from_secs(10);
/// How often a download's progress redraws the card, at most.
const REDRAW_EVERY: Duration = Duration::from_millis(250);
/// At most this many downloads listed per team: past it, the oldest
/// finished one goes, then the oldest.
const MAX_LISTED: usize = 6;
/// The card's size, in logical pixels: a fixed width, and a row per
/// download (pages/downloads.ts and style.css draw the same).
pub const CARD_WIDTH: f64 = 360.0;
pub const CARD_INSET: f64 = 16.0;
const CARD_PADDING: f64 = 8.0;
const ROW_HEIGHT: f64 = 64.0;
/// How long a fetch may go without a byte arriving before it counts
/// as lost. A slow connection keeps delivering; a dead one does not.
const READ_TIMEOUT: Duration = Duration::from_secs(60);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    Downloading,
    Done,
    Failed,
}

impl State {
    fn name(self) -> &'static str {
        match self {
            State::Downloading => "downloading",
            State::Done => "done",
            State::Failed => "failed",
        }
    }
}

/// One download of a team's window.
#[derive(Debug, Clone)]
pub struct Download {
    pub id: u64,
    /// The file's name, as the server suggested or the person chose.
    pub name: String,
    /// Where it is saved.
    pub path: PathBuf,
    pub state: State,
    pub received: u64,
    /// The length the server gave, when it gave one.
    pub total: Option<u64>,
    pub error: Option<String>,
    /// Set by Cancel; the fetch stops at its next chunk.
    pub cancel: Arc<AtomicBool>,
    /// A download the web view saves itself, matched to its finish by
    /// the URL it started from.
    pub webview_url: Option<String>,
    finished_at: Option<Instant>,
    drawn_at: Instant,
}

/// What the card draws for one download (types.ts `Download`).
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct DownloadView {
    pub id: u64,
    pub name: String,
    /// The folder's name ("Downloads"), for "Saved in Downloads".
    pub folder: String,
    pub state: &'static str,
    pub received: u64,
    pub total: Option<u64>,
    pub error: Option<String>,
}

/// Every team's downloads, by team id.
#[derive(Default)]
pub struct Downloads {
    next_id: u64,
    teams: HashMap<String, Vec<Download>>,
    /// Teams whose save dialog is up. One at a time per team, so a page
    /// cannot stack dialogs on the person.
    picking: HashSet<String>,
}

impl Downloads {
    /// Starts asking where to save, unless the team's dialog is already
    /// up.
    pub fn begin_pick(&mut self, team: &str) -> bool {
        self.picking.insert(team.to_string())
    }

    pub fn end_pick(&mut self, team: &str) {
        self.picking.remove(team);
    }

    /// A download under way, to `path`. `webview_url` when the web view
    /// saves it itself. Returns its id and its cancel flag.
    pub fn start(&mut self, team: &str, name: &str, path: PathBuf, webview_url: Option<String>, now: Instant) -> (u64, Arc<AtomicBool>) {
        self.next_id += 1;
        let cancel = Arc::new(AtomicBool::new(false));
        let list = self.teams.entry(team.to_string()).or_default();
        while list.len() >= MAX_LISTED {
            let at = list.iter().position(|d| d.state != State::Downloading).unwrap_or(0);
            list.remove(at);
        }
        list.push(Download {
            id: self.next_id,
            name: name.to_string(),
            path,
            state: State::Downloading,
            received: 0,
            total: None,
            error: None,
            cancel: cancel.clone(),
            webview_url,
            finished_at: None,
            drawn_at: now,
        });
        (self.next_id, cancel)
    }

    fn get(&mut self, team: &str, id: u64) -> Option<&mut Download> {
        self.teams.get_mut(team)?.iter_mut().find(|d| d.id == id)
    }

    /// More bytes arrived. Returns whether the card is due a redraw: the
    /// first bytes, then at most every [`REDRAW_EVERY`].
    pub fn progress(&mut self, team: &str, id: u64, received: u64, total: Option<u64>, now: Instant) -> bool {
        let Some(d) = self.get(team, id) else { return false };
        let first = d.received == 0 && received > 0;
        d.received = received;
        d.total = total;
        if first || now.duration_since(d.drawn_at) >= REDRAW_EVERY {
            d.drawn_at = now;
            return true;
        }
        false
    }

    /// The file is saved. Returns the download, for the notification.
    pub fn finish(&mut self, team: &str, id: u64, now: Instant) -> Option<Download> {
        let d = self.get(team, id)?;
        d.state = State::Done;
        d.error = None;
        d.finished_at = Some(now);
        if let Some(t) = d.total {
            d.received = t;
        }
        Some(d.clone())
    }

    /// The download did not finish; `error` says why, in the card's words.
    pub fn fail(&mut self, team: &str, id: u64, error: String, now: Instant) -> Option<Download> {
        let d = self.get(team, id)?;
        d.state = State::Failed;
        d.error = Some(error);
        d.finished_at = Some(now);
        Some(d.clone())
    }

    /// A download the web view saved itself ended, well or not.
    pub fn finish_webview(&mut self, team: &str, url: &str, success: bool, now: Instant) -> Option<Download> {
        let id = self.teams.get(team)?.iter().find(|d| d.webview_url.as_deref() == Some(url) && d.state == State::Downloading)?.id;
        if success {
            self.finish(team, id, now)
        } else {
            self.fail(team, id, "The download stopped partway.".into(), now)
        }
    }

    /// Cancel: the fetch stops and the download leaves the card. Only
    /// one under way can be cancelled.
    pub fn cancel(&mut self, team: &str, id: u64) -> bool {
        let Some(list) = self.teams.get_mut(team) else { return false };
        let Some(at) = list.iter().position(|d| d.id == id && d.state == State::Downloading) else { return false };
        list[at].cancel.store(true, Ordering::SeqCst);
        list.remove(at);
        true
    }

    /// Dismiss: a finished download leaves the card.
    pub fn dismiss(&mut self, team: &str, id: u64) {
        if let Some(list) = self.teams.get_mut(team) {
            list.retain(|d| d.id != id || d.state == State::Downloading);
        }
    }

    /// Removes the saved downloads that have shown for [`KEEP_DONE`].
    /// Returns whether any left.
    pub fn expire(&mut self, now: Instant) -> bool {
        let mut any = false;
        for list in self.teams.values_mut() {
            let before = list.len();
            list.retain(|d| !(d.state == State::Done && d.finished_at.is_some_and(|t| now.duration_since(t) >= KEEP_DONE)));
            any |= list.len() != before;
        }
        any
    }

    /// The saved file of a finished download, for Show in folder.
    pub fn saved_path(&self, team: &str, id: u64) -> Option<PathBuf> {
        self.teams.get(team)?.iter().find(|d| d.id == id && d.state == State::Done).map(|d| d.path.clone())
    }

    /// How many downloads the team's card lists.
    pub fn count(&self, team: &str) -> usize {
        self.teams.get(team).map_or(0, Vec::len)
    }

    /// The team's downloads as the card draws them, oldest first.
    pub fn view(&self, team: &str) -> Vec<DownloadView> {
        self.teams.get(team).map_or_else(Vec::new, |list| {
            list.iter()
                .map(|d| DownloadView {
                    id: d.id,
                    name: d.name.clone(),
                    folder: folder_name(&d.path),
                    state: d.state.name(),
                    received: d.received,
                    total: d.total,
                    error: d.error.clone(),
                })
                .collect()
        })
    }
}

/// The card's size for `n` downloads, in logical pixels.
pub fn card_size(n: usize) -> (f64, f64) {
    (CARD_WIDTH, 2.0 * CARD_PADDING + ROW_HEIGHT * n as f64)
}

/// The name of the folder a path is in: "Downloads" for
/// `~/Downloads/plan.pdf`; the path itself when it has no parent.
pub fn folder_name(path: &Path) -> String {
    path.parent()
        .and_then(|p| p.file_name())
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.display().to_string())
}

/// Where the bytes go until the download is complete.
pub fn partial_path(path: &Path) -> PathBuf {
    let name = path.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
    path.with_file_name(format!("{name}.kivali-partial"))
}

/// Whether the shell can fetch a download's URL again itself.
pub fn fetchable(url: &Url) -> bool {
    matches!(url.scheme(), "http" | "https")
}

/// The card's words for a fetch that ended with `status`.
pub fn status_error(status: u16) -> String {
    match status {
        401 | 403 => "The team window isn't signed in.".into(),
        404 => "The team no longer has this file.".into(),
        s => format!("The team answered HTTP {s}."),
    }
}

/// A team window's web view asks about a download. On the main thread,
/// inside the web view's callback, so nothing here waits.
pub fn on_download(app: &AppHandle, team: &str, webview: &tauri::Webview, event: DownloadEvent<'_>) -> bool {
    match event {
        DownloadEvent::Requested { url, destination } => {
            let name = destination
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| "download".to_string());
            let sh = app.state::<Shell>();
            if !fetchable(&url) {
                // The web view keeps this one; its finish shows on the card.
                sh.downloads.lock().unwrap().start(team, &name, destination.clone(), Some(url.to_string()), Instant::now());
                shell::changed(app);
                return true;
            }
            if !sh.downloads.lock().unwrap().begin_pick(team) {
                eprintln!("kivali: {}: a download asked while the save dialog is up was dropped", windows::web_label(team));
                return false;
            }
            ask_where(app, team, webview.window(), name, url);
            false
        }
        DownloadEvent::Finished { url, success, .. } => {
            let finished = app.state::<Shell>().downloads.lock().unwrap().finish_webview(team, url.as_str(), success, Instant::now());
            if let Some(d) = finished {
                ended(app, team, &d);
            }
            true
        }
        _ => true,
    }
}

/// The save dialog, a sheet on `parent`, in the downloads folder with
/// the server's name filled in. Save starts the fetch; Cancel ends it.
fn ask_where(app: &AppHandle, team: &str, parent: tauri::Window, name: String, url: Url) {
    use tauri_plugin_dialog::DialogExt;
    let mut dialog = app.dialog().file().set_file_name(&name).set_parent(&parent);
    if let Some(dir) = dirs::download_dir() {
        dialog = dialog.set_directory(dir);
    }
    let (app, team) = (app.clone(), team.to_string());
    dialog.save_file(move |picked| {
        app.state::<Shell>().downloads.lock().unwrap().end_pick(&team);
        let Some(path) = picked.and_then(|p| p.into_path().ok()) else { return };
        let name = path.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or(name);
        let (id, cancel) = app.state::<Shell>().downloads.lock().unwrap().start(&team, &name, path.clone(), None, Instant::now());
        shell::changed(&app);
        tauri::async_runtime::spawn(async move {
            let outcome = fetch(&app, &team, &url, &path, &cancel, |received, total| {
                if app.state::<Shell>().downloads.lock().unwrap().progress(&team, id, received, total, Instant::now()) {
                    shell::emit_changed(&app);
                }
            })
            .await;
            let ended_as = {
                let sh = app.state::<Shell>();
                let mut g = sh.downloads.lock().unwrap();
                match outcome {
                    Ok(()) => g.finish(&team, id, Instant::now()),
                    Err(Cut::Cancelled) => None,
                    Err(Cut::Failed(e)) => g.fail(&team, id, e, Instant::now()),
                }
            };
            shell::changed(&app);
            if let Some(d) = ended_as {
                ended(&app, &team, &d);
            }
        });
    });
}

/// Why a fetch stopped short.
enum Cut {
    Cancelled,
    Failed(String),
}

/// Fetches `url` as the team window's person and writes it to `path`
/// through its partial file, reporting bytes as they arrive.
async fn fetch(app: &AppHandle, team: &str, url: &Url, path: &Path, cancel: &AtomicBool, mut report: impl FnMut(u64, Option<u64>)) -> Result<(), Cut> {
    let (t, origin) = {
        let sh = app.state::<Shell>();
        let t = sh.team(team).ok_or_else(|| Cut::Failed("The team is gone.".into()))?;
        let origin = t.origin().and_then(|o| Url::parse(&o).ok()).ok_or_else(|| Cut::Failed("The team isn't running.".into()))?;
        (t, origin)
    };
    if url.origin() != origin.origin() {
        return Err(Cut::Failed("The file isn't on the team's address.".into()));
    }
    let (a, i) = (app.clone(), team.to_string());
    let jar = tauri::async_runtime::spawn_blocking(move || teamapi::jar(&a, &i))
        .await
        .ok()
        .flatten()
        .ok_or_else(|| Cut::Failed("The team window isn't signed in.".into()))?;
    let cookie = teamapi::cookie_header(&jar, &origin, teamapi::cookie_for(&t, &origin).as_deref())
        .ok_or_else(|| Cut::Failed("The team window isn't signed in.".into()))?;
    let client = crate::download_client(READ_TIMEOUT).map_err(Cut::Failed)?;
    let mut resp = client
        .get(url.clone())
        .header("accept", "*/*")
        .header("cookie", cookie)
        .send()
        .await
        .map_err(|e| Cut::Failed(if e.is_timeout() { "The team didn't answer.".into() } else { "The team can't be reached.".into() }))?;
    if resp.status().as_u16() != 200 {
        return Err(Cut::Failed(status_error(resp.status().as_u16())));
    }
    let total = resp.content_length();
    let partial = partial_path(path);
    let write_error = |e: std::io::Error| Cut::Failed(format!("Kivali can't write in {}: {e}.", folder_name(path)));
    let mut file = std::fs::File::create(&partial).map_err(write_error)?;
    let mut received = 0u64;
    let outcome = loop {
        if cancel.load(Ordering::SeqCst) {
            break Err(Cut::Cancelled);
        }
        match resp.chunk().await {
            Ok(Some(chunk)) => {
                if let Err(e) = file.write_all(&chunk) {
                    break Err(write_error(e));
                }
                received += chunk.len() as u64;
                report(received, total);
            }
            Ok(None) => break Ok(()),
            Err(_) => break Err(Cut::Failed("The connection was lost.".into())),
        }
    };
    let outcome = outcome.and_then(|()| match total {
        Some(t) if t != received => Err(Cut::Failed("The download stopped partway.".into())),
        _ => file.sync_all().map_err(write_error),
    });
    drop(file);
    match outcome {
        Ok(()) => std::fs::rename(&partial, path).map_err(write_error),
        Err(e) => {
            let _ = std::fs::remove_file(&partial);
            Err(e)
        }
    }
}

/// A download ended: Kivali says so when it is not the app in front, and
/// a saved one leaves the card after [`KEEP_DONE`].
fn ended(app: &AppHandle, team: &str, d: &Download) {
    let target = platform::NotifyTarget::Team(team.to_string());
    match d.state {
        State::Done => {
            shell::notify_background(app, &d.name, &format!("Saved in {}.", folder_name(&d.path)), target);
            let app = app.clone();
            std::thread::spawn(move || {
                std::thread::sleep(KEEP_DONE);
                if app.state::<Shell>().downloads.lock().unwrap().expire(Instant::now()) {
                    shell::changed(&app);
                }
            });
        }
        State::Failed => {
            shell::notify_background(app, &format!("Couldn't download {}", d.name), d.error.as_deref().unwrap_or(""), target);
        }
        State::Downloading => {}
    }
}

/// Cancel on the card.
pub fn cancel(app: &AppHandle, team: &str, id: u64) {
    if app.state::<Shell>().downloads.lock().unwrap().cancel(team, id) {
        shell::changed(app);
    }
}

/// The × on a finished download.
pub fn dismiss(app: &AppHandle, team: &str, id: u64) {
    app.state::<Shell>().downloads.lock().unwrap().dismiss(team, id);
    shell::changed(app);
}

/// Show in folder: the saved file, selected in the file manager.
pub fn reveal(app: &AppHandle, team: &str, id: u64) -> Result<(), String> {
    use tauri_plugin_opener::OpenerExt;
    let path = app.state::<Shell>().downloads.lock().unwrap().saved_path(team, id).ok_or("that download is not saved")?;
    app.opener().reveal_item_in_dir(&path).map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(secs: u64) -> Instant {
        // A fixed base, so the tests never read the clock.
        static BASE: std::sync::OnceLock<Instant> = std::sync::OnceLock::new();
        *BASE.get_or_init(Instant::now) + Duration::from_secs(secs)
    }

    // The card is drawn from the view: a download under way shows its
    // bytes and length, a saved one its folder, a failed one its words.
    // Without the folder the card could only say "Saved", and the person
    // could not tell where the file went.
    #[test]
    fn view_follows_a_download_through() {
        let mut ds = Downloads::default();
        let (id, cancel) = ds.start("t", "plan.pdf", PathBuf::from("/Users/dana/Downloads/plan.pdf"), None, at(0));
        assert!(!cancel.load(Ordering::SeqCst));
        assert_eq!(ds.count("t"), 1);
        assert_eq!(ds.count("other"), 0);
        assert!(ds.progress("t", id, 1024, Some(4096), at(0)), "the first bytes redraw");
        let v = &ds.view("t")[0];
        assert_eq!((v.state, v.received, v.total, v.folder.as_str()), ("downloading", 1024, Some(4096), "Downloads"));
        ds.finish("t", id, at(2));
        let v = &ds.view("t")[0];
        assert_eq!((v.state, v.received, v.error.as_deref()), ("done", 4096, None));
        assert_eq!(ds.saved_path("t", id), Some(PathBuf::from("/Users/dana/Downloads/plan.pdf")));
        assert_eq!(ds.view("t")[0].name, "plan.pdf");
    }

    // Progress arrives many times a second; redrawing the card on each
    // chunk would spend the main thread. Only the first bytes and then
    // every REDRAW_EVERY redraw.
    #[test]
    fn progress_redraws_at_most_four_times_a_second() {
        let mut ds = Downloads::default();
        let (id, _) = ds.start("t", "a", PathBuf::from("/d/a"), None, at(0));
        let base = at(0);
        assert!(ds.progress("t", id, 1, None, base));
        assert!(!ds.progress("t", id, 2, None, base + Duration::from_millis(100)));
        assert!(!ds.progress("t", id, 3, None, base + Duration::from_millis(249)));
        assert!(ds.progress("t", id, 4, None, base + Duration::from_millis(250)));
        assert!(!ds.progress("t", id, 5, None, base + Duration::from_millis(400)));
        assert_eq!(ds.view("t")[0].received, 5, "every chunk counts, redrawn or not");
    }

    // A saved download leaves the card on its own after KEEP_DONE; a
    // failed one stays until dismissed, so an error is not missed.
    #[test]
    fn saved_downloads_expire_and_failed_ones_stay() {
        let mut ds = Downloads::default();
        let (done, _) = ds.start("t", "a", PathBuf::from("/d/a"), None, at(0));
        let (failed, _) = ds.start("t", "b", PathBuf::from("/d/b"), None, at(0));
        let (going, _) = ds.start("t", "c", PathBuf::from("/d/c"), None, at(0));
        ds.finish("t", done, at(1));
        ds.fail("t", failed, "The connection was lost.".into(), at(1));
        assert!(!ds.expire(at(1) + KEEP_DONE - Duration::from_secs(1)));
        assert_eq!(ds.count("t"), 3);
        assert!(ds.expire(at(1) + KEEP_DONE));
        let ids: Vec<u64> = ds.view("t").iter().map(|d| d.id).collect();
        assert_eq!(ids, [failed, going]);
        assert_eq!(ds.view("t")[0].error.as_deref(), Some("The connection was lost."));
        ds.dismiss("t", failed);
        ds.dismiss("t", going);
        assert_eq!(ds.count("t"), 1, "a download under way is not dismissed, only cancelled");
        assert!(!ds.expire(at(100)));
    }

    // Cancel sets the flag the fetch watches and takes the row off the
    // card at once; a finished download cannot be cancelled.
    #[test]
    fn cancel_flags_the_fetch_and_removes_the_row() {
        let mut ds = Downloads::default();
        let (id, cancel) = ds.start("t", "a", PathBuf::from("/d/a"), None, at(0));
        let (done, _) = ds.start("t", "b", PathBuf::from("/d/b"), None, at(0));
        ds.finish("t", done, at(0));
        assert!(ds.cancel("t", id));
        assert!(cancel.load(Ordering::SeqCst));
        assert!(!ds.cancel("t", done));
        assert!(!ds.cancel("t", 999));
        assert_eq!(ds.count("t"), 1);
    }

    // A download the web view saves itself (a blob: URL) is matched to
    // its finish by URL; a failure there shows as one.
    #[test]
    fn webview_downloads_finish_by_url() {
        let mut ds = Downloads::default();
        ds.start("t", "a.csv", PathBuf::from("/d/a.csv"), Some("blob:http://127.0.0.1:8081/1".into()), at(0));
        ds.start("t", "b.csv", PathBuf::from("/d/b.csv"), Some("blob:http://127.0.0.1:8081/2".into()), at(0));
        assert!(ds.finish_webview("t", "blob:http://127.0.0.1:8081/3", true, at(1)).is_none());
        let d = ds.finish_webview("t", "blob:http://127.0.0.1:8081/2", false, at(1)).unwrap();
        assert_eq!((d.name.as_str(), d.state), ("b.csv", State::Failed));
        let d = ds.finish_webview("t", "blob:http://127.0.0.1:8081/1", true, at(1)).unwrap();
        assert_eq!((d.name.as_str(), d.state), ("a.csv", State::Done));
        assert!(ds.finish_webview("t", "blob:http://127.0.0.1:8081/1", true, at(1)).is_none(), "finished once");
    }

    // One save dialog at a time per team: a page that asks for a second
    // download while the sheet is up gets nothing, instead of stacking
    // sheets on the person.
    #[test]
    fn one_save_dialog_at_a_time() {
        let mut ds = Downloads::default();
        assert!(ds.begin_pick("t"));
        assert!(!ds.begin_pick("t"));
        assert!(ds.begin_pick("u"), "another team's window is its own");
        ds.end_pick("t");
        assert!(ds.begin_pick("t"));
    }

    // The list is bounded: a page cannot grow the card without end.
    // Finished downloads go first, then the oldest under way.
    #[test]
    fn the_list_is_bounded() {
        let mut ds = Downloads::default();
        let (first, _) = ds.start("t", "0", PathBuf::from("/d/0"), None, at(0));
        for i in 1..MAX_LISTED {
            ds.start("t", &i.to_string(), PathBuf::from(format!("/d/{i}")), None, at(0));
        }
        let (second, _) = (ds.view("t")[1].id, ());
        ds.finish("t", second, at(0));
        ds.start("t", "more", PathBuf::from("/d/more"), None, at(0));
        let ids: Vec<u64> = ds.view("t").iter().map(|d| d.id).collect();
        assert_eq!(ids.len(), MAX_LISTED);
        assert!(ids.contains(&first) && !ids.contains(&second), "the finished one went first");
        ds.start("t", "even more", PathBuf::from("/d/even"), None, at(0));
        assert!(!ds.view("t").iter().any(|d| d.id == first), "then the oldest");
    }

    // The card's height follows its rows, so the webview shows every
    // row and no empty band below them.
    #[test]
    fn card_size_follows_the_rows() {
        assert_eq!(card_size(1), (360.0, 80.0));
        assert_eq!(card_size(3), (360.0, 208.0));
    }

    #[test]
    fn paths_and_words() {
        assert_eq!(folder_name(Path::new("/Users/dana/Downloads/plan.pdf")), "Downloads");
        assert_eq!(folder_name(Path::new("plan.pdf")), "plan.pdf");
        assert_eq!(partial_path(Path::new("/d/plan.pdf")), PathBuf::from("/d/plan.pdf.kivali-partial"));
        assert!(fetchable(&Url::parse("http://127.0.0.1:8081/attachments/abc").unwrap()));
        assert!(fetchable(&Url::parse("https://team.example.com/api/v1/org/files/x/original").unwrap()));
        assert!(!fetchable(&Url::parse("blob:http://127.0.0.1:8081/1").unwrap()));
        assert!(!fetchable(&Url::parse("data:text/plain,hi").unwrap()));
        assert_eq!(status_error(401), "The team window isn't signed in.");
        assert_eq!(status_error(404), "The team no longer has this file.");
        assert_eq!(status_error(500), "The team answered HTTP 500.");
    }
}
