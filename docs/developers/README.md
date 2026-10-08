# Developer guide

How to build, run, test and change Kivali. For what Kivali is and how
to use it, start with the user docs in [docs/](../README.md); for how
the pieces fit together, read [architecture.md](architecture.md) next.

## What is in the repository

```
main.go               the server, and the subcommand dispatch (agent, mcp, prepare-scratch)
mcp_cmd.go            `kivali mcp`: the MCP server each claude CLI process talks to
agent_cmd.go          `kivali agent`: the per-agent runtime that runs in an agent pod
integration_test.go   the integration suite (build tag `integration`)
internal/
  agent/              per-agent runtime, tool dispatch, the runner that drives a turn
  agentpod/           agent pods: the Kubernetes pod manager, the UDS protocol, the in-pod runtime
  assignments/        the assignment tracker (assignments.md)
  auth/               Google sign-in, the allowlist, sessions, the desktop handoff (auth.md)
  backup/             backup and restore of /data
  builtinskills/      skills shipped with the binary
  claudeagent/        the model driver: runs the `claude` CLI; model catalog, pricing, credentials
  claudeauth/         reads `claude auth status --json`; writes a provider's settings `env` block
  clock/              the one time source; tests drive a Fake
  clockcheck/         host clock-skew watchdog
  config/             environment-variable configuration (../configuration.md)
  controlclient/      Unix-socket client the MCP subprocesses use to reach the server
  convert/            office documents and PDFs to text, for project files
  devshell/           the dev-shell daemon: bash and the file_* tools over UDS
  files/              the /files/ namespace behind the file_* tools (files-and-publishing.md)
  graph/              the knowledge graph over published files (knowledge-graph.md)
  mcp/                MCP server pieces: skills, project files, agent memory
  message/            the message file format and parser
  messaging/          the Messenger: route, release, bounce, release-all
  owner/              names the person a team works for
  tracker/            applies a change to the assignment tracker end to end
  provider/           the provider-neutral seam: Client, Provider, request and usage shapes
  seed/               first-run handbook and Chief of Staff role templates
  store/              the filesystem store, the only writer to /data
  supervisor/         kivali-supervisor: VM backends, guest agent protocol, install, upgrade
  web/                JSON API (/api/v1), SSE hubs, agent-pod surfaces
  web/apitypes/       the JSON wire contract; TypeScript is generated from it
  web/ui/             the embedded web app build and its SPA handler
cmd/
  dev-shell/          the per-agent shell sidecar
  egress-proxy/       the egress proxy (per-agent domain allowlist)
  devseed/            seeds a data directory (demo, empty, setup scenarios)
  fake-claude/        a scripted stand-in for the claude CLI (local runs, e2e)
  inspect_request/    prints the request the server would send for an agent
  kivali-supervisor/  runs a team: boots its VM, installs and upgrades the chart
  kivali-guest/       the agent inside that VM
web/                  the web app (React, TypeScript, Vite), its linters and the Playwright suite
design-system/        the vendored Kivali design system (never edited by hand)
charts/kivali/        the Helm chart the supervisor installs into each team's VM
vm/                   the VM image (Alpine + k3s) the supervisor boots
desktop-app/          Kivali Desktop (Tauri), which drives the supervisor
scripts/              build VM, test VM, CI k3s, release and design-system scripts
docs/                 user docs; docs/developers/ is this guide and the references
```

## Prerequisites

- **Go** at the version in `go.mod` (1.24).
- **Node 22** (what CI uses) for the web app; run `make web-install`
  once per checkout (`npm ci` in `web/`, from the committed lockfile).
- **The `claude` CLI** (`@anthropic-ai/claude-code`) — only for
  `CLAUDE=real`, signed in through its own sign-in (run `claude`; the
  menu appears when it is not signed in, `/login` switches). The server
  refuses `ANTHROPIC_API_KEY` and the other credential variables. A
  local run with the fake needs no sign-in.
- **`golangci-lint`** on `PATH` for `make lint`, at the version CI pins
  (`GOLANGCI_VERSION` in the Makefile; `make lint` warns when yours
  differs). **`shellcheck`** at `SHELLCHECK_VERSION`.
- For `make ci`: also `helm`, `jq` and a Rust toolchain with clippy
  (`desktop-app/rust-toolchain.toml`).
