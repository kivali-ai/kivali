# Self-hosting

You can run a Kivali team on your own Kubernetes cluster with the Helm chart: the same server Kivali Desktop runs in its virtual machine, installed by you. This page covers what you need, installing, sign-in, connecting Claude, network policy and egress, upgrades, and backups.

A self-hosted team works like any other. You can open it in a browser, or from Kivali Desktop with **Connect to a team**.

## What you need

- **A Kubernetes cluster** you can install into with `helm` (version 3) and `kubectl`. Nodes run Linux on `amd64` or `arm64`.
- **A CNI that enforces NetworkPolicy.** The chart's policies are what keep agents from reaching the server's API. k3s enforces them out of the box.
- **A default StorageClass** with `ReadWriteOnce` volumes. The team's data volume is 10 GiB by default, and each agent gets a 2 GiB scratch volume.
- **One node with room for the whole team.** Each agent runs in its own pod, and those pods must run on the same node as the server, because they reach it through a socket on that node. The server requests 200m CPU and 1 GiB of memory and may use up to 12 GiB; each agent pod may use up to 8 GiB while it works (4 GiB for the agent, 4 GiB for its sandbox); each browser a web task runs requests 512 MiB and may use up to 2 GiB. A web task whose browser no node has the memory for ends after about 90 seconds, saying the team does not have enough memory for it.
- **A namespace without the `restricted` Pod Security Standard.** Agent pods and browser pods set no seccomp profile (each runs as a non-root user with every capability dropped), so Pod Security Admission at the `restricted` level refuses them; `baseline` admits them. On a cluster whose default seccomp profile or kernel blocks unprivileged user namespaces, Chromium cannot start its own sandbox and runs pages without it; the server logs `running (sandbox: off)` for that browser.
- **An https address**, if anyone will reach the team from another computer.
- **A Google account** to sign in with, and **Claude access**: a Claude subscription, an Anthropic Console account, or Claude on Amazon Bedrock, Google Vertex AI or Microsoft Foundry.

## Get the release

