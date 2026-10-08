# Kivali Desktop VM image

The guest that Kivali Desktop boots ([desktop-app.md](../docs/developers/desktop-app.md)):
a Linux kernel, a tiny initramfs, and a read-only root disk holding
busybox, k3s and the baked airgap image tarballs. Everything stateful
lives on a second disk. It is built for two architectures from the same
sources: linux/arm64 for Virtualization.framework on macOS, and
linux/amd64 for Hyper-V on Windows ([supervisor.md](../docs/developers/supervisor.md)), where
the same root disk also comes as a bootable `root.vhdx`. Releases ship
the arm64 image only. This directory builds the image and boots the
arm64 one on Virtualization.framework to prove it works.

```
make -C vm image                # arm64: kernel + initramfs + root disk -> vm/build/out
make -C vm image ARCH=amd64     # amd64: the same, plus root.vhdx (Hyper-V)
make -C vm dev-images           # the :dev Kivali images (make images VERSION=dev) into the drop dir
make -C vm boottest             # boot build/out with a new data disk; exit 0 on READY + clean stop
make -C vm boottest BOOTTEST_FLAGS=-keep-data   # boot again on the same data disk
make -C vm clean
```

`ARCH` is `arm64` (the default) or `amd64`; `image` and `dev-images`
take it. A build for the host's own architecture takes a few minutes.

`ENGINE` is where the Dockerfile runs. `vm`, the default on macOS and
Windows, is the build VM: BuildKit inside a Kivali VM on this machine
(scripts/build-vm.sh), which needs the supervisor binary (`make
supervisor`) and a VM image to boot, `build/out` when it holds one, else
the installed Kivali Desktop's; the first image on a new machine comes
from a release and builds its successors. It builds the machine's own
architecture only. `docker`, the default on Linux (which has no Kivali
VM) and what CI uses, is this machine's Docker; for the other
architecture the guest agent is still cross-compiled and the downloads
and disk assembly still run natively, but the stages that install and
run the target's Alpine packages (and, for amd64, Debian's `ukify`) run
under emulation, which Docker Desktop provides and a plain Linux host
needs registered (binfmt with QEMU); that is slow.

Needs curl, plus the supervisor binary (Go) for `ENGINE=vm` or Docker
for `ENGINE=docker`; `boottest` needs macOS on Apple Silicon with Go and
CGO. No qemu or other host tools for `image`: everything else runs
inside the build, in the VM or in Docker.

`boottest` flags beyond `-keep-data`: `-format-data` passes the format
flag even on a reused disk; `-crash-on <text>` hard-stops the VM the
moment a console line contains `<text>` (fault injection; prints
`DONE (fault injected; not a pass)`); `-trim-check`, after READY,
writes and deletes 2 GiB on the data disk through the guest agent, runs
`trim --once`, and fails unless `data.img` gave most of it back;
`-trim-crash-after <d>` instead hard-stops the VM `<d>` into that trim
(fault injection).

`-integrity-cycles <n>` is the data disk's crash test: `n` boots on one
data disk, each with `kivali.fsck=force` (after `e2fsck -p` replays the
journal, `boot` runs a full `e2fsck -fn` and any damage is a FATAL),
each waiting for READY (so k3s and its datastore came back too), then
verifying the crash-consistency oracle (`kivali-guest integrity`,
`internal/supervisor/integrity`) against the last write acknowledged
before the previous power cut, then running it (atomic-rename records,
fsync'd journal appends, a corpus written once, churn workers, fstrims,
its own checks after dropping the page cache) and hard-stopping the VM
at a random moment between `-integrity-min-run` and `-integrity-max-run`
(default 5 s..60 s). The last boot verifies and stops cleanly. A lost
acknowledged write, damage or corruption fails it. `-integrity-seed`
repeats a run's power-cut times; the acknowledged count is kept in
`<run>/integrity-acked`, so `-keep-data` continues a run. For a long
run: `make -C vm boottest BOOTTEST_FLAGS='-integrity-cycles 50'`.

`-vz-caching` (`automatic`, `cached`, `uncached`) and `-vz-sync`
(`full`, `fsync`, `none`) attach the data disk with another caching or
synchronization mode than the supervisor's (cached, full; see
`vz.DiskAttachment`), for experiments only. Otherwise the boot test
attaches disks exactly as the supervisor does.

## How the image is built

