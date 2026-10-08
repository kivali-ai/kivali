# Backup and restore

A backup is one zip file holding everything your team keeps. This page covers what is in it, how to take one, how to restore it, and how to move a team to a new machine.

## What a backup contains

Everything on the team's data volume:

- every agent's role, habits, memory, current chat and past chats, with their summaries;
- archived agents, in full;
- the handbook, project files, skills, and files attached to messages;
- everything agents published, and the knowledge graph with its version history;
- every assignment and its history;
- every message between agents and to you;
- your org's name and logo, the network list, auto-release, and the usage record.

What is left out:

- **Claude credentials.** Your Claude sign-in, whichever way you signed in, is not in the backup. After a restore, connect Claude again.
- Temporary files, and copies Kivali rebuilds on its own after a restore.

Each backup ends with a manifest listing every file with its size and checksum. A restore checks every file against it before it writes anything.

> [!WARNING]
> Backups are not encrypted. A backup holds everything your agents know and every file you gave them. Store it as carefully as the team itself.

## Take a backup

In Kivali, open **Org**, **Backup and restore**, and choose **Download a backup**.

The file goes straight to your browser's downloads, which show its progress. A large team takes several minutes. You can close the tab or keep working while it downloads. If a download stops partway, the browser marks it failed; start another.

If Kivali cannot read a file while writing the backup, the download fails rather than leaving the file out, so a backup that finished is complete.

Take a backup before you update a team, delete one, or move it.

## Restore a backup

Restore is offered only on a **fresh** team: one with no agents other than its Chief of Staff and no chat history yet. This way a restore can never overwrite a team in use.

1. Create a new team. In Kivali Desktop, use **New team…** and go through its four steps. On a self-hosted server, install fresh.
2. When the team opens to its setup screens, go to the first one, **Set up your org**. A team made in Kivali Desktop opens at step 3, the files step; choose **Back** until you reach step 1.
3. Choose **Restore from a backup**, and drop the `.zip` file.
4. When it says **Backup restored**, choose **Reload**.

The restored team picks up where the backup left off: its agents, chats, assignments, files and settings.

A restore needs room: the upload, and then everything it unpacks to, must fit on the team's disk with some to spare. If it does not, the restore is refused before anything is written.

## Move a team to a new machine

1. On the old machine, take a backup.
2. On the new machine, install Kivali Desktop and create a team. Sign in with the same Google account, and connect Claude.
3. Restore the backup, as above.
4. Check the restored team. Then delete the old team, or pause it so its agents do not keep working on the old machine.

The same steps move a team between Kivali Desktop and a self-hosted server, in either direction.

## If the restore is refused

The restore checks the whole archive before writing anything. It refuses:

- a file that is not a Kivali backup, or has no manifest;
- an archive whose files do not match the manifest (missing, extra, short or changed), which usually means the download was cut short;
- a team that is not fresh;
- a restore while an agent is in the middle of a turn. Wait for it to finish and try again.

The message says which. Nothing on the team has changed.
