//! The team API: what the shell learns by asking a team's own server as
//! the person signed in in the team's window. The shell runs no session
//! of its own; it reads the session cookie the team's web view
//! (`web-<id>`) already holds and sends it with one request,
//! `GET /api/v1/desktop/facts` (internal/web/api_desktop.go): the signed-in
//! email, the agents, how many are working, the team's files. That fills
//! the tray's "3 agents working", Settings' overview and "Signed in as …",
//! and the pause and delete dialogs.
//! The same cookie store gives the connect page the account a team refused
//! (`kivali_denied`, docs/developers/auth.md).
//!
//! **Threading.** Reading a webview's cookies waits on the main thread
//! (WKWebView's cookie store answers there; WebView2 deadlocks when asked
//! from a synchronous handler), so [`jar`] is only ever called from a
//! worker thread (`spawn_blocking`, a spawned thread), never from a menu
//! or window event or a synchronous command.
//!
//! **Which cookies.** wry's `cookies_for_url` matches a cookie's domain
//! against the URL's *domain*, which an IP address does not have, so it
//! returns nothing for a team here (`127.0.0.1`). The shell reads the
//! whole store (`Webview::cookies`) and keeps the cookies whose domain is
//! exactly the team's host (the server sets host-only cookies), dropping a
//! `Secure` one for plain http off loopback. For a team here only its
//! own session cookie goes (`kivali_session_<id>`; every team here
//! shares 127.0.0.1); for a team
//! elsewhere every such cookie goes, as its browser would send them.
//! Cookie values are never logged.

use crate::shell::{self, Shell};
use crate::teams::Team;
use crate::view::TeamState;
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager};
use url::Url;

/// The endpoint, relative to the team's origin.
pub const FACTS_PATH: &str = "api/v1/desktop/facts";
const SESSION_COOKIE: &str = "kivali_session";
const DENIED_COOKIE: &str = "kivali_denied";
/// More than any answer of the endpoint.
const MAX_BODY: usize = 64 * 1024;

/// The endpoint's answer.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Facts {
    /// The person signed in in the team's window.
    pub email: String,
    /// Hired agents, the person's own seat not counted.
    pub agents: u64,
    /// Agents mid-turn or waiting on background work.
    pub working: u64,
    /// The team's project files (its Files page).
    pub files: u64,
}

#[derive(Debug, PartialEq, Eq)]
pub enum FetchError {
    /// 401/403: the window's session is gone (signed out, expired).
    SignedOut,
    Failed(String),
}

/// A cookie of the webview's store, as much as matters here.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct JarCookie {
    pub name: String,
    pub value: String,
    pub domain: Option<String>,
    pub secure: bool,
}

/// The one cookie sent to a team here, named with the suffix its server
/// runs with (shell.rs `up_request`, `KIVALI_COOKIE_SUFFIX`: its id);
/// None for a team elsewhere (all of its origin's cookies go).
pub fn session_cookie_name(team: &Team) -> Option<String> {
    team.is_here().then(|| format!("{SESSION_COOKIE}_{}", team.id))
}

/// The cookie to send to `team` at `origin`. Cookies ignore the port, so
/// on loopback every team here's cookies share the store: a team elsewhere
/// reached on loopback (a port-forward) gets only the plain session
/// cookie, never another team's.
pub fn cookie_for(team: &Team, origin: &Url) -> Option<String> {
    session_cookie_name(team).or_else(|| is_loopback(origin).then(|| SESSION_COOKIE.to_string()))
}

fn is_loopback(origin: &Url) -> bool {
    let Some(host) = origin.host_str() else { return false };
    let host = host.trim_start_matches('[').trim_end_matches(']').to_ascii_lowercase();
    host == "localhost" || host.parse::<std::net::IpAddr>().is_ok_and(|ip| ip.is_loopback())
}

/// Whether a browser would send this cookie to `origin`: its domain is
/// the origin's host, and a `Secure` cookie only over https or loopback.
pub fn applies_to(c: &JarCookie, origin: &Url) -> bool {
    let Some(host) = origin.host_str() else { return false };
    let host = host.trim_start_matches('[').trim_end_matches(']').to_ascii_lowercase();
    if c.secure && origin.scheme() != "https" && !is_loopback(origin) {
        return false;
    }
    c.domain.as_deref().map(|d| d.trim_start_matches('.').to_ascii_lowercase()).is_some_and(|d| d == host)
}

