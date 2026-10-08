//! The desktop's own Google sign-in, used once, when a team is created,
//! to learn who owns it ("Make it yours"). Only the email
//! is read.
//!
//! It runs through Kivali's public Google client and its relay, exactly
//! as a server does (docs/developers/auth.md, "The public client and the relay"),
//! with this app as the install:
//!
//! 1. The app listens on `127.0.0.1:<port>` and opens Google's consent
//!    page in the system browser with PKCE (S256), a nonce, and `state` =
//!    `<nonce>.<base64url(http://127.0.0.1:<port>/auth/callback)>`; the
//!    redirect URI is the relay's callback.
//! 2. The relay bounces the browser to that loopback address with the
//!    code (its rule: path exactly `/auth/callback`, https or loopback).
//! 3. The app checks `state`, trades the code at the relay's token route
//!    with the verifier, and trusts only the id_token inside the answer:
//!    signed by Google (RS256, Google's published keys), issuer, audience
//!    (the public client), expiry, this sign-in's nonce, a verified email.
//!
//! Nothing is stored; the token is dropped once the email is read.

use crate::signin::{self, Closer};
use base64::Engine as _;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io;
use std::net::{Ipv4Addr, TcpListener};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use url::Url;

/// Google OAuth client Kivali ships with (internal/auth/public_client.go,
/// `DefaultPublicClientID`; a test checks the two agree).
pub const PUBLIC_CLIENT_ID: &str = "508969299515-ol3l2a67ola8hfm2loru9ne8hksl8kfq.apps.googleusercontent.com";
/// The relay's base (internal/auth/public_client.go, `DefaultRelayURL`).
pub const RELAY_URL: &str = "https://kivali.ai/oauth/google";
const GOOGLE_AUTH_URL: &str = "https://accounts.google.com/o/oauth2/v2/auth";
const GOOGLE_JWKS_URL: &str = "https://www.googleapis.com/oauth2/v3/certs";
const GOOGLE_ISSUERS: [&str; 2] = ["https://accounts.google.com", "accounts.google.com"];
/// The only path the relay returns to.
pub const RETURN_PATH: &str = "/auth/callback";
/// How long the browser has to come back before the page says it didn't.
pub const TTL: Duration = Duration::from_secs(5 * 60);

/// A sign-in waiting on the browser. Dropping it closes the listener
/// without an answer.
pub struct Pending {
    closer: Closer,
}

impl Drop for Pending {
    fn drop(&mut self) {
        self.closer.close();
    }
}

/// What one sign-in sends and expects back.
#[derive(Debug, Clone)]
pub struct Request {
    pub nonce: String,
    pub verifier: String,
    pub return_url: String,
    pub state: String,
}

fn random_b64(n: usize) -> io::Result<String> {
    let mut b = vec![0u8; n];
    getrandom::fill(&mut b).map_err(|e| io::Error::other(e.to_string()))?;
    Ok(base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(b))
}

impl Request {
    pub fn new(port: u16, nonce: String, verifier: String) -> Request {
        let return_url = format!("http://127.0.0.1:{port}{RETURN_PATH}");
        let state = format!("{nonce}.{}", base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(&return_url));
        Request { nonce, verifier, return_url, state }
    }

    /// Google's consent page for this sign-in.
    pub fn auth_url(&self) -> Url {
        let challenge = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(Sha256::digest(self.verifier.as_bytes()));
        let mut u = Url::parse(GOOGLE_AUTH_URL).expect("a constant URL");
        u.query_pairs_mut()
            .append_pair("client_id", PUBLIC_CLIENT_ID)
            .append_pair("redirect_uri", &format!("{RELAY_URL}/callback"))
            .append_pair("response_type", "code")
            .append_pair("scope", "openid email")
            .append_pair("state", &self.state)
            .append_pair("nonce", &self.nonce)
            .append_pair("code_challenge", &challenge)
            .append_pair("code_challenge_method", "S256")
            .append_pair("access_type", "online")
            .append_pair("prompt", "select_account");
        u
    }
}

/// What the browser brought back.
#[derive(Debug, PartialEq, Eq)]
pub enum Back {
    Code(String),
    /// Google's `error` (the person declined, say).
    Refused(String),
}

