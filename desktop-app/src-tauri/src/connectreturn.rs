//! The browser's return from an integration's sign-in (docs/developers/desktop-app.md,
//! "Signing in to an integration"; the server's half is
//! docs/developers/integrations.md, "In Kivali Desktop").
//!
//! An integration's Go step runs in the system browser, handed off from
//! the team's window (windows.rs, `open_connect_sign_in`). The window
//! stays and follows the flow, so the browser's part ends at the team's
//! `/connect/callback`, which answers it with a page that says the setup
//! goes on in the app. For the app to come to the front at that moment,
//! the shell listens on `127.0.0.1:<port>` meanwhile, as the desktop
//! sign-in does for Google's answer (signin.rs), and the handoff
//! carries the listener's `return_port` and `return_token`; the server
//! keeps them with the flow, and the callback's page tells the listener
//! with one `GET /connect/<token>`. The listener answers it with nothing
//! (204), closes, and the shell brings the team's window forward.
//!
//! The listener lives at most [`RETURN_TTL`], and closes when a newer
//! sign-in of the same team starts or the pending return is dropped.
//! Nothing but the app's own token reaches it, and the token is never
//! logged.

use crate::signin::{self, constant_time_eq, percent_decode, read_head, respond, Clock, Closer, READ_TIMEOUT};
use std::io;
use std::net::{Ipv4Addr, TcpListener};
use std::sync::Arc;
use std::time::Instant;
use url::Url;

/// The listener's one path, followed by the token; the server builds
/// the same (internal/web, `desktopReturnURL`).
pub const RETURN_PREFIX: &str = "/connect/";
/// How long a browser sign-in may take before the listener gives up:
/// the flow itself lasts ten minutes after its last change on the
/// server, and the owner may read the service's pages a while first.
pub const RETURN_TTL: std::time::Duration = std::time::Duration::from_secs(15 * 60);

/// A browser sign-in handed off and not yet back. Dropping it closes
/// its listener. No `Debug`: it holds the token.
pub struct PendingReturn {
    pub port: u16,
    pub token: String,
    listener: Closer,
}

impl Drop for PendingReturn {
    fn drop(&mut self) {
        self.listener.close();
    }
}

/// The handoff address with the listener appended: `return_port` and
/// `return_token`, as the desktop sign-in names its own.
pub fn with_return(handoff: &Url, port: u16, token: &str) -> Url {
    let mut u = handoff.clone();
    u.query_pairs_mut().append_pair("return_port", &port.to_string()).append_pair("return_token", token);
    u
}

/// Starts listening for the browser's return: binds `127.0.0.1:0`,
/// makes a token and serves the return on a thread of its own.
/// `on_return` runs on that thread, once, with the listener's token,
/// after the listener has answered and closed; it never runs when the
/// listener closes first (expiry, a newer sign-in, the
/// [`PendingReturn`] dropped).
pub fn start<F>(clock: Arc<dyn Clock>, on_return: F) -> io::Result<PendingReturn>
where
    F: FnOnce(String) + Send + 'static,
{
    let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))?;
    let port = listener.local_addr()?.port();
    let token = signin::new_token()?;
    let started = clock.now();
    let closer = Closer::new(port);
    let job = Job { listener, token: token.clone(), started, clock, closer: closer.clone() };
    std::thread::Builder::new().name("kivali-connect-return".into()).spawn(move || job.serve(on_return))?;
    let timer = closer.clone();
    std::thread::Builder::new().name("kivali-connect-return-ttl".into()).spawn(move || {
        if !timer.wait(RETURN_TTL) {
            eprintln!("kivali: an integration's sign-in did not come back within 15 minutes; its listener is closed");
            timer.close();
        }
    })?;
    Ok(PendingReturn { port, token, listener: closer })
}

struct Job {
    listener: TcpListener,
    token: String,
    started: Instant,
    clock: Arc<dyn Clock>,
    closer: Closer,
}

