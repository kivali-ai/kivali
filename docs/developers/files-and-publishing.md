# Files, publishing and the graph maintainer: contract

Where the code and this document disagree, the code wins; fix the
document. For how files and publishing look to someone running a team,
see [../how-kivali-works.md](../how-kivali-works.md); the graph built
from published files is [knowledge-graph.md](knowledge-graph.md).

This document is the contract for how an agent's files are written,
how a file becomes visible to the rest of the org, and how the
knowledge graph stays current. Agents never write their published
area themselves: core publishes on request, and so always knows what
changed without walking anyone's tree.

## Why

- **One writer per area, one set of rights.** The pod's `agent`
  container mounts the org's Claude credentials; the `dev-shell`
  container, where `run_shell` runs, does not. If `file_*` ran in the
  `agent` container over the same `/files` tree the shell writes, the
  shell could plant a symlink that `file_*` then followed with the
  agent container's rights, and any core ingest that followed
  symlinks would do so with core's rights (the whole data volume). Any
  control placed in `file_*` could be walked around with the shell. So
  `file_*` runs with exactly the shell's rights, and core reads agent
  trees through one confined primitive.
- **Core sees every publish.** Publishing goes through core, so the
  graph index is updated at the moment of the write, and no request
  path (sending a message, ending a turn, a graph tool call) has to
  walk every agent's tree to find out what changed.

## The model

Every area of an agent's filesystem has exactly one writer. Controls
live only where that writer is the only path.

| Area | Agent path | Writer | Agent access |
|---|---|---|---|
| Workspace | `/files/` (everything below that is not listed here: `artifacts/private/`, `background/`, notes, `subagents/`), `/scratch/` (the shell's half of the per-agent volume) | the agent (shell and `file_*`, same privileges) | RW |
| Published, own | `/files/artifacts/public/` | core, via `artifact_publish` / `artifact_unpublish` | RO |
| Published, peers | `/files/artifacts/shared/<peer>/` | core, same | RO |
| Core-managed views | `/files/{project,skills,attachments,past-chats,episodes}/` | core | RO |

### File tools are the shell's ergonomics, not a boundary

`file_*` executes inside the `dev-shell` container through its
daemon, with exactly the shell's mounts and privileges. It keeps what
the shell does badly (exact-match `str_replace`, `insert`, line-range
and grep views, images returned as image blocks) and holds no
controls, so there is nothing to bypass. The `agent` container mounts
no agent-writable filesystem: it keeps `/data/claude-home` (CLI HOME),
its own `/scratch` (session files, subagent run dirs, cache) and the
sockets, and nothing an agent can place a symlink in.

The per-agent volume is split between the two containers. Each
mounts its own subdirectory at `/scratch` (`.kivali-agent` in the
`agent` container, `.kivali-shell` in `dev-shell`, whose `$HOME` is
`/scratch/home`), and nothing passes between them through it: they
meet only at the `dev-shell` socket. An init container, the one
thing that mounts the whole volume, empties the agent half at every
pod start (all of it is regenerable).

`/files/artifacts/` is a read-write mount of its own, so that
renaming it, which would carry the read-only `artifacts/shared/`
mount away with it, fails with `EBUSY`. A move between it and the
rest of `/files/` crosses a mount: `mv` copies, and `file_rename`
copies a file and refuses a directory.

The daemon has no caller identity: whatever in the pod reaches its
socket is the agent. Parent and subagent differ only by the root the
client names (`subagents/<id>` for a subagent), exactly as `run_shell`
does.

### Core reads an agent's workspace through one confined primitive

Anything core takes from an agent's tree (share_file, message
attachments, `artifact_publish`) is opened with Go's `os.Root` rooted
at that agent's storage root, refusing any symlink component and
accepting only a regular file (checked on the opened handle). Nothing
core reads from an agent tree is trusted by path.

### Publishing

