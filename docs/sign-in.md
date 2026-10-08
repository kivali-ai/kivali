# Sign-in

This page explains who can sign in to a Kivali team, how Google sign-in works out of the box, how to use your own OAuth client instead, and how to sign in from other computers.

## How sign-in works

Kivali signs you in with Google. Kivali reads only the email address Google confirms, and lets you in when it is the team's owner account. A session lasts 24 hours; after that, sign in again.

## Who can sign in

Only the team's owner. In Kivali Desktop, that is the Google account you signed in with when you created the team. On a self-hosted team, it is `owner-emails` in the `kivali-secrets` Secret; if you use more than one of your own Google accounts, list them all there.

Any other account sees "This Google account is not this team's owner. Only the owner can sign in." and gets no further.

## Google sign-in out of the box

Kivali ships with its own public Google sign-in client, so a new team needs no Google setup. The sign-in exchange passes through a small relay at `kivali.ai`, which holds the client's secret. The relay hands back only Google's signed identity token, which your server checks against Google's own keys, so the relay cannot sign anyone in as someone else.

The public client accepts sign-ins at two kinds of address:

- **Loopback**: `127.0.0.1`, `localhost` or `[::1]`, at any port. This is how Kivali Desktop's windows and **Open in browser** work.
- **The team's external address**: an https address you configure. In Kivali Desktop, that is **Other devices**; on a self-hosted team, it is the chart value `externalURL`.

A sign-in started at any other address is refused with a message naming the setting to change.

## Signing in from other computers

**A team in Kivali Desktop.** In **Settings**, open the team, then **Other devices**, and turn on **Let other computers connect to** *team*. Give it an https address the other computer can reach. Kivali does not set up that address for you; one way is `tailscale serve` pointing at the team's local address. On the other computer, choose **File**, **Connect to a team…** in Kivali Desktop, or open the address in a browser, and sign in with the owner's Google account. See [Kivali Desktop](desktop-app.md#open-a-team-from-other-computers).

**A self-hosted team.** Set `externalURL` to the https address you use. See [Self-hosting](self-hosting.md).

In Kivali Desktop, Google's page opens in your usual browser, and the app picks up the result when you finish. If you sign in with an account that is not the team's owner, the app says only the owner can sign in and offers **Use a different account**.

## Use your own Google OAuth client

You can sign in through a Google OAuth client you own instead of Kivali's. Your server then talks to Google directly and no relay is involved.

1. In Google Cloud, create an OAuth client of type **Web application**.
2. Add your team's public address plus `/auth/callback` as an authorized redirect URI, for example `https://kivali.example.com/auth/callback`. If a tunnel or reverse proxy fronts the team, use its public address.
3. Add three keys to the `kivali-secrets` Secret:

   | Key | Value |
   | --- | --- |
   | `google-oauth-client-id` | The client id. |
   | `google-oauth-client-secret` | The client secret. |
   | `oauth-redirect-url` | The redirect URI from step 2, exactly. |

4. Restart the server.

Set all three or none; the server refuses to start with half a client. With your own client, a sign-in started at another address first moves to the redirect URI's address, so the sign-in cookie lands where the callback runs.

## Another OAuth provider

Your own client can point at another OAuth 2.0 provider instead of Google, with `OAUTH_AUTH_URL`, `OAUTH_TOKEN_URL`, `OAUTH_USERINFO_URL`, and where its identity tokens are verified, `OAUTH_JWKS_URL` and `OAUTH_ISSUER`. The provider's userinfo document must carry `email` and `verified_email`. See [Configuration](configuration.md#sign-in-and-access).

## Several teams on one computer

Browsers share cookies across ports on the same host. Kivali Desktop gives each of its teams its own cookie names, so you can be signed in to several teams on `127.0.0.1` at once. A self-hosted server can do the same with `KIVALI_COOKIE_SUFFIX`.
