# Integrations

An integration is an external service your agents can act on, such as a CRM, a blog or a ticket tracker. Each one comes from a bundle you approve or ships with Kivali, is connected with a token you paste, by signing in to the service or by its own setup, and is given to agents one at a time, each with its own access. This page covers having one built, approving one and its new versions, rolling back to an earlier version, connecting it, what it needs from you once connected, giving agents access, what an agent then sees, the record of their calls, its log, disabling, disconnecting or uninstalling it, and the [Git (Basic)](#git-basic), [GitHub](#github) and [Google Workspace](#google-workspace) integrations that ship with Kivali.

Integrations are under **Org**, **Integrations**, in two groups: **From a bundle**, the integrations you uploaded or your Kivali Developer published, with **Upload a bundle** under them; and **Built in**, the ones that ship with Kivali.

## What an integration is

An integration gives agents **actions**: things they can do in the service, such as "create a blog post" or "list contacts". Its **connection** is how it signs in to the service: a token or key you copy from the service's settings, and sometimes a field or two more, such as an account name. Its **access** is what one agent may do with it, chosen on a form the integration describes: which blogs, which projects, whether it may publish.

An integration is installed one **version** at a time. Nothing in a version runs until you approve it. Once approved and enabled, an integration you upload runs on its own, apart from Kivali and from your agents, and reaches only the hosts its install screen listed.

Some integrations ship with Kivali. They are on the Integrations page already, under **Built in**, and need no approval: their version comes with Kivali, and Kivali's updates bring the next one, so there is nothing to approve, reject or roll back to. They run inside Kivali: one connected with a token reaches the hosts its connection names, and its install screen lists no hosts of its own; one whose own setup makes its connection, such as [GitHub](#github), lists every host its setup and its calls reach. Connect one and give agents access as you would any other. An integration that ships with Kivali is never uninstalled: **Disconnect** forgets its connection and leaves it **Installed**, ready to connect again; disable it when your team should not use it for a while, and it stays disabled. One that this version of Kivali does not include (another version shipped it) is marked **Not in this version of Kivali**: agents cannot use it, and you can uninstall it.

### Where an integration stands

Every integration's page and its row on the Integrations page show where it stands:

- **Waiting for approval**: no version is approved yet. Approve one from its install screen; approving installs it.
- **Installed**: on the team, with no connection. **Connect** on its page runs its setup. A setup that fails or that you cancel keeps nothing, and the integration stays installed.
- **Connected**: it has its connection and is enabled, so agents can be given access. Its **Requirements** say whether it works, and the list shows how many need you ("1 needs you").
- **Disabled**: its connection and every agent's access are kept, but agents cannot use it, until you enable it again.
- **Not in this version of Kivali**: see above.

Installed and disabled are alike for your agents: neither can be used. The difference is what Kivali holds: a disabled integration keeps its connection and everyone's access for when you enable it; an installed one holds nothing of the service's. **Disconnect** forgets the connection and every agent's access, back to **Installed**; **Uninstall** takes an integration you uploaded off the team altogether ([Disable, disconnect or uninstall it](#disable-disconnect-or-uninstall-it)).

## Have an integration built

You do not need a bundle from anyone else: every team has a **Kivali Developer**, an agent that builds them. Kivali hires it with the team, under your Chief of Staff, and it stays: it cannot be let go or moved to another manager.

1. Ask the Kivali Developer for the integration, in its chat or through your Chief of Staff: the service, what your agents should be able to do in it, and what you want to limit agent by agent, such as which blogs an agent may write in or whether it may publish. You do not need to know how the service is reached: the Kivali Developer makes the technical choices, and what it asks you is about what the agents should do, not how.
2. The Kivali Developer reads the service's documentation, chooses how the integration connects, writes it, and tests it against a stand-in for the service it writes itself, never against your account: it never sees your connection's secrets. Before it publishes, it checks the bundle by the rules an install checks it by, and starts it with no network to see that it answers.
3. When it publishes a version, a request arrives in **Needs you**: **Install** and the integration's name and version, with the Kivali Developer's notes on what it does, every host it reaches, each secret it asks for and anything it can change or delete. **Review** opens the version's [install screen](#install-an-integration), which shows the notes beside everything approving lets in. Approve or reject it there or in Needs you; the Kivali Developer hears your answer in its chat.
4. [Connect it](#connect-it), choose **Test connection**, and [give agents access](#give-agents-access). Tell the Kivali Developer what the test said. If it failed, the Kivali Developer reads the integration's [log](#its-log) and publishes a fixed version, which reaches you the same way.

The Kivali Developer uses an integration only if you give it access, like any agent. Its own commands reach the hosts your team allows and the ones you add on its **Security** view, so if it needs to download a package, it asks you for that host.

## Install an integration

An integration arrives as a bundle, a `.zip` file. In **Org**, **Integrations**, under **From a bundle**, choose **Upload a bundle** and pick the file.

Kivali checks the bundle first: that it says what it is and what it reaches, that its form makes sense, and that its files are safe to unpack. A bundle that fails is not stored, and the dialog lists every problem for whoever built it. A version already installed cannot be uploaded again; a changed bundle needs a new version number.

A bundle that passes opens its **install screen**, which waits for your approval. It shows, in words:

- what the integration is, who built it, and who published this version;
- every host it reaches;
- for an integration you connect by signing in, under **Signs in at**, every host the sign-in talks to: your browser is sent to the service's sign-in page there, so check they are the service's own;
- the software it installs, if any, under **Software it installs**: Debian packages, fetched from Debian's repositories and installed each time the integration starts, before it runs. An integration uses them to run a tool for its actions. Read each name before you approve;
- every field its connection needs, with the secrets marked; or, for an integration whose connection its own setup makes, under **Setup**: what setup does, every host it may send your browser to, every field it may ask you for (secrets, and values used once and never kept, marked) and every value it keeps (secrets marked; a value the service shows publicly, such as an app's name, is not secret, and as the screen says under the list, values not marked Secret may be shown to you and to agents). Kivali holds setup to that list, so read it as you would the hosts;
- every action it gives agents;
- what its access form asks, or that access is all or nothing;
- the bundle's size, and anything the check noticed.

Read it before you approve. The check only reads the bundle; this screen is the review. Choose **Approve and install** to make this version the one in use, or **Reject** to turn it down. A rejected version never runs.

### New versions

A newer version is uploaded the same way, and waits on its own install screen, which says which version it replaces. Until you approve it, the version in use stays. The integration's page lists every version with where it stands: **Needs your approval**, **In use**, **Replaced** (in use before) or **Rejected**.

The install screen of a version that would replace the one in use says what is different from it, in words, under **What changes from version** and the number of the version in use: hosts it reaches that the version in use does not, or no longer reaches; hosts its sign-in talks to, added or removed; software it installs that the version in use does not, or no longer installs; connection fields added or removed, with new secrets marked; for a setup, hosts it may send your browser to, fields it may ask for and values it keeps, added or removed, and a kept value that becomes secret or stops being one; actions added or removed; and fields added to or removed from the access form. When none of those differ, it says **No change to hosts, software, secrets, actions or the access form.** The rest of the screen still lists everything the version holds.

### Roll back to an earlier version

If a new version does not work as it should, you can go back to the one it replaced. On the integration's page, under **Versions**, a version marked **Replaced** has **Roll back** beside it; its install screen has the same button, and says what rolling back to it changes from the version in use. **Roll back** asks first, naming the version that stops and the one that runs in its place.

Rolling back stops the version in use and runs the earlier one again in a few seconds, from the code that was approved for it. The version you rolled back from is marked **Replaced**, and you can roll back to it in turn. A rejected version never runs, so it has no **Roll back**, and a version waiting for approval is approved, not rolled back to. The roll back is in [Recent calls](#recent-calls), under your name.

Your agents' access stays as it is. Access that no longer fits the earlier version's access form is marked, as after a new version (see [Give agents access](#give-agents-access)). Every version is kept until you remove the integration, so any version that was in use can be rolled back to.

### Share an integration

An integration from a bundle can be downloaded again as one, to install it on another team. On the integration's page, **Download bundle** at the top saves the version in use as a `.zip`, named after the integration and the version, and under **Versions** every version has its own **Download**, so an earlier version can be passed on too. The zip holds the files the version was installed from, nothing of your connection or your agents' access, and uploads on another team as any bundle does. An integration that ships with Kivali has no bundle to download.

## Connect it

On the integration’s page, under **Connect**, choose **Connect**. For an integration you connect with a token, it asks for the connection's fields, under the integration's name; fill them in and choose **Continue**. The fields are the approved version's, so approve a version first. **Cancel** stops, keeping nothing. An integration whose own setup makes its connection, such as [GitHub](#github), takes you through that setup's steps instead. A setup that does not finish (you cancel it, it fails, nobody takes a step for ten minutes, or Kivali restarts meanwhile) keeps nothing: Kivali asks the integration to undo what it made on the service where it can, forgets the connection, and you can connect again from the start. Starting the setup again while one is under way, in another tab say, ends the first and carries on with what it already made. Once the integration is connected, its page leads with its **Requirements** (open while one needs you; one line, "All met", otherwise) and **Access**, who may use it and how far. Its **Connection** block stays closed until you open it: it holds the connection form, where you change a field and choose **Save connection**, the setup notes, and **Disconnect**.

Secret fields, such as tokens, are never shown again once saved: the page says only that they are set. To change one, type the new value; leave it empty to keep the one stored. They are kept outside your team's data volume, so [backups](backup-and-restore.md) never contain them. After a restore, an integration whose secrets are not there has its **Connection** requirement not met until you enter them again: its **Fix** asks for them.

Saving a connection restarts the integration with the new values, which takes a few seconds.

**Test connection** asks the integration to sign in to the service with the connection you saved, and shows what it found under the form: whether it worked, the integration's own words (such as the account it signed in as), and when you last tested. A failed test keeps the integration's reason, so you can see what to fix. Saving the connection again clears the last result. An integration that has no connection test, such as [Git (Basic)](#git-basic), has no **Test connection**.

### What it needs from you

An installed integration lists its **Requirements**: what it needs to work, each met or not, with what is so in words. Kivali's own come first:

- **Approved version**: the version in use.
- **Connection**: its secrets are stored on this Kivali server.
- **Sign-in**, for an integration you connect by signing in: the service still accepts the sign-in.
- **Connection test**, for one that has a test, once it has run: whether the last test passed, in the integration's words.
- **Running**, for one that runs on its own: whether it is running now.

Then the integration's own, such as whether its app is still installed on the service. Kivali asks the integration for those when you open its page, and once an hour; the list and **Needs you** show what it last answered.

A requirement that is not met has **Fix**, which runs what mends it, with the same steps as connecting: entering the connection again, signing in again, or the integration's own steps, such as approving a new permission on the service. Each one not met is also in **Needs you** on Home, under the integration's name, with **Open** to go to its page; it leaves once the requirement is met, or once you disable, disconnect or uninstall the integration.

### Connect by signing in

Some services hand out no token to paste: you connect them by signing in to the service and approving Kivali there. The integration's page says so and shows the service's own setup steps above the connection form. When the integration asks the service for particular scopes, its install screen and page list them ("Asks the service for: ..."); one whose app's own settings decide what it may do lists none.

1. Register an app with the service, as its setup steps say. The service gives the app a client id and a client secret.
2. On the integration’s page, choose **Connect**, paste the client id and the client secret, with any other field it asks for, and choose **Continue**. The client secret is kept like any other secret field and never shown again.
3. The page shows a code, a **Copy code** button, the service's page to enter it on (it opens in a new tab), and how long the code works, at most ten minutes.
4. Enter the code on the service's page and approve the sign-in there. The integration's page notices within a few seconds and shows **Signed in**, with the account you signed in as when the integration has a connection test. The integration restarts with the sign-in, which takes a few seconds.

If you turn the sign-in down on the service's page, or the code runs out before you approve it, the page says why and offers **Try again**; **Cancel** stops a sign-in that waits. A sign-in waits in Kivali's memory only: if Kivali restarts meanwhile, start it again. Saving the connection with a new client id or secret ends a sign-in that is waiting, since it was started with the old ones.

#### In your browser

Some services sign in in your browser instead of with a code (HubSpot, Slack, Notion), and some offer both. **Connect**, and the **Sign-in** requirement’s **Fix** once connected, sign in in the browser where a sign-in there can finish, and with a code elsewhere (below).

1. Open the integration's page at the address you will keep using for Kivali. Above the connection form, the page shows a **Redirect address**, with **Copy**: Kivali's address as your browser reaches it, followed by `/connect/callback`. It is the same for every integration.
2. Register an app with the service, as its setup steps say, and enter that redirect address exactly as the app's callback URL (some services call it the redirect URL). The service gives the app a client id and a client secret.
3. Choose **Connect**, which names the host it sends you to ("Signs in at app.hubspot.com"), paste the client id and the client secret, and choose **Continue**.
4. Choose **Continue to** the service's host. The page takes you to the service, where you approve Kivali; the service sends you back to the integration's page, which finishes the sign-in and says **Signed in**.

Some services let Kivali register itself, as many MCP servers do: the install screen says "Kivali registers itself with the service; there is nothing to paste." There is no app to register and no redirect address to copy. Choose **Connect** (or, once connected, the **Sign-in** requirement’s **Fix**): Kivali reads where the service signs in from the service itself, registers the team with it under the name "Kivali" and your team's name, and takes you to the service's sign-in, as above. Kivali goes only to the hosts the install screen lists under "Signs in at", and the page names the one you are sent to once it knows it. Signing in later from another address of the team, such as its address for other devices, registers the team again with that address too. **Sign out** and **Disconnect** also remove Kivali’s registration with the service where the service allows it, and the next sign-in registers again.

The sign-in finishes in the browser that went to the service, signed in to Kivali: if Kivali asks you to sign in on the way back, do, and the sign-in carries on. If you turn it down, take longer than ten minutes, or come back in another browser, the page says why and offers **Try again**. One browser sign-in at a time: going to a service from the same browser for another integration ends the one before, and its page says so.

Services check the redirect address exactly, character for character, and set rules for it:

- Most require `https`. Most allow plain `http` only for `localhost` or `127.0.0.1`, at any port; some, such as Slack, allow no plain `http` at all.
- Some, such as Google, refuse a bare IP address other than loopback and names under private suffixes such as `.local` or `.internal`, even though only your browser has to reach the address.

A Kivali server you run yourself therefore needs a real host name with a certificate for browser sign-in. Tailscale is the easy way: its MagicDNS name for the server, under `ts.net`, is a real name with a certificate Tailscale issues, reachable only from your tailnet. [Self-hosting](self-hosting.md#sign-in-to-integrations-in-the-browser) says how.

**In Kivali Desktop**, **Continue to** the service opens your computer's default browser, not the team's window, which many services refuse to sign in inside. The browser arrives already signed in to the team, goes on to the service, and comes back to the integration's page there; the team's window says **Signed in** within a few seconds, and you can close the browser tab. While it waits, the team's window keeps **Continue to** the service, for a browser that did not open.

For a team on your computer, the redirect address is the team's own address there, `http://127.0.0.1:<port>/connect/callback`, with the port the page shows:

- Some, such as GitHub, accept `http://127.0.0.1/connect/callback` registered once, whatever the port.
- Most services need the exact address, port included: register the one the page shows. The team keeps its port.
- A service that allows no plain `http` address, such as Slack, cannot sign in to a team on your computer this way. Sign in with a code where the integration offers it, or give the team an `https` address under **Other devices** in the team's settings and sign in from a browser at that address.

Kivali offers a sign-in in the browser only where it can finish: at the team's address on your computer (`127.0.0.1`), and at the team's address for other computers (**Other devices**, or a server's own address) opened in a browser. Anywhere else the page signs in with a code instead, when the integration has it:

- In Kivali Desktop's window for a team elsewhere, which cannot hand a sign-in to a browser signed in to that team.
- At another address for the team, such as a plain `http` one on your network, which the service's app does not know to send you back to.

When the integration signs in only in the browser, the page says it cannot sign in from there and gives the team's address: open the team in your browser (**Open in browser** in Kivali Desktop) and sign in from its page.

Kivali keeps the sign-in fresh: the integration never sees the client secret or what Kivali renews the sign-in with, only a token that works at the moment of each call, which Kivali renews shortly before it expires. When the service no longer accepts the sign-in (you revoked the app, or changed its secret), its **Sign-in** requirement is not met, with the service's reason and, for an integration that ships with Kivali and knows one, the likely cause, and **Needs you** on Home lists it until you sign in again with its **Fix**, which does not ask for the client id and secret again.

While a connection needs you to sign in, an agent's call through it is refused with words that say the connection needs you to sign in again and to ask you; nothing reaches the service.

**Sign out** ends the sign-in: Kivali forgets the tokens and asks the service to revoke them where the service allows it. The client id and secret stay, so you can sign in again at once with the **Sign-in** requirement's **Fix**. Disconnecting the integration revokes the tokens the same way. Disabling the integration, signing out and disconnecting it end a sign-in that is waiting for your approval.

### Not running

A connected integration that runs on its own has its **Running** requirement not met while it is starting, and when it has stopped. Starting takes a few seconds, or longer for one that installs software: it installs it each time it starts, before it runs, and **Recent logs** says **Installing** and the packages' names, then **Installed**, or which could not be installed and why. While it is not running, **Test connection** and the options in an access form are not available.

If it stays not running, open **Recent logs** on its page: the integration's own messages and Kivali's about it say what happened, such as a server that did not start or stopped answering. Kivali starts a stopped integration again by itself, and restarts one that stops answering. If the log shows the integration failing each time it starts, the version itself is at fault: ask whoever built the bundle for a fixed version, or disable it. An integration whose software cannot be installed stops, tries again each time Kivali starts it, and says so in its log each time: a package name Debian does not have needs a fixed version, while a failure to reach Debian's repositories may pass on its own. On a team short of memory, the log says the team does not have enough memory for it; raise the team's memory in Kivali Desktop's settings, or ask whoever runs your server.

## Its log

**Recent logs** on an integration's page starts closed; select the arrow beside its heading to open it. It lists the latest lines the integration wrote, oldest first: its own messages, the errors it printed, and Kivali's notes about it, such as when it started, stopped or was restarted. **Refresh** reads it again. Secret values of the connection never appear in it: Kivali replaces each one with `[secret]` before the line is kept. The log keeps the last few thousand lines and goes when the integration is removed.

## Give agents access

Under **Access**, choose an agent and **Give access**. The form is the integration's own: a list of blogs or projects to pick from, switches such as "May publish and edit", text fields. A list the integration fills in from the service, such as the blogs your connection can see, is loaded through the running integration with **Load options**, which waits while the integration is not running. An integration with no form gives an agent all of it or nothing. **Change** edits an agent's access and **Remove** takes it away.

Each agent's access is shown in words, on the integration's page and on the agent's **Integrations** tab. When the version in use changes, by an approval or a roll back, so that an agent's access no longer fits the form, the access is marked invalid, with the reason, and gives nothing until you set it again with **Change**. The top of the integration's page says how many access rows no longer fit, such as "1 access row no longer fits this version's access form. Set it again.", until every row fits.

An agent that is offboarded loses its access to every integration.

## What an agent sees

An agent with access to a running integration has its actions as tools, named after the integration and the action: `hubspot_create_post` for HubSpot's "create post". It has only the actions its access lets the integration offer it, among those the install screen listed, and is told that you decide what it may do with each integration. An agent without access, or with access that no longer applies, has none of them.

**Actions** on an integration's page lists its actions grouped by the access that unlocks them, as the integration declares it: first those any access gives, under **Any access**, then those a part of the access form adds, under that part's name, such as **May publish and edit**. An integration whose actions all come with any access lists them without headings.

Kivali checks every call against the agent's access before the integration sees it, and tells the integration which agent is calling and with what access. A call Kivali refuses (no access, the integration disabled, not running or still being connected, an action the agent's access does not include) tells the agent why and to ask you. What comes back is the integration's text, with the connection's secrets replaced by `[secret]`; images and other content that is not text are not passed on in this release.

A change to an agent's access, given or taken away, changes its tools from its next turn. Access you take away is refused from the next call, even in the middle of a turn.

### Calls that wait for you

Some actions of an integration that ships with Kivali reach other people, such as sending an email, and wait for you before anything happens. Every send of [Google Workspace](#google-workspace) does: an email or a draft sent, an invitation, a change or deletion that reaches the people an event invites, an answer to an invitation, and a comment or a reply to one ([Sends that wait for you](#sends-that-wait-for-you)). The agent's call does nothing yet: the agent is told it waits for your approval, that it will hear in its chat whether it was done, and that no word within a day means it was not, and it carries on with other work.

You see an **Integration action** row in **Needs you**, from the agent, titled with what it would do, such as "Send an email to alice@example.org". Expand it to read the integration and the action, whom it reaches, and every field exactly as it will go, such as the subject and the whole body, as plain text. When the agent's calls of that integration returned mail, events or documents written outside your domain in the day before it asked, a marked line above the fields says how many such writers they returned and names the latest, such as "Before asking, this agent read 3 messages, events or documents from outside your domain, the latest from alice@example.org." Text from outside can try to steer an agent, so read the fields closely then. What you read is what runs: Kivali refuses, before it reaches you, a request that carries the integration's secret or characters that show as nothing on screen, such as zero-width spaces or direction marks. The joiner inside emoji such as a family is allowed.

**Approve** runs it at once, with the connection as it is then, and **Deny** keeps it from happening; either can carry a note, which the agent reads. If something changed since the agent asked, such as its access being removed, the switch that lets it ask (such as **May send mail (each send waits for your approval)**) being turned off, or the integration's sign-in lapsing, **Approve** says why it cannot run and the row stays until you approve again or deny it. If the integration finds as it runs that it cannot be done, such as an event that changed after you saw it, nothing is done, the row ends, and you are told why. The agent hears in its chat what happened: approved, with what the integration answered, or denied, with your note.

A row nobody answers within a day ends with nothing done, and so does every waiting row of an integration you disable or disconnect, or of an agent you offboard. An agent can have five rows waiting at a time; a sixth request is refused, and the agent is told to wait for your answers. Waiting rows live only while Kivali runs: a restart drops them with nothing done.

## Recent calls

**Recent calls** on an integration's page starts closed; select the arrow beside its heading to open it. It lists the latest calls through it, newest first: when, which agent, the action, whether Kivali let it through and, for a call that was refused or failed, why, and how long it took. Your own Test connection, the options an access form loads and your roll backs are in it too, under your name; a roll back reads **Roll back**, with the versions it went from and to. Calls an agent made are listed whether Kivali allowed them or not. A [call that waits for you](#calls-that-wait-for-you) is listed twice: once as it starts waiting, naming what it would do, and once as it ends, approved with what was done, denied with your note, or not answered within a day; one a restart dropped has only the first. An approval Kivali could not run adds a row of its own, saying why.

## Disable, disconnect or uninstall it

The **Enabled** switch at the top of a connected integration's page turns it off for every agent at once and stops it running, keeping the connection and everyone's access for when you turn it back on. It reads **Disabled** meanwhile.

**Disconnect**, at the end of the **Connection** block, asks first, naming what goes. Kivali asks the service to undo the installation where the integration can (such as removing its app), revokes a sign-in's tokens where the service allows it, then forgets the connection and its secrets, and every agent's access goes. If the integration could not undo its side, the page says why and nothing changes. The integration stays on the team, **Installed**, ready to connect again.

**Uninstall**, at the top of the page, is for an integration you uploaded: it disconnects it the same way, then takes it off the team with every version and its log, and it stops running; uploading a bundle brings it back from scratch. An integration that ships with Kivali cannot be uninstalled while this version of Kivali carries it; one that another version shipped can.

## Git (Basic)

**Git (Basic)** ships with Kivali. It lets agents clone, fetch and push your repositories on a git host other than `github.com` over HTTPS with a token you give it; `github.com` is the [GitHub](#github) integration's. Everything else an agent does with a repository (status, branches, commits, diffs, merges) is plain git in its own shell.

### The connection

- **Host**: `gitlab.com` or your own git host that serves HTTPS, without `https://`. A host that is not a host name (with a path, for example) is refused when you save, and so is `github.com` (or `www.github.com`): connect [GitHub](#github) for those repositories instead.
- **Access token**: a token that can read the repositories your agents will use: on GitLab, an access token with `read_repository`, or your host's equivalent. For agents that push, the token also needs `write_repository` on GitLab, or write access on your host. Give it no more than that.

A Git connection to `github.com` that an earlier version of Kivali saved stays after the update, but every clone, fetch and push through it is refused, and the agent is told to ask you. Connect [GitHub](#github) for those repositories, then disconnect Git (Basic), or save its connection with another host.

Git (Basic) has no **Test connection**. An agent's first clone shows whether the token works: every clone, fetch and push is in **Recent calls**, with why it failed when it did.

Git (Basic) has one connection, like every integration: one host and its token. Every agent's clones, fetches and pushes go to that host with that token. To point Git (Basic) at another host, save the connection with the new host and a token for it.

The token is kept in the connection's Secret, like every connection secret. The connection also has a network credential of its own, kept beside the token, that lets an agent's clone, fetch or push reach the connection's host and nothing else. Your team's allow list does not change.

A team whose Git (Basic) holds several connections, one per host, as earlier versions of Kivali allowed, keeps one when Kivali starts: the one most agents' access uses (the oldest, when two are used as much). The others are set aside, not deleted: agents can no longer use them, and their tokens stay where they were kept until you disconnect Git (Basic), which forgets them with the connection. An agent whose access was given for a connection set aside keeps its access on the page, marked invalid with the reason, such as "set for the connection Company GitLab, which Git (Basic) no longer has", and can use Git (Basic) again once you set its access with **Change**. If Kivali cannot finish setting the others aside, it says so in its log, and no agent can use Git (Basic) (every access row says why) until it can. Going back to an earlier version of Kivali afterwards keeps the connection, but every agent's Git (Basic) access then reads as invalid until you set it again.

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

An agent with access has Clone and Fetch, and Push when its access names a repository it may push to:

- **Clone** copies a repository its access includes into a new or empty directory of its files or its home.
- **Fetch** brings new commits and tags into one of its working copies, from the repository's address or the one the working copy came from, if its access includes it.
- **Push** sends branches and tags from one of its working copies to the repository, when the repository is one it may push to and its access allows those branches and tags. It can push a branch to the same name or to another (its current commit to `agent/fix-1`, say) and push tags. A push that would overwrite history is refused unless the agent asks to force and you allowed force-pushing, and an existing tag is never moved without force. A push of several branches and tags lands whole or not at all: if the host refuses one, it takes none of them, so the host must accept atomic pushes (GitHub, GitLab and plain git servers do). The host's own rules (protected branches, required reviews) still apply.

Git (Basic)'s page lists Clone and Fetch under **Any access** and Push under **Repositories it may push to**.

All three go to the connection's host with your token. The working copy is an ordinary one, whose origin is the repository's address without the token, and the agent works in it with plain git in its shell. That plain git reaches only the hosts on your team's allow list, and without your token, so a repository host that is not on the list is reachable only through these three actions.

Every clone, fetch and push is in **Recent calls**, with the repository and the branches and tags that moved (a push also says when it forced), or why it failed. Kivali checks each one against the agent's access before anything runs. Each has two rows: one when it starts and one when it ends. If you change an agent's access while its push runs, the second row still records what the push wrote, and says the access changed during it. If you disconnect Git (Basic) and connect it again while an agent’s clone, fetch or push runs, the second row records what ran and says the connection changed during it.

### Not supported

- SSH addresses (`git@gitlab.com:...`): give the HTTPS address or `owner/name`.
- Git LFS: a clone holds the LFS pointers, not the files.
- Submodules are not cloned.
- Deleting a branch or tag on the remote: a push only adds and moves them.
- Very large repositories are slow, and a clone stops after 15 minutes, a fetch or a push after 10.

## GitHub

**GitHub** ships with Kivali. It lets agents read and open issues, comments and pull requests in your GitHub repositories, close the issues and pull requests the app opened, and clone, fetch and push the repositories, as a private GitHub App its setup makes on your account or an organization's. It runs inside Kivali and reaches only `api.github.com` and `github.com`; an agent's clones, fetches and pushes run in its own sandbox and reach only `github.com`.

### Connect it

On GitHub’s page, choose **Connect**. Setup makes the app and installs it on GitHub's own pages, where you are signed in to GitHub, and you copy nothing:

1. Name the account to make the app on: an organization's name, or leave it blank for your personal account. Choose the account that owns the repositories agents should reach: the app can reach only that account's repositories. Choose **Continue**.
2. Choose **Continue to github.com**. GitHub's form for a new GitHub App opens filled in: the name (Kivali and your team's name), the team's address, the webhook off, installation only on that account, and the repository permissions Contents, Issues and Pull requests read and write, Metadata read-only. App names are unique across GitHub, so GitHub may ask you to change the name. Choose **Create GitHub App**.
3. GitHub sends you back to Kivali, which takes the app GitHub made. Choose **Continue to github.com** again: on GitHub's page for installing the app, choose the repositories agents should reach and choose **Install**. Agents reach only repositories the app is installed on.
4. GitHub sends you back, and GitHub is connected. When an organization's owners approve installations first, the page waits until one of them approves it on GitHub.

Kivali keeps the app's id, name, its private key and where it is installed (the installation and the account it is on), and nothing of your own GitHub account's. The private key is a secret, never shown; the app's id, name and installation are GitHub's public names for it, so what the app writes on GitHub reads as written by `<the app's name on GitHub>[bot]`, to agents as on GitHub. Each call is made as the app: Kivali signs with the app's key for a token that lasts an hour at most, narrowed to the call's repository and to what the action needs. The app's key never leaves Kivali's server; a clone, fetch or push takes a token of its own to the agent's sandbox, as **What agents can do** below describes. GitHub also makes a client id and secret for the app, for signing people in with it; Kivali signs no one in with the app and does not keep them.

If setup stops before it is done, Kivali keeps nothing, and you can start again; an app GitHub already made stays on GitHub, where you can delete it. A team opened at a plain `http` address, such as `http://127.0.0.1` in Kivali Desktop, gives GitHub that address to send you back to: if GitHub refuses it, give the team an `https` address under **Other devices** and start again from there.

**Test connection** names the app, as the bot agents act as, and the account it is installed on.

### What it needs once connected

Besides Kivali's own, GitHub's page lists four requirements, each with a **Fix** that takes you to the page on GitHub that mends it:

- **The app's key works**: GitHub accepts the app's key. If not, **Fix** sets GitHub up again, with a new app.
- **Installed on GitHub**: the app is still installed, and not suspended. Removed on GitHub, **Fix** installs it again; suspended, it opens the installation's settings on GitHub.
- **Repositories it reaches**: how many repositories the installation reaches, or all of them. Reaching none, **Fix** opens the installation's settings, to choose them.
- **Permissions current**: the installation has every permission this version of Kivali needs. When a later version needs one more, **Fix** opens the app's settings on GitHub to add it (or the installation's, to approve it), and the page waits until GitHub shows it granted. Pushing needs Contents read and write, so an app made by a version that could not push, with Contents read-only, reads not met after the update, naming `contents: write`: **Fix** opens the app's permissions on GitHub, where you set Contents to read and write and save, then approve the change for its installation where GitHub asks. Until then, pushes are refused by GitHub, and everything else works as before.

### Disconnect

Disconnecting GitHub removes the app’s installation from GitHub, then Kivali forgets the app's key and every agent's access. The app itself stays on GitHub: delete it under **GitHub Apps** in the account's settings on GitHub (for an organization, the organization's settings), where the **The app's key works** requirement links it, when you no longer need it. Connecting GitHub again makes a new app.

### From an earlier version of Kivali

An earlier version of Kivali connected GitHub by signing in with a GitHub App you registered yourself. This version makes its own app, so that connection does not carry over: after the update, GitHub reads **Installed** and says to set it up again, and agents cannot use it until you do. Connect it as above. Connecting it replaces the earlier connection, and Kivali revokes the earlier app's sign-in; every agent's access is kept, and works again once GitHub is connected. If you no longer want GitHub, choose **Disconnect** instead: it removes the earlier connection and every agent's access, and revokes the earlier app's sign-in. Either way, the app you registered stays on GitHub: delete it there when you no longer need it.

### Access

An agent's access has two parts:

- **Repositories**: each `owner/name`, such as `acme/widgets`, or a pattern such as `acme/*`, which covers every repository directly under `acme`. `*` is the only wildcard, and capitals do not matter, as for [Git (Basic)](#access). Name at least one.
- **May create, comment, push and close**: lets the agent open issues and pull requests, comment on them and push branches and tags in those repositories; close the issues and pull requests the app opened; and delete a branch whose commits beyond the default branch the app made, every one, or a branch merged into it. Off, the agent only reads (cloning and fetching among that), and its tools do not include those actions.

Everything an agent does is done as the app, so GitHub shows the app (its name followed by `[bot]`) on its issues, comments and pull requests, not you. That is also how Kivali tells what the app opened: an issue or pull request whose author on GitHub is the app's `[bot]` user. Anything you, anyone else or another app opened, an agent can read and comment on but not close.

### What agents can do

GitHub's page lists these under **Actions** by the part of the access that unlocks them. With **Any access**, an agent can:

- List the repositories its access names, and read a repository's details, its branches, and a comparison of two branches, tags or commits (the commits and the files changed, not the changes themselves).
- List and read issues, with the latest 20 comments, and list and read pull requests, with the diff when it asks (cut at 50,000 characters). Issue search words stay within the repository named: a search that names another repository, organization or user is refused.
- **Clone** a repository its access names into a new or empty directory of its files or its home, and **Fetch** new commits and tags into one of its working copies.

With **May create, comment, push and close**, it can also open an issue, comment on an issue or pull request, **Push** branches and tags to a repository its access names, and open a pull request from a branch on GitHub, such as one it pushed. It can also close what the app opened, and delete a branch whose own commits are all the app's, or that is fully merged:

- Close an issue the app opened, or a pull request the app opened without merging it, with a comment first when it gives one. An issue or pull request someone else opened is refused, and a merged pull request cannot be closed.
- Delete a branch when every commit it has beyond the repository's default branch was authored and committed by the app (its `[bot]` user), or when it has none, being fully merged into the default branch. Authorship is what git records in each commit, and the agent cannot rewrite the branch on GitHub (pushes never force), so one commit of yours or anyone else's on the branch keeps it from being deleted. Kivali compares the branch itself, never a tag of the same name, and reads it again just before deleting it, leaving it if it moved meanwhile. The default branch is never deleted, and neither is a branch an open pull request in that repository has as its head or its base, whoever opened it. Only that repository's pull requests are checked: an open pull request elsewhere, such as in the repository a fork came from, is not. A branch with more than 250 commits beyond the default branch, with a thousand or more open pull requests from it or into it, or with no history in common with the default branch, is not deleted: Kivali cannot check it.

Clone, Fetch and Push work as [Git (Basic)](#what-agents-can-do)'s do: the working copy is an ordinary one, whose origin is the repository's address without a token, and the agent works in it with plain git in its shell. For an agent with **May create, comment, push and close**, a working copy GitHub's Clone makes has the app's `[bot]` user as its git identity, so the commits the agent makes there are authored and committed as the app, and a branch of them is one it may later delete, unless the agent changes the identity. If GitHub does not give Kivali the bot user, the clone still goes ahead, without that identity, and the agent is told its commits there will not read as the app's. Each names its repository, as its `https://github.com/...` address or `owner/name`. Each takes a token of its own: once Kivali has checked the call against the agent's access, it has the app mint a token for that one call, narrowed to that one repository and to reading it (Clone, Fetch) or writing it (Push). Kivali does not keep it, and it expires within the hour. A push may write any branch or tag in the repository, lands whole or not at all, and is never forced: a push that would overwrite history, or move an existing tag, is refused, and so is a push that deletes a branch or tag. GitHub's own rules for the repository, such as branch protection, still apply.

Each action is in **Recent calls**: one that opens, comments, closes or deletes names what it made, closed or deleted, and a clone, fetch or push has two rows, one when it starts and one when it ends, as for Git (Basic). A call for a repository the agent's access does not name, or for one under another account than the one the app is installed on, is refused before anything reaches GitHub, and the agent is told to ask you. [Git (Basic)](#git-basic) does not handle `github.com`: GitHub's repositories are this integration's.

### Not supported

- GitHub Enterprise Server and other hosts than `github.com`.
- Force-pushing, and deleting a tag on GitHub.
- Closing an issue or pull request the app did not open, and deleting a branch that holds a commit someone other than the app made.
- SSH addresses, Git LFS files and submodules, as for [Git (Basic)](#not-supported). A clone stops after 15 minutes, a fetch or a push after 10.
- Anything the app's permissions do not cover, such as changing the repository's settings, merging pull requests or approving reviews.

## Google Workspace

**Google Workspace** ships with Kivali. It lets agents work in your Gmail, Google Calendar and Google Docs: read your mail, write drafts, label messages and send mail you approve; read your calendars, change events on your own calendars, and invite people and answer invitations once you approve; read and write documents, and comment on the documents Kivali made once you approve. It signs in with an OAuth client you make on Google's Auth Platform, in a Google Cloud project of yours. It runs inside Kivali and reaches only Google's APIs (`gmail.googleapis.com`, `www.googleapis.com` and `docs.googleapis.com`) and Google's sign-in (`accounts.google.com` and `oauth2.googleapis.com`).

What an agent may do is split three ways in each service. **Read** sees what is in your account. **Write** changes something in your account that no other person is told about or can see, such as a draft, a label, or an event on a calendar you share with no one. **Send** is anything that makes Google deliver something to another person, such as an email, an invitation or a comment, and any change to a calendar or a document that someone else can see, and every send waits for your approval.

### Connect it

Google signs agents in through an app of your own, made in Google Cloud console, so Google Workspace's page says what the setup is for and that it takes about five minutes, once, and **Connect** walks you through the console one page at a time. Each step says one thing to do, with a link that opens the page in a new tab; do it there, come back and choose **Continue**. Sign in to the console with the Google account agents will work in.

1. Create a project for this in the page's project picker, then turn on the four APIs the page lists: Gmail, Google Calendar, Google Docs and Google Drive. A project has one sign-in app, which shares its name, audience and verification with everything else in the project, so a project you use for anything else, such as an app of your own, is best left alone.
2. Set up the sign-in screen: choose **Get started**, name the app Kivali, give your email address, and for the audience choose **External** for a personal Gmail account or **Internal** for a work account, which keeps the app to your organization.
3. If the audience is External, under **Test users** choose **Add users** and add the Google account agents will work in. The app stays in Testing, where Google ends every sign-in seven days after it was made: once a week the **Sign-in** requirement goes unmet, **Needs you** lists it, and its **Fix** signs you in again. Publishing the app would stop that, but Google then asks for a homepage, a privacy policy and terms of service, and still marks an app that reads Gmail unverified until it has reviewed it, so Kivali does not ask for it. An Internal app has no test users and no expiry.
4. Create the client. The step names the type that works where you opened the team: **Desktop app** for a team on your computer opened at `127.0.0.1`, which Google signs in at any port, so there is nothing to register; or **Web application** for a team opened at another address, such as a server's or the team's address for other devices, with each address to add under **Authorized redirect URIs** shown with a **Copy** button. Google refuses some addresses there, such as a bare IP address other than loopback ([In your browser](#in-your-browser)).
5. Choose **Download JSON** where Google shows the client it made, open the file, and paste everything in it into **Client JSON**. Kivali reads the client id and secret from it and keeps nothing else of the file. A file that is not an OAuth client's, such as a service account's key, one that lacks the client id or secret, or a Desktop app's for a team opened elsewhere, is asked for again, with what is wrong.
6. Choose **Continue to accounts.google.com**, sign in with the Google account agents will work in, and allow everything Kivali asks for. Google sends you back, and Google Workspace is connected. Google Workspace signs in only in the browser, so where the page cannot finish a sign-in in the browser it says where to open the team instead ([In your browser](#in-your-browser)).

Kivali keeps the client's id and secret and the sign-in's tokens as secrets: they are never shown, and Google Workspace's calls are given only a token that works at that moment, which Kivali renews. **Test connection** names the Google account the sign-in is for ("Signed in as dana@example.com"). That account is what "your domain" means below: everyone at its domain, or, for an account at `gmail.com`, that one address alone.

### What it needs once connected

Google Workspace adds no requirements of its own to [Kivali's](#what-it-needs-from-you). Its **Sign-in** requirement is not met when Google no longer accepts the sign-in, and **Needs you** lists it until you sign in again with its **Fix**, which does not ask for the client's JSON again. When Google refuses the sign-in as ended, the requirement adds that if this came about a week after you signed in, it is Google ending the sign-in of an app in Testing after seven days, as it does every week: sign in again. Google refuses a sign-in you revoked in your Google account the same way, so the week is the likely cause, not the only one.

An action whose API you did not turn on in the project is refused by Google, and the agent is told to ask you. **Test connection**, which reads your Gmail profile, and **Load options**, which reads your labels, calendars and folders, say to turn on the API they need.

**Disconnect** forgets the client id and secret, the sign-in and every agent's access, and asks Google to revoke the sign-in. The project and the client stay in Google Cloud console; delete them there when you no longer need them.

### Access

An agent's access has three switches and a list for each of Gmail, Google Calendar and Google Docs, and one list for whom its sends may reach:

| | Gmail | Google Calendar | Google Docs |
| --- | --- | --- | --- |
| Read | **May read mail** | **May read calendars** | **May read documents** |
| Write | **May draft and label** | **May change events** | **May write documents** |
| Send | **May send mail (each send waits for your approval)** | **May invite and respond (each waits for your approval)** | **May comment (each comment waits for your approval)** |
| What it may use | **Labels it may read** | **Calendars it may use** | **Folders it may read** |

- A switch left off gives none of its actions. A send switch only decides whether the agent may ask: every send waits for your approval, always ([Sends that wait for you](#sends-that-wait-for-you)). Inviting people, changing or deleting an event that invites others, and any change on a calendar shared with others, also need **May change events**, which gives the actions that create, change and delete events. A change to a document shared with others, or a document made in a folder shared with others, needs **May comment (each comment waits for your approval)** as well as **May write documents**.
- **Labels it may read**, **Calendars it may use** and **Folders it may read** hold the agent in that service, writing and sending included: it reads and replies to mail only in those labels (and, when the list names any, changes labels only on messages in them), uses only those calendars, and reads, changes and comments on documents only in those folders and the folders inside them. A list left empty is everything, except that an empty label list leaves out Spam and Trash: name them to let the agent read them. **Load options** lists what your account has: Gmail's own labels (Inbox, Starred, Important, Sent, Drafts, Spam and Trash) and yours, the calendars in your Google Calendar list, and the Drive folders you can open. Each is shown by its name, with Google's id for it in parentheses, and the agent is held to the id.
- **May send to** is whom the agent's sends may reach: addresses such as `dana@example.com`, or `*@example.com` for everyone at that domain (not the domains under it). Leave it empty for anyone. Saving the access refuses an entry that is neither. A send is refused before it reaches you when anyone it reaches is outside the list: everyone an email goes to; everyone Google tells of an invitation, a change to an event or an answer to one; everyone Google tells of a comment; and everyone a calendar or a document is shared with, for a change to it. While the list names anyone, a send that also reaches people whose address Google does not show is refused too, since Kivali cannot check them, and so is a change to a calendar or a document shared with everyone at a domain or with anyone, even a domain the list names with `*@`, since Kivali cannot list whom such sharing reaches. The list does not hold a draft: nothing leaves your account until it is sent.

### What agents can do

Google Workspace's page lists its actions under **Actions** by the switch that gives each. A call the agent's access does not allow is refused before anything reaches Google, and the agent is told to ask you. Each call is in **Recent calls**, naming the messages, attachments, events and documents it read, made or changed, and how much, never what a message or a draft says.

#### Gmail

With **May read mail**, an agent can:

- List the labels its access includes: Gmail's own, then yours by name, each with its id.
- Search your mail with Gmail's search words, such as `from:dana@example.com has:attachment`, newest first, 25 messages a page, each with its sender, recipients, subject and Gmail's snippet of it. A page lists only messages in its labels, so it may list fewer than 25, or none with more to come.
- Read a message, or a conversation oldest first, as text: its headers, its body, and its attachments by name, type and size. A body over 60,000 characters is cut, and a long conversation names the messages it had no room for, which the agent reads one by one.
- Save an attachment of up to 25 MB into its files ([Attachments](#attachments)).

A message outside its labels is refused, and a conversation shows only its messages in them, saying how many it left out. A message in Spam or Trash is refused, and left out of searches and conversations, unless **Labels it may read** names that label, whatever other labels the message carries.

With **May draft and label**, an agent can:

- Write a draft, a new email or a reply. A reply joins its conversation when its subject matches the conversation's (a changed subject may start a new one in Gmail), keeps its subject with "Re:" before it unless the agent writes another, and, unless the agent names who it goes to, is addressed as Gmail's **Reply** addresses it (to whom you sent a message of yours), or as **Reply all** does when the agent asks, never to you. A reply to all that would go to more than 100 addresses is refused, and the agent names the recipients itself. A recipient's name may not hold an @, a < or > or an invisible character. Nothing is sent: the draft waits in your Drafts, where you can change it and send it.
- Delete a draft an agent of your team wrote. Each draft Kivali writes carries a header, `X-Kivali-Agent`, naming the agent that wrote it, and an agent may delete only a draft that carries one, never a draft you wrote. A draft you have changed in Gmail may no longer carry it.
- Add and remove labels on a message: file it under a label, archive it (take it out of the inbox), mark it read or unread, or star it. Kivali never moves mail to the trash or to spam.

When **Labels it may read** names labels, an agent changes labels only on messages in them, and may add or remove only those labels, and archive, mark read or unread, and star.

With **May send mail (each send waits for your approval)**, an agent can ask to:

- Send an email it writes, new or a reply, addressed as a draft is. It goes to the addresses alone: a name the agent gives with an address is left out.
- Send a draft from your Drafts, one an agent wrote or one you wrote. Once sent, it leaves your Drafts. A draft Kivali cannot show you whole is refused: one with attachments, with more than one plain or formatted version, or with another kind of part such as an inline image, and one holding invisible characters such as zero-width spaces. Send those from Gmail, or take the invisible characters out there first.

What goes is what you saw: a draft changed in Gmail after the agent asked is sent as it was when you saw it, and the changed draft stays in your Drafts; a draft you sent or deleted meanwhile is not sent. Kivali's `X-Kivali-Agent` header never goes with a draft it sends.

#### What the agent reads in an email

An email can be written by anyone, and may carry text meant to steer the agent. Kivali hands the agent each email's own text between two fixed lines that say it is untrusted, and tells the agent never to follow instructions it finds there. Before it, Kivali writes a line of its own: the sender's address, whether it is from your domain, where replies go when the email's Reply-To is at another domain, and what Gmail's checks of the sender (SPF, DKIM and DMARC) found. A message claiming your domain reads as from it only when Gmail's checks confirm that domain: DMARC passed for it, or a DKIM signature or the SPF check passed for your domain or one under it. A check that passed for some other domain, such as a mail service's own, does not count, and such a message reads as from outside your domain, saying it claims yours unconfirmed. Mail you sent or are drafting, which Gmail does not check, is yours.

Text an email hides from the person reading it is left out, and the agent is told how many characters were: text styled not to show (by the email's style rules or its inline styles), moved off the page, clipped away, in a tiny font, in the colour of its background (white text on the white page counts), in comments, and invisible characters such as zero-width spaces. Each link is replaced by a numbered marker such as [link 1] and listed after the body, its host first, so the agent sees where a link goes before following it.

#### Attachments

An attachment the agent saves lands in its files as it came, under `/files/integrations/google-workspace/untrusted-downloads/`, in a folder of its own for each message: the path tells the agent the file is content from outside, never instructions. Kivali never opens it. Its name is cleaned to a plain file name, and an attachment over 25 MB is refused before any of it is read. An attachment saved again is answered where it already is, and a message's folder holds at most 20 files.

#### Give an agent only some of your mail

The label list is how you choose which mail an agent sees. Make a label for it, such as "Invoices", and a Gmail filter that applies the label to the mail you want it to have: in Gmail, open **Settings**, **See all settings**, **Filters and Blocked Addresses**, then **Create a new filter**, describe the mail, and choose **Apply the label**. Then name that label alone under **Labels it may read**. Kivali never changes your filters: Gmail's settings are not among what it asks Google for.

#### Google Calendar

With **May read calendars**, an agent can:

- List the calendars its access includes, each with your role on it (owner, writer, reader, or free and busy only) and its time zone.
- List the events on a calendar, soonest first, 25 a page, over at most 60 days (the next 7 unless it asks for others), each with when it is, its status, its organizer, how many people it invites, whether Kivali created it, and its title. A repeating event's occurrences are listed one by one.
- Read an event: when and where it is, its organizer, its first 50 attendees and how each answered, the host of its video call, how it repeats, and its description.
- Find when calendars are free over at most 7 days: each calendar's busy times, and the times none of them is busy. Busy times carry nothing of the events' text.

It uses only calendars in your Google Calendar list, the ones **Load options** lists: those named under **Calendars it may use**, or every one in your list when that list is empty. A call that names no calendar uses your primary one, and a calendar outside them is refused. An agent cannot read another person's calendar by its address, even one Google would show you; add it to your Google Calendar list first.

Your own calendars are your primary calendar and the calendars made in Google Calendar on which your role is owner. A person's calendar shared with you is never one of them, whatever your role on it. Agents create, change and delete events, and answer invitations, only on your own calendars.

With **May change events**, an agent can:

- Create an event, at a time or all day, on one of your own calendars, inviting no one or only you. Kivali marks each event it creates as made by Kivali, in a private property of the event on that calendar.
- Change an event's title, time, place or description on one of your own calendars, when you organized it and it invites no one but you.
- Delete an event Kivali created, on one of your own calendars, when it invites no one but you. An event you or anyone else made is refused.

Google emails no one about these. Changing or deleting an event that invites anyone but you, a room included, or that someone else organized, would show on their calendars, so it is a send, as are inviting people, changing who an event invites and answering invitations.

A calendar you share is seen by everyone you share it with, so before each create, change or delete Kivali reads who the calendar is shared with, and on a calendar shared with anyone but you, the change is a send too. Someone who sees only when you are free or busy does not count: they see none of what an event says. A calendar shared with no one else stays a write.

With **May invite and respond (each waits for your approval)** as well as **May change events**, an agent can ask to:

- Create an event that invites people.
- Change who an event invites, or change an event that invites anyone but you or that someone else organized.
- Delete an event Kivali created that invites others, which cancels it for everyone invited.
- Create an event, change one, or delete one Kivali created, on a calendar shared with others. Google emails no one about it unless it invites people, but everyone the calendar is shared with sees it.

With **May invite and respond**, whether or not it has **May change events**, an agent can also ask to answer an invitation on one of your own calendars for you: yes, no or maybe, with a note to the organizer. Only your answer changes; an event you organized, or one that does not invite you, is refused.

Everyone Google will tell of a send must be in **May send to**, people the event already invites included: the people invited after the change, those removed, and the organizer, never you; and so must everyone the calendar is shared with. An agent cannot change who an event invites when Google lists only some of its attendees or shows no address for one; it can still ask to change the rest of the event. If an event the agent asked to change or delete changes in Google Calendar before you approve, nothing is done, and you and the agent are told it changed after you saw it; an event deleted meanwhile is gone, with nothing done.

An event's title, place, description and the names of its organizer and attendees are written by whoever made the event, often someone who invited you, so the agent reads them as it reads an email: between two fixed lines that say the text is untrusted, after a line of Kivali's naming the organizer and whether that is your domain, with hidden text left out and links listed after the description, host first. The list of calendars reaches the agent between such lines too, since a calendar's name is written by whoever made it. Kivali judges each event by its organizer's and creator's addresses, on your own calendars as on any other: one of your own calendars' addresses counts as your domain, and a person's calendar shared with you as that person's.

#### Google Docs

With **May read documents**, an agent can:

- Search your Google Docs, those shared with you included, by words in their name or text, 25 a page, each listed with the folder it is in, its owner, whether that is your domain, whether it is shared, when it last changed and whether Kivali made it. Without words, the most recently changed come first.
- Read a document as text, a page at a time: its headings, paragraphs, lists and tables, in every tab. Suggestions no one has accepted are left out, and so are its headers, footers and footnotes.
- List a document's comments, 20 a page, each with who wrote it, when, whether it is resolved and how many replies it has, its text and the document's text it is on.

With **May write documents**, an agent can:

- Create a document, with its text. It goes in the folder the agent names, which must be within **Folders it may read**, else in the first folder of that list, or at the top of your My Drive when the list is empty. Kivali marks each document it creates as made by Kivali, in a note on the document that only your OAuth client can read or set. When Google Drive will not add a file to the folder, Kivali makes the document at the top of your My Drive instead and tells the agent, which cannot read it there while its access names folders.
- Add text at the end of a document's first tab.
- Replace a piece of text everywhere it appears in a document, its headers, footers and footnotes included.

What an agent writes takes a little Markdown: a line starting with `#` to `######` is a heading, one starting with `-` or `*` is a list item, and `**text**` is bold.

An agent changes only documents you own, and documents Kivali made that no one else owns, such as one it made in a shared drive. A document someone else owns and shared with you is refused, so an agent cannot be steered into changing what other people wrote.

A change to a document shared with others is seen by everyone it is shared with, and a document made in a folder takes the folder's sharing. So before each of these writes Kivali reads who the document, or for a new document its folder, is shared with: a person, a group, everyone at a domain, or anyone with the link. Shared with anyone but you, including the other members of a shared drive, the write is a send; shared with no one else, it stays a write.

With **May comment (each comment waits for your approval)**, an agent can ask to:

- Comment on a document Kivali made, quoting the document's text the comment is about when it gives one.
- Reply to a comment on a document Kivali made.

With **May comment** as well as **May write documents**, an agent can also ask to create a document in a folder shared with others, add text to a document shared with others, and replace text in one. Everyone the document or the folder is shared with must be in **May send to**.

It comments only on documents Kivali made: beyond reading your files, Kivali's only Google Drive permission is for the files Kivali creates, and that is the only kind of file Google lets it comment on. Commenting on any of your documents would need the broader Google Drive permission, which Kivali does not ask for. An agent mentions someone by writing their address with `+` or `@` before it, such as `+dana@example.com`; Google emails each person mentioned. Google also tells the document's owner of a comment, and everyone in the comment's conversation of a reply. Everyone Google tells must be in **May send to**. Once you approve, Google posts it as you.

It reads, changes and comments on only documents in the folders under **Folders it may read**, or in folders inside them, up to ten folders down; with that list empty, every document you can open. A document shared with you whose folder you cannot open is in none of them. A document in your trash is left out of a search and refused when an agent names it.

#### What the agent reads in a document

Anyone who can edit a document can write text in it meant to steer the agent. As with email, Kivali hands the agent a document's text, and each comment's, between two fixed lines that say it is untrusted, and tells the agent never to follow instructions it finds there. Before it, Kivali writes a line of its own: who owns the document, whether that is your domain, whether it is shared and when it last changed. A document in a shared drive has no owner of its own, so it counts as from outside your domain.

Kivali builds the text from the document's structure, not from an export, so text styled not to show is left out wherever it is: text in or close to the colour of what is behind it (the page, a highlight, a shaded paragraph or a table cell), in a transparent colour, or in a font of 3 points or less. The agent is told how many characters were left out. Links are listed after the text, host first, as for email.

An answer holds at most 60,000 characters, so an agent reads a long document a page at a time: each page says which characters of the document's whole length it holds and where the next one starts. Each page is read from the document as it is at that moment.

### Sends that wait for you

Every send is a [call that waits for you](#calls-that-wait-for-you): the agent's call does nothing yet, and a row in **Needs you** shows everyone it reaches and every field exactly as it will go. Kivali builds what goes when the agent asks, and sends that, never anything read again from the agent or from Google. What each row shows:

- **An email**, titled such as "Send an email to alice@example.org and 2 more": From, To, Cc and Bcc by address, the subject, for a reply what it answers (its sender, when it was sent and its subject) and the message id it is threaded under, and the whole body as plain text.
- **A draft**, titled such as "Send a draft to alice@example.org": whether Kivali wrote it, and which agent; From, To, Cc and Bcc, each address with the name the draft gives it after it; Reply-To when set; the subject; the message it replies to; the body as plain text; and, for a draft written with Gmail's formatting, the formatted version as a reader sees it, its links listed and how many characters of hidden text it holds, which go with it. Its other headers, such as its date, go as the draft holds them and are not shown.
- **An invitation**, titled such as "Invite 3 people to "Sync"": the calendar, title, time, place, every attendee and the description, as the event will be made.
- **A change to an event**, titled such as "Change "Sync", telling 4 people": the event as it is (calendar, title, time, place, organizer and attendees), then each change: the new title, time, place or description (with the description before it), and who is added and removed.
- **A deletion**, titled such as "Delete "Sync", telling 4 people": the event as it is, and what happens: it is cancelled for everyone invited, or, for an event someone else organized, it leaves your calendar and Google tells the organizer you will not come.
- **An answer to an invitation**, titled such as "Answer yes to "Sync"": the calendar, title, time, organizer, your answer and the note.
- **A change on a shared calendar**, titled such as "Create "Focus" on a shared calendar", "Change "Sync" on a shared calendar" or "Delete "Sync" from a shared calendar": what a write's row would show, as for an invitation, a change or a deletion, then **Who can see this calendar**: each person by address, each group by address, "everyone at example.com", or "anyone, as the calendar is public". An invitation, a change or a deletion that is a send already, on a shared calendar, shows the same line. That list is read when the agent asks and shown on the card; a calendar shared with more people between then and your approval is still written as the card shows.
- **A change to a shared document**, titled such as "Add to "Plan", a shared document" or "Replace text in "Plan", a shared document": the document (its name and who owns it, or the shared drive it is in), **Who can see it** (people and groups by address, "everyone at example.com", with "with the link" when they must have it, "anyone with the link", or "anyone, as it is public on the web"), and the change: the text to add, as Kivali will write it, at the end of the document as it is when you approve, or what is found, what replaces it and whether capitals must match. **A document in a shared folder**, titled such as "Create "Plan" in a shared folder", shows the folder, who can see it, the title and the text. The list of who can see a document or folder is read when the agent asks and shown on the card; one shared with more people between then and your approval is still written as the card shows.
- **A comment**, titled such as "Comment on "Plan"": the document (its name, who owns it and whether it is shared), the text it quotes, the comment exactly and whom it mentions. **A reply**, titled such as "Reply to a comment on "Plan"", also shows the comment it answers, who wrote it and when.

A row ends with **Also reaches**, "People whose address Google does not show", when the send reaches people Kivali cannot name, which only an empty **May send to** allows.

When the agent's Google Workspace calls in the day before it asked returned mail, events or documents written outside your domain, a marked line at the top of the row says so, with how many such writers its calls returned and the latest, such as "Before asking, this agent read 3 messages, events or documents from outside your domain, the latest from alice@example.org." A message counts by its sender, an event by its organizer, creator and attendees, a document by its owner (one in a shared drive counts as from outside), and a comment by its author; someone whose address Google does not show reads as "an unknown sender". Text from outside can try to steer an agent, so read the row closely then.

**Approve** sends it at once, with the sign-in as it is then. If the agent's access no longer has the switch that lets it ask, such as **May send mail (each send waits for your approval)**, or the sign-in needs you, **Approve** says why and the row stays until you approve again or deny it. If Google finds then that it cannot be done, such as an event that changed after you saw it, or a draft, a document or a comment deleted meanwhile, nothing is done, and you and the agent are told why. If Google does not answer, it may still have been done, and the agent is told what to look at before asking again. **Deny** sends nothing; your note, with either answer, goes to the agent.

An agent can have five requests waiting at a time; a sixth is refused, and the agent is told to wait for your answers. A row nobody answers within a day ends with nothing sent, and so does every waiting row when you disable or disconnect Google Workspace, and every row of an agent you offboard. Waiting rows live only while Kivali runs: a restart drops them with nothing sent, and the agent, told that no word within a day means it was not done, hears nothing more.

### Not supported

- Moving mail to the trash or to spam, and deleting mail.
- Gmail's settings, such as filters and forwarding: Kivali never asks Google for them.
- Attachments on mail an agent sends, and sending a draft that has attachments, an inline image or more than one version of its text: send those from Gmail.
- Reading another person's calendar by its address, and changing events on calendars that are not your own.
- Deleting events Kivali did not create.
- Changing documents someone else owns, commenting on documents Kivali did not make, sharing documents, and moving them to the trash.
- Reading a document's headers, footers and footnotes: agents read its body.
- Google Sheets, Google Slides and Drive files other than Google Docs.