- For a dev team, `make test-vm` and image builds: macOS on Apple
  Silicon, or Windows with Hyper-V (see
  [supervisor.md](supervisor.md), "The Windows backend"), plus `helm`.
  No Docker: images build with BuildKit inside a Kivali VM
  (`scripts/build-vm.sh`). The first VM image on a new machine comes
  from a release or the installed Kivali Desktop. On Linux, image
  builds use the local Docker (`ENGINE=docker`).

## Running locally

```
make run                                    # a demo team, fake Claude
make run DEMO_SCENARIO=setup CLAUDE=real    # a fresh install with the setup wizard, your claude CLI
```

`make run` builds the web app (installing its npm packages the first
time), the server, `cmd/devseed` and `cmd/fake-claude`,
seeds a fresh data directory under `/tmp/kv-demo-*` (printed at start
and left behind for inspection), starts the server on
`http://127.0.0.1:8080` and one agent runtime (`kivali agent`) per
agent, talking to the server over Unix sockets as agent pods do.
Ctrl-C stops everything.

| Variable | Values |
| --- | --- |
| `DEMO_SCENARIO` | `demo` (default: realistic data on every screen), `empty` (setup done, nothing else), `setup` (the setup wizard) |
| `CLAUDE` | `fake` (default: `cmd/fake-claude` answers; no sign-in, no cost) or `real` (the `claude` CLI on your `PATH`, billed to whatever it is signed in to) |
| `DEMO_PORT` | the port (default 8080) |

There are no agent pods or dev-shell locally (the boot log says
`agentpod: disabled (not running in-cluster)`); chat, messaging,
release and the wizard work. `make run` sets
`KIVALI_MCP_LOCAL_FILES=1` and `KIVALI_MCP_LOCAL_FILES_ROOT` (a `files/`
directory beside the data), so `kivali mcp` runs the `file_*` tools and
`run_shell` in-process against that tree, as the e2e suite does.

**`DEV_MODE` turns authentication off.** No Google client, no
`SESSION_KEY`, no allowlist; every request is attributed to
`dev@localhost` (`DEV_USER`). Because that is unsafe anywhere but your
own machine, the server refuses to start in dev mode with
`KIVALI_ENV=prod`, or on a listen address that is not loopback (unless
`DEV_MODE_ALLOW_NONLOOPBACK=true`, which a pod needs and nothing else
should). Every variable is in [../configuration.md](../configuration.md).

To run a full team the way users do (VM, k3s, agent pods, sign-in),
use the dev team below.

## Build and test

```
make help          # every target meant to be typed
make build         # bin/kivali, with the web app embedded
make test          # the quick loop: Go unit tests, lint, web lint and tests, wire types
make ci            # everything CI runs on every commit (needs helm, shellcheck, cargo, jq)
make licenses      # after a dependency change: license notices + license policy check
make nightly       # the race detector and the integration suite
```

What `make test` and `make ci` run, as separate targets:

- `make unit-test` — Go unit and service-level tests.
  `PKG=./internal/web TESTFLAGS='-run TestX -count=1'` narrows them.
- `make lint` — `golangci-lint run` and `fmt --diff`, again with the
  `integration` build tag, then shellcheck over `scripts/` (skipped
  with a warning if shellcheck is missing; required under `make ci`).
- `make web-lint` / `make web-test` — the web app's linters, type check
  and Vitest suite. They fail, never skip, without `web/node_modules`.
- `make api-types-check` — regenerates the TypeScript wire types and
  JSON fixtures from `internal/web/apitypes` and fails if anything
  changed. After changing an API type, run `make api-types` and commit
  the result.
- `make web-e2e` — Playwright against the real binary in `DEV_MODE`
  with `fake-claude` ([web/e2e/README.md](../../web/e2e/README.md)).
  Needs Playwright's Chromium (`npx playwright install chromium` in
  `web/`).
- `make visual-compare BASE=<ref>` — a screenshot diff of this checkout
  against another commit, in the Playwright Docker image.
- `make race-test` — the unit tests under the race detector (nightly).
- `make cross-check` — builds every package for Windows, Linux and
  macOS and vets the supervisor on each (part of `make ci`).
- `make helm-lint` / `make helm-template` — the chart, offline.
- `make -C desktop-app lint test` — the desktop shell.
- `make license-check` — holds every shipped dependency (Go modules,
  the npm production packages of `web/` and `desktop-app/`, the crates
  of `desktop-app/src-tauri`) to the allowed licenses in
  `scripts/license-policy.txt` (part of `make ci`). See "Dependency
  licenses" under Conventions.

The binary embeds `internal/web/ui/dist`; `make build` builds the web
app first. A bare `go build` still compiles, and every app route then
answers 503 saying the frontend is not built.

