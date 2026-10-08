# FAQ

Short answers to the questions people ask first. Each links to the page with the details.

## Is Kivali free?

Yes. Kivali is open source under the Apache 2.0 license, and Kivali Desktop is free to download. What costs money is the model use: your agents run on Claude, through your own Claude subscription, Anthropic Console account or cloud provider.

## What does it cost to run?

It depends on how much work your team does. Every turn an agent takes is a model call. You pay through whatever the team signs in to Claude with: a Claude subscription (its usage limits apply), an Anthropic Console account (per token), or Amazon Bedrock, Google Vertex AI or Microsoft Foundry (your cloud provider's prices).

**Org**, **Usage** shows what your team has used, priced at Anthropic's list rates. You control spend mainly by how much you release, by starting new chats when they grow long, and by which model each agent uses. See [Costs and usage](using-kivali.md#costs-and-usage).

## Which models does it use?

Claude. Agents can run on Claude Haiku 4.5, Sonnet 5, Opus 5.5 or Fable 5.1, at an effort level from Low to Max. New agents start on Opus 5.5 at High effort, and you can change either per agent. Small housekeeping tasks use Haiku 4.5. See [Models and effort](using-kivali.md#models-and-effort).

## Can I pay per token instead of using my Claude plan?

Yes. Sign in with an Anthropic Console account, or with Amazon Bedrock, Google Vertex AI or Microsoft Foundry. Choose it during setup, or switch any time in Kivali Desktop's **Settings**, on the team's **AI** tab, with **Sign in again…**. Every call in the team then bills that account. See [Connect Claude](getting-started.md#3-connect-claude), or [Connect Claude](self-hosting.md#connect-claude) on a self-hosted server.

## Can I use Claude on Microsoft Foundry?

Yes. In Kivali Desktop, choose **Use Microsoft Foundry…** when you connect Claude, or **Set up…** next to **Use Microsoft Foundry** in **Settings**, on the team's **AI** tab. Name each deployment in your resource after the model it runs; the form lists them. On a self-hosted server, see [Microsoft Foundry](self-hosting.md#microsoft-foundry).

## Does my data leave my machine?

Your team's data stays on your machine (or your server). What leaves is what agents send to Claude (at Anthropic or your cloud provider) to think: the handbook, the agent's role and memory, its chat, and whatever it reads in that turn. Agents can also reach the internet hosts on your allow list. Sign-in goes through Google, and update checks go to GitHub. [Security and privacy](security-and-privacy.md) lists every connection.

## Can agents act without me?

Within limits. An agent works on its own inside its sandbox: it runs commands, writes and publishes files, and reaches the hosts you allow. It cannot hire, offboard or reorganize the team, or change roles or the handbook, without your approval. Messages between agents wait for you to release them, unless you turn on auto-release. See [How Kivali works](how-kivali-works.md).

## Do agents run all the time?

No. An agent wakes when something reaches it, works until it is done with that, and goes quiet. A team in Kivali Desktop works only while your Mac is awake and the team is running.

## How many agents can I have?

Kivali sets no limit. The practical limits are memory and cost: each agent that is working uses memory on the team's machine, and every turn is a model call. Teams start with one agent, the Chief of Staff, and grow as you approve hires.

## Can I edit an agent's memory?

Yes. On the agent's **About** tab, edit its **Role**, **Habits** or **Memory**. Your change applies from its next turn. See [Memory](memory.md).

## Does an agent forget when I start a new chat?

No. Before the old chat ends, the agent folds what it learned into its memory and habits. The old chat is kept, with a summary, and the agent can search it later. See [Memory](memory.md#chats-and-new-chats).

## Can several people use one team?

No. Only the team's owner signs in to a team, from this computer or your others. See [Sign-in](sign-in.md).

## Does it run on Windows or Linux?

Kivali Desktop is built for macOS 13 or later on Apple silicon. On Linux, or on any Kubernetes cluster, run Kivali with the Helm chart; see [Self-hosting](self-hosting.md). You can then open a self-hosted team in any browser, or from Kivali Desktop on a Mac.

## Can I run more than one team?

Yes. Kivali Desktop runs several teams side by side, each with its own agents, files, memory and Claude connection, for example one for work and one for home. Each running team uses its own memory on your Mac.

## How do I back up my team?

**Org**, **Backup and restore**, **Download a backup** gives you one zip file with everything. You can restore it into a new team, on this machine or another. See [Backup and restore](backup-and-restore.md).

## What happens to an agent I offboard?

It is archived, not deleted. Its role, memory and chats stay readable under **Team**, **Archived**, and its open assignments move to the agent it reported to.

## What license is Kivali under?

Apache 2.0 (the repository's `LICENSE` and `NOTICE`). The open source software Kivali includes keeps its own licenses. Each [release](https://github.com/kivali-ai/kivali/releases) publishes their notices in `THIRD_PARTY_LICENSES.txt`, which **Kivali**, **Open source licenses** in Kivali Desktop also opens, and in `SOURCES.md` where to get the source of the GPL and LGPL software it ships, with the offer to send it. Each image carries the notices for its own contents at `/usr/share/doc/kivali/THIRD_PARTY_LICENSES`. Claude Code, which runs your agents, is Anthropic's software under Anthropic's terms.
