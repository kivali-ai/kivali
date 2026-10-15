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
  container mounts the org's Claude credentials; the `agent-shell`
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

`file_*` executes inside the `agent-shell` container through its
daemon, with exactly the shell's mounts and privileges. It keeps what
the shell does badly (exact-match `str_replace`, `insert`, line-range
and grep views, images returned as image blocks) and holds no
controls, so there is nothing to bypass. The `agent` container mounts
no agent-writable filesystem: it keeps `/data/claude-home` (CLI HOME),
its own `/scratch` (session files, subagent run dirs, cache) and the
sockets, and nothing an agent can place a symlink in.

The per-agent volume is split between the two containers. Each
mounts its own subdirectory at `/scratch` (`.kivali-agent` in the
`agent` container, `.kivali-shell` in `agent-shell`, whose `$HOME` is
`/scratch/home`), and nothing passes between them through it: they
meet only at the `agent-shell` socket. An init container, the one
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

### Software in the agent shell

An agent's shell has the image's packages plus its effective software:
the org-wide list (`<data>/software.yaml`) and the agent's own
(`software` in its `agent.yaml`), merged by `Store.EffectiveSoftware`.
The provisioner asks for that list on every `Provision`
(`agentpod.WithSoftware`, wired in `main.go`), so a changed list changes
the pod's spec hash and the pod is recreated; an unchanged one leaves
the pod running and the agent is not marked starting
(`Provisioner.ProvisionNotify`). Provisions for one agent run one at a
time (`Server.ProvisionAgentPod` holds a per-slug lock; a hire and
setup's Chief of Staff provision through it too), so a later one
reads the lists after the earlier one is done and applies any change
saved meanwhile. An offboard holds the same lock while it archives the
agent and deletes its pod, and a provision that finds the agent no
longer active does nothing, so an offboarded agent never gets its pod
or its directory back. A restore re-provisions every agent from the restored
lists.

The settings API changes a list by name, never by replacing it:
`POST /api/v1/org/software` and `POST /api/v1/agents/{slug}/software`
take `{packages}` to add, and `DELETE .../software/{name}` removes one,
each applied to the list as it stands on disk (the store serializes
every `agent.yaml` and `software.yaml` read-modify-write). A write
checks the names it adds: each must be a Debian package name, and a
package in the index the shell installs from (`internal/debian`: the
`main` component of `debian.Suite` for the server's architecture,
`Packages.gz` from deb.debian.org, held in memory, revalidated with
its ETag once a day; `debian.Suite` is tested against
`Dockerfile.agent-shell`'s base). Opening either list warms the index
in the background. A name the index lacks is a 400 that adds nothing,
naming the providers of a virtual package or up to three close names.
When the index cannot be read (no earlier download and the archive
unreachable; a failed download is not retried for a minute) the names
are saved and listed under `unchecked` in that answer; the shell's own
check at install time still applies. `propose_software_install` makes
the same check at publish time and refuses the call with the same
wording, so the owner never sees a name Debian does not have; it too
lets names through when the index cannot be read. A name already saved that is not a Debian
package name (a file edited by hand) stays listed, under `invalid` in
the answer and marked on the page as not installed, is left out of what
the pod installs (`Store.EffectiveSoftware` logs it), and `DELETE`
removes it like any other. Each write
re-provisions the pods it reaches in the background
(`Server.ReprovisionAgentPod`): an idle agent's at once, a working
agent's once its turn and background tasks have ended, so a change never
cuts work short. Approving an **Install software** proposal provisions an
idle agent's pod synchronously, on a context the request cannot cancel,
and records on the response's `software_install.outcome` what happened:
`restarted`, `restart_when_idle` (the agent was working; its pod is
recreated once the work has ended; if that provision then fails, the
outcome becomes `saved_not_applied` and the agent is told so),
`already_installed` (nothing new to
add, no provision), or `saved_not_applied` (the provision failed; the
next pod start installs it). The agent's and the owner's wording follow
it.

With a non-empty list the `agent-shell` container gets
`--install=<sorted, comma-joined names>` and `runAsUser: 0`,
`runAsGroup: 0`; with none it runs as the image's 65532 from the start.
`allowPrivilegeEscalation: false` is set either way: nothing in the
container gains privileges (the daemon's drop, apt-get's switch to
`_apt` and `unshare -mUr` only shed them). Started as root, the daemon:

1. starts `agent-shell install-software <names...>` (itself, via
   `/proc/self/exe`) as a root child, with `HOME=/root`, the proxy
   variables and `/` as its working directory. The child marks its
   result pipe (fd 3) close-on-exec, runs `apt-get update` (a failed
   update does not stop the install; its reason is added to each
   failure), then checks each name is exactly a name in the package
   lists (`apt-cache pkgnames --all-names <name>`), so apt-get never
   reads one as a pattern; a name that is not fails with "no package
   has this name". Then one `apt-get install -y --no-install-recommends`
   of every remaining name, and on failure one per name to learn which
   failed; the names are argv, validated as Debian package names (no
   leading or trailing `-`), never shell text. It is the subreaper of
   what it starts and kills whatever is left before it reports its
   result on the pipe. The image's `policy-rc.d` stops maintainer
   scripts from starting services. The daemon waits for the result for
   at most the install's own 20-minute bound plus a grace, then counts
   every package failed ("the installation did not finish in time").
2. drops itself to `--run-as` (65532:65532): `setgroups([])`, `setgid`,
   `setuid`, then reads back the real, effective and saved IDs, the
   groups and the effective and ambient capability sets
   (`/proc/self/status`) and exits unless every ID is 65532, there are
   no groups and both capability sets are empty;
3. only then creates `$HOME`, the socket, and serves.

A daemon started as root drops also without `--install`. One started
as anyone else installs nothing and reports every name as failed.

While the install runs, `POST /v1/exec` waits for it (up to 10 minutes,
then answers that the shell is still installing; the client allows for
that wait) and `GET /v1/packages` waits up to 15s, then names what is
still installing; `POST /v1/files` never waits. `GET /v1/packages`
lists the image's packages and those installed, and the failures with
a short reason (apt-get's error lines, and the update's fetch failure
when the package servers could not be reached). The first `run_shell`
result after an install with failures carries a notice naming them:
the agent's own command, not a subagent's, whose caller is still
waiting for the answer; otherwise the notice waits for the next one.
`run_shell`'s description is built once per `kivali mcp` process, which
lives across turns, so it words an install still running as software
installed when the shell starts, which commands wait for, and never as
a state that ends.

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
