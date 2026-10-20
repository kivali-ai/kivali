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
| **Org** | Settings for the whole team: usage, name and logo, handbook, project files, skills, network, software, backup, and about. |

The sidebar shows how many items wait on you, and every count updates live.

## Needs you

Everything waiting on you is in **Needs you** on Home. Expand an item to act on it:

| Item | What you do |
| --- | --- |
| **Approval** | **Approve** or **Deny**, with an optional note and attached files. |
| **Notice** | **Acknowledge**, with an optional reply. |
| **Install software** | Open it to see each package, with its version and a one-line description from Debian, and why the agent wants them, then **Install** or **Deny**, with an optional note. |
| **Hire**, **Offboard**, **Reorg**, **Role update**, **Handbook update** | Choose **Review** to open the proposal page with the full documents, then approve or deny with an optional note. |
| **Assigned to you** | Set **Resolution** to **Done** or **Dropped**, write the **Outcome**, then **Hand back**. |
| **Needs help** | An agent stopped and needs you. **Open chat** to see why. |

Your answer reaches the agent at once and wakes it.

## Release the Queue

Messages between agents wait in the **Queue** on Home until you let them through, or until [auto-release](#auto-release) does. Each row shows who it is from and to, what it is, and its title. Expand a row to read it.

- **Release** delivers it and wakes the recipient. Type a **Note to the recipient** first to add direction; it arrives as top-priority direction from you.
- **Bounce with note** sends a notice back to its sender undelivered, with your note as the reason. Type the note first; the button needs one. A notice addressed to several agents is cancelled for all of them. Assignment updates cannot be bounced: change the assignment on **Work** instead.
- **Release all** releases every row on screen. With auto-release **Off**, you can tick rows and release only those ("Release 3 selected").
- **All**, **Assignments** and **Notices** filter the Queue. Assignment updates are work moving; notices are usually the low-risk bulk.

A message released while its recipient is in the middle of a turn joins that turn.

### Auto-release

The **Auto-release** control sets how long the Queue holds a message before releasing it on its own: **Now**, **30s**, **2m**, **5m**, **20m** or **Off**. A new team starts at **30s**, and the control stays wherever you set it. Each counting-down row shows its time left, and you can still release or bounce it before then.

- A message's countdown is set when it arrives. Moving the control later does not change countdowns already running, and messages that piled up while it was Off stay queued for you.
- Moving to **Off** cancels every countdown.
- The setting is saved, survives restarts, and is the same in every browser tab.

**Off** is the default. Release is your main control over pace and spend, so start with Off or a long delay until you know how your team behaves.

## Talking to agents

Open any agent from **Team** and type in its **Chat**. It is like walking into a colleague's office: immediate, and part of the agent's context from then on. You can attach files with the paperclip.

- **While an agent is working**, a message you send waits for a moment to deliver and shows as pending. It reaches the agent at its next step. **Send now** pauses the agent's turn so it reads your message at once; **Delete** takes it back before it is delivered.
- **Stop** ends the agent's turn and cancels its background tasks. The agent then waits for you: it does not pick its work back up until you tell it to.
- **Background** shows the agent's subagents and their plan while they run. A task that works in a browser is marked as a web task with its session. While it runs, **Open browser** on its page opens a window that shows the task's browser live, with the page's address at the top. If the window cannot reach the task any more (after Kivali restarted, say) it says so; reopen it from the task's page. A line under the address saying that Chromium is running without its sandbox means the cluster does not let the task's browser use Chromium's own sandbox; the task works the same, with one layer of isolation fewer. Most of the time it is watch only: you can see what the task sees, and nothing you do in the window reaches the page. A web task in a site session (one locked to a site such as `vendor.com`, which keeps its sign-ins) can pause when a page needs you: a sign-in, a second factor, a CAPTCHA or a consent. Home then shows a row for it, badged with what the page asks (**Sign in**, **Second factor**, **CAPTCHA** or **Consent**) and naming the page the task is on; the task's page says the same, and **Background** lists the task as waiting for a person. Open the browser, which now takes your clicks, typing and paste (Cmd+V, Ctrl+V, or right-click and Paste), do what the page asks, and choose **Continue the task** in the window's banner: the task continues at once and the window closes (in a browser tab that stays open, it says the task is continuing, and you can close it). Closing the window instead also continues the task, a few seconds later. While you are in the window the page may leave the site, so a sign-in through Google, Okta, Microsoft or another identity provider can finish; once the task continues, a tab left on another site goes back to the site's home page. Either way, the agent that dispatched it learns nothing until the task's result. If the task's browser stops while it waits for you, the window says so: Kivali opens the browser again on the site's home page, and **Open browser** on Home takes you back to it. Your earlier sign-in steps there may be lost, so start again from that page. If the browser can't be opened again, the task stops waiting for you and carries on without your help, and its row leaves Home. A task nobody answers within a day ends on its own, saying so to its agent, and a task may ask for you at most three times; a wait whose browser had to be opened again does not count. If Kivali restarts while a task is running or waiting, the task does not continue: its page says so, and the agent that dispatched it is told which of its tasks ended.

A web task that uploads the agent's files waits for you before it starts. Home shows an **Upload N files to site** row with a link to download each file, the address the task starts at and what it was asked to do, and the task's page says it is waiting for approval. Open the files first: if one changed after the agent dispatched the task, its link and the task's upload are refused. Choose **Approve upload** and it starts, held to that site for its whole run, or **Deny**, with a note the agent reads, and it ends without uploading anything. A task nobody answers within a day ends the same way; one still waiting when Kivali restarts ends like any unfinished task. Every web task, an approved one too, has a budget on what it sends out: 2 KB for one action, 8 KB for one task and 32 KB a day for each agent. A task that goes over is refused that action and Home shows a **web budget** notice.
- **About** shows its role, habits and memory, and the software installed for it. See [Memory](memory.md) and [Software](#software).
- **Past chats** lists its earlier conversations.

Chat is right for questions, quick corrections and briefing. Anything you want to hear back about, ask the agent to send to you as a notice, so it lands in **Needs you** instead of scrolling away in a chat.

## Installing software for an agent

An agent that needs a program it does not have, such as a PDF reader or a video tool, can ask for it. You see an **Install software** request in **Needs you**; open it to see each package by name, with the version Debian 13 has and Debian's one-line description of it, and, in the agent's own words, what it is for. A name the agent got wrong never reaches you: it is checked against Debian's package list when the agent asks. Choose **Install** and the software is added to that agent's own list, and the agent restarts to install it as soon as it has finished what it is doing; the restart takes a minute or two. If the agent already has the software, nothing restarts. Its next command waits until the installation finishes. If something cannot be installed, the agent is told which and why. Choose **Deny** and nothing changes. Any agent can ask, but only for itself.

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

**Org**, **Network** lists the hosts an agent's shell commands can reach from its sandbox, such as a download or a package install. An agent's own web fetches are not limited to the list. Type a host under **Allow a host** and choose **Allow**: `example.com` allows it and everything under it (`api.example.com`); `api.example.com` allows only that. Changes apply at once. See [Security and privacy](security-and-privacy.md#network-access).

## Software

An agent can have extra programs installed, named by their Debian package, such as `ffmpeg` or `poppler-utils`. Software is installed each time an agent starts.

- **Org**, **Software** lists the packages every agent gets. Type one or more names (separated by spaces or commas) under **Add software** and choose **Add**; choose **Remove** beside a package to take it off.
- An agent's **About** tab has a **Software** view. It shows the org-wide list, which you change on the Org page, and the agent's own list, which you add to and remove from there.
- Changing either list restarts the agents it reaches, so they install the change. Changing the org list restarts every agent that does not already have it; changing one agent's list restarts only that agent. An agent at work restarts once it has finished its turn and any background tasks, and a restart takes a minute or two.

Each name is checked against Debian 13's package list before it is added. A name Debian has no package for is refused, and nothing is added; the message names it and suggests close matches, such as `imagemagick` for `imagemagik`. A name that several packages provide, such as `awk`, is refused with the packages to choose from. Kivali keeps a copy of the list, so a short outage at Debian changes nothing. If it has never been able to download the list, for example on a network without internet access, the names are added anyway and the page says they could not be confirmed; a name Debian does not have is then reported when the agent installs it. Agents are not limited to what you add here: an agent that needs software asks you to install it, and you approve or decline.
