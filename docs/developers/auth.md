# Sign-in: protocol reference

How sign-in works inside the server, the relay and Kivali Desktop. For
setting it up as an operator, see [../sign-in.md](../sign-in.md).

Kivali signs people in with Google. There is one flow, the OAuth 2.0
authorization code flow with PKCE, and two clients it can run through:

- **The public client Kivali ships with.** The default. Its id is baked
  into the server; its secret lives only in a relay on kivali.ai, which
  every install shares. Nothing to register, nothing to configure. The
  relay's callback must be registered on that client as its authorized
  redirect URI, and Google's token endpoint requires the client's
  secret even with PKCE, which is why the relay performs the exchange.
  `OAUTH_PUBLIC_CLIENT_ID` can stand in for the baked id to test another
  relay.
- **Your own Google OAuth client.** Set `GOOGLE_OAUTH_CLIENT_ID`,
  `GOOGLE_OAUTH_CLIENT_SECRET` and `OAUTH_REDIRECT_URL` and the server
  talks to Google directly with your secret. This is also how to point
  sign-in at another OAuth 2.0 server.

Whoever signs in must be on the allowlist, which holds the team's
owner (`OWNER_EMAILS`). Sign-in proves who someone is; the allowlist
decides whether they may enter.

## How a sign-in works

1. The browser opens `/auth/login?next=/somewhere`. The server makes a
   nonce and a PKCE verifier, works out this install's own callback URL
   (below), and sets one HMAC-signed, HttpOnly cookie holding the nonce,
   the verifier, the callback URL, the `next` path and a 10-minute
   expiry. The browser is sent to Google's consent page with the client
   id, the S256 challenge of the verifier, the nonce, and a `state`.
2. Google sends the browser back to the registered redirect URI with a
   `code` and the same `state`. For an own client that is this install's
   `/auth/callback`; for the public client it is the relay's callback,
   which bounces the browser on to the install named in `state`, with
   Google's code replaced by a sealed code bound to that install's
   callback URL (below).
3. `/auth/callback` clears the cookie, checks that `state` equals what
   the cookie predicts, and exchanges the code: directly with Google
   plus the secret for an own client, or at the relay's token route
   with no secret, plus `return_url` (the callback URL from the cookie),
   for the public one. Both exchanges carry the PKCE verifier, so a code
   is worthless to anyone who did not start that sign-in.
4. The person is named by Google's signed `id_token`: its signature is
   checked against Google's published keys, its issuer, audience (this
   client id) and expiry are checked, and its `nonce` must equal the
   one this sign-in sent. Its verified email is then checked against
   the allowlist and a signed session cookie is set. The public client
   accepts nothing else: the relay returns only the id_token, and even
   a relay that lied could not forge Google's signature. An own client,
   whose exchange went to the provider
   directly, falls back to the userinfo document when the provider
   issued no id_token.

**The callback URL.** With an own client it is `OAUTH_REDIRECT_URL`,
which must be the redirect URI registered on that client; sign-in
started on any other host first hops to that host so the cookie lands
where the callback runs. With the public client the registered redirect
URI is the relay's, so this install's callback need not be registered
anywhere: when `OAUTH_REDIRECT_URL` is unset it is derived from the
request, but only for an allowed host:

- loopback: `127.0.0.1`, `localhost` or `[::1]`, any port, with the
  connection's own scheme (`http` unless the server terminates TLS
  itself). This needs no configuration, which is how Kivali Desktop's
  team windows (`http://127.0.0.1:<port>`) and desktop sign-in work;
- the external address, `KIVALI_EXTERNAL_URL` (an https origin such as
  `https://team.example.com`; the server refuses to boot on
  anything else), whose callback is always
  `https://<that host>/auth/callback`. Kivali Desktop sets it from "Let
  other computers connect"; a server reached at a public address sets
  it (chart value `externalURL`).

The host is the first `X-Forwarded-Host` value if that is allowed, else
`Host` if that is allowed. Anything else, a sign-in started at an
in-cluster address or any name not listed, is refused at `/auth/login`
(403, no cookie) with a message naming the host and the setting:
"Sign-in isn't available at <host>: this team only accepts sign-ins at
127.0.0.1, localhost and its external address (KIVALI_EXTERNAL_URL, or
"Let other computers connect" in Kivali Desktop's Other devices
settings)." `X-Forwarded-Proto` is never read. The callback refuses a
state cookie whose callback the install would not derive now (for
example, the external address was removed after the sign-in started).

