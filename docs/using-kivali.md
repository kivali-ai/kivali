# Using Kivali

This page covers the day to day: working through Home, releasing messages, talking to agents, growing and reshaping the team, starting new chats, project files, skills, models, and keeping an eye on cost. If you have not read [How Kivali works](how-kivali-works.md), start there.

## The screens

The sidebar has five destinations:

| Screen | What it is for |
| --- | --- |
| **Home** | Everything that needs you, your goals in flight, and the Queue. **History** shows what you approved, answered and released. |
| **Team** | Your agents by reporting line, what each is doing, and archived agents. Open an agent to chat with it. |
| **Work** | Every assignment, grouped by goal. See [Assignments](assignments.md). |
| **Graph** | Everything your agents published. See [The knowledge graph](knowledge-graph.md). |
| **Org** | Settings for the whole team: usage, name and logo, handbook, project files, skills, network, backup, and about. |

The sidebar shows how many items wait on you, and every count updates live.

## Needs you

Everything waiting on you is in **Needs you** on Home. Expand an item to act on it:

| Item | What you do |
| --- | --- |
| **Approval** | **Approve** or **Deny**, with an optional note and attached files. |
| **Notice** | **Acknowledge**, with an optional reply. |
| **Hire**, **Offboard**, **Reorg**, **Role update**, **Handbook update** | Choose **Review** to open the proposal page with the full documents, then approve or deny with an optional note. |
| **Assigned to you** | Set **Resolution** to **Done** or **Dropped**, write the **Outcome**, then **Hand back**. |
| **Needs help** | An agent stopped and needs you. **Open chat** to see why. |

Your answer reaches the agent at once and wakes it.

## Release the Queue

Messages between agents wait in the **Queue** on Home until you let them through. Each row shows who it is from and to, what it is, and its title. Expand a row to read it.

- **Release** delivers it and wakes the recipient. Type a **Note to the recipient** first to add direction; it arrives as top-priority direction from you.
- **Bounce with note** sends a notice back to its sender undelivered, with your note as the reason. Type the note first; the button needs one. A notice addressed to several agents is cancelled for all of them. Assignment updates cannot be bounced: change the assignment on **Work** instead.
- **Release all** releases every row on screen. With auto-release **Off**, you can tick rows and release only those ("Release 3 selected").
- **All**, **Assignments** and **Notices** filter the Queue. Assignment updates are work moving; notices are usually the low-risk bulk.

A message released while its recipient is in the middle of a turn joins that turn.

### Auto-release

The **Auto-release** control sets how long the Queue holds a message before releasing it on its own: **Now**, **30s**, **2m**, **5m**, **20m** or **Off**. Each counting-down row shows its time left, and you can still release or bounce it before then.

- A message's countdown is set when it arrives. Moving the control later does not change countdowns already running, and messages that piled up while it was Off stay queued for you.
- Moving to **Off** cancels every countdown.
- The setting is saved, survives restarts, and is the same in every browser tab.

**Off** is the default. Release is your main control over pace and spend, so start with Off or a long delay until you know how your team behaves.

## Talking to agents

Open any agent from **Team** and type in its **Chat**. It is like walking into a colleague's office: immediate, and part of the agent's context from then on. You can attach files with the paperclip.

- **While an agent is working**, a message you send waits for a moment to deliver and shows as pending. It reaches the agent at its next step. **Send now** pauses the agent's turn so it reads your message at once; **Delete** takes it back before it is delivered.
- **Stop** ends the agent's turn and cancels its background tasks. The agent then waits for you: it does not pick its work back up until you tell it to.
- **Background** shows the agent's subagents and their plan while they run.
- **About** shows its role, habits and memory. See [Memory](memory.md).
- **Past chats** lists its earlier conversations.

Chat is right for questions, quick corrections and briefing. Anything you want to hear back about, ask the agent to send to you as a notice, so it lands in **Needs you** instead of scrolling away in a chat.

## Hiring, offboarding and reorganizing

You do not create or remove agents directly. Your Chief of Staff does, through proposals you approve.

**Hiring.** Ask your Chief of Staff for the role you need: what it should own and why no one else can. It drafts the role and sends a **Hire** proposal. Review it and choose **Approve hire**; the agent is created the moment you approve. Other agents can also make the case for a hire to their manager, and it reaches the Chief of Staff from there.

