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
tools), `internal/web/api_integrations.go` and
`api_integration_connections.go` (the API, its connections in the
second) and
`internal/web/apitypes/integrations.go` (the wire types).
`internal/builtinintegrations` holds the integrations that ship with
Kivali (each in a package of its own, listed in
`internal/builtinintegrations/builtins`),
`internal/store/builtin_integrations.go` writes them at boot,
`internal/web/integration_builtin.go` calls them and
`internal/web/integration_builtin_egress.go` keeps their connections'
proxy credentials ([Built-in integrations](#built-in-integrations)).
`internal/connect` runs the OAuth flows of a connection made by
signing in, `internal/connect/connecttest` is a vendor in-process for
its tests, and `internal/web/integration_signin.go` keeps and refreshes
the tokens ([Sign-in connections](#sign-in-connections)).
The Git built-in is `internal/builtinintegrations/gitintegration`,
with `internal/gitrepo` (a repository's URL and access patterns),
`internal/shelljob` (the shell job's wire shapes) and
`internal/agentshell/git.go` (the daemon's `POST /v1/git`)
([The git built-in](#the-git-built-in)).
`internal/builtinintegrations/githubintegration` is GitHub as a
built-in of Go code, which `builtins.All()` does not list
([The GitHub built-in](#the-github-built-in)).
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
    "packages": ["jq"],
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
  fields the owner pastes, or a sign-in, `device` or `browser`
  ([The connection section](#the-connection-section)).
- `kivali.hosts` is every host the integration reaches, in the egress
  allowlist's pattern language ([architecture.md](architecture.md),
  the egress proxy).
- `kivali.packages`, optional, is the Debian packages the pod installs
  from Debian's repositories each time it starts, before the server
  runs ([Software](#software)): a CLI the server runs per call, such as
  `gh`, without a binary in the bundle.
- `kivali.access` is the access form (below). Without it, access is
  all or nothing: an agent has the integration or does not.
- `kivali.verify` names the tool Test connection calls. Without it the
  integration has no Test connection.
- `kivali.setup_notes` is what to do on the vendor's side before
  connecting (register an app, the scopes to tick), Markdown. The page
  shows it above the connection form, and the install screen's answer
  carries it (`setup_notes_md`). The literal `{{redirect_url}}` in it is
  replaced with the redirect address for browser sign-in
  ([Signing in](#signing-in)), and `{{external_redirect_url}}` with the
  one at the team's external address (`KIVALI_EXTERNAL_URL`, Other
  devices in Kivali Desktop): `https://<host>/connect/callback`, or
  nothing when the team has no external address or the page was opened
  there, where `{{redirect_url}}` already names it. Notes that pre-fill
  the vendor's app with both register every address the team signs in
  at. A top-level code block in the notes shows as a `CodeBlock` with
  its own Copy, headed by the block's info string (`` ```Organization
  address ``): the way to give an address the owner edits before
  opening it.

An integration's tools reach agents named with its tool prefix, its
name with hyphens as underscores and a trailing underscore
(`integration.ToolPrefix`: `hubspot_create_post`, `my_crm_find`).

### The connection section

A connection made by signing in names the vendor's OAuth endpoints in
`kivali.connection`; core runs every flow against them and the
integration never does OAuth
([Sign-in connections](#sign-in-connections)):

```json
"connection": {
  "method": "device",
  "device_endpoint": "https://github.com/login/device/code",
  "authorization_endpoint": "https://github.com/login/oauth/authorize",
  "token_endpoint": "https://github.com/login/oauth/access_token",
  "revocation_endpoint": null,
  "scopes": ["repo"],
  "pkce": true,
  "client": "operator",
  "extra_authorization_params": { "allow_signup": "false" },
  "credential_keys": { "access_token": "GH_TOKEN" }
}
```

- `method`: `token`, `device` (the device authorization grant, RFC
  8628: a code and an address on the page, core polls the token
  endpoint), `browser` (the authorization code grant with PKCE: the
  owner's browser goes to the vendor and back to Kivali) or `setup`
  (the integration's own setup flow makes the connection, held to
  `kivali.connection.setup`: [Setup connections](#setup-connections)).
  A `device` manifest that also gives `authorization_endpoint` offers
  both ways, and so does a `browser` one that gives `device_endpoint`;
  `method` is the way the page shows first (`Connection.Ways`). The
  list is closed: core knows standards (a pasted token, OAuth's two
  grants) and mechanisms (running a flow's steps), never a vendor; a
  vendor's own way of making or installing an app is that
  integration's setup flow.
- `device_endpoint`, `authorization_endpoint`, `token_endpoint`,
  `revocation_endpoint`: the vendor's addresses. A revocation endpoint
  (RFC 7009) is optional; with one, deleting the connection revokes
  its tokens.
- `scopes`: what a sign-in asks for, shown on the page
  (`IntegrationPage.scopes`) and the install screen.
- `pkce`: whether browser sign-in sends a PKCE challenge (S256);
  absent is true.
- `client`: whose OAuth app signs in. `operator` (or absent): the owner
  registers an app with the vendor, and the connection form takes its
  client id and secret. `registered`: core registers the team as a
  client with the vendor's authorization server (RFC 7591), found from
  the metadata at `discover` (the protected resource's, RFC 9728, or
  the authorization server's own, RFC 8414), so the owner registers
  nothing, the form takes no client id or secret, and the manifest
  gives no endpoints: core reads them there. Only a `browser`
  connection has a registered client. For a bundle, a registered
  client is not in this version of Kivali.
- `discover`: the `https://` address a registered client's metadata is
  read from. Its host is among `SignInHosts`.
- `hosts`: for a registered client, every host the discovered
  endpoints may be on (the authorization server's, and the token and
  revocation endpoints' where they differ), lowercase host names, no
  wildcard, each once. They are the bound: the install screen lists
  them under "Signs in at", and core refuses metadata naming any other
  host. Required for a registered client; refused for any other.
- `extra_authorization_params`: added to the vendor's authorization
  address as they are.
- `credential_keys.access_token`: the name the access token goes under
  in a call's answer (`kivali/credential`'s `fields` and `env`, a
  built-in's `Connection.Fields`).

The connection form of a sign-in connection is
`integration.ConnectionFields(manifest)`: the manifest's `user_config`
followed by two fields core adds, `client_id` ("Client id", required)
and `client_secret` ("Client secret", sensitive, required). Everything
that reads the form (the page, a save, a call's values, the install
screen and what one version changes from another) reads it there. A
sign-in connection's `kivali.hosts` need not list the vendor's OAuth
hosts: core talks to those, not the pod.

The check holds the section to these rules, for bundles and built-ins
(`validateConnection`, `internal/integration/validate.go`):

| Rule | Refused with |
| --- | --- |
| `method` is `token`, `device`, `browser` or `setup`. | `kivali.connection.method <method> is not one Kivali knows; it is token, device, browser or setup` (`kivali.connection.method is missing; ...` without one) |
| A `token` connection gives no endpoint. | `kivali.connection.token_endpoint: a token connection has no endpoints` (each one named) |
| `device` gives `device_endpoint` and `token_endpoint`; `browser` gives `authorization_endpoint` and `token_endpoint`, unless its client is registered, when it gives none. | `kivali.connection.<endpoint> is missing; a <method> connection needs it`; `kivali.connection.<endpoint>: Kivali reads a registered client's endpoints from discover, so the manifest gives none` |
| Every endpoint given, and `discover`, is an absolute `https://` address; plain `http://` only on `localhost`, `127.0.0.1` or `::1`, for a vendor run in-process in tests. | `kivali.connection.<endpoint>: "<address>" must be an https:// address` |
| `client` is absent, `operator` or `registered`. A registered client is a `browser` connection's, with `discover` and `hosts`, and for a built-in; a client Kivali ships (`kivali`) is not in this version. | `kivali.connection.client <client> is not one Kivali knows; it is operator or registered`; `kivali.connection.client registered: only a browser connection has Kivali register itself; ...`; `kivali.connection.client registered is not in this version of Kivali for a bundle; ...`; `kivali.connection.discover is missing; a registered client needs the address whose metadata names the authorization server`; `kivali.connection.discover: only a registered client has one`; `kivali.connection.hosts is missing; a registered client names every host the discovered endpoints may be on`; `kivali.connection.hosts: only a registered client has them; ...`; `kivali.connection.hosts: "<host>" is not a host name; ...` (the setup hosts' rules); `kivali.connection.client: a client Kivali ships is not in this version of Kivali; the owner registers the app and the client is operator` |
| Only a `setup` connection has a `setup` section, and a `setup` connection is held to [its own rules](#setup-connections). | `kivali.connection.setup: only a setup connection has one` |
| `credential_keys` names only `access_token`, and a sign-in connection names it: capital letters, digits and underscores, starting with a letter. | `kivali.connection.credential_keys.access_token is missing; ...` |
| A sign-in connection's `user_config` declares neither `client_id` nor `client_secret`: core adds them. | `user_config.client_id: Kivali adds the client id and secret to a sign-in connection's form, so user_config does not declare them` |
| `scopes` entries are not empty. | `kivali.connection.scopes[<n>] is empty` |
| `extra_authorization_params` names none of `client_id`, `redirect_uri`, `response_type`, `scope`, `state`, `code_challenge`, `code_challenge_method`. | `kivali.connection.extra_authorization_params.<key>: Kivali sets <key> itself` |
| A sign-in connection's `server.mcp_config.env` names no `${user_config.client_secret}` (it reads empty) and sets no key equal to the access token's name (the token would replace it). | `server.mcp_config.env.<key>: the client secret is Kivali's; the server asks kivali/credential for the access token`; `server.mcp_config.env.<key>: Kivali gives the access token under that name, so the env cannot set it` |

### Setup connections

A `setup` connection is made by the integration's own flow: a run of
steps the owner takes (a Form to fill in, a Go that sends the browser
to the service and back, a Wait while something happens on the
service), which core runs and draws, with the integration deciding
each next step (`builtinintegrations.SetupIntegration`, [The
interface](#the-interface); for a bundle, its server's answers to
`kivali/setup`, [For a bundle](#for-a-bundle)). The service's own way of making or
installing an app lives in that flow, and in no other integration.
`kivali.connection.setup` declares what the flow may do, and core holds
every step to it; the install screen lists it under **Setup** before
anything runs, and the page shows it in the Connection block
(`setup` on `IntegrationVersion` and `IntegrationPage`,
`apitypes.IntegrationSetup`):

```json
"connection": {
  "method": "setup",
  "setup": {
    "summary": "Creates a private GitHub App, on your account or an organization's, that may read contents and write issues and pull requests, and installs it on the repositories you choose.",
    "hosts": ["github.com"],
    "fields": {
      "account": {"words": "Your personal account, or an organization's name"},
      "personal_access_key": {"title": "Key", "words": "From the key page", "secret": true, "setup_only": true}
    },
    "keeps": {
      "app_id": "The app's id",
      "private_key": "The app's private key, which Kivali signs with to get a token for each call"
    }
  }
}
```

- `summary`: what setup does, in words for the owner. Required.
- `hosts`: every host a Go step may send the browser to, or a Wait
  step may name a page on; lowercase host names, no scheme, port, path
  or wildcard, each once. Core refuses a step naming any other.
  Empty for a flow that stays in Kivali.
- `fields`: every field a Form step may ask for, in order, each with
  `words` (what to enter and where it comes from; required), an
  optional `title` (the key in words otherwise:
  `integration.SetupFieldTitle`), `secret` (entered as a secret, never
  shown again) and `setup_only` (lives in the flow's memory alone and
  is never kept: a key setup trades for something else). Core draws
  only declared fields, under the integration's name.
- `keeps`: every key setup may store in the connection's Secret, each
  with words for the owner. The kept values are the connection's
  fields, all secret (`integration.ConnectionKeySecret`): a call reads
  them as `Connection.Fields`, a bundle's server as
  `kivali/credential`'s `fields`, and an env placeholder
  `${user_config.<key>}` may name one (`integration.ConnectionKeys`).
  A kept `client_id` and `client_secret` are the app's, read from the
  Secret (not the record, where a pasted client id lives). Core refuses
  a step keeping any other key.
- `then`: `browser` or `device` when setup ends by handing over to
  core's sign-in with the app it made: setup keeps `client_id` and
  `client_secret`, the connection gives that way's endpoints and
  `credential_keys` beside `setup`, and core signs in, refreshes and
  revokes as for any sign-in connection (`Connection.OAuth`,
  `Connection.ThenMethod`). Without `then` the connection gives no
  endpoints and setup keeps the connection itself; it may still keep an
  app's `client_id` and `client_secret` the service minted, or ask the
  owner for them.
- `user_config` is empty and `client` is not given: the connection's
  values are what setup keeps, and the app is the one setup makes.
- A built-in with a setup flow declares `kivali.hosts`: every host its
  setup and its calls reach from Kivali, which the install screen
  lists under "Reaches these hosts" (any other built-in declares none,
  since it reaches the hosts its connection names).

The check's rules, for bundles and built-ins (`validateSetup`), each
refused with `kivali.connection.setup.<field>: <why>`: the summary and
every field's and keep's words are present; a host is a lowercase host
name, not `*` or a pattern, listed once (`validateHostList`, the rule a
registered client's `hosts` are held to as well); a field's or keep's
key is lowercase letters, digits and underscores, starting with a
letter; neither names `oauth_access_token` or `oauth_refresh_token`; a
`setup_only` field is not also kept; `then` is `browser` or `device`,
and with it `client_id` and `client_secret` are kept, while without it
the connection has no endpoints. The same rules hold a bundle's setup
flow and a built-in's; a bundle's runs in its server, over MCP ([For a
bundle](#for-a-bundle)). What a later version changes in the declarations is on its install
screen ("Setup sends your browser to, added: ...", "Setup asks for,
added: ...", "Setup keeps, added: ...": `integration.Diff`).

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
| `server.mcp_config` | `command` is `python3` or `python`; `args` is not empty; each `${user_config.<key>}` in `env` names a field of the connection form (`integration.ConnectionFields`), and `env` holds no other `${...}`. |
| `user_config` | Each key is lowercase letters, digits and underscores, starting with a letter, and neither `oauth_access_token` nor `oauth_refresh_token`, the keys a sign-in's tokens are kept under; `type` is `string`, `number` or `boolean`; `title` is not empty; `sensitive` and `required`, when given, are booleans. |
| `tools` | Not empty; names are unique, lowercase letters, digits and underscores starting with a letter; a tool without a description is a warning. |
| `kivali.connection.method` | `token`, `device` or `browser`. |
| The rest of `kivali.connection` | See [The connection section](#the-connection-section). |
| `kivali.hosts` | Not empty; each a lowercase host or pattern with no scheme, port, path, user or whitespace (`integration.ValidHost`, the rule of `store.ValidEgressHost`, which a store test holds it to); a bare `*` is refused. |
| `kivali.packages` | Each a Debian package name (`debpkg.ValidName`, Debian Policy's rule, which the agent shell's software is held to too): lowercase letters, digits, `+`, `-` and `.`, at least two characters, starting with a letter or digit and not ending in `-`, so no option, removal, version or release; none listed twice. |
| `kivali.access` | See the access form. |
| `kivali.verify` | When given, a tool in `tools`. |
| The bundle | At most 5000 files and 64 MiB; no symlinks, special files, absolute paths or `..`. |

The check reads the bundle. It is not a review of what the code does:
the install screen is, where the owner reads what the version
reaches, holds and does before approving it.

A built-in integration's manifest is checked by
`integration.ValidateBuiltin(manifest)` by the same rules, with four
differences. There is no bundle. A `server` section is refused, and so
is any `kivali.packages` entry and, unless the connection is made by a
setup flow, any `kivali.hosts` entry: a built-in runs inside Kivali,
its network reach is Kivali's own, and it installs no software; a
setup built-in names the hosts its setup and calls reach from Kivali
([Setup connections](#setup-connections)). The names that `kivali.verify` and
`kivali.access.choices` give must be lowercase letters, digits and
underscores, but need not be in `tools`, since they are passed to the
built-in's Go functions
([Built-in integrations](#built-in-integrations)).

Every integration, bundle or built-in, takes one connection: the
installation is its identity. A manifest key Kivali does not read is
ignored, `kivali.connection.multiple` among them (Git's versions
stored by an earlier Kivali carry it).

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
| `boolean` with `"x-kivali-danger": true` | `switch` with `danger` (`AccessField.Danger`) |

`x-kivali-danger` marks a switch whose "on" the owner should choose
with care (Git's force-push): the access dialog draws it in the warning
tone, its description beside a warning icon instead of a muted hint.
It must be `true` or `false`, and it is an error on any property but a
`boolean`.

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
out; no form reads "Full access"). A built-in that implements
`builtinintegrations.AccessSummarizer` puts its access in words itself
([The interface](#the-interface)).

## Where it is kept

Under the data directory, written only by `internal/store`, each
integration's writes under its own lock:

```
integrations/<name>/
  integration.yaml                  name, display_name, current_version, enabled, builtin, installed_at, installed_by
  versions/<version>/version.yaml   version, state, notes_md, published_by, published_at, approved_at, rejected_at,
                                    bundle_bytes, file_count, bundle_digest, findings (the check's warnings)
  versions/<version>/manifest.json  the manifest, byte for byte
  versions/<version>/bundle/        the bundle, unpacked (none for a built-in of Go code)
  connections/<id>.yaml             id, created_at, fields (not secret), secret_keys, secret_name,
                                    verified_at, verified, verify_message, sign_in (the one connection)
  connections-retired/<id>.yaml     a connection set aside at boot, as it was stored (Migration, below)
  access/<slug>.yaml                slug, access, set_at, set_by, retired (set aside with a retired connection)
  audit.jsonl                       one row per call: at, agent, action, allowed, reason, duration_ms
  log.jsonl                         the integration's log: at, level, message (the runtime, below)
```

A version's `bundle_digest` (`store.BundleDigest`) is a SHA-256 over
every file of its `bundle/`, path, size and bytes, in path order,
written with the files; a version stored before digests were kept gets
it from its files the first time core asks for it
(`store.IntegrationBundleDigest`). A version with no bundle has none.

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
its versions follow the binary. Uninstall takes it back to available
while the binary carries it; one the binary does not carry is removed
with `store.RemoveStaleBuiltinIntegration`
([Built-in integrations](#built-in-integrations)). Removing an integration deletes its directory (every
version, whatever its state), its connections' records, every agent's
access, the audit and the log; the API deletes the connections'
Secrets first, and the next reconcile stops its pod
([Uninstall](#uninstall)).

An access row is one agent's access through the integration's
connection; it names no connection. It is checked when it is read
(`store.ListIntegrationAccess`): it is invalid, and grants nothing,
when it was set aside with a retired connection (`retired`, which
holds the reason), when no version is approved, when the integration
has no connection, or when the approved version's form refuses it. So
approving a version or rolling back to one checks every row against
that version's form at once, and a row it refuses is invalid until it
is set again (`PUT .../access/{slug}`) or the version in use changes to
one that accepts it. The reason is on the row, so the page can say why,
and the page counts the invalid rows in a banner. Archiving an agent
clears its access everywhere (`ArchiveAgent`); deleting the connection
(`store.DeleteIntegrationConnection` at Uninstall, or
`store.DeleteIntegrationConnectionByID` for the one a failed install
made) clears every agent's, under the same lock and before the record
goes. Setting an agent's access writes the row afresh, without
`retired`.

## Connections and Secrets

An integration has one connection: the installation is its identity,
and every agent's access and every call goes through it. The
connection is a record under its id (`store.GetIntegrationConnection`,
the oldest record should the directory hold more than one); the id
stays the same across saves, Fixes and sign-ins, and a new one is
drawn only when the connection is made again after Uninstall. A
caller that holds an id (a setup flow, a shell job) asks by it
(`store.GetIntegrationConnectionByID`) whether the connection is still
the one it had.

A connection's fields that are not sensitive are in
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

`PUT .../connection` writes the connection (or makes it when there is
none), `POST .../connection/verify` tests it and `POST
.../connection/sign-out` signs it out ([Sign out](#sign-out)).
[Uninstall](#uninstall) removes it.

A write (`ConnectionWrite`) takes the fields by key, from the approved
version's `user_config`. A field the form does not have is a 400; a
required field with no value, sent or stored, is a 400 ("the
connection needs <title>"); a `number` or `boolean` field must parse.
A body with a key `ConnectionWrite` does not have (a `label`, say) is a
400, as for every JSON body. A sensitive field left out keeps its stored value, and an
empty one clears it; a field that is not sensitive is what is sent. A
write clears the last Test connection result. When the record names
secret fields its Secret does not hold (a backup restored into another
install, or the Secret deleted by hand), `ConnectionState.ReconnectNeeded`
is true, those fields read as not set, and agents cannot use the
connection until they are entered again: the `connection`
[requirement](#requirements) is not met.

The page (`IntegrationPage`) carries `connection`, absent while there
is none, with its id, created at, Test connection's last result,
`reconnect_needed`, `sign_in` for a connection made by signing in
([Token storage](#token-storage)) and `fields`, the connection form
with its values (the non-sensitive values, and which secrets are set);
`connection_fields`, the same form, also before there is a
connection; `connection_method`, `scopes`,
`setup_notes_md` and, for a setup connection, `setup` (what its flow
may do: [Setup connections](#setup-connections)), from the manifest;
`sign_in_client`, whose app a sign-in uses (`operator`, `registered`,
`setup` for a flow that hands over to a sign-in, or empty);
`sign_in_hosts`, the hosts of the manifest's OAuth endpoints and
discover address, each once, sorted (`Connection.SignInHosts`, also on
the install screen under "Signs in
at"), and `sign_in_at`, for each way offered the host it sends the
owner to (the authorization endpoint's for browser, the device
endpoint's for device), which the Sign in block names ("Signs in at
app.hubspot.com"); and `connection_test`, true
when the version in use names `kivali.verify`. Without it the page offers no
Test connection, and no connection carries a last result, even one
kept under an earlier version that had a test. An access write
(`AccessWrite`) is the filled access form alone; it needs the
connection (409 "install <integration> first" without it). An agent's
Integrations tab says whether the agent's calls can go through now
(`AgentIntegration.usable`), read from the connection: not while it
needs its secrets again.

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
instead (the options need it: 409 "install <integration> first"
without it). What a built-in answers is scrubbed of the connection's
secrets; when they cannot be read, the answer is held back. The audit lists what is in
`audit.jsonl`, which these calls and agents' calls
([The audit](#the-audit)) append to.

## The lifecycle

Every integration moves through the same states
(`apitypes.IntegrationState`, `integrationView.state`,
`internal/web/integration_requirements.go`), read from its record, its
connections and its setup flow, never stored:

| State | When |
|---|---|
| `pending` | no version has been approved yet |
| `not_included` | a built-in record this binary does not carry |
| `setting_up` | a flow that installs it runs: started with no connection (or taking over from one that did), not yet ended |
| `available` | an approved version and no connection: a built-in while this binary carries it, a bundle once a version is approved; also while the connection a failed install made is on its way out, and while the connection is one an earlier version made that does not fit this version's setup flow ([Migration](#migration)) |
| `off` | a connection, and switched off: agents cannot use it, and the connection and every agent's access are kept |
| `installed` | a connection, and switched on: agents can be given access |

Install is `POST .../setup` with the flow `setup` ([Setup
flows](#setup-flows)). An integration with nothing installed has
nothing to keep off, so installing one that is switched off switches it
on; the switch is drawn only installed or off. A Fix's flow
([Requirements](#requirements)) runs only installed and on. An
agent's call through an integration that is not installed or is off is
refused as before: no connection, or switched off
([Agents' calls](#agents-calls)).

`IntegrationSummary` carries `state` and `needs`, how many
requirements of an installed integration are not met (0 in any other
state). `IntegrationPage` carries `running`, whether it can be called
now (a built-in while this binary carries it, a bundle while its shim
is connected), `requirements` (installed or off; for an available
integration whose connection an earlier version made, its one unmet
`connection` row, Fix `setup`, which the Install block shows; empty
otherwise),
`flow`, its setup flow under way or the one that last ended, and
`can_uninstall`: an upload in any state, a built-in installed, off or
not included, or available holding a connection an earlier version made. The agent's tab (`AgentIntegration`) carries `state` and
`usable`: installed, running, its access valid, and the connection its
access names with its secrets and, made by signing in, signed in.

The Kivali Developer's `integration_list` says each one's state in
words ("not installed", "being set up", "installed, off") and, for an
installed one, the titles of the requirements not met ("installed, 1
requirement not met: Running").

### Requirements

Requirements are an installed integration's health, built when the
page, the list, Home or `integration_list` is read, never on a timer
(`Server.requirements`). Each row (`apitypes.IntegrationRequirement`)
is `{id, title, words, met, fix}`; `fix` is the flow its Fix button
runs, or empty. Kivali's own rows come first, in this order:

| id | Title | Met while | Fix |
|---|---|---|---|
| `version` | Approved version | always, once installed: "Version 1.2.0 is in use." (a built-in's "ships with Kivali") | |
| `connection` | Connection | the connection has every secret its record names | `setup` |
| `sign_in` | Sign-in | for a connection made by signing in, or by a setup that hands over to a sign-in: `NeedsSignIn` is false. Not met: never signed in, the service refused a renewal (its words), or the token is not in the Secret | `sign_in` |
| `connection_test` | Connection test | the manifest names `kivali.verify`, it has run, and the last result passed (its words, or the integration's words of the failure) | |
| `running` | Running | a bundle, switched on: its shim is connected | |

Then the integration's own rows: a built-in implementing
`builtinintegrations.RequirementsOf` is asked with the connection as
a call reads it, and a setup bundle's server, while it runs, with
`kivali/requirements` ([For a bundle](#for-a-bundle); one not running
is not asked, and the `running` row says why), the read (which may
renew a sign-in with the service) and the call both under one
`requirementsTimeout` (five seconds, on the server's clock), with its
panics recovered. A row whose id is not letters, digits, hyphens and
underscores (64 at most), is one of Kivali's or repeats one, or whose
`fix` is not such a name, is left out and logged; a title left empty
is the id; titles are cut to 80 runes and words to 500, each scrubbed
of the connection's secrets. An error, or no answer in time, is one
unmet row `checks` ("<Integration> could not check its requirements:
<words>"). While the connection needs a sign-in, or the service did
not answer its renewal, the integration is not asked: the `sign_in`
row says why.

What it answered is kept `requirementsCacheTTL` (three seconds, on the
server's clock) for the connection it was asked with
(`Server.requirementsCache`), so the page, the list and Home read on
one screen ask it once. A change of the connection
(`integrationRegistry.ConnectionChanged`, which counts each in
`connectionGen`), a flow done and Uninstall make the next read ask
again. The list and `integration_list` read every integration at once
(`listIntegrations`), as Home does, so a list waits one deadline at
most however many integrations there are.

#### Fix

A Fix runs a flow through the [setup runner](#setup-flows), with the
same steps as installing: `POST .../setup` with the row's `fix`. The
runner takes:

- `setup`: the install flow again, over the connection there is (the
  `connection` row's Fix: a token connection's Form writes into it).
  Started with a connection, it does not make the integration setting
  up.
- `sign_in`: the standard sign-in again (`signInSpecFor`), in the first
  way that works from here, asking for the connection form only when
  the app's client id or secret is not stored; for a setup connection
  that hands over to a sign-in, that sign-in with the app setup kept. A
  connection signed in is a 409 ("the connection is signed in; sign
  out first"); one not made by signing in is a 409 ("<integration> is
  not connected by signing in").
- Any other name: only one the integration's own rows name as a `fix`
  (`fixNamed`, asked before `lockIntegrations` is taken, so a slow
  integration holds up no other write), run by its `SetupIntegration`
  (a bundle's server, through `kivali/setup`) with that flow's name and
  held to its `kivali.connection.setup`
  declarations; a name no row names is a 409 ("<flow> is not a flow
  <integration> has", or "that is not a flow <integration> has" for
  one that is not a flow's name, which is not echoed), and a name a
  built-in whose connection is not `setup` names is a 409
  ("<integration>'s setup does not run its own flows").

A Fix needs the integration installed ("install <integration> first")
and switched on ("switch <integration> on first"). It works on the
connection as it stands: a Fix's flow that fails keeps what it kept.

#### Needs you

`GET /api/v1/home` carries a row per unmet requirement of every
installed integration (`Server.integrationRequirementItems`), each
integration's read at once: kind `integration_requirement`, id
`integration_requirement:<integration>/<requirement id>`, title
"<Integration>: <requirement title>", from Kivali, the words as the
body and `review_path` the integration's page, which Home's **Open**
goes to. Its time is when the row has stood since as far as Kivali
knows: a refused renewal's `error_at`, the failed test's `verified_at`,
else when the connection was made. There is nothing to answer: the row
leaves once the requirement is met, the integration is switched off or
it is uninstalled.

### Uninstall

`DELETE /api/v1/org/integrations/{name}` is Uninstall, for every
integration (`handleAPIIntegrationUninstall`), 204 once done:

1. A built-in with no connection is a 409 ("<integration> is not
   installed, so there is nothing to uninstall"); its setup flow under
   way is cancelled from the page instead.
2. A built-in implementing `builtinintegrations.Uninstaller`, or a
   setup bundle whose server runs (`kivali/uninstall`, [For a
   bundle](#for-a-bundle)), undoes the installation on the service
   first, with no lock held: `Uninstall` is called with the connection
   as a call reads it (as stored when it needs a sign-in or the service
   did not answer its renewal), under `integrationCallTimeout`. A setup
   bundle switched off has no server to ask: its installation on the
   service is left as it is and the rest goes on, so a bundle whose
   server fails is uninstalled by switching it off first. An error, or no answer in time, is a 502
   ("<integration> could not undo its installation: <words>; nothing
   was changed"), scrubbed of the connection's secrets and audited as
   `uninstall`, not allowed; nothing else happens.
3. Under `lockIntegrations`, the setup flow under way is cancelled.
   The sign-in tokens the deleted Secret held are revoked with the
   service once the lock is released, where the manifest has a
   revocation endpoint ([Revocation](#revocation)).
4. A built-in this binary carries (`uninstallBuiltinLocked`): the
   connection's Secret is deleted (one already gone is no error), then
   its scoped proxy credential, its record and every agent's access.
   A Secret that cannot be deleted keeps the record, so the Secret is
   still named, and the answer is a 500 that says the installation was
   undone on the service and to uninstall again to finish (audited as
   `uninstall`, not allowed). Uninstall again calls the built-in's
   `Uninstall` again, which treats an installation already gone as
   done. Once the connection is gone, the Secrets of any other record
   under `connections/` and of the connections set aside at boot
   (`connections-retired/`, [Migration](#migration)) are deleted, best
   effort (a failure is logged), and their records go
   (`store.DeleteIntegrationConnection`); the integration's record and
   its version stay, so it is available again, and the owner's audit
   gains `uninstall`, allowed.
5. Any other integration, an upload in any state or a built-in this
   binary does not carry: the connection's Secret is deleted first (on
   a failure, a 500 and nothing else changes; a bundle has no
   `Uninstall` of its own), then those of the connections set aside,
   best effort, then it is removed whole: every version, its audit and
   its log ([Where it is kept](#where-it-is-kept)).

The page's Uninstall asks first, naming what goes: the connection and
its secrets, each agent's access by name, and either that it stays
available to install again or that it goes with every version and its
log.

## Setup flows

Every connection is made by a setup flow: a run of steps the owner
takes, which core runs and draws (`internal/web/integration_setup.go`).
What decides each step is the flow's driver: for a `setup` connection,
the built-in's `SetupIntegration.Setup` ([The interface](#the-interface))
or the bundle's server ([For a bundle](#for-a-bundle));
for `token`, `device` and `browser`, one of core's standard flows
(`internal/web/integration_setup_standard.go`):

- `token`: a Form of the connection form (`integration.ConnectionFields`),
  then Keep, saved as `PUT .../connection` saves it, and Done. The
  answer is checked as a save checks it before the flow goes on: a
  required field with no value, a value of the wrong type or the
  built-in's `CheckConnection` is a 400, and the flow stays on its Form.
- `device` and `browser`: [Signing in](#signing-in).

A flow works on the integration's connection, made when there is
none. `PUT .../connection` stays for editing a saved
connection.

### Flows

A flow lives in the server's memory (`Server.setupRuns`), never in the
store, at most one per integration: a new start ends the one before
(audited as cancelled). Its id is 16 random bytes in hex, which Kivali
Desktop checks at `/connect/start`. It is forgotten `setupFlowTTL` (ten
minutes, on the server's clock) after its last change: a step nobody
takes in that time ends with it, a Wait whose check finds nothing new
is no change, and a flow that ended is kept that long for the page to
read. A device sign-in's Wait is kept while core polls the vendor,
which stops at the code's own expiry. Deleting a connection,
Uninstall, switching it off, Sign out and a save of the form with a new
client id or secret cancel it, audited; a restart loses it, and `GET
.../setup` then answers 404 and the page offers to start again.

A flow that installs (`setupRun.install`: started with no connection,
or with one an earlier version made that does not fit this version's
setup (`setupMisfit`, [Keep](#keep)), or taking over from one that did)
keeps nothing unless it is done.
When its Keep makes the connection (`setupMade`), and the flow then
fails, is cancelled, is replaced by a flow that does not take it over,
or is forgotten, core forgets that connection off the caller's locks
(`setupDiscard`): its Secret (a sign-in's tokens revoked), its scoped
proxy credential, its record and the access set for it, with no call
of `Uninstall`. A connection an earlier version made that the install
took over keeps its record, emptied of every value, and its access
(`setupRun.madeKept`), so it still reads available and the next
install takes it over again. Until that is done the integration reads available,
and a new start waits for it (`setupAwaitDiscard`), so the next install
never reads the half-made connection; one that began after the wait is
a 409 under the lock ("Kivali is still undoing the last setup; try
again in a moment"). The owner's own save of the connection (`PUT
.../connection`, `POST .../connections`) or Sign out while an install
runs makes the connection theirs (`setupAdopt`, `setupCancelKeep`): it
is kept whatever becomes of the flow.

The driver is called off any request, one call at a time per flow,
under `setupCallTimeout` (five minutes) on the server's clock, with its
panics recovered (`runBuiltin`): first with an empty Answer, then after
each step with what it gave back. A call that does not finish in time
fails the flow ("setup did not finish its step within 5 minutes; start
it again"), and so does an error, whose words the owner reads. A call
whose flow was cancelled or replaced meanwhile is dropped. Every
Answer carries `Callback`: `<scheme>://<host>/connect/callback?state=<state>`,
the address being what `signInRedirectURL` computes from the request
that started the flow, last answered it or came back to it, and the
state a fresh one for each call (a Wait's check is given the latest
again), any of which finds the flow while it lives.

### Steps

Each Step is held to the declarations before anything of it is kept:
for a `setup` connection, `kivali.connection.setup`; for the standard
flows, the connection form's fields, the manifest's sign-in hosts
(`SignInHosts`) and the form's keys. A step that is none or several of
Form, Go, Wait and Done, a Form asking for an undeclared field, a Go or
a Wait naming a page that is not `https://` (plain `http://` only on
loopback, for a service run in-process in tests), that carries user
info, or that is not on a declared host, and a Keep holding an
undeclared key fail the flow in words that say what it tried ("Echo's
setup tried to send your browser to evil.example, which its manifest
does not declare"). A declared host name takes no port: `github.com`
does not admit `github.com:8443`; a sign-in host listed with its port
(`127.0.0.1:8443`, a vendor run in-process) takes that port alone. The
standard device flow's Wait page is exempt from the hosts, not from
https: it is the page the manifest's own device endpoint answered with
(`verification_uri`), which a vendor may put on a host none of its
endpoints is on (Google's is `www.google.com`).

- **Form.** The page draws the step's fields under the integration's
  name ("GitHub asks for"), a secret one as a password field, a
  setup-only one said to be never kept. `POST .../setup/answer` hands
  what the owner entered, trimmed, to the next call as `Answer.Form`
  (the page sends every field the step asks for, one left blank as
  empty; a standard flow's blank secret keeps the value stored, as the
  page's form does); a key the step does not ask for is a 400, and an answer while the
  flow is not on a Form a 409. A setup-only value lives in the flow's
  memory alone (the driver may carry it in `State`) and is never
  written.
- **Go.** The page sends the browser to `GET /connect/start?flow=<id>`
  (`step.start_url`), which sets the flow's cookie for this leg and
  sends the browser on to the step's address: a 303 for a link, or for
  `Go.Post` a small page whose form posts the values there and submits
  itself on load, with a Continue button for a browser that runs no
  scripts (`no-store`, `no-referrer`, a CSP of `default-src 'none'`,
  its one script by hash and `form-action` the address's origin,
  nothing loaded). The service sends the browser back to
  `GET /connect/callback` ([Coming back](#coming-back)). A Go with
  `NoReturn` (a settings page that sends the browser nowhere) sets no
  cookie: `/connect/start` sends the browser and calls the driver again
  at once with `Answer.Went`, and the page opens it in a new tab so it
  stays to follow the flow.
- **Wait.** The page shows the words, a code to copy (`Wait.Code`) and
  the service's page. While someone watches the flow (a stream open on
  it, or a read of its state in the last `setupWatchWindow`, ten
  seconds), core calls the driver with `Answer.Checked` every
  `setupWaitEvery` (three seconds) on the server's clock; nothing is
  checked for a flow nobody is watching. The check keeps the Wait on
  the page until it answers.
- **Done.** The connection is made; the page reads itself again.

`Progress.Stage` sets what the page shows while a call runs: the words,
and a real progress bar where there is a count (`done` of `total`).
Each call is told which it is: `Answer.Leg` counts the calls of the
flow from 1. `Answer.Team` is the team's name (the branding's company
name, empty for none), for naming what setup makes on the service; a
bundle's `kivali/setup` answer carries it as `team` as well.

### Keep

`Step.Keep` is written before the step is shown, under
`lockIntegrations`, unless the flow was cancelled meanwhile (every
cancel takes the same lock, `POST .../setup/cancel` among them, so a
Keep lands before a cancel or not at all): a `setup`
connection's into the connection's Secret beside what it holds, the
connection made when there is none and the keys recorded as its secret
keys (`keepSetupValues`); a standard flow's as a save of the form
writes it (`keepConnectionForm`). A write that fails fails the flow.
`State` is carried to the next Answer as it was, in memory, for this
flow alone.

A `setup` connection an earlier version made can hold values this
version's flow does not keep (`setupMisfit`: a field in the record,
which setup never writes; a key in its Secret outside `setup.keeps`; a
sign-in, for a flow that hands over to none), or none of what it
keeps. The integration then reads available, with one unmet
`connection` row ("Set up <integration> again: ..."), and Install runs
as an install over it. Its first Keep takes the connection over: every
value of the record and its Secret goes (the scoped proxy credential's
secret aside), the sign-in, the test result with it; the record's id,
its Secret's name and every agent's access stay. Once the lock is
released, the sign-in the Secret held (the client id and secret, the
access and refresh tokens) is revoked, best effort, off any lock
(`revokeReplacedSignIn`; a failure is logged, scrubbed of the
sign-in): first as core revokes any sign-in ([Revocation](#revocation)),
with the manifest of the integration's newest superseded version when
its sign-in has a revocation endpoint, so a standard sign-in that
became a setup is revoked with nothing of the integration's own; then,
for a vendor whose revocation is its own, by the built-in, when it is
a `builtinintegrations.SignInReplacer`, under `revokeTimeout`. The
hook is a built-in's alone, not part of a bundle's contract.
Uninstall of such a connection, which the page offers while it reads
available, revokes its sign-in the same way. With several connection
records ([Migration](#migration)), the connection does not read as
one an earlier version made, and no install takes it over.

### Scrubbing and the audit

Every kept value, every secret or setup-only value the owner entered,
every `State` value six characters or longer, each call's state, the
values a callback brings back under `code`, `state` or a key whose name
holds `token`, `secret`, `password` or `key` (ignoring case; no other
returned value is scrubbed, so a driver puts anything else it must keep
hidden into `State`), and a standard flow's own secrets (the client secret, a device code, the
OAuth state and verifier, the code and the tokens) are replaced by
`[secret]`, whole, in the flow's error words, progress words and
steps, in the audit and in what core logs of the flow, before any of
them is cut to 200 runes.

The owner's audit gains one row per flow event under the owner's name
(`ceo`), with the flow's name as its action (`setup`): `started`,
`returned` (a Go came back), `done`, `failed: <words>` (not allowed),
`cancelled`. Never a code, key or token. The rows are queued with the
change they record and written, in that order, once the flows' lock is
released; a stream is sent a state only after its rows are written.

### Coming back

`GET /connect/callback` is on the protected mux, behind the session
middleware: a browser not signed in to Kivali is sent to `/login` with
the whole callback address, query included, as `next`, and comes back
to it once signed in. It is a GET that changes things, as OAuth has it.
Nothing of its query is logged. It finds the flow by the query's
`state`, any state the flow issued (a constant-time compare), or, with
no `state`, by the flow cookie alone when that flow is on a Go step.
The cookie goes with any top-level GET a page the owner visits makes,
so a return with no state is taken only for a Go step that says its
service drops the state (`Go.Stateless`); for any other the flow fails
with "the service sent the browser back without its state; start it
again" and the driver is not called. A callback replayed from an
earlier leg, with a state the flow issued before, is delivered as the
current Go step's `Answer.Return`: the driver checks what it receives
against the service, as it must anyway.
The cookie, `kivali_connect` (`kivali_connect_<suffix>` with
`KIVALI_COOKIE_SUFFIX`), is set by `/connect/start` for each leg:
`Path=/`, host-only, `HttpOnly`, `SameSite=Lax` (the service's redirect
back is a cross-site top-level GET), `Secure` when the callback address
is `https`, twenty minutes, its value `connect:<flow id>:<integration>`
signed with the session codec (`Server.SessionCodec`, else the session
middleware's, else a key the process makes once). One browser holds
one: a Go started in it ends the flow on a Go step its cookie named,
failed with "another setup was started in this browser; start this one
again".

- A flow on its Go step whose cookie names it: the whole query goes to
  the next call as `Answer.Return`, audited as returned, the cookie is
  dropped, and the browser goes to `/org/integrations/<name>?setup=<id>`.
- A cookie that is missing, not signed by Kivali or names another flow:
  the flow fails with "this came back to a different browser than the
  one that left; start it again".
- A flow not on a Go step (ended meanwhile, or the callback replayed):
  nothing runs, and the browser goes to its page.
- No flow: to the page of the integration the cookie names, with its
  flow id, or to `/org/integrations?sign_in_error=start_again`.

The address never carries words: anyone can link the owner to the page
with any query. The page reads `?setup=<id>` once, takes it off the
address and reads the flow from `GET .../setup`; with none it says the
setup is no longer under way and offers to start again.
`Answer.Return` is untrusted for the same reason: the driver checks it
against the service before relying on it.

### The routes and the stream

`POST .../setup` (`SetupFlowStart`, `{"flow": "setup"}`, or a Fix's
flow: [Fix](#fix)) starts the flow and answers its state
(`SetupFlow`); any other flow name is a 409. `GET .../setup` answers
the state, `POST
.../setup/answer` (`SetupFlowAnswer`) answers a Form, `POST
.../setup/cancel` ends the flow, and `GET .../setup/stream` is an SSE
stream of the state: `retry: 1500`, the state at once as a `flow`
event, each change after (byte-equal states are not sent again; a slow
reader gets the newest), a `: heartbeat` comment every 25 seconds, and
the end of the stream when the flow is forgotten. A stream counts as
watching the flow. The page (`SetupFlowPanel`) follows the stream, and
asks `GET .../setup` every three seconds while the tab is visible where
it has no stream.

### For a bundle

A bundle whose connection is `method: setup` implements the three
hooks in its MCP server, as requests core sends through the shim as it
sends `tools/call` (`bundleHooks`, `internal/web/integration_setup_bundle.go`;
the shapes are `internal/integrationpod/hooks.go`). The runner, the
requirements and Uninstall run them by the same code as a built-in's:
the same declarations, deadlines, scrubbing and audit. The server reads
the connection's values as for any call, from its env or
`kivali/credential`: a Keep restarts it with what was kept
([The shim and its socket](#the-shim-and-its-socket)), so a later step
reads a kept value back as a built-in reads `conn.Fields`. Its own calls
to the service go out through its scoped proxy credential, held to
`kivali.hosts` as any call is. Core sends these requests only to a
bundle whose manifest names `method: setup`, and such a server answers
all three from its start, with no connection too (the smoke test asks
for its requirements: [The smoke test](#the-smoke-test)). Shell jobs
and a proxy credential per connection stay built-in only.

- **`kivali/setup`**, one call of the flow. Params:
  `{"flow", "answer": {"state", "form", "return", "checked", "went", "leg", "callback"}, "_meta": {"progressToken"}}`.
  `flow` is `setup` or a Fix's name; `answer` is the
  [Answer](#the-interface), every object present (`{}` when empty) and
  `return` the query with each key's values as a list. The result is
  `{"step": {...}}` with exactly one of `"form": {"words", "fields"}`,
  `"go": {"words", "url", "post", "no_return", "stateless"}` (`post` an
  object of strings or lists of strings), `"wait": {"words", "url", "code"}`
  or `"done": true`, and optionally `"keep"` and `"state"`, objects of
  strings. A key none of these shapes has, or a value of another type,
  fails the flow ("<Integration>'s setup answered with a step Kivali
  cannot read: ..."). While the step runs, the server may send
  `notifications/progress`, `{"progressToken", "progress", "total", "message"}`
  with the request's token: each is the flow's stage (`message`) and
  its progress bar (`progress` of `total`). One naming any other token,
  or sent once the step has answered, is dropped. The step is answered
  within `setupCallTimeout`, five minutes, or the flow fails as a
  built-in's does; a long job in the pod reports progress meanwhile. A
  server not yet running (a bundle switched on to install it) is
  waited for, `bundleStartWait` (two minutes) at most on the server's
  clock, and then the flow fails ("<Integration> is not running, so
  its setup cannot go on; its log says why. Start the setup again once
  it runs").
- **`kivali/requirements`**, params `{}`: the integration's own rows,
  `{"requirements": [{"id", "title", "words", "met", "fix"}]}`, read as
  a built-in's ([Requirements](#requirements)), under the same five
  seconds, and asked only while the server runs.
- **`kivali/uninstall`**, params `{}`: undo the installation on the
  service, answered `{}`, within `integrationCallTimeout`, and asked
  only while the server runs ([Uninstall](#uninstall)).

An error of any of them is a JSON-RPC error whose message is the
owner's words, scrubbed as a built-in's error is; one with no message
reads "<Integration> failed and gave no reason", and a server that
stops before it answers, "<Integration> stopped before it answered".

## Sign-in connections

A connection made by signing in ([The connection section](#the-connection-section))
is a connection like any other, with its form's fields stored as
above, and the tokens a sign-in gives besides. Core alone holds the
client secret and the refresh token; an integration is given a
current access token at the moment of each call and nothing else.

### Token storage

The tokens are in the connection's Secret, under keys that are no
field of any form (the check refuses a `user_config` key equal to
either): `connect.SecretKeyAccessToken` (`oauth_access_token`) and
`connect.SecretKeyRefreshToken` (`oauth_refresh_token`). The record
keeps the rest (`store.ConnectionSignIn`, `sign_in` in
`connections/<id>.yaml`): `method` (how the owner signed in),
`signed_in_at`, `expires_at` (when the access token expires; absent
when the vendor gave no expiry), `lifetime` (how long the vendor last
said a token lasts: a refresh answer with no `expires_in` gives the
new token that life again, never "does not expire", and until a
refresh has said one, `expires_at` less `signed_in_at` stands for it),
`scope` (what the vendor granted, as it said it), and `error` with
`error_at` once the vendor refused to
renew the sign-in for good. A save of the connection form keeps both
the tokens and the sign-in state; a backup carries the record, never
the tokens.

A sign-in connection needs the owner to sign in
(`IntegrationConnection.NeedsSignIn`) when it never has (no `sign_in`),
when its Secret holds no access token, or when `error` is set. While
it does, its `sign_in` [requirement](#requirements) is not met and
agents cannot use it; `reconnect_needed` stays what it is for any
connection, the form's secrets missing. The page's `ConnectionState.sign_in`
(`apitypes.ConnectionSignIn`) carries `ways` (the manifest's method
first, those that work from where the request came from:
[Signing in](#signing-in)), `browser_elsewhere` and `open_at` when none
does, `signed_in`, and while signed in `method`, `signed_in_at` and
`expires_at`, or the `error`. The page draws "Signed in <when> · token
refreshes <when>", or "Needs you to sign in again: <error>", and under
the form a **Sign in** block: Signed in with the connection test's
words and Sign out; or, not signed in where no way works from here,
where to sign in from instead. Signing in again is the `sign_in` row's
Fix, which runs the sign-in's [setup flow](#setup-flows) for the first
of `ways`.

### The credential path

Every read of a connection's values for an integration's call goes
through `integrationRegistry.readConnection`: `kivali/credential` for a
bundle, `Connection.Fields` for a built-in's call, Test connection and
the options a field lists. For a sign-in connection it
(`Server.signInAccessToken`, `internal/web/integration_signin.go`):

1. Answers `integrationpod.CodeSignInNeeded` (-32002, "the connection
   needs the owner to sign in again") while the connection needs a
   sign-in.
2. Hands out the stored access token while it is good for longer than
   `connect.RefreshMargin` (a minute) or does not expire. Otherwise it
   refreshes through the server's `connect.Refresher`, keyed
   `<integration>/<connection id>`: one refresh at a time per
   connection, which every caller that finds the token due waits for.
   The refresh reads the connection again first, so a caller that
   arrives after another's refresh ended finds the token fresh and
   sends the vendor nothing (a vendor that rotates refresh tokens
   refuses the old one). It runs on with a context that is not
   cancelled when its caller goes, since the vendor may already have
   rotated the refresh token. The new tokens go to the Secret and the
   expiry and scope to the record, both read again under
   `lockIntegrations`, the lock the connection routes take, so a save
   in between keeps its fields; a refresh token the vendor did not
   send keeps the stored one.
3. A refusal for good (a `*connect.Terminal`) writes `error` and
   `error_at` on the record, which raises the
   [Needs you](#needs-you-sign-in-again) row, and answers -32002. Any
   other failure (no answer, a 5xx, an answer Kivali cannot read) is
   logged and leaves the connection as it is: the old token is handed
   out while it has not expired, and after that the answer is
   `integrationpod.CodeVendorUnavailable` (-32003, "the vendor did not
   answer"), which a later call may get past. An expired token with no
   refresh token to renew it marks the record as a refusal does. Each
   failed refresh appends an audit row for the owner: agent `ceo`,
   action `refresh`, not allowed, the failure in words. Before the
   vendor's words are kept, shown, logged or audited, the connection's
   tokens and client secret in them are replaced by `[secret]`, whole,
   and only then are they cut to 200 runes (`signInWordsRunes`), so a
   secret astride the cut leaves no piece behind. The engine bounds
   what it carries at `connect.MaxDescriptionRunes` (2000).
4. Puts the access token in the answer under its `credential_keys`
   name, in `fields` and `env` for a bundle (`GH_TOKEN`) and in
   `Connection.Fields` for a built-in. The client id is there; the
   client secret and the refresh token never are, and an `env`
   placeholder for the client secret reads empty.

A built-in's call of a connection that needs a sign-in is refused with
"<Integration>'s connection needs the owner to sign in again; ask the
owner." (audited as not allowed), and one whose vendor did not answer
with "<Integration> did not answer when Kivali renewed the
connection's sign-in; try again later or tell the owner."; the
built-in is not called. A bundle's server gets -32002 or -32003 from
`kivali/credential` and passes it on in words. The shim starts the
server with empty values on -32002 or -32003, as with no connection,
and the server asks for the token at call time.

The tokens are among the secrets the scrubber replaces, in the log and
in every answer. The log scrubber is built from the connection as
stored and never refreshes; a token refreshed since joins it when the
credential is next answered. A caller that holds `lockIntegrations`
(the proxy credential's registration) reads the connection as stored
(`readStoredConnection`), with no token and no refresh.

### Signing in

A sign-in is a standard [setup flow](#setup-flows), run on the same
runner as an integration's own (`signInSetup`,
`internal/web/integration_setup_standard.go`). The page is offered the
ways `ConnectionSignIn.ways` lists (`Server.signInWaysHere`): the
manifest's `Connection.Ways`, kept to those this server signs in by
(`signInWaysOffered`: `device` and `browser`), and to browser only
where a browser sign-in started by that request can finish
(`browserSignInHere`):

- At a loopback address: in a browser, or in Kivali Desktop's window,
  which hands it to the system browser at the same address ([In Kivali
  Desktop](#in-kivali-desktop)).
- At the team's external address (`KIVALI_EXTERNAL_URL`), the request's
  callback address equal to `https://<host>/connect/callback`, and not
  from Kivali Desktop's window: a request carrying the
  `kivali_desktop_window=1` cookie, which Desktop's team web view sets
  (`TEAM_WINDOW_SCRIPT`) and no browser has. That window, for a team
  elsewhere, has no browser signed in to the team to hand the sign-in to.

Those are the two addresses the setup notes give the vendor's app
(`{{redirect_url}}` opened there, `{{external_redirect_url}}`).
Anywhere else, a plain `http` address on the network or another name for
the team, the vendor would send the browser back to an address its app
may not know and end on its own error page. The cookie can be set by
anyone; all it changes is which way is offered.

`POST .../setup` of a sign-in connection runs the first of those ways
(the page's one button says which: **Sign in** for browser, **Sign in
with a code** for device), so a manifest offering both signs in in the
browser where it can finish and with the code where it cannot. With
none, the start is a 409: `errSignInNotHere` for a browser-only
connection read where browser sign-in cannot finish (the page says
signing in does not work from there and gives `open_at`, the team's
external origin, with Copy, to open in a browser), else "this version
of Kivali has no way to sign in to <integration>". A connection signed
in is a 409 too ("the connection is signed in; sign out first"); one
whose refresh the vendor refused may sign in again, which is how the
owner mends it. The page's `sign_in_redirect_url` and each
connection's `redirect_url` are there only where browser is offered.

The flow's steps:

1. When the integration has no connection, or its client id (record)
   or client secret (Secret) is not stored, a **Form** of the
   connection form (`integration.ConnectionFields`: the manifest's
   `user_config`, then the client id and secret, the secret marked
   secret). The answer is checked as a save of the form checks it
   (`planConnectionWrite`; a refusal is a 400 and the flow stays on its
   Form) and kept as a save keeps it (`writeConnection`), the
   connection made when there is none. The client is resolved by
   `signInClientFor`: a pasted client id from the record, or one a
   setup flow kept from the Secret, and the secret from the Secret. A
   registered client (`client: registered`) would be resolved there
   too; this version of Kivali does not register itself, so `POST
   .../setup` of one is a 409 that says so, before any flow starts.
2. Device: `Engine.StartDevice` posts the client id and the manifest's
   scopes to the device endpoint (an answer with no code, or whose
   verification addresses are not absolute http(s) ones, is refused;
   the vendor's refusal fails the flow in its words). The code's expiry
   is cut to ten minutes. The step is a **Wait** with the code
   (`Wait.Code`) and the service's page (`verification_uri_complete`
   when the vendor gives one), held to https but on any host, since it
   is the device endpoint's own answer.
   It is never checked: core's poller (`signInSetup.waiting`) runs
   `Engine.PollDevice` on the flow's context and the server's clock, at
   the vendor's interval, five seconds more for good on each
   `slow_down`, until the code expires, and the flow is kept while it
   polls. Approval keeps the tokens (below) and the flow is done; a
   refusal (`access_denied` and the other codes `PollDevice` ends on)
   or the code's expiry ends it failed in the vendor's words, scrubbed
   of the client secret and the device code and cut to 200 runes, as a
   refresh's are (`signInFailureWords`).
3. Browser: a **Go** to the vendor's authorization address from
   `Engine.AuthorizeURL`, with the manifest's scopes and
   `extra_authorization_params`, the redirect address (the call's
   `Answer.Callback` without its query) and, as the OAuth state, the
   call's own state from that callback, and the S256 challenge of a
   verifier from `connect.NewPKCEVerifier` unless the manifest says
   `pkce: false`. The vendor sends the browser back to
   `/connect/callback?code=&state=` (or `?error=&error_description=&state=`).
   A vendor `error` fails the flow with its `error_description` (or
   "the service refused the sign-in (<code>)"); a return with no state
   or no code fails it too. Otherwise `Engine.Exchange` posts the code,
   the client id and secret, the redirect address and the verifier; a
   refusal (a PKCE verifier that does not match is the vendor's
   `invalid_grant`) fails the flow in the vendor's words. Either way
   the words are scrubbed of the state, the code, the verifier and the
   client secret, then cut to 200 runes.
4. Keeping the tokens (`finishSignIn`), under `lockIntegrations`: the
   tokens go to the connection's Secret beside its other values (no
   refresh token in the answer drops a stored one), and the record
   gets `sign_in` (`method`, `signed_in_at`, `expires_at`, `lifetime`
   when the vendor gave an expiry, `scope`) with any refused refresh's
   `error` gone, which clears the [Needs you](#needs-you-sign-in-again)
   row, and the last connection test cleared. `ConnectionChanged`
   restarts the bundle's server, which asks `kivali/credential` again
   and starts with the token. A flow cancelled meanwhile, a connection
   gone, or a Secret or record that cannot be written keeps nothing and
   revokes the new tokens, best effort ("the sign-in worked, but Kivali
   could not keep it; start it again").
5. When the manifest names `kivali.verify` and the integration runs,
   the connection test runs (`testConnection`, what Test connection
   runs) and its result is kept on the record, so the page names who
   signed in. Then the flow is done.

A setup flow whose manifest says `setup.then` hands over to the same
sign-in once its own driver is done: the runner swaps in the standard
flow for that way and starts it, with the client id and secret setup
kept. A `then: browser` flow is refused at the start where browser
sign-in cannot finish.

The redirect address is `signInRedirectURL`: `<scheme>://<host>/connect/callback`,
the scheme `https` on a TLS connection, else `X-Forwarded-Proto`'s first
value when it is `https`, else `http` (`requestScheme`, as the API's
same-origin check reads it); the host
`X-Forwarded-Host`'s first value when it is a host, else `r.Host`,
lower-cased, without the scheme's default port. Either header can be
anything the browser sends, which is safe: the vendor sends a code only
to the address registered with it, the flow keeps the address it gave
the vendor for the exchange, and the cookie is host-only. The page
carries it as `IntegrationPage.sign_in_redirect_url` (there before any
connection, since the owner registers it with the vendor before there
is a client id to save) and `ConnectionSignIn.redirect_url`, whenever
the manifest offers browser sign-in, and `{{redirect_url}}` in
`kivali.setup_notes` is replaced with it on the page and the install
screen (`{{external_redirect_url}}` with the external address's,
`Server.setupNotes`).

The page shows the manifest's scopes above the block ("Asks the service
for: ..."), as the install screen does, and the host each way signs in
at. Available, the Install block offers **Install**, whose flow asks
for the form first; installed, the form edits the connection (`PUT
.../connection`), the `sign_in` requirement's Fix signs in again, and
the **Sign in** block under the form says Signed in with the
connection test's words and offers Sign out.

#### In Kivali Desktop

The team's window is a webview: a service's page loaded there is
refused and opened in the system browser, which holds neither the
Kivali session nor the flow's cookie, so the callback would ask the
owner to sign in and then refuse the flow as started elsewhere. The
team's web view has no IPC ([desktop-app.md](desktop-app.md),
"Capabilities: why team web apps get nothing"), so the web app does not
call the app: the shell recognises the navigation to the team's own
`/connect/start?flow=<id>` (that exact path, one `flow` of 32 lowercase
hex digits; `team_nav` answers `ConnectSignIn`), whether the page
navigates there or opens it in a new window, and, for a team here on its
loopback origin, refuses it in the window, mints a handoff token from
the team's supervisor and opens `<origin>/auth/handoff?t=<token>&next=<id>`
in the system browser through the window's browser gate
(`open_connect_sign_in` in `desktop-app/src-tauri/src/windows.rs`). The
handoff, for a flow on a Go step whose callback address is the one it
was reached at, signs that browser in as the owner, sets the flow's
`kivali_connect` cookie there and sends it to `/connect/start?flow=<id>`,
which sends it on to the service by a link or a form that posts itself;
for any other `next` it signs nothing in ([auth.md](auth.md#desktop-handoff),
`next`). The callback then comes to a browser that passes both checks.
The page in the window stays and follows the flow; its Go step then
says "If no browser opened, open the team in your browser and continue
from there."

A team elsewhere (no supervisor here, and its handoff answers loopback
only) is offered no browser sign-in in its window: its web view carries
the `kivali_desktop_window` cookie, so the server lists the code alone,
or no way and the team's address to sign in from in a browser. Should a
Go step start there all the same, the shell refuses `/connect/start`
and opens nothing, so no approval is spent on a flow that could not
come back. Nothing here is platform-specific: macOS and Windows behave
the same.

On Desktop's loopback the redirect address is
`http://127.0.0.1:<port>/connect/callback`, the request's own host.
GitHub accepts a registered `http://127.0.0.1/connect/callback` at any
port of the loopback address; most vendors need the exact address;
vendors that allow no plain `http` (Slack) cannot sign in to a team
here: the code sign-in where the vendor has it, or an `https` address
under Other devices.

### Sign out

`POST .../sign-out` cancels the connection's flow, removes
`oauth_access_token` and `oauth_refresh_token` from its Secret (its
fields, the client id and secret among them, stay), clears `sign_in`
and the last connection test from the record, asks the vendor to
revoke the tokens ([Revocation](#revocation)), appends the owner's
audit row `sign_out`, restarts the bundle's server without the token,
and answers the page. The connection then needs a sign-in, so its
`sign_in` requirement is not met. A connection not signed in that
holds no tokens is left as it is (no audit row, no restart, its last
test kept); only a sign-in under way is cancelled.

### Revocation

Signing out, [Uninstall](#uninstall) and forgetting the connection a
failed install made ask the vendor to revoke the connection's tokens once they
are gone from its Secret, when the manifest gives a revocation
endpoint: the refresh token, then the access token, with the client's
credentials, best effort within ten seconds. The tokens are collected
under `lockIntegrations` and revoked once it is released, before the
route answers, so a slow vendor holds up no other integration write.
A failure is logged and the sign-out or delete goes on. Deleting and
Uninstall also cancel the integration's setup flow under way ([Setup
flows](#setup-flows)).

### Needs you: sign in again

A connection the service no longer accepts is its `sign_in`
requirement not met ([Requirements](#requirements)), so Home's Needs
you carries it as an `integration_requirement` row, id
`integration_requirement:<integration>/sign_in`, title "<Integration>:
Sign-in", at `error_at`, the vendor's refusal in the body ("The service
no longer accepts the sign-in: <refusal>. Agents cannot use
<Integration> until you sign in again."). The row leaves once the
owner signs in again (a new sign-in clears `error`), switches the
integration off or uninstalls it.

### The flow engine

The OAuth requests the standard sign-in flows and refreshes make are
`internal/connect`'s; the flows themselves are the setup runner's
([Setup flows](#setup-flows)). `internal/connect` knows nothing of the
store or the web server. An
`Engine{Clock, HTTP}` makes every request through its HTTP client
(core's has an 8-second timeout, so a refresh inside a pod's
`kivali/credential` ends before the shim's 10-second wait for core;
`Server.Connect`) and reads every
expiry and wait from its clock:

- `Refresh(ctx, Endpoints, Client, refresh)` and `Exchange(ctx,
  Endpoints, Client, code, redirectURL, pkceVerifier)` post the grant
  with the client id and secret in the body; `StartDevice(ctx,
  Endpoints, Client, scopes)` and `PollDevice` send the client id
  only, as GitHub's device flow takes. Each is a form-encoded POST with
  `Accept: application/json` (GitHub answers form-encoded without it,
  which is read too). `expires_in`, a number or a quoted one, becomes
  `Tokens.ExpiresAt` on the clock.
- An answer that carries an `error` is that error whatever its status:
  GitHub answers every device-flow and refresh failure with a 200.
  `invalid_grant`, `invalid_client`, `unauthorized_client`,
  `access_denied`, and GitHub's `bad_refresh_token` and
  `incorrect_client_credentials` are a `*connect.Terminal`: the owner
  must sign in again. Anything else is transient.
- `PollDevice` waits the vendor's interval on a clock timer before each
  poll; `authorization_pending` polls again, `slow_down` adds five
  seconds to the interval for good, `expired_token` or the code's own
  expiry is `connect.ErrDeviceExpired`, and `access_denied`,
  `incorrect_device_code`, `device_flow_disabled`,
  `unsupported_grant_type` or any other code ends it as a
  `*connect.Terminal` with the vendor's words. It stops when its
  context ends.
- `AuthorizeURL` builds the authorization address: the endpoint's own
  parameters, the manifest's extra ones, then `response_type=code`,
  the client id, the redirect address, the state, the scopes and the
  S256 challenge of `NewPKCEVerifier`'s verifier (`PKCEChallenge`).
- `Revoke(ctx, Endpoints, Client, Tokens)` posts each token to the
  revocation endpoint; with none it does nothing.
- `RefreshDue(now, expiresAt)` is the margin rule; `Refresher.Do(ctx,
  key, fn)` the one-run-per-key gate.

`connecttest.FakeVendor` is a vendor in an `httptest.Server` on the
test's clock: the device grant, the code grant with PKCE and state
passed back (`/authorize` sends the browser straight back with a
code, or with `AuthorizeError`; the code is good once, for the redirect
address and the challenge it was issued with), refresh with or without
rotation, and revocation. A test with an access token that lasts thirty
minutes and a refresh token, through `/authorize`, is HubSpot's shape.
Its `Knobs` answer each failure a vendor gives (`slow_down`, pending
polls, any device, authorization or refresh error code, a status with
no JSON, an answer that is not JSON), in RFC 6749's style (400, 401 for the client) or GitHub's
(200 with the error), and `OnRefresh` holds a refresh's answer back so
a test can have callers arrive while it is in flight. `Issue` gives a
test a signed-in connection's tokens; `Requests`, `Refreshes`,
`Revoked`, `Valid` and `RefreshAccepted` say what the vendor saw and
holds. `echotest.Echo.SignIn` makes the test built-in a sign-in one.

## Installing a version

An upload (`POST /api/v1/org/integrations`) reads the bundle, runs the
check and stores the version as pending, published by the owner; the
Kivali Developer's `integration_publish` stores one published by the
developer, with its notes, and asks the owner to approve it
([Publishing](#publishing)). Its
install screen (`GET .../versions/{version}`, the web app's
`/org/integrations/<name>/versions/<version>`) says what approving
lets in: who built it and published it, every host, the software its
pod installs (`IntegrationVersion.Packages`, under "Software it
installs", with where it comes from and when it is installed), every
connection field (secrets marked), every action, what the access form
asks, the bundle's size and the check's warnings. Nothing runs before it is
approved.

While another version is in use (`IntegrationVersion.CurrentVersion`),
the screen also says what this one changes from it, under "What
changes from version X": `IntegrationVersion.Changes`, worked out by
`integration.Diff(current, this)`, one line for each of hosts,
sign-in hosts (`Connection.SignInHosts`), software, connection fields,
actions and access form fields added or removed ("Hosts added:
uploads.example.com", "Software added: gh", "Connection fields added:
API key (secret)"). A host is matched by its pattern, a package and an
action by their names, a field by its key. With none of those changed
the list is empty, and the screen says "No change to hosts, software,
secrets, actions or the access form." The same block is on a
superseded version's screen, where it
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

An integration that should run (an approved version, switched on, and
an uploaded bundle) runs in a pod of its own, `integrationpod-<name>`, which core
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
privilege escalation and every capability dropped, except the
`integration` container of a version that declares packages, which
starts as root ([Software](#software)); the pod has no
service-account token, no Service and no ports, `restartPolicy:
Always` (kubelet starts the shim again when it exits with its server),
a 10 s stop grace, and the agent pod's required affinity to the
server's node. Requests 50m/128Mi, limits 1 CPU/1 GiB. Its env is
`KIVALI_INTEGRATION_NAME`, `_VERSION`, `_TOKEN` (the per-pod token
core minted), `_CORE_UDS`, `_BUNDLE`, `_BUNDLE_DIGEST` (the version's
`bundle_digest`, which the shim does not read) and the bare proxy
variables, the
proxy's host resolved to its IP when the pod is built (the pod has no
DNS). The spec names no Secret and carries no connection value, and no
label carries the token. The chart's network policy
`kivali-integrationpod-egress` lets the pod reach the egress proxy's
port and nothing else.

Its server is started with the connection's values
(`integrationRegistry.connectionValues`, the `kivali/credential`
answer).

The spec carries a hash of itself (`kivali-spec-hash`), as agent and
browser pods do. `Start` keeps a pod whose hash matches and replaces one
that differs: a new version, new files (another digest), a new token
or a new image is a new pod.
The registry reads the version's manifest and tells `Start` and `Token`
whether it declares packages, so a pod built without software is never
taken on for a version that installs some. A version without packages
gets the pod described above, which a test pins byte for byte
(`internal/integrationpod/testdata/pod_without_packages.json`).

### Software

A version whose manifest declares `kivali.packages` has them installed
each time its pod starts, before anything of the bundle runs.

- **The pod.** The `integration` container's securityContext is
  `runAsUser: 0`, `runAsGroup: 0`, `runAsNonRoot: false`,
  `allowPrivilegeEscalation: false`, and capabilities `drop: [ALL]`,
  `add:` CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, SETGID, SETPCAP and
  SETUID: what apt-get and dpkg need as root for ordinary packages,
  and SETPCAP for the shim to empty its bounding set as it leaves root
  (`integrationpod.installSecurityContext`). There is no SETFCAP, so a
  package whose maintainer script runs `setcap` may fail to install. A version without
  packages keeps the 65532 container above.
- **The start, in order** (`Shim.prepare`, before the events stream
  opens, so the integration is not running meanwhile, as it is while
  any server starts):
  1. the log gets "Installing gh, jq";
  2. the shim runs itself again as `kivali integration-shim
     install-software gh jq`, a root child that is the agent shell's
     installer (`agentshell.StartInstallCommand`,
     `RunInstallSubcommand`): `apt-get update`, a lookup that each name
     is exactly a package's, then `apt-get install -y -q
     --no-install-recommends` with the names as arguments, never
     through a shell; it kills whatever a package's scripts left
     running before it reports, and the image's `policy-rc.d` keeps
     them from starting services. Its output goes to the log, a line
     each, at level `debug`. The child stops apt-get after
     `agentshell.InstallTimeout` (20 minutes), and the shim gives up on
     a child that has not reported two minutes later, on its clock;
  3. the log gets "Installed gh, jq", or "Could not install <name>
     (<reason>); ... The integration stops and tries again when it
     restarts.", the reason the end of apt-get's error output as the
     agent shell keeps it; a failure ends the shim with status 1;
  4. the shim leaves root for good (`agentshell.LeaveRoot`): every
     capability out of the bounding set, no_new_privs, setresgid and
     setgroups to 65532 and none, setresuid to 65532, every capability
     set emptied, and all of it read back; a failure is logged and ends
     the shim;
  5. it opens the events stream and starts the server on `start`, as
     for any version.

  A shim that ends is started again by kubelet (`restartPolicy:
  Always`, with its back-off), and installs again: an install that
  keeps failing shows in Recent logs, each start's lines after the
  last. A shim that does not start as root and finds packages in the
  manifest logs that it cannot install them and ends.
- **The network.** apt-get reaches `deb.debian.org` through the egress
  proxy with no credential: the child's environment
  (`agentshell.InstallEnv`) carries the pod's bare proxy variables,
  not the server's scoped credential, which the shim registers with
  core only on the events handshake. A request with no credential is
  in the proxy's allowlist mode, held to the team's list, and every
  list holds `deb.debian.org` (`store.PermanentEgressPattern`), as the
  agent shell's own install relies on. So the install needs no change
  to the proxy, the scoped credential's hosts stay the manifest's
  `kivali.hosts`, and the install screen lists those hosts alone, with
  the Software block saying the packages come from Debian's
  repositories.

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
would build it for the approved version and its files
(`Provisioner.Token` reads the token from the pod's spec), so pods
outlive a server restart and their shims reconnect, and a pod built
from other files than the ones stored now is replaced. Shutdown closes every link and stops no pod.

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
version the pod was built for); started as root, it installs the
version's software and leaves root before it opens the events stream
([Software](#software)). It starts the server on `start`:
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
is answered by the shim with -32601; `notifications/progress` goes to
core, which hands it to the setup step it names ([For a
bundle](#for-a-bundle)), and notifications other than it and
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
  value empty, so a connection put later restarts it with them. For a
  sign-in connection both carry a current access token under its
  `credential_keys` name, refreshed first when it is about to expire,
  and never the client secret; the error is -32002 while the owner
  must sign in and -32003 while the vendor does not answer a due
  refresh, and on either the shim starts the server with empty values,
  as with no connection ([The credential path](#the-credential-path)).
- `kivali/log`, `{"level", "message"}`, appends a line to `log.jsonl`
  (level `info`, `warning`, `error`, `debug` or `stderr`; any other is
  `info`; a message is cut to 8 KiB) with every secret value of the
  connection replaced by `[secret]`. The file is capped: past 4000 lines
  it is rewritten with the last 2000. Core adds lines of its own: a
  restart after missed health checks, a pod that cannot be placed for
  want of memory.
- `notifications/progress`, `{"progressToken", "progress", "total",
  "message"}`, is a setup step's stage, handed to the `kivali/setup`
  request whose `_meta.progressToken` it names; any other is dropped.

Core's own requests to the server are MCP's (`initialize` through the
shim, `ping`, `tools/list`, `tools/call`) and, for a bundle whose
manifest names `method: setup`, `kivali/setup`, `kivali/requirements`
and `kivali/uninstall` ([For a bundle](#for-a-bundle)).

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
once. A request waits 60 s for a `tools/call` or a `kivali/uninstall`,
five minutes for a `kivali/setup` step, and 20 s for anything else.

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
calls directly, with no pod, no shim and no Python.

It is built in to the owner: no approval, no upload or publish under
its name, and a version that follows the binary. Uninstall takes one
back to available while the binary carries it, never off the team.

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
type AccessSummarizer interface{ AccessSummary(access map[string]any) string }

// An error of Call or Complete that refuses the call before anything ran.
type Refusal struct{ Text, Reason string }

// Setup, requirements and uninstall: the hooks of an integration whose
// connection its own flow makes (method setup), and of any integration
// with health or an installation of its own.
type SetupIntegration interface {
	Integration
	Setup(ctx context.Context, conn Connection, flow string, last Answer, p Progress) (Step, error)
}
const FlowSetup = "setup"
type Step struct {
	Form *Form; Go *Go; Wait *Wait; Done bool // exactly one
	Keep  map[string]string // into the connection's Secret now, keys from setup.keeps
	State map[string]string // carried to the next Answer, in memory, for this flow
}
type Form struct{ Words string; Fields []string }          // keys from setup.fields
type Go struct{ Words, URL string; Post url.Values; NoReturn, Stateless bool } // a link, or a form that posts Post to URL; NoReturn: nothing comes back; Stateless: it comes back without the state
type Wait struct{ Words, URL, Code string }                // words, a page to open meanwhile, a code to enter there
type Answer struct {
	State    map[string]string // the last Step's State
	Form     map[string]string // what the owner entered: every key the Form asked for
	Return   url.Values        // what the service sent back: untrusted, maybe an earlier leg's replayed
	Checked  bool              // a Wait step's check
	Went     bool              // a Go with NoReturn was sent
	Leg      int               // which call of Setup this is in the flow, from 1
	Callback string            // where a Go step's service may send the browser back
	Team     string            // the team's name, for what setup makes on the service
}
type Progress interface{ Stage(words string, done, total int) }
type RequirementsOf interface {
	Requirements(ctx context.Context, conn Connection) ([]Requirement, error)
}
type Requirement struct{ ID, Title, Words string; Met bool; Fix string }
type Uninstaller interface{ Uninstall(ctx context.Context, conn Connection) error }
type SignInReplacer interface{ RevokeSignIn(ctx context.Context, old SignIn) error }
type SignIn struct{ ClientID, ClientSecret, AccessToken, RefreshToken string }
```

- `Manifest` is the manifest, held to `integration.ValidateBuiltin`
  ([The check](#the-check)): no server section and no
  `kivali.packages`, and no `kivali.hosts` unless its connection is
  made by a setup flow, when `kivali.hosts` names every host its setup
  and its calls reach from Kivali, for the install screen. Otherwise a
  built-in runs inside Kivali and its network reach is Kivali's own,
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
  error in words for the owner. Core calls it only when the manifest
  names `kivali.verify`.
- `Choices` lists an access field's options: `name` is what
  `kivali.access.choices` maps the field to, which need not be a tool.
- `Connection` is the integration's connection, which every call goes
  through: an agent's, Test connection and the options a field lists.
  `Connection.ID` is its id. `Connection.Fields` is every connection field by key, secrets
  included, read from the record and its Secret at the moment of the
  call and kept by no one. The connection's proxy credential is not
  among them. For a sign-in connection the current access token is,
  under its `credential_keys` name, and the client secret and the
  refresh token are not ([The credential path](#the-credential-path)).
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
  egress proxy: its connection gets a scoped proxy credential admitted
  to `Hosts(conn)` and nothing else.
- A `ConnectionChecker` holds a connection to more than its form:
  saving it (`PUT .../connection`, a token flow's Form) calls
  `CheckConnection` after the form's own checks and before it stores
  anything, and answers 400 with the error's words. An `AccessChecker`
  does the same for `PUT .../access/{slug}`, after
  `integration.ValidateAccess`.
- An `AccessSummarizer` built-in's `AccessSummary` is the access in
  words on the Org page and an agent's tab, in place of
  `integration.AccessSummary`'s field by field: for an access form
  whose fields mean something only together. It is given the stored
  access, which may no longer fit the form.

- A `SetupIntegration` is a built-in whose manifest names `method:
  setup` ([Setup connections](#setup-connections)): its `Setup` is the
  flow. Core calls it in the background, off any request, under a
  deadline on its clock: first with an `Answer` holding nothing, then
  after each step with what the step gave back (`Form`, the fields the
  owner entered; `Return`, the query the service sent the browser back
  with, which is untrusted and checked against the service before it
  is relied on; `Checked`, a Wait's check), until it returns a `Step`
  that is `Done` or an error. `flow` is `FlowSetup` for installing, or
  the `Fix` a `Requirement` named. `conn.Fields` holds what earlier
  steps kept. Each `Step` sets exactly one of `Form`, `Go`, `Wait` and
  `Done`, and may `Keep` (into the Secret now, declared keys only) and
  carry `State` (in core's memory, for this flow alone, never written).
  A `Form` names declared fields; a `Go` sends the browser to `URL` on
  a declared host, by a link or by a form that posts `Post`, and the
  service comes back to `Answer.Callback`, or, with `NoReturn` (a
  settings page the owner acts on), nowhere: core then calls `Setup`
  again at once with `Went`, so it can follow with a `Wait`. The
  callback carries a state of the flow's; core issues a fresh one each
  call (a Wait's check is given the latest again) and accepts any it
  issued for the flow while the flow lives, so an address fixed in an
  earlier step still comes back to the flow (a replay of an earlier leg
  arrives as the current Go's `Return`, so `Setup` checks it against the
  service); a return with no state is accepted, by the browser that
  left, only for a `Go` that says `Stateless`. A `Wait` shows `Words`, a
  page on a declared host and a `Code` to enter there, and core calls
  `Setup` again with `Checked` every few seconds while the owner's page
  is open. A step outside the declarations fails the flow; so does an
  error, whose text the owner reads, scrubbed of every kept value.
  `Progress.Stage` names where setup is, with a count where there is
  one.
- A `RequirementsOf` built-in has health of its own: rows met or not,
  each with words and a `Fix`, the flow its Fix runs through `Setup`.
  Core lists its own rows first.
- An `Uninstaller` undoes its installation on the service when the
  owner uninstalls it, before core forgets the Secret and drops every
  agent's access; an error leaves the installation as it is.
- A `SignInReplacer` is a setup built-in whose earlier version made
  its connection by signing in with an app of the owner's: when an
  install takes such a connection over, or Uninstall removes it
  ([Keep](#keep)), core hands it the sign-in the connection held,
  once, off any lock, to revoke with the service, best effort, after
  core's own revocation with the superseded version's endpoint where
  there is one. It is the vendor-specific path for a vendor whose
  revocation is not the standard one, a built-in's hook only.

Core runs `Setup` through the setup runner ([Setup flows](#setup-flows)):
`POST .../setup` of a `setup` connection whose built-in implements
`SetupIntegration`, and the `then` sign-in after it, or a requirement's
Fix ([Fix](#fix)). It asks for `Requirements` when the page, the list or
Home is read ([Requirements](#requirements)), and calls `Uninstall` on
Uninstall ([Uninstall](#uninstall)); the [Dispatch](#dispatch) table
has each. GitHub implements all four ([The GitHub
built-in](#the-github-built-in)). A bundle's server implements the
same three as MCP requests, held to the same declarations ([For a
bundle](#for-a-bundle)).

`Tools`, `Call`, `Verify` and `Choices` run concurrently: many agents
call at once, and each agent's list is asked for in parallel with the
other integrations'. One still running at its deadline (below) is
abandoned and its answer dropped; its context ends, and it must honour
that and stop.

`builtins.All()` (`internal/builtinintegrations/builtins`) is the
built-ins compiled into the binary: Git and GitHub. Each built-in is a package of
its own that imports `builtinintegrations` for its types, so the list
lives in a package apart. `Server.BuiltinIntegrations` is the ones core
calls (`builtins.All()` unless a test sets others).
`internal/builtinintegrations/echotest` is the built-in the tests
register; it is never in `All()`. A test holds every built-in in
`All()` to the check, to a name of its own, and to a `Tools` list
inside its manifest. It also holds each of echo's shapes to the check:
token, sign-in, setup, and setup handing over to a browser or a device
sign-in. Outside setup, echo's setup hooks give no rows and record no
call.

Echo has a token connection, or, with `SignIn`, a sign-in one. With
`SetupFlow` it is a `setup` connection that implements all three setup
hooks, and the web tests drive it over the HTTP API
(`internal/web/integration_setup_echo_test.go`,
`integration_requirements_test.go`). Its declarations are the host
`setup.example`, the fields `account` and a secret, setup-only `key`,
and the keeps `token` and `account`. Its install flow runs a Form
(account, key; asked again while the account is blank), then a Go to
`https://setup.example/install` that posts the key (or links there
when the key is blank), keeps the account and expects `?code=` back.
The step after the return keeps the token the code stands for and
shows a Wait that is checked until `SetInstalled`; then Done, keeping
`ClientID` and `ClientSecret` with `HandOver`. `State` carries the key
alone; each step reads what earlier ones kept from `conn.Fields`. Echo
has no service to trade the code with and takes it as it came back; a
real `Setup` trades it with the service before keeping anything. The `permissions`
requirement's Fix is a one-step flow that grants it. `Requirements`
gives one row met and `permissions`, unmet until granted. `Uninstall`
records the call. Knobs fail the flow (`FailSetup`, with the token and
the key in its words), take a step outside the declarations
(`SetStray`: another host, field or key), fail `Uninstall`
(`FailUninstall`), and hand over to a sign-in (`HandOver`).
`ScriptSetup` and `ScriptRequirements` replace echo's own answers for
the tests of the runner's mechanics and of rows no integration should
give. `Calls` records every call, with each Setup call's flow and
`Answer`.

### Registration at boot

`store.InstallBuiltinIntegrations(manifests, now)` runs at every boot,
right after the built-in skills (`main.go`), and after a restore
(`internal/web/backup.go`). For each built-in it writes:

- `integration.yaml` with `builtin: true`: a new record is switched on,
  installed by `kivali`; an existing one keeps the owner's switch.
- The manifest's version, `approved`, published by `kivali`, notes
  "Ships with Kivali.", with no `bundle/` directory (one an earlier
  Kivali stored under the same version, which shipped the integration
  as a bundle, is removed). Its `manifest.json` is the binary's,
  written again whenever it differs.
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
state is not included, agents' calls of it are refused as not
running, and Test connection and its options answer 409 "<integration>
is not in this version of Kivali, so nothing runs it; you can uninstall
it". Uninstall removes it (`store.RemoveStaleBuiltinIntegration`,
which the API uses only for such a record).

The owner cannot approve, reject or roll back to a built-in's version:
the store refuses (`store.ErrIntegrationBuiltin`) and the API answers
409 "<integration> ships with Kivali, and its version changes only with
Kivali's updates; there is nothing to approve" (to reject, to roll
back to). An upload is a 409 ("... ships with Kivali, so a bundle
cannot replace it"), and so is a publish under its name. Uninstall
takes a built-in back to available ([Uninstall](#uninstall)).
Every one of these names the integration by its display name
(`builtinRefusal`, `internal/web/api_integrations.go`).

#### Migration

Right after `store.InstallBuiltinIntegrations`, at boot and after a
restore, `store.MigrateIntegrationConnections` gives every integration
one connection (`internal/store/integration_connections_migrate.go`).
Git's records from a Kivali that let it hold several connections, each
with a label, and access rows naming theirs by `connection_id`, are
brought to that shape without deleting anything:

- With several records under `connections/`, the one most access rows
  name is kept (ties: the oldest). Each other record moves, as it is, to
  `connections-retired/<id>.yaml`, never over a record already there
  (that is an error, and the move stops); its Secret stays. Nothing
  calls through it again: boot's `RegisterBuiltinEgress` drops its
  scoped proxy credential, so it reaches nothing. A shell job handed
  out through it before the upgrade and ended after is scrubbed with
  its Secret's values too, and audited "(it ran through a connection
  set aside)" ([A call, in two requests](#a-call-in-two-requests)).
- A row whose `connection_id` names the kept connection is written
  again without it. A row naming any other connection is set aside:
  it keeps its `connection_id` on disk and gains `retired`, the reason
  it grants nothing, "set for the connection <label, else host>, which
  Git no longer has" (or "it was set for a connection that is no longer
  there" when no record set aside has that id). Every read shows it as
  the row's `invalid`, the mechanism a changed access form uses, whose
  words (the page's, the agent's refusal) say to set it again, until
  the owner does.

It runs on every integration, so a one-connection integration's row
left naming an earlier connection stays granting nothing, as it did.
Every record is moved before any row is written: should boot stop half
way, or a move fail, the next run finds the same connection to keep and
rows still naming theirs, and finishes. Until it does, the integration
holds more than one record, and no row may reach any of them: every
row reads "<Integration> holds more than one connection record and
Kivali could not bring it to one; see the log",
`store.GetIntegrationConnection` answers
`ErrIntegrationConnectionsSeveral` (so no call is made), the page shows
the oldest record with the `connection` requirement not met and no
Fix, Test connection and the options answer 409, and Uninstall still
works. A second run changes nothing. What it did is logged:
"integration git: one connection per installation: kept connection
<id>, set aside <ids>; access to set again: <slugs>"; a failure is
logged at boot ("integrations: ...") and after a restore.

A new connection's id is never one a record set aside has
(`store.IntegrationConnectionIDTaken`), so its Secret's name is its
own. Uninstall forgets the connections set aside with the connection
([Uninstall](#uninstall)).

The migration is non-destructive, not reversible in place: an older
Kivali started on the migrated data reads the one connection kept, and
every access row, now naming no connection (or naming a connection set
aside), as invalid until the owner sets it again.

GitHub 1.0.0 (a bundle an earlier Kivali shipped and ran in a pod) is
GitHub 2.0.0 here, a built-in of Go code. Boot's
`InstallBuiltinIntegrations` makes 2.0.0 the version in use and 1.0.0
superseded; the reconcile stops 1.0.0's pod, since it runs no pod for
a built-in record. 1.0.0's connection, the owner's own GitHub App
signed in with (its client id in the record; its client secret and
the sign-in's tokens in the Secret), does not fit 2.0.0's setup flow,
which makes its own app (`setupMisfit`, [Keep](#keep)): its record
and Secret are left as they are, GitHub reads available with one unmet
`connection` row ("Set up GitHub again: ..."), agents' calls are
refused by GitHub's own check of the connection ("GitHub's connection
is not set up; ask the owner to set it up again."), and the access
rows stay valid (the access form is unchanged). The owner's install
takes the record over at its first Keep, after GitHub made the new
app: the 1.0.0 values go, the earlier app's grant is revoked with
GitHub (`RevokeSignIn`), and once installed every agent's access
works again. The app the owner registered for 1.0.0 stays on GitHub.
The page offers Uninstall meanwhile: it removes the earlier connection
and every agent's access, and revokes the earlier app's grant as the
install would. With several connection records (a migration that
could not finish), no install takes the connection over. Started on
this data, a Kivali that ships GitHub 1.0.0 reads 2.0.0's
connection as its own and finds no client id there, so GitHub needs
connecting again.

### Dispatch

A built-in of Go code is an integration whose record is built in and
whose name is in `Server.BuiltinIntegrations` (`Server.builtinFor`).
Where core would reach a pod, it calls the function instead
(`internal/web/integration_builtin.go`):

| Where | A bundle | A built-in of Go code |
| --- | --- | --- |
| Running | its shim is connected | while its code is in the binary |
| An agent's list | `tools/list` with `_meta`, kept on the link | `Tools(caller)`, every time |
| An agent's call | `tools/call` through the shim | `Call(caller, connection, name, args)`, with the connection the agent's access row names |
| Test connection | `tools/call` of `kivali.verify` | `Verify(connection)`, with the connection the route names; the audit's action is the name `kivali.verify` gives (`whoami` for echo) |
| An access field's options | `tools/call` of the field's tool | `Choices(connection, name)`, with the connection `?connection=` names or the first; 409 with no connection |
| Work in the agent's shell | none | `Call` answers with `ShellJob`, `Complete` the call's second request |
| Setup (`method: setup`) | `kivali/setup` with the flow and the answer, through the shim, its progress notifications the flow's stages, each step held to `kivali.connection.setup` ([For a bundle](#for-a-bundle)) | `Setup(conn, flow, answer, progress)`, in the background through the setup runner, each step held to `kivali.connection.setup` |
| Requirements | `kivali/requirements` of a setup bundle while its server runs, under a 5 s deadline, as the integration's rows after Kivali's | `Requirements(connection)` of the connection, under a 5 s deadline, as the integration's rows after Kivali's; unmet ones are Needs-you rows |
| Uninstall | `kivali/uninstall` of a setup bundle while its server runs; an error is a 502 that changes nothing | `Uninstall(connection)` of the connection before anything is forgotten; an error is a 502 that changes nothing |
| A connection change | the shim restarts the server | nothing to restart; a `HostsOf` built-in's proxy credential of that connection is registered again |
| Reconcile | a pod while it should run | skipped |

Each function runs under the same deadline a pod's request has (60 s
for a call, Test connection and options, 20 s for a list), on core's
clock; past it the function's context ends and the answer says the
integration did not answer in time. A function that panics fails with
an error. Every answer is read as the MCP result a server would send,
so the agent, the scrubber and the audit see what they see for a pod.

### What the owner sees

The list and the page show a "Built in" badge. A built-in of Go code
is available, setting up, installed or off: never pending, and with no
`running` requirement, since it runs while its code is in the binary.
Test connection, for a built-in that has one, works once it is
installed. The version is the built-in's manifest's, published by
Kivali; the install screen (`IntegrationVersion.Builtin`) says it
ships with Kivali, shows no "Replaces version" line or "What changes
from version" block, no software and no bundle, offers no Approve,
Reject or Roll back, and says it reaches the hosts its connection
names (a setup built-in lists its `kivali.hosts`); the page's version
list offers no Roll back. A built-in record this binary does not carry
shows "Not in this version of Kivali" (`IntegrationStateNotIncluded`),
says agents cannot use it, and offers Uninstall, which removes it.

### The git built-in

Git (`internal/builtinintegrations/gitintegration`, name `git`,
version 2.0.0) lets agents clone, fetch and push repositories over
HTTPS with the owner's token. Everything local is plain git in the agent's shell,
which reaches only the team's allow list. Its manifest:

- `user_config`: `host` (required; "github.com, gitlab.com or any HTTPS
  git host, without https://") and `token` (sensitive, required).
- `kivali.connection.method: token`: one connection, any HTTPS host
  and a token for it, which every agent's call uses. No
  `kivali.hosts`, and no `kivali.verify` ([No Test
  connection](#no-test-connection)). A Git that held several
  connections keeps one at boot ([Migration](#migration)).
- `kivali.access.schema`, in this order: `pull_repositories` (a list,
  "Repositories it may pull": clone and fetch); `push_repositories` (a
  list, "Repositories it may push to": clone, fetch and push);
  `push_refs` (a list, "Branches and tags it may push", in the
  repositories it may push to); `force` (a switch, "May force-push",
  marked `x-kivali-danger`). Push is granted per repository: a
  repository in `push_repositories` may be pushed to, one only in
  `pull_repositories` may not. No field is required, since the form
  cannot ask for one of two lists; `CheckAccess` asks for a repository
  in either ([Checks at save](#checks-at-save)). An access set under
  1.x (`repositories` and a `push` switch over all of it) does not fit
  2.0.0's form, so once boot supersedes 1.x the store marks it invalid
  and it grants nothing until the owner sets it again.
- `tools`: `clone`, `fetch` and `push`, which agents call as
  `git_clone(url, path)`, `git_fetch(path, url?, refs?)` and
  `git_push(path, refs, force?, url?)`. All three are listed to every
  agent with access; each call is checked against the access.
- `Git.AccessSummary` (`AccessSummarizer`): "Pull: acme/* · Push:
  acme/widgets · Branches and tags: agent/* · May force-push".
  `push_refs` and `force` without a repository to push to, and `force`
  without a ref to push, are left out, since they grant nothing; a
  repository to push to with no refs reads "Branches and tags: none".

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

The access's `pull_repositories` and `push_repositories` are patterns
matched against the path with `path.Match`: `*` is the only wildcard
(`?`, `[...]` and `\` are refused, since a class can match a `/`), it
stands within one segment and never across a `/`, case is ignored, and
an entry without it is the one repository it names
(`gitrepo.ValidPattern`, `gitintegration/access.go`). A clone or a
fetch may reach a repository in either list; a push only one in
`push_repositories`. An access whose two lists are both missing or
empty, or either of which holds anything but valid patterns, refuses
every repository.

#### Branches and tags a push names

`internal/gitrepo/refs.go`, used by core and by the daemon alike:

| Written | Is |
| --- | --- |
| `agent/x`, `main`, `v1.2.0` (no `refs/`) | the branch `refs/heads/<name>` |
| `tags/v1.2.0` | the tag `refs/tags/v1.2.0` |
| `refs/heads/...`, `refs/tags/...` | as written |
| anything else under `refs/` (`refs/notes/x`, `refs/remotes/...`) | refused as a destination |
| a pattern: `agent/*`, `tags/v*`, `refs/heads/*` | normalised the same way |
| the pattern `*` | `refs/heads/*` and `refs/tags/*` |
| `[+]<src>:<dst>` | a refspec: `dst` a branch or a tag as above; `src` a ref (`refs/...` as written, a bare name a branch, `tags/...` a tag), `HEAD`, or a 40- or 64-hex object id; `+` forces |
| a bare ref `x` | the refspec `refs/heads/x:refs/heads/x` (a tag the same) |
| `:<dst>` (empty `src`) | a deletion: always refused (`gitrepo.ErrDeleteRef`) |

A pattern matches the whole ref with `path.Match`, with `*` as the
only wildcard (`?` and `[...]` are refused): it stands within one
segment, so `agent/*` matches `refs/heads/agent/x` and not
`refs/heads/agent/a/b` (`agent/*/*` does), and case counts, as it does
in git. A name git's `check-ref-format` refuses (`..`, `@{`, a space,
`~^:?[]\`, a segment starting with `.` or ending `.lock`, `HEAD` or a
leading `-` as a branch or tag name) is refused, and so are wildcards
in a refspec. An existing tag is never moved without force: core
cannot know the remote's tags, so the daemon refuses it. The access's `push_refs` are read with
`gitrepo.NormalizeRefPatterns`: missing or empty grants no ref, and an
entry that does not read refuses every ref.

#### A call, in two requests

Core has no shell of the agent's, so a clone, a fetch or a push takes
two requests on `core.sock`, as the smoke test does
([The smoke test](#the-smoke-test)):

1. The agent's MCP server sends the call (`IntegrationCallRequest`).
   Core works it out as any call ([A call](#a-call)) and calls
   `Git.Call`, which checks the connection (a host, a token), the
   access, the arguments and the repository, and for a push its refs
   (below), and answers with a `builtinintegrations.Refusal` or a shell
   job. Core puts the connection's proxy credential in the job, writes
   an audit row (allowed, the action, `started: <owner/name>`, or
   `started: the working copy's origin at <path>` for a fetch or a push
   without a `url`; a push's adds `, push of <refs>` or `, force push
   of <refs>` from core's own reading of the refs) and answers
   `IntegrationCallResponse{shell_job}`. A refusal is audited and
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
   second audit row: allowed, the action `clone`, `fetch` or `push`,
   and the reason `<owner/name>: updated <refs>` (at most ten named,
   then how many more), `<owner/name>: up to date`, or the repository
   and why it failed. A push's reason is `<owner/name>: pushed <refs>`
   (`force-pushed` when it forced), `<owner/name>: up to date, <refs>`,
   or `<owner/name>: push of <refs>: <why>` (`force push of`); the refs
   it names are those of the shell's report that core asked for. A push
   whose access changed while it ran, so that the check now refuses it,
   is still audited allowed when the result says refs landed: `pushed
   <refs> to <owner/name>; the access changed during the push`, and the
   agent reads what landed and that its access changed; with nothing
   landed the refusal stands. A job that ran through another connection
   than the integration has now (the job's `connection_id`, which the
   daemon echoes in the result, is not the connection's id: Git was
   uninstalled and installed again while it ran) is never refused and
   not checked against the new connection, whose host and token are
   not the job's: the agent reads what ran and that the connection
   changed, and the reason is `<owner/name>: <what ran>; the connection
   changed during the <clone, fetch or push>`, then `(it ran through an
   earlier connection)` when Kivali still has that connection's record,
   whose secrets are scrubbed from the answer and the reason with the
   current connection's; or, with the record gone, `(it ran through a
   connection that is no longer there, so only the current
   connection's secrets were scrubbed)`. The row counts from the job's
   start (the result's `duration_ms`, at most 15 minutes). A second
   request that never comes leaves the first row, so every job that
   carried the token out is in the audit.

A push is checked in core, in this order, before any job is built:
`push_repositories` must name a repository; `push_refs` must read;
each entry of `refs` must read as a ref or a refspec
(`gitrepo.ParsePushRefSpec`), delete nothing and name a destination no
other entry does; each destination must match `push_refs`; an entry
that forces (`force: true` puts `+` on every entry, or the entry's own
`+`) needs `force` on; and a `url`'s repository must be in
`push_repositories`. The job carries `allowed_repositories` (only
`push_repositories`, so a push without a `url` reaches the working
copy's origin only when the agent may push to it), the refspecs
normalised (`[+]src:dst`), `allowed_refs` (the access's `push_refs`
normalised) and `force` only when an entry forces, so the daemon can
check the push again
([The daemon's `POST /v1/git`](#the-daemons-post-v1git)). A clone's
and a fetch's `allowed_repositories` are both lists.

The result is untrusted text from the agent's pod: it becomes only the
agent's answer and the audit's reason, both scrubbed of the
connection's secrets and its proxy credential, and nothing in core acts
on it but its `connection_id`, which only adds that connection's
secrets to the scrubber and a note to the audit, so no nonce binds
it to the job. For a fetch without a `url` the
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
	Op, Path, URL       string   // op: clone, fetch or push; url empty: the working copy's origin
	RefSpecs            []string // json: refspecs; a fetch's, or a push's normalised
	Force               bool     // a fetch: a ref may move other than fast-forward; a push: + is allowed
	AllowedRepositories []string // the access's patterns, for the daemon's own check; a push's: push_repositories only
	AllowedRefs         []string // a push's: the access's push_refs, normalised
	Host                string   // the connection's, made exact
	Auth                GitAuth  // {username: x-access-token, password: the token}
	Proxy               GitProxy // {username: integration-git.<id>, password: the secret}
	TimeoutSeconds      int      // clone 900, fetch and push 600
	ConnectionID        string   // json: connection_id; the connection the job runs through
}
type IntegrationShellJobResult struct{ Skipped string; Git *GitResult }
type GitResult struct {
	Repository   string
	Updated      []GitRefUpdate // {ref, old, new}; old empty for a new ref; at most 200; a push's: the remote's refs
	UpdatedTotal int            // json: updated_total; every ref that moved
	Error        string         // at most 4 KiB
	DurationMs   int64
	TimedOut     bool
	ConnectionID string         // json: connection_id; the job's, echoed by the daemon
}
```

Clone and fetch set no `force` and no `allowed_refs`. Nothing on the
job's path logs it: not core, not the MCP server, not the daemon.

Git's refusals name the cause and the owner only:

| Cause | The agent reads | The audit's reason |
| --- | --- | --- |
| A URL on another host | The Git integration's connection is for <host>; give a repository's URL on it, or owner/name. | the URL is on another host than the connection's |
| A repository that does not read | Kivali cannot read the repository: <why>. Give its HTTPS URL on <host>, or owner/name. | the repository does not read |
| Outside the access | The Git integration's access for you does not include <owner/name>. Ask the owner if your work needs it. | the access does not include <owner/name> |
| An access that names no repositories | Your access to the Git integration names no repositories Kivali can read. Ask the owner to set it again. | the access names no repositories |
| A connection host that is no host | The Git integration's connection has no host Kivali can use. Ask the owner to fix the connection. | the connection's host is not a host name |
| No token | The Git integration's connection has no token. Ask the owner to connect it again. | the connection has no token |
| Arguments the action does not take | git_clone takes url and path, both strings. (git_fetch: path, and optionally url and refs; git_push: path and refs, and optionally force and url) | the arguments do not read |
| A push with no repository to push to | The Git integration's access for you does not allow pushing. Ask the owner if your work needs it. | the access does not allow pushing |
| A push to a repository it may only pull | The Git integration's access for you lets you pull <owner/name> but not push to it. Ask the owner if your work needs it. | the access does not allow pushing to <owner/name> |
| `push_refs` that do not read | Your access to the Git integration names branches or tags Kivali cannot read. Ask the owner to set it again. | the access's branches and tags do not read |
| A ref that does not read | git_push cannot read "<ref>": <why>. Each entry is a branch (agent/x), a tag (tags/v1.2.0) or a refspec such as HEAD:refs/heads/agent/x. | a ref does not read |
| A deletion | Deleting a remote ref is not something the Git integration does. | the call deletes a remote ref |
| A destination twice | git_push names <ref> twice; push each destination once. | a destination is named twice |
| A destination outside `push_refs` | The Git integration's access for you does not include refs/heads/main. Ask the owner if your work needs it. | the access does not include refs/heads/main |
| A force without `force` | The Git integration's access for you does not allow force-pushing. Ask the owner if your work needs it. | the access does not allow force-pushing |

The answers: "Cloned <owner/name> into <path>. Its origin is <url>; use
plain git there for everything local, and git_fetch to fetch with the
connection." or "Fetched <owner/name> into <path>.", then the refs that
moved (at most 50, `<ref> <old>..<new>` or `<ref> new at <new>`) or
"Nothing changed: the working copy was up to date." A push answers
"Pushed refs/heads/agent/x (new) and refs/tags/v1 to <owner/name>."
("Force-pushed" when it forced), then the same lines, or "Nothing to
push: <owner/name> already has <refs> as the working copy does." An error answer
says nothing ran for want of a shell, that the operation did not finish
in time and was stopped, or that it did not go through, with the host's
or the daemon's words, cut to 1000 bytes.

#### The daemon's `POST /v1/git`

`internal/agentshell/git.go` runs the operation in the daemon's own
process with go-git v5 (`github.com/go-git/go-git/v5`). The body is a
`GitRequest` (64 KiB at most), the answer a `GitResponse`
(`{repository, updated, updated_total, error, duration_ms,
timed_out, connection_id}`, the last the request's, echoed). The answer is bounded so it always fits the agent's
next request to core, whose body is capped at 16 MiB: `updated` lists
at most 200 refs (`GitUpdatedMax`), by name, `updated_total` counts
every ref that moved, and `error` is cut to 4 KiB (`GitErrorMaxBytes`)
after it is scrubbed. The bound is the request's `timeout_seconds`, at
most 15 minutes for a clone and 10 for a fetch or a push, 5 when it
gives none, on the daemon's clock; past it the operation's context
ends and the answer is `timed_out`. Jobs on one working copy run one
after the other, by its resolved path (two fetches into one `.git` at
once could leave its refs half written); the wait counts against the
bound.

- **Where.** `path` is absolute under the daemon's root (`/files`) or
  its HOME (`/scratch/home`), `~/...` for the home, or relative to the
  root, as a command's cwd is; the root and the home themselves are
  refused. The existing part of the path is resolved through its
  symbolic links and must stay under the same base. A clone needs a
  directory that does not exist or is empty; when the clone fails,
  go-git removes that directory if it made it, or empties it if it was
  there, and leaves any parent directories it made; a fetch needs a
  working copy whose `.git` is a directory (a `gitdir:` file or a link
  is refused), and so does a push.
- **What it reaches.** A clone's URL is the job's, parsed by the same
  rules on the job's host, and must be one of `allowed_repositories`.
  The clone's origin is that URL, with no user info, so plain git keeps
  working for a repository the team's allow list reaches. A fetch's URL
  is the job's, by the same rules, or, with none, the working copy's
  origin as written in `.git/config` (read raw, so no
  `url.<base>.insteadOf` rewrites it; exactly one URL), parsed by the
  same rules and refused unless it is on the job's host and one of
  `allowed_repositories`. A push's URL is found as a fetch's, so
  origin's `pushurl` is never read. `.git/config` is the agent's to
  write, so nothing else in it is read. Every request goes to that URL:
  go-git's http and https transports are installed with
  `githttp.NoFollowRedirects` (`client.InstallProtocol`, in the
  daemon's `init`), so a host that answers with a redirect (GitHub does
  for a renamed or transferred repository) fails the operation, a push
  included, and the agent reads "the repository moved; ask the owner to
  update the access". The tests serve a 301 from one repository's path
  to another and see nothing reach the other.
- **How it fetches and pushes.** Through an anonymous remote
  (`CreateRemoteAnonymous` with the URL in code, and `RemoteName:
  "anonymous"` on the fetch or the push; go-git refuses a push whose
  `RemoteName` is not the remote's). The daemon's tests pin, against a
  poisoned working copy, that through the anonymous remote the
  repository's own `insteadOf` and `pushInsteadOf` rules and
  `[remote "origin"]` URL and `pushurl` are inert, each with a
  control: a fetch through the named remote follows the rule to the
  other server when origin is the good one, and a fetch or a push
  through it follows origin when origin names the other server. They
  also assert that a global `~/.gitconfig` rule to the other server has
  no effect on a clone, a fetch or a push (go-git reads no global
  configuration here) and that no hook runs, a `pre-push` that refuses
  among them. A named remote (`RemoteName: "origin"` without
  `RemoteURL`) follows the configuration: the daemon never uses one.
- **Refspecs.** None fetches `+refs/heads/*:refs/remotes/origin/*`.
  Each given must parse (`config.RefSpec.Validate`), have a source, and
  write under `refs/remotes/origin/` or `refs/tags/`, cleanly (no `..`),
  so a fetch never moves a local branch. Tags are fetched
  (`git.AllTags`). Already up to date is success with nothing moved. The
  refs that moved are worked out from every ref before and after.
- **A push's refspecs.** Each is held again to the rules core held it
  to, as the URL is to `allowed_repositories`: it parses
  (`gitrepo.ParsePushRefSpec`), deletes nothing, writes a destination
  `allowed_refs` matches, names a destination no other one does, and
  has a `+` only when the job's `force` is set. None is an error. Each
  source is resolved in the working copy first, so a source it does not
  have is an error, not a push of nothing: a ref naming an object
  directly is given to go-git by name (an annotated tag goes as the
  tag), and `HEAD`, a symbolic ref or an object id as the hash of the
  commit it names, the decoded commit's (go-git computes it from the
  content, so a commit stored under another name is refused). go-git
  reports no ref of a push and does not say whether the host takes an
  atomic push, so the daemon first reads the host's receive-pack
  advertisement (through the same transports, auth and proxy): its
  refs, which each destination moves from, and its capabilities.
- **What moves without force.** `PushOptions.Force` stays off; a
  refspec forces by its own `+`. For each refspec without it whose
  destination the remote has with another object, before anything is
  sent: an existing tag is refused ("refs/tags/v1.2.0 exists; moving a
  tag needs force"), since go-git checks a tag as it checks a branch;
  and a branch must move to a descendant of the remote's commit, by the
  daemon's own walk of parents (not go-git's) through `verifiedObjects`,
  a storer that hashes each object it reads (`plumbing.ComputeHash`)
  and refuses one whose content does not match its name ("the working
  copy's objects do not match their names"). The working copy's objects
  are the agent's to write, and go-git's storer never checks them, so
  a forged commit claiming the remote's tip as its parent would pass
  go-git's own check. A remote commit the walk does not reach (a
  shallow clone without it, or a remote that moved on) is "not a
  fast-forward".
- **Atomic.** The push sets `PushOptions.Atomic`, so a host that
  refuses one ref (a protected branch, its `update` or `pre-receive`
  hook) refuses all and the audit's "push of a, b: <why>" is true.
  go-git falls back to a plain push, ref by ref, when the host does not
  advertise `atomic`, so the daemon refuses a push of more than one ref
  to such a host itself ("the host does not take atomic pushes, so a
  push of several refs could land in part; push one ref per call");
  `git http-backend`, GitHub and GitLab advertise it.
- **What it reports.** Each destination whose object changed, old from
  the advertisement (empty for a new ref) and new the object pushed.
  Already up to date is success with nothing moved. A push the host or
  go-git refuses (the host's hooks, a token that cannot write) is the
  answer's `error`, in their words, scrubbed (`GitScrubber`); a
  redirect is "the repository moved; ask the owner to update the
  access", as for a clone or a fetch. Core names in the answer and the
  audit only the refs of the report that it asked for.
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
its host (without the port). The connection of such a built-in has a
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
- Saving the connection (`PUT .../connection`) mints its secret when
  its Secret holds none, keeps it otherwise, and registers it on every
  save while the integration is on, so a changed host is registered at
  once. Uninstall, forgetting a failed install's connection, switching
  the integration off and removing it drop it
  (`DropEgressScopedCredential`); switching it on registers it again.
  The switch takes the same lock (`lockIntegrations`) as the save, so
  a save racing a switch registers or drops the credential by the
  switch as it ends up. Every boot (`Server.RegisterBuiltinEgress` in
  `main.go`) and every restore registers the connection of each
  switched-on built-in again from its Secret, since
  `egress_agents.json` is out of backups and the Secret is not, drops
  the credentials of those switched off, and drops those of every
  connection set aside ([Migration](#migration)). A job
  whose connection's Secret holds no secret mints and registers one
  first.

#### Checks at save

Git is a `ConnectionChecker` and an `AccessChecker`. Saving a
connection whose host `gitrepo.NormalizeHost` cannot read (a path, user
info, a scheme other than `https://`) is a 400, "Host is a host name
such as github.com, without https:// or a path", and nothing is
stored. Saving an access with a `pull_repositories` or
`push_repositories` entry that is not a pattern `gitrepo.ValidPattern`
reads is a 400, "<entry> is not a repository or a pattern"; one that
names no repository in either list is a 400, "Name a repository it may
pull or push to"; and one with a `push_refs` entry that
`gitrepo.NormalizeRefPattern` refuses is a 400, "<entry> is not a
branch, a tag or a pattern". A stored row that is not this all the same (one written
before the check, or by hand) refuses every repository.

#### No Test connection

Git's manifest names no `kivali.verify`, so its page offers no Test
connection, and core never reaches a git host itself: every request
that carries the token leaves from the agent-shell daemon through the
egress proxy, held to the connection's host. Whether a connection
works shows in an agent's first clone, fetch or push, and in its audit
row. `Git.Verify` exists only because `builtinintegrations.Integration`
requires it, and answers an error. The `Git` struct's one field,
`InsecureHTTP`, is a test hook; the tests never reach a real host.

Not supported: SSH remotes, Git LFS (a clone holds the pointers),
submodules (not cloned), and deleting a remote ref.

### The GitHub built-in

`internal/builtinintegrations/githubintegration` (name `github`,
version 2.0.0, in `builtins.All()`) is GitHub as a built-in of Go
code: agents read and open issues, comments and pull requests as a
private GitHub App its setup makes on the owner's account or an
organization's, through GitHub's REST API. The package implements
`Integration`, `SetupIntegration` (`setup.go`), `RequirementsOf`
(`requirements.go`), `Uninstaller` and `SignInReplacer`
(`uninstall.go`); not `ShellJobIntegration` or `HostsOf`, since no
action works in the agent's shell or moves commits.

- **The manifest**: `kivali.connection.method: setup`, with the
  `setup` block of [Setup connections](#setup-connections) (the
  summary, which ends with what Uninstall leaves on GitHub;
  `hosts: ["github.com"]`; one field, `account`, "An organization's
  name, or leave it blank for your personal account"; keeps `app_id`,
  `slug`, `client_id`, `client_secret`, `private_key` and
  `installation_id`; no `then`), an empty `user_config`,
  `kivali.hosts: ["api.github.com", "github.com"]`, `kivali.verify:
  whoami`, and GitHub 1.0.0's access form, so agents' access carries
  over: `repositories` (required) and `write`, "May create and
  comment". It passes `integration.ValidateBuiltin`.
- **The connection** is what setup keeps, read as `conn.Fields`.
  `app_id`, `private_key` (PEM: PKCS#1 `RSA PRIVATE KEY` as GitHub
  issues it, or PKCS#8 `PRIVATE KEY` holding an RSA key) and
  `installation_id` are what a call needs, the two ids whole numbers;
  `client_id` and `client_secret` are read by no action. A connection
  missing any of the three, or whose key or ids do not read, refuses
  every call with "GitHub's connection is not set up; ask the owner to
  set it up again.", its reason in the audit (`the connection is
  incomplete`, or what does not read), and nothing reaches GitHub.
  Core scrubs every kept value from what the owner and the agents read
  ([Scrubbing and the audit](#scrubbing-and-the-audit)), the slug and
  the ids among them, so no words of the built-in's name one: they
  name the app by its `name` (GET /app) and the account by its login.

#### Setup

`Setup` with `FlowSetup` is GitHub's app-manifest flow, two clicks on
GitHub's own pages and nothing copied:

1. **Form** `account`, asked again (with words saying why) when it is
   not a GitHub account's name (letters, digits and hyphens, not
   starting with one, at most 39; an `@` before it is dropped). Blank
   is the owner's personal account.
2. **Go** by POST to `https://github.com/settings/apps/new?state=<state>`
   (an organization's: `https://github.com/organizations/<account>/settings/apps/new?state=<state>`),
   the state the one in `Answer.Callback` (on this address alone, which
   GitHub sends back with the code), with the form field
   `manifest`: `{"name": "Kivali <team>", "url": <the team's address>,
   "redirect_url": <the team's /connect/callback, no query>, "setup_url":
   <the same>, "setup_on_update": true, "public":
   false, "hook_attributes": {"active": false},
   "default_permissions": {"metadata": "read", "contents": "read",
   "issues": "write", "pull_requests": "write"}, "default_events": []}`.
   The name is `Answer.Team` after "Kivali" ("Kivali" alone for a team
   with no name), cut to the 34 characters GitHub takes. The words say
   GitHub may ask the owner to change the name (names are unique across
   GitHub, and GitHub's form lets the owner edit it before Create
   GitHub App) and, for a team on plain `http`, that GitHub may refuse
   the address and to give the team an `https` one under Other devices.
   Nothing is kept yet.
3. GitHub sends the browser back with `code` and `state`. Progress
   "Creating the app on GitHub". The code (one use, within the hour;
   at most 200 characters, never a path) is traded with no credential,
   `POST /app-manifests/{code}/conversions`, and kept nowhere; of the
   answer, the app's `id`, `slug`, `client_id`, `client_secret` and
   `pem` are kept (`app_id`, `slug`, `client_id`, `client_secret`,
   `private_key`), each checked first (an id, a slug of lowercase
   letters, digits and hyphens, a client, a key that reads), and the
   `webhook_secret` dropped. A 404 says the code works once, within an
   hour; any other failure gives GitHub's words, and nothing is kept.
4. **Go** (`Stateless`) to `https://github.com/apps/<slug>/installations/new?state=<state>`,
   where the owner chooses the account's repositories. GitHub sends the
   browser to the setup address with `installation_id` and
   `setup_action`; whether it passes the state on is GitHub's, so the
   return is taken by the flow's cookie when it does not.
5. Progress "Waiting for the installation". The `installation_id` the
   browser brought is not trusted (anyone can load the setup address
   with one): the app's installations are listed with its JWT (`GET
   /app/installations`). A private app installs only on its owner's
   account, so the list holds one, whose id is kept: **Done**. With
   more than one, the id the browser brought picks among them, else
   the flow fails. With none: after `setup_action=request` (an
   organization's owners approve installations), or on a check, a
   **Wait** with no page (the new-installation page's address holds
   the slug, which core scrubs from what a step shows), checked until
   one is listed; otherwise the Go of step 4 again ("GitHub lists no
   installation of the app yet"). A passing failure (no answer, a rate
   limit, GitHub's 5xx) keeps waiting; a key GitHub refuses fails the
   flow in its words.

`State` holds a marker of the step alone ("form", "make", "inst",
"wait", "went"), shorter than the six characters core scrubs. The
Fixes are flows of their own:

- `install`: step 4's Go and step 5, keeping the new installation id.
- `permissions`: a Go with `NoReturn` to where the missing permissions
  are granted: the app's own settings
  (`https://github.com/settings/apps/<slug>/permissions`, or the
  organization's) when the app does not ask for one this version needs
  (GET /app), else the installation's
  (`https://github.com/settings/installations/<id>`, or
  `https://github.com/organizations/<login>/settings/installations/<id>`),
  where its owner approves what the app asks for; then a Wait on that
  page, checked until the installation's `permissions` cover this
  version's. Already covered, it is done at once.
- `settings`: a Go with `NoReturn` to the installation's settings, then
  a Wait checked until it is not suspended and reaches a repository.

#### Requirements

`Requirements` asks GitHub, with the app's JWT and within the runner's
five seconds, at most four requests (GET /app, GET
/app/installations/{id}, and for an installation of selected
repositories an installation token and GET
/installation/repositories?per_page=1 for `total_count`), and caches
nothing beyond core's three seconds. In order:

| id | Title | Met | Not met, and its Fix |
| --- | --- | --- | --- |
| `key` | The app's key works | "GitHub accepts the key of the app <name>. Uninstalling GitHub here leaves the app on GitHub, for you to delete under <the account's apps page>." | values missing, or a 401 from GET /app: "GitHub refused the app's key (HTTP 401): ... Set GitHub up again.", Fix `setup`; nothing after it is asked |
| `installed` | Installed on GitHub | "Installed on <account>." | a 404: "The app is no longer installed on GitHub.", Fix `install`, nothing after it; suspended: "The app's installation on <account> is suspended on GitHub.", Fix `settings` |
| `repositories` | Repositories it reaches | "Installed on <account>, reaching all repositories." or "reaching N repositories." | none: Fix `settings`; left out while suspended |
| `permissions` | Permissions current | "The installation has every permission this version needs." | "The installation lacks permissions this version needs: issues: write, ... Approve them on GitHub.", Fix `permissions` (write covers read, admin both) |

Any other failure is an error, which core shows as its one `checks` row.

#### Uninstall

`Uninstall` is `DELETE /app/installations/{installation_id}` with the
app's JWT. A 404 (gone already) and a 401 (GitHub no longer takes the
key: the app was deleted there) are done, and so is a connection
missing a value; any other failure is an error that leaves everything
as it is ("GitHub did not remove the app's installation: ..."). The
app's registration stays on GitHub: the summary and the `key` row say
so and where to delete it.

`RevokeSignIn` (the take-over of a 1.0.0 connection,
[Migration](#migration)) revokes the earlier app's grant to the
owner's account, `DELETE /applications/{client_id}/grant` with the
client id and secret as basic authentication and the access token in
the body, which ends its access and refresh tokens both. GitHub takes
it only with a live access token, so on a 404 or 422 the refresh token
is traded for a fresh one first (`POST
https://github.com/login/oauth/access_token`, `GitHub.TokenURL`); a
refresh token GitHub no longer takes leaves nothing to revoke.

#### Tokens

Every call acts as the app's installation. The built-in signs a JWT
with the app's key (RS256; `iss` the app id, `iat` 60 seconds back and
`exp` nine minutes ahead, on the server's clock) and trades it for an
installation access token (`POST
/app/installations/{installation_id}/access_tokens`), whose body
narrows it to the call's repository (`repositories: [name]`) and to
the permissions the action needs (`permissions`, always with `metadata:
read`). A minted token is cached in the process's memory, by
connection id, app id, installation id, the repository's name (the
owner dropped, in lower case, as the token's `repositories` names it;
none for a token that reaches the whole installation) and permissions,
and handed out again until a minute before the
`expires_at` GitHub gave, on the same clock; one that expires within
the minute is used once and not cached. Tokens are never written
anywhere and never in an answer: GitHub's words an error passes on have
the request's credential replaced by `[credential]`. `installations`
lists the app's installations with the JWT (`GET /app/installations`),
for the setup flow to check an installation id against.

GitHub refusing a token is an error result for the agent: 401 (the
key) and 404 (no such installation) end with "Ask the owner."; 422 for
a repository says the app may not be installed on it or lack the
permission, and to ask the owner.

#### The actions

| Action | REST | Permissions besides `metadata: read` |
| --- | --- | --- |
| `list_repositories` | `GET /installation/repositories`, every page, kept to the access's patterns | none; the token is not narrowed to a repository |
| `get_repository` | `GET /repos/{o}/{r}` | none |
| `list_issues` | `GET /repos/{o}/{r}/issues` (state, labels, newest first), pull requests left out; with `search`, `GET /search/issues?q=repo:{o}/{r} is:issue ...` | `issues: read` |
| `get_issue` | `GET /repos/{o}/{r}/issues/{n}`, then the last pages of its comments | `issues: read`, `pull_requests: read` |
| `create_issue` | `POST /repos/{o}/{r}/issues` | `issues: write` |
| `comment` | `GET /repos/{o}/{r}/issues/{n}` to learn whether it is a pull request, then `POST /repos/{o}/{r}/issues/{n}/comments` | reads with `issues: read`, `pull_requests: read`; writes with `pull_requests: write` for a pull request, `issues: write` for an issue |
| `list_pull_requests` | `GET /repos/{o}/{r}/pulls` (state, base); `merged` asks for closed ones and keeps those with `merged_at` | `pull_requests: read` |
| `get_pull_request` | `GET /repos/{o}/{r}/pulls/{n}`, and with `diff` the same path as `application/vnd.github.diff`; a 406 there (a diff past GitHub's limits) answers the pull request with a note instead | `pull_requests: read`, and `contents: read` with `diff` |
| `create_pull_request` | `POST /repos/{o}/{r}/pulls` from a head branch already on GitHub; moves no commits | `pull_requests: write` |
| `list_branches` | `GET /repos/{o}/{r}/branches` | `contents: read` |
| `compare` | `GET /repos/{o}/{r}/compare/{base}...{head}`: commits and files, no patches | `contents: read` |
| `whoami` | `GET /app` and `GET /app/installations/{id}`, both with the JWT | no installation token |

The rules are GitHub 1.0.0's:

- **Access.** A repository argument must be `owner/name`
  (`cleanRepository`: spaces and tabs around it and a `.git` dropped,
  each segment letters, digits, `.`, `_` and `-`, never `.`, `..` or
  starting with `-`); one the access's `repositories` patterns do not
  match (`gitrepo.Match`) is refused before any token is minted, "Your
  access to GitHub does not include <owner/name>; ask the owner."
  `create_issue`, `comment` and `create_pull_request` need `write:
  true`: `Tools` leaves them out without it and `Call` refuses them by
  name, "Your access to GitHub does not include creating issues,
  comments or pull requests; ask the owner." `whoami` is Test
  connection's (`Verify`, "Connected as the app <name>, installed on
  <account>", the app's name from GET /app, since core scrubs the kept
  slug): no agent is listed it, and a call of it is refused.
- **The search.** `list_issues`' `search` (one line, at most 500
  characters) holding `repo:`, `org:`, `user:` or `owner:` after
  anything but a letter, digit or underscore is refused; with a search,
  a label holding a double quote is refused, since labels go into the
  query quoted. Whatever GitHub answers, with a search or without,
  only items whose address is under
  `https://github.com/<owner/name>/issues/` and that are not pull
  requests reach the agent.
- **Arguments**, each a refusal: `number` a positive whole number;
  `limit` the same, capped (50 for issues and pull requests, 100 for
  branches; 30 when left out); `state` one of the listed values; a
  branch at most 255 characters of letters, digits, `.`, `_`, `/`, `+`
  and `-`, not starting with `-`, `/` or `.`, not ending with `/`, `.`
  or `.lock`, without `..` or `//` (so `owner:branch` is refused);
  `title` one line of at most 256 characters; `body` at most 60,000;
  `labels` a list of names, each not blank, without a comma and at
  most 100 characters; `diff` and `draft` true or false.
- **Clips**, in characters: a body at 20,000, an issue's comments to
  the latest 20, a diff at 50,000, each with a `note` saying so; and
  every answer at 60,000, with "[The answer is cut to its first 60000
  characters.]". Answers are compact JSON, or text for
  `list_repositories`, `list_branches` and the addresses the write
  actions answer. A write action's audit row is "created <address>".
- **GitHub's failures** are error results with GitHub's words (its
  `message` and each of its `errors`' messages, or the status in words,
  "Bad Gateway", for an answer that has none): a 401 says GitHub
  refused the app's credentials and "Ask the owner."; a 403 says the
  app may lack the permission or not be installed on the repository,
  "ask the owner"; a 429, and a 403 that is GitHub's rate limit
  (`X-RateLimit-Remaining: 0`, `Retry-After`, or words naming a rate
  limit, as a secondary limit's do), says to try again later; a request
  past its deadline, or a call core abandons, says GitHub did not
  answer in time; any other is "GitHub answered <status>: <words>".

One `*http.Client` serves the built-in: 30 seconds a request, at most
16 MiB of an answer read, `Accept: application/vnd.github+json` (or the
diff type), `X-GitHub-Api-Version: 2022-11-28`, the credential as a
bearer (none for the manifest conversion; the client id and secret as
basic authentication for a grant's revocation), and redirects followed only for a GET and only on the API's
own origin (a renamed repository's), at most five: Go would follow a
redirected POST as a GET. `GitHub.APIBase` is the API; it must be
`https://api.github.com`, whose addresses on github.com the actions
read, or plain http on loopback, which only the tests use. `GitHub.Clock` is the clock JWTs
and the cache run on.

The package's tests run against a fake GitHub (`fake_test.go`), an
`httptest` server that mints installation tokens for JWTs the app's
key signed, on a `clock.Fake`, and records every request: method,
path, query, headers, the bearer, a JWT's claims, what the bearer
token was minted for, and the body. Each rule above has a test, as do
GitHub 1.0.0's behaviour tests, ported: the access holds calls to
the patterns (case by case against `gitrepo.Match`), write actions
need write, whoami names the app and the account, a GitHub failure is
an error with its reason, long answers are clipped, comment finds
whether the number is a pull request, create pull request, list
repositories keeps to the patterns, list issues stays in the
repository, every answer is bounded, refusals tell the agent to ask the
owner, and branches and compare call their paths; and tokens are
minted per call, narrowed, cached until a minute before they expire,
and never in an answer. `setup_test.go` drives `Setup`, `Requirements`,
`Uninstall` and `RevokeSignIn` against the same fake, holding every
step to the manifest's declarations and to what core's scrubbing leaves
readable. `internal/web/integration_github_test.go` drives the whole
setup over the HTTP API, on the fake clock, against a fake of GitHub's
API that makes the app from a code once and lists its installation:
the start page's manifest, the conversion and what is kept, the
stateless return and the listed installation, the Wait for an
approval, nothing kept on a failure, the requirement rows and their
Fixes, Uninstall, and the install over a 1.0.0 connection.

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
| `integration_list()` | Every integration on the team: name, display name, version in use, its state and, installed, the requirements not met, a pending version, how many agents have access. |
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
`notifications/initialized` and `tools/list`, and for a manifest that
names `method: setup`, `kivali/requirements`, all within 20 s, answers
the server's own requests as the shim does with no connection, and
prints one JSON line. The findings:

| Finding | Level |
| --- | --- |
| The server does not start, ends before an answer (with its status and the end of its error output), does not answer in 20 s, answers with an error, or writes a line that is not JSON-RPC | error |
| `tools/list` does not list a declared action (worded for `kivali.verify`'s and `kivali.access.choices`' tools) | error |
| `tools/list` lists an action the manifest does not declare, or gives a declared one an input schema that is not an object's | warning |
| A setup bundle's server does not answer `kivali/requirements`, answers it with an error, or answers it in another shape than `{"requirements": [...]}` of rows with an id, a title, words and met | error |
| The check could not run, or did not finish in 30 s | warning |

It checks that the server starts and lists the actions its manifest
declares, with no `_meta.kivali`, as core's own `tools/list` at every
connect asks, and that a setup bundle's server answers for its health
before anything is set up. It is not a review of what the code does,
and it runs no setup step.

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
| `GET /api/v1/org/integrations` | `Integrations`, by display name: each one's `state` ([The lifecycle](#the-lifecycle)) and, installed, `needs`, how many of its requirements are not met. |
| `POST /api/v1/org/integrations` | multipart `file`: 201 `IntegrationVersion`; 400 `IntegrationUploadRefused`; 409 for a version already stored or a built-in name. |
| `GET /api/v1/org/integrations/{name}` | `IntegrationPage`, with its `requirements` installed or off, its `flow`, `running` and `can_uninstall`. |
| `POST .../{name}/enable`, `.../disable` | `IntegrationPage`. |
| `DELETE .../{name}` | Uninstall ([Uninstall](#uninstall)): 204; 409 for a built-in with nothing installed ("<integration> is not installed, so there is nothing to uninstall"); 502 when the built-in's own `Uninstall` failed ("<integration> could not undo its installation: ...; nothing was changed"); 500 when a Secret could not be deleted. |
| `GET .../versions/{version}` | `IntegrationVersion`, with `changes` from the version in use, and `builtin` for a built-in integration's version. |
| `POST .../versions/{version}/approve`, `.../reject` | `IntegrationVersion`; 409 when not pending, or for a built-in integration. |
| `POST .../versions/{version}/rollback` | `IntegrationVersion`; 409 when not superseded, or for a built-in integration; 404 for a version not stored. |
| `PUT .../connection` | `ConnectionWrite` in, `IntegrationPage` out: the connection written, or made when there is none; a sensitive field left out keeps its value; 409 while no version is approved. |
| `POST .../connection/verify` | `IntegrationPage`, with the connection's verified, verify message and verified at; 409 with no approved version, no connection ("install <integration> first"), no `kivali.verify`, while the integration is not running, or for a built-in one this binary does not carry. |
| `POST .../connection/sign-out` | `IntegrationPage`, the connection signed out ([Sign out](#sign-out)); 409 for a token connection ("<integration> is not connected by signing in"), one this binary does not carry, with no approved version, or without a connection ("<integration> has no connection"); 500 when the tokens or the record could not be written. |
| `PUT .../access/{slug}` | `AccessWrite` in, `IntegrationPage` out: the row written afresh (a row set aside at boot is valid again); 404 for an agent not on the team; 409 without a connection ("install <integration> first"); 400 with the field's message. |
| `DELETE .../access/{slug}` | `IntegrationPage`. |
| `GET .../audit?limit=` | `IntegrationAudit`, newest first; limit 100 by default, at most 1000. |
| `GET .../choices/{key}` | `AccessChoices`, through the connection; 404 for a field that lists no options; 409 while the integration is not running, for a built-in one without a connection, or for a built-in one this binary does not carry; 502 for the tool's error, or a result that is not a list of names. |
| `GET .../logs?limit=` | `IntegrationLogs`, oldest first; limit 200 by default, at most 2000. |
| `POST .../setup` | `SetupFlowStart` in (`{"flow": "setup"}` to install, `sign_in` or a flow the integration's requirements name for a Fix: [Fix](#fix)), `SetupFlow` out: the flow started, ending one under way ([Setup flows](#setup-flows)); installing an integration with nothing installed switches it on. 409 for one this binary does not carry, with no approved version, for a Fix of one not installed ("install <integration> first") or switched off ("switch <integration> on first"), for a flow name it does not have ("<flow> is not a flow <integration> has"), while a failed install's connection is still being forgotten ("Kivali is still undoing the last setup; try again in a moment"), for `sign_in` of a connection not made by signing in, for a sign-in connection signed in ("the connection is signed in; sign out first") or none of whose ways works from here, for a request that names no host where the browser signs in, for a registered client ("this version of Kivali does not register itself with the service"), and for a setup connection whose setup this binary does not carry. |
| `GET .../setup` | `SetupFlow`: the flow under way, or the one that last ended (kept ten minutes after its last change); 404 with none, as after a restart. A read counts as watching the flow for ten seconds. |
| `POST .../setup/answer` | `SetupFlowAnswer` in, `SetupFlow` out: the flow's Form answered; 400 for a key the Form does not ask for, or what a save of the connection form would refuse (a standard flow's); 409 when the flow is not on a Form; 404 with no flow. |
| `POST .../setup/cancel` | 204: the flow ended and forgotten, audited as cancelled, under the lock a Keep is written under; 404 with no flow. |
| `GET .../setup/stream` | `text/event-stream`: `retry: 1500`, the flow's `SetupFlow` as a `flow` event at once and on each change, a heartbeat comment every 25 seconds, and the end of the stream when the flow is forgotten; 404 with no flow. |
| `GET /api/v1/agents/{slug}/integrations` | `AgentIntegrations`; 404 for an agent not on the team. |
| `GET /connect/callback` | Not under `/api`: the signed-in UI's route a Go step's service sends the browser back to. The query goes to the flow its `state` (or, with none, the flow cookie) names as `Answer.Return`, and the answer is a 303 to `/org/integrations/<name>?setup=<flow id>`, or `/org/integrations?sign_in_error=start_again` when no flow and no cookie names an integration ([Coming back](#coming-back)). |
| `GET /connect/start?flow=` | Not under `/api`: the signed-in UI's route the web app sends the browser to for a Go step. It sets the flow's cookie and answers a 303 to the step's address, or a page whose form posts the step's values there; a flow not on a Go step returns to its page, any other id to `/org/integrations?sign_in_error=start_again` ([Setup flows](#setup-flows)). Kivali Desktop opens the handoff with `next=<flow>` in its place ([In Kivali Desktop](#in-kivali-desktop)). |
