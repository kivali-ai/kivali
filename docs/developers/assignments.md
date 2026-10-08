# The assignment tracker: contract

This document is the contract the code is built to. Where the code and
this document disagree, fix one of them in the same commit. For how
assignments look to someone running a team, see
[../assignments.md](../assignments.md).

## Why

Messages are a poor carrier for work. If an ask lives in one message
and its answer in another, whether it is still open is a scan over
every message on disk; nothing says what an ask waits on, nothing
groups asks into a larger piece of work, and the only way to hand an
ask off or ask a question about it is another message. Agents left
with only messages use them as a substitute for a tracker the platform
does not have.

The tracker is that system. An assignment is one file. It has one
assignee, one creator, at most one assignment it is part of, and the
assignments it waits on. The runtime derives whether it is blocked,
wakes whoever has to act when that changes, and refuses everything
else. There is no comment primitive, on purpose: an assignment is a
spec and an outcome, and anything that needs discussion needs a chat.

## What an assignment is

One kind of thing. No types, no labels, no priority, no due dates. An
assignment with parts is a container. An assignment that is part of
nothing is a **goal**. A question is an assignment for whoever can
answer it, opened as a part of the assignment that raised it.

| Field | Written by | Rule |
|---|---|---|
| `id` | core | sequential integer, rendered `#42` |
| `title` | creator | one line, at most 200 bytes |
| body (the description) | creator | markdown, at most 4096 bytes; substance goes in an artifact the body points at |
| `assignee` | creator | required, defaults to the creator; an active agent or `ceo` |
| `creator` | core | who opened it |
| `parent` | creator | the assignment this is part of; at most one, and it must be open |
| `blocked_by` | creator or assignee | assignments in other trees this one waits on; cycles refused |
| `acceptance` | creator or assignee | the Done-when conditions: named deliverables this assignment expects from its parts, one line each, unique within the assignment; optional |
| `satisfies` | whoever opens it; then the creator, assignee, or the creator or assignee of the assignment it is part of | which Done-when conditions of the assignment it is part of this one counts toward; only names that assignment declares; needs `parent` |
| `nudge` | core | set when the assignment gains its first part while it has no Done-when conditions; cleared once the assignee's wake note has said so |
| `held` | creator | the pause: this assignment and everything under it stop until it is resumed |
| `status` | core | `open` or `closed`, nothing else is stored |
| `resolution`, `outcome` | assignee at close | `done` or `dropped`, plus one text of at most 2048 bytes |
| `log` | core | one entry per change: seq, timestamp, actor, op, and the note that explains it |

The field names are both the front-matter keys on disk and the tools'
JSON field names (§Storage, §Tools).

### Derived, never stored

An assignment is **blocked** when any assignment in its `blocked_by`
is open, or any of its parts is open. It is **on hold** when it, or
any assignment it is part of, is held. It is **ready** when it is
open, not on hold and not blocked. All three are computed from the
files every time they are read; nothing writes them, so they cannot go
stale. A container becomes ready when its last part closes, and its
assignee is woken to write the rollup outcome and close it.

### Done when

A container is done when its parts are closed, and its parts are
whatever got opened. Nothing in that says what must exist for it to
be done. The Done-when conditions (`acceptance`) do: the assignment's
creator or assignee lists the named deliverables it expects from the
work opened under it, typically when it is opened and independent of
what has been opened so far. A part says in `satisfies` which of those
conditions it counts toward. Names are free text, scoped to the
assignment that declares them, and a part may only name conditions
the assignment it is directly part of declares; the refusal carries
that list. An assignment with no Done-when conditions is a leaf:
closing it as done is what meets the condition it counts toward.
There is no leaf or container kind; an assignment becomes a container
when a part is opened under it.

Everything about conditions is derived from `satisfies`, the
resolution and the tree; nothing about their state is stored:

- A condition is **met** when an assignment that counts toward it
  closes as done. Dropping meets nothing. Reopening un-meets it. When
  several parts name one condition, the first done close meets it.
- A condition is **claimed** while an open part counts toward it, and
  **unclaimed** when nothing opened names it: the work nobody has
  noticed.
