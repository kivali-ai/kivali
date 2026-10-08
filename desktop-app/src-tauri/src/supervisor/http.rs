//! The shell's own small HTTP/1.1 client, over whatever stream the
//! platform's transport hands it (a Unix socket, a named pipe).
//!
//! It does exactly what the supervisor's RPC needs and nothing more:
//! one request per connection (`Connection: close`, or `Upgrade`), a
//! JSON or empty body, and an answer framed by `Content-Length`, by
//! chunked transfer coding, or by the connection closing, which Go's
//! net/http all produce. A `101 Switching Protocols` answer hands back
//! the raw stream (the terminal and exec relays).

use crate::platform::Stream;
use std::io::{self, BufRead, BufReader, Read, Write};

/// The most a response head (status line and headers) may take.
const MAX_HEAD: u64 = 64 * 1024;
/// The most one chunk-size or trailer line may take.
const MAX_LINE: u64 = 4 * 1024;

pub struct Request<'a> {
    pub method: &'a str,
    /// The request target: path and query.
    pub target: &'a str,
    pub headers: Vec<(&'a str, String)>,
    pub body: Option<Vec<u8>>,
}

impl<'a> Request<'a> {
    pub fn new(method: &'a str, target: &'a str) -> Self {
        Self { method, target, headers: Vec::new(), body: None }
    }

    pub fn json(mut self, body: &impl serde::Serialize) -> io::Result<Self> {
        let bytes = serde_json::to_vec(body).map_err(io::Error::other)?;
        self.headers.push(("Content-Type", "application/json".into()));
        self.body = Some(bytes);
        Ok(self)
    }

    pub fn header(mut self, name: &'a str, value: impl Into<String>) -> Self {
        self.headers.push((name, value.into()));
        self
    }

    /// The bytes on the wire. `Host` is required by HTTP/1.1 (Go's server
    /// refuses a request without it) and means nothing on a local
    /// transport. Unless the request is an upgrade it asks the server to
    /// close after answering, so a body without a length ends with the
    /// connection.
    pub fn encode(&self) -> Vec<u8> {
        let mut head = format!("{} {} HTTP/1.1\r\nHost: kivali\r\n", self.method, self.target);
        let upgrading = self.headers.iter().any(|(n, _)| n.eq_ignore_ascii_case("upgrade"));
        for (n, v) in &self.headers {
            head.push_str(&format!("{n}: {v}\r\n"));
        }
        if !upgrading {
            head.push_str("Connection: close\r\n");
        }
        if let Some(b) = &self.body {
            head.push_str(&format!("Content-Length: {}\r\n", b.len()));
        } else if self.method != "GET" && self.method != "HEAD" {
            head.push_str("Content-Length: 0\r\n");
        }
        head.push_str("\r\n");
        let mut out = head.into_bytes();
        if let Some(b) = &self.body {
            out.extend_from_slice(b);
        }
        out
    }
}

/// How the response body ends.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Framing {
    /// This many bytes remain.
    Length(u64),
    /// Chunked; this many bytes remain in the current chunk (0: read the
    /// next size line).
    Chunked(u64),
    /// Until the server closes the connection.
    Close,
    Done,
}

pub struct Response {
    pub status: u16,
    pub reason: String,
    /// Header names lower-cased, in order.
    pub headers: Vec<(String, String)>,
    reader: BufReader<Box<dyn Stream>>,
    framing: Framing,
}

impl std::fmt::Debug for Response {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Response").field("status", &self.status).field("reason", &self.reason).finish()
    }
}

impl Response {
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers.iter().find(|(n, _)| n.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_str())
    }

    pub fn is_success(&self) -> bool {
        (200..300).contains(&self.status)
    }

    /// `200 OK`, as an error message shows it.
    pub fn status_line(&self) -> String {
        if self.reason.is_empty() {
            self.status.to_string()
        } else {
            format!("{} {}", self.status, self.reason)
        }
    }

    /// The body as a reader, framing removed.
    pub fn body(self) -> Body {
        Body { reader: self.reader, framing: self.framing }
    }

    /// The body read whole, at most `limit` bytes (more is an error).
    pub fn bytes(self, limit: u64) -> io::Result<Vec<u8>> {
        let mut out = Vec::new();
        self.body().take(limit + 1).read_to_end(&mut out)?;
        if out.len() as u64 > limit {
            return Err(io::Error::new(io::ErrorKind::InvalidData, format!("response body over {limit} bytes")));
        }
        Ok(out)
    }

    /// After `101 Switching Protocols`: the raw stream, with any bytes
    /// the server sent after its head still to be read first.
    pub fn upgraded(self) -> Upgraded {
        Upgraded { reader: self.reader }
    }
}

