# Kivali Desktop: shell reference

The developer reference for the Tauri app that is Kivali Desktop. For
what a person sees and does, read the user guide,
[../desktop-app.md](../desktop-app.md). Screens are referred to by
their design artboard ids, A1 to G5, as in the code comments. Bundle id
`ai.kivali.desktop`, product name **Kivali**. It runs one or more
**teams** on this computer, each in its own VM driven by its own
bundled `kivali-supervisor` ([supervisor.md](supervisor.md)), and connects to
teams on other computers by their address. Each team gets a window of
its own that loads the team's served web app; when the team cannot show
itself, a bundled Kivali page takes its place. The app also owns the
setup and Settings windows, the menu bar, the tray icon and its menu,
alerts and notifications. A "team" is what the server and the
supervisor call an org. Everything that differs between
operating systems is in one module, `platform` ("Platform" below); the
rest of this document uses the platform's terms, with the macOS detail
in parentheses.

## Design overview

```
+------------------------- Kivali.app --------------------------+
|  Tauri shell (Rust)                                           |
|   one window per team: the team's served web app, or a        |
|   bundled page when the team cannot show itself               |
|   remote origins get no IPC; only the bundled pages do        |
|   tray, menu bar, setup and Settings pages, teams.json        |
|   updater (Tauri updater plugin, GitHub release latest.json)  |
|                          | owner-only Unix socket / named pipe|
|  kivali-supervisor (Go sidecar), one per team here            |
|   boots the VM (Virtualization.framework / Hyper-V)           |
|   vsock: port forward, guest exec, image import, PTY          |
|   release feed check, upgrade journal, rollback               |
+-------------------------------+-------------------------------+
                                | vsock
+-------------------------- guest VM ---------------------------+
|  root disk (from the app): kernel, k3s, baked image tarballs  |
|  data disk (persistent): the whole k3s state directory        |
|  k3s with the Helm controller; NetworkPolicy fences agents    |
|  kivali server pod + egress sidecar, one agent pod per agent  |
+---------------------------------------------------------------+
```

The shape follows from constraints in the server:

- **The whole stack runs inside the VM.** Agent pods reach the server
  over a hostPath Unix socket, mount the server's ReadWriteOnce data
  volume, and carry a required pod affinity to the server pod
  (`internal/agentpod/manifest.go`), so they share its node. The host
  only forwards a port.
- **The window loads each team's served web app**, exactly as a
  browser does. The web app is strictly same-origin (relative fetches,
  a cookie session, `requireSameOrigin` on every write), so a copy
  bundled in the app could not talk to any server, and would drift in
  version from the server it talks to.
- **The list of teams lives in the shell.** Browser storage is per
  origin, and a team elsewhere is third-party content from the shell's
  point of view; no page the shell loads gets a channel to a
  supervisor.
- **One org per server.** Switching team is switching server, so each
  team gets its own window.
- **The supervisor is Go**, bundled as a sidecar: the
  Virtualization.framework bindings (`Code-Hex/vz`) are Go, the rest of
  Kivali is Go, and the same binary drives Hyper-V on Windows through
  the broker ([supervisor.md](supervisor.md)).
- **Images are baked into the VM image** as airgap tarballs, so a first
  boot needs no registry and no network.

**Credentials.** Kivali has one model driver, the Claude Code CLI, and
signs it in with the CLI's own sign-in (a Claude subscription, an
Anthropic Console account, Amazon Bedrock or Google Vertex AI), stored
on the team's data volume, which every CLI in the team uses. The app
opens a terminal into the server container running `claude` for that
sign-in and reads nothing of it; Microsoft Foundry is set up through
the app's own form instead ("The flows", Connect Claude); what it shows (signed in, the account, what it
bills) is the supervisor's `GET /v1/credential`, the CLI's own `claude
auth status`. The credential lives only on the team's data disk (and in
the upgrade snapshot of that disk, which is deleted when the upgrade
ends).

**Two versions.** The desktop app's version and the Kivali release
running in a team are cut together but may drift. A newer Kivali whose
minimum desktop version is above the installed app asks for the app
update first and does nothing else; an older Kivali keeps running
under a newer app, because a team's version is recorded on its data
disk and the root disk carries none. A release that needs a step the
app cannot do sets `manual_steps` in `release.json`, and the app
explains instead of upgrading ("Update states").

## Layout

```
desktop-app/
  Makefile                 dev, build, build-debug, build-trial, test, lint, fmt-check, icons, clean, sidecar, licenses
  package.json             Vite, TypeScript, Vitest, @tauri-apps/cli + api (dev deps only)
  index.html, src/         the bundled pages (plain TypeScript, no framework; "The bundled pages")
    main.ts                routing, the snapshot, rendering
    ipc.ts                 invoke + the shell-changed event (the dev mock under vite)
    ctx.ts, h.ts, ui.ts, icons.ts, theme.ts, preset.ts
    pages/                 setup.ts (A1–A20), connect.ts (B1–B6), team.ts (C4–C8),
                           settings.ts (D1–D11), dialogs.ts (the in-page E sheets),
                           foundry.ts (the Microsoft Foundry form)
    logic/                 route, setup, teams, format, foundry: the pages' decisions (+ .test.ts)
    mock.ts                dev only: fixtures per artboard (?state=<id>)
    types.ts               the snapshot and the commands' shapes
    style.css              the desktop's own styles over the design system's
  scripts/make-icons.mjs   renders the tray icon sets from SVG (rsvg-convert)
  fake-supervisor/         a stand-in kivali-supervisor speaking the real RPC (Unix socket or named pipe)
  src-tauri/
    tauri.conf.json        bundle (macOS: app + dmg), CSP, externalBin, updater endpoint + key
    tauri.windows.conf.json  Windows bundle: per-machine NSIS, its hooks, WebView2 bootstrapper
    windows/installer-hooks.nsh  the installer registers the broker service, the uninstaller removes it
    capabilities/shell-pages.json   the only capability
    Entitlements.plist     com.apple.security.virtualization (macOS)
    build.rs               puts every app command under the ACL
    icons/                 bundle icons from the official Kivali icon (source.svg);
                           tray-<state>-<ink>[-<frame>].png (macOS) and tray-win-… (Windows)
    src/
      lib.rs               startup, run loop, start at login, logs, opening in the browser
      cli.rs               --version, --check
      teams.rs             teams.json: reading, repairing, writing
      shell.rs             state: one Runtime per team here; the flows; the watch loop; quit
      view.rs              team state (C9), phrases, operation stages, the memory rule
      windows.rs           setup, settings and the team windows; the navigation pin
      signin.rs            a team window's sign-in: login rewrite, the loopback return listener
      ownersignin.rs       the owner's Google sign-in in setup (A5–A8)
      teamapi.rs           the team API: a team's own facts, read as its window's person
      menus.rs             the menu bar (F1; on Windows one per team window), the tray menu (F2),
                           native alerts from menus
      trayicon.rs          the tray picture (F3): four states, the pulse; menu status dots (C9)
      commands.rs          IPC commands for the bundled pages
      hostinfo.rs          this computer's memory and CPUs
      diagnostics.rs       Settings → Advanced → Diagnostics
      orgurl.rs            team addresses, the GET /api/v1/login check
      updates.rs           the Kivali release verdict
      paths.rs             file names, atomic writes
      platform/            everything OS-specific ("Platform"): mod.rs, macos.rs, unix.rs,
                           notify.rs, notify_macos.rs, windows.rs, windows_terminal.rs,
                           windows_ui.rs, plugin_dialog.rs, linux.rs
      supervisor/          the supervisor client (mod.rs, http.rs, wire.rs, sidecar.rs)
    tests/supervisor_e2e.rs  the client against a real process on the real transport (socket or pipe)
```

## Running it

Rust (stable, minimal profile) and Node, plus Go for the real
supervisor sidecar. The Makefile finds `cargo` in `~/.cargo/bin` even
when it is not on `PATH`.

```sh
make -C desktop-app dev           # tauri dev against a Kivali-dev config dir (macOS: ~/Library/Application Support/Kivali-dev)
make -C desktop-app build-debug   # a debug bundle (macOS: src-tauri/target/debug/bundle/macos/Kivali.app, ad-hoc signed); no updater key needed
make -C desktop-app build         # release bundles + updater artifacts (needs the real supervisor)
make -C desktop-app build-trial   # release-profile bundle for a trial install: no updater artifacts, no keys checked
make -C desktop-app test          # cargo test + vitest
make -C desktop-app lint          # tsc + clippy, warnings as errors (clippy required; what CI runs)
make -C desktop-app icons         # the app icon and the tray icons (needs rsvg-convert)
npm --prefix desktop-app run dev  # the bundled pages alone, in a browser, on the dev mock
```

The Makefile follows `TRIPLE` (the Rust host by default): a Windows
triple gives the sidecar an `.exe` suffix, builds the real supervisor
with `GOOS=windows CGO_ENABLED=0`, skips `codesign` and the Apple
checks of the release preflight, and bundles the per-machine NSIS
installer (`tauri.windows.conf.json`; "Signing and release"), which
`make build-debug` writes as
`src-tauri/target/debug/bundle/nsis/Kivali_<version>_x64-setup.exe`.

`SUPERVISOR=fake` (or `real`) picks the sidecar; the default picks the
real one whenever `cmd/kivali-supervisor` exists. The fake needs no VM:
it pretends to create, boot, upgrade and delete a team with a stream
of log lines carrying the real stage ids, answers the credential
status (signed out, or with `FAKE_SIGNED_IN=1` signed in as the owner,
billing `FAKE_BILLING` or "Claude Max"), and serves a stand-in page plus `GET /api/v1/login`
on the team's port (the shell's port hint, else `FAKE_PORT`, default
18080; like the real supervisor it moves off a port another program
holds), so every window and menu can be exercised. `FAKE_CHECK` picks
its release verdict (`up-to-date`, `upgrade-available`,
`desktop-update-required`, `manual-steps`, `failed`), `FAKE_STEP_MS`
its pace, `FAKE_FAIL_UP` a failed create, `FAKE_SIGNED_IN=1` a Claude
account already signed in. It never opens a terminal session, so
`terminals` stays 0.

**The dev mock.** `npm --prefix desktop-app run dev` serves the pages
at `http://localhost:1420`. In a plain browser (no Tauri), `ipc.ts`
loads `mock.ts`, which answers every command from fixtures:
`?state=<artboard id>` opens that screen with its snapshot and page
state (`A1`–`A20`, `B1`–`B6`, `C4`–`C8`, `D1`–`D6`, `D9`–`D11`,
`E3`, `E5`–`E8`, `G1`, `G2`, `G4`, `G5`; default `D1`), filling in the
route when the URL has none, and draws the page at the artboard's
content size (`&frame=0` turns that off). `?theme=dark|light` forces a
theme; a `G` id forces dark. Commands are logged to the console and
change the fixture's snapshot where that makes sense. The import is
dead code in a build. Native surfaces (menu bar, tray, alerts E1/E2/E4,
the OS prompt, notifications) and the team web app are not in the mock.

When the VM image's root disk has been built (`make -C vm image`;
`ARCH=amd64` for Windows), the build bundles the image as the `vm`
resource and the shell passes that directory to every `serve
--vm-dir`; without it the supervisor looks for the image itself
(`KIVALI_VM_DIR`). Which files, keyed on which, follows the triple: on
macOS `root.squashfs` keys it and `Image`, `initramfs.gz` and
`root.squashfs` go in, as `Contents/Resources/vm`; on Windows
`root.vhdx` keys it and goes in alone, as `<install dir>\vm` beside
`Kivali.exe` (Tauri's resource directory there is the executable's,
installed or under `target\debug`). `VERSION` goes in with either when
present (the supervisor reads "dev" without it). The shell checks for
the same root file (`platform::VM_ROOT_FILE`) before it passes the
directory on. The Windows image adds about 230 MiB to the installer
(the 248 MiB VHDX holds an already-compressed squashfs), well inside
NSIS's 2 GB limit.

The binary answers two flags before any window exists:

- `Kivali --version` prints `Kivali Desktop <version>`.
- `Kivali --check` reads the config directory without writing, and
  reports teams.json (each team, its folder and port or its address,
  the repairs a launch would make), the bundled supervisor, on Windows
  the broker (`broker: answering` or `not running`: whether
  `\\.\pipe\kivali-broker` exists, asked without connecting), and for
  each team here whether anything listens on its RPC endpoint; exit 1
  when teams.json is unreadable or the supervisor binary is missing.

On Windows a release build is a GUI-subsystem program, started with no
console: run from one with either flag, it attaches to that console
before printing (`platform::attach_terminal`; a redirected or piped
output is used as it is). cmd.exe and PowerShell do not wait for such a
program, so the prompt comes back first and the output lands under it,
and the exit code is not seen; `start /wait Kivali --check` (or a pipe)
waits and sees it.

`KIVALI_CONFIG_DIR` overrides the config directory (the supervisor
reads the same variable); `KIVALI_SUPERVISOR` overrides the sidecar's
path.

## Platform

