package auth

// DefaultPublicClientID is the Google OAuth client Kivali ships with. It
// is a public client: the browser and every Kivali install see the id,
// and only the relay at DefaultRelayURL holds the matching secret. A
// deployment that sets GOOGLE_OAUTH_CLIENT_ID and
// GOOGLE_OAUTH_CLIENT_SECRET uses its own client instead; see
// docs/developers/auth.md.
//
// The id is public by nature (it appears in every consent URL); the
// matching secret is held by the relay and never by a Kivali install.
// Google issues a secret for every client type, and its token endpoint
// requires it even with PKCE, which is exactly why the relay exists.
// An empty value would make config.Validate refuse to boot without
// GOOGLE_OAUTH_CLIENT_ID, so a build cannot ship half-wired.
const DefaultPublicClientID = "508969299515-ol3l2a67ola8hfm2loru9ne8hksl8kfq.apps.googleusercontent.com"

// DefaultRelayURL is the base of the sign-in relay for the public
// client. Its two routes are the only places the public client's secret
// is used: <base>/callback is the redirect URI registered on the client
// and bounces the browser back to the install named in state, and
// <base>/token adds the secret to a token exchange. docs/developers/auth.md gives
// the contract.
const DefaultRelayURL = "https://kivali.ai/oauth/google"