**Offboarding.** Ask your Chief of Staff to let an agent go. Approving the **Offboard** proposal archives the agent: its role, memory and chats stay readable under **Team**, **Archived**, and its open assignments move to the agent it reported to. An agent with direct reports cannot be offboarded until those reports are moved, and the Chief of Staff cannot be offboarded.

**Reorganizing.** A **Reorg** proposal moves reporting lines. Each move applies on its own, so one bad move does not sink the rest.

**Changing a role or the handbook.** The Chief of Staff can propose a **Role update** for any agent, or a **Handbook update**, which you see as a diff. You can also edit the handbook yourself in **Org**, **Handbook**, and any agent's documents on its **About** tab.

## New chats

An agent's chat grows as it works, and a long chat is slower and costs more on every turn. Start a new chat with the **New chat** button on the agent's page. It becomes prominent when the chat is 80% full.

Before the old chat ends, the agent folds what it learned into its memory and habits. The old chat then moves to **Past chats**, with a short summary of what happened, and a fresh chat begins. Assignments and background work carry on. See [Memory](memory.md#chats-and-new-chats).

An agent cannot start a new chat while it is in the middle of a turn. Wait for it to finish, or choose **Stop** first.

## Project files

**Org**, **Project files** holds the documents every agent can read: plans, briefs, specs, decks, contracts. PDFs and office documents are converted to text; images are readable by agents as images. Each file gets a one-line summary so agents can find what they need without opening everything. Each row shows whether its text was extracted, and you can download or remove files.

Keep long background here rather than in the handbook. The handbook is read on every turn; project files are read only when needed.

## Skills

**Org**, **Skills** lists the procedures agents can follow. A skill is a `SKILL.md` file, with a short header giving its `name`, `version` and `description`, plus any scripts and assets it needs.

- **Upload a skill**: a `SKILL.md` on its own, or a `.zip` of the skill's folder. Uploading a newer version replaces the old one; Kivali asks before replacing a skill with an older version.
- Turn a skill off with its switch, or remove it. Built-in skills can be turned off but not removed.

Agents see the name and description of every enabled skill and read the full instructions when they need them.

## Models and effort

Every agent runs on a Claude model with an effort level. New agents start on the team's default model, Claude Opus 5.5, at **High** effort. Change either for one agent with the **Model** and **Effort** pickers under its chat box.

| Model | Relative cost | Good for |
| --- | --- | --- |
| Claude Haiku 4.5 | Lowest | Quick lookups and simple, well-defined tasks. |
| Claude Sonnet 5 | Low | Most everyday work. |
| Claude Opus 5.5 | Higher | Hard reasoning and judgment. The default. |
| Claude Fable 5.1 | Highest | The most demanding work. |

Effort levels are **Low**, **Medium**, **High**, **Extra high** and **Max**. Higher effort spends more on thinking per turn. The pickers offer only the efforts the chosen model supports.

Agents choose the model for the subagents they start, usually the cheapest one that can do the job reliably.

## Costs and usage

**Org**, **Usage** shows spend and calls for the last 24 hours, 7 days and 30 days, spend per day, and each agent's share. **In flight** on Home shows today's and the last 7 days' spend at a glance. All spend is priced at Anthropic's list rates, whichever way Claude is signed in. With a Claude subscription, treat it as a measure of work done, not as a bill; on Amazon Bedrock, Google Vertex AI or Microsoft Foundry, your cloud provider's prices apply.

There is no spending cap. What moves your cost:

- **How much you release.** Every released message wakes an agent. **Release all** and short auto-release delays move the team fastest and spend the most.
- **Chat length.** Every turn re-reads the chat so far. Start new chats when the button suggests it.
- **Memory and habits.** They are sent on every call, so an agent with a large memory costs more on every turn.
- **Model and effort.** Use a smaller model or lower effort for agents doing routine work.
- **Subagents.** Background work is real work, billed like any other.

## Network

**Org**, **Network** lists the hosts agents can reach from their sandboxes. Type a host under **Allow a host** and choose **Allow**: `example.com` allows it and everything under it (`api.example.com`); `api.example.com` allows only that. Changes apply at once. See [Security and privacy](security-and-privacy.md#network-access).
