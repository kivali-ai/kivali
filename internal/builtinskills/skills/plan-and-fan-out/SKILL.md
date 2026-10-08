---
name: plan-and-fan-out
description: Break a task you hold into a written plan, delegate the parts to subagents, verify what comes back, and assemble one answer. The procedure for work you do yourself that is too big for a single sitting.
when_to_use: A task you hold that needs more than one sitting and that you will do with subagents — several independent parts, or a deliverable someone else will read. Skip it for anything you can finish in a few tool calls, and for parts that belong to other agents; those are assignments in the tracker.
version: 1.3.1
---

# Plan and fan out

Dispatching five subagents is not the same as breaking work down. The
difference is whether there was a plan before there were tasks.

This is the procedure for work you do yourself, with subagents, that is
too big to hold in one turn. Parts that belong to other agents are
assignments opened as parts of the one you hold: the tracker carries those, and
they are not tasks in this plan.

## 1. Name the deliverable first

Before anything else, write down what the finished thing is — in one
sentence, in the owner's terms. "A recommendation on whether to renew the
Acme contract, with the three numbers it turns on." Not "research Acme."

If you cannot write that sentence, you do not yet understand the
request. Ask, or state the interpretation you are proceeding on. Do not
start dispatching to find out.

## 2. Write the plan to `/files/background/plan.md`

Before you dispatch anything. The plan is a file, not a thought. This
is one shape that works, not a template to fill in — what matters is
that a reader can tell what is done, what is left, and where the
outputs are:

```markdown
# Renewal recommendation: Acme

**Assignment:** #42 — the one you hold for this work.
**Deliverable:** A recommend/decline call with the three numbers behind
it, in /files/background/recommendation.md. For the owner, by Thursday.

## Tasks
- [ ] 1. Pull actual spend vs. contracted minimum, last 12 months
      → /files/background/spend.md
- [ ] 2. List every support incident and its resolution time
      → /files/background/incidents.md
- [ ] 3. Find two comparable vendors and their list pricing
      → /files/background/alternatives.md
- [ ] 4. Check 1 and 3 against the source documents  (verify, after 1+3)
- [ ] 5. Write the recommendation from 1-3  (assemble, after 4)

## Open questions
- Does "cost" include the implementation hours? Assuming yes.
```

What makes a task well-formed:

- **Independently checkable.** Someone could tell whether it is done
  without reading your mind.
- **It names where its output goes.** A path under `/files/background/`, so
  the next task can consume it and you can assemble without asking
  anyone to resend anything.
- **It does not depend on anything else in the same wave.** Two tasks
  that each need the other's result are one task. A task that needs an
  earlier one's output is fine — mark it `after 2` and dispatch it in
  the next wave, once you have that output.
- **It is yours or a subagent's.** A task that names another agent is
  an assignment in the tracker, not a line here.

Every subagent working for you can read this file, and so can the owner.
Other agents cannot: it is in your workspace, not your published
files. A plan kept current is a status report you never have to
write.

## 3. Dispatch against the plan

Dispatch the tasks that are ready — a task whose input is another
task's output waits for it. Mark the dependency in the plan
(`after 1+3`) so the order is visible rather than remembered.

**Everything you dispatch together finishes together**, so by the time
you read the first answer the rest are already done. You can cancel
them; you cannot redirect them. The gap before the next batch is
therefore your only chance to change course, and your moment to ask the
owner anything the results raised.

It is also the only moment a worker's question can be answered: workers
cannot ask anything while they run, so one that comes back saying "I
cannot do this without knowing X" has done the right thing. Get the
answer and re-dispatch that task rather than guessing for it.

Each prompt must stand alone. A subagent sees its prompt and the shared
workspace, and nothing of your conversation. So: point it at
`/files/background/plan.md`, name the files it needs by path, say what
"done" looks like, and say where to put the result.

> Read /files/background/plan.md for context. You own task 2. Read every
> support ticket in /files/project/acme-tickets.csv, and write one row
> per incident — date, severity, hours to resolution — to
> /files/background/incidents.md. Then reply with just the median resolution
> time and the count of severity-1 incidents.

Pick the cheapest model that can do the job reliably. When in doubt, the
stronger one: a confidently wrong answer propagates silently and costs
far more than the model you saved on.

**Then end your turn.** The dispatch returns a receipt, not answers.
Do whatever does not depend on the results, say what you have
dispatched, and stop. You will be woken as each one lands.

## 4. Use a sub-lead when a task is itself a project

If one task needs several people, do not flatten it into your own batch
— dispatch it as one task and let that subagent split it. It can. Its
own dispatch blocks and it assembles its workers' answers into one
reply, so you get back a single reconciled result instead of three
fragments you would have had to reconcile yourself.

That is the whole point of the tier: one agent accountable for one
piece of the deliverable.

## 5. Verify claims, not artifacts

A subagent that wrote a file gave you something you can read. A
subagent that answered a question gave you something you have to trust.

For anything where a wrong answer would change the recommendation, add
a task that checks the claim against the source — a different subagent,
a cheap model, a narrow question:

> /files/background/spend.md claims total spend of $412k. Recompute it from
> /files/project/invoices-2025.csv and reply with the figure you get
> and whether it matches.

This is the cheapest insurance in the procedure. Confident wrongness is
the characteristic failure of delegated work, and it is invisible
unless something looks.

## 6. Update the plan as results land

Tick the box. Note anything that changed. If a result invalidates a task
you have not dispatched yet, strike it and say why — the plan should
always describe the job as you now understand it, not as you first
imagined it.

If a result makes the whole approach wrong, say so and re-plan. Cancel
the work that has become pointless rather than letting it land.

## 7. Assemble, and own it

Write the deliverable yourself, from the pieces. Your job here is
judgement, not concatenation:

- Where two results disagree, resolve it, or say which you trust and
  why. Never present both and leave the reader to pick.
- Where a result is thin, say so plainly rather than padding it.
- Where something failed and you proceeded anyway, name the gap.

Nobody delegated to you so they could read five subagent replies. They
delegated so that one agent would be accountable for one answer.

**For a wide job, delegate the assembly too.** Results land minutes
apart, with other conversations in between, and synthesising eight
fragments across a scattered afternoon is exactly the work a distracted
context does badly. Make the last task of the plan a subagent whose
prompt is "read /files/background/plan.md and every file it points at, and
write the full recommendation to /files/background/recommendation.md."

It gets one clean context with all the material in front of it. You
still read what it produces, check it against what you know, and put
your name on it — delegating the drafting is not delegating the
judgement.

## Starting over

Your background workspace, `/files/background/`, is one workspace, not
one job. When the assignment closes and you start something new, move
the old plan and its outputs aside —
`/files/background/archive/2026-09-acme/` — so the current plan is
unambiguously the current one.

## When not to use this

- Anything you can finish in a few tool calls. Writing the plan would
  cost more than doing the work.
- A question with one right answer and one place to look. Delegate it
  as a single task, or just look.
- Work whose parts all depend on each other. Splitting it produces
  workers who each need an answer only you have.
- Work whose parts belong to other agents. Open one assignment per part
  under the one you hold; the parts are the plan and their
  outcomes are the progress.
