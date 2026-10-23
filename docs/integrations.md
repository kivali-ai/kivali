# Integrations

An integration is an external service your agents can act on, such as a CRM, a blog or a ticket tracker. Each one is installed from a bundle or ships with Kivali, is connected with a token you paste or by signing in to the service, and is given to agents one at a time, each with its own access. This page covers having one built, installing one and its new versions, rolling back to an earlier version, connecting and testing it, giving agents access, what an agent then sees, the record of their calls, its log, switching it off or removing it, and the [Git](#git) and [GitHub](#github) integrations that ship with Kivali.

Integrations are under **Org**, **Integrations**.

## What an integration is

An integration gives agents **actions**: things they can do in the service, such as "create a blog post" or "list contacts". Its **connection** is how it signs in to the service: a token or key you copy from the service's settings, and sometimes a field or two more, such as an account name. Its **access** is what one agent may do with it, chosen on a form the integration describes: which blogs, which projects, whether it may publish.

An integration is installed one **version** at a time. Nothing in a version runs until you approve it. Once approved and switched on, an integration you install runs on its own, apart from Kivali and from your agents, and reaches only the hosts its install screen listed.

Some integrations ship with Kivali. They are on the Integrations page already, marked **Built in**, and need no install: their version comes with Kivali, and Kivali's updates bring the next one, so there is nothing to approve, reject or roll back to. Some run inside Kivali, and their network reach is Kivali's own: they reach the hosts their connection names, and their install screen lists no hosts of their own. Others run on their own, as an installed integration does: they can show **Not running** for a moment while Kivali starts them, and they reach only the hosts their install screen lists. Connect one and give agents access as you would any other. A built-in integration cannot be removed; switch it off when your team should not use it, and it stays off. One that this version of Kivali does not include (another version shipped it) is marked **Not in this version of Kivali**: agents cannot use it, and you can remove it.

## Have an integration built

You do not need a bundle from anyone else: every team has a **Kivali Developer**, an agent that builds them. Kivali hires it with the team, under your Chief of Staff, and it stays: it cannot be let go or moved to another manager.

1. Ask the Kivali Developer for the integration, in its chat or through your Chief of Staff: the service, what your agents should be able to do in it, and what you want to limit agent by agent, such as which blogs an agent may write in or whether it may publish.
2. The Kivali Developer reads the service's documentation, writes the integration, and tests it against a stand-in for the service it writes itself, never against your account: it never sees your connection's secrets. Before it publishes, it checks the bundle by the rules an install checks it by, and starts it with no network to see that it answers.
3. When it publishes a version, a request arrives in **Needs you**: **Install** and the integration's name and version, with the Kivali Developer's notes on what it does, every host it reaches, each secret it asks for and anything it can change or delete. **Review** opens the version's [install screen](#install-an-integration), which shows the notes beside everything approving lets in. Approve or reject it there or in Needs you; the Kivali Developer hears your answer in its chat.
4. [Connect it](#connect-it), choose **Test connection**, and [give agents access](#give-agents-access). Tell the Kivali Developer what the test said. If it failed, the Kivali Developer reads the integration's [log](#its-log) and publishes a fixed version, which reaches you the same way.

The Kivali Developer uses an integration only if you give it access, like any agent. Its own commands reach the hosts your team allows and the ones you add on its **Security** view, so if it needs to download a package, it asks you for that host.

## Install an integration

An integration arrives as a bundle, a `.zip` file. In **Org**, **Integrations**, choose **Upload a bundle** and pick the file.

Kivali checks the bundle first: that it says what it is and what it reaches, that its form makes sense, and that its files are safe to unpack. A bundle that fails is not stored, and the dialog lists every problem for whoever built it. A version already installed cannot be uploaded again; a changed bundle needs a new version number.

A bundle that passes opens its **install screen**, which waits for your approval. It shows, in words:

- what the integration is, who built it, and who published this version;
- every host it reaches;
- for an integration you connect by signing in, under **Signs in at**, every host the sign-in talks to: your browser is sent to the service's sign-in page there, so check they are the service's own;
- the software it installs, if any, under **Software it installs**: Debian packages, fetched from Debian's repositories and installed each time the integration starts, before it runs. An integration uses them to run a tool such as GitHub's `gh` for its actions. Read each name before you approve;
- every field its connection needs, with the secrets marked;
- every action it gives agents;
- what its access form asks, or that access is all or nothing;
- the bundle's size, and anything the check noticed.

Read it before you approve. The check only reads the bundle; this screen is the review. Choose **Approve and install** to make this version the one in use, or **Reject** to turn it down. A rejected version never runs.

### New versions

A newer version is uploaded the same way, and waits on its own install screen, which says which version it replaces. Until you approve it, the version in use stays. The integration's page lists every version with where it stands: **Needs your approval**, **In use**, **Replaced** (in use before) or **Rejected**.

The install screen of a version that would replace the one in use says what is different from it, in words, under **What changes from version** and the number of the version in use: hosts it reaches that the version in use does not, or no longer reaches; software it installs that the version in use does not, or no longer installs; connection fields added or removed, with new secrets marked; actions added or removed; and fields added to or removed from the access form. When none of those differ, it says **No change to hosts, software, secrets, actions or the access form.** The rest of the screen still lists everything the version holds.

### Roll back to an earlier version

If a new version does not work as it should, you can go back to the one it replaced. On the integration's page, under **Versions**, a version marked **Replaced** has **Roll back** beside it; its install screen has the same button, and says what rolling back to it changes from the version in use. **Roll back** asks first, naming the version that stops and the one that runs in its place.

Rolling back stops the version in use and runs the earlier one again in a few seconds, from the code that was approved for it. The version you rolled back from is marked **Replaced**, and you can roll back to it in turn. A rejected version never runs, so it has no **Roll back**, and a version waiting for approval is approved, not rolled back to. The roll back is in [Recent calls](#recent-calls), under your name.

Your agents' access stays as it is. Access that no longer fits the earlier version's access form is marked, as after a new version (see [Give agents access](#give-agents-access)). Every version is kept until you remove the integration, so any version that was in use can be rolled back to.

## Connect it

On the integration's page, fill in the connection and choose **Save connection**. The form is the approved version's, so approve a version first.

Secret fields, such as tokens, are never shown again once saved: the page says only that they are set. To change one, type the new value; leave it empty to keep the one stored. They are kept outside your team's data volume, so [backups](backup-and-restore.md) never contain them. After a restore, an integration whose secrets are not there shows **Reconnect needed** until you enter them again.

Saving a connection restarts the integration with the new values, which takes a few seconds.

**Test connection** asks the integration to sign in to the service with the connection you saved, and shows what it found under the form: whether it worked, the integration's own words (such as the account it signed in as), and when you last tested. A failed test keeps the integration's reason, so you can see what to fix. Saving the connection again clears the last result. An integration that has no connection test, such as [Git](#git), has no **Test connection**.

**Disconnect** removes the connection and every agent's access, which was given for it.

### Connect by signing in

Some services hand out no token to paste: you connect them by signing in to the service and approving Kivali there. The integration's page says so and shows the service's own setup steps above the connection form. When the integration asks the service for particular scopes, its install screen and page list them ("Asks the service for: ..."); one whose app's own settings decide what it may do, such as a GitHub App, lists none.

1. Register an app with the service, as its setup steps say (on GitHub, a GitHub App under Settings, Developer settings, with device flow turned on). The service gives the app a client id and a client secret.
2. On the integration's page, paste the client id and the client secret into the connection form, with any other field it asks for, and choose **Save connection**. The client secret is kept like any other secret field and never shown again.
3. Under **Sign in**, choose **Sign in with a code**. The page shows a code, a **Copy code** button, the service's page to enter it on (it opens in a new tab), and how long the code works, at most ten minutes.
4. Enter the code on the service's page and approve the sign-in there. The integration's page notices within a few seconds and shows **Signed in**, with the account you signed in as when the integration has a connection test. The integration restarts with the sign-in, which takes a few seconds.

If you turn the sign-in down on the service's page, or the code runs out before you approve it, the page says so and offers **Try again**. A sign-in waits in Kivali's memory only: if Kivali restarts meanwhile, start it again. Saving the connection with a new client id or secret ends a sign-in that is waiting, since it was started with the old ones.

#### In your browser

Some services sign in in your browser instead of with a code (HubSpot, Slack, Notion), and some offer both. The page shows one button: **Sign in**, in the browser, or **Sign in with a code** where a sign-in in the browser cannot finish (below).

1. Open the integration's page at the address you will keep using for Kivali. Above the connection form, the page shows a **Redirect address**, with **Copy**: Kivali's address as your browser reaches it, followed by `/connect/callback`. It is the same for every integration.
2. Register an app with the service, as its setup steps say, and enter that redirect address exactly as the app's callback URL (some services call it the redirect URL). The service gives the app a client id and a client secret.
3. Paste the client id and the client secret into the connection form and choose **Save connection**.
4. Under **Sign in**, which names the host it sends you to ("Signs in at app.hubspot.com"), choose **Sign in**. The page takes you to the service, where you approve Kivali; the service sends you back to the integration's page, which says **Signed in**.

The sign-in finishes in the browser that started it, signed in to Kivali: if Kivali asks you to sign in on the way back, do, and the sign-in carries on. If you turn it down, take longer than ten minutes, or come back in another browser, the page says why and offers **Try again**. One browser sign-in at a time: starting another in the same browser, for any integration, ends the one before, and its page says so.

Services check the redirect address exactly, character for character, and set rules for it:

- Most require `https`. Most allow plain `http` only for `localhost` or `127.0.0.1`, at any port; some, such as Slack, allow no plain `http` at all.
- Some, such as Google, refuse a bare IP address other than loopback and names under private suffixes such as `.local` or `.internal`, even though only your browser has to reach the address.

A Kivali server you run yourself therefore needs a real host name with a certificate for browser sign-in. Tailscale is the easy way: its MagicDNS name for the server, under `ts.net`, is a real name with a certificate Tailscale issues, reachable only from your tailnet. [Self-hosting](self-hosting.md#sign-in-to-integrations-in-the-browser) says how.

**In Kivali Desktop**, **Sign in** opens your computer's default browser, not the team's window, which many services refuse to sign in inside. The browser arrives already signed in to the team, goes on to the service, and comes back to the integration's page there; the team's window says **Signed in** within a few seconds, and you can close the browser tab. While it waits, the team's window offers **Start again**, for a browser that did not open.

For a team on your computer, the redirect address is the team's own address there, `http://127.0.0.1:<port>/connect/callback`, with the port the page shows:

- GitHub accepts `http://127.0.0.1/connect/callback` registered once, whatever the port.
- Most other services need the exact address, port included: register the one the page shows. The team keeps its port.
- A service that allows no plain `http` address, such as Slack, cannot sign in to a team on your computer this way. Use **Sign in with a code** where the integration offers it, or give the team an `https` address under **Other devices** in the team's settings and sign in from a browser at that address.

Kivali offers a sign-in in the browser only where it can finish: at the team's address on your computer (`127.0.0.1`), and at the team's address for other computers (**Other devices**, or a server's own address) opened in a browser. Anywhere else the page offers **Sign in with a code** instead, when the integration has it:

- In Kivali Desktop's window for a team elsewhere, which cannot hand a sign-in to a browser signed in to that team.
- At another address for the team, such as a plain `http` one on your network, which the service's app does not know to send you back to.

When the integration signs in only in the browser, the page says it cannot sign in from there and gives the team's address: open the team in your browser (**Open in browser** in Kivali Desktop) and sign in from its page.

Kivali keeps the sign-in fresh: the integration never sees the client secret or what Kivali renews the sign-in with, only a token that works at the moment of each call, which Kivali renews shortly before it expires. When the service no longer accepts the sign-in (you revoked the app, or changed its secret), the connection reads **Sign in again** with the service's reason, the integration reads not connected, and **Needs you** on Home lists it until you sign in again from its page.

While a connection needs you to sign in, an agent's call through it is refused with words that say the connection needs you to sign in again and to ask you; nothing reaches the service.

**Sign out** ends the sign-in: Kivali forgets the tokens and asks the service to revoke them where the service allows it. The client id and secret stay, so you can sign in again at once. **Disconnect**, deleting the connection and removing the integration revoke the tokens the same way. Switching the integration off, signing out, disconnecting and removing it end a sign-in that is waiting for your approval.

### Several connections

Most integrations take one connection. Some that ship with Kivali, such as [Git](#git), take several: one for each account or host you connect, such as GitHub and your company's GitLab. Their page has **Connections** in place of **Connection**: one card per connection, folded to its label (what you call it, such as "Company GitLab") and a line saying how many agents use it and since when it is connected, with a **Reconnect needed** or **Could not verify** badge when it needs you. Open a card to see its own fields, **Save connection** and **Delete**, and **Test connection** when the integration has a connection test; the others stay folded. **Add a connection** opens a blank form with a **Label** field. Labels are unique within the integration, whatever their capitals.

Each agent's access then names the connection its calls use: **Give access** asks for a **Connection** first, and the access table, like the agent's **Integrations** tab ("Git (Company GitLab)"), says which one each agent uses. **Delete** asks first, naming the agents whose access uses that connection: their access goes with it, and the other connections and the access given for them stay.

### Not running

An integration that is approved and switched on shows **Not running** while it is starting, and when it has stopped. Starting takes a few seconds, or longer for one that installs software: it installs it each time it starts, before it runs, and **Recent logs** says **Installing** and the packages' names, then **Installed**, or which could not be installed and why. While it is not running, **Test connection** and the options in an access form are not available.

If it stays **Not running**, open **Recent logs** on its page: the integration's own messages and Kivali's about it say what happened, such as a server that did not start or stopped answering. Kivali starts a stopped integration again by itself, and restarts one that stops answering. If the log shows the integration failing each time it starts, the version itself is at fault: ask whoever built the bundle for a fixed version, or switch it off. An integration whose software cannot be installed stops, tries again each time Kivali starts it, and says so in its log each time: a package name Debian does not have needs a fixed version, while a failure to reach Debian's repositories may pass on its own. On a team short of memory, the log says the team does not have enough memory for it; raise the team's memory in Kivali Desktop's settings, or ask whoever runs your server.

## Its log

**Recent logs** on an integration's page starts closed; select the arrow beside its heading to open it. It lists the latest lines the integration wrote, oldest first: its own messages, the errors it printed, and Kivali's notes about it, such as when it started, stopped or was restarted. **Refresh** reads it again. Secret values of the connection never appear in it: Kivali replaces each one with `[secret]` before the line is kept. The log keeps the last few thousand lines and goes when the integration is removed.

## Give agents access

Under **Access**, choose an agent and **Give access**. The form is the integration's own: a list of blogs or projects to pick from, switches such as "May publish and edit", text fields. A list the integration fills in from the service, such as the blogs your connection can see, is loaded through the running integration with **Load options**, which waits while the integration is not running. An integration with no form gives an agent all of it or nothing. **Change** edits an agent's access and **Remove** takes it away.

Each agent's access is shown in words, on the integration's page and on the agent's **Integrations** tab. When the version in use changes, by an approval or a roll back, so that an agent's access no longer fits the form, the access is marked invalid, with the reason, and gives nothing until you set it again with **Change**. The top of the integration's page says how many access rows no longer fit, such as "1 access row no longer fits this version's access form. Set it again.", until every row fits.

An agent that is offboarded loses its access to every integration.

## What an agent sees

An agent with access to a running integration has its actions as tools, named after the integration and the action: `hubspot_create_post` for HubSpot's "create post". It has only the actions its access lets the integration offer it, among those the install screen listed, and is told that you decide what it may do with each integration. An agent without access, or with access that no longer applies, has none of them.

Kivali checks every call against the agent's access before the integration sees it, and tells the integration which agent is calling and with what access. A call Kivali refuses (no access, the integration switched off or not running, an action the agent's access does not include) tells the agent why and to ask you. What comes back is the integration's text, with the connection's secrets replaced by `[secret]`; images and other content that is not text are not passed on in this release.

A change to an agent's access, given or taken away, changes its tools from its next turn. Access you take away is refused from the next call, even in the middle of a turn.

## Recent calls

**Recent calls** on an integration's page starts closed; select the arrow beside its heading to open it. It lists the latest calls through it, newest first: when, which agent, the action, whether Kivali let it through and, for a call that was refused or failed, why, and how long it took. Your own Test connection, the options an access form loads and your roll backs are in it too, under your name; a roll back reads **Roll back**, with the versions it went from and to. Calls an agent made are listed whether Kivali allowed them or not.

## Switch it off or remove it

The switch at the top of the integration's page turns it off for every agent at once and stops it running, keeping the connection and everyone's access for when you turn it back on.

**Remove** deletes the integration: every version, the connection and its secrets, every agent's access and its log, and stops it running. Uploading a bundle installs it again from scratch. An integration that ships with Kivali cannot be removed; switch it off instead.

## Git

**Git** ships with Kivali. It lets agents clone, fetch and push your repositories on a git host over HTTPS with a token you give it. Everything else an agent does with a repository (status, branches, commits, diffs, merges) is plain git in its own shell.

### The connection

- **Host**: `github.com`, `gitlab.com` or your own git host that serves HTTPS, without `https://`. A host that is not a host name (with a path, for example) is refused when you save.
- **Access token**: a token that can read the repositories your agents will use. On GitHub, a fine-grained personal access token limited to those repositories, with read access to their contents; on GitLab, an access token with `read_repository`. For agents that push, the token also needs write access to contents on GitHub, or `write_repository` on GitLab. Give it no more than that.

Git has no **Test connection**. An agent's first clone shows whether the token works: every clone, fetch and push is in **Recent calls**, with why it failed when it did.

Git takes [several connections](#several-connections), one per host: connect GitHub and your company's GitLab side by side, each with its own token. A connection you leave without a label is named after its host, such as `github.com`. Each agent's access names one connection, and its clones, fetches and pushes go to that connection's host with that connection's token. An agent has one access to Git, so it works on one host at a time: the one its access names.

The token is kept in the connection's Secret, like every connection secret. Each connection also has a network credential of its own, kept beside the token, that lets an agent's clone, fetch or push reach the connection's host and nothing else. Your team's allow list does not change.

### Access

An agent's access has four parts. You choose, repository by repository, whether the agent may only pull it or may also push to it:

- **Repositories it may pull**: the agent can clone and fetch these, but not push to them.
- **Repositories it may push to**: the agent can clone, fetch and push to these. A repository needs to be in only one of the two lists.
- **Branches and tags it may push**: the only branches and tags the agent can write, in the repositories it may push to. A name is a branch: `agent/fix-1` is the branch `agent/fix-1`, and `v1.2.0` is a branch of that name too. A tag starts with `tags/`: `tags/v1.2.0`. A pattern uses `*`, the only wildcard: `agent/*` is every branch directly under `agent/`, but not `agent/a/b` (write `agent/*/*` for that level), `tags/v*` is every tag starting with `v`, and `*` alone is every branch and every tag at the top level. Capitals count, as they do in git. Empty, the agent can push nothing. Kivali refuses an entry it cannot read when you save.
- **May force-push**: lets a push overwrite history on the remote, replacing commits other people may have built on, or move an existing tag. It is marked as a risky choice in the form and is off unless you turn it on. Leave it off unless the work needs it, and when it does, limit the branches above to ones only that agent uses, such as `agent/*`.

Each repository in either list is `owner/name`, such as `acme/widgets`, or a pattern such as `acme/*`, which covers every repository directly under `acme` but not `acme/tools/cli`. `*` is the only wildcard. GitLab nests groups, so write `group/subgroup/*` for those. Capitals do not matter. Name at least one repository; Kivali refuses an entry it cannot read when you save. A repository that has been renamed or moved is not followed to its new name: a clone, fetch or push of it fails, and the agent is told to ask you to update its access to the new name.

For example, to let an agent read every repository under `acme` but push only to `acme/widgets`, put `acme/*` under **Repositories it may pull** and `acme/widgets` under **Repositories it may push to**.

The page sums it up in one line, such as "Pull: acme/* · Push: acme/widgets · Branches and tags: agent/* · May force-push".

### What agents can do

An agent with access has three actions:

- **Clone** copies a repository its access includes into a new or empty directory of its files or its home.
- **Fetch** brings new commits and tags into one of its working copies, from the repository's address or the one the working copy came from, if its access includes it.
- **Push** sends branches and tags from one of its working copies to the repository, when the repository is one it may push to and its access allows those branches and tags. It can push a branch to the same name or to another (its current commit to `agent/fix-1`, say) and push tags. A push that would overwrite history is refused unless the agent asks to force and you allowed force-pushing, and an existing tag is never moved without force. A push of several branches and tags lands whole or not at all: if the host refuses one, it takes none of them, so the host must accept atomic pushes (GitHub, GitLab and plain git servers do). The host's own rules (protected branches, required reviews) still apply.

All three go to the connection's host with your token. The working copy is an ordinary one, whose origin is the repository's address without the token, and the agent works in it with plain git in its shell. That plain git reaches only the hosts on your team's allow list, and without your token, so a repository host that is not on the list is reachable only through these three actions.

Every clone, fetch and push is in **Recent calls**, with the repository and the branches and tags that moved (a push also says when it forced), or why it failed. Kivali checks each one against the agent's access before anything runs. Each has two rows: one when it starts and one when it ends. If you change an agent's access while its push runs, the second row still records what the push wrote, and says the access changed during it. If you move an agent's access to another connection while its clone, fetch or push runs, the second row records what ran, says the access changed, and names the connection it ran through.

### Not supported

- SSH addresses (`git@github.com:...`): give the HTTPS address or `owner/name`.
- Git LFS: a clone holds the LFS pointers, not the files.
- Submodules are not cloned.
- Deleting a branch or tag on the remote: a push only adds and moves them.
- Very large repositories are slow, and a clone stops after 15 minutes, a fetch or a push after 10.

## GitHub

**GitHub** ships with Kivali. It lets agents read and open issues, comments and pull requests in your GitHub repositories, as the GitHub account you sign in with. It runs GitHub's own `gh` tool for each action, in a place of its own apart from Kivali and your agents, and reaches only `api.github.com` and `github.com`. Its install screen lists `gh` under **Software it installs**.

### The connection

GitHub is connected by [signing in](#connect-by-signing-in) with a GitHub App you register yourself, so Kivali holds no password or personal access token of yours and you choose what the app may touch. Register it on the account that owns the repositories agents should reach: the app can reach only that account's repositories. The integration's page shows the steps above the connection form; in short:

1. Open GitHub's form for a new GitHub App from the page: **Register the app on your personal account**, or, for an organization, copy the address the page gives, put the organization's name in place of `ORGANIZATION` and open it. The form opens filled in: the team's callback URLs, the webhook off, installation only on that account, and the repository permissions Contents and Metadata read-only, Issues and Pull requests read and write.
2. Give the app a name, tick **Enable Device Flow**, and choose **Create GitHub App**.
3. Paste its client id and a client secret you generate into the connection form, and choose **Save connection**.
4. Install the app on the account, for the repositories agents should reach. Agents reach only repositories the app is installed on.
5. Choose **Sign in** and approve the sign-in on GitHub. In Kivali Desktop's window for a team on another computer, the page offers **Sign in with a code** instead, which is what **Enable Device Flow** is for: enter the code on GitHub's page.

The callback URLs are the addresses GitHub may send your browser back to after a sign-in: the one you opened the page at, `http://127.0.0.1/connect/callback` (any port, for a team in Kivali Desktop), and the team's address for other computers when it has one. If you give the team an address for other computers later (**Other devices** in Kivali Desktop), or change it, add that address followed by `/connect/callback` to the app's callback URLs on GitHub, or a sign-in from there ends on GitHub's error page.

Leave **Expire user authorization tokens** on in the app's settings: GitHub's tokens then last eight hours, and Kivali renews each one shortly before it expires, so a token that leaks stops working soon. **Test connection** names the GitHub account the connection signs in as. What agents can do is the smaller of what the app may do and what your GitHub account may do.

### Access

An agent's access has two parts:

- **Repositories**: each `owner/name`, such as `acme/widgets`, or a pattern such as `acme/*`, which covers every repository directly under `acme`. `*` is the only wildcard, and capitals do not matter, as for [Git](#access). Name at least one.
- **May create and comment**: lets the agent open issues and pull requests and comment on them in those repositories. Off, the agent only reads, and its tools do not include those actions.

Everything an agent does is done as the account you signed in with, so GitHub shows your name on its issues, comments and pull requests.

### What agents can do

- List the repositories its access names, and read a repository's details, its branches, and a comparison of two branches, tags or commits (the commits and the files changed, not the changes themselves).
- List and read issues, with the latest 20 comments, and list and read pull requests, with the diff when it asks (cut at 50,000 characters). Issue search words stay within the repository named: a search that names another repository, organization or user is refused.
- With **May create and comment**: open an issue, comment on an issue or pull request, and open a pull request from a branch already on GitHub.

Each action is in **Recent calls**. A call for a repository the agent's access does not name is refused before anything reaches GitHub, and the agent is told to ask you.

GitHub stays apart from [Git](#git): it moves no commits. To open a pull request from an agent's work, the agent first pushes its branch with the Git integration, through Git's own connection and access, then opens the pull request here.

### Not supported

- GitHub Enterprise Server and other hosts than `github.com`.
- Cloning, pushing or any other move of commits: that is Git's.
- Anything `gh` cannot do with a user's token from a GitHub App, such as changing the repository's settings, merging pull requests or approving reviews.
