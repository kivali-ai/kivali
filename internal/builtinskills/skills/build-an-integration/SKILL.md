---
name: build-an-integration
description: How to build or fix a Kivali integration bundle so it works the first time the owner installs it. The steps, the bundle and its manifest, what the server meets where it runs, what each request carries and expects back, a starting bundle, and a script that calls the server the way Kivali does.
when_to_use: Before writing or changing an integration bundle's server or manifest, and again before integration_publish. Skip it for using an installed integration; its actions are tools like any other.
version: 1.0.0
---

# Build an integration

An integration is a bundle: a directory holding `manifest.json` and a
stdio MCP server in Python. The owner installs a version of it, connects
it to the service with a token, and gives each agent access; agents
then call its actions as tools of their own.

`integration_validate` checks the manifest, starts the server with no
connection and asks for its list of actions. It never calls an action.
What only a call shows reaches the owner after they approve, as a
failure an agent reports, and each fix is a new version and another
approval. So start from the template, call every action with `try.py`,
and publish once.

## The steps

1. Read the service's API documentation with WebFetch: the calls the
   actions need, how a token is made and what it may be scoped to,
   every host a call or its redirects reach, and the error answers.
2. Copy the template into the bundle's directory, which lives under
   `/files/artifacts/private/integrations/<name>/`:

   ```
   mkdir -p /files/artifacts/private/integrations
   cp -R /files/skills/build-an-integration/template /files/artifacts/private/integrations/<name>
   ```

3. Write the server and the manifest there.
4. Write a stub of the service in `/files/background/`, a small local
   server answering as the documentation says, and call every action
   against it with `try.py`, as the owner and as agents with the access
   an owner would give.
5. Run `integration_validate` on the directory until it says the bundle
   would be accepted.
6. Publish with `integration_publish` and the version notes.
7. Wait for the owner's answer in your chat. After approving, the owner
   connects the integration, gives agents access, runs Test connection
   and tells you what it found.

Keep what is not the bundle outside its directory: every file in it is
stored with the version and counted on the install screen.

## The template

It is a working bundle for an imaginary notes service at
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

MCPB's manifest with a `kivali` section; the template's is a complete
one. Fields Kivali does not read are kept as written.

