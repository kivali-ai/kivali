# Agent context serialization: invariants

For what an agent's context and memory look like to someone running a
team, see [../how-kivali-works.md](../how-kivali-works.md) and
[../memory.md](../memory.md).

This document catalogs every invariant that must hold when we prepare
context for a Claude call on behalf of an agent. Each invariant has
(a) a short name, (b) a "why it matters" note, (c) the class of bug
it prevents, and (d) where it's verified.

The central test suite is `internal/agent/serialization_test.go` — it
is the single place to look when asking "is the ordering/composition
correct?" Per-package tests (web, store, claudeagent) cover
behaviors like spawn races and transport dispatch that complement
the pure-serialization contract.

---

## 1. System prompt composition

**Invariant:** the system prompt is assembled from stable, identity-
shaped content only. Mutating state (org chart, open assignments, what's
in /files/project/) is NEVER inlined into the system prompt — it
comes from on-demand MCP tool calls whose results carry "snapshot at
timestamp T" phrasing so stale lookups are visibly stale.

**Parts, in order:**

1. `handbook` — the org-wide rules (from `Store.ReadHandbook`).
2. Owner section — `## The owner`: what the person is called and the
   marker their markup appears under (`owner.Term.Section`, from the
   name in `branding/branding.yaml`). Identity-shaped, so it belongs here; it
   changes only on a rename, and the runner's respawn on prompt drift
   carries a rename to the next turn. Stored text never carries the
   name.
3. Filesystem help — the description of the `file_*` tools when the
   agent has them (`FilesystemAvailable`).
4. Agent persona — `role.md`, then habits (`agent_memory_habits.md`)
   above `agent_memory.md`, inlined so the agent always has its own
   durable identity, rules and evolving summary. This block is the
   cache breakpoint.

**Why:** the model treats system-prompt blocks as invariant
background facts. Live state (like the org chart) stuffed in here
makes the model miss changes within the current chat (e.g. reading a
newly hired agent as "pre-existing" after its own hire approval). Tool-results, by contrast, read as
"snapshot I computed at time T" — the right semantics for mutable
state.

**Bug class prevented:** model blindness to mid-chat state changes.

**Verified in:** `internal/agent/serialization_test.go` — the
`TestSystemPrompt*` block.

---

## 2. Chat history projection

**Invariant:** `chat.jsonl` file order IS the canonical conversation
order — UI, model, and any archive view all see the same sequence.
The model's `req.Messages` is built by `chatHistoryToMessages`
iterating chat.jsonl in file order, with role mapping and bucket-
transition flushing.

**Role mapping:**
- `store.RoleReceived` → `provider.RoleUser`
- `store.RoleSent` → `provider.RoleAssistant`

**Bucket-transition flush (user side only):** within a run of
user-role entries, we flush when transitioning between tool traffic
(`tool_result`) and text traffic (`direct_chat`, `inbox_delivery`,
rotation prompts, etc). A mid-turn `direct_chat` packed into the
same user message as a preceding `tool_result` reads to the model
like commentary on the tool output — wrong. Assistant side stays
merged because text-before-tool_use IS one logical turn.

**Dropped kinds (not projected to model):** `doc_published` and
`shell_captured` are persistence records that map to no content
block; they are UI-only.

**Dangling tool_use closed before the next text:** a turn that dies
mid-tool (Stop, a runtime disruption, a turn that stopped on an error) leaves a `tool_use` with no
`tool_result`. The API requires every `tool_use` to be answered in the
very next user message, so before a text-bucket user message opens
after such a gap, the projection emits a user message of synthetic
error `tool_result`s ("interrupted before this tool call returned") for
the unanswered ids. Runtime notes appended behind the gap (the
disruption notice, the `wake_update` note) then follow as text.
Verified in `TestDanglingToolUseIsClosedBeforeTheNextText`.

**Runtime notes:** `wake_update` (the wake note: graph changes,
findings, handbook/role/skills moves and the agent's open assignments,
see [knowledge-graph.md](knowledge-graph.md), "The wake note") projects as user text like any
delivery, but the spawn gate treats it as plumbing: it never causes a
wake.

**Attachments:** on the received side, canonical text of a
referenced file is appended to the body before projection so the
recipient sees the content, not just the filename.

**Why:** the model reasons over this sequence as "what happened."
If UI shows one order and model sees another, the CEO can't
diagnose misbehavior by reading the chat — the two views diverge.
Bucket-flush ensures the model doesn't misread a new user directive
as commentary on a tool result.

