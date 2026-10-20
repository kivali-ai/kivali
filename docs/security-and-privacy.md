# Security and privacy

This page explains where your team's data lives, what leaves your machine and where it goes, how agents are contained, which actions need your approval, where credentials are kept, and how to report a vulnerability.

## Where your data lives

All of a team's data is plain files on the team's own disk. There is no Kivali cloud service holding your team.

- **Kivali Desktop**: each team has its own disk inside its own virtual machine, stored under `~/Library/Application Support/Kivali` and readable only by your macOS user.
- **Self-hosted**: the team's data volume (`kivali-data`) on your cluster.

That data includes everything your agents know and everything you gave them: chats, memory, project files, published files, assignments and messages. [Backups](backup-and-restore.md) contain the same and are not encrypted, so store them with the same care.

## What leaves your machine

| Where to | What | Why |
| --- | --- | --- |
| **Anthropic, or Amazon Bedrock, Google Vertex AI or Microsoft Foundry** | Every model call: the handbook, the agent's role, habits and memory, its chat, and whatever files and tool results the agent reads in that turn. It goes to whichever service your Claude sign-in uses. | This is how agents think. The terms of your Claude subscription, Anthropic Console account, or cloud provider govern this data. |
| **Google** | Your sign-in. Kivali reads only the email address Google confirms. | Signing in. |
| **The sign-in relay at kivali.ai** | The one-time code from a Google sign-in, when you use Kivali's built-in sign-in client. It returns Google's signed identity token, which your server verifies against Google's keys. Not used when you [use your own OAuth client](sign-in.md#use-your-own-google-oauth-client). | Exchanging the sign-in code needs the client secret, which only the relay holds. |
| **GitHub** | Update checks, from Kivali Desktop only: a check for a new app at launch, and a check for a new team version once a day while each team runs. Updates download from the project's GitHub releases. A self-hosted server makes no update checks. | Updates. |
| **Time servers** | A request to `www.google.com`, `www.cloudflare.com` or `gsa.apple.com`, to compare the server's clock with the outside world. | Message records are filed by date, so a wrong clock matters. |
| **Hosts on your network list** | Whatever an agent's shell commands send from its sandbox, such as a download or a package install. | Agents' own work. See [Network access](#network-access). |
| **Public websites** | What an agent's own web fetches and searches send, and what a web task's browser loads and types. | Agents reading the web. Limited by a budget on what they send out. See [Network access](#network-access). |

Kivali's own housekeeping (summaries of project files and past chats) is model calls too, billed to the same Claude sign-in.

## How agents are contained

Each agent runs in its own sandbox: its own pod with its own storage, separate from Kivali's server and from every other agent.

- **Its shell and file tools** run in a dedicated container. They see the agent's workspace, the project files, skills, its own past chats, and everything the team has published, and nothing of the server or of other agents' workspaces. Project files, skills and published files are read-only there; only Kivali itself publishes an agent's files, when the agent asks.
- **No way in.** An agent's pod runs no service other pods or computers can connect to, and has no access to the Kubernetes API. It reaches Kivali's server only through a private socket, and every action it takes goes through the server's checks.
- **Network.** An agent's pod can reach only the egress proxy, by its address. It has no DNS and no route to anything else, so a command that ignores the proxy goes nowhere. The server's web port refuses connections from any pod, and every request to it needs a signed-in session in any case.
- **Subagents** run inside their agent's sandbox, under the same limits. A web subagent's browser runs in a pod of its own, described under Network access.

## Network access

Everything an agent sends out of its sandbox goes through a proxy. The proxy treats two kinds of traffic differently.

### The agent's shell

Commands the agent runs, such as `curl`, `pip`, `git` and `apt` and anything else typed into its shell, reach only the hosts on your team's list. Edit it in **Org**, **Network**; changes apply at once. A request to any other host is refused, and the agent sees the refusal.

A new team's list allows Debian's package servers, PyPI, GitHub (including `api.github.com` and `*.githubusercontent.com`) and the Go module proxy, which agents' tools commonly need. It allows no model provider host: a shell never calls the model, and a provider's API is a way to send data to whoever holds an account there.

Teams created before this list changed keep the list they had. It still holds the Anthropic, Amazon Bedrock, Google Vertex AI and Microsoft Foundry hosts and their token services. A shell does not need them, so you can remove them in **Org**, **Network**.

Each entry covers itself and every name under it: `example.com` also allows `api.example.com`. Within one part of a name, `*` matches any run of characters and `[a-z]` one character, so `*-aiplatform.googleapis.com` allows `us-east5-aiplatform.googleapis.com` and nothing else under `googleapis.com`. The defaults are copied into a team when it is created; after that the list is yours. Remove anything your team does not need, and add hosts as agents need them.

### The agent's own fetches

When an agent fetches a web page or searches the web with its own tools, and when it calls the model, the request goes through the same proxy with a credential that only the agent's own process holds. Its shell never has it. These requests reach any public host, not only the ones on your list. They never reach:

- the cluster: the pod and Service networks, and the machine the cluster runs on;
- loopback, link-local addresses and cloud metadata, Azure's WireServer included;
- private address ranges, and any range the cluster keeps browser pods off.

A host that is on your list is the exception: it is reached under the list's rules, so an intranet host you list works for the agent's fetches too.

The proxy counts the bytes each agent sends out this way, plus the names of the hosts it asks for, and leaves out its calls to the model provider. The limits are 32 KB on one connection, 256 KB in 10 minutes and 4 MB in a day. This release counts and logs what goes over them and does not refuse anything yet.

### Web tasks

A web task's browser runs in its own pod, which reaches the public web directly and nothing in the cluster, the node or a private network. It is not held to your list. The sign-ins of a site session (the saved browser profile and its cookies) are stored on the team's data volume, where only the server reads them; they are deleted when the agent is offboarded or the session is reset, and are not included in backups.

- **Uploads need your approval.** A web task that uploads an agent's files must name the site they go to, and an agent can have at most five such tasks waiting at once. Before the task starts, **Needs you** shows "Upload N files to site", with a link to download each file so you can look before you approve. Kivali records each file's checksum and size when the task is dispatched, and refuses to show or upload a file that has changed since, or one over 50 MB. Two files with the same name are refused. Approve and the task starts, held to that site for its whole run. Deny and it ends without uploading anything; the agent reads your note. If nobody answers in 24 hours, it ends the same way. A web task started by a subagent cannot upload.
- **The site lock** holds the task's own actions to that site. Kivali refuses to navigate or open a tab off the site, and refuses an upload unless every tab of the task is on the site. The browser also stops off-site navigations, popups and frames, and keeps what the task types and presses out of other sites' frames. One gap remains: a frame inside a closed shadow root is invisible to the page's script, so a key press there cannot be checked. The lock binds what the task does. It does not intercept requests the site's own pages make, so what the site does with a file is up to the site, as when you upload it yourself.
- **Every web task has a budget on what it sends out**, an approved one too: 2 KB for one action, 8 KB for one task and 32 KB for one agent in a day. It counts the text the task types, the keys it presses, the values it selects, the scripts it runs, and the data in the addresses it visits: the path and query of an address past its first 64 bytes, and a host name past its first 48, until a pool of 1 KB per task is spent, after which every byte counts. An action over it is refused, and a "web budget" notice appears in **Needs you**. The counters and waiting approvals are in memory, so a restart of Kivali resets the counters, and a web task still waiting for your approval or your sign-in ends as errored, like any task a restart leaves unfinished, and its agent is told which of its tasks ended.

### What this does not stop

The list and the budgets make leaks harder, not impossible. Know these limits when you choose what to allow:

- **Listed hosts that anyone can hold an account on, such as GitHub and PyPI, can still carry data out from a shell.** A command can push a file to a repository its author owns.
- **A listed name may resolve to a private address.** The proxy refuses the cluster, loopback and metadata for every request, but it lets a listed name reach a private address on your network, because that is how you reach an intranet host. If you list a wildcard DNS service such as `nip.io`, a shell can reach any address on your network through it.
- **A web task can carry up to 1 KB per task out in its addresses.** The first 64 bytes of each path and query and the first 48 of each host name go uncounted, up to a pool of 1 KB per task, so a task can encode that much data in the addresses it visits.
- **An agent's own fetches can carry data out, up to the budget.** A web page can lead an agent to send a short secret in an address it fetches. The budget bounds how much, and for now only counts and logs.

If you need a hard limit, enforce it in your own network, for example with firewall rules on the cluster's outbound traffic.

The proxy applies to agents' sandboxes. Kivali's server, which makes the connections in the table above, is not behind it.

## What needs your approval

These cannot happen without you:

- **Changing the team.** Hiring, offboarding, reorganizing, and replacing a role or the handbook all happen only when you approve a Chief of Staff proposal. Offboarding archives; nothing is deleted.
- **Agents reaching each other.** Every message from one agent to another waits in your Queue until you release it, or until auto-release lets it through. A new team starts with auto-release at 30s; set it to Off to hold everything.
- **Uploads from a web task.** A web task that uploads files waits in **Needs you** for you to approve the files and the site.
- **Decisions an agent asks you for**, which wait in **Needs you**.

Within its own sandbox and the hosts you allow, an agent works without asking: it runs commands, writes files, and calls the services it can reach. Choose your network list with that in mind, and read [what the network limits do not stop](#what-this-does-not-stop).

## Credentials

- **Your Claude sign-in.** What Claude Code's own sign-in stores, a subscription or Console sign-in, or the Bedrock or Vertex AI credentials you entered, is kept on the team's data volume, under Claude Code's own home directory. Kivali never sees what you type in Claude's sign-in. Microsoft Foundry's key or service principal secret goes into Claude Code's settings file there, readable only by the server's user. When you enter it in Kivali Desktop, it goes from the app straight to the team's machine and is not logged or shown again. The sign-in is used by the part of Kivali that makes model calls. Agents' shell commands and file tools run in a separate container that does not have it. Claude credentials are never included in backups.
- **Sign-in sessions** are signed with a key specific to each team. Sessions last 24 hours.
- **Kivali Desktop** keeps each team's settings and disk in folders only your user account can read, and asks you to confirm it is you before deleting a team: your password or Touch ID on macOS, Windows Hello on Windows.

## Sign-in and access

- Only the team's owner can sign in, with their own Google account. See [Sign-in](sign-in.md).
- A team in Kivali Desktop listens only on this computer until you turn on **Other devices**, and then only through the https address you set up.
- Every request to the server's web page and API needs a signed-in session. The only routes open without one are the sign-in pages, Google's callback and Kivali Desktop's signed, single-use handoff, which accepts only a request from the same computer.
- On a self-hosted team, the chart's network policies keep agents' pods to the egress proxy and off the cluster, the node and the server's web port. Keep them on.

## Reporting a vulnerability

Please report security problems privately, as described in [SECURITY.md](../SECURITY.md). Do not open a public issue.
