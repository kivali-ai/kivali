//! Remote org addresses: what a person may type into **Connect to an
//! existing org**, and how the answer to `GET /api/v1/login` proves the
//! address is a Kivali org.

use serde::{Deserialize, Serialize};
use std::net::IpAddr;
use url::Url;

/// Normalises what a person typed into an org's origin
/// (`scheme://host[:port]`, no path). A bare host gets `https://`.
/// Plain `http` is accepted only for loopback, where a local org lives:
/// the server's cookies carry `Secure` everywhere else (docs/developers/auth.md),
/// so sign-in over http to any other host cannot work.
pub fn normalize_org_url(input: &str) -> Result<Url, String> {
    let s = input.trim();
    if s.is_empty() {
        return Err("Enter the org's address.".into());
    }
    let with_scheme = if s.contains("://") {
        s.to_string()
    } else {
        format!("https://{s}")
    };
    let url = Url::parse(&with_scheme).map_err(|e| format!("That isn't a web address ({e})."))?;
    match url.scheme() {
        "https" => {}
        "http" if is_loopback(&url) => {}
        "http" => {
            return Err(
                "Use https:// for an org on another machine; sign-in needs a secure connection."
                    .into(),
            )
        }
        other => return Err(format!("Kivali orgs are served over https, not {other}.")),
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("Leave the user name and password out of the address.".into());
    }
    let host = url.host_str().unwrap_or_default();
    if host.is_empty() {
        return Err("The address needs a host name.".into());
    }
    let mut origin = url.clone();
    origin.set_path("/");
    origin.set_query(None);
    origin.set_fragment(None);
    Ok(origin)
}

/// The origin as stored in teams.json: no trailing slash.
pub fn origin_string(url: &Url) -> String {
    url.as_str().trim_end_matches('/').to_string()
}

pub fn is_loopback(url: &Url) -> bool {
    match url.host() {
        Some(url::Host::Domain(d)) => d.eq_ignore_ascii_case("localhost"),
        Some(url::Host::Ipv4(ip)) => IpAddr::V4(ip).is_loopback(),
        Some(url::Host::Ipv6(ip)) => IpAddr::V6(ip).is_loopback(),
        None => false,
    }
}

/// What `GET /api/v1/login` tells anyone (internal/web/apitypes/setup.go,
/// `Login`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LoginInfo {
    pub org_name: String,
    pub auth_ready: bool,
    pub dev_mode: bool,
}

#[derive(Deserialize)]
struct LoginWire {
    org: LoginOrg,
    auth_ready: bool,
    #[serde(default)]
    dev_mode: bool,
}
#[derive(Deserialize)]
struct LoginOrg {
    #[serde(default)]
    name: String,
}

/// Checks a response to `GET /api/v1/login`: status 200, a JSON content
/// type and the Login shape. Anything else is "not a Kivali org".
pub fn parse_login_response(
    status: u16,
    content_type: Option<&str>,
    body: &[u8],
) -> Result<LoginInfo, String> {
    const NOT_KIVALI: &str = "That address answers, but not as a Kivali org.";
    if status != 200 {
        return Err(format!("{NOT_KIVALI} (HTTP {status} from /api/v1/login)"));
    }
    let json = content_type
        .map(|c| {
            c.split(';')
                .next()
                .unwrap_or_default()
                .trim()
                .eq_ignore_ascii_case("application/json")
        })
        .unwrap_or(false);
    if !json {
        return Err(format!("{NOT_KIVALI} (/api/v1/login did not answer with JSON)"));
    }
    let wire: LoginWire = serde_json::from_slice(body)
        .map_err(|_| format!("{NOT_KIVALI} (/api/v1/login answered with other JSON)"))?;
    Ok(LoginInfo {
        org_name: wire.org.name.trim().to_string(),
        auth_ready: wire.auth_ready,
        dev_mode: wire.dev_mode,
    })
}

/// The most of a `/api/v1/login` answer the check reads. The real answer
/// is under 200 bytes.
pub const LOGIN_BODY_CAP: usize = 64 * 1024;

/// Appends `chunk` unless that would pass [`LOGIN_BODY_CAP`].
fn push_capped(body: &mut Vec<u8>, chunk: &[u8]) -> bool {
    if body.len() + chunk.len() > LOGIN_BODY_CAP {
        return false;
    }
    body.extend_from_slice(chunk);
    true
}