- **Progress** of an assignment with Done-when conditions is three
  counts, printed `N met / N claimed-open / N unclaimed`, and this
  replaces the part counts wherever the tracker summarises a
  container: the wake note, `assignment_view`, `assignment_list` and
  the Work board. An assignment without
  conditions is summarised by its parts.
- **Closing as done with conditions unmet** succeeds. The names are
  recorded on the closing log entry, returned to the closer, and
  carried in every wake the close sends, as "Warning: closed as done
  with N Done-when condition(s) unmet: …". Never a refusal: blocking
  teaches vaguer conditions.
- **The nudge.** When an assignment gains its first part while it has
  no Done-when conditions, its assignee's next wake note says once
  "#N now has parts and no Done-when conditions." It is not repeated
  and never a wake on its own; `nudge` is the one flag that makes
  "once" true across restarts.
- Propagation is implicit: a leaf's done close meets the condition it
  counts toward; when a container's conditions are all met it closes
  cleanly, which meets the condition it counts toward one level up,
  and so on. A container closed with conditions unmet still meets
  what it counts toward; the warning travels with it.

The platform never judges whether a deliverable is good; it reports
whether a named deliverable was claimed and closed. Nothing here
calls a model.

### Parts

`parent` is the one hierarchy edge: it makes an assignment part of
another. A part is part of the work of the assignment above it, so an
open part holds that assignment up. Breaking work down and asking
questions are the same edge: the assignee of #7 opens #42 as a part of
it for whoever should do it; #7 waits until #42 closes. Only the
creator, the assignee and the CEO may open parts under an assignment,
because an open part holds it up.

`blocked_by` exists for the one thing the hierarchy cannot say: a
dependency on an assignment in another tree. It is deliberately
narrow.

### Questions are assignments

An assignee who needs input opens a part for whoever can answer, and
carries on with whatever is not waiting on it. The answer is that
part's outcome. Closing it makes the assignment above ready again and
wakes its assignee through the same path every other unblock uses.
There is no waiting state and no question field. Questions are
countable: an assignment with several question parts under it is
visible on the Work board, and a second question on the same
assignment means the spec needs a conversation: the question says so,
and the CEO decides.

### Holds are the pause

`blocked_by` says what an assignment waits on; it does not tell anyone
to stop, and it cannot: an assignee who opens a question under an
assignment carries on with the rest of it, so "blocked" must mean "not
finishable yet", never "hands off". Pausing is a different thing, and
it is the creator's: the creator or the CEO puts an assignment **on
hold** with a note, and the assignment and every open assignment under
it stop. Their assignees are told to stop, each once, listing the
assignments of theirs the hold covers; every listing and every wake
note shows them on hold; none of them is ready until the hold lifts.
Resuming wakes the same agents to carry on, with the state each
assignment is in now.

A hold is told, never woken for. An agent mid-turn has it folded into
the running turn and stops; an idle agent is not working on the
assignment, has nothing to stop, and reads the hold with whatever
wakes them next. The delivery lands in the transcript at once either
way, marked quiet, so the spawn gate walks past it on every later
broadcast wake. A resume is a wake like any other: there is work to
do.

A hold travels down the parent edge only, because a part is part of
the work above it. An assignment in another tree that waits on a held
assignment was waiting already and hears nothing. A hold is a change
on its own: `assignment_update` refuses one alongside any other field,
since it wakes everyone under the assignment and nothing else should
ride on that. An assignment's own hold survives a resume of the one it
is part of, so a manager can resume a goal and keep one part paused.
Closing an assignment lifts its own hold. A hold is not a cancel: to
cancel, drop.

## Rules

Every write goes through core, which validates the whole record before
it touches disk. Only core writes these files.

| Action | Who | Requires |
|---|---|---|
| open (create) | any agent, the CEO | title; assignee defaults to the creator |
| amend title or body | creator, CEO | a note when the assignee is someone else |
| reassign | creator, CEO | a note; the assignee cannot hand an assignment on |
| set or clear what it is part of | creator, CEO | the new container is open and not a descendant; clearing makes it a goal |
| add or remove `blocked_by` | creator, assignee, CEO | no cycle through `blocked_by` and `parent` together |
| add or remove Done-when conditions (`acceptance`) | creator, assignee, CEO | names unique; a condition an open part claims stays until that part's `satisfies` changes |
| add or remove what it counts toward (`satisfies`) | creator, assignee, the creator or assignee of the assignment it is part of, CEO | only conditions that assignment declares; a move to another assignment removes the old one's names in the same call |
| hold or resume | creator, CEO | a note; nothing else in the same change |
| close as `done` | assignee, CEO | an outcome; no open parts |
| close as `dropped` | assignee, creator, the creator or assignee of any assignment it is part of, CEO | an outcome saying why; no open parts |
| reopen | creator, CEO | a note; the assignment it is part of, if any, is open |
| move the creator | CEO (an offboard) | the assignment is open |

