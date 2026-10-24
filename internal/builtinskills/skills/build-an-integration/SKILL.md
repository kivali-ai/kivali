---
name: build-an-integration
description: How to build or fix a Kivali integration bundle so it works the first time the owner installs it. How to choose the way it connects (a pasted token, a sign-in with the owner's app or with a client Kivali registers itself, or a setup flow of the bundle's own), the bundle and its manifest, what the server meets where it runs, every request Kivali sends it and what each expects back, two starting bundles, a stub, and a script that calls the server the way Kivali does.
when_to_use: Before writing or changing an integration bundle's server or manifest, and again before integration_publish. Skip it for using an installed integration; its actions are tools like any other.
version: 2.0.0
---

# Build an integration

An integration is a bundle: a directory holding `manifest.json` and a
stdio MCP server in Python. The owner approves a version of it,
installs it, which makes its connection to the service, and gives each
agent access; agents then call its actions as tools of their own.

`integration_validate` checks the manifest, starts the server with no
connection and asks for its list of actions and, for a setup bundle,
its requirements. It never calls an action and never runs a setup
step. What only a call or a step shows reaches the owner after they
approve, as a failure an agent or the install reports, and each fix is
a new version and another approval. So start from a template, run
every action and every step with `try.py` against a stub, and publish
once.

## The steps

