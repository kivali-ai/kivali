# Integrations

An integration is an external service your agents can act on, such as a CRM, a blog or a ticket tracker. Each one is installed from a bundle, connected with a token you paste, and given to agents one at a time, each with its own access. This page covers installing one, connecting it, giving agents access, and switching it off or removing it.

Integrations are under **Org**, **Integrations**.

## What an integration is

An integration gives agents **actions**: things they can do in the service, such as "create a blog post" or "list contacts". Its **connection** is how it signs in to the service: a token or key you copy from the service's settings, and sometimes a field or two more, such as an account name. Its **access** is what one agent may do with it, chosen on a form the integration describes: which blogs, which projects, whether it may publish.

An integration is installed one **version** at a time. Nothing in a version runs until you approve it.

> [!NOTE]
> In this release, agents cannot call an integration's actions yet, and **Test connection** says the integration is not running yet. You can install, connect and give access now; the actions follow in a later release.

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

**Disconnect** removes the connection and every agent's access, which was given for it.

## Give agents access

Under **Access**, choose an agent and **Give access**. The form is the integration's own: a list of blogs or projects to pick from, switches such as "May publish and edit", text fields. An integration with no form gives an agent all of it or nothing. **Change** edits an agent's access and **Remove** takes it away.

Each agent's access is shown in words, on the integration's page and on the agent's **Integrations** tab. When a new version changes the form so that an agent's access no longer fits, the access is marked invalid and gives nothing until you set it again.

An agent that is offboarded loses its access to every integration.

## Switch it off or remove it

The switch at the top of the integration's page turns it off for every agent at once, keeping the connection and everyone's access for when you turn it back on.

**Remove** deletes the integration: every version, the connection and its secrets, and every agent's access. Uploading a bundle installs it again from scratch. An integration that ships with Kivali cannot be removed; switch it off instead.
