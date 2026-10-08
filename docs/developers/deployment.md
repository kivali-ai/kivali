# Deployment: the dev team, releases and upgrades

How orgs are deployed, from a developer's side. For running Kivali on
your own Kubernetes cluster see [../self-hosting.md](../self-hosting.md);
for backups as a user sees them, [../backup-and-restore.md](../backup-and-restore.md).

Every Kivali org is run by `kivali-supervisor` ([supervisor.md](supervisor.md)):
it boots a VM with k3s and installs the Helm chart `charts/kivali` into
it as a k3s HelmChart (`internal/supervisor/install.go`). Kivali Desktop
drives it, one org per team; from a shell it is `bin/kivali-supervisor`
(`make supervisor`). Upgrades come from the release feed
(`kivali-supervisor upgrade`, or Update in the desktop app). There is no
other deploy path.

The supervisor runs orgs on macOS (Apple Silicon, Virtualization.framework).
A Windows backend (one Hyper-V VM per team, through a privileged broker)
builds and passes its tests, but no release ships it; see
[supervisor.md](supervisor.md), "Platform boundary".

If you only want to try Kivali, don't start here. Use `make run`
([README.md](README.md)).

---

## The org

The supervisor installs the chart as the release `kivali` in the
namespace `kivali` (the chart refuses any other release name: the server
hard-codes the names of the Deployment, Services, PVC, Secret and
ServiceAccount). The values it installs with are in
[supervisor.md](supervisor.md), "Install values": the owner (`up --owner`) and a generated session key go into the `kivali-secrets`
Secret the chart creates (`secrets.create`); they live only on the org's
data disk. The data PVC `kivali-data` carries `helm.sh/resource-policy:
keep`. `make helm-lint` and `make helm-template` check the chart and
render its examples offline; the chart itself is documented in
[charts/kivali/README.md](../../charts/kivali/README.md).

**NetworkPolicy.** The chart ships two policies (agent pods may reach
only the egress proxy and DNS; the server's HTTP port refuses pod
traffic), and k3s enforces them (kube-router is built in).

An org runs three images, all sharing the release tag:

| Image | Role | Built by |
| --- | --- | --- |
| `kivali` | The web pod *and* every per-agent agent pod (via the `kivali agent` subcommand) | `make images` |
| `kivali-egress-proxy` | Per-pod egress allowlist enforcement | `make images` |
| `kivali-dev-shell` | The bash sidecar in each agent pod | `make images` |

The chart's `appVersion` is the default tag of all three; the dev-shell
tag is derived at runtime from the main image tag.

**Architectures.** All three images build for `linux/amd64` and
`linux/arm64`. `make images` builds all three for the host's own
architecture: with `ENGINE=vm` (the default on macOS and Windows) in
the build VM, BuildKit inside a Kivali VM on this machine
(scripts/build-vm.sh; no Docker), straight into that VM's containerd,
where `make -C vm dev-images` exports them for baking and `make
dev-load` runs them; with `ENGINE=docker` (the default on Linux) into
the local Docker. `make release-assets VERSION=vX.Y.Z` builds every
architecture in `RELEASE_ARCHES` (default `amd64 arm64`) with Docker's
buildx into the release's image bundles in `dist/`; it is Docker only.
Nothing pushes. A non-native architecture builds under QEMU, which is
slow for the runtime stages' apt installs; the Go and web stages
cross-compile natively either way. `REGISTRY=ghcr.io/kivali-ai/`
prefixes all three names; empty, the tags are the bare local names.

**The dev-shell toolkit.** The dev-shell's default packages are small
and company-agnostic: bash, coreutils, ca-certificates, git, python3
with pip and venv, jq, ripgrep, fd-find, curl, wget, tar, zip, unzip,
less, pandoc and imagemagick. There is no compiler and no
ffmpeg, database client or Go. To bake others in, name them in the
`DEV_SHELL_EXTRA_PACKAGES` build arg (space-separated apt packages):

```
make images DEV_SHELL_EXTRA_PACKAGES="ffmpeg postgresql-client"
make images DEV_SHELL_EXTRA_PACKAGES="build-essential golang-go ffmpeg"
```

The image records what it installed in `/etc/kivali/dev-shell-packages`
(one package per line); the dev-shell daemon serves that list, and each
agent's `run_shell` tool description is built from it, so agents are
told exactly what is installed.

**Set it once per machine.** `make dev-vm`, `make dev-load` and the
release bundles all build the dev-shell, and a build with no
extras is the slim default, so a dev team that relies on extra tools
would lose them silently on the next load. Put the settings in
`local.mk` at the repo root; the Makefile includes it first and it is
gitignored:

