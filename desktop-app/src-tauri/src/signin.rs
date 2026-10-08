//! The desktop sign-in (docs/developers/desktop-app.md, "Signing in to a team"; the server's
//! half is docs/developers/auth.md, "Desktop sign-in").
//!
//! Google refuses its consent page in an embedded webview, so only the
//! provider's leg runs in the system browser; the org window starts and
//! finishes the sign-in, holding its state cookie throughout. The
//! browser hands Google's answer back through a loopback redirect
//! (RFC 8252, section 7.3):
//!
//! 1. The org window's navigation to `<origin>/auth/login` is refused
//!    (windows.rs, `OrgNav::DesktopLogin`). The shell listens on
//!    `127.0.0.1:<port>` and loads the same URL with `client=desktop`,
//!    `return_port=<port>` and `return_token=<token>`. The server's
//!    redirect to Google then leaves the origin and goes to the system
//!    browser like any other page.
//! 2. The server's callback sends the browser to
//!    `http://127.0.0.1:<port>/signin/<token>?code=…&state=…` (or
//!    `?error=…&state=…`).
//! 3. The listener answers that one request with a page saying to return
//!    to the app, closes, and the shell loads
//!    `<origin>/auth/callback?<the same query>` in the org window, where
//!    the server finishes the sign-in with the webview's cookie.
//!
//! The listener lives at most [`SIGNIN_TTL`], and closes when a newer
//! sign-in starts. Neither the token, the code nor the state is ever
//! logged.

use base64::Engine as _;
use std::io::{self, Read, Write};
use std::net::{Ipv4Addr, Shutdown, SocketAddr, TcpListener, TcpStream};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};
use url::{Origin, Url};

/// The path the org window loads only with `client=desktop`.
pub const LOGIN_PATH: &str = "/auth/login";
/// Where the org window finishes the sign-in.
pub const CALLBACK_PATH: &str = "/auth/callback";
/// The listener's one path, followed by the token.
pub const RETURN_PREFIX: &str = "/signin/";
/// How long a sign-in started in the org window may take.
pub const SIGNIN_TTL: Duration = Duration::from_secs(10 * 60);
/// The most the listener reads of one request.
pub const MAX_REQUEST: usize = 8 * 1024;
/// How long the listener waits on a connection that sends nothing.
pub const READ_TIMEOUT: Duration = Duration::from_secs(5);
/// How much of the provider's `error` the page repeats.
const MAX_ERROR_CHARS: usize = 64;

/// The time, injected so tests can move it.
pub trait Clock: Send + Sync {
    fn now(&self) -> Instant;
}

pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }
}

/// What the listener hands back on a matching request.
pub struct Return {
    /// The sign-in's token, so the caller can tell it is still the one
    /// pending.
    pub token: String,
    /// `<origin>/auth/callback?<the browser's query>`.
    pub callback: Url,
}

/// A sign-in the org window started and the browser has not handed
/// back. Dropping it closes its listener. No `Debug`: it holds the token.
pub struct PendingSignin {
    #[allow(dead_code)]
    pub origin: Origin,
    pub port: u16,
    pub token: String,
    /// When it started; the listener checks [`SIGNIN_TTL`] against its
    /// own copy.
    #[allow(dead_code)]
    pub started: Instant,
    listener: Closer,
}

impl Drop for PendingSignin {
    fn drop(&mut self) {
        self.listener.close();
    }
}

/// Whether a login URL already asks for the desktop flow. Reads the
/// first `client`, as the server does (`r.URL.Query().Get("client")`).
pub fn is_desktop_login(url: &Url) -> bool {
    url.query_pairs().find(|(k, _)| k == "client").is_some_and(|(_, v)| v == "desktop")
}

/// The login URL the org window loads instead: the page's own, query
/// kept, with `client=desktop`, `return_port` and `return_token`
/// appended.
pub fn desktop_login_url(login: &Url, port: u16, token: &str) -> Url {
    let mut u = login.clone();
    u.query_pairs_mut()
        .append_pair("client", "desktop")
        .append_pair("return_port", &port.to_string())
        .append_pair("return_token", token);
    u
}

