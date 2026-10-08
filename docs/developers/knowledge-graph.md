# The knowledge graph: contract

This document is the contract the code is built to. Where the code and
this document disagree, fix one of them in the same commit. How a file
gets into the graph and how the index is kept current is
[files-and-publishing.md](files-and-publishing.md); this document
covers what the graph is. For how the graph looks to someone running a
team, see [../knowledge-graph.md](../knowledge-graph.md).

## Why

Agents with only messages use them as a substitute for systems the
platform does not have: decisions and artifact provenance (SHAs, "do
not use", "supersedes") end up in message bodies, a register of
rulings ends up in one agent's private file that no other agent opens,
rulings are cited from memory, hardened before their premises settle,
and withdrawn without anything downstream noticing, and a standing
requirement from the CEO exists nowhere in the system.

The fix is discovery, not more messaging: every public artifact is a
node an agent can find by owner, subject, kind and status, decisions
and requirements are small nodes with stable ids, and retraction
propagates along declared dependencies.

## What the graph is

The graph is the existing file store plus typed front matter and an
index core derives from it. There is no graph database and no graph
write API. An agent drafts a node in its workspace with the `file_*`
tools and shell and publishes it with `artifact_publish`, which copies
it into its published area (`/files/artifacts/public/`, read-only to
the agent). Core is the only writer of that area: it validates what it
copies, snapshots versions, and serves the result through two
read-only tools, a block on the wake message, and a CEO page.

### Two node types

| Type | What it is | Owner | Source of truth |
|---|---|---|---|
| artifact | anything authored or uploaded as a file | whoever wrote or uploaded it | the file, in the owner's public directory or the project-file store |
| agent | a role | Chief of Staff, via `propose_role_update` | the org chart (`agents/<slug>/agent.yaml`) |

`agent` is a type because it is a different source of truth: projected
from the org chart, created only by the hire flow, archived rather than
superseded, and the only thing an `owner` edge can point at. Project
files, requirements and rulings differ from an artifact by owner or by
kind, not by behaviour, so they are artifacts, not types. Episodes are private to
their agent and cannot be nodes; assignments belong to the tracker.

### One file per node

A node is one markdown file with YAML front matter, in the owner's
published area (`/files/artifacts/public/`). It may name exactly one payload with
`file:`, a regular file anywhere under the same public tree, named
relative to the manifest, zipped if it is more than one thing. The
markdown is the node: id, kind, about, status, summary. The payload is
its bytes.

A public file that is not a manifest (a bare `.md`, a PDF, a zip, an
ELF) is still an artifact, with a derived id, its owner, its path and
its versions, but no kind and no edges. Front matter enriches it.

### Ids

An artifact's id is `<owner>/<name>`. `name` is the front matter `id`
when declared, else derived from the file's path under `public/`:
lowercased, a markdown file without its `.md`, any other file with its
extension kept (so `board.md` and `board.zip` beside each other are
`board` and `board.zip`). Declared ids survive renames; derived ids do
not. Anything another node will point at should declare one. A
rejected manifest keeps its declared id, so peers' edges do not
cascade into dangling ones over a single fault. An agent's id is its
slug. The two never collide: slugs contain no slash.

A reference to a node is its id, optionally pinned to a version:
`data-lead/queue-sizing-analysis@3`.

### Kinds

Kind is optional and, when present, strictly one of four. Each earns
its place with a rule the index checks:

| Kind | Rule | Serves |
|---|---|---|
| `requirement` | `about` required; optional `check` | "what binds X", "which of those are checks" |
| `decision` | `about` required | "what binds X" |
| `certificate` | `about` required; `depends_on` non-empty and every entry pinned | "what attests to X at this version" |
| `reference` | `source` required | material captured from outside: vendor documents, standards, papers, most CEO uploads |

A requirement is a constraint on an object, stated so an
implementation can violate it, one constraint per file. A decision is a
choice among alternatives about an object. Neither is a task (that is
the tracker) nor a finding (that is a plain artifact `about` the
object). An artifact with no kind is authored primary material: a
spec, a design, a report, a plan. There is no `design`, `report` or
`package` kind because no query filters on them and no rule depends on
them.

### Status

One vocabulary for every artifact.

Declared by the owner: `draft` (not in force), `provisional` (in force
but conditional; `condition` required), `current` (in force and
settled), `withdrawn`. Default when absent: `current`.

Derived by the index, never written into the owner's file: the status
`superseded` (some node in force names this one in `supersedes`), and
a separate flag, `flagged` (some `depends_on` target is withdrawn,
superseded, rejected, missing, or itself flagged). Flagged is not a
status value: a flagged node keeps its status and carries the flag
beside it. A flag travels down `depends_on` through any number of
hops, so a retraction reaches every node that rests on it however
indirectly, and every owner in the chain hears at its next wake. Each
flag names the premise one hop up; `graph_node` walks the rest. Flags
clear on their own once the chain above is repaired: the owner
re-pins to a corrected premise, drops the dependency, or supersedes
the flagged node. An edge dropped for closing a cycle carries no
flag; it is a problem on its source. Only a node in force supersedes: drafting or
withdrawing a superseder restores what it superseded to its declared
status, and two nodes that supersede each other are both superseded.

"In force" means provisional or current. "Of record for X" is the
artifact about X with status current, and the answer carries its
version.

### Edges

Four. Each has a runtime meaning.

- `owner`: node to agent. Derived from the path or the store, never
  declared.
- `about`: artifact to any node. The subject. Required on the three
  binding kinds. Makes "what binds X" and "what is about X" one hop.
- `depends_on`: artifact to artifact, optionally pinned. What this
  rests on. Retraction propagates along it.
- `supersedes`: artifact to artifact of the same owner or not, same
  shape. The target's status becomes superseded in the index.

Evidence is a field, not an edge: episode citations in the form agent
memory already uses (`[[ep:<ts>]]`). Episodes are private, so nothing
can follow the reference across agents.

### Front matter

```yaml
---
id: no-dropped-batches             # optional; see Ids
kind: requirement                  # optional; one of four
about: platform-lead/ingest-service
status: provisional                # draft | provisional | current | withdrawn
condition: until the load test on the resized queue lands
depends_on:
  - ceo/never-lose-customer-data
  - data-lead/queue-sizing-analysis@2
supersedes: []
check: a full day's ingest log shows zero dropped batches
source:                            # references only
file:                              # optional single payload, relative to this file
evidence: ["[[ep:2026-09-17T01]]"]
summary: one line for listings     # optional; else the body's first line
---
Every batch that fails is retried until the queue accepts it; nothing
is dropped to make room. The queue is sized for the worst day, so a
cap here would hide a sizing fault rather than protect anything.
```

## Mechanics

### Core is the writer of every published file

Agents cannot write the published trees; `artifact_publish` copies a
file in and `artifact_unpublish` removes one. Both run in core, so core
knows exactly which files changed and indexes them before the tool
returns. The store validates at publish time and reports rather than
rejects: a manifest that breaks a rule is still published, as a bare
artifact, and the `artifact_publish` reply names each file's node id,
version and status and says why anything was not accepted. The same
findings appear in the owner's next wake note.

### Index maintenance

The graph maintainer (`internal/store/graph_maint.go`) owns the index.
Nothing else runs an index pass or takes its lock, and no request path
waits on one: the top and end of a turn, the graph tools and the graph
page read the current index, an immutable snapshot behind an atomic
pointer. The maintainer keeps a per-file cache (size, mtime, the SHA of
the stored snapshot, parsed manifest facts) and the version log, loaded
once at boot. It changes the index on these triggers:

- `artifact_publish` / `artifact_unpublish`: read just the files named
  and rebuild before the tool returns, so the publisher's reply and its
  next `graph_query` see what it published.
- A hire, an archive or a reporting-line change, and a change to a
  project file: rebuild from the cache, walking nothing.
- A restore: reload from disk.
- The anti-entropy scan, at boot and every five minutes: stat every
  published file, reread only what changed, drop what vanished. Core is
  the only writer, so this guards against operator edits, restores and
  bugs, not against agents. The boot scan runs off the boot path so a
  large backfill cannot trip the liveness probe.

The rebuild is deterministic and makes no model calls. Published files
are written atomically (temp file plus rename), so nothing hashes a
half-written file.

Per file read: the bytes are snapshotted into the content-addressed
attachment store (deduplicated for free, no canonical text produced),
and the SHA of what was stored is the only hash a version may record,
so a file rewritten while it was read cannot leave a record that
points at bytes nobody kept. A version is appended only once its
snapshot exists; a file that cannot be read or stored keeps
its previous versions and the id they were recorded under (a declared
id is not lost to the path-derived name for a rebuild), and is tried
again next time. The current
version's snapshot is checked on every scan and taken again if it went
missing. A payload whose size and mtime match its last record is not
read at all. Records are appended per owner as the work goes, so a
scan killed halfway keeps its progress. Then parse front matter, apply
rules, resolve edges, derive statuses and reverse edges, and write the
index (`index.json`, atomically, whenever the sequence moves). A corrupt index is moved aside and rebuilt, with the version
log supplying the floor the sequence resumes from.

### Findings, two tiers

**Rejected**: the manifest is not applied and the file is indexed as a
bare artifact (under its declared id when that id is itself valid),
with one line saying why. Causes:
YAML that does not parse, an id that is not a slug path or collides
with another of the owner's ids, an unknown kind, an unknown status, a
kind rule violated (requirement or decision without `about`,
certificate without pinned dependencies, reference without `source`),
`provisional` without `condition`, a `file` that does not exist or is
not a regular file inside the public tree.

**Problem**: the manifest is applied and the finding is attached to
the node. Causes: an edge whose target does not exist (kept as
unresolved, not traversable), a pin to an unknown version, a
`depends_on` cycle (one edge of the cycle is ignored, chosen
deterministically from the node order), a requirement or decision over
a few dozen lines, a node whose id is derived from its path (a bare
file, or a manifest without `id:`) that another node points at (the
note lands on the target, whose owner can declare the id before a
rename or replacement orphans the edge).

Rejection is for things wrong with the node itself. Problems are for
relationships, which may be transient (the target is being written in
the same turn).

### Versions

Every artifact has a version list, newest last: sequence number,
timestamp, manifest SHA, payload SHA. Versions are appended to
`data/graph/versions.jsonl` and never rewritten; the index is
rebuilt from the files plus that log, so losing the index
loses nothing but a stretch of the change sequence, and each record
carries the sequence it was cut at as a floor. Old bytes stay readable
through the attachment store, and `graph_node` with a pinned id
serves them (below). Pins are `@N` (version number) or
`@<sha prefix>`; the tools print `@N`.

Project files version by original name through the same log: each
upload of the same name is a new version of the same node, owned by
`ceo`, kind chosen at upload and defaulting to `reference`. Deleting an
old upload never renumbers the survivors, so a pin stays a pin; it does
delete that version's bytes, since project files live in their own
store. Deleting the newest upload makes the older one current again
without cutting a version: bytes already on record are never a new
version. The node's path is the newest upload's link under
`/files/project/`, which the sync farm SHA-prefixes on a same-name
re-upload; the node's id comes from the first upload and does not
move. Two different documents whose names sanitise alike are two
nodes, the second under its prefixed link.

### Layout

```
data/graph/
  index.json        rebuilt on each change; the current graph
  versions.jsonl    append-only; one line per new version of any node
agents/<slug>/
  graph_watermark.json   per-agent: last index sequence shown in the
                         wake note, plus handbook, role and skills
                         fingerprints
```

### Reading

Two tools, both read-only, both given to subagents too, dispatched
through the existing state-tool endpoint over UDS. They return ids and
one-liners; the agent opens a node with `file_view` at the path the
tool prints. Each call answers from the current index. Every agent's published
files are mounted at `/files/artifacts/shared/<owner>/`, so a node
listed is readable at the path printed for it.

- `graph_query` with `type`, `owner`, `about`, `kind`, `binding`
  (requirements and decisions only), `status` (also `in_force`, and
  `active` or `archived` for agents) and `limit` (default 50).
- `graph_node` with an `id`: front matter, effective status and flags,
  edges out with resolution, edges in (about-me, dependents,
  superseded-by), versions, payload, problems.
- `graph_node` with a pinned `id` (`owner/name@N`, or `@<sha prefix>`):
  the same header, then the file as it was at that version, front
  matter and body, between `----- begin owner/name@N -----` and
  `----- end -----` lines, with a line saying whether that version is
  current or how many newer ones exist. This is the one body the
  tools serve, because it is the one `file_view` cannot: `file_view`
  reads the current file, and a certificate or a pinned `depends_on`
  names what the file was when the pin was written. The bytes come
  from the attachment-store snapshot the maintainer recorded under
  the version's SHA (a project file's from the upload itself). A
  binary version says its size and is not shown; a version whose
  bytes are gone (a deleted upload, a snapshot lost to a restore) says
  so and stays on record, so pins to it still resolve. Text is capped
  at the inline-file ceiling with the usual truncation marker. A pin
  that names no version is a tool error listing the versions there
  are; agents have no versions to pin.