impl Job {
    fn serve<F: FnOnce(String)>(self, on_return: F) {
        let Job { listener, token, started, clock, closer } = self;
        let mut returned = false;
        for conn in listener.incoming() {
            if closer.is_closed() {
                break;
            }
            let Ok(mut stream) = conn else { continue };
            let _ = stream.set_write_timeout(Some(READ_TIMEOUT));
            let deadline = Instant::now() + READ_TIMEOUT;
            let matched = match read_head(&mut stream, deadline, &Instant::now) {
                Ok(head) => is_return(&head, &token),
                Err(_) => false,
            };
            // A newer sign-in (or expiry) closed this one while the
            // request was being read.
            if closer.is_closed() {
                break;
            }
            if !matched {
                eprintln!("kivali: connect return listener: refused a request");
                respond(&mut stream, "404 Not Found", "text/plain; charset=utf-8", "Not found\n");
                continue;
            }
            if clock.now().saturating_duration_since(started) > RETURN_TTL {
                eprintln!("kivali: connect return listener: refused a return after 15 minutes");
                respond(&mut stream, "410 Gone", "text/plain; charset=utf-8", "");
                closer.mark();
                break;
            }
            // The page reads nothing of the answer (it asks with
            // no-cors); a 204 says the listener heard it.
            respond(&mut stream, "204 No Content", "text/plain; charset=utf-8", "");
            closer.mark();
            returned = true;
            break;
        }
        // The listener closes before the caller hears of the return.
        drop(listener);
        if returned {
            on_return(token);
        }
    }
}