```
# local.mk
DEV_SHELL_EXTRA_PACKAGES = ffmpeg postgresql-client build-essential golang-go libssl-dev libffi-dev libsqlite3-dev
```

`make images` prints the extras it bakes (or `none`), so a
build never drops tools quietly. The release workflow builds a
release's images without extras.

`REGISTRY` can go in the same file, but it must stay empty for the dev
targets: the VM bakes and `load-images` imports the bare `:dev` names,
and the chart references bare image names.

## The dev team

The dev team is a team like any other, run by the supervisor on this
machine from a config directory of its own, `DEV_CONFIG_DIR` (default
`~/.kivali-dev`), so it never mixes with the teams Kivali Desktop runs.
Every target below builds `bin/kivali-supervisor` first and runs it with
`--config-dir $(DEV_CONFIG_DIR)`; the rest is the supervisor's own CLI
with that flag (`sup` below stands for
`bin/kivali-supervisor --config-dir ~/.kivali-dev`).

**Prerequisites:** macOS on Apple Silicon (Go with cgo) or Windows with
Hyper-V and the broker installed, and `helm` (`brew install helm`; point
`HELM=/path/to/helm` at the binary if it is not on `PATH`). No Docker:
images build in a Kivali VM (`scripts/build-vm.sh`), which boots the VM
image in `vm/build/out` or, before there is one, the installed Kivali
Desktop's.

```
make dev-vm                          # once: the three :dev images and a dev chart, baked into vm/build/out
make dev-up DEV_OWNER=you@gmail.com  # boot; the first run installs the team for that Google account
make dev-load                        # the loop: rebuild the :dev images and import them
sup terminal                         # Claude Code in the server container
sup down --exit                      # stop the VM; its data stays
sup destroy --yes --exit             # delete the team and its data disk
```

- **`dev-vm`** builds the `:dev` images and the dev chart
  (`dist/dev/kivali-0.0.0-dev.tgz`) and bakes both into the
  VM image in `vm/build/out`. Run it again when the chart or the VM
  changes.
- **`dev-up`** recovers any upgrade journal, boots the VM, forwards the
  team's port to `127.0.0.1` and, the first time, installs the team with
  `DEV_OWNER` as its owner, the Google account that signs in. It returns
  once `/readyz` answers. The port is 8080 unless that is taken, in which
  case it takes a free one and records it; `bin/kivali-supervisor
  --config-dir ~/.kivali-dev status` shows the URL. `bin/kivali-supervisor
  --config-dir ~/.kivali-dev handoff` prints a one-time token that signs
  the owner in at `http://127.0.0.1:<port>/auth/handoff?t=<token>`
  within 2 minutes.
- **`dev-load`** is the iteration loop: it rebuilds the three `:dev`
  images inside the dev team's own VM, straight into the containerd the
  team runs on, then `restart` deletes the agent pods, restarts the
  server and waits for the rollout.
- **`sup terminal`** opens Claude Code in the server container; its first
  run asks you to log in (a Claude subscription, an Anthropic Console
  account, Amazon Bedrock or Google Vertex AI; `/login` again later), and
  the login lands on the data volume where the server's and every agent
  pod's runs of the CLI find it. That is the only way the dev team gets a
  credential; `sup credential` prints what `claude auth status` reports.
  `terminal --shell` gives bash instead.
- **`sup destroy --yes --exit`** stops the VM and deletes the team and
  its data disk; the next `dev-up` installs a fresh one. It is the fresh
  start. Back up first if the data matters (`sup backup --out
  ~/dev-team.zip`, then `restore --in` into the fresh team).

The chart is installed once, by the first `dev-up`; `dev-load` replaces
images only. To run a changed chart, `make dev-vm`, then `sup destroy
--yes --exit` and `make dev-up` (with a backup and restore around it if
the data matters).

`make run` is the other local option: a team with no VM, no auth and
no agent pods.

## Cutting a release

```
make release VERSION=vX.Y.Z
```