### The wake note

At wake, core reads the index as it stands and appends one received
chat entry of kind `wake_update` behind the deliveries that woke the
agent, before the turn starts. It is the one place the runtime tells
an agent what changed while it was idle and what it holds: once per
wake, never on a delivery, so a message folded into a running turn
brings no copy of it. Its graph sections read "since your last turn"
and list, one line each with kind, owner, status and version:
artifacts about nodes the agent owns, nodes it depends on, and its own
nodes only when the index flagged them or a peer superseded them; then
the index's own findings on the agent's published files. An agent's
own publishes are not echoed back to it; its `artifact_publish` reply
said what the index made of them. Then one line each for the
handbook, the agent's role, and skills if their fingerprint moved.
Last, the assignments the agent holds ([assignments.md](assignments.md), "Wakes"), which is
state rather than a diff and appears on every wake, the first
included. Everything else is left out: a wake after which only
unrelated nodes changed, for an agent holding no assignments, appends
nothing at all (the watermark still advances), because a count of
changes elsewhere would be a paragraph in the model's context that it
cannot act on. The watermark is the index
sequence at the time of the wake; if the index was rebuilt below it,
the note says so once and rebases.

It is chat content, not system prompt, so the stable-to-volatile prompt
order holds. The spawn gate treats `wake_update` as plumbing: it never
causes a wake, and a turn that dies before answering does not respawn
on it. The agent page hides it by default, since it is what the model
was told, not conversation: **Show wake notes (N)** above the
transcript shows each as a collapsed "Wake note" card.

