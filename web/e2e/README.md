# End-to-end suite

Playwright against the real Kivali binary in `DEV_MODE`, with `cmd/fake-claude` standing in for
the Claude Code CLI and `cmd/devseed` building the data directory.

```
make web-e2e                  # web build, the three binaries, then every spec (desktop + phone)
cd web && npx playwright test -c e2e/playwright.config.ts chat --project=desktop   # one spec
```

## What runs

- `global-setup.ts` builds whatever is missing (the web app when `internal/web/ui/dist` is empty,
  then `bin/kivali-e2e`, `bin/devseed` and `bin/fakebin/claude`) and starts one shared server on the demo seed. `make web-e2e` always rebuilds
  first; a bare `npx playwright test` uses what is there.
- A server is the Go binary with `DEV_MODE=true`, `ADDR=127.0.0.1:<free port>`, a fresh
  `DATA_DIR` under `/tmp/kv-*`, `AGENTPOD_UDS_DIR` set, `HOME` pointed into the scratch dir and
  `bin/fakebin` first on `PATH`, plus one `kivali-e2e agent` runtime per agent. Chat turns only
  run through an agent runtime, which spawns `claude` (the fake) exactly as an agent pod does.
  Readiness is event-driven: the "listening" log line, `/healthz`, then an `/org/stream`
  snapshot with no agent disconnected.
- `support/fixtures.ts` gives every test a server. The default `seed: 'shared'` is the global
  one and is for read-only tests. A test that changes data sets `test.use({ seed: 'demo' })`
  (or `'empty'`, `'setup'`) and gets a fresh server for itself. `fakeScenario` sets the fake's
  default script for a fresh server; `fakePauseMs` sets its `slow` pause (a test that lets a slow
  turn run to its end asks for a short one); `seedNow` pins devseed's `-now`.
- `KIVALI_E2E_KEEP=1` keeps each server's scratch dir (data, `server.log`, agent scratch) in
  `/tmp`. A failing test attaches its server's `server.log`, which includes one line per
  stream-json frame fake-claude read or wrote.
- Concurrent local runs must not share an output dir: pass `--output=e2e/test-results-<name>`.

## fake-claude scenarios

A `[fake:<name>]` tag in the newest message of a turn picks its script; otherwise
`FAKE_CLAUDE_SCENARIO` (default `reply`).

| Scenario | What the turn does |
|---|---|
| `reply` | thinking, "I'll " / "check the reminders " / "today.", a `file_view` tool call and result, then "All 500 test reminders went out. …" |
| `slow` | `reply` with a pause before each later delta (`FAKE_CLAUDE_PAUSE_MS`, 20s here unless the test sets `fakePauseMs`) that an interrupt ends at once |
| `fold` | after the first delta, waits for a second message (up to `FAKE_CLAUDE_FOLD_WAIT_MS`), holds it queued for `FAKE_CLAUDE_FOLD_HOLD_MS` (3s here, so the page shows it pending), then consumes it at the tool seam and acknowledges it |
| `error` | some text, then the `<synthetic>` close-out with an API error and an `is_error` result: the turn-error path |
| `subagent` | calls `subagent` on the core MCP server from `--mcp-config`; the subagent runs as another fake process in the runtime and its result wakes the parent |

## Visual baselines

Baselines are generated, never committed: `web/e2e/__screenshots__/` is git-ignored on every
platform. A visual check is always "this commit against its parent (or any base)", so the
baselines are made on the base commit and thrown away with the checkout.

```
make visual-compare BASE=<parent-sha>             # BASE defaults to HEAD~1
```

which runs `scripts/visual-compare.sh <base> [--allow-dirty]`:

1. adds a temporary detached git worktree at `BASE` (your checkout is never touched);
2. runs `make web-e2e-baselines` there (installs web deps with `npm ci` if missing, builds the
   web app and linux/amd64 binaries into `bin/linux/`, then runs the Playwright image with
   `--update-snapshots=all`);
3. copies the result into this checkout's `web/e2e/__screenshots__/` and removes the temporary
   worktree;
4. runs `make web-e2e-visual-check` on the current checkout, the same Docker run without
   `--update-snapshots`, and prints where the report is.

Docker is required. The script refuses to run with uncommitted changes under `web/` (they would
blur the comparison) unless you pass `--allow-dirty`, and it never switches checkouts: to check a
different target, check it out first. The compare targets whatever is checked out.

Reading the result: the run prints one line per failing state. Open the report with
`cd web && npx playwright show-report` (`web/playwright-report/`); each failure shows expected
(base), actual (target) and a diff, with a slider view. The same images are in
`web/e2e/test-results-linux/`. A failure is a difference to look at, not necessarily a bug: an
intended design change shows up as diffs on exactly the screens it changed. No failures means the
target is pixel-identical to the base.

Platform and stability: baselines are linux/amd64, made in the official Playwright image of the
version the lockfile pins (what CI runs), so fonts and antialiasing match between base and target.
Do not compare against screenshots taken on macOS or Windows. `visual.spec.ts` runs only with
`KIVALI_VISUAL=1`, which also adds the `desktop-dark` and `phone-dark` projects; baselines land in
`__screenshots__/linux/<project>/<state>.png`. Each state gets a fresh server seeded at the last
12:00 UTC with the browser clock frozen there, so relative labels never move; dates, clock times
and dollar amounts are masked because the server counts spend windows on its own clock. New
states or masks must keep that rule, or every comparison will be noisy.

The pieces are also available separately: `make web-e2e-baselines` (generate into the current
tree) and `make web-e2e-visual-check` (compare against what is there). The visual spec is not in
CI.