Each release on the [releases page](https://github.com/kivali-ai/kivali/releases) carries:

| Asset | What it is |
| --- | --- |
| `kivali-X.Y.Z.tgz` | The Helm chart. |
| `kivali-images-linux-amd64.tar.zst`, `kivali-images-linux-arm64.tar.zst` | The four container images, `kivali`, `kivali-egress-proxy`, `kivali-agent-shell` and `kivali-browser`, tagged `vX.Y.Z`, in one compressed archive per architecture. |
| `THIRD_PARTY_LICENSES.txt`, `SOURCES.md` | The license notices of the third-party software in the release, and where to get the source code of the components whose licenses offer it. |

The images are not in a public registry. Load them where your cluster can pull them. On k3s, import them on the node:

```sh
zstd -dc kivali-images-linux-amd64.tar.zst | sudo k3s ctr images import -
```

On other clusters, load them with `docker load` (after decompressing with `zstd -d`), tag them for your registry, and push all four. Set `image.repository` and `egress.image.repository` to your registry paths. The server finds the `kivali-agent-shell` and `kivali-browser` images next to `kivali` in the same registry path, with the same tag, so all four must be there.

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

The chart installs three NetworkPolicies, on by default (`networkPolicy.enabled`):

- **Agent pods** may reach only the egress proxy (which runs beside the server), by its address. They have no DNS. All of an agent's outbound traffic goes through the proxy, which treats two kinds of traffic differently:
  - **The agent's shell** (`curl`, `pip`, `git`, `apt` and any other command) reaches only the hosts on your team's list in **Org**, **Network**. A new team's list allows Debian's package servers, PyPI, GitHub and the Go module proxy, and no model provider host. A team created earlier keeps its list, which still holds the Anthropic, Amazon Bedrock, Google Vertex AI and Microsoft Foundry hosts; a shell does not need them, so you can remove them. Add any other hosts your agents need.
  - **The agent's own fetches and model calls** reach any public host, with a credential only the agent's process holds. They never reach the pod network, the Service network (`networkPolicy.serviceCIDR`), the node's own address, loopback, link-local addresses and cloud metadata, private ranges, or the ranges in `networkPolicy.browserEgress.except`, except a host that is on your list. The proxy counts what each agent sends out this way; see [Security and privacy](security-and-privacy.md#network-access).
- **Browser pods**, which run a web subagent's browser, reach the public internet directly and the cluster DNS, and nothing else: not the pod network, the node, cloud metadata or any private range (`networkPolicy.browserEgress.except`). A browser is not held to your team's network list and does not use the proxy; an upload from it needs your approval and stays on one site, and what it types counts against its budget.
- **The server's HTTP port** accepts traffic from outside the pod network only. Every request to the server needs a signed-in session anyway; this keeps pods from even trying.

The third policy also blocks in-cluster front ends, since they are pods too: k3s's Traefik ingress, cloudflared, the Tailscale operator. Admit yours with `networkPolicy.serverIngress.extraFrom`:

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

or turn that policy off with `networkPolicy.serverIngress.enabled=false`. Set `networkPolicy.podCIDR` to your cluster's pod network if it is not k3s's `10.42.0.0/16`, and `networkPolicy.serviceCIDR` to its Service network if it is not k3s's `10.43.0.0/16` (kind uses `10.244.0.0/16` and `10.96.0.0/12`). The proxy refuses both for every agent request, so a wrong value leaves a Service such as the Kubernetes API reachable from an agent's own fetches.

> [!WARNING]
> The agent-pod policy is what keeps an agent's commands on the egress proxy and off your cluster, your nodes and the open internet. Keep it on, and run the chart on a cluster whose CNI enforces it.

The list and the proxy's budget make it harder for an agent to send data out, not impossible. Read [what the network limits do not stop](security-and-privacy.md#what-this-does-not-stop). If you need a hard limit, enforce it in your own network.

## Upgrades

The chart has no upgrade hooks. A restarted server adopts the agent pods it finds, and agent pods left from the old server cannot reconnect, so delete them as part of every upgrade:

1. Take a [backup](backup-and-restore.md).
2. Load the new release's images, as above.
3. Delete the agent pods (the first upgrade to a release that changes the proxy's address recreates each one in any case):

   ```sh
   kubectl -n kivali delete pod -l app=kivali-agentpod --wait
   ```

4. Upgrade the chart:

   ```sh
   helm upgrade kivali kivali-X.Y.Z.tgz -n kivali --reset-then-reuse-values
   ```

   `--reset-then-reuse-values` (Helm 3.14 or later) starts from the new chart's defaults and applies the values you set on earlier installs and upgrades over them. Do not use `--reuse-values`: it keeps the old release's values whole and skips the new chart's defaults, so a value the new chart added is missing and the upgrade fails. With an older Helm, repeat the flags you installed with instead (for example `--set externalURL=https://kivali.example.com`).

If the upgrade did not restart the server (only values changed, same image tag), restart it with `kubectl -n kivali rollout restart deploy/kivali` after deleting the agent pods. The server recreates agent pods as agents are needed.

Read the release notes before upgrading.

## Backups

Take backups from the app: **Org**, **Backup and restore**, **Download a backup**. Restoring is offered on the first setup screen of a fresh install. See [Backup and restore](backup-and-restore.md).

Everything durable is on the PersistentVolumeClaim `kivali-data`. It is kept when you `helm uninstall` (`persistence.keepOnUninstall`), so uninstalling and reinstalling keeps the team. Delete the PVC yourself to remove the data.

## Single-user installs

`secrets.create=true` makes the chart create `kivali-secrets` from `secrets.*` values instead of a Secret you made. The values are then stored in the Helm release record, readable by anyone who can read Secrets in the namespace. Use it only on a machine where you are the only user.