Dropping reaches down the tree so that whoever opened a goal can
cancel it: the parts its assignee opened are part of the goal's work,
and the goal cannot close while they are open. A closed assignment
takes no edit but reopen. When an agent is offboarded, the open
assignments it held move to the agent it reported to, and that agent
becomes the creator of the open assignments it opened, with a note on
each; the creator is who hears when an assignment closes and who may
amend, reassign or reopen it, and an archived agent can do none of
that. Reopening an assignment whose assignee has since left lands it
on the reopener, with a log entry saying why, to hand on with
`assignment_update`.

Every free-text field is stored with LF line endings and no trailing
newline whatever the transport sent, so a record compares equal to
what its own form posts back.

## Wakes

A change that concerns an agent reaches them as an assignment event:
a message whose stored type is `assignment_event` (§Storage),
from the acting agent to the affected agent, carrying the change, its
note or outcome, and a link to the assignment. It rides the existing
pipe unchanged: an agent's change waits in the Queue on Home for the
CEO to release it, the CEO's own changes deliver at once, and a change
that concerns the CEO lands in "Needs you". Nothing can reply to an assignment
event and the CEO cannot bounce one; the CEO changes the assignment on
the Work board instead. Closing or reassigning an assignment whose
first wake is still queued removes the queued wake, and the agent who
never saw it is not told to stop: nobody is woken for work that no
longer exists. A wake a crash left unrouted is routed at the next
boot, and one that had already landed is not delivered again.

The agent reads each one behind the lead "The assignment tracker
reports a change to assignment #N, made by X. Act on the assignment
itself with the assignment_* tools; nothing is owed back on this
message."

| Change | Woken |
|---|---|
| opened | the assignee |
| reassigned | the new assignee, and the old one so they stop |
| title or body amended | the assignee |
| closed | the creator, plus the assignee of every assignment that just became ready |
| dropped by anyone but the assignee | the assignee |
| reopened | the assignee |
| creator moved | the new creator |
| put on hold | the assignee, and the assignee of every open assignment under it, to stop; delivered without a wake |
| resumed | the same, to carry on |
| `blocked_by` or `parent` changed | nobody, unless an assignment became ready |

The actor is never woken for their own change. Everything not in the
table is visible on the next `assignment_list` and wakes nobody.

Every wake, whatever woke the agent, ends with the runtime's wake note
([knowledge-graph.md](knowledge-graph.md), "The wake note"), whose last section,
headed "Your open assignments:", lists the agent's open assignments by
id and title, each marked ready, on hold, or blocked by which ids, and
on a container with Done-when conditions its three counts, computed as
the turn starts. The section ends with the one-time nudge for any
assignment of theirs that gained parts without Done-when conditions;
the flag behind it is cleared once the note has landed. It is once per
wake, never on a delivery: an agent released five messages reads it
once, and a message folded into a running turn brings no copy. An
agent holding nothing gets no section; the assignment event that
closed or moved its last assignment is itself the news. That section
is the whole answer to "what should I be doing": the agent does not
have to remember to ask.

## Tools

Six, served by `kivali mcp` through the state-tool endpoint the graph
tools use. Subagents do not see them.

- `assignment_create(title, description?, assignee?, parent?, blocked_by?, acceptance?, satisfies?)`
- `assignment_update(id, title?, description?, assignee?, parent?, add_blocked_by?, remove_blocked_by?, add_acceptance?, remove_acceptance?, add_satisfies?, remove_satisfies?, hold?, note?)`
- `assignment_close(id, resolution, outcome)`, returning the warning
  when a done close leaves Done-when conditions unmet
