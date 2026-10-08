# Working in this repository

Guidance for coding agents and the people who run them. Human
contributors should read [CONTRIBUTING.md](CONTRIBUTING.md) first; the
full developer guide is [docs/developers/README.md](docs/developers/README.md).

## Orientation

- `main.go`, `*_cmd.go` — the `kivali` binary: the server, plus the
  `agent`, `mcp` and `prepare-scratch` subcommands.
- `internal/` — the Go packages. `store/` is the only writer to the data
  directory; `web/` is the JSON API and SSE streams; `agent/` and
  `agentpod/` run agents; `claudeagent/` drives the `claude` CLI and owns
  the model catalog; `supervisor/` runs a team's VM.
- `web/` — the React web app (TypeScript, Vite), embedded into the binary.
- `desktop-app/` — Kivali Desktop (Tauri, Rust + TypeScript).
- `vm/` — the VM image; `charts/kivali/` — the Helm chart.
- `design-system/` — the vendored design system; `make ds-sync` copies it
  into `web/src/ds/`.
- `docs/` — user documentation; `docs/developers/` — technical references.

Read [docs/developers/architecture.md](docs/developers/architecture.md)
before changing how agents run or how messages move.

## Commands

```
make help        every target meant to be typed
make test        Go unit tests, lint, the web app's lint and tests
make ci          everything CI runs on every commit
make licenses    license notices + license policy check, after a dependency change
make run         a demo team at http://127.0.0.1:8080, no auth, scripted model (needs Go and Node)
```

Run `make test` before every commit and `make ci` before opening a pull
request. `make lint` runs the `golangci-lint` on your PATH and warns
when it is not the version CI pins (`GOLANGCI_VERSION` in the Makefile).

## Conventions

- **Deterministic tests.** No sleeps and no poll loops. Inject a clock
  (`internal/clock`, `internal/supervisor/clock.go`) and drive it.
- **Both sides of the wire.** A field the Go server emits and the web app
  or desktop app reads needs a test on each side. Shapes live in
  `internal/web/apitypes`; regenerate TypeScript with `make api-types`.
- **Agent-facing text states only what is.** The handbook, role templates
  and tool descriptions in `internal/seed/`, `internal/agent/` and
  `internal/mcp/` describe current behaviour. No history, no "there is no longer an X".
- **Company-agnostic.** Kivali ships knowing nothing about any company.
  Anything specific to a business arrives through the setup wizard and
  project files, never through code or seed text.
- **UI copy** follows the voice in `design-system/README.md` and is checked
  by `make test` (sentence case, no exclamation marks, no emoji, no banned
  words).
- **Docs move with code.** A change to behaviour a doc describes updates
  that doc in the same change.
- **Comments describe the code as it is,** not how it came to be.
- **Dependency licenses.** A new or updated dependency must pass
  `make licenses`; the allowed licenses and per-package exceptions (each
  with a reason) are in `scripts/license-policy.txt`.