**Bug class prevented:** model-vs-UI divergence; user-input-read-as-
tool-commentary.

**Verified in:** `internal/agent/serialization_test.go` —
`TestChatProjection*`.

---

## 3. Message entry into chat.jsonl

**Invariant:** all writes to chat.jsonl go through
`Store.AppendChatMessage`, which holds a per-agent mutex. Writes are
serialized within a process. Cross-process writes via `O_APPEND`
serialize at the syscall level for small payloads (≤ PIPE_BUF).

**Entry paths:**

| Path | Role | Kind | Notes |
|---|---|---|---|
| POST `/api/v1/agents/{slug}/messages` | received | `direct_chat` | the CEO typing; through `deliverToAgent` |
| POST `/api/v1/agents/{slug}/new-chat` | received | `rotation_prompt` | `startNewChat` |
| `messaging.Messenger` (`DeliverToAgent`, release) | received | `inbox_delivery` | released messages, assignment events, CEO replies; through the server's `DeliverToAgent` hook, so they stage like any delivery |
| `Messenger.DeliverToCEONow`, `respondAsCEO` | received / sent | `ceo_inbox` | the CEO's own chat (no loop) |
| agent pod turn (`agentpodTurnState.appendChat`, `handleAgentpodChatAppend`) | sent + received | `tool_use`/`tool_result`/`direct_chat`/`doc_published` | the agent's own stream |
| runtime markers (`finalizeSubprocessDisruption`, `finalizeTurnError`, `handleAgentStop`, `RecoverInterruptedTurns`) | received | `runtime-disruption`, `turn-error`, `user-interruption` | written by core when a turn ends abnormally |
| the wake note (`spawnChatLoopIfIdle`) | received | `wake_update` | prepended to a wake |

**Bug class prevented:** interleaved writes corrupting chat.jsonl.

**Verified in:** `internal/store/agent_test.go` covers concurrent
append safety; `internal/agent/serialization_test.go` covers the
downstream projection.

---

## 4. Mid-flight delivery buffering

**Invariant:** when a received entry arrives WHILE a response loop is
already running (CEO instant-delivery, a released assignment event or
notice routed mid-stream, CEO typing during a response), it is held in a per-agent
BUFFER (`Server.pendingDeliveries`) and NOT written to chat.jsonl.
When the running loop ends, the buffer is flushed — all held
entries land in chat.jsonl in arrival order, strictly AFTER the
loop's own final output — and a follow-up loop is spawned to
respond.

**Consequences:**

- **chat.jsonl is strictly append-only at all times.** No
  interleaving, no rewrites, no TS-based reorder. UI sees the file
  exactly as it's written.
- **UI order = file order = model order.** Every view reads the
  same canonical sequence: `prior task → tool flow → prior reply →
  buffered deliveries (in arrival order)`.
- **Multiple mid-flight deliveries batch.** If two deliveries
  arrive during one response, both flush together at loop-end;
  the follow-up responds with both as the latest user turns.