### The integration suite

`integration_test.go` installs the server the way the supervisor does
(the chart in k3s's static charts, the HelmChart the supervisor
renders) into the k3s the VM image bakes, run directly on a
linux/arm64 host. The nightly workflow runs it on an arm64 runner:

```
KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh up     # the VM's k3s, on this host
KIVALI_IT_DISPOSABLE_K3S=1 make integration-test
KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh down   # stop it and delete its state
```

It wipes what it installed on every run, so both the suite and the
script refuse to run without `KIVALI_IT_DISPOSABLE_K3S=1`.

On macOS and Windows, run the Go suites on Linux inside a Kivali VM on
your machine:

```
make test-vm                       # unit tests (PKG, TESTFLAGS pass through)
make test-vm SUITE=integration     # the integration suite, in the VM's own k3s
scripts/test-vm.sh destroy         # delete the test VM and its disk
```

The test VM is the build VM (`scripts/build-vm.sh`), separate from the
dev team. Each run boots it and stops it afterwards, since a running VM
never gives memory back to macOS
([vm/README.md](../../vm/README.md), "Memory on macOS"); its disk, with
Go's and BuildKit's caches, is kept. `make test-mac` covers what only a
Mac can test: the supervisor's macOS backend, the shell's platform
module, and a VM image built from this tree booting and stopping
cleanly. Run it before a release.

Change something that runs in an agent pod or drives the real CLI
(`internal/agentpod`, `internal/claudeagent`, the chart)? Run the
integration suite as well: unit tests use fakes for both.

## The dev team

A full team (VM, k3s, the chart, agent pods, Google sign-in) runs on
your Mac or Windows machine through `kivali-supervisor`, from a config
directory of its own (`DEV_CONFIG_DIR`, default `~/.kivali-dev`), so it
never mixes with the teams Kivali Desktop runs:

```
make dev-vm                          # once: build the :dev images and chart, bake them into vm/build/out
make dev-up DEV_OWNER=you@gmail.com  # boot it; the first boot installs it for that Google account
make dev-load                        # the loop: rebuild the :dev images into it and restart
```

Everything else is the supervisor's own CLI with
`--config-dir ~/.kivali-dev`: `down --exit`, `status`, `terminal`
(Claude Code in the server container, where you sign it in),
`destroy --yes --exit`, `backup --out` and `restore --in`.
[deployment.md](deployment.md) has the full workflow and what to check
when something is wrong; [supervisor.md](supervisor.md) is the
supervisor's reference.

## Subcommands

`kivali` with no subcommand is the server. It also hosts:

- `kivali agent` — the per-agent runtime, the entrypoint of an agent
  pod: supervises the agent's `claude` CLI process and routes every
  authoritative read and write back to the server over UDS.
- `kivali mcp` — the stdio MCP server each `claude` process starts:
  `--toolkit full-agent` (the default) is an agent's whole tool surface
  (`file_*`, `agent_memory_*`, `publish_*`, assignments, state lookups,
  `run_shell`, `share_file`, `subagent`); `--toolkit subagent` is the
  narrow surface a subagent gets. Calls that change state are routed to
  the server over the control socket.
- `kivali prepare-scratch` — the agent pod's init container: prepares
  the per-agent `/scratch` volume.

## Conventions

- **Run `make test` before every commit** and `make lint` in any case;
  CI's golangci-lint and shellcheck versions are the ones that count.
- **Deterministic tests.** No sleeps and no polling loops waiting for
  real time. Production code takes an `internal/clock.Clock`; tests use
  `clock.Fake` and advance it by hand.
- **A wire format needs tests on both sides.** The Go types in
  `internal/web/apitypes` are the contract; golden fixtures from the Go
  side are type-checked by the web tests, and `make api-types-check`
  fails on drift. A new field the JS reads gets a test that asserts the
  server emits it.