/// Sends `req` on `stream` and reads the response head. Informational
/// answers (1xx) other than `101` are skipped.
pub fn send(mut stream: Box<dyn Stream>, req: &Request) -> io::Result<Response> {
    stream.write_all(&req.encode())?;
    stream.flush()?;
    let mut reader = BufReader::new(stream);
    loop {
        let (status, reason, headers) = read_head(&mut reader)?;
        if (100..200).contains(&status) && status != 101 {
            continue;
        }
        let framing = framing(req.method, status, &headers)?;
        return Ok(Response { status, reason, headers, reader, framing });
    }
}

fn bad(msg: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, msg.into())
}

/// One line, CRLF or LF ended, at most `limit` bytes; None at a clean
/// end of stream before any byte.
fn read_line(r: &mut impl BufRead, limit: u64) -> io::Result<Option<String>> {
    let mut buf = Vec::new();
    let n = r.by_ref().take(limit).read_until(b'\n', &mut buf)?;
    if n == 0 {
        return Ok(None);
    }
    if buf.last() != Some(&b'\n') {
        return Err(if n as u64 >= limit { bad("line too long") } else { io::ErrorKind::UnexpectedEof.into() });
    }
    buf.pop();
    if buf.last() == Some(&b'\r') {
        buf.pop();
    }
    String::from_utf8(buf).map(Some).map_err(|_| bad("line is not UTF-8"))
}

type Head = (u16, String, Vec<(String, String)>);

/// The next line of a response head, charged to `budget`.
fn head_line(r: &mut impl BufRead, budget: &mut u64) -> io::Result<String> {
    let line = read_line(r, *budget)?
        .ok_or_else(|| io::Error::new(io::ErrorKind::UnexpectedEof, "the connection closed before a response"))?;
    *budget = budget.saturating_sub(line.len() as u64 + 2);
    if *budget == 0 {
        return Err(bad("response head too large"));
    }
    Ok(line)
}

fn read_head(r: &mut impl BufRead) -> io::Result<Head> {
    let mut budget = MAX_HEAD;
    let status_line = head_line(r, &mut budget)?;
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or_default();
    if !version.starts_with("HTTP/1.") {
        return Err(bad(format!("not an HTTP/1.x response: {status_line:?}")));
    }
    let status: u16 = parts
        .next()
        .and_then(|s| (s.len() == 3).then_some(s))
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| bad(format!("bad status line: {status_line:?}")))?;
    let reason = parts.next().unwrap_or_default().trim().to_string();
    let mut headers = Vec::new();
    loop {
        let line = head_line(r, &mut budget)?;
        if line.is_empty() {
            break;
        }
        let (n, v) = line.split_once(':').ok_or_else(|| bad(format!("bad header line: {line:?}")))?;
        headers.push((n.trim().to_ascii_lowercase(), v.trim().to_string()));
    }
    Ok((status, reason, headers))
}

fn framing(method: &str, status: u16, headers: &[(String, String)]) -> io::Result<Framing> {
    if method == "HEAD" || status == 101 || status == 204 || status == 304 || (100..200).contains(&status) {
        return Ok(Framing::Done);
    }
    let te = headers.iter().filter(|(n, _)| n == "transfer-encoding").map(|(_, v)| v.as_str()).collect::<Vec<_>>();
    if !te.is_empty() {
        let last = te.join(",").rsplit(',').next().unwrap_or_default().trim().to_ascii_lowercase();
        return if last == "chunked" { Ok(Framing::Chunked(0)) } else { Ok(Framing::Close) };
    }
    let mut length: Option<u64> = None;
    for (_, v) in headers.iter().filter(|(n, _)| n == "content-length") {
        let n: u64 = v.trim().parse().map_err(|_| bad(format!("bad Content-Length: {v:?}")))?;
        if length.is_some_and(|l| l != n) {
            return Err(bad("conflicting Content-Length headers"));
        }
        length = Some(n);
    }
    Ok(match length {
        Some(0) => Framing::Done,
        Some(n) => Framing::Length(n),
        None => Framing::Close,
    })
}

/// A response body, framing removed. Wrap it in a `BufReader` to read
/// NDJSON line by line as it arrives.
pub struct Body {
    reader: BufReader<Box<dyn Stream>>,
    framing: Framing,
}

fn eof(what: &str) -> io::Error {
    io::Error::new(io::ErrorKind::UnexpectedEof, format!("the connection closed {what}"))
}