/// Reads one request head: `GET /auth/callback?…` whose `state` is this
/// sign-in's. Anything else is None (answered 404, and the listener
/// keeps waiting).
pub fn parse_return(head: &[u8], state: &str) -> Option<Back> {
    let line = std::str::from_utf8(head.split(|&b| b == b'\n').next()?).ok()?;
    let mut parts = line.trim_end_matches('\r').split(' ');
    let (Some("GET"), Some(target), Some(v), None) = (parts.next(), parts.next(), parts.next(), parts.next()) else {
        return None;
    };
    if !v.starts_with("HTTP/1.") {
        return None;
    }
    let (path, query) = target.split_once('?')?;
    if signin::percent_decode(path)? != RETURN_PATH.as_bytes() {
        return None;
    }
    let pairs: Vec<(String, String)> = url::form_urlencoded::parse(query.as_bytes()).into_owned().collect();
    let got = signin::param(&pairs, "state")?;
    if !signin::constant_time_eq(got.as_bytes(), state.as_bytes()) {
        return None;
    }
    match (signin::param(&pairs, "code"), signin::param(&pairs, "error")) {
        (Some(c), _) => Some(Back::Code(c.to_string())),
        (None, Some(e)) => Some(Back::Refused(e.chars().take(64).collect())),
        (None, None) => Some(Back::Refused("no code".into())),
    }
}

#[derive(Deserialize)]
struct Jwk {
    kid: String,
    #[serde(default)]
    kty: String,
    n: String,
    e: String,
}

#[derive(Deserialize)]
struct Jwks {
    keys: Vec<Jwk>,
}

#[derive(Deserialize)]
struct Claims {
    #[serde(default)]
    iss: String,
    #[serde(default)]
    aud: serde_json::Value,
    #[serde(default)]
    exp: i64,
    #[serde(default)]
    nonce: String,
    #[serde(default)]
    email: String,
    #[serde(default)]
    email_verified: serde_json::Value,
}

/// The verified, lower-cased email in a Google id_token, checked as the
/// server checks one (internal/auth/oauth.go, `emailFromIDToken`).
pub fn verify_id_token(raw: &str, jwks: &[u8], client_id: &str, nonce: &str, now_unix: i64) -> Result<String, String> {
    let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
    let parts: Vec<&str> = raw.split('.').collect();
    let [h, p, s] = parts[..] else { return Err("the id_token is not a JWT".into()) };
    #[derive(Deserialize)]
    struct Header {
        alg: String,
        #[serde(default)]
        kid: String,
    }
    let header: Header = serde_json::from_slice(&b64.decode(h).map_err(|_| "id_token header")?).map_err(|_| "id_token header")?;
    if header.alg != "RS256" {
        return Err(format!("id_token alg {:?}, want RS256", header.alg));
    }
    let keys: Jwks = serde_json::from_slice(jwks).map_err(|e| format!("Google's keys: {e}"))?;
    let key = keys
        .keys
        .iter()
        .find(|k| k.kid == header.kid && (k.kty.is_empty() || k.kty == "RSA"))
        .ok_or("the id_token's key is not one of Google's")?;
    let n = b64.decode(&key.n).map_err(|_| "Google's key")?;
    let e = b64.decode(&key.e).map_err(|_| "Google's key")?;
    let sig = b64.decode(s).map_err(|_| "id_token signature")?;
    let pk = ring::signature::RsaPublicKeyComponents { n: &n, e: &e };
    pk.verify(&ring::signature::RSA_PKCS1_2048_8192_SHA256, format!("{h}.{p}").as_bytes(), &sig)
        .map_err(|_| "the id_token's signature does not verify")?;
    let c: Claims = serde_json::from_slice(&b64.decode(p).map_err(|_| "id_token payload")?).map_err(|_| "id_token payload")?;
    if !GOOGLE_ISSUERS.contains(&c.iss.as_str()) {
        return Err(format!("id_token issuer {:?} not accepted", c.iss));
    }
    let aud_ok = match &c.aud {
        serde_json::Value::String(a) => a == client_id,
        serde_json::Value::Array(a) => a.iter().any(|v| v.as_str() == Some(client_id)),
        _ => false,
    };
    if !aud_ok {
        return Err("the id_token is not for Kivali".into());
    }
    if c.exp == 0 || now_unix > c.exp {
        return Err("the id_token expired".into());
    }
    if c.nonce.is_empty() || !signin::constant_time_eq(c.nonce.as_bytes(), nonce.as_bytes()) {
        return Err("the id_token is not for this sign-in".into());
    }
    let verified = matches!(&c.email_verified, serde_json::Value::Bool(true)) || c.email_verified.as_str() == Some("true");
    if c.email.is_empty() || !verified {
        return Err("Google didn't confirm that account's email address".into());
    }
    Ok(c.email.to_lowercase())
}