- `assignment_reopen(id, note)`
- `assignment_list(assignee?, creator?, parent?, status?, ready?)`,
  ids and one line each with the derived state; defaults to the
  caller's open assignments. A line reads
  `#2 blocked by #3 "Title" — assignee bob, creator alice, part of #1, counts toward "x", done when 1 met / 0 claimed-open / 1 unclaimed; blocks #1`,
  or, on a container without conditions, `…, 1 part (1 open); …`
- `assignment_view(id)`, the full record: fields, description,
  outcome, the Done-when table (condition, state, which part claims or
  met it), the condition it counts toward, its parts, blockers, what
  it holds up, the log

The JSON field names are the model-visible wire schema: `parent` is the assignment this is part of,
`blocked_by` what it waits on, `acceptance` / `add_acceptance` /
`remove_acceptance` the Done-when conditions, `satisfies` /
`add_satisfies` / `remove_satisfies` the conditions it counts toward,
and `resolution` is `done` or `dropped`. The tool descriptions explain
each in the tracker's own words.

Each tool's refusal names the rule and the tool to reach for instead.

## Storage

Renaming any name in this section means migrating every install's
files.

```
data/assignments/
  000042.md            one file per assignment; front matter is the
                       record, the body is the description
  pending/<seq>.json   wakes computed for a change and not yet routed;
                       routed at boot, and after a restore, if a crash
                       left any behind; the assignment id is under the
                       JSON key `assignment`
```

- The directory is `assignments/`, with `assignments/pending/` inside it.
- The front-matter keys are the field names in the table above,
  including `parent`, `blocked_by`, `acceptance` and `satisfies`.
- Log `op` values are `created`, `assigned`, `amended`, `parent`,
  `blocked`, `unblocked`, `closed`, `reopened`, `creator`, `held`,
  `resumed`, `acceptance` and `satisfies`.
- An assignment event is a message of type `assignment_event`, and
  the message's front matter names the assignment under the key
  `assignment`. Its file name carries the type too.

The files are the only state. Ids and log sequence numbers are derived
from the files, so there is no counter to lose. One store mutex
serialises every read-modify-write. A file that fails validation on
read is refused and reported, never served as partial state. When a
body is amended the prior text is snapshotted into the content-
addressed attachment store and the log entry carries its hash.

```yaml
---
id: 42
title: Fix the login redirect
status: open
assignee: engineering-lead
creator: chief-of-staff
parent: 7
blocked_by: [40]
acceptance:
  - sign-in returns to the page it left
  - expired sessions land on sign-in
satisfies: [login flow fixed]
created: 2026-09-21T18:02:11Z
updated: 2026-09-21T18:40:03Z
log:
  - {seq: 118, ts: 2026-09-21T18:02:11Z, by: chief-of-staff, op: created, to: engineering-lead}
  - {seq: 130, ts: 2026-09-21T18:40:03Z, by: engineering-lead, op: blocked, ref: 40}
  - {seq: 131, ts: 2026-09-21T18:41:00Z, by: engineering-lead, op: acceptance, items: [sign-in returns to the page it left, expired sessions land on sign-in]}
---
Fix the login redirect on top of the session change from #40. Done
means sign-in returns to the page it left and the check in
engineering-lead/session-expiry passes.
```

## Assignments and messages

There are no task-request or status-update messages. `publish_notice`
and the CEO-bound tools are the only messages an agent sends; the
handbook's communication rule is: tell with a notice, ask by opening
an assignment.

## Orientation

`internal/assignments/` is the pure package: the record, its
validation, the derived state, every rule, and the wakes a change
produces. `internal/store/assignments.go` reads and writes the files.
`internal/tracker/` applies a change end to end: validate, write,
route the wakes. `internal/agent/assignment_tools.go` holds the six
tools' names, descriptions and schemas; `internal/mcp/assignment_tools.go`
serves them. The CEO's changes go through `internal/web/assignments.go`
and the JSON API under `/api/v1/assignments/{id}` (`api_work.go`,
`api_home.go`); an agent reads an assignment event through
`internal/agent/inbox.go` like every other message.

Prompt surfaces: the tool descriptions and `renderFilesystemSection`
ship in the binary; `internal/seed/handbook.md` §Assignments and
`internal/seed/cos_role.md` are copied into an install once, when it is
seeded, so a change to them reaches an existing install only by editing
its `handbook.md` and the Chief of Staff's `role.md`.
