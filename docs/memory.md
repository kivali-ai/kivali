# Memory

Kivali agents learn as they work and remember what they learned. This page explains what an agent keeps, when it updates it, how it finds its way back to past work, and how you can read, correct and teach it.

## What an agent keeps

Each agent has three documents it carries into every turn, and an archive it can search.

| | What it holds | Who writes it | When it changes |
| --- | --- | --- | --- |
| **Role** | Who the agent is: its job, scope, who it reports to, how it works. | The Chief of Staff at hire, and you. | When you approve a role update, or edit it yourself. |
| **Habits** | Short rules for how the agent acts, each with a one-line reason. | The agent, and you. | When the agent starts a new chat, or when you edit them. |
| **Memory** | What the agent knows to be true, in its own words. Each entry notes where it came from: a past chat, or something a person told it. | The agent, and you. | When the agent starts a new chat, or when you edit it. |
| **Past chats** | Every earlier conversation, each with a short summary of what happened. | Kivali keeps them; a summary is written for each. | Every time a chat ends. Never edited afterwards. |

The role, habits and memory are part of every model call the agent makes. When they disagree, the handbook and the role win over habits, and habits win over memory. Memory is what the agent believes, not an instruction: the message in front of it and a correction from you come first.

## Chats and new chats

An agent's conversation keeps growing as it works. A long chat costs more on every turn and is slower, so from time to time you start a new one. The agent's header shows how full its chat is; the **New chat** button turns prominent once the chat is 80% full.

Starting a new chat is when the agent learns. Before the old chat closes, the agent answers three questions, in order:

1. What should I stop believing?
2. What did I learn that will stay true?
3. What changed about how I act?

It edits its memory and habits accordingly, then the old chat moves to **Past chats** and a fresh one begins. Its assignments and background work carry on. Messages that arrive meanwhile are held and delivered into the new chat.

Agents do not edit their memory or habits in the middle of a chat. Anything worth keeping is already in the chat, and the new-chat step reads it.

Agents keep memory lean on purpose. Memory and habits ride along on every call, so every line costs a little on every turn. The handbook asks agents to keep the two together under about 32 KB, to prune rather than grow, and to record how to look a fact up rather than the fact itself when it changes often.

## Past chats and episodes

After a chat ends, Kivali writes a short summary of it, called an episode: a title, what it touched, what was asked, done and concluded, and what was left open. Agents read their own episodes when a memory entry points to one, when they wonder whether they have seen a problem before, or when you refer to earlier work. They can also search the full text of their past chats.

On an agent's page, **Past chats** lists every earlier chat by its title. Opening one shows:

- **What happened**: the episode.
- **Notes it learned**: what the agent added to its memory from that chat.
- **Habits before and after**: a diff of its habits across that new chat.
- **Read the transcript**: the full conversation.

The diff is how you notice when something was dropped that should have stayed.

## Viewing and editing what an agent knows

Open an agent from **Team**, then its **About** tab. It has three documents:

- **Role**: what it does.
- **Habits**: how it works.
- **Memory**: what it remembers.

Each opens read-only; choose **Edit** to change it, then **Save**. Your edit applies from the agent's next turn. Archived agents' documents can be read but not edited.

Changes to a role usually come from your Chief of Staff as a **Role update** proposal, so the role stays consistent with the rest of the team. You can still edit it directly.

## Teaching an agent

There are several ways to make an agent know something, from lightest to heaviest:

- **Tell it in chat.** What you say becomes part of its conversation, and the next time it starts a new chat it decides what to keep. Say plainly that it should remember: "Remember that invoices go out on the 1st."
- **Correct it.** Agents are told to accept a correction from you at once and act on it, without arguing the point again.
- **Start a new chat after a lesson.** If you just taught it something that matters, start a new chat to have it fold the lesson in now, and check **Notes it learned** on the past chat.
- **Edit its memory or habits** on **About**, for a fact or a rule you want it to have word for word.
- **Ask the Chief of Staff for a role update**, for a change to what the agent is responsible for.
- **Edit the handbook**, in **Org**, **Handbook**, for something every agent should follow.
- **Upload a project file**, in **Org**, **Project files**, for reference material agents should read when they need it rather than carry everywhere.
- **Add a skill**, in **Org**, **Skills**, for a procedure agents should follow the same way every time.

## Where memory lives

An agent's role, habits, memory and past chats are plain files on your team's disk. They are included in every [backup](backup-and-restore.md). When an agent is offboarded, all of it is archived and stays readable; the Chief of Staff can draw on an archived agent's role when drafting a replacement.
