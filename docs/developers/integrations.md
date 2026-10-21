# Integrations

An integration is an external service agents act on, such as a CRM or
a blog, installed from a bundle. This page is the developer's
reference: the bundle and its manifest, the check every bundle passes,
the access form, where Kivali keeps an integration and its connection,
and how a version is approved. What the owner sees is in
[../integrations.md](../integrations.md).

The words, as the owner reads them: an **integration** is the
installed thing; a **version** is one stored bundle of it; its
**connection** is what the owner pastes (a token, a portal name); an
agent's **access** is what that agent may do through it; its
**actions** are the tools it gives agents. The **bundle** (the zip)
and the **manifest** are developer words. Operator-facing text never
says MCP, tool or pod.

The code: `internal/integration` (the manifest, the check, the access
form, reading and unpacking a bundle; a leaf package),
`internal/store/integrations.go` (where everything is kept),
`internal/secrets` (the connection's secret fields),
`internal/web/api_integrations.go` (the API) and
`internal/web/apitypes/integrations.go` (the wire types).

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
  "author": { "name": "Integrations engineer (agent)" },
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
  versions/<version>/bundle/        the bundle, unpacked
  connections/<id>.yaml             id, created_at, fields (not secret), secret_keys, secret_name,
                                    verified_at, verified, verify_message
  access/<slug>.yaml                slug, connection_id, access, set_at, set_by
  audit.jsonl                       one row per call: at, agent, action, allowed, reason, duration_ms
```

A version is stored whole or not at all: it is unpacked into a staging
directory beside the others and renamed into place. A version's state
moves one way:

```
pending ──approve──▶ approved ──a later approval──▶ superseded
   │
   └──reject──▶ rejected
```

Approving sets the integration's `current_version`; the version
approved before becomes superseded. A version stored once, in any
state, cannot be stored again: a changed bundle needs a new version
number. A built-in integration (`builtin`, which an upload never
sets) takes no upload and cannot be removed. Removing an integration
deletes its directory, its connections' records, every agent's access
and the audit; the API deletes the connections' Secrets first.

An access row is checked when it is read
(`store.ListIntegrationAccess`): it is invalid, and grants nothing,
when no version is approved, when it was set for another connection
than the current one, or when the approved version's form refuses it.
The reason is on the row, so the page can say why. Archiving an agent
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

Test connection (`POST .../connection/verify`) and the options an
access field lists (`GET .../choices/{key}`) call the integration, so
they need its runtime; without it they answer 409 "the integration is
not running yet". The audit lists what is in `audit.jsonl`, which the
integration's calls append to.

## Installing a version

An upload (`POST /api/v1/org/integrations`) reads the bundle, runs the
check and stores the version as pending, published by the owner. Its
install screen (`GET .../versions/{version}`, the web app's
`/org/integrations/<name>/versions/<version>`) says what approving
lets in: who built it and published it, every host, every connection
field (secrets marked), every action, what the access form asks, the
bundle's size and the check's warnings. Nothing runs before it is
approved.

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

## The API

Wire types in `internal/web/apitypes/integrations.go`; every route is
the owner's.

| Route | Answer |
| --- | --- |
| `GET /api/v1/org/integrations` | `Integrations`, by display name. Status: pending (no approved version), off, connected (a connection whose secrets are all there), not connected. |
| `POST /api/v1/org/integrations` | multipart `file`: 201 `IntegrationVersion`; 400 `IntegrationUploadRefused`; 409 for a version already stored or a built-in name. |
| `GET /api/v1/org/integrations/{name}` | `IntegrationPage`. |
| `POST .../{name}/enable`, `.../disable` | `IntegrationPage`. |
| `DELETE .../{name}` | 204; 409 for a built-in one. |
| `GET .../versions/{version}` | `IntegrationVersion`. |
| `POST .../versions/{version}/approve`, `.../reject` | `IntegrationVersion`; 409 when not pending. |
| `PUT .../connection` | `ConnectionWrite` in, `IntegrationPage` out; 409 while no version is approved. |
| `DELETE .../connection` | `IntegrationPage`: the Secret, the record and every agent's access removed. |
| `POST .../connection/verify` | 409 without the integration's runtime. |
| `PUT .../access/{slug}` | `AccessWrite` in, `IntegrationPage` out; 404 for an agent not on the team; 409 without a connection; 400 with the field's message. |
| `DELETE .../access/{slug}` | `IntegrationPage`. |
| `GET .../audit?limit=` | `IntegrationAudit`, newest first; limit 100 by default, at most 1000. |
| `GET .../choices/{key}` | 409 without the integration's runtime. |
| `GET /api/v1/agents/{slug}/integrations` | `AgentIntegrations`; 404 for an agent not on the team. |