1. Read the service's API documentation with WebFetch: the calls the
   actions need, how a connection is made (a token to paste, OAuth, an
   app made through the service's own API), what a token may be scoped
   to, every host a call or its redirects reach, and the error answers.
2. Choose how it connects ([How it connects](#how-it-connects)).
3. Copy the template for that way into the bundle's directory, which
   lives under `/files/artifacts/private/integrations/<name>/`:
   `template` for a token or a sign-in, `template-setup` for a setup
   flow:

   ```
   mkdir -p /files/artifacts/private/integrations
   cp -R /files/skills/build-an-integration/template /files/artifacts/private/integrations/<name>
   ```

4. Write the server and the manifest there.
5. Write a stub of the service in `/files/background/`, a small local
   server answering as the documentation says, and run against it with
   `try.py` ([Try it as Kivali calls it](#try-it-as-kivali-calls-it)):
   every action, as the owner and as agents with the access an owner
   would give; for a setup bundle also every flow, each answer the
   service refuses, a flow left partway and started again, the
   requirements met and not met, each Fix and the uninstall.
6. Run `integration_validate` on the directory until it says the bundle
   would be accepted.
7. Publish with `integration_publish` and the version notes.
8. Wait for the owner's answer in your chat. After approving, the owner
   installs the integration, gives agents access, runs Test connection
   and tells you what it found.

Keep what is not the bundle outside its directory: every file in it is
stored with the version and counted on the install screen.

## How it connects

`kivali.connection.method` is one of four. Take the first row that
works for the service: each one after it is more for the owner to read
on the install screen and more for you to get right.

| The service | `kivali.connection` | The owner | The server |
| --- | --- | --- | --- |
| hands out a key or token to paste (an API key, a personal or private app token) | `"method": "token"` | pastes it, with any other field the manifest declares | reads it from its environment |
| offers OAuth (device codes, or a sign-in page and back), and the owner can register an app with it | `"method": "device"` or `"browser"`, `"client": "operator"` | registers an app, pastes its client id and secret, signs in | asks `kivali/credential` for the token on every call |
| publishes OAuth metadata and takes dynamic client registration, as MCP servers do | `"method": "browser"`, `"client": "registered"`, with `discover` and `hosts` | signs in; registers nothing, pastes nothing | as for a sign-in |
| has no token to paste and no standard sign-in, or a far better install is possible by driving its own API (an app made from a key, from a manifest, through the endpoints its own CLI uses) | `"method": "setup"`, with `setup` | takes the steps the bundle's flow gives: Forms, pages on the service, waits | runs the flow (`kivali/setup`), reports health (`kivali/requirements`), undoes the install (`kivali/uninstall`), and reads what setup kept from its environment |

- **`token` is the common case.** Most services hand out a token; the
  owner copies it once.
- **A sign-in** is for a service whose token is not something an owner
  can copy, or expires within hours. Kivali runs all of OAuth: the
  server never sees the client secret or the refresh token
  ([A sign-in connection](#a-sign-in-connection)).
- **A registered client** is a sign-in with no app to register. The
  service must publish where it signs in and take registrations; read
  its metadata first ([A registered client](#a-registered-client)).
- **`setup`** is the bundle's own flow, and the most code: choose it
  when it saves the owner real work (a pasted key in place of an app
  built by hand) or there is no other way. A setup can make an app and
  then hand over to a sign-in with it (`then`). The core of Kivali
  knows standards (a pasted token, OAuth, dynamic registration) and
  never a service; anything specific to the service is the bundle's
  setup ([A setup connection](#a-setup-connection)).

Kivali does not run a remote MCP server as an integration. A service
whose API is an MCP server is a bundle whose server forwards to it,
usually with a registered client
([Wrapping a remote MCP server](#wrapping-a-remote-mcp-server)).

## The templates

`/files/skills/build-an-integration/` holds:

- `template/`: a token bundle for an imaginary notes service at
  `api.notes.example`, standard library only. Its `server/main.py`
  already does what is easy to get wrong:
  - It keeps the output for the protocol and sends everything else,
    `print` included, to the log.
  - It answers each action on a thread of its own and writes each
    message whole, with the request's id as it came.
  - It reaches the service with `urllib.request`, through the proxy,
    with a timeout shorter than Kivali's.
  - It lists each agent the actions its access allows, names that
    agent's notebooks in the actions' input schemas, and refuses an
    argument the access does not cover.
  - It starts and lists its actions with the connection empty, and
    answers a call with "not connected".
- `template-setup/`: a setup bundle for an imaginary service, Deals,
  whose app is made through its API from a one-time key the owner
  pastes ([The example: template-setup](#the-example-template-setup)).
  It has the same plumbing, and answers `kivali/setup`,
  `kivali/requirements` and `kivali/uninstall`. Its `README.md` says
  what each part does.
- `scripts/deals_stub.py`: a stub of Deals for `try.py`, to run
  template-setup's flow before changing it.
- `scripts/try.py`: runs a bundle's server as the pod does and calls it
  as Kivali does.

Change `SERVICE`, `VERSION`, the environment names, the actions and
`TOOLS`, and every field of `manifest.json`, the name, the version, the
hosts and the access form included. Keep the plumbing.

## The bundle

- `manifest.json` at the root, the server's code wherever the manifest's
  `entry_point` says, and optionally `README.md` and `icon.png`.
- At most 5000 files and 64 MiB. No symlinks, special files, absolute
  paths or `..`; `__pycache__/` folders are left out.
- A dependency is vendored: `pip install --target vendor <package>` in
  the bundle's directory, with the server putting `vendor/` on
  `sys.path` before it imports it. Never a virtual environment: its
  links are symlinks, which the check refuses. `pip` reaches a package
  index only when the owner allows its host for your commands. The
  standard library is enough for most services.

## The manifest

MCPB's manifest with a `kivali` section; each template's is a complete
one. Fields Kivali does not read are kept as written.

| Field | What it holds, and the check's rule |
| --- | --- |
| `manifest_version` | `"0.3"`. |
| `name` | 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit. Agents get its actions as the name with hyphens as underscores, an underscore and the action (`my-crm`'s `find` is `my_crm_find`), so the name's first word cannot be one of Kivali's own: file, artifact, agent, get, search, list, read, graph, assignment, publish, run, share, subagent, web, submit, propose, integration, kivali. |
| `version` | `MAJOR.MINOR.PATCH`, with an optional `-pre-release`, no leading zeros. |
| `display_name`, `description` | What the owner sees it called and what it does. |
| `author` | `{"name": "Kivali Developer (agent)"}`. |
| `server` | `"type": "python"`, `entry_point` (a file in the bundle), and `mcp_config`: `command` `python3`, `args` naming the entry point, and `env`. |
| `server.mcp_config.env` | The server's environment. `${user_config.<key>}` becomes that connection value: a `user_config` field's, or for a setup connection a key setup keeps. No other `${...}` is allowed. A plain value, such as the API's address, is passed as written. |
| `user_config` | The connection form of a token or sign-in connection, in this order; empty for a setup connection. Each key is lowercase letters, digits and underscores, starting with a letter, and not one Kivali keeps a sign-in under (`oauth_access_token`, `oauth_refresh_token`, or any other `oauth_` key of a registered client); each field has `type` (`string`, `number` or `boolean`), `title` and `description`, `"sensitive": true` for a secret, and `"required": true` when the connection needs it. |
| `tools` | Every action: a `name` (lowercase letters, digits and underscores, starting with a letter) and a `description` the owner reads on the install screen. |
| `kivali.connection` | How it connects: `token`, `device`, `browser` or `setup` ([How it connects](#how-it-connects)). |
| `kivali.setup_notes` | Markdown the owner reads above the connection form and the Install button: what to do on the service's side first, such as registering an app and the scopes to tick. Optional. |
| `kivali.hosts` | Every host the server reaches, for its actions and for its setup; it reaches those and no other ([The network](#the-network)). Lowercase, no scheme, port or path, and never `*`. |
| `kivali.packages` | Optional: the Debian packages the pod installs before the server starts, such as `["gh"]` ([A command-line tool](#a-command-line-tool)). Each is a package's exact name as `apt-get install` takes it: lowercase letters, digits, `+`, `-` and `.`, with no version or release, listed once. |
| `kivali.access` | The access form ([The access form](#the-access-form)). Without it, access is all or nothing. |
| `kivali.verify` | The action Test connection calls. |

`kivali.connection.multiple` is refused: a bundle's server takes one
connection.

### The access form

`kivali.access.schema` is a JSON Schema of one object, held to what a
flat form shows: `type: object`, `properties`, `required`, and
optionally `title`, `description` and `additionalProperties: false`.
Each property has `type`, `title` and `description`, and is one of:

| Property | The owner fills in |
| --- | --- |
| `string` | text |
| `string` with `enum` | one choice of the enum |
| `boolean` | a switch |
| `boolean` with `"x-kivali-danger": true` | a switch shown as a risky choice, its description in view: for one the owner should turn on with care |
| `array` with `items: {"type": "string"}` | a list, free text |
| `array` with `items.enum` | choices of the enum |

Nothing else: no numbers, no nested objects, no `oneOf` and the like,
no constraints such as `minItems` or `pattern`.
`kivali.access.choices` maps a text or list field to the action that
lists its options through the connection, in place of a fixed enum.

## A token connection

```json
"connection": { "method": "token" },
"hosts": ["api.notes.example"]
```

The connection is the `user_config` fields; the owner pastes them on
Install. `mcp_config.env` gives each to the server
(`"NOTES_TOKEN": "${user_config.token}"`), which reads them once at
start: a changed connection restarts it. The field's `description` and
`kivali.setup_notes` say where the token comes from and the narrowest
scopes to give it. The template is a complete token bundle.

## A sign-in connection

The owner registers an app with the service, pastes its client id and
secret, and approves Kivali on the service's page. Kivali runs the
whole OAuth exchange; the server never does OAuth, and never sees the
client secret or the refresh token.

```json
"connection": {
  "method": "device",
  "device_endpoint": "https://github.com/login/device/code",
  "token_endpoint": "https://github.com/login/oauth/access_token",
  "revocation_endpoint": null,
  "scopes": [],
  "client": "operator",
  "credential_keys": { "access_token": "GH_TOKEN" }
}
```

```json
"connection": {
  "method": "browser",
  "authorization_endpoint": "https://login.crm.example/oauth/authorize",
  "token_endpoint": "https://login.crm.example/oauth/token",
  "revocation_endpoint": "https://login.crm.example/oauth/revoke",
  "scopes": ["contacts.read", "contacts.write"],
  "client": "operator",
  "credential_keys": { "access_token": "CRM_TOKEN" }
}
```

- `method`: `device`, a code the owner enters on the service's page;
  it needs `device_endpoint` and `token_endpoint`. Use it where the
  service offers the device flow (GitHub does; on GitHub the app must
  have it turned on).
- `method`: `browser`, the service's sign-in page and back to Kivali
  (the authorization code grant); it needs `authorization_endpoint`
  and `token_endpoint`. Use it where the service offers no device flow
  (HubSpot, Slack, Notion). A manifest may give both ways' endpoints;
  `method` is the one the page offers first.
- `pkce` (default true) sends a PKCE challenge on browser sign-in; set
  it to false only for a service that refuses one.
  `extra_authorization_params` are added to the service's sign-in page
  address as they are (HubSpot's `optional_scope`); they may not name
  `client_id`, `redirect_uri`, `response_type`, `scope`, `state`,
  `code_challenge` or `code_challenge_method`, which Kivali sets.
- For browser sign-in the owner registers Kivali's redirect address
  with the app: `<Kivali's address>/connect/callback`, shown on the
  integration's page. The service must accept that address (many take
  only `https`, or `http` on `localhost`).
- Every endpoint is an `https://` address from the service's OAuth
  documentation. `revocation_endpoint` is optional: with one, signing
  out, deleting the connection and uninstalling revoke its tokens.
- `scopes`: what the sign-in asks for; the owner sees them on the
  install screen and the integration's page. Leave them empty where
  the app's own settings decide what it may do (a GitHub App).
- `client`: `operator` (or absent), the app the owner registers.
- `credential_keys.access_token`: the name the access token is given
  under, capital letters, digits and underscores (`GH_TOKEN`).
  Required. `mcp_config.env` may not set a key of that name.
- Kivali adds `client_id` and `client_secret` to the connection form
  itself: `user_config` declares neither. Declare only what else the
  server needs.
- `kivali.hosts` lists the hosts the server calls, not the service's
  sign-in hosts: Kivali talks to those.
- `kivali.setup_notes` tells the owner how to register the app: where
  in the service's settings, which settings to turn on (GitHub: device
  flow), which scopes to tick. For browser sign-in, write
  `{{redirect_url}}` where the owner enters the app's callback (or
  redirect) URL, and `{{external_redirect_url}}` beside it for the
  team's address for other devices: the page shows Kivali's own
  addresses in their place.

The token changes while the server runs: Kivali renews it shortly
before it expires, without restarting the server. So the server reads
it from `kivali/credential` on every call, as `fields[<name>]` (or
`env[<name>]`), never from its environment at start and never kept
from one call to the next. The answer is error -32002 while the
connection needs the owner to sign in (never signed in, signed out, or
the service no longer accepts the sign-in) and -32003 when the service
did not answer Kivali's renewal: refuse the call with words that say
so, the first with "ask the owner to sign in again", the second with
"try again later". At start, before the owner signs in, the server
starts with empty values like any other.

## A registered client

A `browser` sign-in where Kivali registers the team as a client with
the service's authorization server (OAuth dynamic client registration,
RFC 7591), so the owner registers no app and pastes nothing. MCP
servers that follow MCP's authorization offer it.

```json
"connection": {
  "method": "browser",
  "client": "registered",
  "discover": "https://mcp.crm.example/mcp",
  "hosts": ["mcp.crm.example", "login.crm.example"],
  "scopes": ["crm.read"],
  "credential_keys": { "access_token": "CRM_TOKEN" }
},
"hosts": ["mcp.crm.example"]
```

- `discover`: the `https://` address Kivali reads the service's
  metadata from: an MCP server's own address, or the authorization
  server's. No endpoint is given: Kivali reads them there, and the
  check refuses any.
- `hosts` (inside `connection`): every host the endpoints Kivali finds
  may be on. Kivali refuses metadata naming any other, and the install
  screen lists them under "Signs in at". Fill it from the service's
  documents, read with WebFetch before you write the manifest:
  1. the protected resource's metadata,
     `https://<discover's host>/.well-known/oauth-protected-resource<discover's path>`
     (then without the path): its `authorization_servers`;
  2. each authorization server's metadata,
     `<issuer>/.well-known/oauth-authorization-server` (or
     `/.well-known/openid-configuration`): the hosts of its
     `authorization_endpoint`, `token_endpoint`, `revocation_endpoint`
     and `registration_endpoint`.

  List each host once, lowercase, no scheme, port or wildcard. The
  discover address's own host is allowed without listing.
- The service's metadata must offer a `registration_endpoint`, the
  authorization code grant and PKCE with `S256`; without them the
  sign-in fails with "Kivali could not find where to sign in to
  <integration>: <why>" or "Kivali could not register itself with
  <integration>: ...". Check before choosing this way. Leave `pkce` out.
- `scopes` go in the registration and the sign-in; `credential_keys`
  and `kivali/credential` are as for any sign-in. The connection form
  has no client id or secret: `user_config` declares only what else the
  server needs, usually nothing. `kivali.hosts` lists the hosts the
  server calls, here the MCP server's.
- The owner sees "Kivali registers itself with the service; there is
  nothing to paste." on the install screen, chooses Install, and
  continues to the service's sign-in. The notes need no
  `{{redirect_url}}`; they say only what the account needs on the
  service's side (a plan that includes its API, an admin's approval).

### Wrapping a remote MCP server

The server lists the actions the manifest declares from a table of its
own and forwards each call to the service's MCP server with the
connection's token, holding each agent to its access as any bundle
does. It cannot fetch the remote list at start: it starts with no
connection, and `integration_validate` starts it with no network.

```python
REMOTE = os.environ.get("CRM_MCP", "https://mcp.crm.example/mcp")
ANSWER_CHARS = 20000  # what an agent reads; the rest is cut

def token():
    """The access token, fresh, from kivali/credential: Kivali renews it."""
    resp = ask_kivali("kivali/credential", {})  # a request to Kivali; the reading loop hands back its answer
    err = resp.get("error")
    if err and err.get("code") == -32002:
        raise Refusal("The CRM connection needs the owner to sign in again; ask the owner.")
    if err and err.get("code") == -32003:
        raise Refusal("The CRM did not answer when Kivali renewed the sign-in; try again later.")
    if err:
        raise Refusal("The CRM integration is not installed yet; the owner installs it on its page in Kivali.")
    return resp["result"]["fields"]["CRM_TOKEN"]

def remote(method, params, tok, session=None):
    """One JSON-RPC message to the remote server (Streamable HTTP): a
    request's answer, or None for a notification."""
    msg = {"jsonrpc": "2.0", "method": method, "params": params}
    if not method.startswith("notifications/"):
        msg["id"] = 1
    headers = {"Authorization": "Bearer " + tok, "Content-Type": "application/json",
               "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": PROTOCOL_VERSION}
    if session:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(REMOTE, data=json.dumps(msg).encode(), headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=45) as resp:
            raw = resp.read().decode("utf-8", "replace")
            sid = resp.headers.get("Mcp-Session-Id") or session
    except urllib.error.HTTPError as e:
        if e.code == 401:
            raise Refusal("The CRM no longer takes the sign-in; ask the owner to sign in again.")
        raise Refusal("The CRM's server answered %d." % e.code)
    if "id" not in msg:
        return None, sid
    if raw.lstrip().startswith("{"):
        return json.loads(raw), sid
    # An event stream: the answer is the data line that carries the id.
    for line in raw.splitlines():
        if line.startswith("data:"):
            got = json.loads(line[5:])
            if got.get("id") == 1:
                return got, sid
    raise Refusal("The CRM's server gave no answer.")

def forward(name, args):
    tok = token()
    _, sid = remote("initialize", {"protocolVersion": PROTOCOL_VERSION, "capabilities": {},
                                   "clientInfo": {"name": "kivali-crm", "version": VERSION}}, tok)
    remote("notifications/initialized", {}, tok, sid)
    got, _ = remote("tools/call", {"name": name, "arguments": args}, tok, sid)
    if "error" in got:
        return refusal("The CRM refused the call: %s" % got["error"].get("message", ""))
    res = got.get("result") or {}
    texts = [c.get("text", "") for c in res.get("content") or [] if c.get("type") == "text"]
    return {"content": [{"type": "text", "text": "\n".join(texts)[:ANSWER_CHARS]}], "isError": bool(res.get("isError"))}
```

- Declare in `tools` each remote tool agents should have, by the
  remote's own name or one of yours, with an input schema in your
  table copied from the service's documentation. A remote tool the
  manifest does not declare never reaches an agent.
- Hold agents to the access form before forwarding: a tool that spends
  the account's credits or writes behind a switch, the arguments
  checked as for any action.
- Pass on text only, cut to what an agent needs. Never put the token in
  an answer or the log; Kivali replaces it with `[secret]` all the
  same.
- `kivali.hosts` holds the remote server's host. `verify` is an action
  that makes one cheap read and names the account.

## A setup connection

The bundle's own flow makes the connection: a run of steps the owner
takes, which Kivali draws and runs, with the server deciding each next
step. Kivali calls `kivali/setup` with the owner's last answer, the
server answers with the next step, and so on until a step is Done:

- **Form**: fields for the owner to fill in, in Kivali's page.
- **Go**: the owner's browser goes to a page on the service, by a link
  or by a form posted there, and usually comes back.
- **Wait**: the owner does something on the service, or the service
  does something, while Kivali checks every few seconds.
- **Done**: the connection is made.

Each step may **keep** values in the connection's Secret, which become
the connection the actions use, and carry **state** to the next call.
After the install, `kivali/requirements` is the integration's health:
rows met or not, each with a Fix that runs another flow. On Uninstall,
`kivali/uninstall` undoes the install on the service.

### The declarations

`kivali.connection.setup` declares everything the flow may do. The
install screen lists it under **Setup** before anything runs, and
Kivali holds every step to it: a step outside it fails the flow.

```json
"connection": {
  "method": "setup",
  "setup": {
    "summary": "Builds a private app in your Deals account with the scopes agents need, installs it there, and keeps its token. The setup key you paste is used once and kept nowhere.",
    "hosts": ["app.deals.example"],
    "fields": {
      "setup_key": {
        "title": "Setup key",
        "words": "A one-time setup key, from Settings, Developer, Setup keys, in your Deals account.",
        "secret": true,
        "setup_only": true
      },
      "token": {
        "title": "App token",
        "words": "The app's token, from its page in Deals, under Show token.",
        "secret": true
      }
    },
    "keeps": {
      "token": "The app's token, which every call sends",
      "app_id": "The app's id in Deals, to rebuild or remove it"
    }
  }
},
"hosts": ["api.deals.example"]
```

| Declaration | What Kivali holds the flow to | The check refuses |
| --- | --- | --- |
| `summary` | The owner reads it on the install screen and the page: what setup does on the service, in one or two sentences. | `kivali.connection.setup.summary is missing; ...` |
| `hosts` | Every host a Go may send the browser to and a Wait may name a page on: `https://` only, no user info, the host exactly as listed (no port, nothing under it). Empty for a flow that stays in Kivali. The API the server calls is not here but in `kivali.hosts`. | a host that is not lowercase, `*`, a pattern, with a scheme, port or path, or listed twice |
| `fields` | Every field a Form may ask for, in order. `words` (required): what to enter and where it comes from. `title`: the label (the key in words otherwise). `secret`: entered as a password, never shown again. `setup_only`: lives in the flow's memory alone, never kept: a key the flow trades for something else. | missing words; a key that is not lowercase letters, digits and underscores, or is one Kivali keeps a sign-in under; `secret` or `setup_only` that is not true or false; a `setup_only` field that is also kept |
| `keeps` | Every key a step may keep, each with words saying what it is. Kept values are the connection: all secret, given to the server by `${user_config.<key>}` in `mcp_config.env` and in `kivali/credential`'s `fields`. | missing words; a key as for fields |
| `then` | `browser` or `device`: setup ends by handing over to Kivali's sign-in with the app it made ([Sketch: an app made by API, then a sign-in](#sketch-an-app-made-by-api-then-a-sign-in)). | another value; `then` without `client_id` and `client_secret` in `keeps`; endpoints without `then` |
| `user_config` | Empty: the connection's values are what setup keeps. | any field |
| `client` | Not given: the app is the one setup makes. | any value |

The words Kivali shows when a step breaks a declaration, which the
owner reads as why the setup did not finish, and `try.py` as a
`problem:` line:

- "<Integration>'s setup tried to send your browser to <host>, which
  its manifest does not declare"
- "<Integration>'s setup named a page on <host>, which its manifest
  does not declare"
- "<Integration>'s setup asked for <key>, which its manifest does not
  declare"; "<Integration>'s setup asked for a form with no fields"
- "<Integration>'s setup tried to keep <key>, which its manifest does
  not declare"
- "setup gave no next step"; "setup gave more than one next step"
- "<Integration>'s setup answered with a step Kivali cannot read: ..."
  (a key none of the shapes below has, or a value of another type)
- "<Integration>'s setup named an address longer than 4096
  characters"; "... gave more to keep and carry than Kivali holds for
  a step (64 KiB)"; "... gave a form to post larger than Kivali sends
  (64 KiB)"

### A flow, call by call

Kivali starts a flow when the owner chooses Install (flow `setup`), or
a requirement's Fix (the flow its row names). It calls `kivali/setup`
first with an empty answer, then after each step with what the step
gave back, one call at a time, until a step is Done or a call answers
an error:

```json
{"jsonrpc": "2.0", "id": 7, "method": "kivali/setup", "params": {
  "flow": "setup",
  "answer": {"state": {}, "form": {}, "return": {}, "checked": false, "went": false, "leg": 1,
             "callback": "https://team.example/connect/callback?state=4f0c9e2a7b1d4c3e8a6f5b2d1c0e9f8a"},
  "_meta": {"progressToken": "8c1d6b0e2f4a"}}}
```

- `flow`: `setup` to install, or a Fix's flow name. A Fix of `setup`
  installs again over the connection there is, with its values in the
  environment.
- `answer.state`: the last step's `state`, as it was; `{}` at first.
- `answer.form`: what the owner entered in the last step's Form, every
  field it asked for, trimmed, one left blank as `""`.
- `answer.return`: the query the service sent the browser back with
  after a Go, each key's values as a list (`{"code": ["x1"], "state":
  ["4f0c..."]}`). Untrusted ([Coming back](#coming-back)).
- `answer.checked`: true for a Wait's check.
- `answer.went`: true after a Go with `no_return` was sent.
- `answer.leg`: which call of the flow this is, from 1.
- `answer.callback`: where a Go's service may send the browser back.
- Every object is there, `{}` when empty.

The answer is one step, and optionally what to keep and to carry:

```json
{"jsonrpc": "2.0", "id": 7, "result": {"step": {
  "form": {"words": "Paste a setup key from your Deals account.", "fields": ["setup_key"]},
  "state": {"at": "key"}}}}
```

| `step` holds | Shape |
| --- | --- |
| exactly one of | `"form": {"words", "fields": [keys]}`; `"go": {"words", "url", "post", "no_return", "stateless"}` (`post` an object of strings, or lists of strings for a key posted more than once; the rest optional); `"wait": {"words", "url", "code"}` (`url` and `code` optional); `"done": true` |
| optionally | `"keep": {key: value}` and `"state": {key: value}`, strings |

An error is a JSON-RPC error, and its message is what the owner reads
under "Setup did not finish", so write it in their words: `{"code":
-32000, "message": "Deals did not create the app: the account has no
developer access. Ask a Deals admin for it, then try again."}`. The
owner then has Try again. A server that answers -32601 (it does not
implement `kivali/setup`) fails every install with "<Integration>'s
server does not answer kivali/setup; its author must add it".

A server not yet running when the flow starts (the install switched it
on) is waited for two minutes at most, after which the flow fails with
"<Integration> is not running, so its setup cannot go on; its log says
why. Start the setup again once it runs".

### Form

For what only the owner knows or can get: a key from the service's
settings, an account or workspace name, a token the service shows once.

- The page shows the step's words, then "<Integration> asks for" and
  each field: its title, its words as the hint (with "Used during
  setup and never kept." for a setup-only one), a password box for a
  secret one. Continue and Cancel.
- Ask only declared fields, and no more than the next call needs.
- Check the answer at once, against the service where you can, and
  answer the same Form again with words saying what was wrong ("Deals
  did not take that setup key; each one works once. Paste a new
  one.") rather than an error: the owner stays on the step.
- A setup-only value reaches the next call alone. To use it in a later
  call, carry it in `state`, where Kivali keeps it in memory for this
  flow and never writes it.

### Go

For a page on the service the owner acts on: approving an app,
creating one from a manifest, installing it on an account, choosing
what it may reach.

- The page shows the words and "Continue to <host>". Kivali sends the
  browser to `url`: a link, or with `post` a small page that posts the
  values to `url` and submits itself (a Continue button for a browser
  that runs no scripts). Use `post` where the service takes a form
  (a manifest, a large or secret value); never put a secret in a
  link's query.
- With a return (the default), the service sends the browser back to
  `answer.callback`, the owner lands on the integration's page, and
  the next call has `answer.return`.
- With `"no_return": true`, for a settings page that sends the
  browser nowhere: Kivali opens it in a new tab and calls setup again
  at once with `"went": true`. Follow it with a Wait that checks the
  service for what the owner was to do there.
- With `"stateless": true`, for a service that drops the state from
  the address it sends the browser back to (below).
- In Kivali Desktop the system browser goes, signed in to the team, and
  comes back there; nothing differs for the server.

### Coming back

`answer.callback` is `<Kivali's address>/connect/callback?state=<s>`,
with a fresh `<s>` for each call. The state is how Kivali finds the
flow when the browser comes back, so it must come back in the query:

- Where the service sends the browser back to an address you give it
  (a manifest's `redirect_url`, a `return_to` parameter), give
  `answer.callback` whole.
- Where the service takes a fixed redirect address and a separate
  `state` parameter (OAuth's shape), give it the callback without its
  query as the address and `<s>` as `state`.
- Kivali takes any state it gave the flow while the flow lives, so an
  address fixed in an earlier call still comes back. The return is
  given to the step on screen: a link replayed from an earlier leg
  arrives as the current Go's `answer.return`.
- Without a state the browser is matched by Kivali's cookie alone,
  which goes with any page the owner opens. So Kivali takes a return
  with no state only for a Go marked `stateless`, and fails any other
  with "the service sent the browser back without its state; start it
  again". Mark `stateless` only where the service drops the state, and
  check what comes back all the more.
- `answer.return` is what anyone could put in an address: never keep
  it as it came. Trade a code with the service before keeping anything
  (a code that the service refuses is a refusal, not a connection);
  look an id up with the service rather than trusting it; and read a
  return that holds `error` (`error=access_denied`) as the owner
  turning it down, answering with the Go again or a Form and words that
  say so.
- The browser must come back to the browser that left, signed in to
  Kivali, or the flow fails with Kivali's own words; nothing reaches
  the server.

### Wait

For something that happens on the service, at the owner's hand or the
service's own pace: a token shown on a page, an app's approval by an
admin, a build that finishes after the call that started it.

- The page shows the words, a code to copy (`code`, for one the owner
  enters on the service's page) and a link to `url` (on a declared
  host), then "Waiting." and Cancel.
- While the owner's page is open, Kivali calls setup with `"checked":
  true` every three seconds, one call at a time. Nothing is checked
  while nobody watches: a check is a moment the owner is there, not a
  timer. Answer with one quick look at the service (a few seconds at
  most) and the same Wait while nothing has changed; the next step
  once it has.
- The same Wait, the same state and nothing kept is no change. A flow
  is forgotten ten minutes after its last change, so a Wait the owner
  does not finish in ten minutes ends the flow: use one for what takes
  minutes, and say in the words what the owner does.

### Done

The connection is made: for an install, the integration is installed
and its requirements are read. A Done may keep what the last call
found. With `then`, Kivali's sign-in takes the flow on with the app
setup kept.

### State and keep

- **`state`** is the flow's memory: where it is (`"at": "key"`), a
  setup-only value, a session or code to trade, what an earlier call
  learned. Each step's `state` replaces the last; a step with none
  carries nothing. It lives in Kivali's memory for this flow alone and
  is never written. The server keeps nothing of the flow itself: it
  may be restarted between calls (each Keep restarts it), and the next
  call may be of a flow started again.
- **`keep`** is the connection: the values the actions use, written to
  the connection's Secret before the step is shown, declared keys
  only. A kept empty value forgets that key. Kivali starts the server
  again with what was kept, so the next call, and every action, reads
  it from the environment (`${user_config.<key>}`) or
  `kivali/credential`'s `fields`. Keep what the actions and the
  requirements need and nothing more.
- An install keeps nothing unless it is Done: when it fails, the owner
  cancels it, or nobody takes its step in ten minutes, Kivali forgets
  the connection it made. A Fix that fails keeps what it kept.
- Every kept value, every secret or setup-only value the owner entered,
  every state value, each callback's state, and the values a return
  brings back under `code`, `state` or a key holding `token`, `secret`,
  `password` or `key`, each one six characters or longer, are replaced
  with `[secret]` wherever the owner reads the flow: its words, its
  progress, a Wait's page address and code, the error, the audit and
  the log. So a value the owner must read (a code to enter, an id in a
  page's address, the account name they typed) cannot also be carried
  in `state` or kept: point a Wait at a page whose address holds none
  (the app list, not the app), and name an account in words only from
  what the service says on this call.

### A flow left partway

Kivali calls nothing when the owner cancels a flow, starts another, or
leaves one to expire, and nothing when a call fails. What setup made on
the service before then stays there. So make each step safe to run
again: before creating an app, look for the one an earlier run made
(by the name setup gives it) and take it up; before installing, ask
whether it is installed. The owner who tries again then gets one app,
not two.

### Progress

A call that takes more than a moment says where it is. Send MCP's
progress notification with the request's `_meta.progressToken`, as it
came, while the call runs:

```json
{"jsonrpc": "2.0", "method": "notifications/progress",
 "params": {"progressToken": "8c1d6b0e2f4a", "progress": 2, "total": 4, "message": "Making the app"}}
```

The page shows `message`, and a progress bar of `progress` of `total`
where `total` is above 0: give real counts where there is a count (the
stages of a build, the pages of a list). One naming another token, or
sent after the call answered, is dropped. A call that waits on the
service more than once (build, poll, deploy) is fine inside its five
minutes, with a stage for each wait.

### Requirements

`kivali/requirements` is the installed integration's health. Kivali
asks for it when the owner's page, the list or Home is read, after its
own rows (Approved version, Connection, Sign-in, Connection test,
Running):

```json
{"jsonrpc": "2.0", "id": 9, "method": "kivali/requirements", "params": {}}
{"jsonrpc": "2.0", "id": 9, "result": {"requirements": [
  {"id": "token", "title": "App token", "words": "The app's token works, in Acme.", "met": true},
  {"id": "scopes", "title": "Scopes", "met": false, "fix": "rescope",
   "words": "This version needs deals.write, which the app was not built with. Paste a new setup key to rebuild it."}]}}
```

- Each row has `id`, `title`, `words` and `met`, and optionally `fix`;
  a row lacking one of the four makes the whole answer unreadable,
  shown as one unmet row "<Integration> could not check its
  requirements: ...". An id or fix is letters, digits, hyphens and
  underscores, 64 at most, and an id is not one of Kivali's
  (`version`, `connection`, `sign_in`, `connection_test`, `running`,
  `checks`) and given once; a row that breaks this is left out. The
  first 20 rows are kept. A title is cut to 80 characters and words to
  500.
- Answer within five seconds, or the page shows that unmet row: the
  owner's page waits on it. Make one request to the service at most,
  with a timeout of four seconds, and none with no connection: the
  smoke test asks with every value empty and no network. Kivali keeps
  the answer three seconds.
- What to check: the kept token works (and whose it is); what this
  version needs is there (its scopes, permissions, a webhook); the app
  is still installed. Each row a thing the owner can understand on its
  own.
- Words: met says what is so ("The app's token works, in Acme."); not
  met says what is wrong and what the Fix does ("Deals no longer takes
  the app's token. Set it up again to build a new app."). A row that
  cannot be checked now (the service did not answer) is not met, with
  no Fix, and says so.
- `fix` is the flow the row's Fix button runs: `setup` (install again,
  over the connection there is), `sign_in` (for a setup that hands over
  to a sign-in), or a flow of your own, which `kivali/setup` gets as
  `flow`. A Fix of your own runs only while a row names it, and only
  while the integration is installed, on and running ("<Integration>
  is not running, so its own flows cannot run; its log says why"). A
  Fix flow is like an install: steps, progress, keeps, Done. It works
  on the connection there is (its values are in the environment), asks
  only for what the mend needs, and a Fix that fails keeps what it
  kept.
- Unmet rows are in the owner's Needs you until they are met.

### Uninstall

```json
{"jsonrpc": "2.0", "id": 12, "method": "kivali/uninstall", "params": {}}
{"jsonrpc": "2.0", "id": 12, "result": {}}
```

On Uninstall, before Kivali forgets the connection, the server undoes
the install on the service: removes the app, the webhook, the
installation. Answer `{}` once done, within 60 seconds.

- An installation already gone is done: answer `{}`, so an uninstall
  tried again finishes. Kivali may ask again after an answer of `{}`,
  when it could not forget the connection the first time.
- A value it needs that is missing (the token is not in the
  environment): nothing can be undone from here; answer `{}`.
- An error is a JSON-RPC error, and the uninstall is refused with
  nothing changed: the owner reads "<Integration> could not undo its
  installation: <message>; nothing was changed". Refuse only for a
  failure that trying again may get past; for one it never will, the
  owner is stuck until they switch the integration off.
- A switched-off integration is uninstalled without the request, its
  install on the service left as it is. A switched-on one whose server
  is not running cannot be uninstalled ("<Integration> is not running,
  so it cannot undo its installation on the service; switch it off to
  uninstall it without that").

### Words the owner reads

Kivali shows every word setup gives as it is: the summary, each field's
title and words, each keep's words, each step's words, progress
messages, errors, requirement titles and words, Test connection's
answer. Write them as the page's own:

- Sentence case, short sentences, no exclamation marks, no emoji, no
  "please". Speak to the owner as "you".
- Say what to do and where, with the service's own names for its
  pages, as the service shows them: "Open Settings, Developer, Setup
  keys in Deals and create a key."
- Say what happens and what is kept: "Kivali uses it once to build the
  app, and keeps it nowhere."
- A failure says what went wrong and what the owner can do: "Deals did
  not take that setup key; each one works once. Paste a new one."
- No words of the bundle's insides: not MCP, tool, pod, server, flow,
  state or step.
- At most 200 characters for a step's words, a progress message and an
  error (Kivali cuts the rest), 64 for a Wait's code, 80 for a
  requirement's title and 500 for its words.

### Bounds and deadlines

| What | Bound |
| --- | --- |
| One `kivali/setup` call | 5 minutes, or the flow fails: "setup did not finish its step within 5 minutes; start it again" |
| A flow | forgotten 10 minutes after its last change; lost when Kivali restarts |
| A Wait's checks | every 3 seconds while the owner's page is open, one at a time |
| The server's start, for a flow | 2 minutes |
| `kivali/requirements` | 5 seconds; 20 rows |
| `kivali/uninstall` | 60 seconds |
| A step's words, progress message, error | 200 characters; a Wait's code 64 |
| A Go's or a Wait's address | 4096 characters |
| `keep` and `state` together, and a Go's `post` | 64 KiB each |

### The example: template-setup

template-setup is a complete setup bundle for Deals, whose app is made
through its API from a setup key the owner pastes. Read its
`server/main.py` beside this.

1. **Install.** The first call has an empty answer; the server answers
   a Form for `setup_key`, with `"state": {"at": "key"}`.
2. **The key.** The owner pastes it; the next call's `answer.form` is
   `{"setup_key": "sk-..."}` and its state `{"at": "key"}`. In this one
   call, `build` trades the key for a session (a refused key answers
   the Form again with words saying why), finds the app an earlier run
   made or creates one with this version's `SCOPES`, installs it and
   reads its token, sending a progress notification at each stage
   (`1 of 4` to `4 of 4`). It answers `{"done": true, "keep": {"token":
   ..., "app_id": ...}}`. Kivali keeps both and starts the server again
   with them in `DEALS_TOKEN` and `DEALS_APP_ID`.
3. **The fallback.** Where Deals does not hand the token back, the call
   keeps `app_id` alone and answers a Wait ("Open the Kivali app in
   Deals and choose Show token.") with the app list as its page, and
   carries the session in state: `{"at": "show", "app_id": ...,
   "session": ...}`. Each check (`"checked": true`) asks Deals whether
   the token has been shown and answers the same Wait until it has;
   then a Form for `token`, which the next call tries against Deals
   before keeping it and answering Done.
4. **Requirements.** With no token: one unmet row, Fix `setup`. With
   one: the token works (one request, four seconds), and the scopes
   are this version's, or an unmet row whose Fix is `rescope`.
5. **The Fix.** `rescope` is a flow of its own: a Form for a new key,
   then one call that updates the app's scopes and installs it again,
   with progress, and Done.
6. **Uninstall.** Removes the app in Deals with its own token; an app
   already gone, or a token Deals no longer takes, is done.

Run it against the stub before you change it:

```
cd /files/skills/build-an-integration
T=scripts/try.py
S="python3 scripts/deals_stub.py 8765"
A=DEALS_API=http://127.0.0.1:8765

# Install: the Form, the build with its stages, what is kept, Done.
python3 $T --stub "$S" --env $A template-setup setup --answer setup_key=sk-test-1

# A key Deals refuses, then one it takes.
python3 $T --stub "$S" --env $A template-setup setup --answer setup_key=wrong --answer setup_key=sk-test-1

# The token only on the app's page: the owner leaves at the Wait after
# one check, and starts again, which takes up the app the first run made.
python3 $T --stub "$S --token-on-page" --env $A --every 0 --checks 1 template-setup \
  setup --answer setup_key=sk-test-1 setup --answer setup_key=sk-test-2 --answer token=app-token-0001

# Installed, with an app built before a version needed its scope: the
# requirements, the Fix, the requirements again, an uninstall asked twice.
python3 $T --stub "$S --installed --scopes ''" --env $A --field token=app-token-0001 --field app_id=app_1 \
  template-setup requirements setup rescope --answer setup_key=sk-test-3 requirements uninstall uninstall

# Before anything is kept, as integration_validate asks.
python3 $T --env $A template-setup requirements
```

The stub's first lines say what it answers. Copy it to
`/files/background/` as a start for your service's.

### Sketch: an app made from a manifest

A service whose app is created from a manifest the owner's browser
posts, then installed on the owner's account (GitHub Apps are made
this way). The owner names the account, approves the app on the
service's page, installs it there, and copies nothing.

```json
"connection": {
  "method": "setup",
  "setup": {
    "summary": "Creates a private app on your Forge account that may read and write code and pull requests, and installs it on the repositories you choose.",
    "hosts": ["forge.example"],
    "fields": {
      "account": { "title": "Account", "words": "Your personal account's name, or an organization's, on Forge." }
    },
    "keeps": {
      "app_id": "The app's id",
      "private_key": "The app's private key, which signs a token for each call",
      "installation_id": "Where the app is installed"
    }
  }
},
"hosts": ["api.forge.example"]
```

1. Form `account`. Next call: answer a Go that posts the manifest:

   ```json
   {"step": {"go": {"words": "Forge shows the app Kivali makes. Choose Create to make it on your account.",
                    "url": "https://forge.example/organizations/acme/settings/apps/new",
                    "post": {"manifest": "{\"name\":\"Kivali for Acme\",\"redirect_url\":\"https://team.example/connect/callback?state=4f0c...\",\"public\":false,\"default_permissions\":{\"contents\":\"write\",\"pull_requests\":\"write\"}}"}},
             "state": {"at": "created"}}}
   ```

   The manifest's `redirect_url` is `answer.callback` whole.
2. The service sends the browser back with `?code=...&state=...`; the
   next call's `answer.return` is `{"code": ["..."], "state": ["..."]}`.
   Trade the code at once (the service takes it once, within the hour):
   `POST https://api.forge.example/app-manifests/<code>/conversions`,
   which answers the app's id, its name and its private key. A trade
   the service refuses answers the Form again with words saying to
   start over. Keep `app_id` and `private_key`; carry the app's name
   in state.
3. Answer a Go, `stateless`, to `https://forge.example/apps/<name>/installations/new`,
   where the owner picks the repositories; the service sends the
   browser to the app's setup address with `installation_id` and no
   state.
4. Ignore the returned `installation_id`: anyone could have written it.
   List the app's installations with a token signed by the kept key
   (a private app installs only on its own account, so there is one),
   keep that `installation_id`, and answer Done.

Requirements: the key signs a token; the app is installed (Fix: a Go,
`no_return`, to its installations page, then a Wait checked until it
is); its permissions are this version's. Uninstall: delete the
installation with the app's token; one already gone is done.

### Sketch: an app made by API, then a sign-in

A service whose OAuth app can be created through its API with an admin
key: setup makes the app, keeps its client id and secret, and hands
over to Kivali's browser sign-in, which then signs in, renews and
revokes as for any sign-in.

```json
"connection": {
  "method": "setup",
  "authorization_endpoint": "https://desk.example/oauth/authorize",
  "token_endpoint": "https://desk.example/oauth/token",
  "scopes": ["tickets.read", "tickets.write"],
  "credential_keys": { "access_token": "DESK_TOKEN" },
  "setup": {
    "summary": "Registers an app for Kivali in your Desk account with an admin key, then has you sign in with it. The admin key is used once and kept nowhere, and the app stays in Desk until an admin removes it.",
    "hosts": [],
    "fields": {
      "admin_key": { "title": "Admin key", "words": "An admin API key, from Admin, API, Keys, in Desk.", "secret": true, "setup_only": true }
    },
    "keeps": {
      "client_id": "The app's client id",
      "client_secret": "The app's client secret"
    },
    "then": "browser"
  }
},
"hosts": ["api.desk.example"]
```

1. Form `admin_key`.
2. One call: create the app with `POST https://api.desk.example/v1/oauth-apps`,
   its redirect address `answer.callback` without its query, its scopes
   the ones the manifest's sign-in asks for; answer `{"done": true, "keep": {"client_id": ...,
   "client_secret": ...}}`.
3. Kivali's browser sign-in takes the flow on: a Go to the
   authorization endpoint with the kept app, back to the callback, the
   code traded with PKCE, the tokens kept. The server reads the token
   from `kivali/credential` on every call
   ([A sign-in connection](#a-sign-in-connection)).

The app knows one redirect address, the one setup gave it; a sign-in
from another of the team's addresses is refused by the service. The
`sign_in` requirement (Kivali's own) asks the owner to sign in again
when the service stops taking the sign-in. Uninstall revokes the
tokens; removing the app needs an admin key, which setup keeps
nowhere, so `kivali/uninstall` answers `{}` and the summary says the
app stays in Desk until an admin removes it.

## Where the server runs

The integration's pod starts the server in the bundle's directory,
mounted read-only at `/bundle`.

- **Its environment** is PATH, `HOME=/tmp`, the manifest's
  `mcp_config.env` with the connection's values put in, and the proxy
  variables. Nothing else, not even LANG; Python reads and writes UTF-8
  all the same.
- **Its writable space** is `/tmp`, 256 MiB. It lasts as long as the
  pod: a restart of the server alone, for a changed connection or after
  it exits, keeps it, so nothing in it may depend on the connection.
  Memory is 1 GiB and CPU one core unless the install sets otherwise.
  What must last lives in the service.
- **Its Python** is the `python3` of your own shell, the same image,
  with the standard library and what the bundle carries in `vendor/`.
  Nothing else is installed but the Debian packages `kivali.packages`
  names. A package your shell installed with `pip`
  without `--target vendor` lives under your home; `integration_validate`
  and `try.py` start the server with a home of their own, so an import
  of it at start fails there too. A Debian package installed for your
  shell is everywhere in your shell, and the check and `try.py` find
  it; the pod does not have it. An import inside an action is found only
  by calling the action. A compiled package vendored with `pip install
  --target vendor` runs in the pod, since the image is the same; a
  pure-Python one is still the safer choice.
- **It starts with no connection**: before the owner installs it, and
  in `integration_validate`, every `${user_config.*}` is empty. It must
  still start, answer `initialize` and list every action (and a setup
  bundle answer `kivali/requirements`) within 20 seconds, which is all
  `integration_validate` gives it. Never exit for a missing value, and
  never reach the service at start; refuse the call instead, as the
  template's `api` does.
- **It reads the connection once.** A changed connection, a Keep among
  them, restarts the server with the new values. Put every field the
  server needs in `mcp_config.env` and the environment holds the
  connection as it is now; `kivali/credential` adds nothing to it. A
  sign-in connection's token is the exception: ask for it on every
  call.
- **It is stopped** by its input closing, and killed 5 seconds later
  if it has not exited. It is started again for a new version, a
  changed connection, and after three missed pings: Kivali pings every
  30 seconds and waits 20 for the answer.

## The network

The pod's network reaches one place, the egress proxy that
`HTTPS_PROXY` and `HTTP_PROXY` name, which lets it reach the hosts in
`kivali.hosts` and nothing else. There is no DNS and no direct
connection. A setup bundle's own calls during setup go the same way:
the API hosts its flow calls are in `kivali.hosts`, and the pages the
owner's browser opens are in `kivali.connection.setup.hosts`.

- **Use a client that reads those variables.** `urllib.request` does,
  and so do `requests` and `httpx` by default. `http.client`, sockets,
  and any client that ignores the variables reach nothing: they wait
  on a name lookup that never answers, then fail.
- **A host covers itself and every name under it**: `notes.example`
  covers `api.notes.example`. A `*` or `[...]` inside a name matches
  within one label. List the narrowest names that work; the owner reads
  each one before approving.
- **Every host a call touches is listed**: a sign-in host, an upload
  host, a regional API host, the host a redirect goes to. The proxy
  refuses any other with "<host> is not a host this integration
  reaches.", which urllib reports as `Tunnel connection failed: 403`.
- **Loopback, link-local, cloud metadata and the cluster's own
  addresses are refused**, whatever the host. A listed host on a
  private address, such as an intranet service, is reached: name such
  a host in the notes as what it is.
- **A connection field holding the account's own address** (a
  workspace name, an instance URL) works only where `kivali.hosts`
  covers it. List the service's domain, which covers every account's
  name under it, and check the field's value against it before calling.
- **Kivali waits 60 seconds for an action.** Time the request to the
  service out sooner, so the agent reads your words and not Kivali's.

## A command-line tool

When the service has a command-line tool that does the work, such as
GitHub's `gh`, the server can run it for each call instead of speaking
the service's API itself.

- **Declare its Debian package** in `kivali.packages`. The pod installs
  it from Debian's repositories (Debian 13, trixie) each time it
  starts, before the server runs; the server itself never runs as
  root. The owner reads the list on the install screen before
  approving. Starting takes longer while it installs, and the log says
  "Installing gh", then "Installed gh", or which package could not be
  installed and why, after which the integration stops and tries again
  when it restarts. Declare only what the actions run.
- **List the tool's hosts.** It reaches the network through the same
  proxy, held to `kivali.hosts`, so every host it calls is listed, and
  it must read `HTTPS_PROXY`, as Go and curl tools do.
- **Run it per call** with `subprocess.run`: the arguments a list,
  never a shell; `stdin=subprocess.DEVNULL`, or it reads Kivali's
  requests from the server's input; `capture_output=True`, or what it
  prints reaches the protocol and Kivali stops the server; a `timeout`
  shorter than Kivali's 60 seconds; and an `env` built for that call:
  PATH, HOME, the proxy variables, and the connection's values the tool
  reads (`GH_TOKEN` for `gh`), from the server's environment, which
  `mcp_config.env` fills, or from `kivali/credential`'s `fields`. Never
  write a value to disk: no login command, no config file, nothing in
  `/tmp`, which outlasts a changed connection.
- **Check each argument** an agent gives before it reaches the command
  line. To most tools a value starting with `-` is an option: refuse
  it, or put `--` before positional values where the tool takes it. A
  repository or project the access does not name is refused as for any
  action.
- **Answer from its output**: the parts of its standard output an agent
  needs, and, when it exits non-zero, an `isError` result with the end
  of its error output.
- **Your shell does not have it** unless the owner installed it for your
  shell, and `integration_validate` starts the server in your shell, so
  never run the tool at start. To call an action that runs it with
  `try.py`, put a stand-in script of the same name, answering as the
  tool would, first on `PATH`
  (`PATH=/files/background/bin:$PATH python3 $T ...`).

```python
import os, subprocess

PROXY_VARS = ("HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy")

def run_gh(args, timeout=45):
    env = {"PATH": os.environ["PATH"], "HOME": os.environ.get("HOME", "/tmp"),
           "GH_TOKEN": os.environ.get("GH_TOKEN", "")}
    env.update({k: os.environ[k] for k in PROXY_VARS if k in os.environ})
    return subprocess.run(["gh", *args], env=env, stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=timeout)
```

## The protocol, as Kivali holds it

- **The output carries JSON-RPC and nothing else**, one object per
  line. Any other line, from a `print`, a library's notice or a command
  the server runs, and Kivali stops the server there and then; so does
  a line over 16 MiB. The log says "the server wrote a line that is not
  JSON-RPC; Kivali stopped it". `integration_validate` sees only what
  the server writes at start: an action that prints is found by calling
  it.
- **The error output is the log.** Each line becomes a log line of
  level `stderr`, cut to 8 KiB. For a level of your own, send
  `kivali/log` as a notification:
  `{"jsonrpc":"2.0","method":"kivali/log","params":{"level":"warning","message":"..."}}`,
  level `info`, `warning`, `error` or `debug`.
- **Requests overlap.** Kivali sends each agent's call as it comes, a
  setup step or a requirements read beside them, and a ping every 30
  seconds. Answer `tools/call` and the three setup requests on a
  thread, write each line whole, and carry each request's `id` back
  exactly as it came. Kivali's ids are whole numbers and it matches
  them as numbers: an id written back as text is never matched, and
  the request fails at its deadline.
- **Answer** `initialize` with `protocolVersion` (the one asked for,
  `2025-06-18`), `"capabilities": {"tools": {}}` and `serverInfo`;
  `ping` with `{}`; `tools/list` and `tools/call` as below; for a
  bundle whose `kivali.connection.method` is `setup`, `kivali/setup`,
  `kivali/requirements` and `kivali/uninstall` as above; any other
  method with error -32601. Kivali sends the three setup requests to no
  other bundle, so the token template answers them -32601 with the
  rest; a setup bundle that answers one of them -32601 fails it, with
  "<Integration>'s server does not answer <method>; its author must add
  it".
- **Only `kivali/log`, `kivali/credential` and a setup step's progress
  reach Kivali.** `notifications/progress` naming the progress token of
  the `kivali/setup` request in flight is that step's progress; any
  other notification the server sends (`list_changed`, progress of
  anything else) is dropped, and any other request (sampling, roots,
  elicitation) is answered -32601. An integration cannot ask the agent
  or the owner anything during a call; a setup asks the owner only
  through its steps.
- **`kivali/credential`** answers the connection's values as they are
  now, `{"env": {...}, "fields": {...}}`, or error -32001 with no
  connection; a sign-in connection's also -32002 and -32003
  ([A sign-in connection](#a-sign-in-connection)). Its answer comes on
  the server's input, among Kivali's requests: the reading loop hands
  it to whatever waits for that id, and a reading loop that stops to
  wait for it never reads it.

## Who is calling, with what access

- **The owner's requests carry no `_meta.kivali`**: the `tools/list` at
  every start, Test connection, an access field's options, and the
  three setup requests. Their list holds every action the manifest
  declares, or `integration_validate` refuses the bundle.
- **An agent's requests carry** `_meta.kivali.agent`, its slug, and
  `_meta.kivali.access`, its access as the owner's form filled it in.
  Kivali decides whether an agent may use the integration at all; the
  server holds it to its access.
- **A field can be absent.** The form always sends a switch, true or
  false, and leaves out a text, choice or list field the owner left
  empty. Access set before a version added a field does not hold it.
  Read every field with the default that grants least: a missing
  switch is off, a missing list is nothing. If empty is to mean
  everything, say so in the field's description, which the owner reads
  on the form, and make the server do exactly that.
- **A changed form changes agents' access.** When a version is
  approved, access holding a field its form no longer has, or a value
  its fixed options no longer offer, or lacking a field it now
  requires, grants nothing until the owner sets it again. Every agent's
  access holds every switch, so removing one does this to all of them.
  Say so in the version notes.
- **Options the integration lists are not checked again.** For a field
  whose options come from `kivali.access.choices`, Kivali checks only
  that the value is text or a list of text, so an option the service no
  longer has stays in an agent's access. Refuse it at call time, as the
  template's `notebook_arg` does.
- **An agent's list is the actions its access allows.** Kivali refuses
  a call to an action that is not on it before the call reaches the
  server. Kivali keeps each agent's list until its access, the
  connection or the pod changes, so the list follows from the access
  alone, never from the time or the service. Kivali reads one page:
  give the whole list, with no `nextCursor`.
- **Within an action, the arguments are yours to check.** A notebook,
  project or blog the access does not name is an error result that
  names the cause and says to ask the owner. It does not list what the
  access does allow; the agent's list says that, in its input schemas,
  as the template's `enum` of notebooks does.
- **Kivali passes on only the actions the manifest declares.** An
  action whose `inputSchema` is not `"type": "object"` is left out, and
  only Kivali's own log says so; `integration_validate`'s warning and
  `try.py` are where you see it.

## What an action reads and answers

- **Two descriptions, two readers.** The manifest's
  `tools[].description` is the owner's, on the install screen: what the
  action does in the service, and what it changes or deletes. The
  description in `tools/list` is the agent's model's: when to use the
  action and what each argument means. Kivali adds "Through the
  <display name> integration; the owner decides what you may do with
  it."
- **The arguments** are what the model wrote; Kivali checks only that
  they are JSON. Check their types and the ones the action needs.
- **The agent reads** the text of each `text` block, or, with none, the
  `structuredContent` as JSON. Images, audio and resources are dropped,
  with a line saying so. Answer compact JSON of what an agent needs,
  not the service's whole response; it goes into the agent's context.
- **A failure is a result with `"isError": true`** and a text saying
  what went wrong and what the agent can do; it is also the reason in
  the integration's audit. A JSON-RPC error reaches the agent as "The
  <display name> integration answered with an error: <message>"; keep
  that for a request that is malformed.
- **Every secret value of the connection, six characters or longer,
  becomes `[secret]`** in what reaches an agent and in the log. Never
  quote the token all the same.
- **The proxy variables carry the integration's own credential for the
  proxy.** Kivali removes it from everything the server sends Kivali,
  but not from what it sends the service: never send the environment
  or the proxy's address anywhere.

## Test connection and an access field's options

Both are a `tools/call` with no arguments and no `_meta`, which Kivali
waits 60 seconds for.

- **`kivali.verify`** is verified when its answer is not an error. The
  owner reads the answer's first text block, cut to 1000 bytes: name
  the account the connection signs in as ("Signed in as
  ops@acme.example, workspace Acme"), and when it fails, say what to
  check.
- **A field's options** (`kivali.access.choices`) are the answer's
  first text block, a JSON array of strings or an object whose
  `options` is one. Anything else, or an error, and the owner's form
  says it could not list them. Each string is what the owner sees and
  what an agent's access holds once chosen, so make it readable, make
  it stable, and make it something the server matches later.

## Try it as Kivali calls it

`/files/skills/build-an-integration/scripts/try.py` starts the server
the way the pod does and sends what Kivali sends: its own `tools/list`
at start, then each request you name, as the owner or as one agent with
one access. It answers the server's `kivali/credential` and
`kivali/log`, shows the log as it comes, and shows each answer as the
agent or the owner reads it. A line starting `problem:` is something
that fails in Kivali, and the script exits 1 when there is one: a line
that is not JSON-RPC, an id that will not match, an action missing
from the owner's list, an input schema Kivali leaves out, an answer
later than Kivali waits, options the form cannot read, and for a setup
bundle every refusal of [The declarations](#the-declarations), a return
whose state the flow never gave, and requirement rows Kivali leaves
out or cannot read. A line starting `note:` is what Kivali does that you
may not expect.

The server gets no proxy variable, so it reaches your shell's own
machine and nothing else: a stub answers, the real service does not.
Write the stub from the documentation, with the paths, answers and
error answers the server handles; `http.server` from the standard
library is enough (`scripts/deals_stub.py` is one). Start it with
`--stub`, which waits until the ports the `--env` addresses name accept
connections, stops it at the end and shows its output. A stub started
with `&` holds your command open until it exits.

```
cd /files/artifacts/private/integrations/notes
T=/files/skills/build-an-integration/scripts/try.py
S="python3 /files/background/notes-stub.py 8765"
A=NOTES_API=http://127.0.0.1:8765

# The owner's side: the list, Test connection, the notebooks field's options.
python3 $T --stub "$S" --env $A --field token=test-token . list verify choices notebooks

# An agent that may search Ideas and not add: what it gets, one call
# its access covers, one it does not, and one Kivali refuses.
python3 $T --stub "$S" --env $A --field token=test-token \
  --agent dana --access '{"notebooks":["Ideas"],"add":false}' . \
  list call find_notes '{"notebook":"Ideas"}' \
  call find_notes '{"notebook":"Ops"}' call add_note '{"notebook":"Ideas","title":"x"}'

# Before the owner installs it.
python3 $T --agent dana --access '{"notebooks":["Ideas"]}' . call find_notes '{"notebook":"Ideas"}'
```

`--field` fills a connection field by its `user_config` key, or for a
setup bundle a key setup keeps; with none, the server starts as it
does before the owner installs it. `--env` sets a variable over the
manifest's. `--access` is the access as the form would send it, and
the script says when it holds something the form never sends.
`python3 $T --help` lists the requests and options.

### A setup bundle's requests

- `setup [FLOW]` runs a flow (`setup`, or a Fix's name) as Kivali's
  runner does: `kivali/setup` call after call with the answer Kivali
  builds, each step printed as the owner's page shows it (secrets as
  `[secret]`), each progress notification as it comes, what each step
  keeps, the server started again with it. A Form's fields come from
  `--answer KEY=VALUE`, in order: give a key twice to answer two Forms
  that ask for it (a refused answer, then a good one); the last is used
  for any Form after. A Go's return comes from `--return QUERY`, one
  per Go (`--return 'code=abc'`, or `--return 'error=access_denied'`
  for the owner turning it down); a return without a state gets the
  one Kivali gave the callback, unless the Go is `stateless`. A Wait is
  checked every `--every` seconds (0 in a script) until it changes or
  `--checks` checks have run. Without the answer a step needs, once a
  Wait's checks have run, or at a Form asked again after the same
  answer, the flow stops there, as when the owner leaves it: an install
  that stops
  before Done forgets what it kept, as Kivali does, and a `setup`
  after it in the same run starts again against what the stub holds.
- `requirements` reads the rows as the page does, held to what Kivali
  keeps of them, and says which flow each unmet row's Fix runs.
- `uninstall` sends `kivali/uninstall` as Uninstall does.

What a `problem:` line from a setup request means:

| `problem:` | Kivali |
| --- | --- |
| Kivali fails the flow: <words> | fails the flow with those words; the owner reads them under "Setup did not finish" |
| the return's state is none the flow gave | finds no flow: the browser lands on the integrations list and setup is not called |
| kivali/setup did not answer within 300 seconds | fails the flow: "setup did not finish its step within 5 minutes" |
| kivali/requirements answered rows Kivali cannot read | shows one unmet row, "<Integration> could not check its requirements" |
| the row <id> is left out | leaves that row out and logs it |
| the integration gave more than 20 rows | shows the first 20 |
| kivali/requirements did not answer within 5 seconds | shows the unmet row with "it did not answer within 5 seconds" |
| Kivali sends kivali/<x> only to a bundle whose method is setup | never sends it to this bundle |

## Publishing

`integration_publish(path, notes)` runs the same checks as
`integration_validate`, refuses a bundle with an error or a version
number already stored, stores the version as waiting for the owner, and
sends the owner the request to install it. Nothing in it runs before
the owner approves it.

The owner decides on the install screen. It lists who built the
version, every host, the software it installs, every connection field
with the secrets marked, or for a setup bundle under **Setup** its
summary, every host it sends the browser to, every field it asks for
and every value it keeps, for a sign-in the hosts it signs in at,
every action with the manifest's description, the access form, the
bundle's size and the check's warnings. While another version is in
use it also lists the hosts, software, connection fields, setup
declarations, actions and access form fields this one adds or removes.
The notes, at most 4096 bytes, are at the top: what the integration
does, every host it reaches, each secret it asks for and the narrowest
scopes it needs, what its setup makes and changes on the service, every
action that changes or deletes something in the service, and what a
changed access form does to the agents' access.

The answer arrives in your chat, naming the version: approved, it is
the version in use; denied, it never runs.

## Versions

- A version is `MAJOR.MINOR.PATCH`, and a number stored once, in any
  state, is refused again. Every change gets a new number: a fix the
  patch, a new action or field the minor.
- The version in use stays until the owner approves a newer one. A
  newer version keeps the connection setup made; one that needs more of
  the service (a scope, a permission) says so in a requirement whose
  Fix gets it, as template-setup's `scopes` row does.
- The owner can roll back to a version that was in use before; its
  code runs again in place of the one in use, and every agent's access
  is checked against its form.

## Before you publish

- `integration_validate` says the bundle would be accepted, and you
  can say why each warning is there.
- `try.py` shows no problem for:
  - the owner's list, `verify` naming the account, and each options
    field's list;
  - each kind of access an owner would give: the list the agent gets,
    and each action called once where the access covers it and once
    where it does not;
  - every action with no connection.
- For a setup bundle, `try.py` also shows no problem for:
  - the install, start to Done, and each other path through it (the
    fallback, each answer the service refuses, a Go the owner turns
    down);
  - a flow left at each step and started again, making nothing twice
    on the service;
  - `requirements` with no connection (no request to the service, at
    once), met, and not met, each unmet row's Fix run to Done and the
    requirements read again;
  - `uninstall`, and `uninstall` again once the install is gone.
- It answers the three setup requests, not -32601; every word the owner
  reads keeps to [Words the owner reads](#words-the-owner-reads); no
  value the owner must read is kept or carried in state.
- `kivali.hosts` holds every host the calls, the setup's calls and
  their redirects reach, and nothing more; `setup.hosts` every host a
  Go or a Wait names, and nothing more.
- `kivali.packages` holds every package an action runs, and nothing
  more.
- The version number is new.
- The notes say what [Publishing](#publishing) lists.

## When the owner reports a failure

`integration_list` shows what is installed and how each one stands
(with the titles of the requirements not met), `integration_versions`
every version of one and what became of it, and `integration_logs` the
running integration's log, its own lines and Kivali's. Read the log
first.

| The log, the page or the agent says | The cause |
| --- | --- |
| the server wrote a line that is not JSON-RPC; Kivali stopped it | Something wrote to the output. |
| `Tunnel connection failed: 403`, or "is not a host this integration reaches" | A host missing from `kivali.hosts`, often a redirect's or one only setup calls. |
| did not answer within 60 seconds | No timeout on the request to the service, an id written back as text, or calls waiting behind one slow call. |
| `server exited` after `stderr` lines with a traceback | An import that is not in `vendor/`, or an exit at start. |
| is not running right now | The server keeps exiting; the log's last lines say why. |
| Could not install, then the package and a reason | A name Debian does not have, or Debian's repositories out of reach; the reason says which. |
| `FileNotFoundError` naming a command | The package that holds it is not in `kivali.packages`. |
| Your access no longer applies | The version in use changed the access form; the owner sets that access again. |
| An action missing from an agent's list | The access does not allow it, its input schema is not an object's, or the manifest does not declare it. |
| <Integration>'s setup tried to send your browser to / named a page on / asked for / tried to keep ..., which its manifest does not declare | A step outside [the declarations](#the-declarations): fix the step, or declare it in a new version. |
| <Integration>'s setup answered with a step Kivali cannot read | A key or a type outside [the step's shape](#a-flow-call-by-call). |
| setup did not finish its step within 5 minutes | A call that waits on the service too long; split it with a Wait. |
| <Integration>'s server does not answer kivali/... ; its author must add it | The server answers that request -32601. |
| <Integration> is not running, so its setup cannot go on | The server did not start within two minutes; its log says why. |
| <Integration> is not running, so its own flows cannot run / so it cannot undo its installation | The server is down; a Fix or Uninstall waits for it, or the owner switches it off to uninstall. |
| <Integration> could not check its requirements: ... | `kivali/requirements` answered an error, rows without `id`, `title`, `words` or `met`, or nothing within five seconds. |
| the service sent the browser back without its state | The return lost the callback's state: give the callback where the service sends the browser back, or mark the Go `stateless`. |
| [secret] in a page address, a code or the step's words | A value kept or carried in state shows there ([State and keep](#state-and-keep)). |
| Kivali could not find where to sign in to / register itself with <Integration> | A registered client's metadata or registration: the hosts, the grant, PKCE or the registration endpoint ([A registered client](#a-registered-client)). |

Then fix it, give it a new version number, call it again with
`try.py`, and publish.
