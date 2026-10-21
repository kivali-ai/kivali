# Integrations

An integration is an external service your agents can act on, such as a CRM, a blog or a ticket tracker. Each one is installed from a bundle, connected with a token you paste, and given to agents one at a time, each with its own access. This page covers installing one, connecting and testing it, giving agents access, what an agent then sees, the record of their calls, its log, and switching it off or removing it.

Integrations are under **Org**, **Integrations**.

## What an integration is

An integration gives agents **actions**: things they can do in the service, such as "create a blog post" or "list contacts". Its **connection** is how it signs in to the service: a token or key you copy from the service's settings, and sometimes a field or two more, such as an account name. Its **access** is what one agent may do with it, chosen on a form the integration describes: which blogs, which projects, whether it may publish.

An integration is installed one **version** at a time. Nothing in a version runs until you approve it. Once approved and switched on, it runs on its own, apart from Kivali and from your agents, and reaches only the hosts its install screen listed.

## Install an integration

An integration arrives as a bundle, a `.zip` file. In **Org**, **Integrations**, choose **Upload a bundle** and pick the file.

Kivali checks the bundle first: that it says what it is and what it reaches, that its form makes sense, and that its files are safe to unpack. A bundle that fails is not stored, and the dialog lists every problem for whoever built it. A version already installed cannot be uploaded again; a changed bundle needs a new version number.

A bundle that passes opens its **install screen**, which waits for your approval. It shows, in words:

- what the integration is, who built it, and who published this version;
- every host it reaches;
- every field its connection needs, with the secrets marked;
- every action it gives agents;
- what its access form asks, or that access is all or nothing;
- the bundle's size, and anything the check noticed.

Read it before you approve. The check only reads the bundle; this screen is the review. Choose **Approve and install** to make this version the one in use, or **Reject** to turn it down. A rejected version never runs.

### New versions

A newer version is uploaded the same way, and waits on its own install screen, which says which version it replaces. Until you approve it, the version in use stays. The integration's page lists every version with what happened to it.

## Connect it

On the integration's page, fill in the connection and choose **Save connection**. The form is the approved version's, so approve a version first.

Secret fields, such as tokens, are never shown again once saved: the page says only that they are set. To change one, type the new value; leave it empty to keep the one stored. They are kept outside your team's data volume, so [backups](backup-and-restore.md) never contain them. After a restore, an integration whose secrets are not there shows **Reconnect needed** until you enter them again.

Saving a connection restarts the integration with the new values, which takes a few seconds.

**Test connection** asks the integration to sign in to the service with the connection you saved, and shows what it found under the form: whether it worked, the integration's own words (such as the account it signed in as), and when you last tested. A failed test keeps the integration's reason, so you can see what to fix. Saving the connection again clears the last result.

**Disconnect** removes the connection and every agent's access, which was given for it.

### Not running

An integration that is approved and switched on shows **Not running** while it is starting, and when it has stopped. Starting takes a few seconds. While it is not running, **Test connection** and the options in an access form are not available.

If it stays **Not running**, open **Recent log** on its page: the integration's own messages and Kivali's about it say what happened, such as a server that did not start or stopped answering. Kivali starts a stopped integration again by itself, and restarts one that stops answering. If the log shows the integration failing each time it starts, the version itself is at fault: ask whoever built the bundle for a fixed version, or switch it off. On a team short of memory, the log says the team does not have enough memory for it; raise the team's memory in Kivali Desktop's settings, or ask whoever runs your server.

## Its log

**Recent log** on an integration's page lists the latest lines the integration wrote, oldest first: its own messages, the errors it printed, and Kivali's notes about it, such as when it started, stopped or was restarted. **Refresh** reads it again. Secret values of the connection never appear in it: Kivali replaces each one with `[secret]` before the line is kept. The log keeps the last few thousand lines and goes when the integration is removed.

## Give agents access

Under **Access**, choose an agent and **Give access**. The form is the integration's own: a list of blogs or projects to pick from, switches such as "May publish and edit", text fields. A list the integration fills in from the service, such as the blogs your connection can see, is loaded through the running integration with **Load options**, which waits while the integration is not running. An integration with no form gives an agent all of it or nothing. **Change** edits an agent's access and **Remove** takes it away.

Each agent's access is shown in words, on the integration's page and on the agent's **Integrations** tab. When a new version changes the form so that an agent's access no longer fits, the access is marked invalid and gives nothing until you set it again.

An agent that is offboarded loses its access to every integration.

## What an agent sees

An agent with access to a running integration has its actions as tools, named after the integration and the action: `hubspot_create_post` for HubSpot's "create post". It has only the actions its access lets the integration offer it, among those the install screen listed, and is told that you decide what it may do with each integration. An agent without access, or with access that no longer applies, has none of them.

Kivali checks every call against the agent's access before the integration sees it, and tells the integration which agent is calling and with what access. A call Kivali refuses (no access, the integration switched off or not running, an action the agent's access does not include) tells the agent why and to ask you. What comes back is the integration's text, with the connection's secrets replaced by `[secret]`; images and other content that is not text are not passed on in this release.

A change to an agent's access, given or taken away, changes its tools from its next turn. Access you take away is refused from the next call, even in the middle of a turn.

## Latest calls

**Latest calls** on an integration's page lists the latest calls through it, newest first: when, which agent, the action, whether Kivali let it through and, for a call that was refused or failed, why, and how long it took. Your own Test connection and the options an access form loads are in it too, under your name. Calls an agent made are listed whether Kivali allowed them or not.

## Switch it off or remove it

The switch at the top of the integration's page turns it off for every agent at once and stops it running, keeping the connection and everyone's access for when you turn it back on.

**Remove** deletes the integration: every version, the connection and its secrets, every agent's access and its log, and stops it running. Uploading a bundle installs it again from scratch. An integration that ships with Kivali cannot be removed; switch it off instead.