/// Fetches and checks `<origin>/api/v1/login`. Redirects are not
/// followed: a Kivali server answers this route itself, without a
/// session. The 10-second limit matches the supervisor's release check;
/// without one, a host that accepts and never answers would leave the
/// Connect form waiting forever.
pub async fn fetch_login(origin: &Url) -> Result<LoginInfo, String> {
    let endpoint = origin
        .join("api/v1/login")
        .map_err(|e| format!("bad address: {e}"))?;
    let client = crate::http_client(reqwest::redirect::Policy::none())?;
    let resp = client
        .get(endpoint)
        .header("accept", "application/json")
        .send()
        .await
        .map_err(|e| {
            if e.is_timeout() {
                "The org didn't answer within 10 seconds.".to_string()
            } else if e.is_connect() {
                "Couldn't reach that address.".to_string()
            } else {
                format!("Couldn't reach that address ({e}).")
            }
        })?;
    let status = resp.status().as_u16();
    let ctype = resp
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .map(str::to_string);
    let mut resp = resp;
    let mut body = Vec::new();
    while let Some(chunk) = resp.chunk().await.map_err(|e| format!("Reading the answer failed: {e}"))? {
        if !push_capped(&mut body, &chunk) {
            return Err("That address answers, but not as a Kivali org (its /api/v1/login answer is too large).".into());
        }
    }
    parse_login_response(status, ctype.as_deref(), &body)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_host_gets_https_and_loses_path() {
        let u = normalize_org_url("  kivali.example.com/agents/x?y=1#z ").unwrap();
        assert_eq!(origin_string(&u), "https://kivali.example.com");
    }

    #[test]
    fn keeps_port() {
        let u = normalize_org_url("https://kivali.example.com:8443/").unwrap();
        assert_eq!(origin_string(&u), "https://kivali.example.com:8443");
    }

    #[test]
    fn http_only_on_loopback() {
        for ok in ["http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "http://127.1.2.3"] {
            assert!(normalize_org_url(ok).is_ok(), "{ok}");
        }
        let err = normalize_org_url("http://kivali.example.com").unwrap_err();
        assert!(err.contains("https"), "{err}");
        assert!(normalize_org_url("http://192.168.1.10:8080").is_err());
    }

    #[test]
    fn rejects_other_schemes_credentials_and_empty() {
        assert!(normalize_org_url("").is_err());
        assert!(normalize_org_url("   ").is_err());
        assert!(normalize_org_url("ftp://kivali.example.com").is_err());
        assert!(normalize_org_url("file:///etc/passwd").is_err());
        assert!(normalize_org_url("javascript://alert(1)").is_err());
        assert!(normalize_org_url("https://user:pw@kivali.example.com").is_err());
        assert!(normalize_org_url("https://").is_err());
    }

    #[test]
    fn body_cap() {
        let mut b = Vec::new();
        assert!(push_capped(&mut b, &vec![0u8; LOGIN_BODY_CAP - 1]));
        assert!(push_capped(&mut b, &[0u8]));
        assert!(!push_capped(&mut b, &[0u8]));
        assert_eq!(b.len(), LOGIN_BODY_CAP);
    }

    #[test]
    fn login_response_accepted() {
        let body = br#"{"org":{"name":" Plainsong ","has_logo":true},"auth_ready":true,"dev_mode":false}"#;
        let info = parse_login_response(200, Some("application/json; charset=utf-8"), body).unwrap();
        assert_eq!(
            info,
            LoginInfo { org_name: "Plainsong".into(), auth_ready: true, dev_mode: false }
        );
    }

    #[test]
    fn login_response_unnamed_org() {
        let body = br#"{"org":{"name":"","has_logo":false},"auth_ready":false}"#;
        let info = parse_login_response(200, Some("application/json"), body).unwrap();
        assert_eq!(info.org_name, "");
        assert!(!info.dev_mode);
    }

    #[test]
    fn login_response_rejected() {
        let good = br#"{"org":{"name":"x"},"auth_ready":true}"#;
        assert!(parse_login_response(404, Some("application/json"), good).is_err());
        assert!(parse_login_response(302, Some("application/json"), good).is_err());
        assert!(parse_login_response(200, Some("text/html"), good).is_err());
        assert!(parse_login_response(200, None, good).is_err());
        assert!(parse_login_response(200, Some("application/json"), b"<html>").is_err());
        assert!(parse_login_response(200, Some("application/json"), br#"{"status":"ok"}"#).is_err());
        assert!(parse_login_response(200, Some("application/json"), br#"[1,2]"#).is_err());
    }
}