impl Read for Body {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        if out.is_empty() {
            return Ok(0);
        }
        loop {
            match self.framing {
                Framing::Done => return Ok(0),
                Framing::Close => return self.reader.read(out),
                Framing::Length(left) => {
                    let want = out.len().min(usize::try_from(left).unwrap_or(usize::MAX));
                    let n = self.reader.read(&mut out[..want])?;
                    if n == 0 {
                        return Err(eof("inside the body"));
                    }
                    let left = left - n as u64;
                    self.framing = if left == 0 { Framing::Done } else { Framing::Length(left) };
                    return Ok(n);
                }
                Framing::Chunked(0) => {
                    let line = read_line(&mut self.reader, MAX_LINE)?.ok_or_else(|| eof("before the last chunk"))?;
                    let size = line.split(';').next().unwrap_or_default().trim();
                    let size = u64::from_str_radix(size, 16).map_err(|_| bad(format!("bad chunk size: {line:?}")))?;
                    if size == 0 {
                        // Trailers, up to the blank line that ends them.
                        loop {
                            match read_line(&mut self.reader, MAX_LINE)? {
                                Some(t) if !t.is_empty() => continue,
                                _ => break,
                            }
                        }
                        self.framing = Framing::Done;
                        return Ok(0);
                    }
                    self.framing = Framing::Chunked(size);
                }
                Framing::Chunked(left) => {
                    let want = out.len().min(usize::try_from(left).unwrap_or(usize::MAX));
                    let n = self.reader.read(&mut out[..want])?;
                    if n == 0 {
                        return Err(eof("inside a chunk"));
                    }
                    let left = left - n as u64;
                    if left == 0 {
                        // The CRLF after the chunk's data.
                        match read_line(&mut self.reader, MAX_LINE)? {
                            Some(l) if l.is_empty() => {}
                            _ => return Err(bad("a chunk is not followed by CRLF")),
                        }
                    }
                    self.framing = Framing::Chunked(left);
                    return Ok(n);
                }
            }
        }
    }
}

/// The stream after `101 Switching Protocols`: reads first return what
/// the server sent behind its head, then the stream itself.
pub struct Upgraded {
    reader: BufReader<Box<dyn Stream>>,
}

impl Read for Upgraded {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        self.reader.read(out)
    }
}

impl Write for Upgraded {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        self.reader.get_mut().write(b)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.reader.get_mut().flush()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    /// A canned server: reads come from `input`, writes are kept.
    struct Canned {
        input: io::Cursor<Vec<u8>>,
        written: Arc<Mutex<Vec<u8>>>,
    }