`src-tauri/src/platform/` holds everything that differs between
operating systems, one signature per concern, and nothing outside it
names an OS, a socket type or a system call. `mod.rs` has the shared types
and re-exports the build's OS module: `macos.rs` (with `unix.rs`, what
macOS and Linux share), `windows.rs` (with `windows_terminal.rs`, its
command lines, and `windows_ui.rs`, what a TaskDialog's answer and a
key press mean, both kept pure so they are tested on every OS), and
`linux.rs`, which keeps Linux compiling but is not a shipped platform
(no terminal launcher, no menu bar, no owner check; its alerts are the
dialog plugin's message box, `plugin_dialog.rs`). A compile-time block
in `mod.rs` pins every signature, so an OS module that drifts fails its
own build.

| Concern | `platform::` | macOS | Windows |
| --- | --- | --- | --- |
| config directory | `config_dir` | `~/Library/Application Support/Kivali` | `%LOCALAPPDATA%\Kivali` (local, never roaming) |
| instance lock | `InstanceLock::acquire` | flock on `shell.lock` | `shell.lock` opened with no sharing |
| RPC transport | `endpoint`, `is_listening`, `connect` | Unix socket `<team dir>/supervisor.sock`, mode 0600 | named pipe `\\.\pipe\kivali-<hash>` of the team's folder (`pipe_name`), owner-only ACL, its server checked to run as this user |
| HTTP over it | `supervisor/http.rs` (common) | the shell's own HTTP/1.1 client: requests, `Content-Length`, chunked and close-delimited bodies, NDJSON streams, `101` upgrades | same |
| VM image, broker | `VM_ROOT_FILE`, `broker_listening` | `root.squashfs` in `Contents/Resources/vm`; no broker (`None`) | `root.vhdx` in `<install dir>\vm`; whether `\\.\pipe\kivali-broker` exists (`WaitNamedPipeW`, no connection: its server is LocalSystem, so the owner check does not apply) |
| who owns serve | `peer_pid`, `parent_pid`, `start_time`, `is_orphan` | `LOCAL_PEERPID`, `proc_pidinfo`; orphan = re-parented to launchd | `GetNamedPipeServerProcessId`, process snapshot, `GetProcessTimes`; orphan = parent gone or started after it |
| starting serve | `spawn_serve` | plain spawn | `CREATE_NO_WINDOW`, `DETACHED_PROCESS` |
| stopping serve | `request_stop`, `kill`, `connect_timeout` | `down --exit` over the RPC (given up after 30 s of silence), then SIGTERM, then SIGKILL | `down --exit` over the RPC (given up after 30 s of silence), then `TerminateProcess`; when `down` failed or timed out, that leaves the VM running until the next start handles it |
| waiting for exit | `wait_exit` | kqueue `EVFILT_PROC` | `WaitForSingleObject` |
| terminal | `open_terminal` | `<team dir>/terminal.command` opened with `open -a Terminal` | Windows Terminal (`wt.exe new-tab`) when on PATH, else `cmd.exe /c start` |
| tray icons | `TRAY_ICONS`, `TRAY_FRAMES`, `TRAY_FRAME_MS` | `tray-*.png`, 40×44 (@2x of 20×22 pt), coloured, light and dark inks | `tray-win-*.png`, 32×32, the same drawings |
| appearance | `menu_bar_dark`, `menus_dark`, `reduce_motion`, `app_is_active` | the status bar window's effective appearance (else `AppleInterfaceStyle`), NSWorkspace's Reduce motion, NSRunningApplication | `SystemUsesLightTheme` / `AppsUseLightTheme` under `HKCU\…\Themes\Personalize`, `SPI_GETCLIENTAREAANIMATION`, the foreground window's process |
| alerts | `alert`, `AlertSpec` | NSAlert, a sheet on the parent window when it shows, else app-modal; destructive styling; a suppression checkbox | `TaskDialogIndirect` (comctl32 v6) on a thread of its own, one at a time: owned by the parent window when it shows (disabled meanwhile, the dialog centred on it), else unowned; any number of buttons; a verification checkbox; no destructive styling (the warning icon instead) |
| owner check | `authenticate` | LocalAuthentication, `LAPolicyDeviceOwnerAuthentication` (Touch ID or the login password) | Windows Hello (`UserConsentVerifier`, owned by the foreground window); where Hello is not set up, a Continue/Cancel confirmation |
| notifications | `notify`, `NotifyTarget`, `on_notification_click` | UNUserNotificationCenter (`notify_macos.rs`): the target rides in `userInfo` and the center's delegate routes the click; outside an app bundle, `tauri-plugin-notification` | `tauri-plugin-notification`; a click only brings Kivali forward |
| team window frames | `guard_frames` | nothing: WKWebView asks wry about frames too, so `on_navigation` decides them | WebView2's `FrameNavigationStarting`, hooked through Tauri's `with_webview` on its `ICoreWebView2Controller` (`webview2-com` 0.39, Tauri's own copy); a frame `frame_may_load` refuses is cancelled |
| app menu | `APP_MENU` | the menu bar (F1) | none |
| window menu | `WINDOW_MENU`, `menu_shortcuts`, `press_keys` | none (the menu bar is the app's) | a menu bar in each team window (Tauri's per-window menu); WebView2's `AcceleratorKeyPressed` hands it the webview's keys; the Edit items type their shortcut into the webview (`SendInput`) |
| wording | `TRAY_AREA` | menu bar | notification area |
| second launch | `SECOND_LAUNCH_FOCUSES`, `notify_running_instance`, `on_second_launch`, `is_reopen` | refused with a dialog; the Dock's reopen opens Kivali | refused; the running shell opens Kivali (a named event, `Local\kivali-shell-<hash>`) |
| updater | `INSTALL_EXITS` | install, pause the teams, restart | the installer ends the process: the teams pause in the updater's before-exit hook |
| file modes | `owner_only_dir`, `file_mode` | 0700 directories, 0600/0700 files | none (the profile's ACL) |
| the shell's log | `stderr_is_seen`, `redirect_output`, `attach_terminal` | stderr not a terminal (`isatty`): `logs/shell.log` `dup2`'d onto fd 2 | no console window (`GetConsoleWindow`) and no open standard error handle: `logs/shell.log` made standard output and error (`SetStdHandle`); a flag run from a console attaches to it (`AttachConsole(ATTACH_PARENT_PROCESS)`) |

**The pipe name** is the supervisor's rule
(`internal/supervisor/host/host_windows.go`, `Endpoint`):
`\\.\pipe\kivali-` followed by the first 8 lower-case hex characters of
the SHA-256 of `strings.ToLower(filepath.Clean(filepath.Abs(dir)))`,
where `dir` is the team's supervisor folder. The shell makes the path
absolute the same way (`GetFullPathNameW`), then collapses repeated
separators, drops a trailing one, and lower-cases character by
character as Go does (`pipe_key`). Both sides test the same vector
(the platform module's tests and `internal/supervisor/broker`'s).

**A squatted pipe.** Another local account could create the pipe name
before the supervisor does. Every connection (`connect`,
`is_listening`, `peer_pid`) therefore checks who serves the pipe:
`GetNamedPipeServerProcessId`, then that process's token user
(`OpenProcess` with `PROCESS_QUERY_LIMITED_INFORMATION`,
`OpenProcessToken`, `GetTokenInformation(TokenUser)`) compared with
this process's (`EqualSid`). On a mismatch the connection is closed,
the endpoint counts as not listening, and the shell logs one fixed
line, once: `kivali: the supervisor pipe is held by another account`.
The client also opens the pipe with `SECURITY_IDENTIFICATION`, so a
server can never impersonate it.

`Sidecar::start` has a 15-second deadline on every platform, so a
transport that never answers cannot hang the shell.

**Windows.** The crate builds, passes its tests and lints warning-free
on Windows (`x86_64-pc-windows-msvc`, MSVC Build Tools), and
`make build-debug` produces the NSIS installer; "Known gaps" lists
what no test covers there.

## Teams and their files

Everything lives in the config directory (`platform::config_dir`:
macOS `~/Library/Application Support/Kivali`, Windows
`%LOCALAPPDATA%\Kivali`), deliberately not Tauri's bundle-id directory,
because the command-line supervisor uses it without the shell.

```
<config dir>/
  teams.json               the shell's list of teams (teams.rs)
  teams.json.bak           the file as it was before a repair
  shell.lock               the instance lock
  teams/<id>/              one supervisor config directory per team here:
    local.json             the supervisor's (supervisor.md, "local.json")
    data.img, downloads/   the team's disk and release downloads (the supervisor's)
    supervisor.sock        its RPC endpoint (macOS)
    supervisor.pid         the serve this shell started or adopted
    logs/supervisor.log    serve's output (the shell appends it), console.log
    terminal.command       the script Terminal runs for Claude's sign-in (macOS)
```

**`teams.json`** is the shell's (`src-tauri/src/teams.rs`):

```json
{
  "teams": [
    {"id": "plainsong-3f2a", "name": "Plainsong", "kind": "work", "place": "here",
     "dir": "teams/plainsong-3f2a", "port": 8080, "owner": "dana@example.com",
     "memory_mb": 4096, "cpus": 4, "paused_at": "2026-10-04T18:02:11Z"},
    {"id": "9f3a0c12", "name": "Studio", "place": "elsewhere", "url": "https://dana-imac.local:8443"}
  ],
  "start_at_login": true,
  "ask_before_quit": true,
  "running_at_quit": ["plainsong-3f2a"],
  "last_open": "plainsong-3f2a"
}
```

- A team **here** (`place: "here"`) has its own supervisor folder
  (`dir`, relative to the config directory, always `teams/<one
  segment>`), the port its forward last used (0 before the first
  start), the owner's Google account, its VM's memory and CPUs (default
  4096 MB and 4), its kind (`work` or `personal`) and when it was last
  paused; also, when known, when Claude signed out (`signed_out_at`,
  D4: stamped by the watch loop on a signed-in to signed-out change,
  cleared by any sign-in). A team **elsewhere** has only its origin.
- New ids are the name's ASCII letters and digits, lower-cased,
  non-alphanumerics folded to single dashes, at most 16 characters,
  then four random hex digits (`plainsong-3f2a`; `team-<hex>` when
  nothing is left). They stay short so the supervisor's socket path
  stays under macOS's 104-byte limit. A team elsewhere's id is an FNV-1a
  hash of its origin; adding an origin already listed returns that team.
- `running_at_quit` names the teams here that were running when Kivali
  last quit; they resume at the next launch. `last_open` is the team
  whose window opened last ("Open Kivali" reopens it). The order of
  `teams` is the order the tray and Settings list them in.
- Written atomically (temporary file + rename), owner-only (macOS:
  mode 0600, in a directory created 0700; Windows: the profile's ACL).
- Read tolerantly: each team entry is parsed on its own and a bad one
  costs only itself (no id, an id with a slash or `.`/`..`, a repeated
  id, an unknown place, a folder not under `teams/` or used twice, an
  unusable address). Empty names get a default (the host, else
  "Team"), addresses are reduced to their origin, flags that are not
  booleans take their default, and ids in `running_at_quit` and
  `last_open` that name no team are dropped. A file that needed any
  repair is copied to `teams.json.bak` before the repaired one replaces
  it. A file that is not a JSON object is renamed to
  `teams.json.unreadable-<unix seconds>` and Kivali starts with no
  teams. Keys this version does not know are kept.
- A team here's port follows what its supervisor reports after each
  start.

**`local.json`** is each supervisor's. The shell never reads it
directly; it gets the same facts from `GET /v1/status`.

**Start Kivali at login** is `start_at_login` in teams.json (Settings →
General). The login item (through `tauri-plugin-autostart`; macOS: a
LaunchAgent, Windows: a `Run` registry value) is made to match the file
at every launch, and starts the app with `--autostart`: the teams in
`running_at_quit` resume and nothing opens but the tray.

## App windows

Three kinds, and a team's web app and the bundled pages never share a
webview (`windows.rs`):

| Window | Label | Size | Shows |
| --- | --- | --- | --- |
| Setup | `setup` | 680 × 580, fixed | adding a team: `#/setup/welcome` (A1, first launch, "Welcome to Kivali"), `#/setup/add` (A1 as "Add a team", with Cancel), `#/setup/new` (the create steps A2–A20, "New team"), `#/connect` (B1–B6, "Connect to a team") |
| Settings | `settings` | 880 × 640, fixed width, taller allowed | `#/settings/general` (D1, D11), `#/settings/team/<id>[/overview\|ai\|mac]` (D2–D6, or D9 for a team elsewhere), `#/settings/advanced` (D10) |
| A team | `team-<id>` | 1200 × 760, resizable, at least 900 × 600 | two child webviews in the same place, one shown at a time (below) |

A team's window is titled with its name, plus `· <host>` for a team
elsewhere. It holds:

- **`web-<id>`**, the team's own served web app, loaded as a remote URL
  (`http://127.0.0.1:<port>/` for a team here, the team's `https://`
  origin for one elsewhere) and pinned to that origin. No capability
  names it, so it has no IPC.
- **`page-<id>`**, the bundled page `#/team/<id>`, shown when the team
  cannot show itself: paused (C4), waking up, pausing, applying a
  change or deleting (C5), updating (C6, with its four steps),
  couldn't start (C7, with the reason when known, Show details and Try
  again or "Pause X and resume"), or a team elsewhere that does not
  answer (C8, retried quietly). It has IPC.

`sync_team` decides which shows on every change: a team here shows its
web app only while it is running and has a port; a team elsewhere shows
it unless the last reachability check failed (or while it is being
connected). When the web app shows and the team's origin moved (the
first start, or a port the supervisor changed), the pin moves and the
web view navigates there first. Windows of teams that are gone are
destroyed.

Choosing a team in the tray, the end of setup, or a sign-in finishing
opens its window or brings it to the front (`show_team`), and records
it as `last_open`. **Open Kivali** (the tray, the Dock's reopen, a
second launch) opens the `last_open` team's window, else the first
team's, else setup's welcome. Closing any window hides it: the team
keeps running and Kivali stays in the tray. **Quit** is the app's own
exit ("Launch and quit").

**The team web view is pinned to its team's origin** (scheme, host and
port), its frames included, and only that origin loads (`team_nav`).
On macOS wry asks the shell about every navigation, the page's and its
frames' alike. On Windows wry hands over only the page's (WebView2's
`NavigationStarting`), so the shell hooks WebView2's
`FrameNavigationStarting` itself (`platform::guard_frames`, installed
in the same turn of the main thread that creates the web view, before
its first page can start a frame) and cancels every frame
`frame_may_load` refuses: a frame loads exactly what `team_nav` loads
as is, checked against the pin as it stands. Any other web page,
including another team's URL, a different port on the same host
(another team here), or a sign-in page such as Google's, is refused in
the window and opened in the system browser instead. The app's own
origin (`tauri://`, `tauri.localhost`, `ipc.localhost`, the dev server)
and non-web schemes (`file:`, `data:`, `asset:`, foreign `blob:`) are
refused outright; `about:blank`/`about:srcdoc` frames and the team's own
`blob:` URLs load. Links that open a new window go to the system
browser. The team's own `/auth/login` (that exact path) loads only with
`client=desktop` in its query; without it, the shell starts the desktop
sign-in, which loads it again with that parameter and the return
address ("Signing in to a team" below).

A frame the Windows hook refuses is cancelled and logged with its
origin only (`kivali: web-<id> refused a frame of <origin>`), and that
is all: it never opens the browser and never starts the desktop
sign-in, so the team's own `/auth/login` without `client=desktop` does
not load in a frame either. WebView2 does not hold the frame's request
back while the shell answers, so the request still reaches the refused
address (as an image or a `fetch` from the page could), but the answer
never shows: the frame's document is never committed. On macOS wry
cannot tell a frame from the page, so a refused frame takes the page's
way, to the browser through the gate below.

| Navigation in a team's web view | What happens |
| --- | --- |
| the pinned origin, `/auth/login` without `client=desktop` | refused; the desktop sign-in starts its listener and loads it again with `client=desktop`, `return_port` and `return_token` |
| the pinned origin, any other path (including `/auth/login?…client=desktop`, as is, and `/auth/callback`) | loads |
| `about:blank`, `about:srcdoc`, the pinned origin's `blob:` | loads |
| any other `http`/`https` page (another team, another port, Google) | refused, opened in the system browser |
| the app's origin, `file:`, `data:`, `asset:`, foreign `blob:` | refused |

Opening the system browser is rate-limited per team window, because
every refused frame or scripted navigation would otherwise open one,
and a hostile page could loop `location.assign` or add iframes to drive
the person's browser through any URL without a click. A team window
opens the browser **at most once a second**, and **only while it has
focus or lost it less than three seconds ago**; everything else is
dropped and logged with the origin only (`BrowserGate`). (`data:`
frames stay refused on both platforms: on macOS wry hands the shell
only the URL, so a frame cannot be told from a top-level navigation,
and Windows follows the same table.)

The bundled pages' webviews (`setup`, `settings`, `page-<id>`) may load
only the app's own origin (`tauri://localhost` on macOS,
`http://tauri.localhost` on Windows), plus the Vite dev server
(`http://localhost:1420`) in a debug build (`page_may_load`).