/// 32 random bytes, base64url without padding: 43 characters.
pub fn new_token() -> io::Result<String> {
    let mut b = [0u8; 32];
    getrandom::fill(&mut b).map_err(|e| io::Error::other(e.to_string()))?;
    Ok(base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(b))
}

/// Starts a sign-in for `origin`: binds `127.0.0.1:0`, makes a token and
/// serves the return on a thread of its own. `on_return` runs on that
/// thread, once, after the browser has had its page and the listener is
/// closed; it never runs when the listener closes first (expiry, a newer
/// sign-in, the [`PendingSignin`] dropped).
pub fn start<F>(origin: Origin, clock: Arc<dyn Clock>, on_return: F) -> io::Result<PendingSignin>
where
    F: FnOnce(Return) + Send + 'static,
{
    let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))?;
    let port = listener.local_addr()?.port();
    let token = new_token()?;
    let started = clock.now();
    let closer = Closer::new(port);
    let job = Job { listener, token: token.clone(), origin: origin.clone(), started, clock, closer: closer.clone() };
    std::thread::Builder::new().name("kivali-signin".into()).spawn(move || job.serve(on_return))?;
    let timer = closer.clone();
    std::thread::Builder::new().name("kivali-signin-ttl".into()).spawn(move || {
        if !timer.wait(SIGNIN_TTL) {
            eprintln!("kivali: sign-in expired after 10 minutes; its listener is closed");
            timer.close();
        }
    })?;
    Ok(PendingSignin { origin, port, token, started, listener: closer })
}

/// Shared between a pending sign-in, its listener thread and its timer.
#[derive(Clone)]
pub(crate) struct Closer(Arc<(Mutex<bool>, Condvar, u16)>);

impl Closer {
    pub(crate) fn new(port: u16) -> Closer {
        Closer(Arc::new((Mutex::new(false), Condvar::new(), port)))
    }

    pub(crate) fn is_closed(&self) -> bool {
        *self.0 .0.lock().unwrap()
    }

    /// Marks it closed; true when it was open.
    pub(crate) fn mark(&self) -> bool {
        let was_open = !std::mem::replace(&mut *self.0 .0.lock().unwrap(), true);
        self.0 .1.notify_all();
        was_open
    }

    /// Marks it closed at once and wakes the listener out of `accept`
    /// with a connection of its own, made on another thread so a caller
    /// (perhaps holding a lock, perhaps the main thread) never waits.
    pub(crate) fn close(&self) {
        if self.mark() {
            let addr = SocketAddr::from((Ipv4Addr::LOCALHOST, self.0 .2));
            std::thread::spawn(move || {
                let _ = TcpStream::connect_timeout(&addr, READ_TIMEOUT);
            });
        }
    }

    /// Waits up to `d` for it to close; true when it did.
    pub(crate) fn wait(&self, d: Duration) -> bool {
        let (m, cv, _) = &*self.0;
        let (closed, _) = cv.wait_timeout_while(m.lock().unwrap(), d, |closed| !*closed).unwrap();
        *closed
    }
}

struct Job {
    listener: TcpListener,
    token: String,
    origin: Origin,
    started: Instant,
    clock: Arc<dyn Clock>,
    closer: Closer,
}