- `artifact_publish(source, dest?)`: `source` is a workspace path
  (file or directory); `dest` is the path under the caller's published
  area, defaulting to the source's path relative to
  `artifacts/private/` when it lives there, else its base name. A
  directory is published recursively; only regular files are taken,
  symlinks are refused. Core reads through the confined primitive,
  enforces limits, snapshots the bytes into the attachment store,
  writes the published copy atomically, records the version, updates
  the graph index, and answers with what the index made of it: node
  id, version, status and any findings (a rejected manifest, a
  dangling `depends_on`). The agent learns at once, not at its next
  wake.
- `artifact_unpublish(path)`: removes a published file or directory;
  the graph treats it as the file vanishing.
- A subagent publishes into its parent's area.
- The manifest schema, statuses, supersedes and withdrawn premises are
  in [knowledge-graph.md](knowledge-graph.md).

### Storage layout

Published files live in one top-level tree on the data volume:

    data/public/<slug>/<path>

- `/files/artifacts/public/` in an agent's pod is a read-only subPath
  mount of `public/<slug>`.
- `/files/artifacts/shared/` is a read-only subPath mount of `public/`
  itself, so every peer, archived ones included, appears as
  `shared/<peer>/` with no per-agent copy. A hire appears the moment
  its directory is created; nothing is re-linked.
- Archiving an agent leaves its published files where they are, at
  `public/<slug>/`, owned by an archived agent.
- There is no per-agent copy of a peer's files, and so no walk of
  every peer's tree on the turn and tool-call paths.

A restore into a fresh deployment first empties every
`public/<slug>/` (keeping the directories, which a running pod
mounts), so only the archive's files end up published.

### The graph maintainer

`store.GraphMaintainer` (`internal/store/graph_maint.go`, reached with
`FSStore.Graph`) owns the index. It lives in the store because
everything it keeps is the store's, and so `Published` can be called
synchronously by the publish path. Nothing outside it runs an index
pass or takes its lock. Its surface:

    Start(ctx, clk)                             // load index.json, then scan now and every interval
    Index() *graph.Index                        // current index; never waits on disk
    Published(owner, paths) (*graph.Index, warnings, error)  // synchronous: one owner, the files named
    OrgChanged()                                // hire, archive, reporting line
    ProjectFilesChanged()
    Reload() error                              // after a restore
    Scan(ctx) error                             // anti-entropy, stat-only

- **State.** One mutex serialises every change to an in-memory per-file
  cache (size, mtime, SHA, parsed manifest facts: front matter, first
  line, line count) and the version log, loaded once at boot. `Index()`
  returns an immutable snapshot from an atomic pointer and does not take
  the mutex once loaded. `index.json` is written atomically when the
  sequence moves.
- **Triggers.** `artifact_publish` / `artifact_unpublish` call
  `Published` synchronously (read-your-writes by construction). An org
  change (hire, archive, reporting line) or a project-file change
  re-reads the org chart or the project-file index and rebuilds from the
  cache, synchronously, walking nothing; before the boot scan has run,
  it is logged and left to that scan, which reads both. A restore calls
  `Reload`.
- **Anti-entropy.** At boot and every five minutes
  (`store.GraphScanInterval`; a mock clock in tests): stat every
  published file, reread only what changed. Until the boot scan has
  run, `Published` waits for it. Core is
  the only writer, so this guards against operator edits, restores and
  bugs, not against agents.
- **Call sites.** The top of a turn, the end of a turn, the graph
  tools and the graph page read `Index()`. No request path waits on a
  pass. The wake note describes the index as it stands when the agent
  wakes, and lists the index's findings on the agent's published
  files.

### Nothing indexes on a request path

- The message POST runs no graph pass and no filesystem sync, and
  neither does a turn. Each core-managed view is kept current by the
  edge that changes what it mirrors: project/ on upload and delete;
  skills/ on install, delete, enable and disable; past-chats/ on
  rotation; episodes/ when a digest is written; attachments/ when the
  row carrying them lands. The backstops are a hire, an agent's pod
  being created, boot and restore.
- A turn's end runs no graph pass before the hub completes.
- `graph_query` / `graph_node` run no pass and no sync; they read
  `Index()`.
