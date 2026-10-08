# Configuration

This page lists the settings an operator can change: the Helm chart's values and the server's environment variables. Most of what you change day to day is in the app instead (**Org** in Kivali, **Settings** in Kivali Desktop); this page is for what is set when a team is installed.

Kivali Desktop sets all of this for you. You need this page when you [self-host](self-hosting.md).

## Helm chart values

The chart's `values.yaml` is commented key by key. These are the values most installs touch.

| Value | Default | What it does |
| --- | --- | --- |
| `externalURL` | empty | The https address other computers reach the team at, such as `https://kivali.example.com`. Needed for Google sign-in at any address other than loopback. Sets `KIVALI_EXTERNAL_URL`. |
| `models.agent` | empty | The default model for agents and new hires. Empty uses Kivali's default, Claude Opus 5.5. Sets `AGENT_MODEL`. |
| `models.summary` | empty | The model for small housekeeping calls (file and chat summaries). Empty uses Claude Haiku 4.5. Sets `SUMMARY_MODEL`. |
| `image.repository`, `image.tag` | `kivali`, the chart's version | The server image, also used by agent pods. The sandbox image `kivali-dev-shell` is taken from the same path and tag. |
| `egress.image.repository`, `egress.image.tag` | `kivali-egress-proxy`, the chart's version | The egress proxy image. |
| `service.type`, `service.nodePort` | `NodePort`, `30080` | How the server is exposed. |
| `persistence.size` | `10Gi` | The data volume. Everything durable lives here. |
| `persistence.storageClassName` | empty | Empty uses the cluster's default StorageClass. |
| `persistence.keepOnUninstall` | `true` | Keep the data volume when the chart is uninstalled. |
| `server.resources` | requests 200m, 1Gi; limits 1000m, 12Gi | Server resources. Agents run in their own pods, outside this limit. |
| `networkPolicy.enabled` | `true` | Install the agent-pod and server-ingress policies. See [Self-hosting](self-hosting.md#network-policy-and-egress). |
| `networkPolicy.podCIDR` | `10.42.0.0/16` | Your cluster's pod network, excluded from the server's ingress. The default is k3s's. |
| `networkPolicy.serverIngress.enabled` | `true` | The server-ingress policy alone. |
| `networkPolicy.serverIngress.extraFrom` | `[]` | Extra peers allowed to reach the server, such as an in-cluster ingress controller. |
| `oauth.redirectURL` | empty | A fixed sign-in callback URL. Use only where the address is the same for everyone, such as a loopback address; otherwise use the Secret's `oauth-redirect-url`. |
| `extraEnv` | `[]` | Extra environment variables for the server, as Kubernetes `EnvVar` objects. Use it for the variables below that have no chart value. |
| `secrets.create` | `false` | Create `kivali-secrets` from `secrets.*` values. Single-user installs only. |
| `kivaliEnv` | `prod` | Keep `prod`. `prod` refuses `devMode`. Sets `KIVALI_ENV`. |

### The `kivali-secrets` Secret

| Key | What it is |
| --- | --- |
| `owner-emails` | Your Google account: the only account that can sign in. Several of your own accounts go comma-separated. Required. |
| `session-key` | 32 random bytes, hex or base64url. Signs sessions. Replacing it signs you out. |
| `google-oauth-client-id`, `google-oauth-client-secret`, `oauth-redirect-url` | Optional, together. Your own Google OAuth client. |

Install-specific values live in the Secret rather than in values files, so an upgrade with a shared values file can never overwrite them.

## Environment variables

The chart sets these from its values and the Secret. They are listed for reference and for setting through `extraEnv`.

### Sign-in and access

| Variable | Default | What it does |
| --- | --- | --- |
| `OWNER_EMAILS` | empty | The Google accounts that can sign in, comma-separated. The server refuses to start without it. |
| `SESSION_KEY` | none | Signs session cookies. At least 32 bytes. |
| `KIVALI_EXTERNAL_URL` | empty | The https origin other computers use, `https://host[:port]`. Anything else refuses to start. |
| `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET` | empty | Your own Google OAuth client. Set both, or neither. |
| `OAUTH_REDIRECT_URL` | empty | The callback URL registered on your own client, ending `/auth/callback`. Required with your own client. |
| `OAUTH_AUTH_URL`, `OAUTH_TOKEN_URL`, `OAUTH_USERINFO_URL`, `OAUTH_JWKS_URL`, `OAUTH_ISSUER` | Google's | Another OAuth 2.0 provider's endpoints, used with your own client. The userinfo document must carry `email` and `verified_email`. |
| `OAUTH_PUBLIC_CLIENT_ID`, `OAUTH_RELAY_URL` | Kivali's | Replace the public client and its sign-in relay. Only for running your own relay. |
| `KIVALI_COOKIE_SUFFIX` | empty | Up to 24 of `a-z`, `0-9`, `_`, `-`. Gives this server's cookies their own names, so several servers on one host keep separate sign-ins. |

### Claude and models

| Variable | Default | What it does |
| --- | --- | --- |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` | unset | Refused: the server does not start with any of them set. Model calls bill Claude Code's own sign-in on the data volume ([Connect Claude](self-hosting.md#connect-claude)), which agent pods share. |
| `AGENT_MODEL` | `claude-opus-5-5` | The default model for agents and new hires. Must be one Kivali knows. |
| `SUMMARY_MODEL` | `claude-haiku-4-5` | The model for housekeeping summaries. |

The models Kivali offers are `claude-haiku-4-5`, `claude-sonnet-5`, `claude-opus-5-5` and `claude-fable-5-1`.

### Team defaults

Stored the first time the server starts with them, and never over a value already set in the app.

| Variable | What it does |
| --- | --- |
| `KIVALI_SEED_ORG_NAME` | The team's name, at most 64 characters. |
| `KIVALI_TEAM_KIND` | `work` or `personal`. A personal team's setup treats files as optional, and its handbook has an "About you" section in place of "The company". Unset behaves as `work`. |
| `KIVALI_OWNER_NAME` | What agents call you, such as "Jane" or "CEO". At most 40 characters on one line, without `:`, `` ` `` or `>`. Change it later in **Org**, **Organization**. |

### Server

| Variable | Default | What it does |
| --- | --- | --- |
| `ADDR` | `:8080` | Listen address. |
| `DATA_DIR` | `/data` | Where everything durable is kept. |
| `KIVALI_ENV` | `dev` (the chart sets `prod`) | `prod` refuses `DEV_MODE`. |
| `KIVALI_MEMSTATS_INTERVAL` | `60s` | How often the server logs memory statistics. |

### Set by the chart

The chart sets these to match the objects it creates. Leave them alone unless you are not using the chart.

| Variable | What it does |
| --- | --- |
| `AGENTPOD_NAMESPACE` | Namespace for agent pods and their volumes. |
| `AGENTPOD_IMAGE` | Image agent pods run. |
| `AGENTPOD_UDS_DIR` | Directory on the node holding the socket agent pods use to reach the server. |
| `EGRESS_ALLOWLIST_SYNC_PATH` | Where the server writes the host list for the egress proxy. |
| `EGRESS_PROXY_URL` | The proxy agent pods send their outbound traffic through. |

### Development only

Never set these on a team other people can reach.

| Variable | What it does |
| --- | --- |
| `DEV_MODE` | Turns sign-in off entirely; every request is treated as `DEV_USER`. Refused with `KIVALI_ENV=prod` and on any non-loopback address. The chart's `devMode` sets it. |
| `DEV_USER` | The email every request is attributed to in dev mode. Default `dev@localhost`. |
| `DEV_MODE_ALLOW_NONLOOPBACK` | Lets dev mode listen on every interface. |

## Settings kept in the app

These live with the team's data, are changed in the app, and are included in backups:

| Setting | Where |
| --- | --- |
| Name, logo, what agents call you | **Org**, **Organization** |
| Handbook | **Org**, **Handbook** |
| Project files, skills | **Org**, **Project files** and **Skills** |
| Hosts agents can reach | **Org**, **Network** |
| Auto-release | **Home**, the Queue |
| Each agent's model and effort | The agent's chat |
