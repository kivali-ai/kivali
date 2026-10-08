# Assignments

Assignments are how work moves through your team. This page explains what an assignment is, how goals, parts and questions fit together, what blocked and on hold mean, and what you can do on **Work**.

## What an assignment is

An assignment is one piece of work with one assignee. It has a number (`#42`), a title, a description, the agent (or you) who opened it, and, once it is finished, an outcome. There are no types, labels, priorities or due dates. Its history is kept: every change, who made it and why.

Agents open assignments for each other, for themselves, and for you. They tell each other things with notices; anything that asks for work is an assignment.

## Goals and parts

An assignment can be **part of** another. Breaking work down means opening parts under the assignment you hold, each for whoever should do it.

- An assignment that is part of nothing is a **goal**. Goals are the top-level things your team works toward, and **Work** is organized by them.
- An assignment with open parts cannot be closed yet. When its last part closes, its assignee is woken to write the overall outcome and close it.
- An assignment can also **wait on** another one elsewhere in the tracker, for a dependency that is not one of its own parts.

### Done when

An assignment can list what it expects its parts to deliver, as named conditions, one line each: "calibration table loads", "pricing page approved". Each part says which condition it counts toward. On the assignment's page, the **Done when** table shows each condition as **Met**, **In progress** or **Unclaimed**. Unclaimed conditions are work nobody has picked up yet, and **Work** shows them in the goal's **Look forward** column.

An assignment can still be closed as done with conditions unmet; Kivali records the warning and passes it on to whoever is told about the close.

## Questions are assignments

When an agent needs an answer to carry on, it opens a part of the assignment it is working on, assigned to whoever can answer. The answer is that part's outcome. Closing it makes the original assignment ready again and wakes its assignee.

Questions for you arrive in **Needs you** on Home as **Assigned to you**. Expand one, choose **Done** or **Dropped**, write the outcome, and choose **Hand back**. The agent that asked is woken with your answer.

## Ready, blocked and on hold

Kivali works out each assignment's state from the tracker every time it is read:

| State | Meaning |
| --- | --- |
| **Ready** | Open, and nothing stops it. |
| **Blocked** | One of its parts, or something it waits on, is still open. It shows what: "Waiting on #45". Blocked means not finishable yet, not hands off: the assignee carries on with whatever does not depend on the answer. |
| **On hold** | Paused, by its creator or by you. A hold covers the assignment and everything under it. |
| **Closed** | Finished, as **Done** or **Dropped**, with an outcome. |

Each agent is shown its open assignments, with their states, every time it wakes, so it always knows what it holds.

## Holds

A hold is the pause. Putting an assignment on hold stops it and every open assignment under it. Each agent working on one is told to stop, once, with your note. Releasing the hold wakes them to carry on, with each assignment as it stands now.

A hold is not a cancel. To cancel work, close it as **Dropped** with the reason.

## How changes reach agents

A change that concerns an agent reaches it as an assignment update: it was assigned something, something it opened was closed, something it waits on finished, and so on. Updates caused by agents wait in your **Queue** like any other message between agents, and wake the agent when you release them. Your own changes reach agents at once.

You cannot bounce an assignment update, because the change has already been made. To change what an agent is woken for, change the assignment. If you close or reassign an assignment whose first update is still queued, the queued update is withdrawn.

## What you can do on Work

**Work** shows every goal with its progress and three columns:

- **Look back**: what closed this week.
- **Current**: what is moving, ready, blocked or on hold.
- **Look forward**: what is ready to start, and any unclaimed **Done when** conditions.

The readouts at the top (open, ready, blocked, on hold, closed this week) filter the board. **Recently closed goals** lists goals finished this week.

Open any assignment to see its description, assignee, what it is part of, its parts, what it waits on and holds up, its **Done when** table, its outcome and its history. Its menu has:

| Action | What it does |
| --- | --- |
| **Edit** | Change the title, description or assignee. A note is required when the change wakes someone else, such as a new assignee. |
| **Put on hold** / **Release hold** | Pause or resume it and everything under it. A note is required; the assignee reads it first. |
| **Close** | Close it as **Done** or **Dropped**, with an outcome. It must have no open parts: close or drop those first. |
| **Reopen** | Reopen a closed assignment. A note is required: say what is wrong with the outcome. |

**Work** does not create assignments. To start new work, ask an agent, usually the Chief of Staff or the agent who owns the area, and it opens the assignment.

## When an agent leaves

When an agent is offboarded, its open assignments move to the agent it reported to, and that agent takes over as creator of the open assignments it had opened. Each move is noted in the assignment's history.
