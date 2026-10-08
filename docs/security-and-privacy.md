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
| **Hosts you allow** | Whatever agents send from their sandboxes, to hosts on your network list. | Agents' own work. See [Network access](#network-access). |

Kivali's own housekeeping (summaries of project files and past chats) is model calls too, billed to the same Claude sign-in.

## How agents are contained

Each agent runs in its own sandbox: its own pod with its own storage, separate from Kivali's server and from every other agent.

- **Its shell and file tools** run in a dedicated container. They see the agent's workspace, the project files, skills, its own past chats, and everything the team has published, and nothing of the server or of other agents' workspaces. Project files, skills and published files are read-only there; only Kivali itself publishes an agent's files, when the agent asks.
- **No way in.** An agent's pod runs no service other pods or computers can connect to, and has no access to the Kubernetes API. It reaches Kivali's server only through a private socket, and every action it takes goes through the server's checks.
- **Network.** An agent's pod can reach only the egress proxy and the cluster's DNS. The server's web port refuses connections from any pod.
- **Subagents** run inside their agent's sandbox, under the same limits.

## Network access

Every outbound connection from an agent's sandbox goes through a proxy that allows only the hosts on your team's list. Edit it in **Org**, **Network**; changes apply at once.

A new team allows:

- `api.anthropic.com` and `platform.claude.com`, which a Claude subscription or Console sign-in needs;
- the Amazon Bedrock and Google Vertex AI endpoints in every region, and the AWS and Google token services their sign-ins refresh through (`sts.amazonaws.com`, the IAM Identity Center hosts, `oauth2.googleapis.com`, `sts.googleapis.com`, `iamcredentials.googleapis.com`, `www.googleapis.com`);
- every Microsoft Foundry resource (`*.services.ai.azure.com`) and Microsoft Entra ID's token service (`login.microsoftonline.com`), which a service principal signs in through;
- Debian's package servers, PyPI, GitHub (including `api.github.com` and `*.githubusercontent.com`), and the Go module proxy, which agents' tools commonly need.

Each entry covers itself and every name under it: `example.com` also allows `api.example.com`. Within one part of a name, `*` matches any run of characters and `[a-z]` one character, so `*-aiplatform.googleapis.com` allows `us-east5-aiplatform.googleapis.com` and `bedrock-runtime.[a-z][a-z]-*-[0-9].amazonaws.com` allows every region's Bedrock endpoint, and nothing else under `amazonaws.com`. The defaults are copied into a team when it is created; after that the list is yours. Remove anything your team does not need, and add hosts as agents need them. A request to any other host is refused, and the agent sees the refusal.

The proxy applies to agents' sandboxes. Kivali's server, which makes the connections in the table above, is not behind it.

## What needs your approval

These cannot happen without you:

- **Changing the team.** Hiring, offboarding, reorganizing, and replacing a role or the handbook all happen only when you approve a Chief of Staff proposal. Offboarding archives; nothing is deleted.
- **Agents reaching each other.** Every message from one agent to another waits in your Queue until you release it, unless you turn on auto-release.
- **Decisions an agent asks you for**, which wait in **Needs you**.

Within its own sandbox and the hosts you allow, an agent works without asking: it runs commands, writes files, and calls the services it can reach. Choose your network list with that in mind.

## Credentials

- **Your Claude sign-in.** What Claude Code's own sign-in stores, a subscription or Console sign-in, or the Bedrock or Vertex AI credentials you entered, is kept on the team's data volume, under Claude Code's own home directory. Kivali never sees what you type in Claude's sign-in. Microsoft Foundry's key or service principal secret goes into Claude Code's settings file there, readable only by the server's user. When you enter it in Kivali Desktop, it goes from the app straight to the team's machine and is not logged or shown again. The sign-in is used by the part of Kivali that makes model calls. Agents' shell commands and file tools run in a separate container that does not have it. Claude credentials are never included in backups.
- **Sign-in sessions** are signed with a key specific to each team. Sessions last 24 hours.
- **Kivali Desktop** keeps each team's settings and disk in folders only your user account can read, and asks you to confirm it is you before deleting a team: your password or Touch ID on macOS, Windows Hello on Windows.

## Sign-in and access

- Only the team's owner can sign in, with their own Google account. See [Sign-in](sign-in.md).
- A team in Kivali Desktop listens only on this computer until you turn on **Other devices**, and then only through the https address you set up.
- On a self-hosted team, the chart's network policies keep agents' pods from calling the server's web port. Keep them on.

## Reporting a vulnerability

Please report security problems privately, as described in [SECURITY.md](../SECURITY.md). Do not open a public issue.