1. `scripts/fetch-k3s.sh` fetches, for `K3S_VERSION` (default in
   `vm/Makefile`) and `ARCH`, the
   release's `sha256sum-<arch>.txt` and `k3s-images.txt`, and the tag's
   `scripts/version.sh` (the k3s-root version, for `SOURCES.md`), into
   `build/dl/<version>/` (text only; the binary itself is fetched in
   the Docker build, below), and pulls exactly these five for
   `linux/<arch>` into `build/k3s-images/<arch>/k3s-images.tar` (with
   `ENGINE=docker`, `docker pull` and `docker save`; with `ENGINE=vm`,
   the build VM's containerd and `ctr images export`, an OCI tar):
   `rancher/mirrored-pause:3.10.2`, `rancher/mirrored-coredns-coredns:1.14.7`,
   `rancher/local-path-provisioner:v0.0.37`,
   `rancher/mirrored-library-busybox:1.37.0`,
   `rancher/klipper-helm:v0.13.3-build20260727`.
   Their digests are pinned in the checked-in `k3s-images.lock`: a ref
   not yet in it is resolved once and appended (commit the file); a ref
   whose tag now resolves elsewhere fails the build ("the tag moved").
   The pinned digest is the image index's (the repo digest `docker
   image inspect` reports for a multi-arch tag), the same whichever
   platform is pulled, so one lock serves both architectures.
   The tarball holds only the requested platform, hence one directory
   per architecture.
2. `Dockerfile` (syntax `docker/dockerfile:1.27`; Alpine 3.23 and
   Debian trixie pinned by digest), built for `linux/<arch>` to the
   target `artifacts-<arch>`. Stages that only download or
   cross-compile run on the build host's own platform:
   - **k3s**: downloads the release's `k3s` (amd64) or `k3s-arm64`
     binary with curl and checks it against `sha256sum-<arch>.txt`.
   - **guest**: builds `kivali-guest` (`./cmd/kivali-guest`) static
     for the target, `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X
     main.version=$VM_VERSION"`, in `golang:<GO_VERSION>-alpine`, where
     the Makefile reads `GO_VERSION` from the root `Dockerfile` (the
     server image's Go). Only `go.mod`, `go.sum`, `cmd/kivali-guest`
     and `internal/supervisor/guestapi` are copied in, from the named
     context `src` (the repository root);
     `go mod download` and the build cache use cache mounts.
   - **common-licenses**: Debian's texts of the GNU licenses, for the
     root disk's `/usr/share/doc/kivali/licenses/`.
   - **kernel**: Alpine's `linux-virt` (6.18 LTS). On arm64 it is built
     with `EFI_ZBOOT`, so `scripts/unzboot.sh` unpacks the gzip payload
     into the raw arm64 `Image` that `VZLinuxBootLoader` requires; on
     amd64 it is a bzImage with the EFI stub and is kept as is. The
     script checks the result's magic for the architecture (`ARM\x64`
     at 0x38, `HdrS` at 0x202).
   - **rootfs**: a fresh `apk --root` install of only `alpine-baselayout`,
     `busybox`, `musl`, `kmod`, `e2fsprogs`, `nftables` and the CA bundle (no apk,
     no OpenRC, no firmware), plus the kernel's modules, k3s (with
     `kubectl`/`crictl`/`ctr` links), the guest agent, the image
     tarballs, the guest init (`rootfs/`) and the self-test chart
     (`selftest/`). The guest init's line endings and modes are fixed
     here, whatever the checkout: CRs are stripped (a Windows checkout
     has CRLF, and inittab, the scripts and the lists are read line by
     line in the guest), the scripts are 0755 and everything else 0644.
   - **initramfs**: static busybox, the modules that make the root disk
     visible, decompressed (`virtio_blk.ko` and `squashfs.ko`; on amd64
     also `scsi_transport_fc.ko`, `hv_storvsc.ko` and `sd_mod.ko`; vmbus
     and the SCSI core are built in), and `initramfs/init`, also with
     CRs stripped.
   - **rootdisk**: `mksquashfs -comp zstd -all-root`, then checks that
     the guest agent and k3s are in it.
   - **uki** (amd64): `ukify build` with systemd's `linuxx64.efi.stub`
     (Debian's `systemd-ukify` and `systemd-boot-efi`) joins the kernel,
     `initramfs.gz`, the command line (`UKI_CMDLINE`, under "Boot
     contract") and a `.osrel` naming the guest into `kivali.efi`.
   - **vhdx** (amd64): assembles `root.vhdx` without root or loop
     devices (under "The Hyper-V disk"), reads every part back, and
     converts it with `qemu-img`.
   - **artifacts**: `scratch` with the outputs (`artifacts-arm64`:
     the common set; `artifacts-amd64`: plus `root.vhdx` and
     `kivali.efi`), exported to `build/out`.

`build/out` holds exactly one build: the export goes to
`build/out.new`, which then replaces `build/out` whole, so nothing of
another architecture or an earlier build stays behind, and a failed
build leaves the previous one in place. `make` writes `VERSION`
(`VM_VERSION`) next to the artifacts.

### Built inside Docker

No Linux or macOS executable is ever written to the host's filesystem
by `make -C vm image`: the k3s binary is downloaded and verified in the
`k3s` stage, the guest agent compiled in the `guest` stage, and both go
straight into the squashfs. What lands in `build/` is text (the sha256
list, `k3s-images.txt`, lock stamps), the `docker save` tarball of the
k3s images, and the artifacts, in which the executables sit inside a
squashfs (and, on amd64, inside the VHDX too). The reason is Microsoft
Defender on the Windows build host: it flags Go-built static Linux
executables of this tree as `Trojan:Linux/Kaiji.A!MTB` (a false positive
on a botnet written in Go) and quarantines them mid-build. The rule
holds on every host so the build is the same everywhere; it also means
`image` needs no Go on the host.

**Reproducibility.** k3s and its five images are pinned (sha256 and
digest lock). Alpine keeps only the current patch level of each
package, so the kernel and userland follow Alpine 3.23's patch level at
build time: they are **recorded, not pinned**, in `build/out/packages.txt`
(on amd64 also the EFI stub's Debian packages, `systemd-ukify` and
`systemd-boot-efi`, which follow Debian's point releases the same way).
Squashfs timestamps are not normalised, so two builds are
not byte-identical; the unified kernel image of an unchanged kernel and
initramfs is (`ukify` is deterministic), but the GPT carries fresh
random GUIDs on every assembly.

### Artifacts, arm64 (default build)

Sizes are indicative, from one build; they move with Alpine's patch
level and k3s.

| File | Size | What |
| --- | --- | --- |
| `build/out/Image` | 36,306,944 B (34.6 MiB; 10.3 MB gzipped) | raw arm64 kernel Image |
| `build/out/initramfs.gz` | 708,112 B (0.7 MiB) | busybox + 2 modules + `/init` |
| `build/out/root.squashfs` | 197,005,312 B (188 MiB) | the root disk, zstd squashfs (203 MiB unpacked) |
| `build/out/packages.txt` | 1,446 B | installed Alpine package versions |
| `build/out/SOURCES.md` | ~9 KB | the GPL/LGPL software's sources and Kivali's written offer (also in the root disk at `/usr/share/doc/kivali/`) |
| `build/out/kernel.config` | 161,386 B | the kernel's config, for reference |
| `build/out/kernel.release` | 15 B | the kernel release, e.g. `6.18.54-0-virt` |

Inside the root disk: k3s binary 71.4 MB, `k3s-images.tar` 105.1 MB,
kernel modules ~25 MB, everything else a few MB. With the three local
`:dev` Kivali images baked (`make dev-images`, a 797 MB tarball) the
root disk is 988 MB. A data disk after one boot holds ~680 MB.

### Artifacts, amd64 (`ARCH=amd64`, no Kivali images or chart baked)

| File | Size | What |
| --- | --- | --- |
| `build/out/root.vhdx` | 260,046,848 B (248 MiB; virtual size 294,649,856 B, 281 MiB) | the bootable Hyper-V disk: dynamic VHDX, 8 MiB blocks, GPT (below) |
| `build/out/kivali.efi` | 13,544,448 B (12.9 MiB) | the unified kernel image, also inside the VHDX as `EFI/BOOT/BOOTX64.EFI` |
| `build/out/Image` | 12,637,184 B (12.1 MiB) | x86 bzImage with the EFI stub |
| `build/out/initramfs.gz` | 813,308 B (0.8 MiB) | busybox + 5 modules + `/init` |
| `build/out/root.squashfs` | 224,735,232 B (214 MiB) | the root disk, zstd squashfs (236 MiB unpacked); also partition 2 of the VHDX |
| `build/out/packages.txt` | 2,050 B | installed Alpine package versions, plus the EFI stub's Debian packages |
| `build/out/SOURCES.md` | ~10 KB | as on arm64, plus the EFI stub's Debian source packages |
| `build/out/kernel.config` | 151,996 B | the kernel's config, for reference |
| `build/out/kernel.release` | 15 B | the kernel release |

Inside the amd64 root disk: k3s binary 79.1 MB, `k3s-images.tar` 116.1
MB, `kivali-guest` 7.0 MB. A first amd64 build takes under a minute on
a 16-CPU host with the base images present; an unchanged rebuild about
15 s.

### The Hyper-V disk

`root.vhdx` is a dynamic VHDX holding a GPT disk, 1 MiB aligned:

| Partition | Sectors (512 B) | Type | Name | Holds |
| --- | --- | --- | --- | --- |
| 1 | 2048 to 133119 (64 MiB) | `EF00` EFI system partition | `EFI system partition` | FAT32, label `KIVALI-EFI`, one file: `EFI/BOOT/BOOTX64.EFI` = `kivali.efi` |
| 2 | 133120 to the end of the squashfs, rounded up to 1 MiB | `8300` Linux filesystem | `kivali-root` | `root.squashfs`, byte for byte |

The last MiB holds the backup GPT. A Generation 2 VM with Secure Boot
off (the image is unsigned) boots it with no boot loader and no NVRAM
entry: UEFI's removable-media default path is `EFI/BOOT/BOOTX64.EFI`,
and systemd's stub inside it starts the kernel with the embedded
initramfs and command line. The supervisor attaches it as the parent of
a differencing disk and never writes it. Windows refuses to attach a
VHDX that is NTFS-compressed, encrypted or sparse, so it is copied as a
plain file.

The `vhdx` stage builds it as a sparse raw image: a 64 MiB file
formatted with `mkfs.vfat -F 32 -s 1 -S 512 -h 2048` (one 512-byte
sector per cluster, since 64 MiB with larger clusters falls below
FAT32's minimum cluster count; hidden sectors = the partition's start)
and filled with `mmd`/`mcopy`; a zero file of the full size,
partitioned with `sgdisk`; each partition image `dd`'d into place with
`conv=notrunc,sparse`. It then reads everything back (`sgdisk -v` and
`-p`, `mdir` and a byte compare of `BOOTX64.EFI` through mtools' offset
syntax, a byte compare of partition 2 with `root.squashfs`, `unsquashfs
-s` at the partition's offset), converts with `qemu-img convert -O vhdx
-o subformat=dynamic`, and checks the VHDX against the raw image with
`qemu-img compare`. The build log shows each of these.

### Where the Kivali images go

The build bakes every file in `KIVALI_IMAGES_DIR` (default
`vm/build/kivali-images/<arch>/`) next to `k3s-images.tar` in the root
disk's `/usr/share/kivali/images/`. The release pipeline sets
`KIVALI_IMAGES_DIR` to a directory holding its `docker save` (or
`.tar.zst`; k3s accepts `.tar`, `.tar.gz`, `.tar.zst`, `.tar.lz4`)
tarball of `kivali`, `kivali-egress-proxy` and `kivali-dev-shell` for
the image's architecture; `make -C vm dev-images` fills the default
with the `:dev` tags `make images VERSION=dev` built, exported from the
build VM's containerd as an OCI tar (`ENGINE=vm`) or saved from Docker
for `linux/<arch>` (`ENGINE=docker`); k3s imports either. The default
is per architecture so a tarball saved for one is never baked into the
other.
Plain `.tar` is best inside squashfs, which compresses it. Images
keep the names they were saved with: the `:dev` tags import as
`docker.io/library/kivali:dev` etc., so the chart's image references
must match whatever names the pipeline saves.

## Boot contract

**Devices on Virtualization.framework** (arm64; the supervisor and the
boot test both attach disks through `vz.DiskAttachment`, cached caching
mode and full synchronization):

| Device | Guest | Notes |
| --- | --- | --- |
| kernel `Image`, initrd `initramfs.gz` | | `VZLinuxBootLoader` |
| virtio-blk #1: `root.squashfs`, read-only | `/dev/vda` | the root disk, replaceable by an app update |
| virtio-blk #2: data disk, read-write | `/dev/vdb` | raw, sparse; created all zeros by the host |
| virtio-console | `/dev/hvc0` | serial console: markers out, control verb in |
| virtio-net | `eth0` | DHCP; outbound only; **fixed MAC** (`02:4b:56:00:00:01`) |
| virtio-rng, virtio-vsock | | entropy; vsock for the supervisor's agent |

The supervisor must also give the NIC a fixed, locally administered
MAC so DHCP hands out the same address on every boot: k3s persists the
node address, and with a changed IP it keeps reconnecting to the old one
("no route to host").

**Devices on Hyper-V** (amd64; the VM [supervisor.md](../docs/developers/supervisor.md),
"The Windows backend", configures):

| Device | Guest | Notes |
| --- | --- | --- |
| Generation 2 firmware, Secure Boot off, the root disk first | | UEFI loads `EFI/BOOT/BOOTX64.EFI` from the root disk's EFI system partition |
| SCSI disk at controller location 0: a differencing child of `root.vhdx` | `sda` or `sdb` (whichever answers the probe first): partition 1 the ESP, partition 2 the squashfs | the parent is never written; an app update brings a new parent |
| SCSI disk at controller location 1: data disk, dynamic VHDX, no partition table | the other of `sda` and `sdb`, found by its SCSI address `0:0:0:1` | created all zeros by the host |
| COM1, mapped to a named pipe | `/dev/ttyS0` (8250 UART) | serial console: markers out; the format answer and the control verb in |
| network adapter (netvsc) | `eth0` | DHCP; fixed MAC per team, as on macOS |
| Hyper-V sockets | | `hv_sock`: vsock for the supervisor's agent, port 1024 |
| integration services | | `hv_utils` (graceful shutdown), `hv_balloon` (Dynamic Memory) |

**The root disk is found by content.** The initramfs mounts the first
block device or partition whose first four bytes are the squashfs magic
`hsqs`, scanning `/dev/vd*` then `/dev/sd*`, each in name order (a disk
before its partitions: `sda`, `sda1`, `sda2`, `sdb`). That is `/dev/vda`
on Virtualization.framework and `/dev/sda2` or `/dev/sdb2` on Hyper-V,
whose storage driver probes the disks concurrently and names them in the
order they answer: the root disk can be `sda` on one boot and `sdb` on
the next. Disks and their partitions appear
asynchronously after the driver loads, so the scan repeats every 50 ms
for up to 10 s; then it is a FATAL (`initramfs: no squashfs root disk
on /dev/vd* or /dev/sd*`). It prints `KIVALI-VM: initramfs: root disk
<dev> mounted read-only`. No other disk the guest is given begins with
that magic (the data disk is all zeros or ext4, whose first KiB mkfs
leaves zero; a GPT disk begins with its protective MBR), so the order
cannot mislead it.

**Kernel command line.** On Virtualization.framework the host passes it
per boot: `console=hvc0 cgroup_no_v1=all panic=10`, plus the flags
below. On Hyper-V it is baked into the unified kernel image:
`console=ttyS0,115200 cgroup_no_v1=all panic=10 kivali.data=scsi:0:0:0:1
kivali.format-data=ask`. The flags:

- `kivali.data=<dev>` or `kivali.data=scsi:<h:c:t:l>`: the data disk,
  `/dev/vdb` without it. A device must be under `/dev`; a SCSI address
  (host:channel:target:lun, the Hyper-V image's `scsi:0:0:0:1`, the
  disk the broker attaches at controller location 1) is resolved
  through `/sys/block/*/device`. Anything else is a FATAL; `boot` waits
  up to 5 s for the disk, then prints `boot: data disk <dev>`.
- `kivali.format-data=1` **only** on the boot right after the host
  created the data disk file, never again. The guest formats the data
  disk only when this flag is present **and** the disk's first MiB reads
  back in full as zeros (a short or failed read counts as not blank).
- `kivali.format-data=ask`: the same decision, asked on the console at
  every boot (the format question, below), because a baked command line
  cannot carry a per-boot flag.
- `kivali.selftest=1` to install the self-test chart (the boot test sets
  it; a product boot leaves it off).
- `kivali.fsck=force`: after `e2fsck -p`, a full `e2fsck -fn` of the
  data disk, and any damage is a FATAL (the boot test's
  `-integrity-cycles` sets it).

**The format question** (`kivali.format-data=ask`). After networking
and before it touches the data disk, `boot`:

1. turns the console's echo off and prints, on a line of its own,
   exactly `KIVALI-VM ASK format-data`;
2. reads one line from the console (`/dev/console`, the kernel's
   console: `ttyS0` on Hyper-V) for up to 30 s. It is the console's only
   reader then: `console-control` is a respawn entry that init starts
   after `boot`, a sysinit entry, has finished;
3. logs the answer, `KIVALI-VM: boot: format-data: the host answered
   '<line>'` (printable ASCII only, at most 64 characters), or
   `KIVALI-VM: boot: format-data: no answer within 30 s`;
4. formats only if the line is exactly `KIVALI-FORMAT-DATA 1` (a
   trailing CR is ignored, so `\r\n` endings work; any other byte,
   spaces included, makes it a no) **and** the disk is blank by the rule
   above. A yes for a disk that is not blank logs `the host answered
   KIVALI-FORMAT-DATA 1 but the data disk is not blank; not formatting`.
   Anything else is a no, and the disk must pass the acceptance rule
   below.

The host answers `KIVALI-FORMAT-DATA 1\n` on the boot right after it
created the data disk file and `KIVALI-FORMAT-DATA 0\n` on every other
boot, as soon as it sees the question. Whatever the host wrote to the
console before the question is read as the answer, so it writes nothing
there before it. A boot nobody answers waits 30 s and goes on as a no:
right for an existing disk, a FATAL (not ext4) for a new one.

**Data disk acceptance.** Without a format, the data disk must be ext4
labelled `kivali-data`; anything else is a FATAL and the guest does not
touch it. It is checked with `e2fsck -p` (exit 4 or more is FATAL) and
mounted `noatime,errors=remount-ro` (also the filesystem's default,
set at mkfs with `-e remount-ro`). A disk with data in its first MiB is
never formatted, whatever the host says (`kivali.format-data=1 but the
data disk is not blank; not formatting`, then the acceptance rule).

The one rule for the host: **a data disk whose first boot never reached
READY is disposable.** The supervisor deletes the file, creates a new
all-zero one, and says yes again on its first boot
(`kivali.format-data=1`, or `KIVALI-FORMAT-DATA 1` to the question). It
never says yes for the old file and never boots a new file without
saying yes.

**Console markers.** Every line the guest init prints starts with
`KIVALI-VM:`; kernel messages are interleaved. The ones that matter:

- `KIVALI-VM ASK format-data`: only with `kivali.format-data=ask`, once
  per boot, before the data disk is touched; the host answers it (the
  format question, above).

- `KIVALI-VM READY`: exactly once per boot, when all hold (each wait is
  bounded at 5 minutes; an expired wait is a FATAL naming it):
  - the node's Ready condition is True and its `nodeInfo.bootID` equals
    the guest's `/proc/sys/kernel/random/boot_id` (a Ready left over
    from the previous boot does not count); the node is then uncordoned,
    unconditionally, since a clean shutdown cordons it;
  - kube-router's NetworkPolicy controller is active: its
    `KUBE-ROUTER-FORWARD` chain exists, checked with k3s's bundled
    iptables (nf_tables backend);
  - an unprivileged user can `unshare -mUr` and bind-mount inside it;
  - with `kivali.selftest=1`, the Helm controller has installed the
    self-test chart.
- `KIVALI-VM FATAL: <reason>`: boot or a READY check failed (including
  user namespaces not working); the guest powers off right after.
- `KIVALI-VM: shutdown: data disk unmounted cleanly`: printed during a
  clean shutdown, just before power-off.

**Console input.** Two readers, one after the other, never at once.
During `boot`, with `kivali.format-data=ask`, `boot` reads the one
answer line to the format question. From then on `console-control`
(respawned by init) is the only reader of the console (`/dev/hvc0` on
Virtualization.framework, `/dev/ttyS0` on Hyper-V); every other guest
process has stdin on `/dev/null`. It accepts one verb:
`KIVALI-POWEROFF\n` starts a clean shutdown. It is not a shell. It
exists because `VZVirtualMachine.requestStop` cannot reach this kernel:
Alpine's `linux-virt` has `CONFIG_INPUT_KEYBOARD` unset, so there is no
gpio-keys driver for the virtual power button. The guest agent's
shutdown over vsock is the normal path; this verb is its fallback, and
`vm.Stop()` is a hard power cut and the only other way out. On Hyper-V
there is one more graceful path: `hv_utils`'s shutdown integration
service turns a graceful `Stop-VM` (or the host's own shutdown) into the
kernel's orderly poweroff, which runs `/sbin/poweroff` and so the same
clean shutdown.

**Clean shutdown**, in order:

1. `k3s kubectl cordon kivali`, then `k3s kubectl delete pods -A -l
   'app in (kivali,kivali-agentpod)' --wait --timeout=60s`, while
   kubelet is still up, so each Kivali pod gets its
   `terminationGracePeriodSeconds`. The cordon keeps the Deployment
   controller's immediate replacement for the server pod Pending, so it
   is not started and then killed without its grace period in step 3.
   At the next boot `ready` uncordons the node, the controller's
   pending server pod starts, and the server provisions agent pods,
   exactly as a deploy does.
2. k3s: SIGTERM, up to 30 s, then SIGKILL.
3. Every remaining process: SIGTERM, up to 10 s for all to exit, then
   SIGKILL.
4. Unmount everything on the data disk, then the disk; sync; power off.

**NetworkPolicy at boot.** Agent pods are bare Pods persisted on the
data disk. After an **unclean** stop (hard power cut, host crash) they
are still in the cluster, and at the next boot kubelet can start them
a few seconds before kube-router has programmed the NetworkPolicy
rules, so for that window an agent pod could reach the server's API.
The clean shutdown above deletes every Kivali pod first, which is why
that window normally cannot occur; READY additionally waits for the
policy controller.

**Data disk layout** (`/dev/vdb` or `kivali.data=`, ext4 labelled
`kivali-data`, mounted at `/var/lib/kivali`):

```
/var/lib/kivali/
  k3s/                      k3s --data-dir: the whole k3s state directory
    agent/containerd/       containerd image store and snapshots
    agent/images/           <- bind mount, read-only, of the root disk's
                               /usr/share/kivali/images (baked tarballs)
    server/db/              kine/sqlite: every object, Secrets included
    server/manifests/       auto-applied manifests (the HelmChart goes here)
    server/static/charts/   chart tarballs served at /static/charts/
    storage/                local-path PVs (kivali-data, agent scratch)
  kubelet/                  bound at /var/lib/kubelet (pod volumes)
  rancher/                  bound at /etc/rancher (k3s.yaml, node password)
  log/k3s.log, k3s.log.1    k3s output, this boot and the previous one
```

**Installing a chart.** Copy the chart tarball to
`/var/lib/kivali/k3s/server/static/charts/<name>-<version>.tgz` and
write a HelmChart into `/var/lib/kivali/k3s/server/manifests/`:

```yaml
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata: {name: kivali, namespace: kube-system}
spec:
  chart: https://%{KUBERNETES_API}%/static/charts/kivali-0.16.0.tgz
  targetNamespace: kivali
  createNamespace: true
```

`%{KUBERNETES_API}%` is expanded by the Helm controller to the
in-cluster API address; k3s serves `<data-dir>/server/static/` at
`https://<apiserver>/static/` and the controller's job trusts the
cluster CA. `selftest/kivali-selftest.yaml` is a working example; the
boot test proves this path end to end.

**Other guest paths.** `/var/run/kivali/uds` (the agent pods' hostPath)
is on `/run`, a tmpfs, so it is recreated empty each boot, as on any
node. The node is always named `kivali` (hostname and `--node-name`),
which keeps its identity across root-disk replacements.

## What the guest init does

PID 1 is busybox `init` (`rootfs/etc/inittab`): it reaps orphans, runs
`boot` once, starts `k3s-server`, `ready` and `trim` once each, keeps
`guest-agent` and `console-control` running, and runs `shutdown` before
powering off. The
scripts live in `rootfs/usr/libexec/kivali/`. `k3s-server` and `ready`
do nothing unless `boot` finished (`/run/kivali/booted`); if it did not,
`k3s-server` prints a FATAL and powers off.

`trim`: waits for this boot's READY, then runs `fstrim` on the data
disk and again every 24 hours, printing `KIVALI-VM: trim: done:
/var/lib/kivali: N bytes trimmed`. It is how `data.img` shrinks: a block
the guest wrote stays allocated on the host until it is discarded, and
the disk is otherwise never discarded (below).

`boot`: mounts proc/sys/dev/devpts, tmpfs on `/run`, `/tmp`, `/root`
and `/dev/shm`; cgroup2 on `/sys/fs/cgroup` with every controller
delegated to children; loads `rootfs/etc/kivali/modules` and coldplugs
device modules from sysfs; applies `rootfs/etc/sysctl.d/kivali.conf`
(bridge-nf-call-iptables/ip6tables, ip_forward, ipv6 forwarding,
inotify limits, `user.max_user_namespaces`, the kubelet's
overcommit/panic values); overlays tmpfs on `/etc` and `/var`; runs
`udhcpc` on the first NIC; takes the data disk from `kivali.data=`,
asks the format question if told to, and formats or checks the disk as
above; mounts it, makes the k3s layout (including `server/manifests` and
`server/static/charts`), and binds the image tarballs, kubelet dir and
`/etc/rancher`; makes `/` rshared.

**Modules** (`rootfs/etc/kivali/modules`). One list serves both
architectures and both hypervisors. A module that does not exist for
the architecture, or refuses to load on this hypervisor (`hv_sock`,
`hv_utils` and `hv_balloon` anywhere but Hyper-V), is named once in
`KIVALI-VM: boot: modules not loaded: …` and is otherwise harmless; on
Virtualization.framework that line lists the Hyper-V modules, on
Hyper-V `vmw_vsock_virtio_transport`. The order of the vsock lines
matters: the kernel accepts exactly one guest-to-host vsock transport,
the first to register (a second gets `EBUSY`), and the virtio transport
registers at load whether or not a virtio-vsock device exists, while
`hv_sock` refuses to load outside Hyper-V (`ENODEV`). So `vsock` and
`hv_sock` come before `vmw_vsock_virtio_transport`: on Hyper-V `hv_sock`
takes the slot, elsewhere the virtio transport does. With the virtio
transport first, the agent would be unreachable on Hyper-V. The
initramfs loads its own few modules (above) before any of this.

`k3s-server`: rotates `k3s.log` to `k3s.log.1`, then runs
`k3s server --data-dir=/var/lib/kivali/k3s
--disable=traefik,servicelb,metrics-server --node-name=kivali
--write-kubeconfig-mode=0600 --prefer-bundled-bin`. The Helm controller
stays on.

## Kernel

Alpine `linux-virt` 6.18, unmodified (`build/out/kernel.config`; the
exact release is in `build/out/kernel.release`).
What the guest relies on, and how it is provided: virtio PCI and
console built in, virtio-blk/net/rng/balloon/vsock as modules; ext4,
squashfs (zstd and xz) and overlayfs; cgroup v2 with cpu, cpuset, io,
memory, hugetlb, pids (and dmem), CFS bandwidth, BPF and the device
controller; all namespaces including `CONFIG_USER_NS=y` (unprivileged
user namespaces work, required at every boot); bridge, br_netfilter,
veth, vxlan (flannel); nf_tables with nft_compat, legacy iptables,
conntrack, NAT, and ipset with `xt_set` (kube-router's NetworkPolicy
controller); seccomp; PL031 RTC (the guest clock is correct at boot).
Not present on arm64: keyboard input drivers (hence the console verb
above).

On x86_64 the same kernel also carries Hyper-V, as its config shows:
`CONFIG_HYPERV=y` and `HYPERV_VMBUS=y` (vmbus built in), `HYPERV_NET=y`
(netvsc built in), `HYPERV_STORAGE=m` (`hv_storvsc`), `SCSI=y` with
`BLK_DEV_SD=m` (`sd_mod`), `HYPERV_VSOCKETS=m` (`hv_sock`),
`HYPERV_UTILS=m`, `HYPERV_BALLOON=m`, `SERIAL_8250_CONSOLE=y` with
`SERIAL_8250_PNP=y` (the `ttyS0` console) and `EFI_STUB=y`, so the
unified kernel image needs no zboot unpacking. `SCSI_SCAN_ASYNC=y` is
why the initramfs waits for the disks. The clock there is the CMOS RTC
(`RTC_DRV_CMOS=y`), and keyboard drivers are present.

**mkfs runs with `-E nodiscard,lazy_itable_init=0,lazy_journal_init=0`,
and the data disk is never mounted with `-o discard`; the only discard
is `trim`'s `fstrim` of the mounted filesystem.** Eager init means the
kernel never runs the background zeroing (ext4lazyinit) that would issue
write-zeroes for the life of the first boots; it costs no real
allocation (a data disk is about 680 MB allocated after a first boot),
most likely because mke2fs zeroes its tables with write-zeroes, which
VZ leaves unallocated. `boot` logs the data disk's `discard_max_bytes`,
`max_discard_segments` and `write_zeroes_max_bytes` once per boot. With
discard enabled at mkfs, re-formatting a data disk whose previous format
had been hard-stopped corrupted guest memory in some runs (an `Oops -
Undefined instruction` that wedged the guest, or `BUG: Bad rss-counter
state`); with `nodiscard` none did. The likely cause, unconfirmed, is
Virtualization.framework's discard (hole punching) on a partly allocated
image file; it is worth re-testing on later macOS releases.

## Memory on macOS

A running VM never gives memory back to macOS: the
`phys_footprint` of `com.apple.Virtualization.VirtualMachine` (the
Memory column in Activity Monitor) only grows, to the guest's high-water
mark, until the VM stops. So the VM's configured memory is its real
host cost, and the levers are that size and stopping an idle VM; the
build and test VMs (`scripts/build-vm.sh`, `scripts/test-vm.sh`) are
stopped after each run for this reason.

The image attaches no memory balloon. Virtualization.framework's only
reclaim device, `VZVirtioTraditionalMemoryBalloonDevice`, negotiates
only the MUST_TELL_HOST and DEFLATE_ON_OOM features (no free page
reporting), and nothing the guest balloons is released to the host,
not even under critical host memory pressure; the host compresses
ballooned pages like live data. Worse, the Linux driver zeroes each
page it inflates when `init_on_alloc=1` (the kernel's default here), so
inflating backs every guest page that the host had never touched.
Measured on an 8 GiB, 4-CPU VM (footprint in MB; fill 3 GiB of
urandom into a guest tmpfs, free it, inflate the balloon by 6.5 GiB,
60 s of `memory_pressure -l critical` on the host, deflate, then stop
and start the VM):

| Step | `init_on_alloc=0` | `init_on_alloc=1` |
| --- | --- | --- |
| booted | 1,821 | 1,807 |
| 3 GiB written | 4,870 | 4,849 |
| freed in the guest | 4,870 | 4,849 |
| balloon inflated | 4,871 | **8,198** |
| critical host pressure | 4,834 | 8,163 |
| deflated | 4,834 | 8,163 |
| VM stopped and started | **1,672** | **1,669** |

Stopping the VM is the one thing that returns the memory. Products that
do reclaim memory on macOS (libkrun/krunkit, OrbStack, Docker VMM) own
the guest RAM mapping on Hypervisor.framework and release pages with
`madvise` themselves; under Virtualization.framework the guest RAM
belongs to Apple's XPC helper, so no client can. Judge any such change
by system-wide free and compressor counters, not per-process footprint
alone.

## Decisions

| Decision | Chosen | Alternative and why not |
| --- | --- | --- |
| Base | Alpine 3.23 + its `linux-virt` kernel, root built with `apk --root` in Docker | LinuxKit: a second toolchain and kernel build to learn and maintain for the same result; Alpine's packaged kernel already has every option k3s needs, and Docker is already here |
| Kernel | stock `linux-virt`, unpacked from zboot (arm64) or used as is (amd64) | a custom kernel config: smaller and could add gpio-keys, but a kernel build to own; revisit if size or the power button matter |
| Boot media | tiny initramfs (0.7 MB) + squashfs root disk | initramfs-only: the 190 MB of k3s and tarballs would be unpacked into RAM on every boot; ext4 root disk: larger, and writable unless carefully mounted |
| Hyper-V boot | one VHDX: GPT with an ESP holding a unified kernel image as `EFI/BOOT/BOOTX64.EFI`, then the squashfs | a boot loader (systemd-boot, GRUB) on the ESP: a menu and a config to own for one entry; separate kernel and initrd files: Hyper-V has no direct kernel boot; a second disk for the ESP: one more attachment per VM |
| Finding the root | by content: the first `/dev/vd*`/`/dev/sd*` device or partition starting with `hsqs` | a fixed device name: differs between virtio-blk and Hyper-V SCSI; the GPT partition name `kivali-root`: Hyper-V only (the macOS root disk is a whole-disk squashfs with no partition table), so a second path to keep |
| Format decision on Hyper-V | asked on the console at each boot (`kivali.format-data=ask`) | a per-boot command line: Hyper-V has no direct kernel boot, and the command line is inside the UKI on the release's read-only root disk; a flag file on a disk: the host would have to write into a VHDX; a KVP or vsock message: needs the KVP daemon or the agent, which starts only after `boot` |
| Guest binaries | k3s downloaded and the agent compiled inside the Docker build | on the host: Defender quarantines the Go-built Linux executables mid-build on Windows |
| Root filesystem | read-only squashfs; tmpfs `/run`, `/tmp`, `/root`; tmpfs overlays on `/etc` and `/var` | a writable root or a whole-root overlay: hides what writes where; the read-only root guarantees an app update can swap it freely |
| Data disk mount | `/dev/vdb` (or `kivali.data=`) at `/var/lib/kivali`, `k3s --data-dir=/var/lib/kivali/k3s`, kubelet dir and `/etc/rancher` bound from it | data disk at k3s's default `/var/lib/rancher/k3s`: works too, but then kubelet and `/etc/rancher` (node password) need their own homes anyway; one mount point owns all state |
| When to format | the host says yes (flag `kivali.format-data=1`, or `KIVALI-FORMAT-DATA 1` to the console question) AND first MiB fully read as zeros; otherwise ext4 + label `kivali-data` required | the guest guessing from a zero first MiB alone: a disk the host did not just create could be wiped; `blkid` alone: an unrecognised signature would read as blank |
| cgroups | v2 only (`cgroup_no_v1=all`), controllers delegated at boot | v1/hybrid: deprecated by Kubernetes, and Alpine and k3s default to v2 |
| Guest init | busybox `init` + shell scripts | a static Go init: more code for the same mount/modprobe/exec sequence; busybox init already reaps zombies and sequences shutdown, and the supervisor's Go guest agent is the place for logic |
| Image import | k3s's own airgap import from a read-only bind mount | importing with `ctr` from the init: duplicate of what k3s does |
| Networking | VZ NAT, DHCP, outbound only; the org's port reaches the host over vsock | bridged: needs a restricted entitlement; a vsock user-mode network stack: a second network stack for what one forwarded port does |
| Shutdown | guest agent over vsock, console verb `KIVALI-POWEROFF` as fallback; Kivali pods deleted with their grace periods before k3s stops | `requestStop`: no driver in this kernel; stopping k3s first: kubelet is gone, so every container gets a flat kill instead of its grace period |
| Readiness of "this boot" | node `bootID` = guest `boot_id` | heartbeat time vs guest clock: depends on clocks agreeing |
| Logs | k3s log on the data disk, one previous boot kept | tmpfs: lost on the crash you want to debug |
| Memory balloon | none | Virtualization.framework's balloon releases nothing to the host, and inflating can grow the footprint ("Memory on macOS") |

## The guest agent and the supervisor

`kivali-guest` (`cmd/kivali-guest`, compiled static for the image's
architecture in the Dockerfile's `guest` stage, under "Built inside
Docker") is baked at
`/usr/libexec/kivali/kivali-guest` and started by init (`respawn`, through
`guest-agent`, which puts its stdin on `/dev/null`). It listens on vsock
port 1024 (through `vmw_vsock_virtio_transport` on
Virtualization.framework, `hv_sock` on Hyper-V, where the host dials the
VM's id and the port's service id) and serves the host's
`kivali-supervisor`: status, exec, a PTY,
write-file into the data directory, image import, a TCP proxy to the
guest's loopback (the org's port forward) and the clean shutdown, which
runs the same `shutdown` sequence through `poweroff`; the console verb
stays as the fallback. Its log is `log/guest.log` on the data disk. The
protocol, the supervisor and its CLI are in [supervisor.md](../docs/developers/supervisor.md).

The agent accepts only connections from the host's vsock CID (2).
`rootfs/etc/modprobe.d/kivali.conf` blacklists `vsock_loopback` and
`vhost_vsock` (`blacklist` plus `install … /bin/false`, so neither
autoloading nor an explicit modprobe can load them): no guest process can
open a vsock connection to the agent even if the module list changes.
`hv_sock` is allowed: like the virtio transport it only connects the
guest to its host.

**No inbound connections on the NAT NIC.** VZ NAT's guest address is
reachable from the Mac's other users through the vmnet bridge. `boot`
drops every inbound TCP SYN, and VXLAN (udp/8472, flannel's port; a single
node never receives remote VXLAN), on that NIC before k3s starts, in an nftables
table of its own (`inet kivali_inbound`, prerouting at raw priority), with
`nft` from Alpine's `nftables` package (the root disk has no iptables until
k3s unpacks its own). Outbound traffic and its replies, DHCP, loopback,
cni0/flannel and vsock are unaffected; a failure to install the rule is a
FATAL.

The boot scripts also record their markers for the agent: `ready` writes
this boot's id to `/run/kivali/ready` just before printing READY, and every
FATAL writes its reason to `/run/kivali/fatal`.

`make -C vm image KIVALI_CHART=<tgz>` bakes that chart at
`/usr/share/kivali/charts/` so a first install needs no download;
`VM_VERSION` is written to `/usr/share/kivali/vm-version`.

A current cost: k3s re-imports **every** tarball in `agent/images` on
**every** start (5 k3s images: ~4 s; the three Kivali `:dev` images: ~8 s
more). The content is already in containerd, so this costs read time,
not disk, but it grows with what is baked. The org's port reaches the host
through the agent's vsock proxy; the NIC stays VZ NAT, outbound only.

## Files

```
vm/Makefile                      image, dev-images, boottest, clean (ARCH)
vm/Dockerfile                    the build, k3s and the guest agent included
vm/k3s-images.lock               pinned digests of the five k3s images (both arches)
vm/scripts/fetch-k3s.sh          k3s sha256 list + the five k3s images, per arch
vm/scripts/unzboot.sh            vmlinuz -> raw arm64 Image (zboot) or bzImage, magic checked
vm/initramfs/init                find the root disk by content and mount it
vm/rootfs/                       files copied into the root disk
vm/selftest/                     the self-test chart and HelmChart
vm/boottest/                     Go boot test (Code-Hex/vz) + entitlements
```