All teams share the webview's one website data store (cookies, local
storage, caches) of the app (macOS: WKWebView's; Windows: WebView2's).
Teams on different hosts are isolated exactly as in a browser, by the
same-origin policy. Teams here all live on `127.0.0.1`, and cookies
ignore the port, so each team here created by this app runs with
`KIVALI_COOKIE_SUFFIX=<team id>` ([auth.md](auth.md), "Cookies"): its cookies
are `kivali_session_<id>` and `kivali_oauth_state_<id>`, and signing in
to one team never signs another out. Local storage is per origin, port
included, so it needs nothing. Nothing else is partitioned.

The **View** menu acts on the team window in front: Reload reloads
whichever webview shows; Actual size, Zoom in and Zoom out set both
webviews' zoom (0.5 to 2.0 in steps of 0.1, per team, for this run).

## Signing in to a team

Google refuses its consent page inside an embedded webview, and the
system browser's cookie jar is not the team window's, so a sign-in that
merely bounced out to the browser would leave the session in the
browser. The team window starts and finishes the sign-in, holding its
state cookie throughout, and only Google's leg runs in the system
browser, which hands Google's answer back to a listener the app runs on
`127.0.0.1` for the length of the sign-in, the loopback redirect of
RFC 8252 section 7.3 (`signin.rs`; the server's half is [auth.md](auth.md),
"Desktop sign-in"). No URL scheme is registered, the browser asks
nothing, and nothing else is needed on Windows. The same flow serves a
team here (whose web app asks its owner to sign in) and a team
elsewhere.

When the team page navigates to `<origin>/auth/login?…` without
`client=desktop`, `team_nav` answers `DesktopLogin`. The shell closes
that team's earlier pending sign-in, if any, binds `127.0.0.1:0` (port
`P`), makes a token `T` (32 random bytes, base64url without padding, 43
characters), keeps origin, port, token, listener and start time in
memory per team, and loads the same URL in the team's web view with
`client=desktop&return_port=P&return_token=T` appended (the original
query, `next` included, is kept). The load runs from another thread,
and is dropped if the team's pin moved meanwhile. The server sets its
state cookie in the webview, puts `P` and `T` in the sign-in's nonce
and redirects to Google, which is off the origin and so opens in the
system browser through the browser gate, like any other page. Its
callback then sends the browser, which has no cookie, to
`http://127.0.0.1:P/signin/T?code=…&state=…` (or `?error=…&state=…`
when the person declined).

The listener serves on its own thread, reading at most 8 KiB of a
request head, which must arrive whole within 5 seconds. A `GET` whose
(percent-decoded) path is exactly `/signin/T`, compared in constant
time, gets a small self-contained `200` page ("Signed in to Kivali. You
can close this tab and return to the app.", or "Sign-in was not
completed (<error>)…" with the error HTML-escaped and cut to 64
characters), `Cache-Control: no-store`, `Connection: close`. The
listener then closes, and the shell consumes the pending sign-in, moves
the team's pin to the callback's origin and loads
`<origin>/auth/callback?<the same query, re-encoded>` in the team's web
view, where the server checks the state against the webview's cookie
and sets the session; the team's window comes forward (a team being
connected shows only once the outcome is known, below). Anything else
(another path or token, another method, a head over 8 KiB, a connection
that sends nothing) gets a `404` and the listener keeps waiting. It
closes without a return when a newer sign-in of the same team starts,
the team window closes, or after ten minutes, and the browser then
finds nothing listening (connection refused); only a matching request
in the instant before the ten-minute timer fires gets a `410` page
saying to try again. The token, the code and the state are never
logged; a refusal logs one fixed line,
`kivali: sign-in listener: refused a request`. The pending sign-ins
live only in the running process, and nothing is registered with the
system, so `make dev` exercises the same flow as the packaged app.

**Signing out of a team elsewhere** (D9 Sign out) loads the team's own
`/auth/logout` in its window.

## The team API

What the shell shows about the inside of a team (the tray's
"Plainsong · 3 agents working", D2's "3 agents working", D9's "Signed
in as dana@example.com", E1's "3 agents are working", E7's "6 agents and
what they remember" and "1,204 files") comes from the team's own
server, asked as the person signed in in the team's window
(`teamapi.rs`). The shell holds no session of its own: it borrows the
one the team's web view (`web-<id>`) already has.

- **The endpoint** (`internal/web/api_desktop.go`): `GET
  /api/v1/desktop/facts`, behind the ordinary session middleware (a
  JSON 401 without a session), answers `{"email", "agents", "working",
  "files"}`: the session's email; the hired agents, the person's own
  `ceo` seat not counted (the agents the web app lists); those working
  as the web app's Home counts them (a turn running or background work
  out, `Readouts.Working`); and the team's project files, what its Files
  page lists (`GET /api/v1/org/files`; agents' private workspaces are not
  counted). The web app does not read it.
- **The cookie.** Off the main thread (`spawn_blocking`), the shell
  reads the whole webview cookie store (`Webview::cookies`; reading it
  waits on the main thread, and WebView2 deadlocks when asked from a
  synchronous handler, so it is never read on the main thread). It
  does not use `cookies_for_url`, which matches a cookie's domain
  against the URL's *domain* and so finds nothing for `127.0.0.1`. It
  keeps the cookies whose domain is exactly the team's host (the server
  sets host-only cookies), drops a `Secure` one for plain http off
  loopback, and sends, for a team here, only that team's session cookie
  (`kivali_session_<id>`, since every team here shares 127.0.0.1),
  for a team elsewhere every cookie
  of its origin, as the browser would. One request (reqwest, no
  redirects, 10 seconds); the cookie values are never logged.
- **When.** For each team that runs (a team here running, a team
  elsewhere not known to be unreachable) and has a window: on the watch
  loop's slow tick, when its window gains focus or finishes loading a
  page outside `/auth/`, and when one of its operations succeeds;
  `team_facts` reads fresh. A team without a window, or not running,
  is not asked and keeps what was read last.
- **Stored** per team (`Runtime::facts` here, `Reach::facts` elsewhere,
  in memory only) and shown in the snapshot as `agents`, `working`
  (only while the team runs), `files` and `signed_in_as`. A 401/403, or
  a window with no session cookie, clears them (signed out there); any
  other failure keeps the last ones.

## The owner's Google sign-in (setup)

Creating a team asks who owns it by signing in with Google once, in
setup (A5–A8, `ownersignin.rs`). This is the only time the app uses a
Google client of its own, and it reads only the email. It runs through
Kivali's public Google client and its relay exactly as a server does
([auth.md](auth.md), "The public client and the relay"), with the app as the
install:

1. **Continue with Google** binds `127.0.0.1:0` (port `P`) and opens
   Google's consent page in the system browser: the public client id
   (`PUBLIC_CLIENT_ID`, the same as `internal/auth/public_client.go`'s
   `DefaultPublicClientID`; a test reads the Go file), `redirect_uri`
   the relay's callback (`https://kivali.ai/oauth/google/callback`),
   scope `openid email`, `prompt=select_account`, PKCE (S256 over a
   48-byte verifier), a 32-byte nonce, and `state` =
   `<nonce>.<base64url("http://127.0.0.1:P/auth/callback")>`.
2. The relay bounces the browser to that loopback address with the
   code. The listener (the same head reader as above: 8 KiB, 5 seconds)
   accepts only `GET /auth/callback` whose `state` equals this
   sign-in's (constant time); anything else gets a `404` and it keeps
   waiting. A `code` gets "Signed in. You can close this tab and return
   to Kivali."; an `error` gets "Sign-in was not completed (…)".
3. The app trades the code at the relay's token route
   (`https://kivali.ai/oauth/google/token`, form-encoded, with the
   verifier; no redirects followed, 10-second limit) and trusts only
   the `id_token` in the answer, checked as the server checks one
   (`internal/auth/oauth.go`, `emailFromIDToken`): RS256, a key from
   Google's published JWKS (`kid` match), issuer
   `https://accounts.google.com` or `accounts.google.com`, audience
   the public client, not expired, this sign-in's nonce (constant
   time), and `email_verified` true. The email, lower-cased, is the
   owner.

The page shows A6 while waiting (with **Open the browser again**,
which starts a fresh sign-in and closes the previous listener), A7 with the
account (and **Use a different account**), or A8 when the browser did
not come back within five minutes or the check failed. Nothing is
stored; the token is dropped once the email is read, and setup's end
clears the answer. `create_team` refuses an owner other than the email
this sign-in produced.

## Capabilities: why team web apps get nothing

The only capability, `shell-pages`, names the `setup` and `settings`
windows and the `setup`, `settings` and `page-*` webviews, on local
origins only, and lists the app's own commands plus event listening.
There is no `remote` entry, no `dangerousRemoteDomainIpcAccess`, and no
entry that matches `team-*` windows or `web-*` webviews, so:

- Tauri's ACL refuses any command from a remote origin outright, and
  `build.rs` declares an app manifest so even the app's own commands
  need an `allow-*` grant; none is granted to a team's web view. A
  test (`commands::tests`) checks that build.rs and the capability list
  every command in `COMMANDS`.