### The CEO page

The Graph screen (`/graph`, fed by `GET /api/v1/graph`): a searchable
list of every node, owners folded with counts, with Flagged and
Problems filters. A node's page (`/graph/nodes/<id>`, fed by
`GET /api/v1/graph/nodes/{id}`) shows its fields, edges, problems and
the text of a chosen version. Both read the current index.

## Acceptance: the nine queries

1. What binds X right now: `about` X, kind requirement or decision,
   status in force.
2. What is of record for X, at which version: `about` X, status
   current; the answer carries the version.
3. What does this rest on, what rests on it: `graph_node`, edges both
   ways.
4. What changed since my last turn: the wake note.
5. Which things binding X are checks a verifier must run: `about` X,
   kind requirement, `check` present.
6. What has owner O published, and what is new from them: `owner` O;
   the wake note for "new".
7. What is about X, all of it: `about` X, no kind filter.
8. What attests to version X@N, and is the version I am holding still
   good: kind certificate `about` X with a pin to N; `graph_node` on X
   for its effective status.
9. What did X@N actually say, now that X has moved on: `graph_node`
   on `X@N`; the text a certificate was judged against, a reviewer's
   verbatim.

## Out of scope here

A claim type. Episodes as nodes. Assignments, challenges, tasks and anything
that wakes an agent (that is the assignment tracker). Any store write into an
owner's file. Handbook, skills and roles as artifact nodes (the
wake note reports their changes from their own stores). Whitelisting
descriptive kinds.

