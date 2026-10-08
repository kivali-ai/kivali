# Self-hosting

You can run a Kivali team on your own Kubernetes cluster with the Helm chart: the same server Kivali Desktop runs in its virtual machine, installed by you. This page covers what you need, installing, sign-in, connecting Claude, network policy and egress, upgrades, and backups.

A self-hosted team works like any other. You can open it in a browser, or from Kivali Desktop with **Connect to a team**.

## What you need

- **A Kubernetes cluster** you can install into with `helm` (version 3) and `kubectl`. Nodes run Linux on `amd64` or `arm64`.
- **A CNI that enforces NetworkPolicy.** The chart's policies are what keep agents from reaching the server's API. k3s enforces them out of the box.
- **A default StorageClass** with `ReadWriteOnce` volumes. The team's data volume is 10 GiB by default, and each agent gets a 2 GiB scratch volume.
- **One node with room for the whole team.** Each agent runs in its own pod, and those pods must run on the same node as the server, because they reach it through a socket on that node. The server requests 200m CPU and 1 GiB of memory and may use up to 12 GiB; each agent pod may use up to 8 GiB while it works (4 GiB for the agent, 4 GiB for its sandbox).
- **An https address**, if anyone will reach the team from another computer.
- **A Google account** to sign in with, and **Claude access**: a Claude subscription, an Anthropic Console account, or Claude on Amazon Bedrock, Google Vertex AI or Microsoft Foundry.

## Get the release