- Every command also checks that its caller's webview label is
  `setup`, `settings` or `page-<id>` (`is_page_label`), so an error in
  the capability file alone does not expose one.
- A team's web view cannot navigate to the app's origin, so it never
  runs the bundled pages either; the page webview beside it in the
  same window is a separate webview with its own label.

The reasons: a team elsewhere is third-party content
from the shell's point of view, and a team here is served by a server
agents can influence. Neither may reach a supervisor, which can boot,
upgrade, delete the team and open a root shell in it. Each
supervisor's endpoint is owner-only (macOS: a Unix socket; Windows: a
named pipe) and a web page can open neither, so the IPC bridge would
be the only path, and it is closed.

The plugins (updater, autostart, dialog, opener, notification) are
used from Rust only; none of their JavaScript permissions is granted.
The bundled pages run under a CSP that allows only their own scripts,
styles and fonts (`style-src 'self'`, no inline styles: sizes that
depend on a value are set through CSSOM), `data:` images, and IPC.

## Talking to the supervisors

Every team here has a `Runtime` in the shell (`shell.rs`): its own
`kivali-supervisor --config-dir <its folder> serve`, its last status,
its credential status, its one long operation, its release check.
Everything local goes through `src-tauri/src/supervisor/`, one
`Supervisor` per team, whose typed calls are:

| Shell call | Supervisor |
| --- | --- |
| `report()` / `status()` | `GET /v1/status`: the `Report` (incl. `terminals` and the disk fields), reduced to absent / stopped / starting / running / upgrading / failed, port, Kivali version, message |
| `up(req, progress)` | `POST /v1/up {"install": {"owner", "env"}, "port_hint", "memory_mb", "cpus", "prepare"}` |
| `credential()` | `GET /v1/credential`: signed in or not, the account's email and what it bills, as `claude auth status` reports them |
| `setup_info()`, `apply_setup(req)`, `clear_provider()` | `GET`/`POST /v1/credential/setup`, `POST /v1/credential/clear-provider` (the Foundry sheet; clearing before a Terminal sign-in) |
| `handoff()` | `POST /v1/handoff`, once, when setup opens the new team |
| `set_address(url, progress)` | `POST /v1/address {"external_url"}` (Other devices) |
| `destroy(exit, progress)` | `POST /v1/destroy {"exit"}`, answers `freed_bytes` |
| `down(exit, progress)`, `down_within` | `POST /v1/down {"exit"}` |
| `check()` | `POST /v1/check`, plus the last successful check's time from `GET /v1/status` |
| `upgrade(progress)` | `POST /v1/upgrade` |
| `install(progress)` | `POST /v1/install` (wired, not used) |
| terminal | the system terminal runs `kivali-supervisor --config-dir <team dir> terminal` (`platform::open_terminal`) |

**What `up` is sent** (`Shell::up_request`), on every start (create,
resume, retry): the owner, the team's memory and CPUs, and while the
team has no port yet a `port_hint`, the first port from 8080 that no
other team in teams.json has recorded (the supervisor still moves off
one some other program holds, and records what it used; the shell then
writes it to teams.json). For a team in `teams/<id>` it also sends the
server environment `KIVALI_SEED_ORG_NAME=<name>`,
`KIVALI_TEAM_KIND=work|personal` and `KIVALI_COOKIE_SUFFIX=<id>`
([../configuration.md](../configuration.md)); the supervisor uses the install fields only on
the team's first install, so a create cut short by a quit still
installs on the next start. `up` carries no credential: the team signs
in to Claude in the terminal once it runs ("Claude's sign-in in
Terminal").

The RPC is HTTP/1.1 over the platform's owner-only endpoint (macOS:
the socket `<team dir>/supervisor.sock`; Windows: the named pipe
`\\.\pipe\kivali-<hash>`), spoken by the shell's own small client,
`supervisor/http.rs`, over the stream `platform::connect` returns:
one request per connection, `Host: kivali`, a JSON body with its
`Content-Length`, `Connection: close` (or `Upgrade`), and an answer
read by `Content-Length`, by chunked transfer coding (Go's net/http
streams operations that way) or to the connection's close, with heads
capped at 64 KiB. `Supervisor::open_stream` sends `Upgrade:
kivali-stream` and hands back the raw stream after `101 Switching
Protocols`, for `/v1/terminal` and `/v1/exec`; nothing in the shell
uses it yet. Operations answer with NDJSON: `{"log": …, "stage": …}`
lines, then one `{"done": true, "error" | "result"}` line. Only the
fields the shell reads are declared, so the supervisor can add fields
freely; `wire.rs` is shared by path with the fake, and its tests assert
the same JSON `internal/supervisor`'s `TestWireShapes` asserts on the
Go side. The terminal goes through the command, never the shell: it is
a PTY the shell must never see; the system terminal runs the command
(macOS: the script `<team dir>/terminal.command`, 0700, opened with
`open -a Terminal`; Windows: `wt.exe new-tab -- <exe> --config-dir
<dir> terminal`, else `cmd.exe /c "start "" "<exe>" --config-dir
"<dir>" terminal"`).

`src-tauri/tests/supervisor_e2e.rs` runs the client against the fake
(create with its stages, the login check, the credential status, check,
upgrade, destroy with exit, and the RPC-first stop)
and, with `KIVALI_REAL_SUPERVISOR=<binary>`, against the real `serve`
(status, check, `down --exit`; no VM boot), over the socket on macOS
and the named pipe on Windows. The fake serves the pipe as the
supervisor does (`fake-supervisor/src/pipe.rs`): the same name rule, a
DACL granting only the current user, one instance per connection with
the next already waiting, and each answer ended by closing the instance,
so the client reads what is left and then the end
(`DisconnectNamedPipe` would discard unread bytes and fail the client's
next read instead). On Windows the real `serve` only looks for a
`root.vhdx` in its VM folder before it answers, so without
`KIVALI_VM_DIR` the test hands it an empty one.

**Operations and stages.** Each team runs one long operation at a time
(`run_op`, on its own thread); asking for a second while one runs is
refused, as is any operation once Quit has begun. `view.rs` maps the
supervisor's stage ids to the words and steps the pages show, with a
typical length per stage for the progress bar and "about N seconds":

| Operation (`OpKind`) | Activity | Stages shown (stage id) |
| --- | --- | --- |
| create | creating | Making room (`making-room`) · Starting up (`starting`) · Setting up your team (`setting-up`) · Ready |
| resume | waking | Starting up · Setting up your team |
| update | updating | Downloading (`downloading`) · Saving a snapshot (`snapshot`) · Installing (`installing`) · Starting up |
| pause | pausing | Pausing (`pausing`) |
| address | applying | Applying the change (`setting-up`) |
| delete | deleting | Deleting (`deleting`) |

A line whose stage this kind does not know keeps the step it had. The
share done counts earlier stages whole and the current one by time
spent against its typical length, never past 95% of it, and never
above 99% until the operation finishes. Every raw line is kept for
"Show details".

**Lifecycle.**

- *One shell.* At startup the shell takes the instance lock on
  `<config dir>/shell.lock` (`platform::InstanceLock`; macOS: an
  exclusive, non-blocking `flock`; Windows: the file opened with no
  sharing) and holds it for its lifetime (the OS drops it however the
  process ends). A second launch finds it held and exits without
  touching teams.json or any supervisor: on Windows it first asks the
  running shell to open Kivali (a named event the first shell waits
  on); on macOS it shows "Kivali is already running. Use the Kivali
  icon in the menu bar." It sets up no shell state, window, tray or
  menu, so no IPC command can run in it, and a reopen while its dialog
  shows (macOS: a Dock click) does nothing.
- *One supervisor per running team.* The shell starts
  `kivali-supervisor --config-dir <team dir> serve [--vm-dir …]`
  (`platform::spawn_serve`; Windows: without a console window,
  detached) when a team is created or resumed and nothing answers on
  its endpoint, creating the folder first, appends its output to
  `<team dir>/logs/supervisor.log`, waits until the endpoint answers,
  the process exits, or 15 seconds pass (then it kills the process and
  says so), and records the pid in `<team dir>/supervisor.pid`. A
  paused team's supervisor stays running (`down` without `exit` stops
  only the VM) until Quit.
- *Ownership.* A `serve` the shell started is **owned**: Quit stops it.
  A `serve` already listening on a team's endpoint at launch (or when
  the team is next started) is adopted as owned when that folder's
  `supervisor.pid` names the process serving the endpoint
  (`platform::peer_pid`; macOS: `LOCAL_PEERPID`; Windows:
  `GetNamedPipeServerProcessId`) and that process's parent is gone
  (`platform::is_orphan`; macOS: re-parented to launchd; Windows: the
  parent pid names no process, or one started after the child) or is
  this shell; that is the supervisor a crashed or killed shell left
  behind (`sidecar::adoptable`). Any other listening supervisor
  (started from a terminal) is used as is and left running at quit,
  minus its VM. An adopted process is not the shell's child, so its pid
  can die and be reused: the shell records its start time at adoption
  (`platform::start_time`; macOS: `proc_pidinfo`; Windows:
  `GetProcessTimes`) and checks it before every signal and every wait:
  a dead pid, or one now held by another process, is a `serve` already
  gone and is never signalled. It is waited for with
  `platform::wait_exit` (macOS: kqueue `EVFILT_PROC`/`NOTE_EXIT`;
  Windows: `WaitForSingleObject` on its handle), and the identity check
  runs after the wait is registered, so a pid reused before it is never
  waited on.
- *Stopping* is RPC-first everywhere (`Sidecar::stop`): `POST /v1/down
  {"exit": true}` on a connection that gives up after 30 seconds without
  a byte (`Supervisor::down_within`, `platform::connect_timeout`), so a
  `serve` that accepts but is wedged cannot block the stop; a shutdown
  that keeps logging keeps going. Then up to 30 seconds for the process
  to end; then the platform's stop request (`platform::request_stop`;
  macOS: SIGTERM, on which `serve` also stops the VM cleanly; Windows:
  none, the RPC is the request) and up to 60 seconds; then a kill
  (macOS: SIGKILL; Windows: `TerminateProcess`). The pid file is removed
  if it still names the process. On Windows a failed or timed-out
  `down` therefore ends in `TerminateProcess`, which leaves the VM
  running until the next start handles it.
- *Closing a runtime.* Quit, a delete and the system's exit close a
  team's sidecar slot under its lock: a `serve` that an operation was
  starting at that moment is stopped instead of stored, and the lock is
  never held across a stop or a start, so the exit path never waits on
  it.

Quit, the system's exits and app updates are under "Launch and quit".

**Sidecar paths.** The supervisor is `externalBin`
(`src-tauri/binaries/kivali-supervisor-<target triple>`, plus `.exe` on
Windows), which `make sidecar` writes: `go build ./cmd/kivali-supervisor`
for the real one, the fake's cargo build otherwise; on macOS it is
ad-hoc signed with the virtualization entitlement. Tauri copies it next
to the app's executable in both `tauri dev` (`target/debug/`) and the
bundle (macOS: `Contents/MacOS/`; Windows: the install directory,
`C:\Program Files\Kivali\kivali-supervisor.exe` beside `Kivali.exe`,
with the `vm` resource as `C:\Program Files\Kivali\vm`), so one lookup
serves both. On macOS Tauri re-signs it in the bundle with the app's
entitlements, which carry `com.apple.security.virtualization`. On
Windows the same file is also the broker service's binary
(`kivali-supervisor.exe broker`, registered by the installer), so the
installer stops that service before it replaces the file.

## Team state

Every surface shows the same four states (C9, `view.rs`), each with a
word beside its dot, never colour alone:

| State | Team here | Team elsewhere | Dot |
| --- | --- | --- | --- |
| running | the supervisor reports running | the last check reached it (or none has run yet) | success teal |
| starting | an operation runs (any kind, including pause and delete), or the supervisor reports starting or upgrading | | the paused grey, pulsing in the pages |
| paused | stopped, absent, or no supervisor answering | the last check did not reach it | ink-faint grey |
| couldn't start (`failed`) | the last start failed (until the next succeeds), or the supervisor reports failed | | danger red |

The phrase beside it (tray rows, the tooltip, Settings) is "running"
("3 agents working" or "1 agent working" when the team API says some
are; "Claude isn't signed in" when the credential check says so),
"getting ready", "waking up", "pausing", "updating", "applying a
change", "deleting", "paused", "couldn't start"; for a team elsewhere
"on <host>" or "can't reach <host>".

**Memory.** A team here may start only if its memory fits:
`resume_team` sums the memory of the other teams here that run or are
starting (not pausing), and refuses with `memory:<largest such team>`
when this team needs more than the host's memory minus a quarter of it
(at least 4 GB) minus that sum (`view::memory_free_mb`). The pages and
the menus answer that with E5 ("Not enough memory for Home. Home needs
4 GB, and this Mac has 2 GB free while Plainsong runs. Pause
Plainsong to resume Home?", the free amount being that same estimate,
which the snapshot carries per team as `memory_free_mb`), whose
confirm runs `pause_and_resume`:
pause the other team, then resume this one. A start that failed for
memory says so on C7 and in Settings, with "Pause X and resume".