/// A name or value that cannot break a `Cookie` header.
fn header_safe(s: &str) -> bool {
    !s.is_empty() && !s.chars().any(|c| c.is_control() || c == ';')
}

/// The `Cookie` header for `origin`: every cookie that applies, or only
/// the one named `only`. None when nothing applies (not signed in there).
pub fn cookie_header(jar: &[JarCookie], origin: &Url, only: Option<&str>) -> Option<String> {
    let parts: Vec<String> = jar
        .iter()
        .filter(|c| applies_to(c, origin) && only.is_none_or(|n| c.name == n))
        .filter(|c| header_safe(&c.name) && header_safe(&c.value) && !c.name.contains('='))
        .map(|c| format!("{}={}", c.name, c.value))
        .collect();
    (!parts.is_empty()).then(|| parts.join("; "))
}

/// The account a team refused, from its `kivali_denied[_<suffix>]`
/// cookie: something shaped like an email, or None. On loopback only the
/// plain name counts: a suffixed one belongs to a team here.
pub fn denied_email(jar: &[JarCookie], origin: &Url) -> Option<String> {
    let suffixed_ok = !is_loopback(origin);
    jar.iter()
        .filter(|c| applies_to(c, origin))
        .filter(|c| c.name == DENIED_COOKIE || (suffixed_ok && c.name.strip_prefix(DENIED_COOKIE).is_some_and(|r| r.starts_with('_'))))
        .map(|c| c.value.trim_matches('"').to_string())
        .find(|v| v.len() <= 320 && v.contains('@') && !v.chars().any(|c| c.is_whitespace() || c.is_control()))
}

/// Reads the endpoint's answer.
pub fn parse(status: u16, body: &[u8]) -> Result<Facts, FetchError> {
    match status {
        200 => serde_json::from_slice(body).map_err(|e| FetchError::Failed(format!("unreadable answer: {e}"))),
        401 | 403 => Err(FetchError::SignedOut),
        s => Err(FetchError::Failed(format!("HTTP {s}"))),
    }
}

/// `GET <origin>/api/v1/desktop/facts` with `cookie`. Redirects are not
/// followed; 10 seconds; the cookie is never logged.
pub async fn fetch(origin: &Url, cookie: &str) -> Result<Facts, FetchError> {
    let endpoint = origin.join(FACTS_PATH).map_err(|e| FetchError::Failed(e.to_string()))?;
    let client = crate::http_client(reqwest::redirect::Policy::none()).map_err(FetchError::Failed)?;
    let mut resp = client
        .get(endpoint)
        .header("accept", "application/json")
        .header("cookie", cookie)
        .send()
        .await
        .map_err(|e| FetchError::Failed(if e.is_timeout() { "no answer within 10 seconds".into() } else { "unreachable".into() }))?;
    let status = resp.status().as_u16();
    let mut body = Vec::new();
    while let Some(chunk) = resp.chunk().await.map_err(|_| FetchError::Failed("reading the answer failed".into()))? {
        if body.len() + chunk.len() > MAX_BODY {
            return Err(FetchError::Failed("answer too large".into()));
        }
        body.extend_from_slice(&chunk);
    }
    parse(status, &body)
}

/// The cookies of a team window's web view; None when it has no window.
/// Blocks on the main thread: call it from a worker thread only.
pub fn jar(app: &AppHandle, id: &str) -> Option<Vec<JarCookie>> {
    let w = app.get_webview(&crate::windows::web_label(id))?;
    match w.cookies() {
        Ok(cs) => Some(
            cs.into_iter()
                .map(|c| JarCookie {
                    name: c.name().to_string(),
                    value: c.value().to_string(),
                    domain: c.domain().map(str::to_string),
                    secure: c.secure().unwrap_or(false),
                })
                .collect(),
        ),
        Err(e) => {
            eprintln!("kivali: {id}: cannot read the team window's cookies: {e}");
            None
        }
    }
}

/// A listed team that shows its web app and runs: a team here running, a
/// team elsewhere not known to be unreachable.
fn askable(sh: &Shell, id: &str) -> Option<(Team, Url)> {
    let team = sh.teams.lock().unwrap().get(id).cloned()?;
    if sh.team_state(&team).0 != TeamState::Running {
        return None;
    }
    let origin = Url::parse(&team.origin()?).ok()?;
    Some((team, origin))
}

