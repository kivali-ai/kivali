# Getting started

This page takes you from download to a working team: install Kivali Desktop, create a team, connect Claude, hire your Chief of Staff, and make your first hire. It takes about fifteen minutes, most of it reading and answering questions.

If you want to run Kivali on a server instead, see [Run it on your own server](#run-it-on-your-own-server) at the end.

## What you need

| | |
| --- | --- |
| A computer | A Mac with Apple silicon (M1 or later) and macOS 13 or later, or a 64-bit PC with Windows 10 or 11 Pro, Enterprise or Education. |
| Memory | Each running team uses 4 GB by default. You can change it per team in **Settings**, on the team's **This Mac** (**This PC**) tab, up to half your computer's memory. |
| Disk | Each team reserves up to 64 GB, but only what it actually uses takes space. |
| A Google account | You sign in to your team with it. Kivali reads only your email address. |
| Claude | A Claude subscription (Pro, Max, Team or Enterprise), an Anthropic Console account, or Claude on Amazon Bedrock, Google Vertex AI or Microsoft Foundry. See [Connect Claude](#3-connect-claude). |

Kivali Desktop runs on macOS and Windows, and works the same way on both. On Linux, or anywhere you run Kubernetes, use the [Helm chart](self-hosting.md).

## 1. Install Kivali Desktop

On macOS:

1. Download the latest `Kivali_<version>_aarch64.dmg` from the [releases page](https://github.com/kivali-ai/kivali/releases/latest).
2. Open the `.dmg` and drag **Kivali** into **Applications**.
3. Open Kivali from Applications.

On Windows:

1. Turn on Hyper-V if it is not on already: open **Turn Windows features on or off**, select **Hyper-V** and restart. Your teams run in Hyper-V virtual machines.
2. Download the latest `Kivali_<version>_x64-setup.exe` from the [releases page](https://github.com/kivali-ai/kivali/releases/latest) and run it. It asks for administrator rights once, to install the service that runs your teams' virtual machines.
3. Open Kivali from the Start menu.

The app lives in the menu bar on macOS and in the notification area on Windows. Closing its windows leaves it running there; **Quit Kivali** pauses your teams. See [Kivali Desktop](desktop-app.md) for the full tour.

## 2. Create a team

A team is one group of agents, with its own machine, files and memory. The first window offers two choices:

- **Create a team** runs it on this computer.
- **Connect to a team** opens one that runs on another computer or a server. See [Other devices](desktop-app.md#open-a-team-from-other-computers).

Choose **Create a team**. Setup has four steps. Your team's machine starts getting ready in the background while you answer; from step 3, a bar at the bottom shows its progress.

**Step 1: What is this team for?** Pick **Work** (a company, a practice, a project) or **Personal** (your home, plans, money, health). The choice sets the starting words and suggestions; you can change any of it later. Then name the team (**What should we call it?**) and, if you like, tell agents what to call you (**What should your agents call you?**), such as "Jane", "Dr. Patel" or "CEO". Without a name, agents call you "the CEO" on a work team and "the owner" on a personal one.

**Step 2: Sign in.** Choose **Continue with Google**. Your browser opens; sign in there. That Google account owns the team, and only it can open the team, from this computer or your other computers.

## 3. Connect Claude

**Step 3: Connect Claude.** Your team uses Claude Code's own sign-in, which runs inside the team. Choose **Open Claude sign-in**; a terminal window opens and starts Claude inside your team. Pick how to sign in:

| | What it bills |
| --- | --- |
| **Claude subscription** | Your Pro, Max, Team or Enterprise plan. Its usage limits apply. |
| **Anthropic Console account** | Your Console organization, per token. |
| **Amazon Bedrock**, under 3rd-party platform | Your AWS account. Use a Bedrock API key or an access key; the team has no AWS CLI to sign in with. |
| **Google Vertex AI**, under 3rd-party platform | Your Google Cloud project, through a service account key file the team can read. |

For a subscription or a Console account, Claude shows a link: open it, sign in, and paste the code Claude gives you back into the terminal window. For Bedrock and Vertex AI, Claude asks for your credentials, region and models. Kivali never sees what you type there. When the sign-in is done, the step shows what the team bills, such as **Claude Max** or **Amazon Bedrock**.

**Microsoft Foundry** isn't in Claude's sign-in. Choose **Use Microsoft Foundry…** on the same step instead, and enter your resource's name and its API key, or a service principal's tenant ID, client ID and client secret. Kivali saves them in Claude Code's settings inside the team, then checks each model: name each deployment in your resource after the model it runs, as the form lists them. A missing deployment doesn't undo the setup; the form names it so you can add it.

Everything in the team bills that sign-in: every agent, every background task, and Kivali's own small housekeeping calls (summaries of files and past chats). To switch to another account or another way of billing later, open **Settings**, the team's **AI** tab, and choose **Sign in again…**. Agents switch at the start of their next turn.

> [!NOTE]
> The **Usage** page in Kivali prices every call at Anthropic's list rates, whichever way you sign in. With a Claude subscription, read those numbers as a measure of how much work the team did, not as a bill; on Bedrock, Vertex AI or Foundry, your cloud provider's prices apply.

**Step 4: Ready.** When the team's machine is up, choose **Open** followed by your team's name. The team opens in its own window.

## 4. Set up your org

The first time a team opens, it asks a few short questions before your first hire. Every step saves as you go, so you can leave and come back.

**Your org.** The name and a square logo (PNG, at least 64 px a side) shown in the sidebar and on the sign-in page. Both are optional and you can change them later in **Org**, **Organization**. A team made in Kivali Desktop already has its name, so setup starts at the next step; use **Back** to change it.

**Project files.** Drop in anything that explains your business or your life: a plan, a deck, pricing, contracts, notes. PDF, Markdown, CSV, slides, office documents and images all work. Your Chief of Staff reads these to learn how things work, and every agent can read them later.

On a work team, tick **Write the handbook from these files?** to have your Chief of Staff draft a handbook for your business once it has read them. It sends the draft to you to approve. If the files are thin, it asks you a few questions first. On a personal team, files are optional: your Chief of Staff asks you a few questions either way and drafts an "About you" section of the handbook.

**Hire your Chief of Staff.** Your first agent. It starts with a default role and a default handbook, which you can read and edit here. Most people leave them as they are. Choose **Hire Chief of Staff**. Hiring takes 30 to 60 seconds; you can leave the page while it runs.

If this step says **No model is connected yet**, Claude is not signed in. Everything you entered is saved: connect Claude in Kivali Desktop's **Settings**, on the team's **AI** tab, then choose **Try again**.

## 5. Your first conversation

When the Chief of Staff is hired, choose **Go to Home**. Home is where everything that needs you lands, under **Needs you**. Your Chief of Staff introduces itself there, or sends the handbook draft or its questions, depending on what you chose.

Open its chat (from **Team**, or **Open its chat** on the last setup screen) and ask it to tell you what it understood about your business. This is the quickest way to find out whether your files said enough. If its answer is thin, add more files in **Org**, **Project files**, or tell it directly in chat.

If it sent a handbook draft, open it from **Needs you** with **Review**, read the changes, and approve or deny. You can add a note either way.

Read [How Kivali works](how-kivali-works.md) next. One idea in particular is different from a normal chat app: messages between agents wait for you to release them.

## 6. Your first hire

You never create agents directly. Ask your Chief of Staff in chat for the role you need, for example:

> I need someone to handle bookkeeping: monthly reconciliation, invoices out, and a short cash report each Friday.

The Chief of Staff drafts the role and sends you a **Hire** proposal. It appears in **Needs you** with **Review**. The proposal page shows the full role document. Choose **Approve hire**, and the new agent is live by the time your Chief of Staff hears back.

## 7. Take your first backup

Once you have a team you would be sorry to lose, take a backup: **Org**, **Backup and restore**, **Download a backup**. It is one zip file with everything the team keeps. See [Backup and restore](backup-and-restore.md).

## Run it on your own server

Kivali also runs on any Kubernetes cluster through its Helm chart, for a team you reach from anywhere. See [Self-hosting](self-hosting.md). You can open a self-hosted team from Kivali Desktop with **Connect to a team**.

To build Kivali from source, see the [developer docs](developers/README.md).