**Entry point:** external writers (POST /messages, inbox releases,
subagent results, handoffs) route through `Server.deliverToAgent`
(`deliverOrStage` underneath, chat.go). Internal writers (the turn's
own stream events — the agent's output) continue using
`Store.AppendChatMessage` directly, since those are part of the
in-flight response stream, not external deliveries.

**Busy detection:** `deliverOrStage` holds `streamMu` across the busy
check + direct-write decision (`midTurnLocked`), so a hub claim or
turn start can't race with the check. When `chatHubs[slug]` holds a
hub that has not completed OR `turnInFlight(slug)` is true, the
message is buffered (`stageLocked`) and — unless a rotation is
pending or a wind-up was already asked for — offered to the running
agent-pod turn as a fold.

**Flush gates:**
- **Fold landed:** `flushFoldedDelivery` appends that one row at the
  seam where the running turn consumed it and drops it from the
  buffer.
- **Turn end:** `finalizeAgentpodTurn` → `flushAndComplete` drains
  the buffer under one `streamMu` hold together with the hub's
  completed flip, so a delivery racing the drain either buffers
  ahead of it or writes directly after it. A non-empty flush
  triggers the follow-up spawn. `abandonSpawn` (a hub claim that
  never became a turn) flushes the same way.

**Pending messages (the CEO's view of the buffer):** a CEO direct
chat staged behind a running turn is `visible`: POST /messages
answers `pending_id`, and the agent stream announces
`pending_message`, then `pending_offered` once the fold is handed to
the pod (from then on the text sits in the CLI's input queue, which
gives back everything or nothing, so Delete is refused with 409),
then `pending_delivered {id, ts}` when the row reaches chat.jsonl —
on both exits, and always before the `chat_message` that paints the
row. Every other delivery kind stays invisible until it is on disk.
Send now (`sendPendingNow`) is `preemptForDelivery` plus
the `pausedForDelivery` latch on the turn state: the parent turn is
cancelled, and at turn end the partial reply is kept, a
`kind:paused-to-deliver` marker ("Paused to deliver your message")
is written, then the buffered rows — all in one `streamMu` hold
(`flushAndCompleteMarked`), which also reads the latch, so a Send now
that answered 202 always has its marker in front of the messages it
delivers. The marker is a system boundary (skipped by
`LastRealReceivedTS`) that the spawn gate walks past without
holding: the delivered messages after it are what the agent owes,
and the follow-up spawn answers them. The model reads it bracketed
in front of them, like the other boundary markers (§2). A deleted
pending message lives on as an in-memory tombstone
(`Server.pendingTombstones`) for ten seconds so a restore can put it
back.

**Defensive net:** `spawnFollowupIfNewerReceived` (a TS comparison
between post-loop `lastRealReceivedTS(hist)` and the hub's
`spawnReceivedTS`) remains in place as a safety check. Production
shouldn't hit this path — all external writes go through
`deliverToAgent` — but a regression elsewhere (a new caller
wiring directly to `AppendChatMessage` for a received entry) would
still produce a follow-up instead of silently orphaning the work.
The defensive path logs a warning so the bug is visible.

**Why:** a delivery written straight into chat.jsonl while the agent
is responding lands in the middle of a response the model never saw
it during, and the UI then shows that response as if it addressed the
delivery. Rewriting the file afterwards to move such entries to the
tail makes entries appear to jump and leaves a window where the file
is out of canonical order. Buffering closes that window: the file is
always in canonical order, because out-of-order entries never reach
it in the first place.

**Bug class prevented:** orphan deliveries; chat.jsonl
interleaving; UI-vs-model divergence.

**Verified in:**
- `internal/web/chat_test.go`:
  `TestChatLoopBuffersDeliveriesDuringLoop`,
  `TestChatLoopBuffersMultipleDeliveriesInArrivalOrder`.
- `internal/web/chat_pending_test.go` (the pending message events on
  both exits, Send now's marker and kept partial reply, Delete after
  the offer, restore, offboard) and
  `internal/store/chat_paused_test.go` (the marker never holds).
- `internal/agent/serialization_test.go`:
  `TestFileOrderIsStrictlyAppendCanonical` (the invariant that the
  buffer implements).

---

## 5. Driver serialization

**Invariant:** callers build req.Messages once, as a full
conversation; the driver converts it to the wire form the CLI
expects, and the model sees the same conversation either way.

**The CLI driver (`claude -p --resume <sid>`):**
- Per-agent `session_id` stored in `agents/<slug>/claude_session.json`.
- On first call: no `--resume`, fresh CLI session; capture
  session_id from first `system` event, persist.
- On repeat calls: `--resume <stored-id>`, stdin carries ONLY the
  latest user turn. The CLI reads prior context from its session
  log (`$HOME/.claude/projects/*/<id>.jsonl`).
- Stale-session probe: glob for the log before using `--resume`;
  if absent, clear the stored id and start fresh.

**Why bootstrapping through `--resume` matters:** the CLI's
stream-json stdin writes a replay of assistant history as orphan
branches in its session tree (parentUuid=null). The API never sees that
context, so each call is effectively the system prompt + one user turn. `--resume` is the only
way to give the model proper multi-turn history through the CLI.

**Bug class prevented:** model amnesia across turns; 400 "tool use
concurrency issues" from orphan tool_use blocks.

**Verified in:** `internal/claudeagent/stream_test.go`
(`TestLatestUserMessage`, `TestResolveResumeSessionID*`),
`internal/agent/serialization_test.go` (transport-agnostic message
shape).

### The provider seam

Everything above the driver talks to `provider.Client` and
`provider.Provider` (`internal/provider`); `internal/claudeagent` is the
one implementation of both, and nothing outside it knows a Claude Code
CLI, a Claude model id or an Anthropic price is underneath. One
provider serves an org.

- **Package layout.** `internal/provider` holds only provider-neutral
  things: `Client`, `Stream`, `Folder`, the request/response, message,
  content-block, tool, stream-event and usage shapes (`TokenUsage`,
  `ModelUsage`, `UsageDisplay`, `UsageRecorder`), `ToolSet`,
  `MCPServer`, `SubagentRequest`, the `Stop*` constants,
  `ErrSubprocessExited` / `ErrUserCancelled`, `Provider`, `ModelInfo`,
  `Effort`, `Defaults`, `Credentials`, `CredentialStatus`, a few
  helpers (`Label`, `EffortIDs`, `DefaultEffort`), and the mocks (`MockClient`,
  `MockStream`, and `MockProvider`: two models on offer, `mock-large`
  with three efforts and `mock-small` with none, a retired
  `mock-large-0`, fixed prices). `internal/claudeagent` is the Claude
  driver: the catalog (`catalog.go`: one row per model, one selectable
  row per lineage), lineage (`lineage.go`), the pricing table, the
  `[1m]` suffix and context windows, dated-snapshot normalisation and
  Bedrock and Vertex AI model ids read as catalog ids, the
  label renderer, the `<synthetic>` sentinel, `TurnUsage` /
  `UsageGauge`, the effort levels and the default models: unexported,
  or answered through `Driver`, apart from a few constants
  (`Effort*`, `Default*Model`, `HomeDirName`) the
  composition roots and dev tools use.
- **`Provider`.** `Name()` ("claude"), `Models()` (what a person may
  pick, in display order — cheapest first for Claude), `Resolve(id)`
  (any stored spelling: dated, retired, `[1m]`, `claude:`-prefixed — to
  the row that prices and labels it), `Current(id)` (where a pin on
  that lineage runs today; boot and restore rewrite pins through it),
  `Price(id, usage)` (USD; 0 for an unknown id, so callers that must
  tell "free" from "unpriced" ask `Resolve` first), `Defaults()`
  (agent, subagent, summary models), `Effort(model, id)` (the level if
  `model` offers it, else the model's default and false) and
  `Credentials()`.
- **`ModelInfo` / `Effort`.** A row is `ID`, `Label`, `Lineage`,
  `Current`, `ContextWindow` and `Efforts` (`{ID, Label, Default}`, low
  to high; empty means no reasoning control, and the UI hides the
  chip). Model ids stay opaque strings in stored data and are resolved
  at read time; effort fields stay strings holding an effort id. The
  Claude provider offers `low`, `medium`, `high` (default), `xhigh`,
  `max` on every model — the ids stored values already hold.
- **`Credentials`.** `Status(ctx)` (`Present`, `Who` and `Billing`;
  never spends a model call — for Claude: `claude auth status --json`
  run as the server with its HOME, parsed by `internal/claudeauth`:
  signed in, the account's email, and "Claude Max", "Anthropic
  Console", "Amazon Bedrock", "Google Vertex AI" or "Microsoft
  Foundry"), `Guidance()` (one
  sentence for a person on who can sign the server in) and `HomeDir()`
  (the directory under the data volume the provider keeps its state
  in: `claude-home`, the subPath every agent pod mounts as the CLI's
  HOME, `ProvisionerConfig.UseCredentials`). No credential variable
  reaches an agent pod.
- **What asks the provider.** config (the default models,
  `Config.ResolveModels`; an override must `Resolve`), web (labels,
  usage rollups and usage rows through `Price`, the picker through
  `Models`, agent model/effort validation, subagent model/effort
  validation and resolution, the effort a turn request carries —
  resolved, never the raw stored value — the pin rewrite after a
  restore, the setup credential), mcp (the subagent tools' model enum and per-model
  effort description), agentpod (the subagent default). The API
  exposes each model row as `{id, label, legacy, current,
  context_window, efforts, provider}` and the setup credential from
  `Status` and `Guidance`.
- **The import rule** (`internal/provider_imports_test.go`):
  `internal/provider` imports nothing from the module; no package
  under `internal/` but `internal/claudeagent` imports
  `internal/claudeagent`; outside `internal/` only the listed files
  may — the composition roots `main.go`, `agent_cmd.go` and
  `mcp_cmd.go`, the dev tools `cmd/fake-claude`, `cmd/devseed` and
  `cmd/inspect_request`, and the build-tagged prompt evals. The roots
  build one `*claudeagent.Driver` and pass it wherever a `Client` or a
  `Provider` is needed.
- **The interface.** `Stream` runs a turn (warm per-agent runner, or a
  per-call subprocess for agent-less calls); `RunSubagent` runs one
  `SubagentRequest` — a fresh one-shot session with no history, its
  own `ToolSet` and its own `MCPServer`s — and returns the same
  `Stream`, so the agent pod maps a subagent's events with the code it
  uses for a turn. `ToolSet` names tools neutrally: `Kivali` holds the
  kivali MCP server's tools by bare name (`file_view`, `subagent`),
  `Builtins` holds provider-side tools by neutral id
  (`provider.BuiltinWebFetch`, `provider.BuiltinWebSearch`). `MCPServer`
  is a server as the core launches it: name, command, args.
- **The typed error.** A model call that fails and ends the run (a
  usage limit, an expired login, a 429, an output-token maximum) is a
  `StreamError` event carrying the provider's text, then a `Final`
  with `StopReason == provider.StopError` and the text in `Error`. It is
  never a delta and never in `Content`. The CLI driver holds the
  CLI's `<synthetic>` close-out text until the run's outcome: an
  is_error result (or a failed exit) makes it the typed error; on a
  run that completed it is logged and dropped. An error result with
  no such text still fills `Error` from the result's kind or the exit
  status. Core turns it into the kind:turn-error marker (parent turns)
  or a failed run (subagents).
- **A protocol violation fails the run.** A stream-json line the
  per-call reader cannot parse sets the stream's `Err` (parent
  empty-slug calls and every subagent run), and the error names the
  line's type or quotes its first 80 characters.
- **The driver owns** the binary (`claudeagent.Options.ClaudeBinary`,
  which defaults to `claude` on PATH; only `agent_cmd.go` sets it, for
  agent pods, and `main.go` takes the default), its flags (`--tools`,
  `--allowedTools`, `--effort`, …), the `--mcp-config` file format,
  the tool-name spelling on its wire (`mcp__kivali__<tool>`, stripped
  from every event it emits, parent and subagent alike), the CLI's
  `<synthetic>` placeholder model (reported as the typed error, its
  tokens billed to the model that answered), and session ids.
- **The core owns** which tools a tier gets
  (`agentpod.SubagentTools(depth)`, bare names; the subagent tool only
  where `CanDelegate`), the MCP server a subagent loads and its launch
  arguments (`kivali mcp --toolkit subagent --parent … --subagent-id …
  --core-uds … --subagent-files-root … --depth …`, built by the agent
  pod's runtime), and the transcript.

**Verified in:** `internal/claudeagent/subagent_test.go` (flags, MCP
config, typed error and cancel against the fake CLI),
`internal/agentpod/subagent_runtime_test.go` (the pod's request and
failed-reason mapping on the mock client),
`internal/web/subagent_wire_test.go` (the spec core emits is the spec
the pod decodes), `internal/claudeagent/provider_test.go` (the Claude
provider's rows, spellings, prices, efforts and credentials),
`internal/web/agent_setmodel_test.go` and
`internal/web/subagent_validate_test.go` (the picker and the dispatch
schemas from the mock provider), `internal/provider_imports_test.go`
(the import rule).

---

## 6. Cross-path coordination

**Invariant:** at most ONE Claude subprocess runs per agent at a time.

- **One hub per agent:** `getOrCreateHub` is atomic under
  `streamMu`. A second caller gets `isOriginator=false` and does
  nothing.
- **Every spawn goes through the claim:** `spawnChatLoopIfIdle` is the
  only way a turn starts, and `ChatHubActive` reports an open hub to
  anything that must not start a parallel run.

**Why:** two independent Claude subprocesses on identical chat
history produce near-duplicate tool calls (for example, two copies of
the same approval request).

**Bug class prevented:** double inference; duplicate tool calls
read as model mistakes.

**Verified in:** `internal/web/ceo_chat_spawn_test.go`,
`internal/agent/serialization_test.go`.

---

## 7. Rotation (new chat)

**Invariant:** rotation is async via the normal chat loop. The UI
experience is "the CEO clicks New chat → sees the rotation prompt and
the memory edits stream → the page replaces the transcript with a fresh
chat."

**Flow:**
1. `startNewChat` (refused while a turn runs) snapshots memory and
   habits, mints the archive timestamp, delivers a `rotation_prompt`
   received entry, writes the on-disk `pending_rotation.json` marker
   and calls `spawnChatLoopIfIdle`. The API answers at once.
2. The rotation prompt asks the agent to reconcile its habits and
   `agent_memory.md` using the everyday `agent_memory_*` and
   `agent_memory_habits_*` tools — no special rotation-only tool.
3. Turn end: `consumeRotationIfReady` reads the marker; if set,
   `finalizeRotation` archives the chat, writes the prior memory and
   habits snapshots into the archive dir, clears the session id,
   sends `session-reset`, regenerates the /files/ symlinks and clears
   the marker. The new memory is whatever the
   agent left in `agent_memory.md` after running their edits —
   finalize does not write memory.
4. `finalizeRotation` runs BEFORE `hub.close + removeHub`, so
   "hub idle" is an authoritative "everything is done" signal.
5. A delivery that arrives while the rotation turn is running is
   held: `deliverToAgent` stages it (§4) but offers no fold and no
   preempt, because the turn is the memory reconcile and the
   transcript it is writing is about to be archived. The archive
   runs first and the flush second, so the held messages open the
   fresh chat.jsonl in arrival order and the flush spawns the
   follow-up that answers them. The marker alone does not hold: an
   idle agent with a marker writes through, so a rotation that was
   interrupted by a restart never strands messages in memory.

**Bug class prevented:** a user-visible hang on new-chat (a
synchronous memory call before redirecting would take 30-60 s); chats
left un-rotated by a rotation-only tool missing from the CLI's
allow-list (sharing the everyday agent_memory_* path means whatever
the CLI accepts for normal curation also works for rotation); a
message folded into the reconcile turn being answered in the archived
transcript instead of the fresh chat, or cancelling the reconcile.

**Verified in:** `internal/web/handoff_test.go`
(`TestNewChatQueuesRotationAndArchivesOnCompletion`,
`TestNewChatPreservesDeliveriesArrivingMidRotation`),
`internal/web/chat_rotation_hold_test.go`.

---

## 8. Backup / restore

**Invariant:** backup zips everything under `/data` except the CLI's
sign-in and session logs, debug dumps and temp files. Restore is only
allowed when the deployment is "fresh" (seeds only, no CoS chat
history).

**Excluded from backup** (`files.ShouldExcludeFromBackup`): the whole
`claude-home/` subtree (sign-in, caches, telemetry, per-project
session logs), `debug/`, and any `.tmp-*` file. The session logs only
serve `--resume`; Kivali's own `chat.jsonl` archives are the durable
record, and the CLI starts a fresh session on the restored side. The
owner signs the CLI in again after a restore.

**Restore gate:** `IsFreshForRestore` — active agents ⊆ {ceo,
chief-of-staff} AND CoS chat empty.

**Pre-validation** (`backup.CheckZip`): before any file is written,
every name is checked (no traversal, no absolute paths, no duplicates),
the members must be exactly what the zip's manifest lists, with the
manifest's content, and they must fit in the free space. A single bad
entry rejects the whole zip.

**Post-restore:** the same reconcile a boot runs, including
`SyncAgentFilesystem`, which regenerates each active agent's symlinks
(symlinks are not in the zip).

**Bug class prevented:** leaked credentials in exported backup;
restore overwriting a live deployment; path-traversal zip attacks; a
damaged zip leaving a half-restored deployment.

**Verified in:** `internal/web/backup_test.go`,
`internal/web/restore_parity_test.go`, `internal/backup/`.

---

## Summary table — invariants at a glance

| # | Invariant | Central test file |
|---|---|---|
| 1 | System prompt = stable identity only | `internal/agent/serialization_test.go` |
| 2 | chat.jsonl file order = UI = model | `internal/agent/serialization_test.go` |
| 3 | Serialized chat.jsonl writes | `internal/store/agent_test.go` |
| 4 | Mid-flight delivery → buffer + follow-up | `internal/web/chat_test.go`, `internal/web/chat_pending_test.go` |
| 5 | CLI session serialization | `internal/claudeagent/stream_test.go` |
| 6 | At most one loop per agent | `internal/web/ceo_chat_spawn_test.go` |
| 7 | Rotation is async + atomic | `internal/web/handoff_test.go` |
| 8 | Backup/restore is safe | `internal/web/backup_test.go` |

When adding a new feature that touches agent context, skim this
list. If any invariant could be broken, a test must lock in the
correct behavior. The central serialization test
(`internal/agent/serialization_test.go`) is the go-to place for
projection-level contracts; per-path tests cover behavioral
choreography (spawn, race recovery, rotation).
