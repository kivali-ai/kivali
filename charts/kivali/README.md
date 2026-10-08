# kivali Helm chart

The Kubernetes deployment of Kivali: the server Deployment (with its
egress-proxy sidecar), the two Services, the data PVC, the RBAC the server
needs to manage agent pods, and NetworkPolicies. `kivali-supervisor`
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
`app=kivali-agentpod`; the hostPath UDS dir defaults to `/var/run/kivali/uds`.

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

# 2. Install (images kivali, kivali-egress-proxy and kivali-dev-shell must
#    exist at the same tag; the server derives the dev-shell image from
#    AGENTPOD_IMAGE, so it has no value here).
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

Agents run shell commands in pods, and the HTTP API trusts whoever reaches
it as the owner. A pod must therefore not be able to call it. Both policies
sit under `networkPolicy.enabled` (default true):

1. `kivali-agentpod-egress` (load-bearing): agent pods (`app=kivali-agentpod`)
   get egress default deny, allowing only TCP 3128 to pods labelled
   `app=kivali` (the proxy sidecar lives in the server pod) and TCP/UDP 53
   to CoreDNS in `kube-system`. Agent pods reach core over the hostPath Unix
   socket, and every outbound request of the CLI and the dev-shell already
   goes through the proxy via `HTTP_PROXY`.
2. `kivali-server-ingress` (defence in depth): the server's HTTP port 8080
   accepts only `0.0.0.0/0` except `networkPolicy.podCIDR`; port 3128
   accepts only pods in the namespace. On k3s, kube-router evaluates policy
   before kube-proxy's SNAT and accepts traffic from the local node ahead of
   every pod chain, so kubelet probes and host-forwarded NodePort traffic
   are admitted, while a pod's request to the NodePort is dropped with its
   real pod address. `podCIDR` defaults to k3s's `10.42.0.0/16`; set it
   to the cluster's pod network elsewhere (`examples/values-dev.yaml`
   sets `10.244.0.0/16`).

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