## The flows

**First launch** (no teams) opens the setup window on
A1 with two choices: set up a new team, or connect to one.

**Setting up a team** (`#/setup/new`, `pages/setup.ts`, decisions in
`logic/setup.ts`). The step and the answers live in the page; the
team's progress comes from the snapshot.

1. *What is it for* (A2–A4): work or personal, then "What should we
   call it?" (at most 64 characters).
2. *Make it yours* (A5–A8): the owner's Google sign-in (above). Continue
   is the Google button until it has an account. **Continue** then calls
   `create_team`: the team is written to teams.json under a new id and
   folder and its create operation starts (the supervisor, then `up`),
   so it gets ready while the remaining steps are answered. The team is
   *provisional* until setup finishes (`finish_setup`). Going back past
   this step abandons it; changing the name or kind and continuing again
   abandons it and creates another; the same answers reuse it.
3. *Connect Claude* (A11–A15): Claude Code's own sign-in in Terminal
   ("Claude's sign-in in Terminal" below). **Open Claude sign-in** is
   offered once the team is running; Continue is disabled until the
   team is ready and signed in. The step names the choices Claude
   Code's sign-in offers (a Claude subscription, an Anthropic Console
   account, Amazon Bedrock or Google Vertex AI), offers **Use Microsoft
   Foundry…** (the Foundry sheet, `pages/foundry.ts`, which applies a
   sign-in setup through `POST /v1/credential/setup`; also in Settings →
   AI), and once signed in shows what the team bills (`ai.billing`). The
   getting-ready bar (Making room · Starting up · Setting up your team ·
   Ready; clicking it shows the raw lines) sits above the footer from
   here to the end.
4. *Ready* (A18 work, A19 personal): **Open** (`finish_setup`) hides
   the setup window, opens the team's window, and clears the owner
   sign-in. The window opens signed in as the owner who signed in at
   step 1, without a second Google sign-in
   (`windows::show_team_signed_in`): off the main thread the shell asks
   the team's supervisor for a one-time token (`POST /v1/handoff`,
   valid 2 minutes) and loads `<origin>/auth/handoff?t=<token>` in the
   team's web view, on its pinned origin, where the server sets the
   session and redirects to `/` ([auth.md](auth.md), "Desktop handoff"). On any
   failure the window stays as it opened and the ordinary sign-in
   follows. The token is never logged. The team's own web setup
   continues there and skips kind, name and AI, since the server was
   seeded with them.

If the create fails, A20 shows the last lines and the error, with
**Try again** (`retry_create`: abandon the team, then create it again
with the same answers) and Back. Abandoning (`abandon_team`, also Back
from step 3) refuses a team that has been
ready; otherwise it sends `destroy` with `exit` when the supervisor
answers (which also cancels a running `up`), waits for the operation,
stops the supervisor, removes `teams/<id>`, drops the team from
teams.json and closes its window.

**Claude's sign-in in Terminal** (A11–A15, Settings → AI's **Sign
in…** and **Sign in again…**). The app never reads the login.
`open_claude_signin` refuses unless the team is running, removes every
cloud provider variable from the CLI's settings (one left behind would
outrank the new sign-in), opens the
system terminal on `kivali-supervisor --config-dir <team dir> terminal`
(Claude Code in the team's server container, whose first run asks for
the login, which lands on the team's data volume; when it is already
signed in, `/login` there switches account or billing), records the
sign-in it opened on (`SigninWatch.from`: signed in, email, billing),
and starts watching. Claude Code runs there with `BROWSER` set
to a helper that hands its sign-in page to the terminal client, which
opens it in this computer's browser, pointed at Claude's
paste-the-code callback ([supervisor.md](supervisor.md), `terminal`): the person
signs in in their browser and pastes the code Claude shows into
Terminal, with no link to copy (Claude's printed link is wrapped across
lines, and a hand-joined copy breaks). While anyone signs in, the watch loop
asks that team's supervisor every 3 seconds for its status (whose
`terminals` counts attached terminal sessions) and its credential
status:

| What the shell sees (`shell::next_signin`) | Page | `ai.signin` |
| --- | --- | --- |
| a sign-in other than the one Terminal opened on | signed in (A14) | `idle` |
| a terminal session attached, no new sign-in yet | waiting on Terminal (A13) | `waiting` |
| the session it saw has ended, not signed in | not signed in yet (A15) | `closed` |
| the session it saw has ended, the old sign-in kept (Sign in again, nothing changed) | as before | `idle` |
| no session for 15 minutes after opening | the page stops waiting | `idle` |


**Connecting to a team** (`#/connect`, B1–B6, `pages/connect.ts`).