`OAUTH_REDIRECT_URL` with the public client still pins the callback, for
every request, loopback included, with the same hop to its host as an
own client: it is configuration, not a request's word, so it is safe
whatever headers arrive. Its path must be exactly `/auth/callback`, the
only path the relay returns to, and the server refuses any other at
boot. `KIVALI_EXTERNAL_URL` is the usual way to name a public address;
it keeps loopback sign-in working in the same install.

**Why the host is allowlisted.** `state` names the callback, and the
relay seals the code for whatever callback `state` names; an attacker
can rewrite `state` in a Google link to their own server and get the
owner to open it. The attacker then holds the victim's sealed
code, bound to the attacker's address. Redeeming it at the install needs
a state cookie whose callback is that same address, with the
attacker's own nonce and verifier. If the install derived its callback
from whatever host a request named, the attacker could get exactly that
cookie by starting a sign-in with `Host` or `X-Forwarded-Host` set to
their server: directly, if the install is exposed, or through any proxy
that appends to a client's `X-Forwarded-Host` rather than replacing it.
The allowlist closes that: a derived callback is either loopback, which
sends the victim's browser to the victim's own machine, or the
install's configured external address, so a sealed code reaches no one
but the person signing in. A forged loopback `Host` gains nothing for
the same reason, and neither does the cookie `Secure` rule it unlocks
(below): the cookie is the sender's own.

**State.** `state` is `<nonce>.<base64url(callback URL)>`. The nonce
contains no dot (it is base64url; a desktop sign-in's is
`desktop~<port>~<token>~<base64url>`, tildes and all), so the relay
splits on the first one. The install compares the whole value with what its cookie
predicts, so a rewritten return address is rejected even with the right
nonce.

## The public client and the relay

