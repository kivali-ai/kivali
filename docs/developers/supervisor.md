# kivali-supervisor: reference

The host side of Kivali Desktop ([desktop-app.md](desktop-app.md)): one
Go binary that boots the VM image ([vm/README.md](../../vm/README.md))
through a backend (Virtualization.framework on macOS, Hyper-V on
Windows), talks to the guest agent over the backend's agent connection
(vsock), forwards the org's port to `127.0.0.1`, installs and upgrades
Kivali through k3s's Helm controller, and serves all of it on an
owner-only RPC endpoint (a Unix socket on macOS and Linux, a named pipe
on Windows). It works from a command line; the desktop shell starts
`serve` as its sidecar and speaks the same endpoint. The dev team
(`make dev-*`) is run by it too ([deployment.md](deployment.md)).

```
make supervisor            # bin/kivali-supervisor, ad-hoc signed with the virtualization entitlement
make chart-package VERSION=vX.Y.Z
make images VERSION=dev    # the three :dev images, built in the build VM (scripts/build-vm.sh)
make -C vm dev-images      # export them from the build VM for baking
make -C vm image KIVALI_CHART=$PWD/dist/kivali-X.Y.Z.tgz   # bake the chart too
bin/kivali-supervisor up --owner you@example.com
```

Code: `cmd/kivali-supervisor` (CLI), `internal/supervisor` (state,
journal, flows, RPC; no OS-specific code, unit tested against a fake
backend and the real host of the OS running the tests),
`internal/supervisor/host` (the Host of each OS),
`internal/supervisor/vz` (darwin + cgo: the macOS backend),
`internal/supervisor/hyperv` (windows: the Hyper-V backend) and
`internal/supervisor/broker` (windows: the privileged Hyper-V broker it
drives; its wire types, client and policy compile everywhere),
`internal/supervisor/guestapi` (the guest agent protocol: types, client,
server), and `cmd/kivali-guest` (the guest binary, linux only).

## Platform boundary

Everything that differs between operating systems sits behind two
interfaces in `internal/supervisor/platform.go`; the common code names no OS,
hypervisor, socket type or file system feature.