- *Address* (B1, B2): `connect_check` normalises it to an origin (a bare
  host gets `https://`; plain `http` only for loopback, since the
  server's cookies are `Secure` everywhere else) and checks that `GET
  /api/v1/login` answers 200 with JSON in the Login shape (10-second
  limit, no redirects, at most 64 KiB of body read). Before sign-in,
  the address reveals only that a Kivali team answers and its name.
  Failures are B4 (can't reach) or B5 (answers, but not as a Kivali
  team).
- *Sign in* (B3): `connect_signin` uses the team's own Google sign-in,
  the one its web page uses, through the desktop sign-in above. An
  origin already listed just opens that team. Otherwise the team is
  held aside (`connecting`, listed nowhere), and a hidden team window
  loads `<origin>/auth/login?next=%2F`, which starts the desktop
  sign-in. Each page the hidden web view finishes decides the outcome
  (`connect_page_loaded`): `/auth/not-invited` ([auth.md](auth.md): the
  allowlist did not admit that account) is B6, which names the refused
  account: before that window closes, the shell reads the team's
  `kivali_denied` cookie ([auth.md](auth.md)) from its webview's cookie store,
  off the main thread, into `connect.email` (`teamapi::note_denied`;
  without it the page says "This Google account"); `/auth/callback` still
  showing (the server's error page) is "Sign-in didn't finish"; in both
  the hidden window closes. Any other path outside `/auth/` and `/login` means
  the team's app loaded signed in: the team is added to teams.json as
  `last_open`, the setup window hides and the team's window shows.
  **Use a different account** (`connect_reset`) drops the attempt and
  its window.

**Later launches** open the `last_open` team's window (or setup's
welcome when there are no teams), adopt supervisors left running, and
resume the teams in `running_at_quit` ("Launch and quit").

**Pausing and resuming.** Pause (E1, from the Team menu or
Settings) runs `down` without `exit`: the VM stops, the supervisor
keeps serving, and `paused_at` is recorded. Resume (C4, the menus,
Settings) is the memory check, then `up` with the progress page (C5).
A team that couldn't start offers Try again, which is resume.

**Updating a team** (E3, from the tray or Settings → team → Update…):
"Update Plainsong to 0.17? It takes about 3 minutes. Agents pause while
it updates. If anything goes wrong, Plainsong goes back to 0.16."
with Update, Cancel and What's new; the length is `view::typical` of
the update's stages (the snapshot's `update.takes`), in the page and
the native alert alike. The update operation runs
`POST /v1/upgrade` with C6's steps, then checks for releases again.

**Deleting a team here** (Settings → team → Overview, below a divider;
nowhere else):

1. E7 lists what goes: its agents and what they remember, its files,
   every chat and assignment, the Claude sign-in, and
   its disk: "6 agents and what they remember", "1,204 files", "18 GB
   on this Mac" (`team_facts` reads agents and files fresh from the team
   API, falling back to the last ones read, and gives the disk's
   allocated bytes; a line whose number is unknown carries none).
   **Keep Plainsong** is the default
   button; Continue leads to E8.
2. E8: **Delete forever** stays disabled until the typed name matches
   exactly. `delete_team` checks the name again.
3. F5: the system's own owner check (`platform::authenticate`; macOS:
   LocalAuthentication with Touch ID or the password, the reason
   string "delete the team <name>", which macOS shows as "Kivali is
   trying to delete the team <name>."; Windows: Windows Hello, else a
   confirmation). A cancel leaves E8 open, with no error.
4. The delete operation makes sure a supervisor answers, sends `destroy`
   with `exit` (the supervisor stops the VM and deletes the disk,
   snapshots, downloads, `local.json` and logs, then exits), waits for
   the process, removes `teams/<id>`, drops the team from teams.json, closes its
   window, and records the name and the bytes freed; Settings goes to
   General, which shows them (D11).

**Removing a team elsewhere** (E6, Settings → the team): it is dropped
from teams.json and its window closes; it keeps running where it is.

**Resources** (Settings → team → This Mac, D6): memory from 2 GB to the
host's total in whole gigabytes, CPUs from 1 to the host's count
(`set_resources` refuses anything else), stored in teams.json and sent
with the next `up`: "Changes apply the next time Plainsong resumes."
The page shows how the host's memory is shared and the disk's use.

**The watch loop** (`start_loops`, one thread, started at launch). It
wakes every 5 seconds, or every 3 while someone signs in to Claude
(above). Once a minute it also, for each team here with no operation
running and a supervisor answering (or a status remembered), refreshes
the status, notices a team that stopped on its own (a notification
when it had been running and Kivali is not quitting), and every 5
minutes asks a running team whether Claude is still signed in (a
notification when it signed out); and it asks each team elsewhere's
`GET /api/v1/login` again, every minute while unreachable, every 5
minutes otherwise, picking up a rename; and it reads every running
team's facts from the team API (below). A credential that goes from
signed in to signed out stamps the team's `signed_out_at`. Any change
redraws every
surface (`shell::changed`: the menus, the tray, the team windows, and
the `shell-changed` event the pages listen to). Opening the tray menu
also refreshes every idle team's status. The pages additionally poll
the snapshot once a second while an operation, a sign-in, a connect or
an app update is under way, since progress moves without events.

## Launch and quit

**Launch** (`lib.rs`, `shell::launch`): take the instance lock; load
teams.json (writing back repairs); reconcile the
login item; build the menu bar and the tray; then, off the main thread,
adopt each team's left-behind supervisor and refresh its status (a
running team gets its credential checked and its release check
started), resume every team in `running_at_quit` that is not running
(each through the memory check), and start the watch loop. Unless
started with `--autostart`, the `last_open` team's window opens (setup
when there are no teams). The app update check runs at every launch.

**Quit** (the tray, the menu bar with ⌘Q) asks first (E2, a native
alert, a sheet on the window in front on macOS) when `ask_before_quit`
is set and a team here is running or busy: "Quit Kivali? Plainsong and
Home pause until you open Kivali again." with Quit, Cancel and, on
macOS, a "Don't ask again" checkbox that turns `ask_before_quit` off
(Settings → General turns it back on). Quitting then waits for every
running operation, records the teams that run as `running_at_quit`
(stamping their `paused_at`), and stops every team's supervisor in
parallel: an owned one RPC-first as above, one it does not own with
`down` without `exit`. The run loop refuses every other exit request,
so closing the last window leaves Kivali in the tray. On Windows, if
that `down` fails or times out, the supervisor is terminated and the
VM is left running until the next start handles it.

**The system's exits.** Logout and shutdown (macOS: also Quit from the
Dock) do not go through Quit; the run loop ends with `RunEvent::Exit`
(macOS: AppKit's `applicationWillTerminate`) and the shell stops every
owned supervisor the same RPC-first way, in parallel. That path does
not rewrite `running_at_quit`, so the next launch resumes what the last
Quit recorded.

**Updating the app** (E4): "Restart to update Kivali? Kivali 0.18.0 is
ready. Running teams pause and resume after the restart." with Restart
and Later. Restart downloads and installs the update, then pauses every
team as Quit does (so they resume after the restart) and restarts
(macOS: the install returns first). On Windows the installer ends the
process inside the install call, so the teams pause in the updater's
before-exit hook (`platform::INSTALL_EXITS`). A failed install puts the
app back to normal with the error in Settings → General.

## Menus, tray, alerts and notifications

All native menus are rebuilt from the shell's state on every change
(`menus.rs`), and every menu routes clicks through one handler,
registered once (Tauri hands each menu event, the menu bar's, the
tray's or a window's, to every listener the app has). The menu bar
holds commands only, each with a verb, and hides what never applies to
the team in front; the tray is a glance at every team plus the common
commands. Teams open from the tray.

**Menu bar** (F1; on macOS the application menu, `platform::APP_MENU`;
on Windows inside each team window, below):

- **Kivali**: About Kivali · Open source licenses · Settings… ⌘, · Check for updates… (checks
  for an app update and opens Settings → General) · Hide Kivali · Hide
  others · Quit Kivali ⌘Q (the app's own item, not the predefined one,
  so it pauses teams first).
- **File**: New team… ⌘N (setup's welcome when there are no teams,
  else "Add a team") · Connect to a team… ⇧⌘N · Close window.
- **Edit**: Undo · Redo · Cut · Copy · Paste · Select all (the webviews
  need them for copy and paste).
- **View**: Reload ⌘R · Actual size ⌘0 · Zoom in ⌘= · Zoom out ⌘- ·
  Enter full screen ⌃⌘F.
- **Team**, present only while a team window is in front (or was last,
  while it is still showing), acting on that team: Pause Plainsong…
  (E1) / Resume Plainsong / Try starting Plainsong again, for a team
  here and not while it is starting · Plainsong settings… · Open in
  browser ⇧⌘B.
- **Window**: Minimize · Zoom · Bring all to front; macOS lists the
  open team windows itself.
- **Help**: Kivali help (the repository's docs) · What's new (the
  releases page) · Show logs (the team in front's `logs` folder, else
  `teams/`, else the config directory). `logs/shell.log` in the config directory holds the shell's own
`kivali:` lines whenever it runs without a terminal (from Finder,
Explorer, the Start menu or the login item; a Windows release build
never has a console of its own); run from a terminal they stay there,
as they do on Windows when standard error is redirected or piped.

**The menu bar on Windows** (C3, `platform::WINDOW_MENU`) is inside each
team window, set as the window is made (Tauri's per-window menu, so its
webviews are laid out under it from the start), with the same commands
in Windows' arrangement: no application menu and no Window menu, and
every team command acts on that window's own team (the items' ids carry
it, `reload:<team>`), so there is no team in front to find. The setup
and Settings windows have none: C3 is the team window's, and their
commands are on their pages, in the tray and in Settings.

- **File**: New team… Ctrl+N · Connect to a team… Ctrl+Shift+N ·
  Settings… Ctrl+, · Check for updates… · Close window Ctrl+W (hides it;
  the team keeps running) · Quit Kivali Ctrl+Q (the app's own, pausing
  teams first).
- **Edit**: Undo Ctrl+Z · Redo Ctrl+Y · Cut Ctrl+X · Copy Ctrl+C · Paste
  Ctrl+V · Select all Ctrl+A. The keys are WebView2's own, so these are
  Kivali's items, not muda's predefined ones: they show their shortcut
  without registering it, and choosing one focuses the window's webview
  and types the shortcut there (`platform::press_keys`). A predefined
  item registers Ctrl+C and answers it by typing Ctrl+C, which a window
  holding the keyboard focus itself would answer again, without end.
- **View**: Reload Ctrl+R · Actual size Ctrl+0 · Zoom in Ctrl+= (also
  Ctrl+Shift+=) · Zoom out Ctrl+-.
- **Team**: Pause Plainsong… (E1) / Resume Plainsong / Try starting
  Plainsong again, for a team here and not while it is starting ·
  Plainsong settings… · Open in browser Ctrl+Shift+B. Refilled in place
  when the team changes (taking the menu bar off and putting it back
  would resize the webviews twice).
- **Help**: Kivali help · What's new · Show logs (this team's `logs`
  folder, else `teams/`, else the config directory) · Open source
  licenses.

The shortcuts reach the menu from a focused webview through WebView2:
its keys go to WebView2's own window in its own process, so the event
loop's accelerator table (Tauri's `TranslateAcceleratorW` hook) never
sees them. Each of a team window's two webviews hooks the controller's
`AcceleratorKeyPressed` (`platform::menu_shortcuts`), spells each press
as the menus do ("Ctrl+Shift+N"; Ctrl and Shift from `GetKeyState`, Alt
from the event), runs the item it names and marks the key handled, so
the webview does not act on it too (WebView2's own Ctrl+R reload, say).
A held key repeats nothing. Alt, F10 and Alt+letter do not open the menu
bar from a webview; the mouse does.

**Dock.** Kivali is in the Dock only while one of its windows shows:
closing the last one switches it to the menu bar alone (macOS's
accessory activation policy, `windows::sync_dock`), and showing a window
puts it back. Started at login with no window, it starts out of the Dock.
Every alert first brings Kivali, and the window its sheet hangs from, to
the front, since one often answers a click in the tray or the menu bar
while another app is in front.

**Tray menu** (F2; left and right click open the same menu): Open
Kivali · one row per team, "Plainsong · running" with the team's status
dot (C9) as its image, which opens (or brings forward) the team's window;
pausing, resuming, its settings and Open in browser live in the Team
menu (the menu bar's; on Windows the team window's), the team's own
page and Settings, not in the tray ·
then at most one update item, the app's first ("Update Kivali to
0.18.0…", E4) else the first team here with one ("Update Plainsong to
0.17…", E3) · New team… · Connect to a team… · Settings… · Quit Kivali.

The status dots are drawn in code (`trayicon::dot_image`): a 16 px
canvas (@2x of 8 pt) with an anti-aliased 8 px dot in C9's colours,
light or dark by the menus' appearance (`platform::menus_dark`). The
starting dot does not pulse in a menu.

**Tray icon** (F3, `trayicon.rs`). Four states drawn from the logo's
bars and dots:

| State | Picture | Files (`<ink>` is `light` or `dark`) |
| --- | --- | --- |
| running | solid bars and dots, full strength | `tray-running-<ink>.png` |
| starting | the paused drawing pulsing 45% → 100% → 45% over 1.6 s: 10 frames of 160 ms | `tray-starting-<ink>-0.png` … `-9.png` |
| starting, Reduce motion | the paused drawing held at 50% | `tray-starting-<ink>.png` |
| paused (or no team here) | outlined, at 45% | `tray-paused-<ink>.png` |
| couldn't start | solid bars, red dots (`#B3261E`, dark `#F2877D`) | `tray-couldnt-start-<ink>.png` |

With several teams the icon shows the first that applies among the
teams here: couldn't start, starting, running, paused; so any running
team means running, and paused means nothing here runs. Teams
elsewhere never change it. There is no badge: what is waiting is said
in the tray menu and in notifications. The images are coloured, never
template images, so the red survives; the shell picks the ink itself
(`#1D1D1F` for a light menu bar, `#F5F5F5` for a dark one) through
`platform::menu_bar_dark` (macOS: the status bar window's effective
appearance, which follows the wallpaper; Windows: the taskbar's
theme), each time it sets a picture or a frame. The pulse is one timer
thread that runs only while the state is starting; Reduce motion
(`platform::reduce_motion`) is checked on every change and every frame
and stops it at the held picture. The tooltip is "Kivali · Plainsong
running · Home paused" (just "Kivali" with no team here). macOS uses
the 40×44 set (shown 18 pt tall), Windows the 32×32 `tray-win-` set.
`scripts/make-icons.mjs` renders both from SVG (viewBox 11 3 78 88, the
design's 20×22 pt icon) with rsvg-convert; `make icons` runs it; the
PNGs are committed. A test decodes every file and checks its size, its
ink, the red, the 45% and 50% alphas and the pulse's rise and fall.

**Alerts.** Every dialog is a sheet on the window it came from. Those
raised from native surfaces are native alerts (`platform::alert`;
macOS: NSAlert as a sheet on the parent window when one is showing,
else app-modal; the first button is the default, a "Cancel" button
answers Escape, and closing it otherwise answers Cancel).

On Windows the alert is a TaskDialog (`TaskDialogIndirect`, from
comctl32 v6, which the executable's manifest asks for: tauri-build
embeds the `Microsoft.Windows.Common-Controls` 6.0.0.0 dependency). Its
window title is "Kivali", its main instruction the alert's title, its
content the message; the buttons are the alert's own, in order and any
number of them (ids from 100), the first the default; Escape, Alt+F4
and the close box answer the Cancel button, as on macOS; "Don't ask
again" is its verification checkbox. With a parent window that shows,
the parent is brought forward (restored if minimised) and owns the
dialog, which disables it while the dialog shows and centres on it;
without one the dialog has no owner and takes the foreground itself.
TaskDialog has no destructive button style: an alert with a destructive
button (the owner check's stand-in where Windows Hello is not set up)
shows the warning icon instead. Each alert runs on a thread of its own,
and alerts queue, one at a time: on the main thread the dialog's modal
loop would run inside a tao event handler, which holds back every other
event (menus, the tray's picture, progress) and spins a core until it
closes, and from a command or a webview's event it would run inside
that callback.

| Alert | Raised from | Native |
| --- | --- | --- |
| E1 Pause a team | the Team menu (the menu bar's; on Windows the team window's) | yes (on the team's window when showing) |
| E1 | Settings → team → Pause… | yes, app-modal |
| E2 Quit | Quit | yes, with "Don't ask again" |
| E3 Update a team | the tray's update item | yes, app-modal |
| E3 | Settings → team → Update… | in-page sheet |
| E4 Update the app | the tray's update item, Settings → General → Update… | yes |
| E5 Resume wouldn't fit | Resume in the menus | yes |
| E5 | Resume on the team page or in Settings | in-page sheet |
| E6 Remove, E7 Delete · what goes, E8 Delete · say it, the key and account switches | Settings | in-page sheets |
| F5 owner check | E8 | the system's own prompt |

In-page sheets (`pages/dialogs.ts`) are drawn at the top of the page's
own window, with the same default and Escape buttons.

**Notifications** (F4, `platform::notify`), only while Kivali is not
the frontmost app (`platform::app_is_active`):

| When | Title | Body | Click opens |
| --- | --- | --- | --- |
| an update finished | Plainsong is updated | It's on Kivali 0.17. Agents have picked up where they left off. | the team's window |
| an update failed and the team still runs | Plainsong couldn't update | It's still on 0.16 and running. Open Settings for details. | its Settings page (`team/<id>`) |
| a running team stopped on its own | Plainsong stopped unexpectedly | Open Kivali to start it again. | the team's window |
| Claude signed out on a running team | Plainsong needs you | Claude signed out. Sign in again so agents can work. | its Settings AI tab (`team/<id>/ai`) |

Each notification carries a `platform::NotifyTarget` (`Team(id)` or
`Settings(route)`); `lib.rs` registers one handler at startup
(`platform::on_notification_click`) that runs on the main thread and
calls `windows::show_team` or `windows::show_settings`. On macOS the
notification goes through UNUserNotificationCenter with the target in
its `userInfo` (`kivali.target`: `team:<id>` or `settings:<route>`); the
first one asks for permission (alerts, no sound), and macOS asks the
person once. The center is used only from an app bundle (a bundle
identifier and a `.app` main bundle); a bare binary (`cargo test`,
`make dev`) falls back to the notification plugin, whose clicks only
bring Kivali forward, as on Windows and Linux.

## Update states

**A team's Kivali** (`updates.rs`, `shell::update_view`). Each team
here checks for a Kivali release once it is up (at launch for a team
already running, after every start), then every 24 hours, and again
after an update. The supervisor fetches `release.json` and gives a
verdict; the shell reduces it to one state, shown in Settings → team →
Overview (D2) and, when an update is available, as the tray's update
item:

| State | Settings line | Tray |
| --- | --- | --- |
| `not_checked` (no check yet, or Kivali not installed) | Kivali 0.16.0 | |
| `up_to_date` | Up to date · checked 3 hours ago | |
| `available` | 0.17.0 is available, with Update… (E3) and What's new | Update Plainsong to 0.17… |
| `desktop_first` | 0.17.0 is available · update this app first, in General | |
| `manual_steps` | 0.17.0 is available · it needs a few steps by hand, with What's new | |
| `failed` | Couldn't check for updates | |

"Update this app first" is the shell's own judgement: whenever the
release is newer than the team, the shell compares release.json's
minimum desktop version (`min_desktop_version` in the check result)
with its own version, `APP_VERSION`. The supervisor's verdict is then a
second opinion: it alone reports "failed" and "not installed", and its
"app too old" and "manual steps" stand even when the minimum did not
reach the shell. Versions compare as semver with an optional leading
`v`; an unreadable minimum counts as unmet. (`updates.rs` also
computes one-line labels and a badge, which no surface reads.)

**The app's own updates** use `tauri-plugin-updater` against
`https://github.com/kivali-ai/kivali/releases/latest/download/latest.json`,
checked at every launch and on **Check for updates** (the Kivali menu,
Settings → General). Settings → General shows "Checking for updates…",
"Up to date · checked …", "0.18.0 is available" with Update… (E4),
"Installing 0.18.0…" or "Couldn't check for updates · <error>"; the
tray offers the update item. Installing is E4 ("Launch and quit"). The
release's `latest.json` must carry, per platform it ships, an entry
whose `url` is the updater artifact the bundler writes with
`createUpdaterArtifacts` and whose `signature` is the contents of its
`.sig`, plus the top-level `version`: on macOS
`platforms."darwin-aarch64"` with `Kivali.app.tar.gz`; a Windows
release would add `platforms."windows-x86_64"` with the NSIS installer
(there is no Windows release).

## The bundled pages

Plain TypeScript, no framework, built by Vite into `dist/` (dev server
on port 1420). `index.html` has one `<div id="app">`; `main.ts` reads
the route from the hash (`logic/route.ts`), fetches the snapshot, and
renders the page for it: `pages/setup.ts`, `pages/connect.ts`,
`pages/team.ts` or `pages/settings.ts` (with `pages/dialogs.ts`). Every
render replaces the page's DOM from the snapshot and the page's own
state, keeping the focused element, its caret and the scroll position
of elements marked `data-keep-scroll`. Unknown routes fall back to the
welcome screen.

- Pages reach the shell only through `ipc.ts`: `invoke` and the
  `shell-changed` event, after which `main.ts` fetches the snapshot
  again and redraws if it changed. `ctx.act` runs a command and shows
  its error in the page's notice line ("cancelled" is silent).
- The pages use the Kivali design system the web app ships
  (`design-system/styles.css`, its tokens, fonts and logos, imported
  from `../design-system`; Vite inlines nothing, since the CSP's
  `font-src` is `'self'`). `ui.ts` builds its components (`kv-*`
  classes) as DOM, `h.ts` is a small element helper, `icons.ts` the
  Lucide outlines used, `style.css` the desktop's own layout.
- `theme.ts` follows the system appearance as the design system's
  `data-theme` on `<html>`; under `vite dev`, `?theme=` or a `G` state
  forces one.
- The decisions live in `logic/` and are tested in node with Vitest:
  `setup.ts` (which A screen shows, Back, the getting-ready bar, error
  words), `teams.ts` (dots and words, the team page, Settings rows,
  the E dialogs' words, when to poll), `format.ts` (sizes, times,
  versions, device names), `route.ts`.

**The snapshot** (`shell_snapshot`; `src/types.ts` `Snapshot`, built by
`Shell::snapshot`; a Rust test serialises one and checks every key
`types.ts` reads):

| Key | What |
| --- | --- |
| `app_version`, `platform` | this app's version; `macos`, `windows` or `linux` |
| `teams[]` | per team: `id`, `name`, `kind`, `place` (`here`/`elsewhere`), `url`, `device`, `state`, `activity`, `phrase`, `reason`, `pause_first` (the team to pause when memory stopped a start), `op` (the current or last operation: kind, running, error, finished, raw lines, stage, stage index, stages, start time, percent, time left), `version`, `update` (above, plus `takes`, an update's typical length in words, "about 3 minutes", from its stages), `ai` (`provider`, `signed_in`, `email` and `billing` as `claude auth status` reports them, `terminal_open`, `signin`, `signed_out_at` while Claude is signed out), `owner`, `memory_mb`, `cpus`, `disk_used_bytes`, `disk_size_bytes`, `paused_since`, `last_reached`, `provisional`, `agents`, `working` (only while running), `files`, `signed_in_as` ("The team API"; null until read), `memory_free_mb` (a team here: `view::memory_free_mb` beside the other teams running now, E5's "2 GB free"; null elsewhere) |
| `host` | this computer's memory and CPUs |
| `settings` | `start_at_login`, `ask_before_quit` |
| `app_update` | the app update's state, version, check time, error |
| `owner_signin` | the setup sign-in: `idle`, `waiting`, `signed_in` (with the email), `failed` |
| `connect` | the connect sign-in: `idle`, `signing_in`, `not_invited` (with `email`, the refused account, when its cookie was read), `done` (with the team), `failed` |
| `config_dir`, `logs_dir` | for Settings → Advanced |
| `last_deleted` | the last team deleted in this run and the bytes freed (D11) |

**Commands** (`commands.rs`; every one checks its caller's label):

| Command | What it does |
| --- | --- |
| `shell_snapshot` | the snapshot |
| `set_title`, `close_window` | the calling window's title (control characters dropped, at most 80); hide the calling window |
| `open_setup(route)`, `open_settings(id?, tab?)` | open setup at `welcome`/`add`/`connect`; open Settings at General, a team (and tab) or Advanced (no page calls `open_settings`) |
| `owner_signin_start`, `owner_signin_reset` | start (or restart) the owner's Google sign-in; forget it |
| `create_team(team)` | `{name, kind, owner, call_me}`: record the team, start its create; answers its id |
| `open_claude_signin(id)` | clear every cloud provider variable from the CLI's settings (`POST /v1/credential/clear-provider`), then Terminal on the team's `terminal` (setup's step 3, **Sign in…**, **Sign in again…**), and start watching for a sign-in other than the current one |
| `credential_setup(id)` | the catalog's model ids and the sign-in setup in force, without secrets (`GET /v1/credential/setup`) |
| `apply_credential_setup(id, setup, values)` | sign the team's Claude in with a sign-in setup the Claude driver declares and check each model (`POST /v1/credential/setup`); answers `{credential, models}` and records the credential. The Microsoft Foundry sheet (`pages/foundry.ts`) is its one caller |
| `finish_setup(id)` | hide setup, open the team's window signed in as the owner (the desktop handoff), clear the owner sign-in |
| `abandon_team(id)`, `retry_create(id)` | remove a provisional team and everything it made; that, then create it again (answers the new id) |
| `connect_check(address)` | normalise and check an address; `{origin, name, host}` or `{kind: unreachable \| not_kivali \| invalid, message}` |
| `connect_signin(origin, name)`, `connect_reset` | start B3's sign-in in a hidden window; drop it |
| `open_team(id)`, `open_team_page(id, path)` | open a team's window; open a path of its web app there (no page calls either) |
| `confirm_pause`, `resume_team`, `retry_team` (id) | show E1, then pause; resume (refused with `memory:<id>` when it would not fit); resume, or recheck a team elsewhere |
| `pause_and_resume(pause, resume)` | E5's confirm |
| `update_team(id)` | the Kivali update |
| `open_in_browser(id)` | the team's address in the system browser |
| `remove_team(id)` | forget a team elsewhere (E6) |
| `delete_team(id, typed)` | the name check, F5, then the delete |
| `team_facts(id)` | E7's numbers: agents and files read fresh from the team API (the last ones read when it can't be asked now; null when never), the disk's allocated bytes |
| `set_resources(id, memory_mb, cpus)` | D6 |
| `set_public_url(id, address)` | D7/D8: checks and records the operator's https address (null turns it off) |
| `sign_out_team(id)` | the team's `/auth/logout` in its window (D9) |
| `reorder_teams(ids)` | the order of `teams` (no page calls it) |
| `set_setting(key, value)` | `start_at_login` (also the login item) or `ask_before_quit` |
| `check_app_update`, `install_app_update` | check now; show E4 |
| `open_logs`, `open_config_dir` | Show logs (as Help → Show logs); the config directory in the file manager |
| `save_diagnostics` | ask where, then write the diagnostics file; answers its path or "cancelled" |
| `open_external(url)` | an `https` link (release notes) in the system browser |

**Settings → Advanced** (D10): **Logs** shows the logs folder of the
team in front (else `teams/`, else the config directory); **Settings
folder** shows the config directory; **Diagnostics** saves
`kivali-diagnostics-<date>.txt` where the person picks: the app's
version and OS, teams.json, and for the config directory and every
`teams/<id>` folder its `local.json` and the last 2000 lines of
`logs/supervisor.log` and `logs/console.log`. No team data and no
sign-ins: neither file holds a secret (the Claude sign-in and the
session key live only on each team's disk).

## Versions

There is one desktop version: `version` in
`desktop-app/src-tauri/tauri.conf.json`. `Cargo.toml` and
`package.json` follow it, and `build.rs` stops the build when either
disagrees. It is compiled in as `APP_VERSION` (what `--version`
prints, and what the minimum desktop version is compared with), and
`make sidecar` stamps the Go supervisor with it
(`-ldflags "-X main.version=<version>"`), so the app and its sidecar
carry one number (the root `make supervisor` stamps the same string).
`scripts/release.sh` bumps it with the rest of the release (chart
`appVersion` and the five desktop files), and
`make version-check` asserts they all agree; the release tag `vX.Y.Z` is
the source for everything else, and the release workflow refuses a tag
that differs from this version. [releasing.md](releasing.md) has the
whole procedure.

## Signing and release

[releasing.md](releasing.md) names every secret the release workflow
needs (certificate, keychain password, notarization, updater key) and
where each goes; `make desktop-release VERSION=vX.Y.Z` is the local
equivalent of the workflow's desktop jobs, on the platform it runs on. What a release needs, none of
which is in the repository (all but the updater key are macOS's; a
Windows build is not code-signed, and the preflight checks the
Apple material only for a darwin `TRIPLE`):

- **A Developer ID Application certificate** for the project owner.
  Set `APPLE_SIGNING_IDENTITY` (it overrides the ad-hoc `"-"` in
  tauri.conf.json); without it Gatekeeper blocks the app.
- **Notarization credentials**: `APPLE_ID`, `APPLE_PASSWORD` (an
  app-specific password) and `APPLE_TEAM_ID`, or an App Store Connect
  API key (`APPLE_API_KEY`, `APPLE_API_ISSUER`, `APPLE_API_KEY_PATH`).
  The bundler notarizes and staples when they are set.
- **The updater key.** `tauri.conf.json` carries the public key of a
  development keypair generated with `npx tauri signer generate`; its
  private key is `desktop-app/.keys/updater.key`, which is git-ignored
  and stays on the machine that made it. A release replaces the public
  key with the project owner's and builds with
  `TAURI_SIGNING_PRIVATE_KEY` (and its password) set to the owner's
  private key. An app signed with one key never accepts an update signed
  with the other, so the switch has to happen before the first public
  release.
- **The virtualization entitlement.** `Entitlements.plist` carries
  `com.apple.security.virtualization`; the bundler signs both the app
  and the sidecar with it under the hardened runtime. It is not a
  restricted entitlement, so Developer ID signing honours it without an
  Apple grant (unlike `com.apple.vm.networking`, which the design
  avoids).

`make build` refuses to proceed, listing every reason, when it would
ship development material: `TAURI_SIGNING_PRIVATE_KEY` unset (it would
fall back to the development key) or set to the development key,
tauri.conf.json still carrying the development public key, (macOS) an
ad-hoc signing identity (`APPLE_SIGNING_IDENTITY` unset or `-`) or no
notarization credentials, or versions that disagree
(`scripts/version-check.sh`).
`ALLOW_DEV_KEYS=1` overrides that for a local trial, never for
distribution. It also refuses the fake supervisor unless
`ALLOW_FAKE_SUPERVISOR=1`. `make build-debug` checks none of this, and
needs no updater key: without one (`TAURI_SIGNING_PRIVATE_KEY` unset or
empty, no `.keys/updater.key`) it bundles with `createUpdaterArtifacts`
off, so the debug bundle has no updater artifacts.

**The Windows bundle** (`tauri.windows.conf.json`) is one NSIS
installer, `Kivali_<version>_x64-setup.exe`, installed per machine: it
asks for elevation (`RequestExecutionLevel admin`), installs into
`C:\Program Files\Kivali` (`Kivali.exe`, `kivali-supervisor.exe`,
`vm\root.vhdx`, `vm\VERSION`, `uninstall.exe`) and records itself under
`HKLM`. It carries Kivali's icon and header and sidebar bitmaps
(`src-tauri/windows/installer-*.bmp`, rendered from the app icon), and
stores its files uncompressed (`compression: none`): the root disk is
a squashfs already compressed with zstd, so NSIS's default LZMA spent
minutes extracting a gigabyte it could not shrink. What the installer
cannot brand without an Authenticode certificate is the elevation
prompt, which names the publisher as unknown until one signs it.

Its hooks (`src-tauri/windows/installer-hooks.nsh`, Tauri's
`NSIS_HOOK_*` macros) look after the broker, the service
`kivali-broker` that `kivali-supervisor.exe broker` runs as, under its
own virtual account ([supervisor.md](supervisor.md), "The broker"):

- Before the files are copied, the broker is stopped (`net stop`, which
  waits for it), since an upgrade replaces the binary it runs.
- After, it is registered unless it already is (`kivali-supervisor.exe
  broker install`: automatic start, its event-log source) and started
  (`net start`). An upgrade keeps the registration, whose path does not
  change. A registration that fails is explained in a message that
  gives the command to run, and the install still completes.
- Before the files are removed, it is stopped and removed (`broker
  uninstall`). One that cannot be removed is explained (`sc.exe delete
  kivali-broker`) and the uninstall goes on. The uninstall an update
  runs keeps it.

Both ask whether Kivali is running (and offer to close it) before they
touch the broker, so a Cancel there leaves the service as it was.
Messages are skipped in passive mode, the updater's; the details list
records every step. System tools are run by full path (`$SYSDIR\net.exe`,
`sc.exe`), never found beside the installer. A broker registered by
hand from another path is kept as it is: `broker uninstall` it first.

**Hyper-V is a prerequisite** the installer checks and does not
provide. Teams here run in Hyper-V VMs, and enabling the role takes an
administrator and a restart (Turn Windows features on or off, Hyper-V;
or `Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V
-All`). When Hyper-V's management service (`vmms`) is missing after the
install, the installer says so. The broker installs and runs either way;
teams elsewhere need none of it.

**No MSI.** The Windows target is NSIS only: Tauri's WiX bundle has no
hook that runs `broker install`, and an install without the service
cannot start a team here. An MSI would need a WiX fragment that
registers the service.

The installer carries `root.vhdx` (248 MiB, an already-compressed
squashfs inside, so LZMA saves little), which makes it about 240 MiB
(the debug build: 238 MiB, against 11 MiB without the image); NSIS's
limit is 2 GB.

## Decisions

| Decision | Chosen | Alternative and why not |
| --- | --- | --- |
| Page framework | plain TypeScript, a small element helper, the design system's CSS, Vite | Vite + React: the pages are forms and lists redrawn from one snapshot; React would double the bundle and add a render model for no gain |
| OS differences | one `platform` module with one signature per concern, a compile-time check that every OS module offers them all | `cfg` attributes through the common code: a placeholder that compiles on one OS can silently do the wrong thing there (a lock that always succeeds, a wait that returns at once) |
| Several teams | one supervisor and one VM per team, each in `teams/<id>` with its own `local.json`, disk, endpoint and port; `--config-dir` scopes everything | one supervisor running several VMs: a new multiplexing layer in the supervisor, where the folder already scopes it all |
| Where the UI comes from | each team's own served web app, loaded in the shell | a bundled copy: cannot pass the server's same-origin checks, and drifts in version from the server |
| Where the list of teams lives | the shell, in teams.json | the web app: browser storage is per origin, and a team elsewhere is untrusted content that must not reach a supervisor |
| Shell to supervisor channel | an owner-only Unix socket or named pipe | a localhost port with a token: reachable from a web page in principle, and every other process would need the token |
| Supervisor language | Go sidecar | Rust: the VM libraries are Go, and so is the rest of Kivali |
| Team windows | one window per team, no switcher; teams open from the tray; the Window menu lists them | one window that navigates between teams: teams should sit side by side, each titled with its name |
| Web app and Kivali page | two child webviews in the team's window, one shown at a time (Tauri's multiwebview, the `unstable` feature) | one webview swapping between the bundled page and the team's URL: the ACL could then only be scoped by origin, and a team's page would share a webview with the IPC bridge |
| Team navigation | pinned to the team's origin, frames included (on Windows through the shell's own WebView2 frame hook), its `/auth/login` only with `client=desktop`; other web pages open in the system browser | any http(s) page: another team or a look-alike page could load in a window the person trusts as their team |
| Web data | the webview's one shared data store, isolated by origin; a cookie suffix per team here | a data store per team: more machinery for the isolation the same-origin policy already gives, except cookies across ports, which the suffix covers |
| Remote IPC | none: no `remote` capability; app manifest in build.rs; webview-label check in every command | relying on origin scoping alone: one line in a capability file away from exposing a supervisor |
| Owner at setup | the app's own Google sign-in through the public client and relay, id_token verified locally | typing an email: a typo locks the owner out of their own team; signing in later in the team's page: the server needs the owner before it can admit anyone |
| Connecting to a team | the team's own sign-in through the existing desktop sign-in (loopback return), the outcome read from where the hidden window lands | a new one-time-code handoff endpoint on the team: the loopback flow already lands the session in the window |
| Claude credential | the CLI's own sign-in (`claude` in the team's server container), which offers a subscription, a Console account, Bedrock and Vertex AI, plus the app's Microsoft Foundry form; status from `claude auth status` | an API key typed into the app: a second billing path for Kivali to carry to agent pods, check, store and rotate, covering less than the CLI's own sign-in |
| Claude sign-in progress | the supervisor's `terminals` count and credential status, polled while a sign-in is open | reading the Terminal session: the providers' terms keep the login in their own CLI, and the app must not see it |
| Supervisor protocol | the real RPC in `internal/supervisor/rpc.go` (HTTP over the platform's endpoint, NDJSON with stage ids) | a protocol of the shell's own: would have needed a second server in the supervisor |
| HTTP client for the RPC | the shell's own HTTP/1.1 client over `platform::connect` (`supervisor/http.rs`) | reqwest: its Unix-socket transport has no named-pipe twin, and a second HTTP stack per transport is more code than the subset the RPC uses |
| Terminal | through the `kivali-supervisor terminal` command in the system terminal | over the RPC directly: the terminal is a PTY stream the shell must not handle (`Supervisor::open_stream` exists for when it should) |
| Backup | not in the app: each team's web app has its own backup download | a tray item: a second path to the same zip |
| Sidecar, dev vs release | `externalBin` built by `make sidecar` (Go for the real one, cargo for the fake) and found next to the app's executable in both; `KIVALI_SUPERVISOR` overrides | a path baked into dev builds: two lookups, and `tauri dev` already copies externalBin beside the binary |
| Starting the sidecar | std `Command` from Rust | `tauri-plugin-shell`'s sidecar API: same lookup, plus a JS-facing plugin whose permissions would have to be kept away from every page |
| Supervisor ownership | pid file + the endpoint's peer pid + parent check, per team, the rule common and the facts from `platform`; adopted processes waited for with `platform::wait_exit` | owning whatever listens: would stop a supervisor started from a terminal; owning only children: a crashed shell's `serve` would never be stopped |
| Stopping `serve` | RPC first (`down --exit`), then the platform's stop request, then a kill, each wait bounded; all teams in parallel at quit | a signal first: Windows has none for a windowless process, and the RPC is the one path both OSes share |
| Deleting a team | Settings only, E7 then E8 (the typed name), then the system's owner check, then `destroy` | a confirmation alone: the delete is final and a slip of the hand should not cause it |
| Memory | refuse a start that would not fit (a quarter of the host, at least 4 GB, kept back), offer to pause the largest running team | starting anyway: the host swaps or the VM fails mid-boot |
| Single instance | the platform's lock on `shell.lock` and exit (macOS: `flock` and a dialog; Windows: an exclusive open, and the running shell is asked to open Kivali through a named event) | tauri-plugin-single-instance: another plugin for a lock the OS already gives |
| Waiting for `serve` | until the endpoint answers or the process exits, at most 15 seconds | no deadline: a transport that never answers would hang the shell; `serve` listens before it boots anything, so 15 seconds is not a slow first start |
| Minimum desktop version | compared by the shell with its own version, the supervisor's verdict as a second opinion | the verdict alone: a supervisor stamped with a different version would judge for an app it is not |
| Tray picture | four coloured states in two inks, the ink chosen by the shell, the pulse as frames on a timer | template images: the system tints them, so couldn't start's red would be lost; one animated image: the tray takes still images only |
| Quit | a custom menu item, E2 when a team here runs | the predefined Quit ends the process before the teams are paused |
| Alerts | native alerts from native surfaces, in-page sheets from the bundled pages | in-page everywhere: a menu or the tray has no page to draw on; native everywhere: the dialog plugin cannot draw E7/E8's list and name field |
| Remote team check | 10-second limit, no redirects, 64 KiB body cap | following redirects: a login wall would pass for a team; an unbounded read: any host could feed the check forever |
| Status freshness | `shell-changed` after every operation and every observed change; the watch loop once a minute (a crash, a Claude sign-out, teams elsewhere), every 3 s while a Claude sign-in is open; the tray click; the pages poll each second only while something moves | no polling: a team that stopped on its own or a Claude that signed out would go unseen until the person looked |

## Known gaps

**Current limitations**

- **Other devices (D7, D8): the operator provides the https.** Kivali
  runs no network listener and no TLS of its own. Settings → the team →
  Other devices records an https address the operator put in front of
  the team's loopback port (`tailscale serve` → `http://127.0.0.1:<port>`,
  or a reverse proxy), after `set_public_url` checks that it answers
  `GET /api/v1/login` as this team; it is stored as `public_url` in
  teams.json and shown with Copy. The connect side accepts only https
  off loopback. The team's port stays put while nothing else takes it,
  but if the supervisor has to move it (another program holds it) the
  operator's proxy must follow. The team's own sign-in accepts only
  loopback and this one external name ([auth.md](auth.md)): the address
  is handed to the team's server as `KIVALI_EXTERNAL_URL` (the
  supervisor's `POST /v1/address`, the chart's `externalURL`) when it is
  saved and the team runs, and again at every start (a no-op when
  unchanged), so a change made while the team was paused reaches it.
  "Who can sign in" shows the owner: "On the other computer, sign in
  with the same Google account."
- **Numbers come only from a signed-in window.** Agents, files and
  "3 agents working" come from the team API as the person signed in in
  the team's window ("The team API"); until that window has been opened
  and signed in, those lines carry no numbers and the tray says
  "running".
- **No Dock menu or Windows jump list** listing the teams.
- **Notifications route on macOS only, and only from the app bundle.**
  On Windows, Linux and an unbundled macOS build, the notification
  plugin shows them and a click only brings Kivali forward. In a dev
  build (`make dev`, an unbundled binary) macOS attributes them to
  Terminal, since the process has no bundle of its own.
- **The macOS owner prompt's wording.** The reason string is "delete
  the team <name>", which macOS shows as "Kivali is trying to delete
  the team <name>."; the rest of the prompt is the system's.
- **Windows menu bar keys.** Alt, F10 and Alt+letter do not open a team
  window's menu bar from a webview; the mouse and the listed shortcuts
  do.
- **Windows alerts** have no destructive button style (an alert with a
  destructive button shows the warning icon instead).
- **Windows flags from a console.** A release build is a GUI-subsystem
  program: `Kivali --version` and `--check` attach to the parent
  console and print there, but the prompt comes back before that output
  and the exit code is not seen unless the shell waits (`start /wait`,
  a pipe); started with a flag but no parent console (a shortcut), it
  prints nowhere. The debug installer's `Kivali.exe` is a console
  program, so its lines stay in its own console window, and closing that
  window closes the app; `make build-trial` (the release profile without
  updater artifacts) is the build to try an install with.
- **The team window's frame guard.** A frame loads only what `team_nav`
  loads as is, so a team page cannot show another origin, a look-alike
  sign-in page included, inside the team window. Beyond that: on
  Windows a refused frame's request still reaches the refused address,
  though its answer never shows; on macOS a refused frame goes to the
  browser through the gate like any refused page, since wry cannot tell
  it from one; `data:` frames stay refused everywhere (a team page that
  wants a generated frame uses `about:srcdoc` or a `blob:` of its own
  origin); and the server sends no CSP `frame-src` (with `child-src`)
  limited to its own origin, which would cover every client, browsers
  included.
- **A team added by another address than its sign-in host.** When a
  team elsewhere is added by a URL whose origin is not the one its
  server signs in on (an own Google client whose `OAUTH_REDIRECT_URL`
  is on another host or scheme, or a port-forward alias), the server's
  login hop to the configured host leaves the team window's origin, so
  the sign-in runs in the browser and never comes back to the app. Add
  the team by the host its sign-in uses. **Open in browser** (the Team
  menu, ⇧⌘B on macOS and Ctrl+Shift+B in a Windows team window, the
  tray, Settings) works for any team.
- **A logout or shutdown does not record which teams ran.** The
  system's exit stops the supervisors but leaves `running_at_quit` as
  the last Quit wrote it.
- **A terminated `serve` on Windows leaves its VM running.** When
  `down --exit` fails or times out, the stop ends in `TerminateProcess`;
  the next start has to deal with the VM left behind.
- **The pipe is a synchronous handle.** A relay over an upgraded stream
  (terminal, exec) that reads and writes on two threads needs
  overlapped I/O or two handles on Windows; nothing relays over it.
- **Unix socket paths** are limited to 104 bytes on macOS; team ids are
  kept short for it, but a deep `KIVALI_CONFIG_DIR` may still exceed
  it.
- **Code with no caller**: `updates.rs`'s labels and badge, the
  commands `open_team`, `open_team_page`, `open_settings` and
  `reorder_teams` (Settings cannot reorder teams).
- **No Windows release.** No Windows build is code-signed (no
  Authenticode certificate) and `latest.json` carries no
  `windows-x86_64` entry.

**Covered by unit tests only, not exercised end to end**

- The desktop sign-in (a team window, the browser and back), the
  owner's sign-in through Google, the relay and the id_token check
  against Google's real keys, and connecting to a real team elsewhere.
  The URL building, the listeners (over real loopback sockets), the
  return parsing and the id_token checks (with a test key) are
  unit-tested.
- The team API against a real webview's cookie store: which cookies
  `Webview::cookies` returns (and with which domain) from WKWebView and
  WebView2, and the `kivali_denied` read on B6.
- Claude's sign-in in Terminal with the real supervisor, and the
  `terminals` count closing when Terminal does.
- Delete with the real supervisor, LocalAuthentication and Windows
  Hello included.
- Several teams at once on real VMs: the port hints, the cookie suffix
  keeping sign-ins apart, the memory check against real usage.
- `claude auth status` in a real team's server container, and a
  values-only Helm roll, which have run only against the fake VM
  ([supervisor.md](supervisor.md)).
- Adopting an orphaned `serve`: `tests/supervisor_e2e.rs` runs on macOS
  and Windows, but its adoption test needs `/bin/sh`, so it covers
  macOS only.
- On Windows, the platform module's tests cover the instance lock, the
  named pipe against a server of this user, the broker probe, the
  process facts, `wait_exit`, `TerminateProcess`, the second-launch
  event, the registry, animation and Hello availability queries, and
  the shell's log in a process started without a console. Not covered
  by any test: the squatting check against another account, the
  terminal launcher, Windows Hello's prompt, the TaskDialog alerts,
  notifications, the tray and its menu, the updater hook, and the
  installer's broker hooks (register, start, stop on upgrade, remove).
- The GUI as a whole: the pages are checked against the artboards
  through the dev mock; menus, the tray, alerts, notifications and OS
  prompts have no automated test.
- Release signing: whether the bundler re-signs the sidecar with the
  virtualization entitlement under a Developer ID identity, and whether
  notarization accepts it; the updater against a published
  `latest.json`; the dmg.