    impl Read for Canned {
        fn read(&mut self, b: &mut [u8]) -> io::Result<usize> {
            self.input.read(b)
        }
    }
    impl Write for Canned {
        fn write(&mut self, b: &[u8]) -> io::Result<usize> {
            self.written.lock().unwrap().extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    fn canned(answer: &[u8]) -> (Box<dyn Stream>, Arc<Mutex<Vec<u8>>>) {
        let written = Arc::new(Mutex::new(Vec::new()));
        (Box::new(Canned { input: io::Cursor::new(answer.to_vec()), written: written.clone() }), written)
    }

    fn body_text(r: Response) -> io::Result<String> {
        let mut s = String::new();
        r.body().read_to_string(&mut s)?;
        Ok(s)
    }

    #[test]
    fn request_bytes() {
        let req = Request::new("POST", "/v1/down").json(&serde_json::json!({"exit": true})).unwrap();
        assert_eq!(
            String::from_utf8(req.encode()).unwrap(),
            "POST /v1/down HTTP/1.1\r\nHost: kivali\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: 13\r\n\r\n{\"exit\":true}"
        );
        let get = Request::new("GET", "/v1/status");
        assert_eq!(String::from_utf8(get.encode()).unwrap(), "GET /v1/status HTTP/1.1\r\nHost: kivali\r\nConnection: close\r\n\r\n");
        let empty_post = Request::new("POST", "/v1/install");
        assert!(String::from_utf8(empty_post.encode()).unwrap().ends_with("Content-Length: 0\r\n\r\n"));
        let up = Request::new("GET", "/v1/terminal?rows=24").header("Connection", "Upgrade").header("Upgrade", "kivali-stream");
        let text = String::from_utf8(up.encode()).unwrap();
        assert!(text.contains("Connection: Upgrade\r\nUpgrade: kivali-stream\r\n"), "{text}");
        assert!(!text.contains("Connection: close"), "{text}");
    }

    #[test]
    fn content_length_body() {
        let (s, written) = canned(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{\"a\":true}\nTRAILING GARBAGE");
        let r = send(s, &Request::new("GET", "/v1/status")).unwrap();
        assert_eq!((r.status, r.reason.as_str()), (200, "OK"));
        assert_eq!(r.header("Content-Type"), Some("application/json"));
        assert!(r.is_success());
        assert_eq!(body_text(r).unwrap(), "{\"a\":true}\n");
        assert!(written.lock().unwrap().starts_with(b"GET /v1/status HTTP/1.1\r\n"));
    }

    #[test]
    fn chunked_ndjson_streams_line_by_line() {
        // Go's net/http: chunked, one flush per event, a chunk extension
        // and a trailer for good measure.
        let wire = b"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n\
            12\r\n{\"log\":\"booting\"}\n\r\n\
            7;ext=1\r\n{\"done\"\r\n\
            7\r\n:true}\n\r\n\
            0\r\nX-Trailer: yes\r\n\r\n";
        let r = send(canned(wire).0, &Request::new("POST", "/v1/up")).unwrap();
        let lines: Vec<String> = BufReader::new(r.body()).lines().map(Result::unwrap).collect();
        assert_eq!(lines, ["{\"log\":\"booting\"}", "{\"done\":true}"]);
    }

    #[test]
    fn close_delimited_body() {
        let wire = b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n{\"log\":\"a\"}\n{\"done\":true}\n";
        let r = send(canned(wire).0, &Request::new("POST", "/v1/down")).unwrap();
        assert_eq!(body_text(r).unwrap(), "{\"log\":\"a\"}\n{\"done\":true}\n");
    }

    #[test]
    fn informational_answers_are_skipped() {
        let wire = b"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n";
        let r = send(canned(wire).0, &Request::new("POST", "/v1/x")).unwrap();
        assert_eq!(r.status, 204);
        assert_eq!(body_text(r).unwrap(), "");
    }

    #[test]
    fn upgrade_returns_the_raw_stream() {
        let wire = b"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: kivali-stream\r\n\r\nframe-bytes";
        let (s, written) = canned(wire);
        let req = Request::new("GET", "/v1/exec").header("Connection", "Upgrade").header("Upgrade", "kivali-stream");
        let r = send(s, &req).unwrap();
        assert_eq!(r.status, 101);
        assert_eq!(r.header("upgrade"), Some("kivali-stream"));
        let mut up = r.upgraded();
        let mut got = String::new();
        up.read_to_string(&mut got).unwrap();
        assert_eq!(got, "frame-bytes");
        up.write_all(b"stdin").unwrap();
        assert!(written.lock().unwrap().ends_with(b"\r\n\r\nstdin"));
    }

    #[test]
    fn error_status_with_body() {
        let wire = b"HTTP/1.1 400 Bad Request\r\nContent-Length: 17\r\nConnection: close\r\n\r\nbad request: nope";
        let r = send(canned(wire).0, &Request::new("POST", "/v1/down")).unwrap();
        assert!(!r.is_success());
        assert_eq!(r.status_line(), "400 Bad Request");
        assert_eq!(r.bytes(1024).unwrap(), b"bad request: nope");
    }

    #[test]
    fn malformed_answers_are_errors() {
        let send_err = |wire: &[u8]| send(canned(wire).0, &Request::new("GET", "/")).unwrap_err();
        assert_eq!(send_err(b"").kind(), io::ErrorKind::UnexpectedEof);
        assert_eq!(send_err(b"HTTP/1.1 200 OK\r\nContent-Le").kind(), io::ErrorKind::UnexpectedEof);
        assert_eq!(send_err(b"SSH-2.0-OpenSSH\r\n\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(send_err(b"HTTP/1.1 2000 OK\r\n\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(send_err(b"HTTP/1.1 200 OK\r\nno colon\r\n\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(send_err(b"HTTP/1.1 200 OK\r\nContent-Length: x\r\n\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(
            send_err(b"HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n").kind(),
            io::ErrorKind::InvalidData
        );
        let mut huge = b"HTTP/1.1 200 OK\r\n".to_vec();
        huge.extend(std::iter::repeat_n(b'a', 70 * 1024));
        assert_eq!(send_err(&huge).kind(), io::ErrorKind::InvalidData);

        let body_err = |wire: &[u8]| body_text(send(canned(wire).0, &Request::new("GET", "/")).unwrap()).unwrap_err();
        // Short Content-Length body, truncated chunk, bad chunk size,
        // missing CRLF after a chunk, no last chunk.
        assert_eq!(body_err(b"HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc").kind(), io::ErrorKind::UnexpectedEof);
        assert_eq!(body_err(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nab").kind(), io::ErrorKind::UnexpectedEof);
        assert_eq!(body_err(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(body_err(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nabXY0\r\n\r\n").kind(), io::ErrorKind::InvalidData);
        assert_eq!(body_err(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nab\r\n").kind(), io::ErrorKind::UnexpectedEof);
    }

    #[test]
    fn body_size_limit() {
        let r = send(canned(b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello").0, &Request::new("GET", "/")).unwrap();
        assert_eq!(r.bytes(4).unwrap_err().kind(), io::ErrorKind::InvalidData);
    }
}