In short: with the public client, every Google sign-in, Kivali
Desktop's included, passes through the hosted relay at
`https://kivali.ai/oauth/google` (`DefaultRelayURL` in
`internal/auth/public_client.go`). The relay holds the public client's
secret, sees the sign-in's code exchange and the email Google returns,
and hands back only Google's signed id_token. It never sees the
install's session cookie or session key, never issues access or refresh
tokens, and takes no part in anything after sign-in. An install that
should not depend on it uses its own Google client ("Using your own
Google client"; no relay is involved), another OAuth server ("Another
OAuth server"), or points `OAUTH_RELAY_URL` and `OAUTH_PUBLIC_CLIENT_ID`
at a relay and client of its own that implement the two routes below.

The relay is small and holds exactly one secret. Its base is
`OAUTH_RELAY_URL` (the server is built with a default) and it serves two
routes:

**`GET <base>/callback`**, the redirect URI registered on the public
client. Google arrives with `code` and `state` (or `error` and `state`).
The relay decodes the return URL from `state` and answers a 302 to that
URL with the query string passed through, except `code`: Google's code
never reaches the browser. The relay replaces it with a **sealed code**,
`k1.<base64url>`: AES-GCM over Google's code, the return URL it is
sending the browser to, and a 10-minute expiry, under a key only the
relay holds (`RELAY_SEAL_KEY`). The install treats the code as opaque
and passes it back unchanged (the desktop loopback bounce included). The
relay requires the return URL's path to be exactly `/auth/callback`
with no query, and its scheme to be `https`, except `http` for
`127.0.0.1`, `localhost` and `[::1]`; it rate-limits and stores nothing.

**`POST <base>/token`**, the code exchange. The body is the standard
`application/x-www-form-urlencoded` token request a client would send
to Google (`grant_type=authorization_code`, `code`, `code_verifier`,
`client_id`, `redirect_uri`) plus `return_url`, the install's own
callback URL (the one its state cookie recorded and `state` named). The
relay accepts only `grant_type=authorization_code` (so no refresh
tokens are ever minted through it), requires `client_id` to be the
public client's, `redirect_uri` to be its own callback, no
`client_secret`, and `return_url`; it opens the sealed code and redeems
it only if it has not expired and was sealed for that same `return_url`
(compared as parsed URLs), otherwise answering `400 invalid_grant`. It
then adds `client_secret`, forwards Google's own code to Google's token
endpoint, and returns Google's error verbatim or, on success, only
`{"id_token", "token_type", "expires_in"}`: no access or refresh token
leaves the relay. Google enforces PKCE, so a code can only be redeemed
by whoever holds its verifier. The install signs in on the id_token
alone, checked against Google's keys, never on the relay's word. It
calls this route server to server.

What the binding buys: a code lured to another address by a rewritten
`state` is sealed for that address, so it is redeemable only by an
exchange naming that address, which an install does only with a state
cookie it issued for it. The install never issues one for an address
someone else controls (see "The callback URL").

**What the relay records.** One log line per sign-in: the install it
was for (the return URL's origin) and the Google account that signed in
(the email in the id_token Google returned), with the outcome. Codes
and tokens are never logged or stored. This is Kivali's view of who
signs in through its public client; an own client reports to no one.

**What a shared public client cannot prevent.** Anyone can build a
Google consent link that names Kivali's public client and their own
return URL, and a person who consents hands their Google identity to
that party under Kivali's name. That is inherent to every public OAuth
client; the relay's path rule only narrows where the browser can be
sent. Such a flow's sealed code is bound to that party's address, and
the relay hands out only an id_token for it.

What the install needs to reach: the relay's host for the token
exchange, and `accounts.google.com` plus `www.googleapis.com` for the
consent redirect and Google's signing keys. These are the server pod's
own outbound connections, not agent traffic through the egress proxy.

## Desktop sign-in

Kivali Desktop shows an org in its own webview, which has its own cookie
jar, and Google does not allow its consent page inside an embedded
webview. So the webview starts and finishes the sign-in, and only the
provider's leg runs in the system browser, which brings Google's answer
back to the app through a loopback listener (the native-app pattern of
RFC 8252, section 7.3):

1. When the org's page navigates to `/auth/login`, the app first binds
   a listener on `127.0.0.1` at a free port and makes a random token
   (32 bytes, base64url), then lets the webview go there with
   `client=desktop`, `return_port=<port>` and `return_token=<token>`
   added. Both are checked for shape (a port from 1 to 65535; 43
   base64url characters). Step 1 above happens in the webview: the
   state cookie is set there, and the nonce, and so the `state` the
   provider echoes back, is `desktop~<port>~<token>~<random>`, a shape
   no random nonce can take. The own-client hop to the configured host
   carries the three parameters along. The webview is then sent to
   Google's consent page, which the app opens in the system browser
   instead.
2. The browser signs in and comes back to this install's
   `/auth/callback` with the provider's answer, `code` and `state`, or
   `error` and `state`, but no cookie for this sign-in. Reading the
   listener out of `state`, the callback verifies and stores nothing:
   it redirects the browser to
   `http://127.0.0.1:<port>/signin/<token>?<the answer, unchanged>`.
   The browser gets no session. A cookie the browser holds from an
   ordinary sign-in of its own, abandoned earlier, does not change this;
   only a cookie that itself belongs to a desktop sign-in, which only a
   webview sets, makes the callback run the checks below.
3. The app's listener answers that one request, if its token matches,
   with a page saying the tab can be closed, and the app navigates its
   webview to `<origin>/auth/callback?<the same answer>`. That is step 3
   above, with the cookie: the state, the nonce, the PKCE exchange and
   the id_token are checked exactly as for any sign-in, and the session
   cookie lands in the webview. One outcome differs: an account the
   allowlist does not admit is redirected (302) to `/auth/not-invited`
   rather than answered with a plain-text 403, because the webview
   reports where it is sent but not a status code. That route needs no
   session and serves the same sentence ("This Google account is not
   this team's owner. Only the owner can sign in.") as a minimal page
   with status 403. The email is not in the URL:
   the redirect also sets `kivali_denied` (below), holding the refused
   email for two minutes, HttpOnly and scoped to `/auth/not-invited`,
   which the app reads from its webview's cookie store once that page
   has loaded, so its "sam@example.com can’t sign in to Plainsong" can
   name the account. An ordinary browser sign-in still gets the 403 that
   names the account, and no such cookie.

Nothing on that loopback hop is worth stealing. With the public client
the code is the relay's sealed one, passed through unchanged and
redeemable only for the install's own callback. It cannot be
exchanged without the PKCE verifier, which is in the webview's HttpOnly
cookie, and a code and state fed to a webview whose cookie did not start
that very sign-in fail the state check, so one person cannot sign
another's app in as themselves. The listener is bound by the app alone,
takes one request for its token, and goes away with the sign-in. The
server keeps no state for any of this.

A browser that opens `/auth/login` without `client=desktop` gets the
ordinary flow and its cookie; that is also how the app's "open in
browser" works.

## Desktop handoff

Desktop setup has the owner sign in with Google once, to learn the email
it installs as `OWNER_EMAILS`. So that the team's window does not then
ask for a second sign-in, the app opens it at `GET
/auth/handoff?t=<token>`, which trades a one-time token for the session
an ordinary sign-in would leave.

- **The token** is `<payload>.<sig>`: payload is base64url (unpadded) of
  the JSON `{"email", "exp" (unix seconds), "nonce" (16 random bytes,
  base64url)}`, sig is base64url of HMAC-SHA256 over
  `"kivali-handoff\n" + payload`, keyed with the same `SESSION_KEY`
  bytes that sign the session cookie. The prefix keeps a handoff
  signature from ever matching a session cookie's or a state cookie's,
  and the other way round. The org's supervisor mints it
  (`POST /v1/handoff`, [supervisor.md](supervisor.md)) from the session key and the
  first owner in the install values on the data disk, valid for
  2 minutes; the server refuses an `exp` in the past or more than
  5 minutes ahead.
- **Loopback only:** the route answers 404 unless the host the request
  addressed is loopback (`localhost`, `127.0.0.1`, `[::1]`), the same
  test that drops `Secure` from the cookies.
- **Single use:** the server remembers each redeemed nonce in memory
  until its token expires; a replay fails. A restart forgets them,
  which a 2-minute token barely outlives.
- The email must pass the allowlist exactly as at the OAuth callback.
  On success the session cookie is set as the callback sets it (with
  `KIVALI_COOKIE_SUFFIX`), and the answer is a 302 to `/`. Any failure
  is a 302 to `/` with no session, where the ordinary sign-in follows,
  and one fixed log line ("auth handoff: refused a handoff token");
  the token is never logged.

The route exists whenever sign-in is enforced (not under `DEV_MODE`),
with or without a Google client.

## Using your own Google client

Register a client in Google Cloud, add this deployment's public URL plus
`/auth/callback` as an authorized redirect URI, and set:

| Variable | Value |
| --- | --- |
| `GOOGLE_OAUTH_CLIENT_ID` | the client id |
| `GOOGLE_OAUTH_CLIENT_SECRET` | the client secret |
| `OAUTH_REDIRECT_URL` | the registered redirect URI, exactly |

In Kubernetes these are the `google-oauth-client-id`,
`google-oauth-client-secret` and `oauth-redirect-url` keys of
`kivali-secrets`. Half an own client is refused at boot: a secret
without an id would pair your secret with the public client, and an id
without its secret would run as a public client under the wrong id.

## Another OAuth server

Any OAuth 2.0 server with an authorization-code flow works in place of
Google. Point `OAUTH_AUTH_URL`, `OAUTH_TOKEN_URL` and
`OAUTH_USERINFO_URL` at its endpoints, and `OAUTH_JWKS_URL` and
`OAUTH_ISSUER` at its signing keys and issuer if it issues id_tokens;
otherwise its userinfo document must carry `email` and
`verified_email`. Usually that means your own client on that server, so
set the three variables above too; with the public client an explicit
`OAUTH_TOKEN_URL` replaces the relay's token route. The requested scope
is `openid email profile`.

## Cookies

- `kivali_oauth_state`: the sign-in in progress, signed with
  `SESSION_KEY`, HttpOnly, `SameSite=Lax`, ten minutes, cleared by the
  callback on every outcome.
- `kivali_session`: the signed session, 24 hours.
- `kivali_denied`: only after a desktop sign-in the allowlist refused
  (Desktop sign-in, step 3): the refused email, unsigned, HttpOnly,
  `SameSite=Lax`, `Path=/auth/not-invited`, two minutes. Nothing on the
  server reads it; Kivali Desktop does, to name the account.

All carry `Secure` unless the host is loopback, so a deployment
reached over plain HTTP on any other hostname cannot sign in; put it
behind TLS.

Browsers key cookies by host, not port, so two servers on one host
(several teams on one Mac, each at `http://127.0.0.1:<port>`) would
overwrite each other's cookies and sign each other's people out.
`KIVALI_COOKIE_SUFFIX` keeps them apart: with it set, the cookies are
`kivali_oauth_state_<suffix>`, `kivali_session_<suffix>` and
`kivali_denied_<suffix>`, set, read and cleared under those names only. Unset, the names are the plain
ones above. Changing the suffix signs everyone out once.

## Dev mode

`DEV_MODE=true` disables sign-in entirely and attributes every request
to `DEV_USER`. The server refuses that off loopback unless
`DEV_MODE_ALLOW_NONLOOPBACK` is also set.