| Field | What it holds, and the check's rule |
| --- | --- |
| `manifest_version` | `"0.3"`. |
| `name` | 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit. Agents get its actions as the name with hyphens as underscores, an underscore and the action (`my-crm`'s `find` is `my_crm_find`), so the name's first word cannot be one of Kivali's own: file, artifact, agent, get, search, list, read, graph, assignment, publish, run, share, subagent, web, submit, propose, integration, kivali. |
| `version` | `MAJOR.MINOR.PATCH`, with an optional `-pre-release`, no leading zeros. |
| `display_name`, `description` | What the owner sees it called and what it does. |
| `author` | `{"name": "Kivali Developer (agent)"}`. |
| `server` | `"type": "python"`, `entry_point` (a file in the bundle), and `mcp_config`: `command` `python3`, `args` naming the entry point, and `env`. |
| `server.mcp_config.env` | The server's environment. `${user_config.<key>}` becomes that connection field's value; no other `${...}` is allowed. A plain value, such as the API's address, is passed as written. |
| `user_config` | The connection form, in this order. Each key is lowercase letters, digits and underscores, starting with a letter; each field has `type` (`string`, `number` or `boolean`), `title` and `description`, `"sensitive": true` for a secret, and `"required": true` when the connection needs it. |
| `tools` | Every action: a `name` (lowercase letters, digits and underscores, starting with a letter) and a `description` the owner reads on the install screen. |
| `kivali.connection` | `{"method": "token"}`: fields the owner pastes. Or a connection made by signing in, `device` or `browser` ([A sign-in connection](#a-sign-in-connection)). |
| `kivali.setup_notes` | Markdown the owner reads above the connection form: what to do on the service's side first, such as registering an app and the scopes to tick. Optional. |
| `kivali.hosts` | Every host the server reaches; it reaches those and no other ([The network](#the-network)). Lowercase, no scheme, port or path, and never `*`. |
| `kivali.packages` | Optional: the Debian packages the pod installs before the server starts, such as `["gh"]` ([A command-line tool](#a-command-line-tool)). Each is a package's exact name as `apt-get install` takes it: lowercase letters, digits, `+`, `-` and `.`, with no version or release, listed once. |
| `kivali.access` | The access form ([The access form](#the-access-form)). Without it, access is all or nothing. |
| `kivali.verify` | The action Test connection calls. |

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

### A sign-in connection

A service that hands out no token to paste is connected by signing in:
the owner registers an app with the service, pastes its client id and
secret, and approves Kivali on the service's page. Kivali runs the
whole OAuth exchange; the server never does OAuth, and never sees the
client secret or the refresh token.

```json
"connection": {
  "method": "device",
  "device_endpoint": "https://github.com/login/device/code",
  "token_endpoint": "https://github.com/login/oauth/access_token",
  "revocation_endpoint": null,
  "scopes": ["repo"],
  "client": "operator",
  "credential_keys": { "access_token": "GH_TOKEN" }
}
```

- `method`: `device`, a code the owner enters on the service's page,
  where the service offers it; it needs `device_endpoint` and
  `token_endpoint`. `browser` (the service's sign-in page and back)
  needs `authorization_endpoint` and `token_endpoint`. A manifest may
  give both ways' endpoints; `method` is the one the page shows first.
- Every endpoint is an `https://` address from the service's OAuth
  documentation. `revocation_endpoint` is optional: with one, signing
  out and deleting the connection revoke its tokens.
- `scopes`: what the sign-in asks for; the owner sees them on the
  install screen. `pkce` (default true) and
  `extra_authorization_params` apply to browser sign-in.
- `client`: `operator` (or absent), the app the owner registers.
- `credential_keys.access_token`: the name the access token is given
  under, capital letters, digits and underscores (`GH_TOKEN`).
  Required.
- Kivali adds `client_id` and `client_secret` to the connection form
  itself: `user_config` declares neither, nor `oauth_access_token` or
  `oauth_refresh_token`. Declare only what else the server needs.
- `kivali.hosts` lists the hosts the server calls, not the service's
  sign-in hosts: Kivali talks to those.
- `kivali.setup_notes` tells the owner how to register the app: where
  in the service's settings, which settings to turn on (GitHub: device
  flow), which scopes to tick.

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
- **It starts with no connection**: before the owner connects it, and
  in `integration_validate`, every `${user_config.*}` is empty. It must
  still start, answer `initialize` and list every action within 20
  seconds, which is all `integration_validate` gives it. Never exit for
  a missing value, and never reach the service at start; refuse the
  call instead, as the template's `api` does.
- **It reads the connection once.** A changed connection restarts the
  server with the new values. Put every field the server needs in
  `mcp_config.env` and the environment holds the connection as it is
  now; `kivali/credential` adds nothing to it. A sign-in connection's
  token is the exception: ask for it on every call.
- **It is stopped** by its input closing, and killed 5 seconds later
  if it has not exited. It is started again for a new version, a
  changed connection, and after three missed pings: Kivali pings every
  30 seconds and waits 20 for the answer.

## The network

The pod's network reaches one place, the egress proxy that
`HTTPS_PROXY` and `HTTP_PROXY` name, which lets it reach the hosts in
`kivali.hosts` and nothing else. There is no DNS and no direct
connection.

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
- **Requests overlap.** Kivali sends each agent's call as it comes, and
  a ping every 30 seconds. Answer `tools/call` on a thread, write each
  line whole, and carry each request's `id` back exactly as it came.
  Kivali's ids are whole numbers and it matches them as numbers: an id
  written back as text is never matched, and the call fails after 60
  seconds.
- **Answer** `initialize` with `protocolVersion` (the one asked for,
  `2025-06-18`), `"capabilities": {"tools": {}}` and `serverInfo`;
  `ping` with `{}`; `tools/list` and `tools/call` as below; any other
  method with error -32601.
- **Only `kivali/log` and `kivali/credential` reach Kivali.** Any other
  notification the server sends (progress, `list_changed`) is dropped,
  and any other request (sampling, roots, elicitation) is answered
  -32601. An integration cannot ask the agent or the owner anything
  during a call.
- **`kivali/credential`** answers the connection's values as they are
  now, `{"env": {...}, "fields": {...}}`, or error -32001 with no
  connection; a sign-in connection's also -32002 and -32003
  ([A sign-in connection](#a-sign-in-connection)). Its answer comes on the server's input, among Kivali's
  requests: the reading loop hands it to whatever waits for that id,
  and a reading loop that stops to wait for it never reads it.

## Who is calling, with what access

- **The owner's requests carry no `_meta.kivali`**: the `tools/list` at
  every start, Test connection, and an access field's options. Their
  list holds every action the manifest declares, or
  `integration_validate` refuses the bundle.
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
later than Kivali waits, options the form cannot read.

The server gets no proxy variable, so it reaches your shell's own
machine and nothing else: a stub answers, the real service does not.
Write the stub from the documentation, with the paths, answers and
error answers the server handles; `http.server` from the standard
library is enough. Start it with `--stub`, which waits until the ports
the `--env` addresses name accept connections and stops it at the end.
A stub started with `&` holds your command open until it exits.

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

# Before the owner connects it.
python3 $T --agent dana --access '{"notebooks":["Ideas"]}' . call find_notes '{"notebook":"Ideas"}'
```

`--field` fills a connection field by its `user_config` key; with none,
the server starts as it does before the owner connects it. `--env`
sets a variable over the manifest's. `--access` is the access as the
form would send it, and the script says when it holds something the
form never sends. `python3 $T --help` lists the requests and options.

## Publishing

`integration_publish(path, notes)` runs the same checks as
`integration_validate`, refuses a bundle with an error or a version
number already stored, stores the version as waiting for the owner, and
sends the owner the request to install it. Nothing in it runs before
the owner approves it.

The owner decides on the install screen. It lists who built the
version, every host, the software it installs, every connection field
with the secrets marked,
every action with the manifest's description, the access form, the
bundle's size and the check's warnings. While another version is in
use it also lists the hosts, software, connection fields, actions and
access form fields this one adds or removes. The notes, at most 4096 bytes, are at
the top: what the integration does, every host it reaches, each secret
it asks for and the narrowest scopes it needs, every action that
changes or deletes something in the service, and what a changed access
form does to the agents' access.

The answer arrives in your chat, naming the version: approved, it is
the version in use; denied, it never runs.

## Versions

- A version is `MAJOR.MINOR.PATCH`, and a number stored once, in any
  state, is refused again. Every change gets a new number: a fix the
  patch, a new action or field the minor.
- The version in use stays until the owner approves a newer one.
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
- `kivali.hosts` holds every host the calls and their redirects reach,
  and nothing more.
- `kivali.packages` holds every package an action runs, and nothing
  more.
- The version number is new.
- The notes say what [Publishing](#publishing) lists.

## When the owner reports a failure

`integration_list` shows what is installed and how each one stands,
`integration_versions` every version of one and what became of it, and
`integration_logs` the running integration's log, its own lines and
Kivali's. Read the log first.

| The log or the agent says | The cause |
| --- | --- |
| the server wrote a line that is not JSON-RPC; Kivali stopped it | Something wrote to the output. |
| `Tunnel connection failed: 403`, or "is not a host this integration reaches" | A host missing from `kivali.hosts`, often a redirect's. |
| did not answer within 60 seconds | No timeout on the request to the service, an id written back as text, or calls waiting behind one slow call. |
| `server exited` after `stderr` lines with a traceback | An import that is not in `vendor/`, or an exit at start. |
| is not running right now | The server keeps exiting; the log's last lines say why. |
| Could not install, then the package and a reason | A name Debian does not have, or Debian's repositories out of reach; the reason says which. |
| `FileNotFoundError` naming a command | The package that holds it is not in `kivali.packages`. |
| Your access no longer applies | The version in use changed the access form; the owner sets that access again. |
| An action missing from an agent's list | The access does not allow it, its input schema is not an object's, or the manifest does not declare it. |

Then fix it, give it a new version number, call it again with
`try.py`, and publish.