`scripts/release.sh` bumps `appVersion` in `charts/kivali/Chart.yaml`
(the default image tag) and the Kivali Desktop version files with it
(`make version-check` asserts they agree), commits with a templated
message, creates an annotated tag at that commit, pushes both to origin,
and packages the chart into `dist/kivali-<version>.tgz` (`make
chart-package VERSION=vX.Y.Z` does the same alone). It does **not**
build or push images. Pushing the tag starts the release workflow, which
publishes the GitHub release: the image bundles, the chart,
`release.json` (the supervisor's feed), the VM image, the signed desktop
app and its update feed. The full procedure, its secrets and rollback
are in [releasing.md](releasing.md).

Guards before it will proceed: VERSION is semver, branch is `main`, the
tree is clean, `origin/main` has nothing local `main` lacks, the tag
doesn't already exist, the chart has the line it expects, every
versioned file (chart, desktop) agrees before the bump, and the
desktop updater's public key is not the development key
(`scripts/check-updater-key.sh`).

`VERSION` must be passed as `key=value`. `make release v0.2.5` is
rejected loudly, because VERSION would otherwise silently fall back to
`git describe` and ship the wrong thing.

## Upgrading an org

> Some releases require work the app cannot do. Such a release is
> marked `manual_steps` in its `release.json`, and the supervisor does
> not upgrade to it (the app shows "needs steps the app can't do yet");
> its release notes say what.

`kivali-supervisor check` reads the feed; `kivali-supervisor upgrade`
(Update in Kivali Desktop) takes its release. The upgrade downloads and
verifies the release's chart and image bundle, imports the images,
deletes the agent pods, stops the VM cleanly, snapshots the data disk,
boots, rewrites the HelmChart to the new chart and image tags, and waits
for the rollout and `/readyz`. A failure rolls back to the snapshot. The
journal and its recovery rules are in [supervisor.md](supervisor.md),
"Upgrade journal".

### Agent pods are cycled

Rolling the Kivali pod without cycling agent pods orphans them: they are
wedged to the hostPath control socket, and the restarted server adopts
existing pods rather than recreating them. The supervisor deletes them
whenever it replaces the server: `upgrade` before it stops the VM,
`load-images` alongside the server's restart. A
rollout done by hand has to do the same:

```
kivali-supervisor exec -- k3s kubectl -n kivali delete pod -l app=kivali-agentpod
```

## Backup and restore

There is one backup and one restore: the app's. Everything else calls
them.

**Backup** on the Org page (`/org/backup`, `POST /api/v1/org/backup`)
streams a zip of the entire data tree — agents, chats, messages,
memory, project files, attachments, archived generations, the published
files (`public/<slug>/`), the knowledge graph with its version log, the
assignment tracker. The walk carries whatever is under the data
directory, so a new store needs no backup change; only the CLI's home
(`claude-home/`, with the Claude sign-in) and debug dumps
are left out. A file the walk cannot read aborts the download
rather than leaving it out. Every backup ends with a manifest listing
each file's size and SHA-256. The page's button is a plain form POST,
so the zip goes straight to the browser's own downloads, with its
progress, and never through the page; closing the Kivali tab does not
stop it. A download that stops partway shows in the browser as failed.

**Restore** is offered on the first setup screen ("Restore from a
backup", `POST /api/v1/org/restore`), and only while the deployment is
fresh (no agents beyond the seeds, no chat history), so a restore can
never silently destroy a running org. It takes the zip a backup wrote:

- The upload is staged on the data volume, then checked whole before
  anything is written: every name, every file against the manifest
  (missing, extra, short or changed fails it). An archive without a
  manifest is refused.
- There is no fixed size limit. The upload, and then what the archive
  unpacks to by its manifest, must fit on the data volume with 256 MiB
  to spare; otherwise it is refused (507) before anything is written.
- It ends the way a boot does (published files,
  built-in skills, the files sync, the graph re-verified, assignment
  wakes routed, model pins moved, agent pods provisioned) and then
  audits the result with the server's own readers; anything this binary
  cannot read is named in the server log.

**From a shell**, `kivali-supervisor backup --out org.zip` and
`kivali-supervisor restore --in org.zip` do the same through the same
endpoints: the supervisor signs in to the org as its owner with the
desktop handoff ([supervisor.md](supervisor.md)).

## When something is wrong

- `kivali-supervisor status` shows the VM, the guest, the forward,
  `local.json` and any open upgrade journal.
- The logs are in the config directory: `logs/supervisor.log` (the
  supervisor's operations) and `logs/console.log` (the guest's boot and
  console; `.1` is the previous boot).
- `kivali-supervisor exec -- k3s kubectl get pods -A` runs kubectl in the
  guest; `kivali-supervisor terminal --shell` is bash in the server
  container.
- An upgrade that stopped half way: [supervisor.md](supervisor.md),
  "Upgrade journal". Everything else:
  [../troubleshooting.md](../troubleshooting.md).