impl Job {
    fn serve<F: FnOnce(Return)>(self, on_return: F) {
        let Job { listener, token, origin, started, clock, closer } = self;
        let mut done = None;
        for conn in listener.incoming() {
            if closer.is_closed() {
                break;
            }
            let Ok(mut stream) = conn else { continue };
            let _ = stream.set_write_timeout(Some(READ_TIMEOUT));
            let deadline = Instant::now() + READ_TIMEOUT;
            let answer = match read_head(&mut stream, deadline, &Instant::now) {
                Ok(head) => answer(&head, &token),
                Err(_) => Answer::NotFound,
            };
            // A newer sign-in (or expiry) closed this one while the
            // request was being read.
            if closer.is_closed() {
                break;
            }
            match answer {
                Answer::NotFound => {
                    eprintln!("kivali: sign-in listener: refused a request");
                    respond(&mut stream, "404 Not Found", "text/plain; charset=utf-8", "Not found\n");
                }
                Answer::Return { .. } if clock.now().saturating_duration_since(started) > SIGNIN_TTL => {
                    eprintln!("kivali: sign-in listener: refused a return after 10 minutes");
                    respond(&mut stream, "410 Gone", "text/html; charset=utf-8", &page(
                        "This sign-in took longer than 10 minutes. Return to Kivali and try again.",
                    ));
                    closer.mark();
                    break;
                }
                Answer::Return { query } => {
                    let pairs: Vec<(String, String)> = url::form_urlencoded::parse(query.as_bytes()).into_owned().collect();
                    respond(&mut stream, "200 OK", "text/html; charset=utf-8", &page(&return_message(&pairs)));
                    closer.mark();
                    done = Some(Return { token: token.clone(), callback: callback_url(&origin, &pairs) });
                    break;
                }
            }
        }
        // The listener closes before the caller hears of the return.
        drop(listener);
        if let Some(r) = done {
            on_return(r);
        }
    }
}

/// What the listener makes of one request head.
#[derive(Debug, PartialEq, Eq)]
enum Answer {
    NotFound,
    /// `GET /signin/<token>?<query>`; `query` is raw, possibly empty.
    Return { query: String },
}

/// A reader whose reads can be bounded in time.
pub(crate) trait TimedRead: Read {
    fn set_timeout(&mut self, d: Duration) -> io::Result<()>;
}

impl TimedRead for TcpStream {
    fn set_timeout(&mut self, d: Duration) -> io::Result<()> {
        self.set_read_timeout(Some(d))
    }
}

/// Reads up to the blank line ending the head, at most [`MAX_REQUEST`]
/// bytes, all of it by `deadline`: each read may wait only for what is
/// left, so a client trickling bytes cannot hold the listener past it.
/// A longer head, an early end or a missed deadline is an error.
pub(crate) fn read_head(r: &mut impl TimedRead, deadline: Instant, now: &dyn Fn() -> Instant) -> io::Result<Vec<u8>> {
    let mut buf = vec![0u8; MAX_REQUEST];
    let mut n = 0;
    while n < buf.len() {
        let left = deadline.saturating_duration_since(now());
        if left.is_zero() {
            return Err(io::Error::new(io::ErrorKind::TimedOut, "request head took too long"));
        }
        r.set_timeout(left)?;
        let got = r.read(&mut buf[n..])?;
        if got == 0 {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "request ended early"));
        }
        let from = n.saturating_sub(3);
        n += got;
        if buf[from..n].windows(4).any(|w| w == b"\r\n\r\n") {
            buf.truncate(n);
            return Ok(buf);
        }
    }
    Err(io::Error::new(io::ErrorKind::InvalidData, "request head over 8 KiB"))
}

fn answer(head: &[u8], token: &str) -> Answer {
    let Some(line) = head.split(|&b| b == b'\n').next() else { return Answer::NotFound };
    let Ok(line) = std::str::from_utf8(line) else { return Answer::NotFound };
    let mut parts = line.trim_end_matches('\r').split(' ');
    let (Some("GET"), Some(target), Some(version), None) = (parts.next(), parts.next(), parts.next(), parts.next()) else {
        return Answer::NotFound;
    };
    if !version.starts_with("HTTP/1.") {
        return Answer::NotFound;
    }
    let (path, query) = target.split_once('?').unwrap_or((target, ""));
    let Some(path) = percent_decode(path) else { return Answer::NotFound };
    let Some(got) = path.strip_prefix(RETURN_PREFIX.as_bytes()) else { return Answer::NotFound };
    if !constant_time_eq(got, token.as_bytes()) {
        return Answer::NotFound;
    }
    Answer::Return { query: query.to_string() }
}

pub(crate) fn percent_decode(s: &str) -> Option<Vec<u8>> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' {
            let hex = b.get(i + 1..i + 3)?;
            out.push(u8::from_str_radix(std::str::from_utf8(hex).ok()?, 16).ok()?);
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    Some(out)
}