/// Stores a team's facts (None: signed out there), redrawing on a change.
fn store(app: &AppHandle, id: &str, facts: Option<Facts>) {
    let sh = app.state::<Shell>();
    let changed = match sh.runtime(id) {
        Some(rt) => {
            let mut g = rt.facts.lock().unwrap();
            let c = *g != facts;
            *g = facts;
            c
        }
        None => {
            let mut reach = sh.reach.lock().unwrap();
            let r = reach.entry(id.to_string()).or_default();
            let c = r.facts != facts;
            r.facts = facts;
            c
        }
    };
    if changed {
        shell::changed(app);
    }
}

/// The team's facts read now, as its window's person, and stored; None
/// when the team isn't running, has no window, or didn't answer (the
/// last facts stay), or the window isn't signed in (they are cleared).
pub async fn load(app: &AppHandle, id: &str) -> Option<Facts> {
    let (team, origin) = askable(&app.state::<Shell>(), id)?;
    app.get_webview(&crate::windows::web_label(id))?;
    let (a, i) = (app.clone(), id.to_string());
    let jar = tauri::async_runtime::spawn_blocking(move || jar(&a, &i)).await.ok()??;
    let Some(header) = cookie_header(&jar, &origin, cookie_for(&team, &origin).as_deref()) else {
        store(app, id, None);
        return None;
    };
    match fetch(&origin, &header).await {
        Ok(f) => {
            store(app, id, Some(f.clone()));
            Some(f)
        }
        Err(FetchError::SignedOut) => {
            store(app, id, None);
            None
        }
        Err(FetchError::Failed(e)) => {
            eprintln!("kivali: {id}: the team's facts: {e}");
            None
        }
    }
}

/// [`load`] in the background: the watch loop's slow tick, a team window
/// coming forward or finishing a page, a team coming up.
pub fn refresh(app: &AppHandle, id: &str) {
    let app = app.clone();
    let id = id.to_string();
    tauri::async_runtime::spawn(async move {
        let _ = load(&app, &id).await;
    });
}