- **UI text follows the brand's content rules**
  ([design-system/README.md](../../design-system/README.md), "Content
  fundamentals"): `web/lint/copy-lint.mjs` rejects banned words,
  exclamation marks, emoji and Title Case labels. The design-system
  linters in `web/lint/` also check tokens and component props; a
  single line can be excused with `kivali-lint-disable-next-line
  <rule>` and a reason.
- **Dependency licenses.** After adding or updating a dependency, run
  `make licenses`: it regenerates `dist/licenses/THIRD_PARTY_LICENSES.txt`
  and runs `make license-check`. A dependency outside the policy fails
  CI. Replace it, or, after review, add `allow <SPDX-ID>` (a license
  acceptable for every dependency) or `exception <ecosystem> <name>
  <reason>` (that one component) to `scripts/license-policy.txt`.
- **The provider seam.** Only `internal/claudeagent` knows about the
  `claude` CLI; everything else reaches it through `internal/provider`.
  `internal/provider_imports_test.go` enforces it.
- **Agent-facing vocabulary.** Prompts and tool descriptions use the
  product's words (an assignment, parts, the handbook);
  `internal/vocabulary_test.go` checks them.
- **Company-agnostic.** Nothing in the code or shipped prompts names a
  real company; anything specific to one comes from the files uploaded
  at setup.
- **The design system is vendored.** Do not edit `design-system/`;
  bring in a new export ([design-system/PROVENANCE.md](../../design-system/PROVENANCE.md))
  and run `make ds-sync`.

## Troubleshooting the build

**`compile: version "goX.Y.Z" does not match go tool version`.** A stale
`GOROOT` in your environment (a version manager, a shell profile)
points at another Go than the `go` on `PATH`. The Makefile unexports
`GOROOT`, so `make` targets work; a bare `go` command in the same shell
does not. Drop `GOROOT` from your shell profile.

**`web/node_modules is missing: run 'make web-install' first`.** The
web app's dependencies are not installed in this checkout.

**`make lint` says shellcheck is not on PATH.** Shell scripts are
skipped with a warning; install shellcheck to lint them (required by
`make ci`).

**`lint: golangci-lint on PATH is not vX.Y.Z`.** Your linter differs
from CI's; findings may differ. Install the pinned version.

**`api-types-check: … was stale`.** An `apitypes` change without
regenerating. The files are now regenerated; commit them.

**`control socket: listen unix <path>: bind: invalid argument`.** The
`DATA_DIR` path is too long: Unix socket paths are limited to about 104
bytes on macOS and 108 on Linux, and the control socket lives at
`$DATA_DIR/.kivali-control.sock`. Use a shorter `DATA_DIR`.

**`DEV_MODE=true but ADDR=":8080" is not loopback`.** A bare port binds
every interface. Set `ADDR=127.0.0.1:8080` (`make run` does).

**`config: required env vars missing: [...]`.** The server was started
without `DEV_MODE=true` and without a sign-in configuration. Use `make
run`, or see [../configuration.md](../configuration.md).

**Every app route answers 503.** The binary was built without the web
app; use `make build`.

**Image builds or `make test-vm` cannot find a VM image.** The build VM
boots `vm/build/out` when it holds an image, else the installed Kivali
Desktop's. Install Kivali Desktop, or unpack a release's VM image into
`vm/build/out`; from then on `make -C vm image` builds its successors.

## Developer docs

| Doc | What it covers |
| --- | --- |
| [architecture.md](architecture.md) | The components, how a turn flows, message delivery, agent pods, subagents, rotation, the /data layout. |
| [context-serialization.md](context-serialization.md) | How an agent's prompt is assembled, message rendering, rotation, the provider seam. |
| [assignments.md](assignments.md) | The assignment tracker's contract: files, states, tools, events. |
| [files-and-publishing.md](files-and-publishing.md) | The `/files/` namespace, workspaces, publishing and the shared tree. |
| [knowledge-graph.md](knowledge-graph.md) | The graph published files form: links, indexing, queries. |
| [auth.md](auth.md) | Google sign-in, the public client and relay, sessions, the allowlist, the desktop handoff. |
| [deployment.md](deployment.md) | Teams run by the supervisor: the dev team, upgrades, backup and restore, first checks. |
| [supervisor.md](supervisor.md) | kivali-supervisor: backends (macOS, Windows), CLI, RPC, guest protocol, install, upgrade journal. |
| [desktop-app.md](desktop-app.md) | Kivali Desktop, the Tauri shell around the supervisor. |
| [releasing.md](releasing.md) | Cutting a release: versions, the release workflow, signing, the update feeds. |
| [vm/README.md](../../vm/README.md) | The VM image: build, boot contract, guest init, memory on macOS. |
| [charts/kivali/README.md](../../charts/kivali/README.md) | The Helm chart: fixed names, Secret keys, values, NetworkPolicy. |
| [web/e2e/README.md](../../web/e2e/README.md) | The Playwright suite, fake-claude scenarios, visual baselines. |
| [design-system/README.md](../../design-system/README.md) | The brand guide: content rules, visual foundations, components. |