## Orientation

`internal/graph/` is the pure package: front matter, rules, index
build, derived state. `internal/store/graph_maint.go` is the maintainer:
it owns the cache, snapshots versions, persists and answers `Index()`;
`internal/store/publish.go` is the core side of `artifact_publish`.
`internal/agent/graph_tools.go` holds the two tools' names, schemas and
descriptions; `internal/mcp/graph_tools.go` serves them. The wake note is built in `internal/agent/wake_update.go`
and appended in `web.spawnChatLoopIfIdle`. The CEO page is
`internal/web/graph.go` and `api_graph.go`.

Prompt surfaces, and which reach an existing install on upgrade:
`renderGraphSection` and `renderFilesystemSection` in
`internal/agent/agent.go`, the `artifact_publish` / `artifact_unpublish`
and graph tool descriptions, the `file_*` and `run_shell` descriptions,
the subagent tool descriptions and prompt, and the
wake note ship in the binary and are current everywhere the moment it
runs. `internal/seed/handbook.md` §The
knowledge graph and `internal/seed/cos_role.md` item 5 are copied into
an install once, when it is seeded; an existing install keeps its
copies until they are edited into `data/handbook.md` and the Chief of
Staff's `role.md`.
Tests in `internal/agent/filesystem_prompt_test.go` and
`internal/seed/seed_test.go` pin every surface to `graph.Kinds`.