/// A refusal from the relay, or Google's passed through it, as one line:
/// `the sign-in service answered 401 (invalid_client: The provided client
/// secret is invalid.)`. Both answer JSON `{error, error_description}`;
/// anything else is left out. This is what shell.log and the setup page show, so the
/// cause (a relay whose secret Google rejects, say) is not a guess.
fn relay_refusal(status: u16, body: &[u8]) -> String {
    #[derive(Deserialize)]
    struct Refusal {
        #[serde(default)]
        error: String,
        #[serde(default)]
        error_description: String,
    }
    let detail = serde_json::from_slice::<Refusal>(body)
        .ok()
        .map(|r| match (r.error.is_empty(), r.error_description.is_empty()) {
            (true, true) => String::new(),
            (false, true) => r.error,
            (true, false) => r.error_description,
            (false, false) => format!("{}: {}", r.error, r.error_description),
        })
        .unwrap_or_default();
    let detail: String = detail.chars().filter(|c| !c.is_control()).take(200).collect();
    if detail.is_empty() {
        format!("the sign-in service answered {status}")
    } else {
        format!("the sign-in service answered {status} ({detail})")
    }
}

/// Trades the code at the relay and verifies the id_token.
async fn exchange(code: &str, req: &Request) -> Result<String, String> {
    let client = crate::http_client(reqwest::redirect::Policy::none())?;
    let form = [
        ("grant_type", "authorization_code"),
        ("code", code),
        ("code_verifier", req.verifier.as_str()),
        ("client_id", PUBLIC_CLIENT_ID),
        ("redirect_uri", &format!("{RELAY_URL}/callback")),
        // The relay redeems a sealed code only for the callback it was
        // sealed to (docs/developers/auth.md, The public client and the relay).
        ("return_url", req.return_url.as_str()),
    ];
    let body = url::form_urlencoded::Serializer::new(String::new()).extend_pairs(form).finish();
    let resp = client
        .post(format!("{RELAY_URL}/token"))
        .header("content-type", "application/x-www-form-urlencoded")
        .body(body)
        .send()
        .await
        .map_err(|e| format!("couldn't reach the sign-in service ({e})"))?;
    let status = resp.status().as_u16();
    let bytes = resp.bytes().await.map_err(|e| e.to_string())?;
    if status >= 400 {
        return Err(relay_refusal(status, &bytes));
    }
    #[derive(Deserialize)]
    struct Tok {
        #[serde(default)]
        id_token: String,
    }
    let tok: Tok = serde_json::from_slice(&bytes).map_err(|_| "the sign-in service answered unexpectedly")?;
    if tok.id_token.is_empty() {
        return Err("Google returned no identity".into());
    }
    let jwks = client.get(GOOGLE_JWKS_URL).send().await.map_err(|e| format!("couldn't reach Google ({e})"))?;
    let jwks = jwks.bytes().await.map_err(|e| e.to_string())?;
    let now = time::OffsetDateTime::now_utc().unix_timestamp();
    verify_id_token(&tok.id_token, &jwks, PUBLIC_CLIENT_ID, &req.nonce, now)
}