Each release on the [releases page](https://github.com/kivali-ai/kivali/releases) carries:

| Asset | What it is |
| --- | --- |
| `kivali-X.Y.Z.tgz` | The Helm chart. |
| `kivali-images-linux-amd64.tar.zst`, `kivali-images-linux-arm64.tar.zst` | The three container images, `kivali`, `kivali-egress-proxy` and `kivali-dev-shell`, tagged `vX.Y.Z`, in one compressed archive per architecture. |
| `THIRD_PARTY_LICENSES.txt`, `SOURCES.md` | The license notices of the third-party software in the release, and where to get the source code of the components whose licenses offer it. |

The images are not in a public registry. Load them where your cluster can pull them. On k3s, import them on the node:

```sh
zstd -dc kivali-images-linux-amd64.tar.zst | sudo k3s ctr images import -
```

On other clusters, load them with `docker load` (after decompressing with `zstd -d`), tag them for your registry, and push all three. Set `image.repository` and `egress.image.repository` to your registry paths. The server finds the `kivali-dev-shell` image next to `kivali` in the same registry path, with the same tag, so all three must be there.

## Install

The release must be named `kivali`; the chart refuses any other name.

**1. Create the namespace and the Secret.** The Secret holds what is specific to your install. Create it yourself, entering values interactively where your shell history matters:

```sh
kubectl create namespace kivali
kubectl -n kivali create secret generic kivali-secrets \
  --from-literal=owner-emails=you@example.com \
  --from-literal=session-key="$(openssl rand -hex 32)"
```

| Key | |
| --- | --- |
| `owner-emails` | Your Google account: the only account that can sign in. If you use more than one of your own accounts, list them comma-separated. |
| `session-key` | 32 random bytes, hex or base64url. Signs sign-in sessions; replacing it signs you out. |
| `google-oauth-client-id`, `google-oauth-client-secret`, `oauth-redirect-url` | Optional, all three together: your own Google OAuth client. See [Sign-in](sign-in.md#use-your-own-google-oauth-client). |

**2. Install the chart.**

```sh
helm install kivali kivali-X.Y.Z.tgz -n kivali \
  --set externalURL=https://kivali.example.com
```

Leave out `externalURL` if you will only reach the team at a loopback address (for example through `kubectl port-forward`). The chart's `values.yaml` is commented key by key; the settings that matter most are in [Configuration](configuration.md#helm-chart-values).

**3. Reach it.** The server is exposed as the Service `kivali`, a NodePort on port 30080 by default. Put your https front end (a reverse proxy, an ingress, Tailscale) in front of it at the address you set as `externalURL`, then open that address and sign in.

**4. Set up the team.** [Connect Claude](#connect-claude) first, so the Chief of Staff can start. The first visit opens setup: your org's name and logo, project files, and hiring your Chief of Staff. See [Getting started](getting-started.md#4-set-up-your-org).

## Sign-in

Out of the box, sign-in uses Google through the public client Kivali ships with. You register nothing. It accepts sign-ins at loopback addresses and at `externalURL`, so set `externalURL` to the https address you use. Only the accounts in `owner-emails` can sign in. See [Sign-in](sign-in.md).

## Connect Claude

Sign the server in once with Claude Code's own sign-in, by starting Claude inside the server container:

```sh
kubectl -n kivali exec -it deploy/kivali -c kivali -- claude
```

When Claude is not signed in, it opens its sign-in menu: a Claude subscription, an Anthropic Console account, and, under 3rd-party platform, Amazon Bedrock or Google Vertex AI. The sign-in is kept on the data volume, and every agent pod and the server's own calls use it, so the whole team bills one way. To switch to another account or another way of billing, run the same command and type `/login`. Agents switch to the new sign-in at the start of their next turn; a turn already running finishes on the old one.

To see how the server is signed in:

```sh
kubectl -n kivali exec deploy/kivali -c kivali -- claude auth status
```

The container has no AWS or Google Cloud CLI. For Bedrock, sign in with a Bedrock API key or an access key; for Vertex AI, with a service account key file stored under `/data/claude-home`, for example copied there with `kubectl cp`. Agent pods reach Bedrock and Vertex AI through the egress proxy, whose default list allows their regional endpoints and the AWS and Google token services (see below).

### Microsoft Foundry

Claude Code has no sign-in for Microsoft Foundry: it reads the `env` block of its settings file instead. Write it in the server container; the file is on the data volume, so every agent pod uses it too:

```sh
kubectl -n kivali exec -i deploy/kivali -c kivali -- sh -c \
  'umask 077; mkdir -p "$HOME/.claude"; cat > "$HOME/.claude/settings.json"' <<'EOF'
{
  "env": {
    "CLAUDE_CODE_USE_FOUNDRY": "1",
    "ANTHROPIC_FOUNDRY_RESOURCE": "my-resource",
    "ANTHROPIC_FOUNDRY_API_KEY": "<the resource's key>"
  }
}
EOF
```

This replaces the settings file; if it already holds settings, add the `env` entries to it instead. `ANTHROPIC_FOUNDRY_RESOURCE` is the resource's name, not its URL. To sign in with a service principal instead of a key, set `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` and `AZURE_CLIENT_SECRET` in place of `ANTHROPIC_FOUNDRY_API_KEY`; the service principal needs only the Azure AI User role on the resource.

Kivali names models by their ids, and on Foundry a model id names a deployment, so name each deployment after the model it runs: `claude-haiku-4-5`, `claude-sonnet-5`, `claude-opus-5-5` and `claude-fable-5-1`. A missing deployment shows up only when an agent uses that model. To check one:

```sh
kubectl -n kivali exec deploy/kivali -c kivali -- sh -c 'cd /tmp && claude -p "Reply with OK." --model claude-sonnet-5 --max-turns 1'
```

While these `env` entries are set, they outrank any other sign-in. To sign in another way, remove them first, then use `/login`. Agents use changed settings from their next turn. The same holds for the entries Claude's own Bedrock and Vertex AI sign-in writes there.

The server refuses to start with `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `CLAUDE_CODE_OAUTH_TOKEN` in its environment: agent pods share the sign-in, not the server's environment.

## Network policy and egress

The chart installs two NetworkPolicies, on by default (`networkPolicy.enabled`):

- **Agent pods** may reach only the egress proxy (which runs beside the server) and the cluster DNS. All of an agent's outbound traffic goes through the proxy, which allows only the hosts on your team's list in **Org**, **Network**. A new team's list allows Anthropic, the Amazon Bedrock, Google Vertex AI and Microsoft Foundry endpoints and their token services, and common package and code hosts. Add any others your agents need there.
- **The server's HTTP port** accepts traffic from outside the pod network only, so no pod can call the server's API.

The second policy also blocks in-cluster front ends, since they are pods too: k3s's Traefik ingress, cloudflared, the Tailscale operator. Admit yours with `networkPolicy.serverIngress.extraFrom`:

```yaml
networkPolicy:
  serverIngress:
    extraFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: kube-system
        podSelector:
          matchLabels:
            app.kubernetes.io/name: traefik
```

or turn that policy off with `networkPolicy.serverIngress.enabled=false`. Set `networkPolicy.podCIDR` to your cluster's pod network if it is not k3s's `10.42.0.0/16`.

> [!WARNING]
> The agent-pod policy is what keeps agents' shell commands away from the server's API, which acts as you. Keep it on, and run the chart on a cluster whose CNI enforces it.

## Upgrades

The chart has no upgrade hooks. A restarted server adopts the agent pods it finds, and agent pods left from the old server cannot reconnect, so delete them as part of every upgrade:

1. Take a [backup](backup-and-restore.md).
2. Load the new release's images, as above.
3. Delete the agent pods:

   ```sh
   kubectl -n kivali delete pod -l app=kivali-agentpod --wait
   ```

4. Upgrade the chart:

   ```sh
   helm upgrade kivali kivali-X.Y.Z.tgz -n kivali --reuse-values
   ```

If the upgrade did not restart the server (only values changed, same image tag), restart it with `kubectl -n kivali rollout restart deploy/kivali` after deleting the agent pods. The server recreates agent pods as agents are needed.

Read the release notes before upgrading.

## Backups

Take backups from the app: **Org**, **Backup and restore**, **Download a backup**. Restoring is offered on the first setup screen of a fresh install. See [Backup and restore](backup-and-restore.md).

Everything durable is on the PersistentVolumeClaim `kivali-data`. It is kept when you `helm uninstall` (`persistence.keepOnUninstall`), so uninstalling and reinstalling keeps the team. Delete the PVC yourself to remove the data.

## Single-user installs

`secrets.create=true` makes the chart create `kivali-secrets` from `secrets.*` values instead of a Secret you made. The values are then stored in the Helm release record, readable by anyone who can read Secrets in the namespace. Use it only on a machine where you are the only user.