/// Whether one request head is `GET /connect/<token>`, with any query.
fn is_return(head: &[u8], token: &str) -> bool {
    let Some(line) = head.split(|&b| b == b'\n').next() else { return false };
    let Ok(line) = std::str::from_utf8(line) else { return false };
    let mut parts = line.trim_end_matches('\r').split(' ');
    let (Some("GET"), Some(target), Some(version), None) = (parts.next(), parts.next(), parts.next(), parts.next()) else {
        return false;
    };
    if !version.starts_with("HTTP/1.") {
        return false;
    }
    let (path, _query) = target.split_once('?').unwrap_or((target, ""));
    let Some(path) = percent_decode(path) else { return false };
    let Some(got) = path.strip_prefix(RETURN_PREFIX.as_bytes()) else { return false };
    constant_time_eq(got, token.as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpStream;
    use std::sync::{mpsc, Mutex};
    use std::time::Duration;

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
    fn get(port: u16, target: &str) -> String {
        let mut s = TcpStream::connect((Ipv4Addr::LOCALHOST, port)).unwrap();
        s.write_all(format!("GET {target} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\n\r\n").as_bytes()).unwrap();
        let mut out = String::new();
        s.read_to_string(&mut out).unwrap();
        out
    }

    fn start_test(clock: Arc<MockClock>) -> (PendingReturn, mpsc::Receiver<String>) {
        let (tx, rx) = mpsc::channel();
        let p = start(clock, move |token| tx.send(token).unwrap()).unwrap();
        (p, rx)
    }

    const WAIT: Duration = Duration::from_secs(10);

    // Why this test exists: the server reads the listener out of the
    // handoff's return_port and return_token, by the names the desktop
    // sign-in gave them; a handoff naming them otherwise would still
    // sign the browser in, and the app would never come forward.
    #[test]
    fn with_return_appends_the_listener_to_the_handoff() {
        let flow = "9f86d081884c7d659a2feaa0c55ad015";
        let handoff = Url::parse(&format!("http://127.0.0.1:8080/auth/handoff?t=a.b&next={flow}")).unwrap();
        let got = with_return(&handoff, 54321, "abcDEF0123456789-_abcDEF0123456789-_abcDEF0");
        assert_eq!(
            got.as_str(),
            format!("http://127.0.0.1:8080/auth/handoff?t=a.b&next={flow}&return_port=54321&return_token=abcDEF0123456789-_abcDEF0123456789-_abcDEF0")
        );
    }

    // Why this test exists: the callback's page asks the listener once,
    // with no-cors, and the shell must then bring the team's window
    // forward exactly once and stop listening: a second request must
    // find nothing, so no page the owner visits later can bring the app
    // forward.
    #[test]
    fn the_return_answers_204_once_and_closes() {
        let clock = MockClock::new();
        let (p, rx) = start_test(clock.clone());
        assert_ne!(p.port, 0);
        assert_eq!(p.token.len(), 43);
        clock.advance(RETURN_TTL);
        let got = get(p.port, &format!("/connect/{}", p.token));
        assert!(got.starts_with("HTTP/1.1 204 No Content\r\n"), "{got}");
        assert!(got.contains("\r\nCache-Control: no-store\r\n"));
        assert!(got.contains("\r\nConnection: close\r\n"));
        assert_eq!(rx.recv_timeout(WAIT).unwrap(), p.token);
        assert!(TcpStream::connect((Ipv4Addr::LOCALHOST, p.port)).is_err());
    }

    // Why this test exists: only the app's own token brings it forward.
    // Any other request is refused and the listener keeps waiting for
    // the real return.
    #[test]
    fn anything_else_is_404_and_the_listener_keeps_waiting() {
        let clock = MockClock::new();
        let (p, rx) = start_test(clock);
        let last = if p.token.ends_with('A') { 'B' } else { 'A' };
        let wrong = format!("{}{last}", &p.token[..42]);
        for target in [
            format!("/connect/{wrong}"),
            format!("/connect/{}x", p.token),
            format!("/signin/{}", p.token),
            "/connect/".to_string(),
            "/".to_string(),
        ] {
            let got = get(p.port, &target);
            assert!(got.starts_with("HTTP/1.1 404 Not Found\r\n"), "{target}: {got}");
        }
        assert!(rx.try_recv().is_err());
        let got = get(p.port, &format!("/connect/{}?x=1", p.token));
        assert!(got.starts_with("HTTP/1.1 204 No Content\r\n"), "{got}");
        rx.recv_timeout(WAIT).unwrap();
    }

    // Why this test exists: a return after the listener's time is no
    // return: the flow on the server has ended by then, and bringing
    // the app forward on a stale request would surprise the owner.
    #[test]
    fn a_return_past_its_time_is_gone() {
        let clock = MockClock::new();
        let (p, rx) = start_test(clock.clone());
        clock.advance(RETURN_TTL + Duration::from_secs(1));
        let got = get(p.port, &format!("/connect/{}", p.token));
        assert!(got.starts_with("HTTP/1.1 410 Gone\r\n"), "{got}");
        // The answer came after the listener had decided: nothing more
        // can run on_return.
        assert!(rx.try_recv().is_err());
        assert!(TcpStream::connect((Ipv4Addr::LOCALHOST, p.port)).is_err());
    }

    // Why this test exists: a newer sign-in of the same team replaces
    // the pending return, and dropping it must close the listener, or
    // ports and threads would pile up across a day of sign-ins.
    #[test]
    fn dropping_the_pending_return_closes_the_listener() {
        let clock = MockClock::new();
        let (p, rx) = start_test(clock);
        let (port, token) = (p.port, p.token.clone());
        drop(p);
        // Closed before the drop returned: a request with the token now
        // is refused the connection, or connected and left unanswered
        // as the listener leaves; it is never a return.
        let answered = TcpStream::connect((Ipv4Addr::LOCALHOST, port))
            .and_then(|mut s| {
                s.write_all(format!("GET /connect/{token} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\n\r\n").as_bytes())?;
                let mut out = String::new();
                s.read_to_string(&mut out)?;
                Ok(out)
            })
            .unwrap_or_default();
        assert!(!answered.contains("204"), "{answered}");
        assert!(rx.try_recv().is_err());
    }
}