/// Starts a sign-in: listens, and returns the page to open in the
/// browser. `done` runs once, on another thread: the email, or why not
/// (`"timeout"` when the browser never came back within [`TTL`]). It
/// never runs when the [`Pending`] is dropped first.
pub fn start<F>(done: F) -> io::Result<(Pending, Url)>
where
    F: FnOnce(Result<String, String>) + Send + 'static,
{
    let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))?;
    let port = listener.local_addr()?.port();
    let req = Request::new(port, random_b64(32)?, random_b64(48)?);
    let url = req.auth_url();
    let closer = Closer::new(port);
    let done = Arc::new(Mutex::new(Some(done)));
    let finish = {
        let done = done.clone();
        move |r: Result<String, String>| {
            if let Some(f) = done.lock().unwrap().take() {
                f(r);
            }
        }
    };
    let finish2 = finish.clone();
    let c2 = closer.clone();
    std::thread::Builder::new().name("kivali-owner-signin".into()).spawn(move || {
        for conn in listener.incoming() {
            if c2.is_closed() {
                return;
            }
            let Ok(mut stream) = conn else { continue };
            let _ = stream.set_write_timeout(Some(signin::READ_TIMEOUT));
            let deadline = Instant::now() + signin::READ_TIMEOUT;
            let back = signin::read_head(&mut stream, deadline, &Instant::now).ok().and_then(|h| parse_return(&h, &req.state));
            if c2.is_closed() {
                return;
            }
            match back {
                None => signin::respond(&mut stream, "404 Not Found", "text/plain; charset=utf-8", "Not found\n"),
                Some(Back::Refused(e)) => {
                    signin::respond(
                        &mut stream,
                        "200 OK",
                        "text/html; charset=utf-8",
                        &signin::page(&format!(
                            "Sign-in was not completed ({}). Return to Kivali and try again.",
                            signin::html_escape(&e)
                        )),
                    );
                    c2.mark();
                    drop(listener);
                    return finish2(Err(e));
                }
                Some(Back::Code(code)) => {
                    signin::respond(
                        &mut stream,
                        "200 OK",
                        "text/html; charset=utf-8",
                        &signin::page("Signed in. You can close this tab and return to Kivali."),
                    );
                    c2.mark();
                    drop(listener);
                    let r = tauri::async_runtime::block_on(exchange(&code, &req));
                    if let Err(e) = &r {
                        eprintln!("kivali: owner sign-in: {e}");
                    }
                    return finish2(r);
                }
            }
        }
    })?;
    let timer = closer.clone();
    std::thread::Builder::new().name("kivali-owner-signin-ttl".into()).spawn(move || {
        if !timer.wait(TTL) {
            timer.close();
            finish(Err("timeout".into()));
        }
    })?;
    Ok((Pending { closer }, url))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn public_client_matches_the_server() {
        let go = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../../internal/auth/public_client.go")).unwrap();
        assert!(go.contains(&format!("DefaultPublicClientID = \"{PUBLIC_CLIENT_ID}\"")));
        assert!(go.contains(&format!("DefaultRelayURL = \"{RELAY_URL}\"")));
    }

    #[test]
    fn state_and_auth_url() {
        let r = Request::new(5123, "n0nce".into(), "verifier".into());
        assert_eq!(r.return_url, "http://127.0.0.1:5123/auth/callback");
        let (nonce, enc) = r.state.split_once('.').unwrap();
        assert_eq!(nonce, "n0nce");
        let dec = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(enc).unwrap();
        assert_eq!(dec, r.return_url.as_bytes());
        let u = r.auth_url();
        let q: std::collections::HashMap<_, _> = u.query_pairs().into_owned().collect();
        assert_eq!(q["redirect_uri"], "https://kivali.ai/oauth/google/callback");
        assert_eq!(q["client_id"], PUBLIC_CLIENT_ID);
        assert_eq!(q["code_challenge_method"], "S256");
        // S256 of "verifier".
        assert_eq!(q["code_challenge"], "iMnq5o6zALKXGivsnlom_0F5_WYda32GHkxlV7mq7hQ");
        assert_eq!(q["state"], r.state);
    }

    #[test]
    fn refusals_carry_the_relay_s_words() {
        assert_eq!(
            relay_refusal(401, br#"{"error":"invalid_client","error_description":"The provided client secret is invalid."}"#),
            "the sign-in service answered 401 (invalid_client: The provided client secret is invalid.)"
        );
        assert_eq!(relay_refusal(400, br#"{"error":"invalid_grant"}"#), "the sign-in service answered 400 (invalid_grant)");
        assert_eq!(relay_refusal(502, b"<html>bad gateway</html>"), "the sign-in service answered 502");
    }

    #[test]
    fn returns() {
        let st = "abc.def";
        let get = |t: &str| format!("GET {t} HTTP/1.1\r\nHost: x\r\n\r\n");
        assert_eq!(parse_return(get("/auth/callback?code=c1&state=abc.def").as_bytes(), st), Some(Back::Code("c1".into())));
        assert_eq!(
            parse_return(get("/auth/callback?error=access_denied&state=abc.def").as_bytes(), st),
            Some(Back::Refused("access_denied".into()))
        );
        assert_eq!(parse_return(get("/auth/callback?code=c1&state=other").as_bytes(), st), None);
        assert_eq!(parse_return(get("/auth/callback?code=c1").as_bytes(), st), None);
        assert_eq!(parse_return(get("/elsewhere?code=c1&state=abc.def").as_bytes(), st), None);
        assert_eq!(parse_return(b"POST /auth/callback?code=c&state=abc.def HTTP/1.1\r\n\r\n", st), None);
    }

    /// A token signed with a throwaway test key (testdata, made with
    /// `openssl genpkey -algorithm RSA`), published as a JWKS the way
    /// Google publishes its own.
    fn signed(claims: &str) -> (String, Vec<u8>) {
        let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
        let der = include_bytes!("testdata/idtoken-test-key.der");
        let kp = ring::signature::RsaKeyPair::from_der(der).unwrap();
        let h = b64.encode(br#"{"alg":"RS256","kid":"k1","typ":"JWT"}"#);
        let p = b64.encode(claims);
        let mut sig = vec![0u8; kp.public().modulus_len()];
        kp.sign(&ring::signature::RSA_PKCS1_SHA256, &ring::rand::SystemRandom::new(), format!("{h}.{p}").as_bytes(), &mut sig)
            .unwrap();
        let pubk: ring::rsa::PublicKeyComponents<Vec<u8>> = kp.public().into();
        let n = b64.encode(&pubk.n);
        let e = b64.encode(&pubk.e);
        let jwks = format!(r#"{{"keys":[{{"kid":"other","kty":"RSA","n":"{n}","e":"{e}"}},{{"kid":"k1","kty":"RSA","n":"{n}","e":"{e}"}}]}}"#);
        (format!("{h}.{p}.{}", b64.encode(sig)), jwks.into_bytes())
    }

    #[test]
    fn id_token_accepted_and_checked() {
        let ok = r#"{"iss":"https://accounts.google.com","aud":"cid","exp":2000,"nonce":"n1","email":"Dana@Example.com","email_verified":true}"#;
        let (tok, jwks) = signed(ok);
        assert_eq!(verify_id_token(&tok, &jwks, "cid", "n1", 1000).unwrap(), "dana@example.com");
        assert!(verify_id_token(&tok, &jwks, "other-client", "n1", 1000).unwrap_err().contains("not for Kivali"));
        assert!(verify_id_token(&tok, &jwks, "cid", "n2", 1000).unwrap_err().contains("not for this sign-in"));
        assert!(verify_id_token(&tok, &jwks, "cid", "n1", 2001).unwrap_err().contains("expired"));
        // A changed payload no longer matches the signature.
        let parts: Vec<&str> = tok.split('.').collect();
        let (other, _) = signed(&ok.replace("Dana", "Mallory"));
        let forged = format!("{}.{}.{}", parts[0], other.split('.').nth(1).unwrap(), parts[2]);
        assert!(verify_id_token(&forged, &jwks, "cid", "n1", 1000).unwrap_err().contains("signature"));
        let (tok, jwks) = signed(&ok.replace("true", "false"));
        assert!(verify_id_token(&tok, &jwks, "cid", "n1", 1000).unwrap_err().contains("confirm"));
        let (tok, jwks) = signed(&ok.replace("https://accounts.google.com", "https://evil.example"));
        assert!(verify_id_token(&tok, &jwks, "cid", "n1", 1000).unwrap_err().contains("issuer"));
    }

    #[test]
    fn id_token_refusals() {
        let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
        let jwks = br#"{"keys":[{"kid":"k1","kty":"RSA","n":"AQAB","e":"AQAB"}]}"#;
        assert!(verify_id_token("a.b", jwks, "c", "n", 0).unwrap_err().contains("not a JWT"));
        let h = b64.encode(br#"{"alg":"HS256","kid":"k1"}"#);
        assert!(verify_id_token(&format!("{h}.e30.c2ln"), jwks, "c", "n", 0).unwrap_err().contains("RS256"));
        let h = b64.encode(br#"{"alg":"RS256","kid":"nope"}"#);
        assert!(verify_id_token(&format!("{h}.e30.c2ln"), jwks, "c", "n", 0).unwrap_err().contains("not one of Google's"));
        let h = b64.encode(br#"{"alg":"RS256","kid":"k1"}"#);
        assert!(verify_id_token(&format!("{h}.e30.c2ln"), jwks, "c", "n", 0).unwrap_err().contains("signature"));
    }
}
