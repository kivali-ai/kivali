# Integrations

An integration is an external service agents act on, such as a CRM or
a blog, installed from a bundle or shipped with Kivali. This page is
the developer's reference: the bundle and its manifest, the check every
bundle passes, the access form, where Kivali keeps an integration and
its connection, how a version is approved or rolled back to, how an
approved version runs, how agents call it, the integrations built into
Kivali, and the Kivali Developer, the agent that builds bundles.
What the owner sees is in
[../integrations.md](../integrations.md).

The words, as the owner reads them: an **integration** is the
installed thing; a **version** is one stored bundle of it; its
**connection** is what the owner pastes (a token, a portal name); an
agent's **access** is what that agent may do through it; its
**actions** are the tools it gives agents. The **bundle** (the zip)
and the **manifest** are developer words. Operator-facing text never
says MCP, tool or pod.

The code: `internal/integration` (the manifest, the check, the access
form, reading and unpacking a bundle, what one version changes from
another; a leaf package),
`internal/store/integrations.go` and `integration_logs.go` (where
everything is kept), `internal/secrets` (the connection's secret
fields), `internal/integrationpod` (the pod, its provisioner and the
shim that runs in it), `internal/web/integration_registry.go` and
`integration_socket.go` (core's side of a running integration),
`internal/web/integration_tools.go` and `internal/mcp/integration_tools.go`
(agents' calls, core's side and the agent's MCP server's),
`internal/web/kivali_developer.go` and
`internal/mcp/kivali_developer_tools.go` (the Kivali Developer's
tools), `internal/web/api_integrations.go` (the API) and
`internal/web/apitypes/integrations.go` (the wire types).
`internal/builtinintegrations` holds the integrations that ship with
Kivali (each in a package of its own, listed in
`internal/builtinintegrations/builtins`),
`internal/store/builtin_integrations.go` writes them at boot,
`internal/web/integration_builtin.go` calls them and
`internal/web/integration_builtin_egress.go` keeps their connections'
proxy credentials ([Built-in integrations](#built-in-integrations)).
The Git built-in is `internal/builtinintegrations/gitintegration`,
with `internal/gitrepo` (a repository's URL and access patterns),
`internal/shelljob` (the shell job's wire shapes) and
`internal/agentshell/git.go` (the daemon's `POST /v1/git`)
([The git built-in](#the-git-built-in)).
`cmd/fake-integration` is a stdio MCP server for tests
(`internal/integrationpod/fakeintegration`);
`internal/integration/testdata/echo` is a real Python bundle the tests
build on, end to end; `internal/builtinintegrations/echotest` is a
built-in that only tests register.

## The bundle

A bundle is a zip of a directory:

```
manifest.json      the manifest, at the root
server/main.py     the server's code (any layout; the manifest names the entry point)
README.md          optional
icon.png           optional
```

The zip may hold the directory itself (`zip -r hubspot hubspot/`):
when every entry sits in one top-level folder, that folder is
stripped, as for skills. macOS's `__MACOSX/` folder is ignored. A
bundle holds at most 5000 files and 64 MiB unpacked, and no symlinks,
special files, absolute paths or `..`. Unpacking goes through
`internal/zipsafe`, which skills use too, and refuses all of these
whatever the zip declares, counting the bytes it writes.

## The manifest

`manifest.json` is MCPB's manifest with one more section, `kivali`.
Fields Kivali does not read are kept: the store keeps the manifest
byte for byte.

```json
{
  "manifest_version": "0.3",
  "name": "hubspot",
  "version": "1.2.0",
  "display_name": "HubSpot",
  "description": "Blog posts and contacts in HubSpot.",
  "author": { "name": "Kivali Developer (agent)" },
  "server": {
    "type": "python",
    "entry_point": "server/main.py",
    "mcp_config": {
      "command": "python3",
      "args": ["server/main.py"],
      "env": { "HUBSPOT_TOKEN": "${user_config.token}" }
    }
  },
  "user_config": {
    "token": { "type": "string", "title": "Private app token", "description": "...", "sensitive": true, "required": true }
  },
  "tools": [ { "name": "create_post", "description": "..." } ],
  "kivali": {
    "connection": { "method": "token" },
    "hosts": ["api.hubapi.com"],
    "access": {
      "schema": {
        "type": "object",
        "properties": {
          "blogs": { "type": "array", "items": { "type": "string" }, "title": "Blogs" },
          "write": { "type": "boolean", "title": "May publish and edit" }
        },
        "required": ["blogs"]
      },
      "choices": { "blogs": "list_blogs" }
    },
    "verify": "whoami"
  }
}
```

- `user_config` is the connection form, in the manifest's order. A
  `sensitive` field is a secret (below).
- `kivali.connection.method` is how the connection is made: `token`,
  fields the owner pastes.
- `kivali.hosts` is every host the integration reaches, in the egress
  allowlist's pattern language ([architecture.md](architecture.md),
  the egress proxy).
- `kivali.access` is the access form (below). Without it, access is
  all or nothing: an agent has the integration or does not.
- `kivali.verify` names the tool Test connection calls.

An integration's tools reach agents named with its tool prefix, its
name with hyphens as underscores and a trailing underscore
(`integration.ToolPrefix`: `hubspot_create_post`, `my_crm_find`).

## The check

`integration.Validate(manifest, files)` returns findings, each an
error or a warning. A bundle with an error is refused at upload with
every finding (`IntegrationUploadRefused`); warnings are stored with
the version and shown on its install screen. The rules:

| Field | Rule |
| --- | --- |
| `manifest_version` | Present, and `0.x`. |
| `name` | 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit. The first word of its tool prefix is not one of Kivali's own tool prefixes: file, artifact, agent, get, search, list, read, graph, assignment, publish, run, share, subagent, web, submit, propose, integration, kivali. |
| `version` | `MAJOR.MINOR.PATCH` with an optional `-pre-release`, no leading zeros, no build metadata (it names a directory). Versions order by semantic versioning (`integration.CompareVersions`). |
| `display_name` | Not empty. |
| `description` | Not empty, or a warning. |
| `server.type` | `python`. |
| `server.entry_point` | A file in the bundle. |
| `server.mcp_config` | `command` is `python3` or `python`; `args` is not empty; each `${user_config.<key>}` in `env` names a `user_config` key, and `env` holds no other `${...}`. |
| `user_config` | Each key is lowercase letters, digits and underscores, starting with a letter; `type` is `string`, `number` or `boolean`; `title` is not empty; `sensitive` and `required`, when given, are booleans. |
| `tools` | Not empty; names are unique, lowercase letters, digits and underscores starting with a letter; a tool without a description is a warning. |
| `kivali.connection.method` | `token`. A sign-in method (`device`, `browser`) is refused. |
| `kivali.hosts` | Not empty; each a lowercase host or pattern with no scheme, port, path, user or whitespace (`integration.ValidHost`, the rule of `store.ValidEgressHost`, which a store test holds it to); a bare `*` is refused. |
| `kivali.access` | See the access form. |
| `kivali.verify` | When given, a tool in `tools`. |
| The bundle | At most 5000 files and 64 MiB; no symlinks, special files, absolute paths or `..`. |

The check reads the bundle. It is not a review of what the code does:
the install screen is, where the owner reads what the version
reaches, holds and does before approving it.

A built-in integration's manifest is checked by
`integration.ValidateBuiltin(manifest)` by the same rules, with three
differences. There is no bundle. A `server` section is refused, and so
is any `kivali.hosts` entry: a built-in runs inside Kivali, and its
network reach is Kivali's own. The names that `kivali.verify` and
`kivali.access.choices` give must be lowercase letters, digits and
underscores, but need not be in `tools`, since they are passed to the
built-in's Go functions
([Built-in integrations](#built-in-integrations)).

A bundle is read from a zip (`integration.ReadBundle`, an upload) or
from a directory (`integration.ReadDirectory`, the Kivali
Developer's publish), by the same rules: `manifest.json` a regular file
at the root, every entry listed for the check, a symlink or special
file listed and never followed, `__MACOSX/` left out. A directory's
`__pycache__/` folders are left out too. The directory is read
through an `os.Root`, so nothing it opens is outside it, and a
directory bundle is stored by copying its files under the same limits,
with the manifest written as it was read, byte for byte.

## The access form

`kivali.access.schema` is a JSON Schema of one object, held to what a
flat form can show. The top level takes `type: object`, `properties`,
`required`, and optionally `title`, `description`, `$schema` and
`additionalProperties: false`. Each property takes `type`, `title`,
`description`, and, by type:

| Property | Form field (`AccessFieldKind`) |
| --- | --- |
| `string` | `text` |
| `string` with `enum` (strings) | `choice`, with the enum as options |
| `boolean` | `switch` |
| `array` of `items: {type: string}` | `list`, free text |
| `array` with `items.enum` | `choices`, with the enum as options |

Anything else is an error naming the property: nested objects,
numbers, combinators (`oneOf` and the like), constraints the form
would not show (`minItems`, `pattern`, ...). A `required` key must be a
property; a property without a title is a warning, and the form shows
its key.

`kivali.access.choices` maps a text or list field to the tool that
lists its options live through the connection: the field becomes a
`choice` or `choices` field with `choices: true` and no fixed options.
A field cannot have both an enum and a tool.

`integration.ValidateAccess` checks one agent's filled form: every key
a field, every value of its field's kind and from its fixed options,
every required field filled (an empty text or list does not fill it).
For a form whose options are listed live, only the kind is checked.
Without a form, only `{}` is valid. `integration.AccessSummary` puts
the access in words, field by field: "Blogs: Marketing, Eng · May
publish and edit" (a switch that is off and an empty field are left
out; no form reads "Full access").

## Where it is kept

Under the data directory, written only by `internal/store`, each
integration's writes under its own lock:

```
integrations/<name>/
  integration.yaml                  name, display_name, current_version, enabled, builtin, installed_at, installed_by
  versions/<version>/version.yaml   version, state, notes_md, published_by, published_at, approved_at, rejected_at,
                                    bundle_bytes, file_count, findings (the check's warnings)
  versions/<version>/manifest.json  the manifest, byte for byte
  versions/<version>/bundle/        the bundle, unpacked (none for a built-in integration)
  connections/<id>.yaml             id, created_at, fields (not secret), secret_keys, secret_name,
                                    verified_at, verified, verify_message
  access/<slug>.yaml                slug, connection_id, access, set_at, set_by
  audit.jsonl                       one row per call: at, agent, action, allowed, reason, duration_ms
  log.jsonl                         the integration's log: at, level, message (the runtime, below)
```

A version is stored whole or not at all: it is unpacked into a staging
directory beside the others and renamed into place. A version's states
and the moves between them:

- **pending**, stored and waiting: **approve** makes it approved
  (`store.ApproveIntegrationVersion`); **reject** makes it rejected
  (`store.RejectIntegrationVersion`).
- **approved**, the version in use, one at a time: it becomes
  superseded when another version is approved or rolled back to.
- **superseded**, in use before: **roll back** makes it approved again
  (`store.RollBackIntegrationVersion`), and the version in use becomes
  superseded.
- **rejected**: nothing leaves it; it never runs.

Approving or rolling back sets the version's `approved_at` to that
moment, and the integration's `current_version` and display name to
the version's. A version stored once, in any state, cannot be stored again: a
changed bundle needs a new version number. Every version is kept, its
bundle with it, in every state, until the integration is removed: a
roll back runs a superseded version's bundle again. A built-in
integration (`builtin`, which boot sets and an upload never does) takes
no upload and refuses the moves above (`store.ErrIntegrationBuiltin`):
its versions follow the binary. It cannot be removed while the binary
carries it; one the binary does not carry is removed with
`store.RemoveStaleBuiltinIntegration`
([Built-in integrations](#built-in-integrations)). Removing an integration deletes its directory (every
version, whatever its state), its connections' records, every agent's
access, the audit and the log; the API deletes the connections'
Secrets first, and the next reconcile stops its pod.

An access row is checked when it is read
(`store.ListIntegrationAccess`): it is invalid, and grants nothing,
when no version is approved, when it was set for another connection
than the current one, or when the approved version's form refuses it.
So approving a version or rolling back to one checks every row against
that version's form at once, and a row it refuses is invalid until it
is set again (`PUT .../access/{slug}`) or the version in use changes to
one that accepts it. The reason is on the row, so the page can say why,
and the page counts the invalid rows in a banner. Archiving an agent
clears its access everywhere (`ArchiveAgent`); deleting the connection
clears every agent's.

## Connections and Secrets

The connection's fields that are not sensitive are in
`connections/<id>.yaml`. The sensitive ones are in a Kubernetes Secret
of the server's namespace, `kivali-connection-<integration>-<id>`
(Opaque, labelled `app=kivali` and `kivali.ai/integration=<name>`),
through `secrets.Store`: `secrets.Kube` in a cluster, `secrets.Memory`
outside one (`make run`; the values are gone when the server stops).
The server logs which it uses at boot. The record keeps only which
sensitive fields are set (`secret_keys`) and the Secret's name. No
value is ever in the data directory, a log or an API answer, so
backups never carry one.

The chart's Role (`charts/kivali/templates/rbac.yaml`) grants get,
create, update and delete on Secrets in the namespace; the client
(`kube.Client.GetSecret`, `PutSecret`, `DeleteSecret`) updates with
PUT and creates with POST when there is none.

`PUT .../connection` takes the fields by key, from the approved
version's `user_config`. A field the form does not have is a 400; a
required field with no value, sent or stored, is a 400 ("the
connection needs <title>"); a `number` or `boolean` field must parse.
A sensitive field left out keeps its stored value, and an empty one
clears it; a field that is not sensitive is what is sent. A write
clears the last Test connection result. When the record names secret
fields its Secret does not hold (a backup restored into another
install, or the Secret deleted by hand), `ConnectionState.ReconnectNeeded`
is true, those fields read as not set, and the status is not
connected until they are entered again.

Test connection (`POST .../connection/verify`) calls the manifest's
`kivali.verify` tool through the running server, with no arguments and
no agent on the call, and keeps what it found on the connection's
record: `verified` (the call went through and the tool's result is not
an error), `verify_message` (the result's first text content, or the
error's text, cut to 1000 bytes) and `verified_at`. A call not answered
within 60 seconds is kept as failed. The options an access field lists
(`GET .../choices/{key}`) call the field's tool the same way and read
its result's first text content as a JSON array of strings, or an
object whose `options` is one. Both write an audit row for the owner
(agent `ceo`, the tool as the action, the integration's words as the
reason when it failed), and both answer 409 "the integration is not
running" while no shim is connected; a built-in integration is running
while its code is in the binary, and its `Verify` and `Choices` are called with the connection
instead (and the options need one: 409 "connect the integration
first" without it). The audit lists what is in
`audit.jsonl`, which these calls and agents' calls
([The audit](#the-audit)) append to.

## Installing a version

An upload (`POST /api/v1/org/integrations`) reads the bundle, runs the
check and stores the version as pending, published by the owner; the
Kivali Developer's `integration_publish` stores one published by the
developer, with its notes, and asks the owner to approve it
([Publishing](#publishing)). Its
install screen (`GET .../versions/{version}`, the web app's
`/org/integrations/<name>/versions/<version>`) says what approving
lets in: who built it and published it, every host, every connection
field (secrets marked), every action, what the access form asks, the
bundle's size and the check's warnings. Nothing runs before it is
approved.

While another version is in use (`IntegrationVersion.CurrentVersion`),
the screen also says what this one changes from it, under "What
changes from version X": `IntegrationVersion.Changes`, worked out by
`integration.Diff(current, this)`, one line for each of hosts,
connection fields, actions and access form fields added or removed
("Hosts added: uploads.example.com", "Connection fields added: API key
(secret)"). A host is matched by its pattern, a field by its key and an
action by its name. With none of those changed the list is empty, and
the screen says "No change to hosts, secrets, actions or the access
form." The same block is on a superseded version's screen, where it
says what rolling back to it changes.

### Rolling back

`POST .../versions/{version}/rollback` makes a superseded version the
one in use again, and the version in use superseded
([Where it is kept](#where-it-is-kept)). It answers with the version's
install screen; any other state is a 409 that says why (the version in
use, a pending version, which is approved instead, or a rejected one,
which never runs). The registry reconciles: the pod's spec carries the
version, so it is built again from the rolled-back version's bundle,
with a new token. An audit row records it for the owner: agent `ceo`,
action `rollback`, reason "from version 1.1.0 to version 1.0.0". Every
agent's access stays as it is and is checked against the rolled-back
version's form when it is next read. The web app offers **Roll back**
on a superseded version, in the page's version list and on its install
screen, after a dialog that names the version that stops.

A request to install one comes to the owner as a
`ceo_approval_request` carrying `integration_install: {name,
version}` (`store.IntegrationInstall`). In Needs you it is a proposal
(`ProposalKindIntegrationInstall`) whose review page is the install
screen; the proposal page answers 404 for it. Answering it, from Needs
you or from the install screen, goes through `respondAsCEO`: approving
approves the version before the response is written, denying rejects
it, and the response names the version on both outcomes, so the
publisher reads "I approved version 1.2.0 of the `hubspot`
integration. It is the version in use." An approval of the version
already in use goes through; one of a version rejected, superseded or
no longer stored is refused. A denial of a version no longer stored,
or already rejected, goes through; one of the version in use is
refused. Approving or rejecting from the install screen when no
request waits for it, or when the agent that asked has left the team,
changes the version alone.

## The runtime

An integration that should run (an approved version, switched on, not
built in) runs in a pod of its own, `integrationpod-<name>`, which core
keeps up while it should (`internal/integrationpod`, `manifest.go` and
`provisioner.go`). Core talks MCP to the integration's server through a
shim in the pod. An agent's calls carry the agent and its access
([Agents' calls](#agents-calls)); core's own calls (Test connection,
the options a field lists, the `tools/list` at each connect, `ping`)
carry no agent.

### The pod

| Container | Image | What it does |
| --- | --- | --- |
| `shim` (init) | `kivali` | `cp /usr/local/bin/kivali /shim/kivali`, into an emptyDir |
| `integration` | `kivali-agent-shell` | `/shim/kivali integration-shim` (`integration_shim_cmd.go`, `internal/integrationpod/shim.go`), which starts the server: the agent shell's image has python3 |

The `integration` container mounts the `shim` emptyDir at `/shim`,
read-only; `integration/`, the UDS dir's subdirectory that holds
`integration.sock`, read-only and with `type: Directory`; the version's
bundle, the data PVC's subPath
`integrations/<name>/versions/<version>/bundle`, at `/bundle`,
read-only; and an emptyDir at `/tmp` (256 MiB), the server's HOME.
Nothing else of the data PVC. Both containers run as 65532 with no
privilege escalation and every capability dropped; the pod has no
service-account token, no Service and no ports, `restartPolicy:
Always` (kubelet starts the shim again when it exits with its server),
a 10 s stop grace, and the agent pod's required affinity to the
server's node. Requests 50m/128Mi, limits 1 CPU/1 GiB. Its env is
`KIVALI_INTEGRATION_NAME`, `_VERSION`, `_TOKEN` (the per-pod token
core minted), `_CORE_UDS`, `_BUNDLE` and the bare proxy variables, the
proxy's host resolved to its IP when the pod is built (the pod has no
DNS). The spec names no Secret and carries no connection value, and no
label carries the token. The chart's network policy
`kivali-integrationpod-egress` lets the pod reach the egress proxy's
port and nothing else.

The spec carries a hash of itself (`kivali-spec-hash`), as agent and
browser pods do. `Start` keeps a pod whose hash matches and replaces one
that differs: a new version, a new token or a new image is a new pod.

### Reconcile and boot

`internal/web/integration_registry.go` keeps one entry per integration
that should run: its version, its pod's token, the connected shim (the
link), the requests waiting on it, the last `tools/list` answer, and
its health. All pod operations run on one goroutine
(`Server.StartIntegrations`): a reconcile at boot and after every
approval, rejection, roll back, switch, removal and connection change,
which starts the pod of every integration that should run and stops the
pod (and drops the proxy credential) of every one that should not. A
version newly in use, approved or rolled back to, gets a new token, and
so a new pod. At boot core also
stops the pods of integrations no longer installed, and takes on a pod
left running from before the restart when it is built exactly as core
would build it for the approved version (`Provisioner.Token` reads the
token from the pod's spec), so pods outlive a server restart and their
shims reconnect. Shutdown closes every link and stops no pod.

### The shim and its socket

`integration.sock` (`StartIntegrationSocket`,
`internal/web/integration_socket.go`) serves two routes and nothing
else. Every request carries `X-Kivali-Integration-Token`, which must be
the pod's token.

- `GET /v1/integration/{name}/events`: the shim's one SSE stream
  (`event:` and a one-line JSON `data:`; comment heartbeats every 15 s).
  The handshake also carries `X-Kivali-Integration-Version`, which must
  be the approved version, and `X-Kivali-Integration-Proxy-Hash`. A bad
  token or version is a 403, and the shim tries again after a wait on
  its clock, 200 ms doubling to 5 s. A second connection for an
  integration replaces the first. Events: `start` (sent on every
  connect: fetch the connection's values and start the server; a shim
  whose server runs starts it again only if the values changed),
  `rpc` (one JSON-RPC request or notification for the server),
  `restart` (the connection changed: stop the server, fetch the values
  again, start it) and `shutdown` (stop the server and exit).
- `POST /v1/integration/{name}/rpc`: one JSON-RPC message from the pod:
  a response to core's request, matched by its id, or a request or
  notification. A request is answered with its JSON-RPC response, and
  anything else with 204.

The shim reads `/bundle/manifest.json` (the bundle must be the name and
version the pod was built for) and starts the server on `start`:
`mcp_config.command` and its args in `/bundle`, with PATH, `HOME=/tmp`,
LANG, the manifest's env with the connection's values put in, and the
proxy variables carrying its scoped credential, and nothing else of its
own environment. Then the MCP handshake: `initialize` with protocol
version `2025-06-18` and client `kivali`, then
`notifications/initialized`, within 20 s, or the shim exits non-zero.
Core's `rpc` messages go to the server's input a line each, queued
until the server has started; until then a call simply waits. The
server's output is read a line at a time: a response goes to core; a
`kivali/credential` or `kivali/log` request goes to core and core's
answer to the server; any other request (sampling, roots, elicitation)
is answered by the shim with -32601; notifications other than
`kivali/log` are dropped. A line that is not JSON-RPC, or over 16 MiB,
ends the server. The server's error output becomes `kivali/log` lines
of level `stderr`, and the shim adds its own (`server started`,
`server exited: ...`). The shim exits with the server's status; on
SIGTERM it closes the server's input and kills it after 5 s.
`KIVALI_INTEGRATION_COMMAND`, a path, replaces `mcp_config.command`:
a test hook, which the pod spec never sets.

### The pod's requests

- `kivali/credential` answers `{"env": {...}, "fields": {...}}`: the
  manifest's `mcp_config.env` with each `${user_config.<key>}` replaced
  by the connection's value (`integration.ServerEnv`), and every field's
  value by key. Core reads them from the connection's record and its
  Secret at that moment and keeps none. With no connection the error is
  -32001 "no connection", and the shim starts the server with every
  value empty, so a connection put later restarts it with them.
- `kivali/log`, `{"level", "message"}`, appends a line to `log.jsonl`
  (level `info`, `warning`, `error`, `debug` or `stderr`; any other is
  `info`; a message is cut to 8 KiB) with every secret value of the
  connection replaced by `[secret]`. The file is capped: past 4000 lines
  it is rewritten with the last 2000. Core adds lines of its own: a
  restart after missed health checks, a pod that cannot be placed for
  want of memory.

### The scoped proxy credential

The shim draws a 32-byte secret when it starts (`internal/egresscred`)
and sends its SHA-256 on the events handshake. Core registers it under
the proxy user `integration-<name>` with the hosts the approved
version's `kivali.hosts` names (`store.SetEgressScopedCredential`; kept
in `egress_agents.json` beside the agents' hashes, out of backups, and
mirrored under `scoped` in the proxy's agents file), and drops it when
the integration stops or is removed. The server's proxy variables carry
`http://integration-<name>:<secret>@<proxy>`. In scoped mode the proxy
admits a host only when it matches one of those hosts, in the
allowlist's pattern language and under its address rules, never the
team's list, and counts nothing. The shim scrubs the secret, in every
form it can be written, from everything it sends core, the server's
error output included. A built-in integration's connection has a
scoped credential of its own, which core keeps
([The git built-in](#the-git-built-in)); its user,
`integration-<name>.<connection id>`, is never a bundle's, since an
integration name has no dot, and the boot reconcile, which drops the
credentials of bundles no longer installed, leaves it alone.

### Health

Every 30 seconds core sends each connected server MCP's `ping` (20 s to
answer; any answer counts, an error too). Three missed in a row give the
integration a new token, and so a new pod, and say so in its log. An
integration that should run and has had no shim for three checks gets a
reconcile, which makes its pod again if it is gone. A shim's stream that
ends makes the integration not running; requests waiting on it fail at
once. A request waits 60 s for a `tools/call` and 20 s for anything
else.

## Agents' calls

An agent calls an integration's actions as tools of its own
`kivali mcp`, named with the integration's tool prefix
(`hubspot_create_post`). Only the full-agent toolkit has them;
subagents and web subagents have none. Core is the only place access
is enforced (`internal/web/integration_tools.go`): the integration
never authenticates an agent, and reads who is calling, with what
access, from the call's `_meta`.

### What an agent may call

`Server.agentIntegrationTools(slug)` takes each integration that has
an approved version, is switched on, is running (its shim connected,
or built in) and has a valid access row for the agent, and asks it for
its `tools/list` for that agent (a built-in's `Tools`, with the agent
and its access). Of the answer it keeps the tools the
approved version's manifest declares, the actions its install screen
listed, whose input schema is an object's (a tool with none gets
`{"type":"object"}`; one with another schema is left out and logged).
Each becomes a tool named prefix and tool, with the server's
description and one sentence added: "Through the <display name>
integration; the owner decides what you may do with it." The list is
sorted by name; when two integrations' prefixes overlap and name two
actions alike, the first is kept. The integrations are asked at once,
and one that does not answer within 20 s is left out and logged.

An agent's `tools/list` answer is kept on the link it came through, by
agent and the SHA-256 of its access (`integrationRegistry.AgentTools`).
It is asked for again when the agent's access changes, when the
connection changes (the server restarts with the new values), and when
the shim connects again: a new pod, a new version, a reconnect. A
built-in's `Tools` is asked each time and kept by no one.

### The stamp

Every `tools/list` and `tools/call` core sends for an agent carries

```json
"_meta": { "kivali": { "agent": "dana", "access": { "blogs": ["Marketing"] } } }
```

`access` is the agent's filled access form, `{}` for an integration
without one. Core's own requests carry no `_meta.kivali`. An
integration that holds an agent to its access reads it here: which
tools to list, what a call may touch.

### When a grant lands

The chat turn's request (`spawnChatLoopIfIdle`, `internal/web/chat.go`)
carries the agent's actions after Kivali's own tools, each marked
`provider.Tool.Integration`, and they ride into the CLI's
`--allowedTools`. The runner keeps the sorted set it was spawned with
(`internal/claudeagent/runner.go`), and a turn that carries another set
is spawn-time drift: the runner respawns with `--resume`, the new CLI
starts a new `kivali mcp`, and that lists the actions from core once,
when it starts (`POST /v1/agent/{slug}/integrations/tools`; a failure,
or no answer within 10 s, starts it with none, and the model is shown
none until the CLI next respawns). There is no mid-turn
`tools/list_changed`:
access given lands at the agent's next turn. Access taken away is
refused from the next call, since core checks every call, and its
tools leave the agent's list at its next turn.

### The routes

On `core.sock`, bound to the slug header like every agent route:

| Route | Body | Answer |
| --- | --- | --- |
| `POST /v1/agent/{slug}/integrations/tools` | `{}` | `agentpod.IntegrationToolsResponse`: `{tools: [{name, description, input_schema}]}` |
| `POST /v1/agent/{slug}/integrations/call` | `agentpod.IntegrationCallRequest`: `{name, arguments, shell_job_result}`, the prefixed name; `shell_job_result` only on a call's second request ([The git built-in](#the-git-built-in)) | `agentpod.IntegrationCallResponse`: `{content: [text, ...], is_error, shell_job}`; `shell_job` set, it is not yet the answer |
| `POST /v1/agent/{slug}/kivali-developer/dispatch` | `agentpod.KivaliDeveloperRequest`: `{tool, input, smoke_result}` | `agentpod.KivaliDeveloperResponse`: `{body, is_error, smoke}` |

A refusal or the integration's error is a 200 with `is_error`; a
non-2xx answer is a transport error, which the MCP tool reports as
"The call did not reach Kivali: ...".

### A call

Core matches the prefixed name against what the agent may call now,
worked out again from the store and the integration's list for the
agent, never from anything else in the request. Each integration whose
prefix the name starts with is tried, the longest prefix first. A
match goes to the integration as `tools/call` with `{name, arguments,
_meta}` (to a built-in's `Call`, with the agent, its access and the
connection) and waits up to 60 s, which the MCP server's context bounds
too. Without a match the call is refused with the first cause found,
in words that name the cause and the owner and nothing else:

| Cause | The agent reads |
| --- | --- |
| No approved version | The <integration> integration has no approved version. Ask the owner if your work needs it. |
| Switched off | The <integration> integration is switched off. Ask the owner if your work needs it. |
| No access | You have no access to the <integration> integration. Ask the owner if your work needs it. |
| Access that no longer applies | Your access to the <integration> integration no longer applies (why). Ask the owner to set it again. |
| Not running | The <integration> integration is not running right now; try again later or tell the owner. |
| An action the agent's list does not have | Your access to the <integration> integration does not include <action>. Ask the owner if your work needs it. |
| A built-in integration with no connection | The <integration> integration has no connection yet. Ask the owner to connect it. |
| A shell job's result with a call whose integration asks for no job | The <integration> integration asked your shell for nothing in this call, so its result was not read. Call the action again. |
| A built-in's own refusal (`builtinintegrations.Refusal`) | The refusal's `Text`, audited with its `Reason`: for Git, see [The git built-in](#the-git-built-in) |

A name that starts with no integration's prefix is answered "No
integration has an action named <name>." and has no audit row.

### The answer

Each `text` block of the result is one content string; a result with
none passes its `structuredContent` as JSON text. `isError` is the
answer's `is_error`. Every other block (an image, audio, a resource)
is left out in this version, and a last line names what was: "The
answer also held content Kivali does not pass on (image)." A JSON-RPC
error is an error with its message; no answer within 60 s, or a shim
that goes before it answers, is an error that says so. Every string
goes through the connection's secret scrubber, each secret value
becoming `[secret]` as in the log, before it reaches the agent; when
the connection's secrets cannot be read, the answer is held back. A
built-in's answer is read the same way, as the result a server would
send: each content string a text block, its `IsError` the result's,
an error of its function's a JSON-RPC error with the error's words.

### The audit

Every call whose name matches an integration, allowed or refused,
appends a row to that integration's `audit.jsonl`: `{at, agent,
action, allowed, reason, duration_ms}`, the action without the prefix.
A refused call is `allowed: false` with the cause as its reason (`no
access`, `the integration is off`, `the agent's access does not
include it`, ...). An allowed call that failed keeps the
integration's words, scrubbed, as its reason. A call that runs a job
in the agent's shell ([The git built-in](#the-git-built-in)) appends
two rows: one when core hands the job out, with the built-in's
`Result.Audit` as its reason (`started: acme/widgets`), and one when
the second request ends it, with how it ended; a second request that
never comes leaves the first row alone. The owner's own calls
are rows for `ceo`, and so is a roll back, with the action `rollback`
and the versions as its reason ([Rolling back](#rolling-back)), which
the page shows as "Roll back" and "From version 1.1.0 to version
1.0.0".

## Built-in integrations

A built-in integration ships in the binary
(`internal/builtinintegrations`). It has a bundle's contract surface,
a manifest of the same shape (name, display_name, description,
user_config, tools, `kivali.connection`, `kivali.access`,
`kivali.verify`), so the Org page, the install screen, the connection
form and the access form are drawn by the same code and every agent's
access is checked by the same core. Its actions are Go functions core
calls directly: no pod, no shim, no Python.

### The interface

```go
type Integration interface {
	Manifest() *integration.Manifest
	Tools(ctx context.Context, caller Caller) ([]Tool, error)
	Call(ctx context.Context, caller Caller, conn Connection, name string, args json.RawMessage) (*Result, error)
	Verify(ctx context.Context, conn Connection) (message string, err error)
	Choices(ctx context.Context, conn Connection, name string) ([]string, error)
}

type Caller struct{ Agent string; Access map[string]any }
type Connection struct{ ID string; Fields map[string]string }
type Tool struct{ Name, Description string; InputSchema json.RawMessage }
type Result struct {
	Content  []string
	IsError  bool
	Audit    string        // the audit row's reason; empty: the failure's words
	ShellJob *shelljob.Job // work for the agent's shell (ShellJobIntegration)
}

// Optional capabilities.
type ShellJobIntegration interface {
	Integration
	Complete(ctx context.Context, caller Caller, conn Connection, name string, args json.RawMessage, result shelljob.Result) (*Result, error)
}
type HostsOf interface{ Hosts(conn Connection) []string }
type ConnectionChecker interface{ CheckConnection(conn Connection) error }
type AccessChecker interface{ CheckAccess(access map[string]any) error }

// An error of Call or Complete that refuses the call before anything ran.
type Refusal struct{ Text, Reason string }
```

- `Manifest` is the manifest, held to `integration.ValidateBuiltin`
  ([The check](#the-check)): no server section and no `kivali.hosts`.
  A built-in runs inside Kivali, so its network reach is Kivali's own,
  the hosts its connection names, not a host list on its install
  screen. Its `version` is the built-in's own, semantic, bumped only
  when the manifest's surface changes. It is called on every lookup,
  concurrently, so it must be cheap and safe to call from many
  goroutines.
- `Tools` is the actions an agent is listed, unprefixed, for its
  access. Core keeps only the ones the manifest declares, as it does a
  pod's `tools/list`, and may be given fewer than the manifest
  declares.
- `Call` runs one action for the agent with the model's arguments. A
  failure the agent should read is a `Result` with `IsError`; an error
  is the function's own failure.
- `Verify` is Test connection: a nil error with what it found, or an
  error in words for the owner.
- `Choices` lists an access field's options: `name` is what
  `kivali.access.choices` maps the field to, which need not be a tool.
- `Connection.Fields` is every connection field by key, secrets
  included, read from the record and its Secret at the moment of the
  call and kept by no one. The connection's proxy credential is not
  among them.
- A `Refusal` from `Call` or `Complete` is audited as not allowed, with
  its `Reason`, and the agent reads its `Text`; any other error is the
  function's own failure, as above. `Result.Audit`, when set, is the
  audit row's reason in place of the failure's words.
- A `ShellJobIntegration` runs part of an action in the agent's shell:
  its `Call` answers with `Result.ShellJob` (and `Result.Audit`, the
  reason of the row core writes as it hands the job out), and
  `Complete` puts the shell's result in words on the call's second
  request ([The git built-in](#the-git-built-in)).
- A `HostsOf` built-in's shell jobs reach the network through the
  egress proxy: each of its connections gets a scoped proxy credential
  admitted to `Hosts(conn)` and nothing else.
- A `ConnectionChecker` holds a connection to more than its form:
  `PUT .../connection` calls `CheckConnection` after the form's own
  checks and before it stores anything, and answers 400 with the
  error's words. An `AccessChecker` does the same for
  `PUT .../access/{slug}`, after `integration.ValidateAccess`.

`Tools`, `Call`, `Verify` and `Choices` run concurrently: many agents
call at once, and each agent's list is asked for in parallel with the
other integrations'. One still running at its deadline (below) is
abandoned and its answer dropped; its context ends, and it must honour
that and stop.

`builtins.All()` (`internal/builtinintegrations/builtins`) is the
built-ins compiled into the binary: Git. Each built-in is a package of
its own that imports `builtinintegrations` for its types, so the list
lives in a package apart. `Server.BuiltinIntegrations` is the ones core
calls (`builtins.All()` unless a test sets others).
`internal/builtinintegrations/echotest` is the built-in the tests
register; it is never in `All()`. A test holds every built-in in
`All()` to the check, to a name of its own, and to a `Tools` list
inside its manifest.

### Registration at boot

`store.InstallBuiltinIntegrations(manifests, now)` runs at every boot,
right after the built-in skills (`main.go`), and after a restore
(`internal/web/backup.go`). For each built-in it writes:

- `integration.yaml` with `builtin: true`: a new record is switched on,
  installed by `kivali`; an existing one keeps the owner's switch.
- The manifest's version, `approved`, published by `kivali`, notes
  "Ships with Kivali.", with no `bundle/` directory. Its
  `manifest.json` is the binary's, written again whenever it differs.
- When the version differs from the one in use, the one in use becomes
  superseded and the binary's is the one in use, as an approval does.
  The binary is the truth, so an older binary makes its older version
  the one in use again.

An installed integration of the same name that is not built in is left
as it is, and the built-in is not installed; boot logs it. A backup
carries the records like any integration's, and the restore writes the
binary's versions over them.

A built-in record whose code the binary does not carry (another
version of Kivali shipped it) is left as it is: nothing runs it, its
status is not included, agents' calls of it are refused as not
running, and Test connection and its options answer 409 "<integration>
is not in this version of Kivali, so nothing runs it; you can remove
it". The owner may remove it (`store.RemoveStaleBuiltinIntegration`,
which the API uses only for such a record).

The owner cannot approve, reject or roll back to a built-in's version:
the store refuses (`store.ErrIntegrationBuiltin`) and the API answers
409 "<integration> ships with Kivali, and its version changes only with
Kivali's updates; there is nothing to approve" (to reject, to roll
back to). Remove is a 409 ("... ships with Kivali and cannot be
removed; switch it off instead"), and so is an upload ("... ships with
Kivali, so a bundle cannot replace it") or a publish under its name.
Every one of these names the integration by its display name
(`builtinRefusal`, `internal/web/api_integrations.go`).

### Dispatch

A built-in is an integration whose record is built in and whose name
is in `Server.BuiltinIntegrations` (`Server.builtinFor`). Where core
would reach a pod, it calls the function instead
(`internal/web/integration_builtin.go`):

| Where | A bundle | A built-in |
| --- | --- | --- |
| Running | its shim is connected | while its code is in the binary |
| An agent's list | `tools/list` with `_meta`, kept on the link | `Tools(caller)`, every time |
| An agent's call | `tools/call` through the shim | `Call(caller, connection, name, args)` |
| Test connection | `tools/call` of `kivali.verify` | `Verify(connection)`; the audit's action is the name `kivali.verify` gives (`whoami` for echo) |
| An access field's options | `tools/call` of the field's tool | `Choices(connection, name)`; 409 with no connection |
| Work in the agent's shell | none | `Call` answers with `ShellJob`, `Complete` the call's second request |
| A connection change | the shim restarts the server | nothing to restart; a `HostsOf` built-in's proxy credential is registered again |
| Reconcile | a pod while it should run | skipped |

Each function runs under the same deadline a pod's request has (60 s
for a call, Test connection and options, 20 s for a list), on core's
clock; past it the function's context ends and the answer says the
integration did not answer in time. A function that panics fails with
an error. Every answer is read as the MCP result a server would send,
so the agent, the scrubber and the audit see what they see for a pod.

### What the owner sees

The list and the page show a "Built in" badge and no Remove. The
status is connected, not connected or off: never pending, never not
running. Test connection works once there is a connection. The
version is the built-in's manifest's, published by Kivali; the
install screen (`IntegrationVersion.Builtin`) says it ships with
Kivali, that it reaches the hosts its connection names, shows no
bundle and no "Replaces version" line or "What changes from version"
block, and offers no Approve, Reject or Roll back; the page's version
list offers no Roll back. A built-in record this binary does not carry
shows "Not in this version of Kivali" (`IntegrationStatusNotIncluded`),
says agents cannot use it, and offers Remove.

### The git built-in

Git (`internal/builtinintegrations/gitintegration`, name `git`,
version 1.0.0) lets agents clone and fetch repositories over HTTPS with
the owner's token. Everything local is plain git in the agent's shell,
which reaches only the team's allow list. Its manifest:

- `user_config`: `host` (required; "github.com, gitlab.com or any HTTPS
  git host, without https://"), `token` (sensitive, required) and
  `test_repository` (optional, owner/name).
- `kivali.connection.method: token`, no `kivali.hosts`,
  `kivali.verify: "ls_remote"` (a name for `Verify`, not a tool).
- `kivali.access.schema`: one required list field, `repositories`
  ("owner/name, or a pattern such as acme/*").
- `tools`: `clone` and `fetch`, which agents call as `git_clone(url,
  path)` and `git_fetch(path, url?, refs?)`.

#### Repositories and access

A repository is its path on the connection's host, `owner/name`, or
more segments where GitLab nests groups (`group/sub/project`)
(`internal/gitrepo`, used by core and by the daemon alike). The host
is the connection's, made exact (`gitrepo.NormalizeHost`: spaces, an
`https://` in front and slashes behind dropped, lower case; a host
name or address with an optional port). A call names the repository by
URL or by `owner/name`:

- A URL must be `https://<the connection's host>/<path>`, with an
  optional `.git`, and no user info, query or fragment. Another scheme
  is refused (SSH among them), and so is another host, a port included
  ("the connection is for <host>").
- Anything without `://` is `owner/name`, used as it is.
- The path loses slashes around it and `.git`; each segment is letters,
  digits, `.`, `_` and `-`, never `.` or `..`, and there are at least
  two. It keeps its case in the URL core builds,
  `https://<host>/<path>.git`, and is matched in lower case.

The access's `repositories` are patterns matched against the path with
`path.Match`: `*` and `?` stand within one segment and never across a
`/`, case is ignored, and an entry with neither is the one repository it
names (`gitintegration/access.go`). An access whose `repositories` is
missing or empty, or holds anything but valid patterns, refuses every
repository.

#### A call, in two requests

Core has no shell of the agent's, so a clone or a fetch takes two
requests on `core.sock`, as the smoke test does
([The smoke test](#the-smoke-test)):

1. The agent's MCP server sends the call (`IntegrationCallRequest`).
   Core works it out as any call ([A call](#a-call)) and calls
   `Git.Call`, which checks the connection (a host, a token), the
   access, the arguments and the repository, and answers with a
   `builtinintegrations.Refusal` or a shell job. Core puts the
   connection's proxy credential in the job, writes an audit row
   (allowed, the action, `started: <owner/name>`, or `started: the
   working copy's origin at <path>` for a fetch without a `url`) and
   answers `IntegrationCallResponse{shell_job}`. A refusal is audited and
   answered at once.
2. The MCP server converts the job to an `agentshell.GitRequest` and has
   the agent-shell daemon run it (`SidecarShell.Git`, `POST /v1/git`),
   waiting the operation's own bound and 30 s more, as `SidecarShell.Exec`
   does for a command. A server with no sidecar (`KIVALI_MCP_LOCAL_FILES`)
   sends `skipped` ("the agent has no shell to run git in").
3. It sends the call again: the same name and arguments, with
   `shell_job_result`. Core works out the integration, the agent's
   access and the connection again exactly as on the first request, and
   `Git.Complete` checks the call again (access taken away while the job
   ran refuses it) and puts the result in words. That writes the call's
   second audit row: allowed, the action `clone` or `fetch`, and the
   reason `<owner/name>: updated <refs>` (at most ten named, then how
   many more), `<owner/name>: up to date`, or the repository and why it
   failed. The row counts from the job's start (the result's
   `duration_ms`, at most 15 minutes). A second request that never
   comes leaves the first row, so every job that carried the token out
   is in the audit.

The result is untrusted text from the agent's pod: it becomes only the
agent's answer and the audit's reason, both scrubbed of the
connection's secrets and its proxy credential, and nothing in core acts
on it, so no nonce binds it to the job. For a fetch without a `url` the
repository named is the one the daemon reports. The deadlines are per
leg: each request is a call under the 60 s `integrationCallTimeout`;
the job runs in between under its own bound. A result sent with a call
whose integration asks for no job is refused and audited. The MCP
server never runs a second job in one call.

The wire shapes (`internal/shelljob`, named in `agentpod` as
`IntegrationShellJob`, `GitJob` and the rest):

```go
type IntegrationShellJob struct{ Git *GitJob }             // json: git
type GitJob struct {
	Op, Path, URL       string   // op: clone or fetch; url empty: the working copy's origin
	RefSpecs            []string // json: refspecs; a fetch's
	Force               bool     // a ref may move other than fast-forward
	AllowedRepositories []string // the access's patterns, for the daemon's own check
	Host                string   // the connection's, made exact
	Auth                GitAuth  // {username: x-access-token, password: the token}
	Proxy               GitProxy // {username: integration-git.<id>, password: the secret}
	TimeoutSeconds      int      // clone 900, fetch 600
}
type IntegrationShellJobResult struct{ Skipped string; Git *GitResult }
type GitResult struct {
	Repository   string
	Updated      []GitRefUpdate // {ref, old, new}; old empty for a new ref; at most 200
	UpdatedTotal int            // json: updated_total; every ref that moved
	Error        string         // at most 4 KiB
	DurationMs   int64
	TimedOut     bool
}
```

`refspecs` and `force` are carried for a push; clone and fetch set no
`force`. Nothing on the job's path logs it: not core, not the MCP
server, not the daemon.

Git's refusals name the cause and the owner only:

| Cause | The agent reads | The audit's reason |
| --- | --- | --- |
| A URL on another host | The Git integration's connection is for <host>; give a repository's URL on it, or owner/name. | the URL is on another host than the connection's |
| A repository that does not read | Kivali cannot read the repository: <why>. Give its HTTPS URL on <host>, or owner/name. | the repository does not read |
| Outside the access | The Git integration's access for you does not include <owner/name>. Ask the owner if your work needs it. | the access does not include <owner/name> |
| An access that names no repositories | Your access to the Git integration names no repositories Kivali can read. Ask the owner to set it again. | the access names no repositories |
| A connection host that is no host | The Git integration's connection has no host Kivali can use. Ask the owner to fix the connection. | the connection's host is not a host name |
| No token | The Git integration's connection has no token. Ask the owner to connect it again. | the connection has no token |
| Arguments the action does not take | git_clone takes url and path, both strings. (git_fetch: path, and optionally url and refs) | the arguments do not read |

The answers: "Cloned <owner/name> into <path>. Its origin is <url>; use
plain git there for everything local, and git_fetch to fetch with the
connection." or "Fetched <owner/name> into <path>.", then the refs that
moved (at most 50, `<ref> <old>..<new>` or `<ref> new at <new>`) or
"Nothing changed: the working copy was up to date." An error answer
says nothing ran for want of a shell, that the operation did not finish
in time and was stopped, or that it did not go through, with the host's
or the daemon's words, cut to 1000 bytes.

#### The daemon's `POST /v1/git`

`internal/agentshell/git.go` runs the operation in the daemon's own
process with go-git v5 (`github.com/go-git/go-git/v5`). The body is a
`GitRequest` (64 KiB at most), the answer a `GitResponse`
(`{repository, updated, updated_total, error, duration_ms,
timed_out}`). The answer is bounded so it always fits the agent's
next request to core, whose body is capped at 16 MiB: `updated` lists
at most 200 refs (`GitUpdatedMax`), by name, `updated_total` counts
every ref that moved, and `error` is cut to 4 KiB (`GitErrorMaxBytes`)
after it is scrubbed. The bound is the request's `timeout_seconds`, at
most 15 minutes for a clone and 10 for a fetch, 5 when it gives none,
on the daemon's clock; past it the operation's context ends and the
answer is `timed_out`. Jobs on one working copy run one after the
other, by its resolved path (two fetches into one `.git` at once could
leave its refs half written); the wait counts against the bound.

- **Where.** `path` is absolute under the daemon's root (`/files`) or
  its HOME (`/scratch/home`), `~/...` for the home, or relative to the
  root, as a command's cwd is; the root and the home themselves are
  refused. The existing part of the path is resolved through its
  symbolic links and must stay under the same base. A clone needs a
  directory that does not exist or is empty; when the clone fails,
  go-git removes that directory if it made it, or empties it if it was
  there, and leaves any parent directories it made; a fetch needs a working copy whose `.git`
  is a directory (a `gitdir:` file or a link is refused).
- **What it reaches.** A clone's URL is the job's, parsed by the same
  rules on the job's host, and must be one of `allowed_repositories`.
  The clone's origin is that URL, with no user info, so plain git keeps
  working for a repository the team's allow list reaches. A fetch's URL
  is the job's, by the same rules, or, with none, the working copy's
  origin as written in `.git/config` (read raw, so no
  `url.<base>.insteadOf` rewrites it; exactly one URL), parsed by the
  same rules and refused unless it is on the job's host and one of
  `allowed_repositories`. `.git/config` is the agent's to write, so
  nothing else in it is read. Every request goes to that URL: go-git's
  http and https transports are installed with
  `githttp.NoFollowRedirects` (`client.InstallProtocol`, in the
  daemon's `init`), so a host that answers with a redirect (GitHub does
  for a renamed or transferred repository) fails the operation, and the
  agent reads "the repository moved; ask the owner to update the
  access". The tests serve a 301 from one repository's path to another
  and see nothing reach the other.
- **How it fetches.** Through an anonymous remote
  (`CreateRemoteAnonymous` with the URL in code, and `RemoteName:
  "anonymous"` on the fetch). The daemon's tests pin, against a
  poisoned working copy, that through the anonymous remote the
  repository's own `insteadOf` rule and `[remote "origin"]` URL are
  inert, each with a control: a fetch through the named remote follows
  the rule to the other server when origin is the good one, and follows
  origin when origin names the other server. They also assert that a
  global `~/.gitconfig` rule to the other server has no effect on a
  clone or a fetch (go-git reads no global configuration here) and that
  no hook runs. A named remote (`RemoteName: "origin"` without
  `RemoteURL`) follows the configuration: the daemon never uses one. A
  push through an anonymous remote needs `PushOptions.RemoteName:
  "anonymous"`.
- **Refspecs.** None fetches `+refs/heads/*:refs/remotes/origin/*`.
  Each given must parse (`config.RefSpec.Validate`), have a source, and
  write under `refs/remotes/origin/` or `refs/tags/`, cleanly (no `..`),
  so a fetch never moves a local branch. Tags are fetched
  (`git.AllTags`). Already up to date is success with nothing moved. The
  refs that moved are worked out from every ref before and after.
- **The proxy.** Always given in code (`ProxyOptions`): the daemon's own
  bare proxy URL (the first of `HTTPS_PROXY`, `https_proxy`,
  `HTTP_PROXY`, `http_proxy` in its environment, without user info) with
  the job's credential, so go-git never reads a proxy from the
  environment. With none in the environment (tests, a laptop) it
  connects directly.
- **Credentials.** The token is basic auth (`x-access-token` and the
  token, what GitHub expects of a personal access token; GitLab takes
  any user). It and the proxy credential live in the request for the
  one operation, are never written to disk (`.git/config` holds neither,
  which the tests check) and are scrubbed from every string of the
  answer: the token, `user:token` in base64 and their URL-escaped forms
  become `[token]`; the proxy secret, `user:secret` in base64, the
  credentialed proxy URL and their URL-escaped forms become `[proxy
  credential]` (`agentshell.GitScrubber`). The MCP server scrubs its
  own transport error the same way before it sends it to core.
- **Whose files.** The daemon runs it with its filesystem uid, 65532, so
  a clone is owned as every file the shell writes is.
- **When it runs nothing.** A daemon that did not drop to its own user
  (a command could then read its memory), or could not set the sticky
  bit on its socket's directory (a command could then stand in for the
  daemon), answers every request with why
  (`ServerConfig.GitUnavailable`, [architecture.md](architecture.md#the-agent-shell-daemons-user)).

`ServerConfig.GitInsecureHTTP` lets the tests' git server, `git
http-backend` under `net/http/cgi` on `httptest`, be reached over
`http://`; nothing else sets it, and anything but `https` is refused.

#### The connection's proxy credential

Git implements `builtinintegrations.HostsOf`: its connection reaches
its host (without the port). Each connection of such a built-in has a
scoped proxy credential of its own (`internal/web/integration_builtin_egress.go`):

- A 32-byte secret (`egresscred.NewSecret`) in the connection's Secret
  under `kivali-proxy-secret` (`builtinintegrations.ProxySecretKey`).
  That key has a hyphen, so it is never a `user_config` key; it is not
  in the record's `secret_keys`, the fields the owner is shown, or
  `Connection.Fields`. Core alone puts it in a job.
- Its SHA-256 is registered as the scoped proxy user
  `integration-git.<connection id>` (`builtinintegrations.ProxyUser`)
  with the built-in's hosts (`store.SetEgressScopedCredential`). The
  proxy admits that user to those hosts alone and counts nothing; the
  team's allow list is not touched.
- Saving the connection (`PUT .../connection`) mints the secret when the
  Secret holds none, keeps it otherwise, and registers it on every save
  while the integration is on, so a changed host is registered at once.
  Deleting the connection, switching the integration off and removing it
  drop it (`DropEgressScopedCredential`); switching it on registers it
  again. The switch takes the same lock (`lockIntegrations`) as the
  save, so a save racing a switch registers or drops the credential by
  the switch as it ends up. Every boot (`Server.RegisterBuiltinEgress`
  in `main.go`) and every restore registers each switched-on built-in's
  connection again from its Secret, since `egress_agents.json` is out
  of backups and the Secret is not, and drops the credentials of those
  switched off. A job whose connection's Secret holds no secret mints
  and registers one first.

#### Checks at save

Git is a `ConnectionChecker` and an `AccessChecker`. Saving a
connection whose host `gitrepo.NormalizeHost` cannot read (a path, user
info, a scheme other than `https://`) is a 400, "Host is a host name
such as github.com, without https:// or a path", and nothing is
stored. Saving an access with a `repositories` entry that is not a
`path.Match` pattern is a 400, "<entry> is not a repository or a
pattern". A stored row that is not this all the same (one written
before the check, or by hand) refuses every repository.

#### Test connection

`Git.Verify` runs in core, which reaches the host directly (the
server's container has no egress proxy). It first resolves the
connection's host and refuses, before any request, a host with any
address that is loopback, private (RFC 1918, fc00::/7), link-local
(169.254.0.0/16, fe80::/10), unspecified or multicast, an IP-literal
host the same way: "Test connection reaches only public hosts."
(`gitrepo.PublicAddresses`; `Git.LookupIP` is the test hook for the
resolver). go-git follows no redirect here either (`gitintegration`'s
`init` installs the same transports as the daemon): a repository that
moved fails with words saying so. With `test_repository` set it
lists the repository in memory with the token
(`git.NewRemote(memory.NewStorage(), ...)` and `ListContext` with basic
auth) and answers "Connected. <n> branches in <owner/name>." Without it,
for `github.com` it asks `https://api.github.com/user` with
`Authorization: Bearer <token>` ("Connected to github.com as <login>.")
and for `gitlab.com` `https://gitlab.com/api/v4/user` with
`PRIVATE-TOKEN` ("Connected to gitlab.com as <username>."); any other
host is the error "Name a repository for Test connection: this host
has no account endpoint Kivali knows." A failure is the host's words
with the token taken out (`[token]`). The `Git` struct's fields are
test hooks (`InsecureHTTP`, `HTTPClient`, `GitHubAPI`, `GitLabAPI`,
`LookupIP`); the tests never reach a real host.

Not supported: SSH remotes, Git LFS (a clone holds the pointers),
submodules (not cloned), and pushing through the integration.

## The Kivali Developer

The agent that builds integrations: it writes a bundle in its own
`/files/`, checks it, and publishes it as a pending version for the
owner to approve. It never sees a connection's secrets and calls an
installed integration only as any agent with access does.

### The agent

Every set-up team has one, slug `kivali-developer`
(`agent.KivaliDeveloperSlug`, `graph.KivaliDeveloperSlug`,
`message.KivaliDeveloper`), role "Kivali Developer", icon `code`,
reporting to `chief-of-staff`, with Kivali's role
(`seed.KivaliDeveloperRole`, `internal/seed/kivali_developer_role.md`).
`Server.EnsureKivaliDeveloper` (`internal/web/onboarding.go`) hires it
on a team that has a Chief of Staff and no Kivali Developer, on the
model the Chief of Staff is hired on: setup calls it right after
hiring the Chief of Staff and provisions both pods; boot (`main.go`,
before the files sync and `BulkProvisionAgentPods`) and a restore call
it for a team that has none. `propose_hire` has no way to hire it:
its slug is reserved, like `ceo` and `chief-of-staff`.

It stays and always reports to the Chief of Staff: `propose_offboard`
and `propose_reorg` refuse it at parse, and `ApplyOffboard` and
`applyReorgMove` again at apply. Its role is Kivali's, as the Chief of
Staff's is: see [Kivali's roles](architecture.md#kivalis-roles).
`IsFreshForRestore` counts it as seed state while its chat is empty.

The tools are its by slug, as the Chief of Staff's are: its MCP server
(`mcp_cmd.go`) registers them when `--agent` is `kivali-developer`, and
core refuses every request whose slug header is any other agent's. The
CLI's `--allowedTools` lists the five tools for every agent
(`claudeagent.allowedToolNames`): only the Kivali Developer's MCP
server registers them.

### The skill

The role says who the Kivali Developer is, what it owns, how it shapes
an integration and what it never does. How to build one is the
built-in skill `build-an-integration`
(`internal/builtinskills/skills/build-an-integration/`), which the role
points at and every agent can list. It holds:

- `SKILL.md`: the steps, the bundle and its manifest, and the parts of
  this page a bundle's author meets and the check cannot show: the
  pod's environment and network, the line protocol as the shim holds
  it, the shape of `_meta.kivali.access`, what reaches an agent from an
  answer, and how Test connection and an access field's options read
  theirs.
- `template/`: a bundle for an imaginary notes service, standard
  library only, which the developer copies to start from. Its server
  keeps the output for JSON-RPC (descriptor 1 becomes the error
  output), answers each `tools/call` on a thread, reaches the service
  with `urllib.request` through the proxy, lists each agent the actions
  its access allows, and starts with no connection.
- `scripts/try.py`: runs a bundle's server as the pod does (no proxy
  variable, so only a stub on the shell's own machine answers) and
  sends what core sends, as the owner or as one agent with one access:
  the `tools/list` at start, then `list`, `call`, `verify` and
  `choices`. It answers `kivali/credential` and `kivali/log` as core
  does, refuses an action missing from the agent's list as core does,
  shows each answer as the agent or the owner reads it, and reports
  what the check does not see: a line that is not JSON-RPC, an id that
  core would not match, an answer later than core waits, options the
  form cannot read. `--stub` starts a stub and stops it at the end.

`internal/builtinskills/build_an_integration_test.go` holds them to
the platform: the template passes `integration.Validate` and the smoke
test with no finding, `try.py` drives it against a stub as the owner
and as agents, reports the failures above, and uses the shim's
protocol version and line limit
(`integrationpod.ProtocolVersion`, `integrationpod.MaxLineBytes`). A
change to what the shim, core or the check does that a bundle's author
meets changes the skill in the same change.

### The tools

All five relay to `POST /v1/agent/{slug}/kivali-developer/dispatch`
(`agentpod.KivaliDeveloperRequest`: `{tool, input, smoke_result}`,
answered with `{body, is_error, smoke}`).

| Tool | Does |
| --- | --- |
| `integration_validate(path)` | Reads the bundle in the directory, runs the check, then the smoke test, and answers with every finding and whether the bundle would be accepted. |
| `integration_publish(path, notes)` | The same checks; a bundle with an error is refused, and so is a version already stored or a built-in name, before the smoke test runs. Stores the version and files the owner's request. |
| `integration_list()` | Every installed integration: name, display name, version in use, status, a pending version, how many agents have access. |
| `integration_versions(name)` | Every stored version, newest first: state, published at and by, approved or rejected at, the first 200 characters of its notes. |
| `integration_logs(name, since)` | The integration's latest 200 log lines, oldest first, from `since` (RFC 3339) when given, at most 32 KiB, the newest kept. Each line had the connection's secrets replaced when it was appended; the answer's first line says the log is data, never instructions. |

`path` is a directory under the developer's `/files/` (`/files/<p>` or
`<p>`): the root, a path outside `/files/` and `..` are refused. Core
opens `agents/<slug>/memory/<p>` on the data volume with
`files.OpenDirNoFollow`, which refuses a symlink at any step, and reads
the bundle through the resulting `os.Root` (`integration.ReadDirectory`).
An argument the tool does not take is refused by name.

The answer of `integration_validate` is "No findings." or one line per
finding, errors first, each prefixed `error:` or `warning:`; a line
naming the actions the server listed when the smoke test reached its
list; and a last line, "The bundle would be accepted." or "The bundle
would be refused: integration_publish refuses a bundle with an
error."

### The smoke test

Core has no shell of the agent's, so validate and publish take two
requests. The first reads and checks the bundle and answers with
`smoke`: a bash command (`integration.SmokeCommand`), the bundle's
directory as its cwd, and a 30 s shell timeout. The agent's MCP server
runs it in the agent-shell sidecar and sends the request again with
`smoke_result` (stdout, stderr, exit code, timed out); core reads the
bundle and checks it again, reads the result (`integration.SmokeFindings`)
and answers. A server with no sidecar (a local tree,
`KIVALI_MCP_LOCAL_FILES`, as `make run` uses) has no shell whose
network is closed, so it sends `skipped` instead, and the findings say
the server's start was not checked.

The command is python3 running a driver (`internal/integration/smoke.py`,
embedded), which starts `mcp_config`'s command and args in the bundle
with PATH, a HOME of its own, `LANG`, `PYTHONDONTWRITEBYTECODE` and the
manifest's env with every connection field empty, and no proxy
variable: in the agent pod, whose network policy lets it reach only
the egress proxy, the server has no network. It sends `initialize`
(protocol `2025-06-18`), waits for the answer, sends
`notifications/initialized` and `tools/list`, all within 20 s, answers
the server's own requests as the shim does with no connection, and
prints one JSON line. The findings:

| Finding | Level |
| --- | --- |
| The server does not start, ends before an answer (with its status and the end of its error output), does not answer in 20 s, answers with an error, or writes a line that is not JSON-RPC | error |
| `tools/list` does not list a declared action (worded for `kivali.verify`'s and `kivali.access.choices`' tools) | error |
| `tools/list` lists an action the manifest does not declare, or gives a declared one an input schema that is not an object's | warning |
| The check could not run, or did not finish in 30 s | warning |

It checks that the server starts and lists the actions its manifest
declares, with no `_meta.kivali`, as core's own `tools/list` at every
connect asks. It is not a review of what the code does.

### Publishing

A publish needs `notes`, at most 4096 bytes, which become the version's
`notes_md` and the request's body. With no error, core stores the
bundle (`store.StoreIntegrationVersion`) as pending, published by the
developer, with the check's and the smoke test's warnings as its
findings, and files a `ceo_approval_request` from the developer: title
"Install <display name> <version>", body the notes,
`integration_install: {name, version}`, routed by `Messenger.Route`
into the owner's inbox and Needs you. From there it is answered as
[Installing a version](#installing-a-version) says, and the developer
reads the answer in its chat. A version stored whose request could not
be filed is said so to the developer, and waits on its install screen.

## The API

Wire types in `internal/web/apitypes/integrations.go`; every route is
the owner's.

| Route | Answer |
| --- | --- |
| `GET /api/v1/org/integrations` | `Integrations`, by display name. Status: not included (a built-in integration this binary does not carry), pending (no approved version), off, not running (approved and on, with no shim connected; a built-in integration is never not running while its code is in the binary), and, while it runs, connected (a connection whose secrets are all there) or not connected. |
| `POST /api/v1/org/integrations` | multipart `file`: 201 `IntegrationVersion`; 400 `IntegrationUploadRefused`; 409 for a version already stored or a built-in name. |
| `GET /api/v1/org/integrations/{name}` | `IntegrationPage`. |
| `POST .../{name}/enable`, `.../disable` | `IntegrationPage`. |
| `DELETE .../{name}` | 204; 409 for a built-in one this binary carries. |
| `GET .../versions/{version}` | `IntegrationVersion`, with `changes` from the version in use, and `builtin` for a built-in integration's version. |
| `POST .../versions/{version}/approve`, `.../reject` | `IntegrationVersion`; 409 when not pending, or for a built-in integration. |
| `POST .../versions/{version}/rollback` | `IntegrationVersion`; 409 when not superseded, or for a built-in integration; 404 for a version not stored. |
| `PUT .../connection` | `ConnectionWrite` in, `IntegrationPage` out; 409 while no version is approved. |
| `DELETE .../connection` | `IntegrationPage`: the Secret, the record and every agent's access removed. |
| `POST .../connection/verify` | `IntegrationPage`, with the connection's verified, verify message and verified at; 409 with no approved version, no connection, no `kivali.verify`, while the integration is not running, or for a built-in one this binary does not carry. |
| `PUT .../access/{slug}` | `AccessWrite` in, `IntegrationPage` out; 404 for an agent not on the team; 409 without a connection; 400 with the field's message. |
| `DELETE .../access/{slug}` | `IntegrationPage`. |
| `GET .../audit?limit=` | `IntegrationAudit`, newest first; limit 100 by default, at most 1000. |
| `GET .../choices/{key}` | `AccessChoices`; 404 for a field that lists no options; 409 while the integration is not running, for a built-in one without a connection, or for a built-in one this binary does not carry; 502 for the tool's error, or a result that is not a list of names. |
| `GET .../logs?limit=` | `IntegrationLogs`, oldest first; limit 200 by default, at most 2000. |
| `GET /api/v1/agents/{slug}/integrations` | `AgentIntegrations`; 404 for an agent not on the team. |
