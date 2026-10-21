# kivali Helm chart

The Kubernetes deployment of Kivali: the server Deployment (with its
egress-proxy sidecar), the two Services, the data PVC, the RBAC the server
needs to manage agent pods, browser pods and integration connections'
Secrets, and NetworkPolicies. `kivali-supervisor`
installs it into each org's VM as a k3s HelmChart
(`internal/supervisor/install.go`; its values are in
[supervisor.md, "Install values"](../../docs/developers/supervisor.md#install-values)), and
that is how every org is deployed
([deployment.md](../../docs/developers/deployment.md)). It also installs
standalone with helm; `examples/` holds a dev-style and a prod-style values
file for that. helm is a prerequisite for the make targets
(`brew install helm`).

This file is the chart's reference. The operator walkthrough for running
Kivali on your own Kubernetes cluster is
[docs/self-hosting.md](../../docs/self-hosting.md).

## Fixed names

The server hard-codes these, so the chart renders them regardless of release
name and **fails the render unless the release is named `kivali`**:

| Object | Name |
| --- | --- |
| Deployment (label `app=kivali`) | `kivali` |
| Services | `kivali`, `kivali-egress` |
| PVC | `kivali-data` |
| Secret | `kivali-secrets` |
| ServiceAccount | `kivali` |
| Role / RoleBinding | `kivali-agentpod-manager` |

The namespace comes from the release (`-n`). Agent pods are labelled
`app=kivali-agentpod` and browser pods `app=kivali-browserpod`; the
hostPath UDS dir defaults to `/var/run/kivali/uds`.

The Role `kivali-agentpod-manager` grants, in the release's namespace
only: get, list, create and delete on pods; get, list and create on
PVCs (no delete: an offboarded agent's PVC is kept); and get, create,
update and delete on Secrets, for the Secret each integration
connection keeps its secret fields in (`kivali-connection-<integration>-<id>`,
labelled `app=kivali` and `kivali.ai/integration=<name>`). Secrets have
no list or watch, so the server reads only Secrets it names. Kubernetes
cannot limit create to a name, so the grant covers every Secret in the
namespace, `kivali-secrets` included, which the server already has as
its environment.

## Install on a server

```sh
# 1. Secret, created by you (keys below). Sign-in goes through the
#    public Google client the server is built with; to use your own
#    client, add google-oauth-client-id, google-oauth-client-secret and
#    oauth-redirect-url (the redirect URI registered on it).
#    See docs/developers/auth.md.
kubectl create namespace kivali
kubectl -n kivali create secret generic kivali-secrets \
  --from-literal=owner-emails=you@example.com \
  --from-literal=session-key=$(openssl rand -hex 32)

# 2. Install (images kivali, kivali-egress-proxy, kivali-agent-shell and
#    kivali-browser must exist at the same tag; the server derives the
#    agent-shell image from AGENTPOD_IMAGE, so it has no value here).
helm install kivali charts/kivali -n kivali -f charts/kivali/examples/values-prod.yaml

# 3. Connect Claude: start Claude Code in the server container and pick a
#    sign-in from its menu (a Claude subscription, an Anthropic Console
#    account, Amazon Bedrock or Google Vertex AI; /login switches later).
#    It persists on the PVC (HOME=/data/claude-home) and every agent pod
#    shares it. `claude auth status --json` reports it. Microsoft Foundry
#    is an env block in $HOME/.claude/settings.json instead; see
#    docs/self-hosting.md.
kubectl -n kivali exec -it deploy/kivali -c kivali -- claude
```

The keys of `kivali-secrets`:

| Key | |
| --- | --- |
| `owner-emails` | the owner's Google account(s), comma-separated, compared case-insensitively; the only accounts that can sign in. The server refuses to boot without it |
| `session-key` | 32 random bytes, hex or base64url; signs session cookies (replacing it ends every session) |
| `google-oauth-client-id`, `google-oauth-client-secret`, `oauth-redirect-url` | optional, together: your own Google client, and the public URL plus `/auth/callback` registered on it |

They live in the Secret rather than in values because they are specific
to the install: a literal in a shared values file is a placeholder that
an upgrade writes over the real value. Enter them interactively rather
than with `--from-literal=` where the shell history matters.

`secrets.create=true` makes the chart create `kivali-secrets` from `secrets.*`
values. That is for single-user installs (the supervisor's VM) only: the
values are stored in the Helm release record.

`devMode=true` turns authentication off (`DEV_MODE=true`,
`DEV_MODE_ALLOW_NONLOOPBACK=true`, because a pod cannot bind loopback) and
drops the OAuth and owner env vars. The server refuses it with
`kivaliEnv: prod`; the chart does too.

## Values

See `values.yaml`, which is commented key by key. Defaults are the prod
numbers (server 200m/1Gi requests, 1000m/12Gi limits; egress 20m/32Mi,
200m/128Mi). The dev example (`examples/values-dev.yaml`) uses
`server.resources` requests 200m/512Mi, limits 1000m/8Gi, `:dev` tags,
`kivaliEnv: dev`, `devMode: true`.

## Browser values

The server starts one browser pod per agent web session, from
`browser.image` (`BROWSER_IMAGE`; an empty `repository` means
`kivali-browser` beside the server image in `image.repository`'s path, an
empty `tag` means the server image's, `image.tag` or else the chart's
`appVersion`), and deletes it when the
browser is done. `browser.resources` sizes each pod's container (default
100m/512Mi requests, 2/2Gi limits; `BROWSER_CPU_REQUEST`,
`BROWSER_MEMORY_REQUEST`, `BROWSER_CPU_LIMIT`, `BROWSER_MEMORY_LIMIT`).
`browser.maxPerTeam` (`BROWSER_MAX_PER_TEAM`, default 3, at least 1) is
how many browsers the team's agents may have running at once. Browser pods
run on the server's node (they reach it over the hostPath socket), keep
nothing on a volume, and reach the public web directly, with no proxy, and
nothing in the cluster: a browser is not held to the egress allowlist
(see NetworkPolicy below).

## Sign-in values

`oauth.redirectURL` makes `OAUTH_REDIRECT_URL` a literal instead of the
`oauth-redirect-url` Secret key. Use it only where the address is the same
for everyone (a loopback address); a literal in a shared values file is
otherwise a placeholder that an upgrade writes over the operator's real
hostname. `OWNER_EMAILS` has no literal form for the same reason. With the
public client and neither the value nor the Secret key set, each sign-in
derives the callback, but only for loopback (`127.0.0.1`, `localhost`,
`[::1]`) or `externalURL` (`KIVALI_EXTERNAL_URL`, an https origin such as
`https://kivali.example.com`), so an install reached at any other address
through the public client sets `externalURL`. `oauth.publicClientID`, `oauth.relayURL`, `oauth.authURL`,
`oauth.tokenURL`, `oauth.userinfoURL`, `oauth.jwksURL` and
`oauth.issuer` set the public-client override, the relay, the provider
endpoints and where id_tokens are verified; see
[auth.md](../../docs/developers/auth.md).

Without `secrets.create`, the Secret `kivali-secrets` is created and edited
by the operator, outside the chart (keys above).

## NetworkPolicy

Agents run shell commands in pods. These policies keep agent and browser
pods off the cluster, the node and cloud metadata, and hold an agent's
traffic to the egress proxy. The HTTP API is not open to pods by default:
every route needs a Google session (`internal/web/web.go`), and the only
routes open without one are login, the OAuth callback and the
loopback-only, signed, single-use Desktop handoff
([auth.md](../../docs/developers/auth.md)). All three policies sit under
`networkPolicy.enabled` (default true):

1. `kivali-agentpod-egress` (load-bearing): agent pods (`app=kivali-agentpod`)
   get egress default deny, allowing only TCP 3128 to pods labelled
   `app=kivali` (the proxy sidecar lives in the server pod). No DNS: the
   server resolves the proxy Service to its cluster IP when it creates a
   pod, and a client that honours the proxy hands it host names. Agent pods
   reach core over the hostPath Unix socket. The proxy holds the agent's
   shell (the shell's own per-pod credential, or none) to the team's host
   list and that agent's own hosts, and lets the agent's own fetches and
   model calls (a second per-pod credential) reach any public host; an agent
   whose network limits the owner has removed reaches any public host with
   either, uncounted. The
   proxy refuses, for every agent request, loopback, link-local and cloud
   metadata, `networkPolicy.podCIDR`, `networkPolicy.serviceCIDR` and the
   node's own address (`status.hostIP`, passed as `--deny-cidr`), and, for
   hosts not on the list, private ranges. The proxy container also takes
   `--agents` (`/etc/egress/agents.json`, written by the server from
   `EGRESS_AGENTS_SYNC_PATH`) and its budget flags, whose defaults the chart
   keeps: 32 KB per connection, 256 KB per 10 minutes and 4 MB a day, counted
   and logged without refusing (`--enforce-budget` is off). See
   [architecture.md](../../docs/developers/architecture.md#the-egress-proxy-and-the-fence).
2. `kivali-browserpod-egress` (load-bearing): browser pods
   (`app=kivali-browserpod`) get egress to `0.0.0.0/0` except the ranges
   in `networkPolicy.browserEgress.except`, and TCP/UDP 53 to CoreDNS in
   `kube-system`; no ingress at all (`policyTypes` names Ingress with no
   rules). A browser browses the web for a web subagent directly, with no
   proxy, and is not held to the egress allowlist; the exceptions keep
   Chromium, and any page that drives it, off the pod network
   (`networkPolicy.podCIDR`, always excepted), the node, cloud metadata
   (the link-local range and Azure's WireServer `168.63.129.16/32`) and
   every private range. The default list is `10.0.0.0/8`,
   `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16`, `127.0.0.0/8`,
   `100.64.0.0/10` and `168.63.129.16/32`; a cluster whose nodes or
   services sit elsewhere adds their ranges. A browser reaches core over
   the hostPath Unix socket, not the network. Core holds what a web task
   can send out (approval for uploads, a budget for the rest), and the
   egress proxy refuses the same ranges for the agent's own fetches
   (`--open-deny-cidr`).
3. `kivali-server-ingress` (defence in depth): the server's HTTP port 8080
   accepts only `0.0.0.0/0` except `networkPolicy.podCIDR`; port 3128
   accepts only pods in the namespace. This is defence in depth, because
   the API checks a session on every request. On k3s, kube-router evaluates policy
   before kube-proxy's SNAT and accepts traffic from the local node ahead of
   every pod chain, so kubelet probes and host-forwarded NodePort traffic
   are admitted, while a pod's request to the NodePort is dropped with its
   real pod address. `podCIDR` defaults to k3s's `10.42.0.0/16`; set it
   to the cluster's pod network elsewhere, and `serviceCIDR` (default k3s's
   `10.43.0.0/16`) to its Service network (`examples/values-dev.yaml` sets
   `10.244.0.0/16` and `10.96.0.0/12`).

   The cost: any in-cluster front end (k3s's Traefik ingress, cloudflared,
   Tailscale operator pods) is a pod and is blocked on a server install.
   Admit it with `networkPolicy.serverIngress.extraFrom` (NetworkPolicyPeer
   entries appended to the 8080 rule), or drop the policy with
   `networkPolicy.serverIngress.enabled=false`:

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

k3s enforces these (embedded kube-router controller), so they hold in
every supervisor-run org. A CNI that does not enforce NetworkPolicy
(kindnet, for one) accepts the objects and enforces nothing.

## The supervisor's VM

The VM's HelmChart `valuesContent` sets `initImage` to the busybox helper
that k3s bakes into its airgap images:
`docker.io/rancher/mirrored-library-busybox:<tag>`, where `<tag>` is the one
in that k3s release's `k3s-images.txt`. The default
`docker.io/library/busybox:1.36` is not baked, and the VM's first boot is
offline, so the init container would never pull.

## Upgrades

The chart has no hooks. The server adopts existing agent pods at boot, and
an agent pod wedged to a dead `core.sock` never reconnects, so delete them:

```sh
kubectl -n kivali delete pod -l app=kivali-agentpod --wait
```

The supervisor does this itself: `upgrade` deletes them before it stops
the VM, and `load-images` and `restart` before they restart the server. A
standalone install does it by hand:

- Prod-style installs (tag moves each release): run it **before**
  `helm upgrade`, so no agent pod outlives the boot of the new server.
  This only works when the upgrade restarts the server. If the server pod
  did not restart (same image tag, only values changed), run
  `kubectl rollout restart deploy/kivali` afterwards, because the server
  has no reconcile loop and adopts nothing that is gone.
- Dev-style installs (mutable `:dev` tag, so the upgrade changes nothing
  until the pod restarts): run it **after** `helm upgrade`, then
  `kubectl rollout restart deploy/kivali`.

The release that gives agent pods the proxy by IP changes every pod's
spec, so the server recreates each agent pod once on that upgrade. The
egress proxy reloads its agents file when a pod registers, so a recreated
pod needs no further step.

A change to a chart-managed Secret rolls the server pod (checksum
annotation); cycle agent pods the same way.

## Packaging

```sh
make chart-package VERSION=vX.Y.Z  # dist/kivali-X.Y.Z.tgz, chart version and appVersion = VERSION
```

The release workflow attaches the packaged chart to the GitHub release,
where the supervisor's upgrade fetches it (`release.json`). There is no
chart registry; a standalone install uses the directory or the `.tgz`. `helm lint` always uses the release name `test-release`,
which the name check lets through (and only that name besides `kivali`) so
lint renders every template.
