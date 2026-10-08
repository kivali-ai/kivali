# Troubleshooting

Symptoms you may run into, what causes them, and how to fix them. If nothing here helps, save a diagnostics file in Kivali Desktop (**Settings**, **Advanced**, **Diagnostics**) and [open an issue](https://github.com/kivali-ai/kivali/issues). The diagnostics file holds logs and settings, with no team data and no sign-ins.

## Setting up

### "No model is connected yet" when hiring the Chief of Staff

The team is not signed in to Claude. Everything you entered is saved. In Kivali Desktop, open **Settings**, the team, then **AI**, and choose **Sign in…**. Then choose **Try again** on the setup screen.

On a self-hosted team, run `claude` in the server container and sign in. See [Self-hosting](self-hosting.md#connect-claude).

### "Not signed in yet: Terminal closed before Claude finished signing in"

Claude's sign-in runs in a terminal window, and the window closed before it finished. Choose **Open Claude sign-in again** and finish Claude's steps: for a subscription or Console account, open the link Claude shows, sign in, and paste the code back into the terminal window.

### Agents fail on Amazon Bedrock or Google Vertex AI

Claude signed in, but agents' calls fail. Check that the models are enabled for your AWS account or Google Cloud project in the region you chose, and that their hosts are still on **Org**, **Network**: see [Network access](security-and-privacy.md#network-access) for the list.

### "Couldn't finish signing in" during setup

The browser did not come back to Kivali. Choose **Continue with Google** again and keep the setup window open until it updates.

### The Chief of Staff could not start

The hiring screen says what happened and who can fix it. Most often the Claude connection is not working; fix it as above and choose **Try again**.

### The handbook draft has not arrived

If your files did not say enough, the Chief of Staff asks you a few questions first. Look for them in **Needs you**, answer, and the draft follows.

### A project file says "Stored only"

Kivali could not extract text from it. Agents can still see that it exists, and can view images. For documents, upload a PDF, Markdown or office file instead.

## Teams in Kivali Desktop

### A team couldn't start

Its window shows the reason and **Try again**. **Show details** shows what happened. If the reason is memory, Kivali offers to pause another team first. You can also lower other teams' memory in **Settings**, under each team's **This Mac** (**This PC**) tab.

### "Not enough memory" when resuming

Your computer does not have the team's memory free while another team runs. Choose to pause the other team, or lower this team's memory under **This Mac** (**This PC**) (it applies the next time the team resumes).

### "Claude isn't signed in", or a notification that a team needs you

Claude signed your account out, often after a password change or plan change. Open **Settings**, the team, **AI**, and choose **Sign in…**. An agent whose turn stopped while you were signed out shows **Needs help**; send it a message to resume (see below).

### A team stopped unexpectedly

Open its window and choose **Resume**. If it keeps stopping, look at **Help**, **Show logs**, and save a diagnostics file.

### A team update says to update the app first, or that it needs steps by hand

The new team version needs a newer Kivali app: update the app in **Settings**, **General**, then update the team. If it needs steps by hand, **What's new** describes them.

### A team couldn't update

Nothing is lost: the team went back to the version it was on and keeps running. Try again later from the team's **Overview**, or save a diagnostics file and report it.

### Another computer can't connect

- The team must be running and your computer awake.
- Your https address must be serving the team (for example, `tailscale serve` must be running).
- The address in **Other devices** must match the address the other computer uses.
- "This address answered, but it isn't a Kivali team": the address points somewhere else.
- "… can't sign in to …": only the team's owner can sign in. Use the owner's Google account. See [Sign-in](sign-in.md#who-can-sign-in).

## Signing in

### "This Google account is not this team's owner"

The account is not the team's owner. Sign in with the account that owns the team. On a self-hosted team, check that `owner-emails` holds the address Google shows.

### "Sign-in isn't available at …"

With Kivali's built-in sign-in, a team accepts sign-ins only at loopback addresses and at its external address. Open the team at one of those, or set the external address: **Other devices** in Kivali Desktop, `externalURL` on a self-hosted team.

### The browser did not send the sign-in cookie

The sign-in started at one address and Google returned to another, for example `localhost` and `127.0.0.1`, which browsers treat as different sites. Open the address the message names and sign in again. Behind a reverse proxy, make sure it passes the public host name in `Host` or `X-Forwarded-Host`.

### Google says `redirect_uri_mismatch`

Only with your own OAuth client. The `oauth-redirect-url` in the Secret must match a redirect URI registered on the client exactly: scheme, host, port and path. Behind a tunnel or reverse proxy, both must be the public address.

## Agents

### An agent sent a message, but the other agent never got it

Messages between agents wait in the **Queue** on Home until you release them. This is by design. See [Release the Queue](using-kivali.md#release-the-queue).

### An agent shows "Needs help"

Its last turn stopped on an error, and it will not start again on its own. Open its chat: a "Stopped on an error" marker says what happened, such as a usage limit, an expired Claude sign-in or a failed model call. Fix the cause (wait out the limit, sign in to Claude again), then send the agent any message. It resumes where it left off.

### An agent shows "Stopped after repeated failures"

Its runtime failed several times in a row, so Kivali stopped restarting it. Send it a message to start it again. If it fails again, pause and resume the team, and check the logs.

### An agent shows "Stopped by you"

You pressed **Stop**. The agent waits for your direction; send it a message to carry on.

### An agent shows "Not connected"

Kivali lost contact with the agent's sandbox. In Kivali Desktop, pause and resume the team. On a self-hosted team, delete the agent pods (`kubectl -n kivali delete pod -l app=kivali-agentpod`); Kivali recreates them.

### An agent can't reach a website or service

Agents can reach only the hosts on your team's list. Add the host in **Org**, **Network**. An entry covers itself and everything under it, so `example.com` is enough for `api.example.com`.

### "… is in the middle of a turn, so a new chat cannot start yet"

Wait for the agent to finish, or choose **Stop**, then start the new chat.

### Costs are higher than expected

Check **Org**, **Usage** for which agents spend most. The usual causes are releasing a lot at once (**Release all**, or a short auto-release delay), long chats (start new ones), large memories, and expensive models or high effort on routine work. See [Costs and usage](using-kivali.md#costs-and-usage).

## Backups

### The backup download failed

The browser marks a download that stopped partway as failed. Start another from **Org**, **Backup and restore**. If it fails every time, the message says why; check the team has free disk space.

### "Restore from a backup" is not offered

Restore is offered only on a fresh team, on the first setup screen. Create a new team and restore into it. See [Backup and restore](backup-and-restore.md#restore-a-backup).

## Self-hosted teams

### The server will not start: nobody could sign in

The server needs at least one owner. Add `owner-emails` to the `kivali-secrets` Secret and restart.

### Agents stop responding after the server restarted

Agent pods from before the restart cannot reconnect. Delete them, and the server recreates them as needed:

```sh
kubectl -n kivali delete pod -l app=kivali-agentpod
```

### The server is unreachable through an in-cluster ingress

The chart's network policy blocks traffic from pods, and ingress controllers are pods. Admit yours with `networkPolicy.serverIngress.extraFrom`. See [Self-hosting](self-hosting.md#network-policy-and-egress).

### Timestamps look wrong

Kivali files messages by date and checks the server's clock against outside time servers; the health endpoint `/healthz` reports the difference as `clock_drift_seconds`. Fix the host's clock and restart the server.
