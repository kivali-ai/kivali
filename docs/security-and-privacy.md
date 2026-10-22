# Security and privacy

This page explains where your team's data lives, what leaves your machine and where it goes, how agents are contained, what installing an integration lets in, which actions need your approval, where credentials are kept, and how to report a vulnerability.

## Where your data lives

All of a team's data is plain files on the team's own disk. There is no Kivali cloud service holding your team.

- **Kivali Desktop**: each team has its own disk inside its own virtual machine, stored under `~/Library/Application Support/Kivali` and readable only by your macOS user.
- **Self-hosted**: the team's data volume (`kivali-data`) on your cluster.

That data includes everything your agents know and everything you gave them: chats, memory, project files, published files, assignments and messages. [Backups](backup-and-restore.md) contain the same and are not encrypted, so store them with the same care.

## What leaves your machine

| Where to | What | Why |
| --- | --- | --- |
| **Anthropic, or Amazon Bedrock, Google Vertex AI or Microsoft Foundry** | Every model call: Kivali's instructions to the agent, the handbook, the agent's role, habits and memory, its chat, and whatever files and tool results the agent reads in that turn. It goes to whichever service your Claude sign-in uses. | This is how agents think. The terms of your Claude subscription, Anthropic Console account, or cloud provider govern this data. |
| **Google** | Your sign-in. Kivali reads only the email address Google confirms. | Signing in. |
| **The sign-in relay at kivali.ai** | The one-time code from a Google sign-in, when you use Kivali's built-in sign-in client. It returns Google's signed identity token, which your server verifies against Google's keys. Not used when you [use your own OAuth client](sign-in.md#use-your-own-google-oauth-client). | Exchanging the sign-in code needs the client secret, which only the relay holds. |
| **GitHub** | Update checks, from Kivali Desktop only: a check for a new app at launch, and a check for a new team version once a day while each team runs. Updates download from the project's GitHub releases. A self-hosted server makes no update checks. | Updates. |
| **Time servers** | A request to `www.google.com`, `www.cloudflare.com` or `gsa.apple.com`, to compare the server's clock with the outside world. | Message records are filed by date, so a wrong clock matters. |
| **Hosts on your network list** | Whatever an agent's shell commands send from its sandbox, such as a download or a package install. | Agents' own work. See [Network access](#network-access). |
| **Public websites** | What an agent's own web fetches and searches send, and what a web task's browser loads and types. | Agents reading the web. Limited by a budget on what they send out. See [Network access](#network-access). |
| **An integration's hosts** | What an approved integration sends the service it connects to: its connection, and what it is asked to do there, such as an agent's call or a Test connection. Only to the hosts its install screen listed. | The integration's work. See [Integrations](#integrations). |
| **The Git integration's host** | The token you gave it, with each clone, fetch and push an agent makes from its sandbox and each Test connection Kivali's server makes; for a Test connection with no repository named, to GitHub's or GitLab's account endpoint. Only to the host its connection names. | Agents' work with your repositories. See [Integrations](#integrations). |

Kivali's own housekeeping (summaries of project files and past chats) is model calls too, billed to the same Claude sign-in.

## How agents are contained

Each agent runs in its own sandbox: its own pod with its own storage, separate from Kivali's server and from every other agent.

- **Its shell and file tools** run in a dedicated container. They see the agent's workspace, the project files, skills, its own past chats, and everything the team has published, and nothing of the server or of other agents' workspaces. Project files, skills and published files are read-only there; only Kivali itself publishes an agent's files, when the agent asks.
- **The shell's own process**, the one that runs an agent's commands, has a user of its own, apart from the user the commands run as. A command cannot read that process's memory or environment, or stop it.
- **No way in.** An agent's pod runs no service other pods or computers can connect to, and has no access to the Kubernetes API. It reaches Kivali's server only through a private socket, and every action it takes goes through the server's checks.
- **Network.** An agent's pod can reach only the egress proxy, by its address. It has no DNS and no route to anything else, so a command that ignores the proxy goes nowhere. The server's web port refuses connections from any pod, and every request to it needs a signed-in session in any case.
- **Subagents** run inside their agent's sandbox, under the same limits. A web subagent's browser runs in a pod of its own, described under Network access.

## Network access

Everything an agent sends out of its sandbox goes through a proxy. The proxy treats two kinds of traffic differently.

### The agent's shell

Commands the agent runs, such as `curl`, `pip`, `git` and `apt` and anything else typed into its shell, reach only the hosts on your team's list and the hosts you add for that agent. Edit the team's list in **Org**, **Network**, and an agent's own hosts on its **About** tab, under **Security**; changes apply at once. A request to any other host is refused, and the agent sees the refusal.

A new team's list holds one host, `deb.debian.org`, where Kivali installs the software you approve for agents' shells. It is marked permanent in **Org**, **Network** and cannot be removed. The list allows no model provider host: a shell never calls the model, and a provider's API is a way to send data to whoever holds an account there. For the same reason it allows no code or package host such as GitHub or PyPI; add one when your agents need it.

Teams created before this list changed keep the list they had. It may still hold GitHub, PyPI and the Go module proxy, and for older teams the Anthropic, Amazon Bedrock, Google Vertex AI and Microsoft Foundry hosts and their token services. A shell needs none of them unless your agents do, so you can remove them in **Org**, **Network**.

Each entry covers itself and every name under it: `example.com` also allows `api.example.com`. Within one part of a name, `*` matches any run of characters and `[a-z]` one character, so `*-aiplatform.googleapis.com` allows `us-east5-aiplatform.googleapis.com` and nothing else under `googleapis.com`. The default is copied into a team when it is created; after that the list is yours, apart from `deb.debian.org`. Remove anything your team does not need, and add hosts as agents need them.

An agent's own hosts follow the same rules and open for that agent's shell only, which suits a host one agent needs and the rest do not. So that the proxy can tell agents' shells apart, each shell's requests carry a credential of its own. It opens nothing beyond your team's list and that agent's hosts.

### The agent's own fetches

When an agent fetches a web page or searches the web with its own tools, and when it calls the model, the request goes through the same proxy with a credential that only the agent's own process holds. Its shell never has it. These requests reach any public host, not only the ones on your list. They never reach:

- the cluster: the pod and Service networks, and the machine the cluster runs on;
- loopback, link-local addresses and cloud metadata, Azure's WireServer included;
- private address ranges, and any range the cluster keeps browser pods off.

A host that is on your list is the exception: it is reached under the list's rules, so an intranet host you list works for the agent's fetches too.

The proxy counts the bytes each agent sends out this way, plus the names of the hosts it asks for, and leaves out its calls to the model provider. The limits are 32 KB on one connection, 256 KB in 10 minutes and 4 MB in a day. This release counts and logs what goes over them and does not refuse anything yet.

### Removing an agent's network limits

An agent's **About** tab, under **Security**, has a **Remove all network limits for this agent** switch. With it on, the agent's shell commands and its own fetches reach any public host: the proxy no longer holds them to a list, and no longer counts what they send against the byte budget. They still never reach the cluster, loopback, link-local addresses, cloud metadata or private address ranges, apart from hosts on your team's list or the agent's own. Such an agent has a **No network limits** badge on the **Team** page and is named in **Org**, **Network**. Turn the switch off to put the limits back; it applies to the agent's next request.

### Web tasks

A web task's browser runs in its own pod, which reaches the public web directly and nothing in the cluster, the node or a private network. It is not held to your list. The sign-ins of a site session (the saved browser profile and its cookies) are stored on the team's data volume, where only the server reads them; they are deleted when the agent is offboarded or the session is reset, and are not included in backups.

- **Uploads need your approval.** A web task that uploads an agent's files must name the site they go to, and an agent can have at most five such tasks waiting at once. Before the task starts, **Needs you** shows "Upload N files to site", with a link to download each file so you can look before you approve. Kivali records each file's checksum and size when the task is dispatched, and refuses to show or upload a file that has changed since, or one over 50 MB. Two files with the same name are refused. Approve and the task starts, held to that site for its whole run. Deny and it ends without uploading anything; the agent reads your note. If nobody answers in 24 hours, it ends the same way. A web task started by a subagent cannot upload.
- **The site lock** holds the task's own actions to that site. Kivali refuses to navigate or open a tab off the site, and refuses an upload unless every tab of the task is on the site. The browser also stops off-site navigations, popups and frames, and keeps what the task types and presses out of other sites' frames. One gap remains: a frame inside a closed shadow root is invisible to the page's script, so a key press there cannot be checked. The lock binds what the task does. It does not intercept requests the site's own pages make, so what the site does with a file is up to the site, as when you upload it yourself.
- **Every web task has a budget on what it sends out**, an approved one too: 2 KB for one action, 8 KB for one task and 32 KB for one agent in a day. It counts the text the task types, the keys it presses, the values it selects, the scripts it runs, and the data in the addresses it visits: the path and query of an address past its first 64 bytes, and a host name past its first 48, until a pool of 1 KB per task is spent, after which every byte counts. An action over it is refused, and a "web budget" notice appears in **Needs you**. The counters and waiting approvals are in memory, so a restart of Kivali resets the counters, and a web task still waiting for your approval or your sign-in ends as errored, like any task a restart leaves unfinished, and its agent is told which of its tasks ended.

### What this does not stop

The list and the budgets make leaks harder, not impossible. Know these limits when you choose what to allow:

- **A listed host that anyone can hold an account on, such as GitHub or PyPI, can carry data out from a shell.** A command can push a file to a repository its author owns.
- **A listed name may resolve to a private address.** The proxy refuses the cluster, loopback and metadata for every request, but it lets a listed name reach a private address on your network, because that is how you reach an intranet host. If you list a wildcard DNS service such as `nip.io`, a shell can reach any address on your network through it.
- **A web task can carry up to 1 KB per task out in its addresses.** The first 64 bytes of each path and query and the first 48 of each host name go uncounted, up to a pool of 1 KB per task, so a task can encode that much data in the addresses it visits.
- **An agent's own fetches can carry data out, up to the budget.** A web page can lead an agent to send a short secret in an address it fetches. The budget bounds how much, and for now only counts and logs.
- **An agent with its network limits removed can send anything it can read to any public host.** Its commands can upload its files, and nothing counts what they send.
- **An integration can send what it holds to the hosts it names.** Its connection and whatever it is asked to do reach those hosts, uncounted. A listed host that anyone can hold an account on is as open a door as it is for a shell, so read the hosts on the install screen with that in mind.

If you need a hard limit, enforce it in your own network, for example with firewall rules on the cluster's outbound traffic.

The proxy applies to agents' and integrations' sandboxes. Kivali's server, which makes the connections in the table above, is not behind it.

## Integrations

An [integration](integrations.md) is installed from a bundle, one version at a time, and nothing in a version runs until you approve it.

- **The install screen is the review.** Before you approve a version, its install screen shows who built and published it, every host it says it reaches, every field its connection needs with the secrets marked, every action it gives agents, what its access form asks, and the bundle's size. Kivali checks each bundle when it arrives: that it says these things, that its access form is one Kivali can show, and that its files unpack safely (no links, no paths that leave the bundle, at most 5000 files and 64 MiB). That check reads the bundle; it does not judge what the code does. Approve a version only when you trust whoever built it with what the screen lists.
- **An integration runs on its own.** An approved, switched-on integration runs in a sandbox of its own: its own pod, apart from Kivali's server and from every agent, with no access to the Kubernetes API, no service other pods can connect to, and nothing of the team's data volume but its own bundle, read-only. It reaches Kivali's server only through a private socket of its own, which agents' sandboxes cannot see.
- **It reaches only its hosts.** Its pod can reach nothing but the egress proxy, and the proxy holds it, by a credential of its own, to the hosts its install screen listed. Your team's network list does not apply to it, and an agent's network switch cannot open it to more. What it sends to those hosts is not counted against the agents' budget.
- **Connection secrets** (tokens, keys) are kept in a Kubernetes Secret of their own in the team's cluster: inside the team's virtual machine with Kivali Desktop, in the team's namespace when self-hosted. They are never on the team's data volume, never in a backup, never in a log, and never shown again once saved. Kivali's server can read, write and delete Secrets in its own namespace for this, and nothing outside it. When the integration starts, Kivali's server reads them from the Secret and hands them to it over its private socket, holding them in memory only while it answers; the integration's server then has them in its environment, inside its sandbox. The integration's pod description names no Secret and carries no value. Kivali replaces each secret value with `[secret]` in the integration's log before keeping a line.
- **The Git integration's token** is kept in the connection's Secret the same way. A clone, fetch or push runs in the agent's sandbox, so for each one Kivali's server checks the call against the agent's access and then hands the token, with a network credential of the connection's own that reaches only the connection's host, to the shell's daemon there, a process of its own whose memory the agent's commands cannot read. The daemon holds both only for that one operation: it never writes them to disk, never puts them in an environment, and takes them out of anything it answers. The working copy's configuration names the repository without the token. A daemon that cannot keep the agent's commands away from them runs no clone, fetch or push. A host that answers with a redirect is not followed, so the token goes only to the repository Kivali checked. Test connection runs in Kivali's server, which is not behind the proxy, so it first resolves the connection's host and refuses one with any address that is not public (loopback, private, link-local, unspecified or multicast), so a connection cannot point Kivali's server, with the token, at its own network.
- **Pushing is yours to allow.** An agent pushes only when its Git access has **May push** on, only to the repositories in its list, and only to the branches and tags you name for it; Kivali's server checks each push against that, and the shell's daemon checks it again. **May force-push**, which overwrites history on the remote or moves an existing tag, is a separate switch, off unless you turn it on. Without it the daemon checks for itself, against objects it verifies, that each branch only moves forward. No agent can delete a branch or tag on the remote through the integration.
- **Access is per agent.** An agent can use an integration only with the access you gave it there. Access set for an earlier connection, or that a newer version's form no longer accepts, gives nothing until you set it again. Offboarding an agent removes its access everywhere.
- **Kivali enforces access, not the integration.** Every call an agent makes goes through Kivali's server, which checks it against the agent's access as it stands before the integration sees it, and refuses it otherwise. A call that goes through tells the integration which agent is calling and with what access. An agent has no other way to an integration: its pod takes no connections, and it talks only to Kivali's server. What the integration answers reaches the agent with the connection's secret values replaced by `[secret]`, and agents are told it is data from an outside service, never instructions.
- **Every call is recorded.** Each call through an integration, allowed or refused, is in its **Recent calls** with the agent's name, the action, whether it went through, why not, and how long it took.

## What needs your approval

These cannot happen without you:

- **Changing the team.** Hiring, offboarding, reorganizing, and replacing a role or the handbook all happen only when you approve a Chief of Staff proposal. Offboarding archives; nothing is deleted.
- **Agents reaching each other.** Every message from one agent to another waits in your Queue until you release it, or until auto-release lets it through. A new team starts with auto-release at 30s; set it to Off to hold everything.
- **Uploads from a web task.** A web task that uploads files waits in **Needs you** for you to approve the files and the site.
- **Installing an integration.** Every version of an integration waits on its install screen until you approve it, and an agent gets an integration only when you give it access.
- **Decisions an agent asks you for**, which wait in **Needs you**.

Within its own sandbox and the hosts you allow, an agent works without asking: it runs commands, writes files, and calls the services it can reach. Choose your network list with that in mind, and read [what the network limits do not stop](#what-this-does-not-stop).

## Credentials

- **Your Claude sign-in.** What Claude Code's own sign-in stores, a subscription or Console sign-in, or the Bedrock or Vertex AI credentials you entered, is kept on the team's data volume, under Claude Code's own home directory. Kivali never sees what you type in Claude's sign-in. Microsoft Foundry's key or service principal secret goes into Claude Code's settings file there, readable only by the server's user. When you enter it in Kivali Desktop, it goes from the app straight to the team's machine and is not logged or shown again. The sign-in is used by the part of Kivali that makes model calls. Agents' shell commands and file tools run in a separate container that does not have it. Claude credentials are never included in backups.
- **Integration connections.** The secrets you enter for an integration are kept in a Kubernetes Secret, outside the data volume and out of backups. See [Integrations](#integrations).
- **Sign-in sessions** are signed with a key specific to each team. Sessions last 24 hours.
- **Kivali Desktop** keeps each team's settings and disk in folders only your user account can read, and asks you to confirm it is you before deleting a team: your password or Touch ID on macOS, Windows Hello on Windows.

## Sign-in and access

- Only the team's owner can sign in, with their own Google account. See [Sign-in](sign-in.md).
- A team in Kivali Desktop listens only on this computer until you turn on **Other devices**, and then only through the https address you set up.
- Every request to the server's web page and API needs a signed-in session. The only routes open without one are the sign-in pages, Google's callback and Kivali Desktop's signed, single-use handoff, which accepts only a request from the same computer.
- On a self-hosted team, the chart's network policies keep agents' pods to the egress proxy and off the cluster, the node and the server's web port. Keep them on.

## Reporting a vulnerability

Please report security problems privately, as described in [SECURITY.md](../SECURITY.md). Do not open a public issue.