/// The connecting window landed on the not-invited page. Off the main
/// thread, reads the refused account from its cookies into the connect
/// state, then closes the window (whose webview holds them).
pub fn note_denied(app: &AppHandle, id: &str, origin: Option<String>) {
    let app = app.clone();
    let id = id.to_string();
    std::thread::spawn(move || {
        let origin = origin.and_then(|o| Url::parse(&o).ok());
        let email = origin.and_then(|o| jar(&app, &id).and_then(|j| denied_email(&j, &o)));
        {
            let sh = app.state::<Shell>();
            let mut c = sh.connect.lock().unwrap();
            if c.state == "not_invited" {
                c.email = email;
            }
        }
        let a = app.clone();
        let _ = app.run_on_main_thread(move || crate::windows::close_team_window(&a, &id));
        shell::changed(&app);
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::teams::{Kind, TeamsFile};

    fn ck(name: &str, value: &str, domain: &str, secure: bool) -> JarCookie {
        JarCookie { name: name.into(), value: value.into(), domain: Some(domain.into()), secure }
    }

    fn u(s: &str) -> Url {
        Url::parse(s).unwrap()
    }

    #[test]
    fn session_cookie_follows_the_suffix_rule() {
        let mut f = TeamsFile::default();
        let a = f.add_here("Plainsong", Kind::Work, "dana@example.com", &mut || 0x3f2a);
        let b = f.add_elsewhere(&crate::orgurl::normalize_org_url("studio.example.com").unwrap(), "Studio");
        assert_eq!(session_cookie_name(f.get(&a).unwrap()).as_deref(), Some("kivali_session_plainson-3f2a"));
        assert_eq!(session_cookie_name(f.get(&b).unwrap()), None);
        // A team elsewhere: everything for its host, but on loopback only the plain session.
        let studio = f.get(&b).unwrap();
        assert_eq!(cookie_for(studio, &u("https://studio.example.com")), None);
        assert_eq!(cookie_for(studio, &u("http://127.0.0.1:9000")).as_deref(), Some("kivali_session"));
        assert_eq!(cookie_for(f.get(&a).unwrap(), &u("http://127.0.0.1:9000")).as_deref(), Some("kivali_session_plainson-3f2a"));
        let jar = vec![ck("kivali_session_plainson-3f2a", "F", "127.0.0.1", false), ck("kivali_session", "T", "127.0.0.1", false)];
        assert_eq!(cookie_header(&jar, &u("http://127.0.0.1:9000"), cookie_for(studio, &u("http://127.0.0.1:9000")).as_deref()).as_deref(), Some("kivali_session=T"));
    }

    #[test]
    fn only_this_teams_cookie_goes_to_a_team_here() {
        // Every team here lives on 127.0.0.1: the store holds all of theirs.
        let jar = vec![
            ck("kivali_session_home-0001", "H", "127.0.0.1", false),
            ck("kivali_session_plainson-3f2a", "F", "127.0.0.1", false),
            ck("kivali_session", "S", "studio.example.com", true),
        ];
        let o = u("http://127.0.0.1:8080");
        assert_eq!(cookie_header(&jar, &o, Some("kivali_session_plainson-3f2a")).as_deref(), Some("kivali_session_plainson-3f2a=F"));
        assert_eq!(cookie_header(&jar, &o, Some("kivali_session_other")), None);
    }

    #[test]
    fn a_team_elsewhere_gets_its_own_origins_cookies() {
        let jar = vec![
            ck("kivali_session", "S", "studio.example.com", true),
            ck("kivali_oauth_state", "X", "studio.example.com", true),
            ck("kivali_session", "E", "evil.example.com", false),
            ck("kivali_session", "P", "example.com", false),
            ck("bad", "a;b", "studio.example.com", false),
        ];
        assert_eq!(cookie_header(&jar, &u("https://studio.example.com"), None).as_deref(), Some("kivali_session=S; kivali_oauth_state=X"));
        // A Secure cookie never goes over plain http off loopback.
        assert_eq!(cookie_header(&jar, &u("http://studio.example.com"), None), None);
        // On loopback it does (browsers treat it as secure).
        let local = vec![ck("kivali_session", "L", "127.0.0.1", true)];
        assert_eq!(cookie_header(&local, &u("http://127.0.0.1:8081"), None).as_deref(), Some("kivali_session=L"));
        // A cookie whose domain is unknown goes nowhere.
        let unknown = vec![JarCookie { name: "a".into(), value: "b".into(), domain: None, secure: false }];
        assert_eq!(cookie_header(&unknown, &u("https://studio.example.com"), None), None);
    }

    #[test]
    fn denied_account() {
        let o = u("https://studio.example.com");
        let jar = vec![ck("kivali_denied", "sam.work@example.com", "studio.example.com", true)];
        assert_eq!(denied_email(&jar, &o).as_deref(), Some("sam.work@example.com"));
        let suffixed = vec![ck("kivali_denied_home-0001", "\"sam@example.com\"", "studio.example.com", true)];
        assert_eq!(denied_email(&suffixed, &o).as_deref(), Some("sam@example.com"));
        // On loopback a suffixed one is a team here's, never the connecting team's.
        let local = vec![ck("kivali_denied_home-0001", "sam@example.com", "127.0.0.1", false)];
        assert_eq!(denied_email(&local, &u("http://127.0.0.1:8081")), None);
        let plain = vec![ck("kivali_denied", "\"sam@example.com\"", "127.0.0.1", false)];
        assert_eq!(denied_email(&plain, &u("http://127.0.0.1:8081")).as_deref(), Some("sam@example.com"));
        let other = vec![ck("kivali_denied", "sam@example.com", "other.example.com", true), ck("kivali_deniedx", "a@b", "studio.example.com", true)];
        assert_eq!(denied_email(&other, &o), None);
        assert_eq!(denied_email(&[ck("kivali_denied", "not an email", "studio.example.com", true)], &o), None);
    }

    #[test]
    fn answers() {
        let body = br#"{"email":"dana@example.com","agents":6,"working":3,"files":1204}"#;
        assert_eq!(parse(200, body), Ok(Facts { email: "dana@example.com".into(), agents: 6, working: 3, files: 1204 }));
        assert_eq!(parse(401, b"{}"), Err(FetchError::SignedOut));
        assert_eq!(parse(403, b""), Err(FetchError::SignedOut));
        assert!(matches!(parse(404, b""), Err(FetchError::Failed(_))));
        assert!(matches!(parse(200, b"<html>"), Err(FetchError::Failed(_))));
    }
}