**Backend** is the VM: `Image(dir)` (the image's version and guest
architecture; the architecture picks the release's image bundle),
`DataDiskPath(configDir)`,
`CreateDataDisk(path, size)`, and `NewMachine` from a neutral
`MachineConfig` (image directory, data disk, `FormatData` on a disk's
first boot, CPUs, memory, console log). A `Machine` has `Start`,
`Events` (the marker lines), `Done`, `DialAgent`, `RequestPoweroff` (when
the agent cannot be reached) and `HardStop`. A backend whose VM outlives
`serve` (Hyper-V's is a persistent object) also implements the optional
`MachineRemover` (`RemoveMachine(configDir)`), which `destroy` calls.

| Backend | Package | Image | Data disk | Agent | Poweroff request |
| --- | --- | --- | --- | --- | --- |
| macOS | `vz` (darwin + cgo) | `Image`, `initramfs.gz`, `root.squashfs`, `VERSION`; arm64 | `data.img`, a sparse raw file | vsock port 1024 | the console verb `KIVALI-POWEROFF` |
| Windows | `hyperv` (windows) | `root.vhdx`, `VERSION`; amd64 | `data.vhdx`, a dynamic VHDX (the broker makes it) | Hyper-V sockets to vsock port 1024 | the console verb `KIVALI-POWEROFF` |

The macOS backend owns the kernel command line and its format flag, the
fixed MAC and the serial console. The Windows backend (one Generation 2
Hyper-V VM per team, "The Windows backend" below) owns the same
through the broker: a differencing child of `root.vhdx` and a dynamic
`data.vhdx`, the Default Switch with a per-team MAC, COM1 on a named
pipe carrying the marker lines out and the poweroff verb and the
first-boot format answer in, and the guest agent over a Hyper-V socket
to vsock port 1024. It needs the broker ("The broker" below) and a VM
directory holding `root.vhdx`; without one `serve` and `up` stop at once
with "no VM image found (root.vhdx): pass --vm-dir …". Linux builds with
no backend: the client subcommands work, `serve` refuses.

**Host** is the operating system (`internal/supervisor/host`,
`host.Default()`):

| Method | macOS | Windows | Linux |
| --- | --- | --- | --- |
| `DefaultConfigDir` | `~/Library/Application Support/Kivali` | `%LOCALAPPDATA%\Kivali` (local, never roaming) | `$XDG_DATA_HOME/kivali`, else `~/.local/share/kivali` |
| `Snapshot` | `clonefile` (APFS clone) | copy, keeping a sparse file sparse, flushed | copy, fsynced |
| `SyncDir` | `F_FULLFSYNC` | checks the directory only: a directory handle cannot be flushed | fsync |
| `Rename` | `rename`, then `SyncDir` | `MoveFileEx` with `MOVEFILE_REPLACE_EXISTING \| MOVEFILE_WRITE_THROUGH` | as macOS |
| `ExcludeFromBackup` | Time Machine attribute | no-op | no-op |
| `FreeBytes` | `statfs` | `GetDiskFreeSpaceEx` | `statfs` |
| `FileUsage` (allocated bytes, size) | `st_blocks` × 512, `st_size` | `GetCompressedFileSizeW` (the size if it fails), size | as macOS |
| `Listen`/`Dial`/`Endpoint` | `supervisor.sock`, mode 0600 | `host.PipeName`: `\\.\pipe\kivali-` and the first 8 hex characters of the SHA-256 of the lower-cased, cleaned absolute config directory (the shell computes the same); a DACL granting only the current user. Every dial checks the server process's user SID against the current one (`GetNamedPipeServerProcessId`), since any account can create the name first: a client refuses another account's pipe ("the supervisor pipe is held by another account"), and `Listen` reports when such a squatter keeps it from creating the pipe. The owner keeps `WRITE_DAC` and an administrator can take ownership, so the pipe is closed to other ordinary accounts, not to an administrator | as macOS |
| `LockServe` | `flock` on `serve.lock` | `LockFileEx` on `serve.lock` | as macOS |
| `Detach` | `setsid` | `DETACHED_PROCESS`, `CREATE_NO_WINDOW`, own process group | as macOS |
| `AddrInUse` | `EADDRINUSE` | `WSAEADDRINUSE` | `EADDRINUSE` |

The config directory rule is the shell's too; `$KIVALI_CONFIG_DIR` and
`--config-dir` override it.

The tests run the common code against the fake backend
(`fake_test.go`, which keeps its data disk in a subdirectory and reports
amd64, so a hard-coded file name or architecture fails) and the real host
of whichever OS runs them; `host_test.go` covers each Host method on that
OS. `.github/workflows/ci.yml`'s desktop jobs run the suite on
GitHub-hosted macOS and Windows runners on every pull request and every
push to `main` (`make ci-desktop`),
and CI cross-builds and vets for Windows and Linux.

## Processes

One process owns the VM: `serve`. It holds the machine (on macOS the
`VZVirtualMachine` and its vsock device), the port forward and
`local.json`, and listens on the RPC endpoint (on macOS
`<config>/supervisor.sock`, mode 0600, in a 0700 directory). Every other
subcommand is a client of that endpoint. `up` starts `serve` in the
background (detached: `setsid` on macOS; log
`<config>/logs/supervisor.log`) when nothing answers on the endpoint.
Before touching the endpoint, `serve` takes the exclusive serve lock on
`<config>/serve.lock` (`flock` on macOS; released by the kernel when the
process exits, however it exits): of two `serve`s started at once, the
loser exits with status 3 ("another kivali-supervisor serve owns this
config directory") and `up` connects to the winner. Without the lock,
both could find no answer on the endpoint and the second would remove
the first's live socket. If `serve` dies, the VM dies with it (it is
in-process); the next `up` boots it again and recovers any upgrade
journal. `serve` exits on `down --exit` or SIGINT/SIGTERM, shutting the VM
down cleanly first. On Windows a detached `serve` has no console and
gets no console control events, so it exits only through `down --exit`
(terminating it is the SIGKILL case below). On a signal it cancels a running `up`, `install` or
`load-images`, but waits for a running `upgrade` for as long as it takes
(still answering `status`), because the VM is in-process and would die
mid-upgrade with it; the 5-minute bound applies only to the stop itself,
counted from when it starts. A second signal changes nothing; SIGKILL
leaves the journal for the next `up` to recover.

Only one operation that changes the org runs at a time. `down` cancels a
running `up`, `install` or `load-images` (their waits end at once) and
then always carries on to the clean stop, even if its client has gone,
so a cancelled `up` is never left without one. An `upgrade` owns the
journal and is never cancelled: `down` immediately logs "an upgrade is
running; waiting for it to finish" and waits, and that wait (only the
wait, where nothing has been cancelled yet) ends if the client
disconnects, so the shell can show progress and let the person stop
waiting. `destroy` does the same as `down`, but refuses outright while
an upgrade runs or its journal is open. An `address` change refuses ("an
upgrade is running; try again once it has finished") rather than queue
behind another operation. `status` and `GET /v1/credential` never wait; `status`'s `operation`
field names the running operation.

## CLI

Global flag `--config-dir` (default `$KIVALI_CONFIG_DIR`, else the
host's config directory; on macOS `~/Library/Application Support/Kivali`).

| Command | What it does |
| --- | --- |
| `serve [--vm-dir D]` | own the VM and the RPC endpoint; runs until signalled |
| `up [--owner E] [--chart T] [--public-client-id I] [--env N=V]… [--set k.p=v]… [--port P] [--port-hint P] [--memory-mb M] [--cpus C] [--prepare] [--vm-dir D]` | recover a journal, boot if stopped, forward, install if not installed, wait until `/readyz` answers through the forward; `--prepare` stops before the install (the desktop boots a new team's VM while setup still asks for its owner) |
| `down [--exit]` | clean shutdown through the agent (the backend's poweroff request if the agent is unreachable, on macOS the console verb; hard stop after 3 min); `--exit` also ends `serve` |
| `status [--json]` | serve, VM, guest (state, node, boot id, versions), forward, `local.json`, journal |
| `install …` | install into a running VM (the install flags of `up`); refuses if installed |
| `load-images --from FILE\|-` | import an image archive (a docker-archive or OCI tar, from `docker save` or `ctr images export`; `-` reads stdin into a file in the config directory first, since serve reads a file) into the guest's containerd, then `restart`. Not needed on a machine with a Kivali VM: `make dev-load` builds the `:dev` images inside the team's own VM (scripts/build-vm.sh, BuildKit on the VM's containerd) and calls `restart` |
| `restart` | delete the agent pods, `rollout restart deploy/kivali`, wait: the org onto the images now in the VM (a mutable tag such as `:dev` leaves the manifest unchanged, so nothing else picks new images up) |
| `forward [--port P]` | move the loopback forward and record the port |
| `check [--feed F]` | fetch `release.json` (10 s timeout), record the outcome |
| `upgrade [--feed F] [--force]` | the journaled upgrade below |
| `credential` | print how the org's Claude CLI is signed in, as `claude auth status` reports it ("Claude: signed in · owner@example.com · Claude Max"); sign in with `terminal` (see "Credential") |
| `handoff` | print a one-time token that signs the org's first owner in at `http://127.0.0.1:<port>/auth/handoff?t=<token>` within 2 minutes (debugging; see "Handoff") |
| `destroy --yes [--exit]` | stop the VM and delete the org: the data disk, its snapshot, `downloads/`, `local.json`, the logs (see "local.json"); `--exit` also ends `serve` |
| `backup --out FILE` | `serve` signs in to the org as its owner (a handoff over the guest's proxy to the NodePort) and downloads the app's own backup zip from `POST /api/v1/org/backup`, writing FILE itself (temporary file, fsync, rename; nothing appears on failure, a cut download or an empty archive). FILE must be inside the config directory or the home directory of the user `serve` runs as |
| `restore --in FILE` | the same sign-in, then FILE uploaded to the app's own `POST /api/v1/org/restore`: the restore setup offers, refused unless the org is freshly set up, checked whole before anything is written. FILE has the same location rule as `backup` |
| `terminal [--shell]` | a PTY running `k3s kubectl -n kivali exec -it deploy/kivali -c kivali -- sh -c '…exec claude'` (`--shell`: `exec bash`), so the person lands in Claude Code and its first run asks them to log in (a Claude subscription, an Anthropic Console account, or Amazon Bedrock or Google Vertex AI under 3rd-party platform; `/login` signs in again); the login lands in the container's HOME on the data volume, where the server's and every agent pod's runs of the CLI find it. Claude Code runs with `BROWSER=/tmp/kivali-open-browser`, a helper the command writes first, which prints `ESC ] 1337;KivaliOpenURL=<page> BEL` to its terminal; this client removes that sequence from what it shows and, when the page is Claude's own sign-in (https, claude.com, claude.ai or anthropic.com), opens it in this computer's browser with its localhost callback swapped for `https://platform.claude.com/oauth/code/callback`, the paste-the-code callback Claude's printed link uses (at most once every 3 s). Nothing else of the stream is read, and nothing typed. Raw mode and window-size changes with a TTY (SIGWINCH on Unix; on Windows the console size is polled every 250 ms and VT output processing is turned on), pipe mode (then ^D) without |
| `exec [-i] -- argv…` | run a command in the guest as root with k3s's tools on PATH (debugging; the shell does not need it). The serve log records only `argv[0]` and the argument count, never the arguments |

`--vm-dir` is the directory with the VM image (on macOS `Image`,
`initramfs.gz` and `root.squashfs`): the flag, `$KIVALI_VM_DIR`, `vm/`
next to the binary, or `vm/build/out` under the working directory, the
first the backend's `Image` accepts, in that order.

`--env` adds `extraEnv` entries; a name the chart already sets is refused
by the API server ("duplicate entries"), so chart-managed variables go
through `--set` (string values; `true`/`false` become booleans).

## local.json

Written atomically and durably, mode 0600: a synced temporary file and
the Host's `Rename` (rename and `F_FULLFSYNC` of the directory on macOS,
`MOVEFILE_WRITE_THROUGH` on Windows). Every journal step is on the disk
before the step it records begins; a lost `snapshot_complete` would make
recovery take the default branch and delete the only good copy.

```json
{
  "vm_image": "1.2.0",
  "kivali": "1.2.1",
  "data_disk_formatted": true,
  "memory_mb": 4096,
  "cpus": 4,
  "port": 18080,
  "feed": "https://github.com/kivali-ai/kivali/releases/latest/download/release.json",
  "last_check": "…",
  "last_check_ok": true,
  "latest": {"status": "upgrade-available", "current": "1.2.0", "latest": "1.2.1", "min_desktop_version": "1.2.0", "message": "…", "checked_at": "…", "feed": "…"},
  "upgrade": null
}
```

`vm_image` is what the guest reports (`/usr/share/kivali/vm-version`);
`kivali` is the installed chart version (re-read from the manifest on the
data disk if missing). `port` is the forward's: the supervisor owns the
forward; it is 0 until the first forward records one (see "Port
forward"). `latest` is the last successful check. The data disk's path
is the backend's (`DataDiskPath`), not a field.

**Data disk rule** ([vm/README.md](../../vm/README.md), "Boot contract"): a missing disk, or one whose first boot
never reached READY (`data_disk_formatted` false), is recreated by the
backend's `CreateDataDisk` (64 GiB; on macOS a sparse all-zero raw file)
and booted with `FormatData` (on macOS `kivali.format-data=1` on the
kernel command line); format is never asked for on any other boot. A
missing `local.json` next to an existing data disk counts as formatted:
the guest refuses (FATAL) a disk that is not a Kivali disk, but a
reformat would destroy one that is.

Other files: the RPC socket (`supervisor.sock`, macOS and Linux),
`serve.lock`, the data disk (`data.img` on macOS), its snapshot next to it
(`data.img.upgrade`, only during an upgrade), `downloads/<version>/`
(verified release assets, deleted when the upgrade commits or rolls
back), `logs/console.log` (+ `.1`, the previous boot; timestamped; the
guest's boot and console output, on macOS headed by the kernel command
line), `logs/supervisor.log`.

`destroy` deletes exactly these, after a clean stop: the data disk, its
snapshot, the backend's data directory if that is then empty (`data\` on
Windows), `downloads/`, `local.json`, stray `.tmp-local.json-*` and
`.kivali-backup-*` temporary files, and the three log files (the `logs`
directory too if nothing else is in it). It never touches the RPC
endpoint, `serve.lock`, the config directory itself or any file it does
not know (the shell keeps its own there, one directory per team:
`<config>/teams/<id>/`). Its result's `freed_bytes` is what the deleted
files occupied (the Host's `FileUsage`). Without `--exit`, `serve` keeps
running with the defaults, and the next `up` starts a new org. On
Windows a `supervisor.log` that `serve` still writes cannot be deleted;
that is logged, not an error. Where the backend's VM outlives `serve`
(`MachineRemover`: Hyper-V), `destroy` removes it after the stop and
before the files, whether or not this `serve` booted it: through the
broker (`DELETE /v1/vm`), which takes the VM, its differencing child
and the broker's `vm\` directory, never the data disk. A failure there
is logged and the files go all the same.

## Secrets at rest

The owner email and the generated session key are written by the
install, in plaintext, and live only on the data disk (`data.img` on
macOS):

- the HelmChart manifest `k3s/server/manifests/kivali.yaml` (its
  `valuesContent`; written mode 0600 by the guest agent);
- the HelmChart object and the Helm controller's chart-values Secret in
  `kube-system`, the Helm release record (a Secret in `kivali` holding the
  rendered values), and the `kivali-secrets` Secret the chart creates: all
  four are rows in k3s's kine database, `k3s/server/db/state.db`
  (SQLite, not encrypted at rest).

The Claude credential the CLI's sign-in stored (a subscription's OAuth
tokens, a Console key, or the Bedrock or Vertex AI credentials), and a sign-in setup's values in the CLI's settings
file, live on the same disk, in the data volume
(`/data/claude-home`). On macOS `data.img` is mode 0600 in the 0700
config directory; nothing on the host outside it holds any of these. The
credential status returns only what `claude auth status` reports: whether
it is signed in, the account's email and what it bills. During an upgrade the
snapshot is a full copy of the disk and carries the same data until it is
deleted, on every outcome.

The data disk, its snapshot and `downloads/` go through the Host's
`ExcludeFromBackup`, re-asserted on the data disk at every boot. On macOS
that is Time Machine's `com.apple.metadata:com_apple_backup_excludeItem`
extended attribute (value: the binary plist of `com.apple.backupd`, byte
for byte what `tmutil addexclusion` writes). The attribute is sticky: it
travels with the item through renames and clones and needs neither root
nor a subprocess. `tmutil addexclusion -p` (a fixed path in the system's
list) would need root, so it is not used. Windows and Linux have no
equivalent; it is a no-op there.

## The VM (macOS backend)

`internal/supervisor/vz` matches the boot contract: `VZLinuxBootLoader`
with `console=hvc0 cgroup_no_v1=all panic=10` (plus the format flag),
root disk read-only as `/dev/vda`, data disk as `/dev/vdb`, virtio console
to `logs/console.log`, VZ NAT with the fixed MAC `02:4b:56:00:00:01`,
entropy, vsock. Every disk is attached with the cached caching mode and
full synchronization (`vz.DiskAttachment`): the automatic mode corrupts
Linux guests' disks on Apple Silicon under mixed I/O, and full
synchronization turns a guest flush into `F_FULLFSYNC`, so what the
guest's journal commits survives a host crash. Defaults 4096 MiB and 4
CPUs; sizing a team is the shell's job. The VM keeps the memory it has
touched until it stops ([vm/README.md](../../vm/README.md), "Memory on
macOS").

**Nothing connects in over the NAT NIC.** VZ NAT's guest address is
reachable from the Mac, and so from every local user, through the vmnet
bridge, which would expose NodePort 30080 and the API server's 6443. The
guest's `boot` drops every inbound TCP SYN on the NIC before k3s starts:
an nftables table of its own, `inet kivali_inbound`, with a prerouting
chain at raw priority (-300) holding `iifname "eth0" tcp flags & (syn |
ack) == syn drop` and `iifname "eth0" udp dport 8472 drop` (flannel's
VXLAN port: a single node never receives remote VXLAN, and a crafted
packet there would land on the pod network). It runs before kube-proxy's NAT, and k3s's
iptables-restore never touches a table it did not create. Replies to the
guest's own outbound connections, DHCP, loopback, cni0/flannel and vsock
are unaffected; the org's port reaches the host only through the vsock
forward. A separate nft table is used rather than `iptables -t raw`
because the root disk has no iptables until k3s unpacks its own, after
boot. A failure to install the rule is a FATAL.

**READY and FATAL** (common to every backend). The boot waits for the
marker line `KIVALI-VM READY` in `Machine.Events` (on macOS from the
console; 10 min bound); a `KIVALI-VM FATAL` line, the VM stopping, or the
bound fails the boot with that line and the console log's path. It then
polls the agent until it reports `ready` for this boot (or `fatal`, with
the reason the boot scripts recorded). The markers are authoritative for
the boot in progress because they work even with a broken agent; the
agent is authoritative afterwards (`status`).

## The Windows backend

`internal/supervisor/hyperv` runs one Generation 2 Hyper-V VM per team,
mirroring the macOS backend through the broker. A VM per team, rather
than WSL2 distros, keeps the macOS model: one kernel Kivali ships and
pins, memory of its own, the guest agent on vsock, the markers on a
serial console, the root disk replaced on an update. WSL2 would put
every team in one VM with one kernel, network stack and memory budget,
shared with everything else in it, so one `wsl --shutdown` or kernel
panic would stop every team. The cost: Windows Pro, Enterprise or
Education (Home has no Hyper-V role), enabling the role once with a
reboot, and a privileged broker.

The VM is named `kivali-<key>`; its name, static MAC
(`02:4b:56` then the first three bytes of the key) and console pipe
(`\\.\pipe\kivali-<key>-com1`) all derive from the config directory's
key, the same `host.DirKey` the RPC pipe uses. The devices: a
differencing child of the release's `root.vhdx` as the root disk and a
dynamic `data.vhdx` as the data disk (SCSI locations 0 and 1), one
adapter on Hyper-V's Default Switch, COM1 on the console pipe, Dynamic
Memory (minimum 1 GiB, startup and maximum the team's memory), the
team's CPUs, Secure Boot off, checkpoints off, and the shutdown
integration service on. `Start` serves the console pipe itself, as the
person's account, before it starts the VM through the broker: Hyper-V
creates a COM port's pipe only if nothing holds the name, with a
security descriptor an ordinary account cannot open, and otherwise
connects to the existing pipe as a client, as LocalSystem, as the VM
starts, without retrying. So the pipe is one byte-mode instance created
exclusively (a name already held by anyone fails the start, and nothing
of the VM's ever reaches a pipe someone else serves), with a protected
DACL admitting that account and LocalSystem only, and a connect pending
before `start` is sent; `Start` then waits up to 30 s for Hyper-V to
connect and runs `ScanConsole` on the connection for the marker lines
and `logs/console.log` exactly as on macOS; a tee off the same stream
answers the first-boot question. `Done` closes when the
console pipe ends or, as a backstop for a stop that leaves the pipe
open, a poll of the broker (every 15 s, a state-only query) reports the
VM Off. `DialAgent` is go-winio's Hyper-V socket dial to the VM's id and
the vsock service id of port 1024. `RequestPoweroff` and the format
answer are written to the console under one lock, so they never
interleave. The VM's configuration and differencing disk live in the
broker's VM directory of the team, `ProgramData\Kivali\broker\vm\<key>`;
the data disk is `<config>/data.vhdx`, 64 GiB dynamic, with no partition
table.

A Hyper-V VM outlives the `serve` that started it (a logoff, a crash, the
app killing `serve`). Before a start, a leftover running VM is shut down
cleanly (the guest agent's shutdown, else Hyper-V's shutdown integration
service), never turned off; one that will not stop cleanly, or is Saved
or Paused, is an error for a person. `HardStop` never turns off a paused
VM either: Hyper-V pauses a VM whose host volume is full
(`PausedCritical`), and it resumes intact once there is room.

**The format question.** The command line baked into the Windows image
ends `kivali.format-data=ask`, so on first boot the guest prints
`KIVALI-VM ASK format-data` and reads one line from the console. The
backend answers `KIVALI-FORMAT-DATA 1` when `MachineConfig.FormatData`
is set (the boot right after it created the data disk) and `0`
otherwise, so the host is the only one who ever says yes, the rule
vm/README.md states. The inbound drop on `eth0`, READY and FATAL, and
the agent protocol are common to both backends. Every team's VM sits on
the Default Switch, so the guest's inbound drop on `eth0` is what keeps
one team's NodePort from another, as on macOS.

**What works on Windows.** The Hyper-V backend through the broker, both
as the installed service and in console mode: a first boot with the
format question, `status`, `exec`, a clean `down`, a second boot, two
teams at once, `install`, `forward`, `backup`, `restore`,
`terminal` (also in pipe mode), `upgrade` with its rollback, and
`destroy`. A copied dynamic VHDX (the upgrade snapshot) attaches and
boots, and a rebuilt parent `root.vhdx` is noticed and the differencing
child recreated. `make -C vm image ARCH=amd64` builds the image; `make
test-vm` runs under Git Bash; image builds use the build VM
(`ENGINE=vm`), so no Docker Desktop is needed. The desktop shell builds,
tests and lints on Windows, and the NSIS installer is per machine,
carries `vm/root.vhdx` and registers the broker service. As measured on
one machine (16 CPUs, 32 GB): Start-VM to READY in about 20 s on a first
boot and 12 s on a later one; a clean `down` under 5 s.

**Known limitations on Windows.**

- No test boots a VM on Hyper-V: GitHub-hosted Windows runners may not
  offer Hyper-V under nested virtualisation.
- No Authenticode signing, and no Defender check of the installer (a
  `root.vhdx` holding the guest binary is not flagged).
- No end-to-end test covers the desktop GUI on Windows.
- The Default Switch's subnet changes at a host reboot; k3s takes the
  lease's address as its node IP each boot; no test covers a host
  reboot between two boots.
- Dynamic Memory: nothing is reclaimed at idle, and no test covers
  behaviour under load.
- The exec stream through the supervisor's pipe moves a few MB/s, so
  copying a VM image's artifacts out of the build VM takes minutes.
- A development `root.vhdx` must sit where every account can read it
  (Hyper-V checks the parent disk as the broker's account), for example
  under `C:\Users\Public`, not inside a person's profile. A config
  directory created from inside an MSIX-packaged app lands in the
  package's private `LocalCache`, where the broker cannot see it.
- A machine that sleeps stops its teams answering.

## The broker

Managing Hyper-V needs a standing administrator-equivalent privilege, so
Kivali puts it behind a helper rather than in the person's session, as
Docker Desktop and Rancher Desktop do. The broker
(`internal/supervisor/broker`, `kivali-supervisor broker`) is a Windows
service running as its own virtual account (`NT SERVICE\kivali-broker`,
a member of Hyper-V Administrators whose token keeps only the
impersonation and traverse privileges; `broker install`, run by the
elevated installer, registers it, and `broker uninstall` removes the
service, its event source and the group membership), or a foreground
process in an elevated terminal with `--console`, serving HTTP/1.1 on
the named pipe `\\.\pipe\kivali-broker`: `POST /v1/vm` (make the VM exist
with exactly a configuration), `GET`/`start`/`stop`/`DELETE` on it,
`POST /v1/vhd` (a new dynamic data disk), and `GET`/`POST /v1/setup`
(Hyper-V, the Default Switch and the vsock service registration). The
`hyperv` backend and `kivali-supervisor broker setup`/`status` are its
clients, as the ordinary user; `KIVALI_BROKER_PIPE` overrides the pipe
for a dev run. The client (`broker.Dial`) connects at impersonation
level, so the broker can check as the caller, and before writing
anything reads the pipe's owner through its own handle: it must be
the broker service's account, LocalSystem or the Administrators group
(the service names its own account, an elevated console broker the
group) and an ordinary account can name none; anything else, such as a
squatter that created the pipe first, is refused, as is an owner that
cannot be read.

The pipe's DACL lets Authenticated Users connect (read and write data)
but not add an instance to the pipe, since every instance shares the
first's owner; the policy is what protects the broker's operations,
and it is reviewed as an elevation boundary. The broker
impersonates the pipe's client for every file it names, so it opens
nothing the caller could not, canonicalises each path from the opened
handle and refuses any reparse point along it, and requires every path
but `root_vhdx` to lie under a `config_dir` the caller can write to. The
VM's name must be `kivali-` plus the key of that directory, and a VM is
started, stopped or removed only by the account whose SID the broker
wrote into its notes at creation. The broker's own VM directory of the
team (`ProgramData\Kivali\broker\vm\<key>`, owned by the broker, the
caller may only read it, and refused if anyone else made it) is where
it creates files as itself: `POST /v1/vhd` makes the disk there, grants
the caller the right to move it, has the caller's own account rename it
into place (replacing nothing), and checks by file identity that the
disk the broker made is what landed before granting full control
through a handle on it; `DELETE` removes the directory after the VM,
through handles, deleting a link inside as the link, and removes it on
its own when the VM does not exist and the directory was made for the
caller. Inputs reach PowerShell
only as fields of a JSON object on the stdin of a fixed, embedded
script, validated (name, MAC, sizes, counts) before anything runs; the
runner is an interface, so the policy is unit-tested with a fake and the
real `powershell.exe` runs only on the machine.

## The guest agent protocol (vsock port 1024)

HTTP/1.1, one request per agent connection (`Machine.DialAgent`; on macOS
a vsock connection). The protocol is the same over any connection. The
agent listens on nothing but vsock, and accepts a connection only if its
peer context id is the host's (2); any other peer is logged and closed
before a byte is read.
`vsock_loopback` (which would let a guest process reach a guest vsock
port) and `vhost_vsock` are blacklisted with `install … /bin/false` in
`/etc/modprobe.d/kivali.conf`, so they cannot load even if the module list
changes. Nothing from the pod network or the guest's own processes can
reach the agent.

The client never sees what carries a connection. On macOS each agent
connection is a Virtualization.framework vsock connection; on Windows it
is a Hyper-V socket (go-winio's `Dial` to the VM's id and the vsock
service id of port 1024, `00000400-facb-11e6-bd58-64006a7986d3`, which
`POST /v1/setup` registers under
`HKLM\…\GuestCommunicationServices`). Either way the guest listens on
nothing but vsock port 1024 and admits only the host (CID 2); the
guest contract is unchanged across the two.

| Request | Answer |
| --- | --- |
| `GET /v1/status` | JSON `Status`: `boot_id`, `state` (`booting`/`ready`/`fatal` from `/run/kivali/{ready,fatal}`; `ready` only if it holds this boot's id), `fatal`, `node_ready` (node Ready with this boot's id, read now), `versions` (agent, vm_image, kernel, k3s), `baked_images` (refs inside `/usr/share/kivali/images/*`), `baked_charts` (name/version from each `/usr/share/kivali/charts/*.tgz`) |
| `GET /v1/exec?spec=<json>` + `Upgrade: kivali-stream` | 101, then frames |
| `GET /v1/pty?spec=<json>` + upgrade | 101, then frames (rows/cols in the spec) |
| `PUT /v1/file?path=<rel>&mode=<octal>` body | 204; written atomically under `/var/lib/kivali`, paths escaping it refused |
| `POST /v1/images/import` body = image tar | JSON `{exit_code, output}` of `k3s ctr -n k8s.io images import -` |
| `GET /v1/proxy?port=N` + upgrade | 101, then raw bytes to `127.0.0.1:N` in the guest |
| `POST /v1/shutdown` | 202, then `poweroff` (init runs `shutdown`: cordon, delete Kivali pods with their grace, stop k3s, unmount) |

`spec` is `{"argv": [...], "env": [...], "dir": "", "rows": 0, "cols": 0}`;
the base environment is PATH with k3s's bundled binaries and `HOME=/root`.

**Frames** (exec and pty): 1 type byte, 4-byte big-endian length, payload
(≤ 1 MiB). Host to guest: `0` stdin, `4` close stdin (a PTY gets ^D), `5`
resize `{"rows","cols"}`. Guest to host: `1` stdout (the PTY's output),
`2` stderr, `3` exit `{"code","error"}` (last; a signal is 128+n, a start
failure -1 with `error`). An exec runs in its own process group; closing
the stream SIGKILLs the whole group, and stragglers holding its output
pipes get 5 s. Closing a PTY stream sends SIGHUP to the terminal's session
and SIGKILL to it 5 s later if it has not exited. The agent logs only
`argv[0]` and the argument count of what it runs.

## The RPC (serve)

HTTP/1.1 over the RPC endpoint (`supervisor.sock` on macOS, the named
pipe on Windows). Operations are `POST /v1/<name>` with a
JSON body and answer NDJSON: `{"log": "...", "stage": "..."}` lines, then
one `{"done": true, "error": "...", "result": {...}}`. They run to
completion even if the client disconnects, except `down`'s and
`destroy`'s wait behind another operation and `backup`, which end with
the request.

| Endpoint | Body | Result |
| --- | --- | --- |
| `GET /v1/status` | | `Report` (plain JSON) |
| `POST /v1/up` | `{"install": {owner, chart, public_client_id, env, set}, "port", "port_hint", "memory_mb", "cpus", "prepare"}` | `Report` |
| `POST /v1/down` | `{"exit": bool}` | (first line, if an upgrade runs: "an upgrade is running; waiting for it to finish") |
| `POST /v1/install` | the install object | |
| `POST /v1/load-images` | `{"path": "<an image archive serve can read>"}` | |
| `POST /v1/restart` | `{}` | |
| `POST /v1/forward` | `{"port": N}` | |
| `POST /v1/check` | `{"feed": "..."}` | `CheckResult` |
| `POST /v1/upgrade` | `{"feed": "...", "force": bool}` | `Report` |
| `POST /v1/backup` | `{"path": "/abs/file.zip"}` (inside the config directory or the home directory) | `{"path", "bytes"}`; the file exists only on success |
| `POST /v1/restore` | `{"path": "/abs/file.zip"}` (same rule) | the status, once the org restored it |
| `GET /v1/credential` | | `{"signed_in", "email" (when reported), "billing" (when reported: "Claude Max", "Anthropic Console", "Amazon Bedrock", "Google Vertex AI", or a setup's "<provider> · <target>"), "checked_at" (RFC 3339)}` (plain JSON); 409 with a sentence when the VM is not running or Kivali is not installed |
| `GET /v1/credential/setup` | | `{"models", "current"}` (plain JSON; see "Sign-in setups"); 409 as above |
| `POST /v1/credential/setup` | `{"setup": "<id>", "values": {...}}` | `{"credential", "models": [{"model", "ok", "missing", "problem"}]}` (plain JSON); 400 with the driver's sentence for values it refuses, 409 as above |
| `POST /v1/credential/clear-provider` | `{}` | `{"cleared": [names]}` (plain JSON); 409 as above |
| `POST /v1/address` | `{"external_url": "https://host[:port]"}` (empty clears) | none; sets the HelmChart's `externalURL` (the server's `KIVALI_EXTERNAL_URL`, the one off-loopback name its sign-in accepts) and, when that changed anything, waits for the Helm controller to apply it and the server to serve again |
| `POST /v1/destroy` | `{"exit": bool}` | `{"freed_bytes": N}` |
| `POST /v1/handoff` | `{}` | `{"token": "..."}` (plain JSON, not an operation); 409 with a sentence when the VM is not running, Kivali is not installed or an upgrade journal is open |
| `GET /v1/terminal?rows=&cols=[&shell=1]` + upgrade | | the guest PTY's frames, relayed byte for byte |
| `GET /v1/exec?argv=<json>` + upgrade | | the guest exec's frames |

`Report` is `running`, `forward`, `url`, `guest` (the agent's status) or
`guest_error`, `state` (`local.json`), `config_dir`,
`supervisor_version`, `busy`, `operation`, `terminals` (open
`/v1/terminal` sessions: counted once attached, until the splice ends, so
the shell sees the Terminal running `claude` login close) and
`disk_used_bytes`/`disk_size_bytes` (the data disk's allocated bytes and
logical size, the Host's `FileUsage`; 0 without a disk).

**Stages.** Every log line of an operation carries the stage it belongs
to (the serve log shows it in brackets), so the shell can draw steps
without parsing lines:

| Operation | Stages, in order |
| --- | --- |
| `up`, fresh disk | `making-room` (creating the disk, its first boot to READY), `starting` (guest agent, forward), `setting-up` (install, rollout, `/readyz`) |
| `up`, installed | `starting` (boot to READY, guest agent, forward), `setting-up` (rollout, `/readyz`) |
| `install`, `address` | `setting-up` |
| `upgrade` | `downloading` (preflight, fetch, verify, import), `snapshot` (journal, quiesce, stop, snapshot), `installing` (boot, chart, HelmChart rewrite), `starting` (rollout, `/readyz`); on failure `rolling-back` |
| `down` | `pausing` |
| `destroy` | `deleting` |

`load-images`, `forward`, `check` and `backup` have no stages (`stage`
is absent).

**Credential.** `GET /v1/credential` is one exec (20 s bound) of
`claude auth status --json` in the server container, as the server's
user with its HOME (`/data/claude-home`, `internal/claudeagent`'s
`HomeDirName`): the CLI's own verdict, so every way the CLI signs in
reads the same. The CLI exits 1 when signed out and prints the same
JSON; `internal/claudeauth` parses it (`loggedIn`, `authMethod`,
`apiProvider`, `email`, `subscriptionType`) and names the billing in
words. Output that is not that JSON is an error and is never echoed. It
never takes the operation lock, and neither do the three below. The CLI
is signed in by running `claude` in `terminal` (its sign-in menu, or
`/login`), or by a sign-in setup;
agent pods mount the same HOME, so they follow it.

**Sign-in setups.** A provider the CLI has no interactive sign-in for
(Microsoft Foundry) is configured purely by the `env` block of the
CLI's settings file, `$HOME/.claude/settings.json` in the server
container. The Claude driver (`internal/claudeauth`, `setups.go`)
declares each such setup behind an id: it validates the values, maps
them to env variables, reads back the non-secret ones and words a
missing model. The supervisor and the desktop pass ids and values
through and know nothing of either.

- `GET /v1/credential/setup` answers `{"models": [the catalog's model
  ids], "current": {"setup", "values"}}` (`current` only when a setup is
  in force; its values carry no secret).
- `POST /v1/credential/setup` takes `{"setup": "<id>", "values": {...}}`
  (400 with the driver's sentence when it refuses them). It reads the
  settings file (exec `cat`), merges the driver's env block in (every
  provider variable replaced, every other key and variable kept; a file
  that is not a JSON object is left alone), and writes it back with an
  exec whose stdin carries the file (umask 077, mode 0600, a rename):
  values are never in an argv, a log line or an answer. Then the
  credential as above, and one `claude -p` per catalog model, in
  parallel, each bounded at 60 s (`--max-turns 1`, no tools, no saved
  session, `CLAUDE_CODE_MAX_RETRIES=1`). Answers `{"credential",
  "models": [{"model", "ok", "missing", "problem"}]}`; a model that
  fails does not undo the setup. While a setup is in force, the
  credential's `billing` names its target ("Microsoft Foundry ·
  my-resource"), read from the settings with one more exec.
- `POST /v1/credential/clear-provider` removes every provider
  variable (each `CLAUDE_CODE_USE_*` switch and the variables any setup
  or the CLI's Bedrock and Vertex wizards write, `claudeauth.ProviderEnvKeys`)
  from the settings file and answers `{"cleared": [names]}`. The desktop
  runs it before it opens Claude's sign-in in Terminal, since a provider
  variable left behind outranks a `/login`.

A long-running agent's CLI read the settings when it started; a setup
reaches it when it restarts.

**Handoff.** `POST /v1/handoff` reads the HelmChart's values on the data
disk (`secrets.sessionKey`, and the first address of
`secrets.ownerEmails`) and mints the server's one-time sign-in token for
that owner, valid for 2 minutes ([auth.md](auth.md), "Desktop handoff";
`auth.NewHandoffToken`). It is not an operation and never takes the
lock, but refuses while an upgrade journal is open. The serve log says
"handoff token minted for the owner", never the token or the key. The
shell calls it once, when setup's **Open** shows the new team.

**What the shell calls:** `GET /v1/status` to draw the tray (`running`,
`url`, `guest.state`, `state.kivali`, `state.latest` with its
`min_desktop_version`, `state.upgrade`, `busy`, `operation`,
`terminals`, the disk fields); `POST /v1/up` (with `prepare` while
setup still asks for the owner, then with `install.owner` from setup's
Google sign-in, the team's `install.env` and a `port_hint` per team);
`GET /v1/credential`, the two
`/v1/credential/setup` calls and `clear-provider`; `POST /v1/handoff`
once, when setup opens the new team; `POST /v1/address`; `POST
/v1/destroy` to delete a team's org; `POST /v1/down` (with `exit` on
quit; show its log lines, since behind an upgrade it waits, and closing
the request abandons only the wait); `POST /v1/check` daily and on
demand; `POST /v1/upgrade`; Claude's sign-in by running
`kivali-supervisor terminal` in the system terminal. The shell does not
call backup or restore (the web app's own cover them). Progress lines of `up` and `upgrade` are human-readable
and safe to show as they arrive. The shell must not open the window on
the org before `up` returns success.

## Port forward

`127.0.0.1:<port>` only (the address is not configurable). Each accepted
TCP connection opens its own agent connection (vsock on macOS) to the
agent's proxy, which
dials `127.0.0.1:30080` in the guest, the NodePort as seen from the node.
k3s's policy controller admits traffic from the node itself, which is
what makes this path work while every pod is fenced off.

The port is `local.json`'s; an org with none recorded yet starts on
`up`'s `port_hint` (the shell gives each team's org its own), else 8080,
and the first forward records it. A hint is ignored once a port is
recorded. When another program
holds it and `up` was not given `--port`, `up` logs that, takes a free
port instead and records it, so the org still comes up on a machine
whose 8080 is busy (the Host's `AddrInUse` recognises the error); the
shell reads the port from `status`, and the next
`up` tries the recorded port first. A hinted port moves like the
default. A port asked for by name (`up --port`, `forward --port`) is
never moved: that `up` fails with the address-in-use error.

Each forward is bound to the guest of the boot it was started for: its
connections dial that guest directly and never take the supervisor's
lock. Stopping the forward (on `down`, a VM that stops on its own, an
upgrade's restart, `forward --port`) happens outside that lock, cancels
dials still in progress and closes open connections, so a browser
connection arriving at that moment cannot wedge `serve`. Every new boot
starts a new forward.

## Install values

Chart source: `--chart` (a host tarball, copied in through write-file),
otherwise the one chart baked into the VM image (`/usr/share/kivali/charts`,
copied with `cp` in the guest), so the first boot is offline. It lands as
`<data>/k3s/server/static/charts/<name>-<version>.tgz`, and the manifest
`<data>/k3s/server/manifests/kivali.yaml` (mode 0600; it holds the session
key) is a HelmChart in `kube-system`, `targetNamespace: kivali`,
`createNamespace: true`, `chart: https://%{KUBERNETES_API}%/static/charts/<file>`,
with `valuesContent`:

```yaml
kivaliEnv: dev
devMode: false
initImage: docker.io/rancher/mirrored-library-busybox:1.37.0   # from the baked k3s images
image: {repository: kivali, tag: dev, pullPolicy: IfNotPresent}  # from the baked Kivali images
egress: {image: {repository: kivali-egress-proxy, tag: dev}}
secrets: {create: true, ownerEmails: <--owner>, sessionKey: <32 random bytes, hex>}
networkPolicy: {enabled: true, podCIDR: 10.42.0.0/16}
oauth: {publicClientID: <--public-client-id, if given>}
extraEnv: <--env>
# then every --set
```

Image tags are discovered from the baked tarballs' `manifest.json`
(`RepoTags`) or OCI `index.json` annotations: exactly one `kivali` tag,
with `kivali-egress-proxy` and `kivali-dev-shell` at the same tag and
prefix, else the install refuses. The install waits for the deployment
(label `helm.sh/chart: kivali-<version>`, container image, rollout
complete) and then `/readyz` through the forward. It fails fast when a
`kivali` pod waits on `ErrImagePull`, `ImagePullBackOff`,
`ErrImageNeverPull` or `InvalidImageName`, when the Helm controller's
`helm-install-kivali` job has failed 3 times (with helm's error), or when
the chart's label matches but it renders another image.

## Release feed

`release.json` (written by `scripts/release-json.sh`, published by the
release workflow, [releasing.md](releasing.md); this is the format the
supervisor reads):

```json
{
  "version": "1.2.1",
  "chart": {"name": "kivali-1.2.1.tgz", "sha256": "…"},
  "images": {"linux/arm64": {"name": "kivali-images-linux-arm64.tar.zst", "sha256": "…"}},
  "min_desktop_version": "1.2.0",
  "vm_image": "1.2.1",
  "manual_steps": false,
  "notes_url": "https://…"
}
```

Asset names resolve against the feed's own URL (or directory, for a local
path or `file://` feed); an asset may carry an absolute `url`. Bundles may
be `.tar`, `.tar.gz` or `.tar.zst` (decompressed on the host, streamed to
`ctr import`). `check` answers a `CheckResult`: `status`, `current`, `latest`,
`min_desktop_version` (the release's, passed through on every successful
check whatever the verdict, so the shell can compare it with its own app
version), `message`, `notes_url`, `checked_at`, `feed`; the last
successful one is kept as `latest` in `local.json`. `status` is one of
`up-to-date`, `upgrade-available`, `desktop-update-required`,
`manual-steps`, `not-installed`, `failed`. The supervisor's own verdict
compares the minimum with the supervisor's version; a development
supervisor (version not semver) satisfies every minimum.

## Upgrade journal

An upgrade, with `local.json`'s `upgrade` as the journal:

1. Preflight: no open journal; the release is newer (or `--force`), not
   `manual_steps`, not above our version; guest `ready`, node Ready,
   deployment healthy; 2 GiB free (the Host's `FreeBytes`) in the data
   disk's directory.
2. Download chart and bundle into `downloads/<version>/`, verify SHA-256
   (a file appears under its name only once verified), read the chart's
   `Chart.yaml` (its version must equal release.json's `version`, or the
   release is refused, naming both) and the bundle's image refs, import,
   check `ctr images ls` lists every ref. Nothing has changed yet; a
   failure here needs no rollback.
3. Journal `{from, to, step: prepared, snapshot, started}`.
4. `quiescing`: delete agent pods with a wait, clean shutdown.
5. `snapshotting`: remove any stale snapshot, the Host's `Snapshot` of
   the data disk next to it (on macOS `clonefile(data.img,
   data.img.upgrade)`), then the Host's `SyncDir` on that directory (on
   macOS `F_FULLFSYNC`) so the snapshot is on the drive before the journal
   says so; then `snapshot_complete: true`, step `snapshotted`. A failed
   snapshot deletes the partial file, clears the journal and starts the
   org unchanged.
6. `applying`: boot (the old release comes up first), place the new chart,
   rewrite the manifest from the one on the data disk (only the chart URL
   and the two image tags change, so the owner and session key never
   leave the org's disk).
7. Wait for label, image, rollout, `/readyz`.
8. `committed` (and `kivali` = to), delete the snapshot and
   `downloads/<version>/`, clear the journal.

Rollback (failure in 6 or 7): step `rolling-back`, stop the VM, swap the
snapshot over the data disk with the Host's durable `Rename`, clear the journal,
delete the downloads, boot and wait for the old chart. If the rename
succeeded but the restored disk then fails to boot, the error says the
disk was restored to the old release and starting it failed; it is not
reported as a failed rollback.

On `up` with a journal:

| Journal | Recovery |
| --- | --- |
| `committed` | delete the snapshot, record the new version, clear |
| `snapshot_complete`, snapshot present | record `rolling-back`, swap it over the data disk (`Rename`), clear |
| `rolling-back`, snapshot missing | the rename happened before the interruption: clear |
| `snapshot_complete` at any other step, snapshot missing | **refuse**: the data disk may be the half-applied new release. Nothing is booted, the journal is kept, and the error names the step, the missing file and the two ways out below |
| anything earlier | delete a partial snapshot, clear (the disk was never touched) |

Ways past a refusal, both with `serve` left running (`up` re-reads
`local.json` from disk whenever a journal is involved, so an edit made
while `serve` runs takes effect; run no other command in between, since
any of them rewrites `local.json` from memory):

1. Put the snapshot file back at the recorded path, then run
   `kivali-supervisor up`: it rolls back to the old release.
2. Or, to boot the data disk as it is (possibly the half-applied new
   release), edit `local.json` so that `"upgrade"` is `null`, then run
   `kivali-supervisor up`.

The snapshot holds the Claude credential and the secrets above and is
deleted on every path that completes. Each row is unit-tested against
the fake VM (`TestRecover*` in `internal/supervisor`).

## Decisions

| Decision | Chosen | Alternative and why not |
| --- | --- | --- |
| Guest agent protocol | HTTP/1.1 per connection, 101 upgrade for streams, a 5-byte-header frame format for exec/pty | gRPC: a codegen dependency and HTTP/2 for no gain; a custom mux on one connection: agent connections (vsock on macOS) are cheap and per-connection lifetimes make cancellation trivial |
| Port forward | one agent connection per TCP connection to the agent's TCP proxy | gvisor-tap-vsock user-mode network: a second network stack and a dependency to replace a NIC that already works for outbound; revisit if the guest ever needs inbound ports other than the org's |
| Forward target | `127.0.0.1:30080` in the guest (NodePort from the node) | the Service ClusterIP or pod IP: both bypass the NodePort the chart exposes, and the node-origin path is what the server-ingress policy already admits |
| Who owns the VM | `serve`, one process; the rest are RPC clients; `up` autostarts `serve` | each subcommand booting its own VM: the VM dies with the process and two processes would fight over the disk |
| READY/FATAL | marker lines for the boot (the console on macOS), then the agent's recorded state | agent only: no signal at all if the agent is broken; console only: no way to ask later |
| Chart at install | baked into the VM image next to the image tarballs, `--chart` override | from `dist/` on the host: a desktop app has no repo; download: first boot must be offline |
| Image tags | read from the baked tarballs' manifests, all three Kivali images at one tag | a version file written at build time: can drift from what is actually baked |
| Upgrade values | rewrite the manifest found on the data disk | keep install values in local.json: would put the owner's session key outside the org's disk |
| Session key | generated once at install, lives only in the manifest/Secret | regenerating per upgrade: logs everyone out and rolls the pod |
| Long ops vs. `down` | `down` cancels up/install/load-images, waits for upgrade (saying so, abandonable) | uncancellable: a Stop click would wait out a 10-minute bound |
| One serve | the serve lock on `serve.lock` (`flock`, `LockFileEx`) before the endpoint | probe-then-remove the socket: two simultaneous starts remove each other's |
| Platform code | two interfaces, Backend and Host, implemented per OS under build tags; the common code and its tests are the same on every OS | build-tagged files inside the common package: the OS leaks into the flows and only the macOS path is tested |
| Backup transport | serve writes the file at a checked path; NDJSON result | streaming the archive with an error trailer: a client that ignores trailers saves a truncated or empty archive as a success |
| Time Machine exclusion | the sticky `com_apple_backup_excludeItem` xattr | `tmutil addexclusion -p`: needs root and is keyed to a path |
| Giving the guest's unused memory back to macOS | not done: a VM keeps the memory it has touched until it stops; the levers are its configured memory and stopping it when idle | the traditional virtio balloon, the only reclaim Virtualization.framework offers: it negotiates no free page reporting, and nothing ballooned is released, not even under critical host memory pressure; with the guest's default init_on_alloc=1 inflating backs the whole VM ([vm/README.md](../../vm/README.md), "Memory on macOS") |
| Inbound on the NAT NIC | nft table at raw priority dropping SYNs on eth0 | `iptables -t raw`: no iptables on the root disk before k3s unpacks its own |
| Releases | `release.json` as above, assets relative to it, published with the GitHub release | an update server: one more service to run for files a release already hosts |
| Windows VMs | a Hyper-V VM per team through a broker service | WSL2: one shared VM and kernel for every team; Hyper-V Administrators membership for the person: a standing, administrator-equivalent privilege in their whole session |

## Current limitations

- Release bundles in `.tar.zst` are supported; the end-to-end runs used
  `.tar`.
- A crash *during* the snapshot itself and the missing-snapshot recovery
  rows are unit-tested only (`internal/supervisor`); a real kill at
  `applying` with the snapshot present recovers as described.
- The exec process-group kill and the PTY SIGHUP/SIGKILL are tested on
  the host's own Unix (the exec group kill deterministically in
  `guestapi`), not inside the VM.
- The peer-CID check is exercised only on the admitted path (the host);
  no guest-side vsock client can be run against it, since vsock loopback
  cannot load.
- Cancelling `up` while the VM is still booting hard-stops it (a first
  boot's disk is disposable anyway; a later boot gets e2fsck on the next
  start).
- The macOS backend gives every VM the same MAC (`02:4b:56:00:00:01`),
  so two teams running at once share one address on the NAT network.
  The Windows backend derives a MAC per team.
- `kivali.selftest=1` is not passed by the supervisor (product boots).
- Stages, `terminals`, the disk fields, `port_hint`, `credential` and
  `destroy` are unit-tested against the fake VM only; `claude auth
  status` in the server container and the Helm controller's rollout on a
  values-only change have not run in a real macOS VM.
- `serve` on Linux builds but has no backend.
- Windows: see "What is not done on Windows" above.