/// Equal length and contents, in time that depends only on the length.
pub(crate) fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub(crate) fn param<'a>(pairs: &'a [(String, String)], name: &str) -> Option<&'a str> {
    pairs.iter().find(|(k, _)| k == name).map(|(_, v)| v.as_str()).filter(|v| !v.is_empty())
}

/// The sentence the browser's page shows.
fn return_message(pairs: &[(String, String)]) -> String {
    if param(pairs, "code").is_some() {
        return "Signed in to Kivali. You can close this tab and return to the app.".into();
    }
    let error: String = param(pairs, "error").unwrap_or("no code").chars().take(MAX_ERROR_CHARS).collect();
    format!("Sign-in was not completed ({}). Return to Kivali and try again.", html_escape(&error))
}

pub(crate) fn html_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
    out
}

/// A self-contained page around one sentence (already HTML).
pub(crate) fn page(sentence: &str) -> String {
    format!(
        "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">\
<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Kivali</title>\
<style>body{{margin:0;min-height:100vh;display:grid;place-items:center;\
font:16px/1.5 -apple-system,system-ui,sans-serif;background:#fff;color:#1a1a1a}}\
@media (prefers-color-scheme:dark){{body{{background:#1a1a1a;color:#eee}}}}\
p{{max-width:32em;padding:0 16px;text-align:center}}</style></head>\
<body><p>{sentence}</p></body></html>\n"
    )
}

pub(crate) fn respond(stream: &mut TcpStream, status: &str, content_type: &str, body: &str) {
    let head = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\n\
Cache-Control: no-store\r\nReferrer-Policy: no-referrer\r\n\
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'\r\nConnection: close\r\n\r\n",
        body.len()
    );
    let _ = stream.write_all(head.as_bytes());
    let _ = stream.write_all(body.as_bytes());
    let _ = stream.flush();
    let _ = stream.shutdown(Shutdown::Write);
}

/// `<origin>/auth/callback?<pairs>`, re-encoded.
fn callback_url(origin: &Origin, pairs: &[(String, String)]) -> Url {
    let mut u = Url::parse(&origin.ascii_serialization()).expect("a tuple origin is a URL");
    u.set_path(CALLBACK_PATH);
    if !pairs.is_empty() {
        u.query_pairs_mut().extend_pairs(pairs);
    }
    u
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::mpsc;

    fn u(s: &str) -> Url {
        Url::parse(s).unwrap()
    }

    struct MockClock(Mutex<Instant>);

    impl MockClock {
        fn new() -> Arc<MockClock> {
            Arc::new(MockClock(Mutex::new(Instant::now())))
        }
        fn advance(&self, d: Duration) {
            *self.0.lock().unwrap() += d;
        }
    }

    impl Clock for MockClock {
        fn now(&self) -> Instant {
            *self.0.lock().unwrap()
        }
    }

    /// One request over a real socket; the whole answer, read to EOF.
    fn send(port: u16, request: &str) -> String {
        let mut s = TcpStream::connect((Ipv4Addr::LOCALHOST, port)).unwrap();
        s.write_all(request.as_bytes()).unwrap();
        let mut out = String::new();
        s.read_to_string(&mut out).unwrap();
        out
    }

    fn get(port: u16, target: &str) -> String {
        send(port, &format!("GET {target} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\n\r\n"))
    }

    fn start_test(origin: &str, clock: Arc<MockClock>) -> (PendingSignin, mpsc::Receiver<Return>) {
        let (tx, rx) = mpsc::channel();
        let p = start(u(origin).origin(), clock, move |r| tx.send(r).unwrap()).unwrap();
        (p, rx)
    }

    const WAIT: Duration = Duration::from_secs(10);

    #[test]
    fn desktop_login_url_keeps_the_query_and_appends_three_parameters() {
        let tok = "abcDEF0123456789-_abcDEF0123456789-_abcDEF0";
        let got = desktop_login_url(&u("http://127.0.0.1:8080/auth/login?next=%2Fagents%3Fx%3D1"), 54321, tok);
        assert_eq!(
            got.as_str(),
            format!("http://127.0.0.1:8080/auth/login?next=%2Fagents%3Fx%3D1&client=desktop&return_port=54321&return_token={tok}")
        );
        assert!(is_desktop_login(&got));
        let bare = desktop_login_url(&u("https://org.example.com/auth/login"), 1, "t");
        assert_eq!(bare.as_str(), "https://org.example.com/auth/login?client=desktop&return_port=1&return_token=t");
        assert!(!is_desktop_login(&u("https://org.example.com/auth/login?next=%2F")));
        assert!(!is_desktop_login(&u("https://org.example.com/auth/login?client=web")));
    }

    #[test]
    fn tokens_are_43_base64url_characters_and_differ() {
        let a = new_token().unwrap();
        let b = new_token().unwrap();
        assert_eq!(a.len(), 43);
        assert!(a.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_'), "{a}");
        assert_ne!(a, b);
    }

    #[test]
    fn pending_signin_records_origin_port_token_and_start() {
        let clock = MockClock::new();
        let (p, _rx) = start_test("http://127.0.0.1:8080/", clock.clone());
        assert_eq!(p.origin, u("http://127.0.0.1:8080").origin());
        assert_ne!(p.port, 0);
        assert_eq!(p.token.len(), 43);
        assert_eq!(p.started, clock.now());
    }

    #[test]
    fn a_matching_return_answers_the_page_and_hands_back_the_callback() {
        let clock = MockClock::new();
        let (p, rx) = start_test("https://org.example.com:8443/", clock.clone());
        clock.advance(SIGNIN_TTL);
        let got = get(p.port, &format!("/signin/{}?code=4%2F0Ab-x_y&state=desktop~abc.DEF-_", p.token));
        assert!(got.starts_with("HTTP/1.1 200 OK\r\n"), "{got}");
        assert!(got.contains("\r\nContent-Type: text/html; charset=utf-8\r\n"));
        assert!(got.contains("\r\nCache-Control: no-store\r\n"));
        assert!(got.contains("\r\nConnection: close\r\n"));
        assert!(got.contains("Signed in to Kivali. You can close this tab and return to the app."));
        let r = rx.recv_timeout(WAIT).unwrap();
        assert_eq!(r.token, p.token);
        assert_eq!(r.callback.as_str(), "https://org.example.com:8443/auth/callback?code=4%2F0Ab-x_y&state=desktop%7Eabc.DEF-_");
        // Served once: the listener is closed.
        assert!(TcpStream::connect((Ipv4Addr::LOCALHOST, p.port)).is_err());
    }

    #[test]
    fn a_refusal_from_google_shows_the_error_escaped_and_still_returns() {
        let clock = MockClock::new();
        let (p, rx) = start_test("http://127.0.0.1:8080", clock);
        let long = "x".repeat(100);
        let got = get(p.port, &format!("/signin/{}?error=%3Cb%3Eaccess_denied{long}&state=s", p.token));
        assert!(got.starts_with("HTTP/1.1 200 OK\r\n"), "{got}");
        let shown = format!("&lt;b&gt;access_denied{}", "x".repeat(64 - "<b>access_denied".len()));
        assert!(got.contains(&format!("Sign-in was not completed ({shown}). Return to Kivali and try again.")), "{got}");
        assert!(!got.contains("<b>"));
        let r = rx.recv_timeout(WAIT).unwrap();
        let q: Vec<(String, String)> = r.callback.query_pairs().into_owned().collect();
        assert_eq!(q, [("error".into(), format!("<b>access_denied{long}")), ("state".into(), "s".into())]);
    }

    #[test]
    fn anything_else_is_404_and_the_listener_keeps_waiting() {
        let clock = MockClock::new();
        let (p, rx) = start_test("http://127.0.0.1:8080", clock);
        // The last character changed to a different one: the right
        // length, never the token.
        let last = if p.token.ends_with('A') { 'B' } else { 'A' };
        let wrong = format!("{}{last}", &p.token[..42]);
        assert_ne!(wrong, p.token);
        for target in [
            format!("/signin/{wrong}?code=c&state=s"),
            format!("/signin/{}x?code=c&state=s", p.token),
            format!("/signin/{}/?code=c&state=s", p.token),
            "/signin/?code=c&state=s".to_string(),
            format!("/other/{}?code=c&state=s", p.token),
            "/favicon.ico".to_string(),
        ] {
            let got = get(p.port, &target);
            assert!(got.starts_with("HTTP/1.1 404 Not Found\r\n"), "{target}: {got}");
        }
        let got = send(p.port, &format!("POST /signin/{} HTTP/1.1\r\nContent-Length: 0\r\n\r\n", p.token));
        assert!(got.starts_with("HTTP/1.1 404 Not Found\r\n"), "{got}");
        // Still listening: the right request is served.
        let got = get(p.port, &format!("/signin/{}?code=c&state=s", p.token));
        assert!(got.starts_with("HTTP/1.1 200 OK\r\n"), "{got}");
        assert_eq!(rx.recv_timeout(WAIT).unwrap().callback.as_str(), "http://127.0.0.1:8080/auth/callback?code=c&state=s");
    }

    #[test]
    fn the_token_may_arrive_percent_encoded() {
        let clock = MockClock::new();
        let (p, rx) = start_test("http://127.0.0.1:8080", clock);
        let encoded: String = p.token.bytes().map(|b| format!("%{b:02X}")).collect();
        let got = get(p.port, &format!("/signin%2F{encoded}?code=c&state=s"));
        assert!(got.starts_with("HTTP/1.1 200 OK\r\n"), "{got}");
        assert!(rx.recv_timeout(WAIT).is_ok());
    }

    #[test]
    fn a_return_after_ten_minutes_is_refused_and_closes() {
        let clock = MockClock::new();
        let (p, rx) = start_test("http://127.0.0.1:8080", clock.clone());
        clock.advance(SIGNIN_TTL + Duration::from_millis(1));
        let got = get(p.port, &format!("/signin/{}?code=c&state=s", p.token));
        assert!(got.starts_with("HTTP/1.1 410 Gone\r\n"), "{got}");
        // The thread ended without handing anything back.
        assert_eq!(rx.recv_timeout(WAIT).err(), Some(mpsc::RecvTimeoutError::Disconnected));
        assert!(TcpStream::connect((Ipv4Addr::LOCALHOST, p.port)).is_err());
    }

    #[test]
    fn dropping_the_pending_signin_closes_the_listener() {
        let clock = MockClock::new();
        let (p, rx) = start_test("http://127.0.0.1:8080", clock);
        let port = p.port;
        drop(p);
        assert_eq!(rx.recv_timeout(WAIT).err(), Some(mpsc::RecvTimeoutError::Disconnected));
        assert!(TcpStream::connect((Ipv4Addr::LOCALHOST, port)).is_err());
    }

    #[test]
    fn closer_wait_returns_once_closed() {
        let c = Closer::new(0);
        assert!(!c.wait(Duration::ZERO));
        assert!(c.mark());
        assert!(!c.mark());
        assert!(c.wait(Duration::from_secs(3600)));
    }

    /// Hands out one chunk per read, each read taking `step` on the
    /// mock clock; records the timeout set before each read.
    struct Trickle {
        chunks: std::collections::VecDeque<Vec<u8>>,
        clock: Arc<MockClock>,
        step: Duration,
        timeouts: Vec<Duration>,
    }

    impl Trickle {
        fn new(chunks: &[&[u8]], clock: Arc<MockClock>, step: Duration) -> Trickle {
            Trickle { chunks: chunks.iter().map(|c| c.to_vec()).collect(), clock, step, timeouts: vec![] }
        }
    }

    impl Read for Trickle {
        fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
            self.clock.advance(self.step);
            let Some(mut c) = self.chunks.pop_front() else { return Ok(0) };
            let n = c.len().min(buf.len());
            buf[..n].copy_from_slice(&c[..n]);
            if n < c.len() {
                self.chunks.push_front(c.split_off(n));
            }
            Ok(n)
        }
    }

    impl TimedRead for Trickle {
        fn set_timeout(&mut self, d: Duration) -> io::Result<()> {
            self.timeouts.push(d);
            Ok(())
        }
    }

    fn read_all(chunks: &[&[u8]], step: Duration) -> (io::Result<Vec<u8>>, Vec<Duration>) {
        let clock = MockClock::new();
        let mut r = Trickle::new(chunks, clock.clone(), step);
        let deadline = clock.now() + READ_TIMEOUT;
        let got = read_head(&mut r, deadline, &|| clock.now());
        (got, r.timeouts)
    }

    #[test]
    fn heads_are_read_to_the_blank_line_and_capped_at_8_kib() {
        let z = Duration::ZERO;
        let (got, _) = read_all(&[b"GET / HTTP/1.1\r\nHost: x\r", b"\n\r\nleftover"], z);
        assert_eq!(got.unwrap(), b"GET / HTTP/1.1\r\nHost: x\r\n\r\nleftover");
        let big = format!("GET / HTTP/1.1\r\nX: {}\r\n\r\n", "a".repeat(MAX_REQUEST)).into_bytes();
        assert_eq!(read_all(&[&big], z).0.unwrap_err().kind(), io::ErrorKind::InvalidData);
        assert_eq!(read_all(&[b"GET / HTTP/1.1\r\n"], z).0.unwrap_err().kind(), io::ErrorKind::UnexpectedEof);
    }

    #[test]
    fn one_deadline_bounds_the_whole_head() {
        // A byte every two seconds: each read is well inside the 5 s, but
        // the third finds the deadline gone, and the timeouts shrink.
        let s = Duration::from_secs;
        let (got, timeouts) = read_all(&[b"G", b"E", b"T", b" ", b"/"], s(2));
        assert_eq!(got.unwrap_err().kind(), io::ErrorKind::TimedOut);
        assert_eq!(timeouts, [s(5), s(3), s(1)]);
    }

    #[test]
    fn request_lines() {
        let t = "tok";
        let ret = |q: &str| Answer::Return { query: q.into() };
        assert_eq!(answer(b"GET /signin/tok?code=c&state=s HTTP/1.1\r\n\r\n", t), ret("code=c&state=s"));
        assert_eq!(answer(b"GET /signin/tok HTTP/1.0\r\n\r\n", t), ret(""));
        assert_eq!(answer(b"GET /signin/t%6Fk?a=b HTTP/1.1\r\n\r\n", t), ret("a=b"));
        for bad in [
            &b"HEAD /signin/tok HTTP/1.1\r\n\r\n"[..],
            b"get /signin/tok HTTP/1.1\r\n\r\n",
            b"GET /signin/tok HTTP/2\r\n\r\n",
            b"GET /signin/tok\r\n\r\n",
            b"GET  /signin/tok HTTP/1.1\r\n\r\n",
            b"GET http://127.0.0.1/signin/tok HTTP/1.1\r\n\r\n",
            b"GET /signin/to HTTP/1.1\r\n\r\n",
            b"GET /signin/tokk HTTP/1.1\r\n\r\n",
            b"GET /signin/tok%2 HTTP/1.1\r\n\r\n",
            b"GET /SIGNIN/tok HTTP/1.1\r\n\r\n",
        ] {
            assert_eq!(answer(bad, t), Answer::NotFound, "{}", String::from_utf8_lossy(bad));
        }
    }

    #[test]
    fn callback_url_round_trips_its_values() {
        let pairs = [("code".to_string(), "a b&c/d".to_string()), ("state".to_string(), "desktop~x.y-z_".to_string())];
        let got = callback_url(&u("https://org.example.com:8443/").origin(), &pairs);
        assert_eq!(got.path(), "/auth/callback");
        let q: Vec<(String, String)> = got.query_pairs().into_owned().collect();
        assert_eq!(q, pairs);
        assert_eq!(callback_url(&u("http://127.0.0.1:8080").origin(), &[]).as_str(), "http://127.0.0.1:8080/auth/callback");
    }
}
