# syntax=docker/dockerfile:1.27
# check=skip=InvalidDefaultArgInFrom
#
# (The check is skipped because GO_VERSION deliberately has no default:
# the Makefile passes the root Dockerfile's, the one source of it.)
#
# Builds the Kivali Desktop guest for one architecture, TARGETARCH:
# arm64 for Virtualization.framework (macOS), amd64 for Hyper-V
# (Windows). Both get a kernel, a small initramfs that finds the root
# disk, and a read-only squashfs root disk holding busybox, k3s, the
# guest init and the baked airgap image tarballs; amd64 adds root.vhdx,
# a bootable GPT disk (an EFI system partition with a unified kernel
# image, then the squashfs). Driven by vm/Makefile (`make -C vm image
# ARCH=...`), which builds target artifacts-$ARCH for linux/$ARCH and
# supplies these named build contexts:
#
#   k3sdl         the k3s release's sha256sum-<arch>.txt (text only)
#   k3simages     k3s-images.tar (the five k3s images, for the target arch)
#   kivaliimages  Kivali image tarballs, possibly none
#   kivalicharts  the Kivali chart tarball of the same release, possibly none
#   src           the repository root, for the guest agent's sources
#
# The k3s binary and the guest agent (kivali-guest) are downloaded and
# built here, in the k3s and guest stages, never on the host: no Linux
# executable is written to the host's filesystem (vm/README.md, "Built
# inside Docker").
#
# VM_VERSION is written to /usr/share/kivali/vm-version and stamped
# into the guest agent. K3S_VERSION and GO_VERSION have no default; the
# Makefile passes them (GO_VERSION read from the root Dockerfile).
#
# The final stages are `scratch` holding only the artifacts, exported
# with `--output type=local`.

ARG ALPINE=alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
ARG DEBIAN=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG GO_VERSION

# ---------------------------------------------------------------- k3s
# The k3s binary for TARGETARCH, downloaded from the release and
# checked against the release's own sha256 list (fetched by
# scripts/fetch-k3s.sh, context k3sdl). The release names it `k3s` for
# amd64 and `k3s-arm64` for arm64. Runs on the build host's platform:
# it only downloads.
FROM --platform=$BUILDPLATFORM ${ALPINE} AS k3s
RUN apk add --no-cache curl file
ARG TARGETARCH
ARG K3S_VERSION
COPY --from=k3sdl sha256sum-${TARGETARCH}.txt /tmp/
RUN set -eu; \
    [ -n "$K3S_VERSION" ] || { echo "k3s: K3S_VERSION is not set" >&2; exit 1; }; \
    case "$TARGETARCH" in \
      amd64) asset=k3s ;; \
      arm64) asset=k3s-arm64 ;; \
      *) echo "k3s: unsupported architecture '$TARGETARCH'" >&2; exit 1 ;; \
    esac; \
    want=$(awk -v a="$asset" '{sub(/\r$/, "")} $2 == a {print $1}' "/tmp/sha256sum-$TARGETARCH.txt"); \
    [ -n "$want" ] || { echo "k3s: no sha256 for $asset in sha256sum-$TARGETARCH.txt" >&2; exit 1; }; \
    url="https://github.com/k3s-io/k3s/releases/download/$(printf '%s' "$K3S_VERSION" | sed 's/+/%2B/')/$asset"; \
    echo "k3s: $url"; \
    curl -fsSL --retry 3 -o /tmp/k3s "$url"; \
    echo "$want  /tmp/k3s" | sha256sum -c -; \
    mkdir -p /out; \
    mv /tmp/k3s /out/k3s; \
    chmod 0755 /out/k3s; \
    file /out/k3s

# ---------------------------------------------------------------- guest
# kivali-guest, the vsock agent, static for TARGETARCH, cross-compiled
# on the build host's platform from the repository (context src). Only
# go.mod, go.sum and the packages the guest imports are copied in.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS guest
RUN apk add --no-cache file
WORKDIR /src
COPY --from=src go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY --from=src cmd/kivali-guest/ cmd/kivali-guest/
COPY --from=src internal/supervisor/guestapi/ internal/supervisor/guestapi/
COPY --from=src internal/supervisor/integrity/ internal/supervisor/integrity/
ARG TARGETARCH
ARG VM_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" go build -trimpath \
      -ldflags "-s -w -X main.version=$VM_VERSION" \
      -o /out/kivali-guest ./cmd/kivali-guest; \
    file /out/kivali-guest; \
    file /out/kivali-guest | grep -q 'statically linked'

# ---------------------------------------------------------------- kernel
# Alpine's linux-virt: a virtio-first kernel with cgroup v2, user
# namespaces, overlayfs, netfilter/nftables, ipset, vxlan and vsock;
# on x86_64 also Hyper-V (vmbus, storvsc, netvsc, hv_sock, the
# integration services and the balloon). On arm64 it is built with
# EFI_ZBOOT, so /boot/vmlinuz-virt is a PE wrapper around a gzip
# payload, and Virtualization.framework's Linux boot loader wants the
# raw arm64 Image: the payload is unpacked here. On x86_64 it is a
# bzImage with the EFI stub, used as is.
FROM ${ALPINE} AS kernel
ARG TARGETARCH
RUN apk add --no-cache linux-virt kmod
COPY --chmod=0755 scripts/unzboot.sh /usr/local/bin/unzboot
RUN mkdir -p /out \
 && unzboot /boot/vmlinuz-virt /out/Image "$TARGETARCH" \
 && cp /boot/config-*-virt /out/kernel.config \
 && ls /lib/modules > /out/kernel.release \
 && apk list -I linux-virt > /out/kernel-packages.txt \
 && cp /lib/apk/db/installed /out/kernel-apk-installed \
 && { grep -E '^CONFIG_(HYPERV[A-Z_]*|EFI_STUB|SCSI|BLK_DEV_SD|SQUASHFS|SQUASHFS_ZSTD|SERIAL_8250_CONSOLE|VSOCKETS|VIRTIO_VSOCKETS)=' /out/kernel.config || true; }

# ---------------------------------------------------------------- common-licenses
# Debian's texts of the GNU licenses, for the root disk's
# /usr/share/doc/kivali/licenses (text only, so the build host's platform).
FROM --platform=$BUILDPLATFORM ${DEBIAN} AS common-licenses

# ---------------------------------------------------------------- rootfs
# A fresh Alpine root with only what the guest runs: busybox (init,
# shell, udhcpc, mount), kmod (modprobe for the .ko.gz modules),
# e2fsprogs (mkfs.ext4, e2fsck) and the CA bundle. No apk, no
# OpenRC, no kernel image, no firmware.
FROM ${ALPINE} AS rootfs
RUN mkdir -p /rootfs \
 && apk add --no-cache --root /rootfs --initdb \
      --keys-dir /etc/apk/keys --repositories-file /etc/apk/repositories \
      alpine-baselayout-data alpine-baselayout busybox busybox-binsh musl \
      kmod e2fsprogs ca-certificates-bundle nftables \
 && mkdir -p /out \
 && apk list -I --root /rootfs | sort > /out/rootfs-packages.txt \
 && cp /rootfs/lib/apk/db/installed /out/rootfs-apk-installed \
 && rm -rf /rootfs/lib/apk /rootfs/etc/apk /rootfs/var/cache/apk
# Alpine keeps only the current patch level of each package, so these
# are recorded (build/out/packages.txt), not pinned.
COPY --from=kernel /out/kernel-packages.txt /tmp/
RUN { echo "# kernel"; cat /tmp/kernel-packages.txt; echo "# root disk"; cat /out/rootfs-packages.txt; } > /out/packages.txt
COPY --from=kernel /lib/modules /rootfs/lib/modules
COPY --from=k3s --chmod=0755 /out/k3s /rootfs/usr/local/bin/k3s
COPY --from=k3simages k3s-images.tar /rootfs/usr/share/kivali/images/
COPY --from=kivaliimages . /rootfs/usr/share/kivali/images/
COPY --from=kivalicharts . /rootfs/usr/share/kivali/charts/
# The guest init. A Windows checkout has CRLF line endings and no
# executable bits, so both are fixed here, whatever the host: CRs are
# stripped (inittab, the scripts and the lists are read line by line
# in the guest) and every mode is set explicitly.
COPY rootfs/ /tmp/overlay/
RUN set -e; \
    cr=$(printf '\r'); \
    cd /tmp/overlay; \
    find . -type f -exec sed -i "s/${cr}\$//" {} +; \
    find . -type d -exec chmod 0755 {} +; \
    find . -type f -exec chmod 0644 {} +; \
    chmod 0755 usr/libexec/kivali/*; \
    cp -a . /rootfs/; \
    rm -rf /tmp/overlay
COPY --from=guest --chmod=0755 /out/kivali-guest /rootfs/usr/libexec/kivali/kivali-guest
# The GNU licenses' texts, from Debian's common-licenses, beside the
# root disk's SOURCES.md (stage rootdisk).
COPY --from=common-licenses /usr/share/common-licenses/GPL-2 /usr/share/common-licenses/GPL-3 \
     /usr/share/common-licenses/LGPL-2 /usr/share/common-licenses/LGPL-2.1 /usr/share/common-licenses/LGPL-3 \
     /rootfs/usr/share/doc/kivali/licenses/
COPY selftest/ /tmp/selftest/
ARG VM_VERSION=dev
RUN set -e; \
    cd /rootfs/usr/local/bin; \
    for t in kubectl crictl ctr; do ln -sf k3s "$t"; done; \
    rm -f /rootfs/usr/share/kivali/images/.gitkeep /rootfs/usr/share/kivali/charts/.gitkeep; \
    echo "$VM_VERSION" > /rootfs/usr/share/kivali/vm-version; \
    mkdir -p /rootfs/var/lib/kivali /rootfs/var/lib/kubelet /rootfs/etc/rancher \
             /rootfs/usr/share/kivali/selftest; \
    # The self-test chart is a plain helm chart tarball: a top-level
    # directory named after the chart, packed with tar.
    tar -C /tmp/selftest -czf /rootfs/usr/share/kivali/selftest/kivali-selftest-0.1.0.tgz kivali-selftest; \
    cp /tmp/selftest/kivali-selftest.yaml /rootfs/usr/share/kivali/selftest/; \
    echo kivali > /rootfs/etc/hostname; \
    printf '127.0.0.1\tlocalhost kivali\n::1\tlocalhost kivali\n' > /rootfs/etc/hosts; \
    : > /rootfs/etc/resolv.conf; \
    # Applet links (existing files such as kmod's modprobe are kept),
    # and busybox init replaces OpenRC.
    chroot /rootfs /bin/busybox --install -s; \
    ln -sf /bin/busybox /rootfs/sbin/init; \
    test -x /rootfs/sbin/modprobe; test -x /rootfs/sbin/mkfs.ext4; \
    test -x /rootfs/usr/libexec/kivali/boot; test -x /rootfs/usr/libexec/kivali/kivali-guest; \
    chroot /rootfs /bin/sh -c 'command -v fstrim' >/dev/null; \
    readlink /rootfs/sbin/modprobe | grep -q kmod

# ---------------------------------------------------------------- initramfs
# A ~1 MB initramfs: static busybox, the few modules needed to see the
# root disk (decompressed, so busybox insmod can load them), and /init.
# On every architecture virtio_blk and squashfs; on amd64 also the
# Hyper-V storage path, hv_storvsc and the SCSI disk class sd_mod (vmbus
# itself is built in). Their dependencies, scsi_mod among them, come
# from modprobe.
FROM ${ALPINE} AS initramfs
ARG TARGETARCH
RUN apk add --no-cache busybox-static kmod cpio squashfs-tools
COPY --from=kernel /lib/modules /lib/modules
COPY initramfs/init /initramfs/init
RUN set -e; \
    kver=$(ls /lib/modules); \
    case "$TARGETARCH" in \
      amd64) mods='virtio_blk squashfs hv_storvsc sd_mod' ;; \
      *) mods='virtio_blk squashfs' ;; \
    esac; \
    cd /initramfs; \
    # A Windows checkout has CRLF line endings.
    sed -i "s/$(printf '\r')\$//" init; \
    mkdir -p bin dev proc sys newroot lib/modules; \
    cp /bin/busybox.static bin/busybox; \
    chmod 0755 init bin/busybox; \
    : > lib/modules/load.list; \
    for m in $mods; do \
      modprobe -S "$kver" --show-depends "$m" | awk '$1 == "insmod" {print $2}'; \
    done | awk '!seen[$0]++' > /tmp/mods; \
    while read -r path; do \
      name=$(basename "$path" .gz); \
      gunzip -c "$path" > "lib/modules/$name"; \
      echo "$name" >> lib/modules/load.list; \
    done < /tmp/mods; \
    cat lib/modules/load.list; \
    for m in $mods; do \
      grep -q "^$m\.ko\$" lib/modules/load.list || { echo "initramfs: $m is not a loadable module of $kver" >&2; exit 1; }; \
    done; \
    mkdir -p /out; \
    find . -print0 | sort -z | cpio --null -o -H newc --reproducible --quiet | gzip -9n > /out/initramfs.gz

# ---------------------------------------------------------------- root disk
# SOURCES.md, written into the root disk's /usr/share/doc/kivali before
# the squashfs is made, and exported: where the source of the GPL and LGPL software in the kernel,
# the initramfs (this stage's busybox-static), the root disk, k3s and its
# images is published, and Kivali's written offer of it
# (scripts/third-party-sources.sh).
FROM initramfs AS rootdisk
COPY --from=rootfs /rootfs /rootfs
COPY --from=kernel /out/kernel-apk-installed /tmp/sources/
COPY --from=rootfs /out/rootfs-apk-installed /tmp/sources/
COPY --from=k3sdl version.sh /tmp/sources/k3s-version.sh
COPY k3s-images.lock /tmp/sources/
COPY --from=src scripts/third-party-sources.sh /tmp/sources/
ARG K3S_VERSION
RUN set -eu; \
    s=/tmp/sources; \
    alpine=$(cat /etc/alpine-release); \
    { sh $s/third-party-sources.sh header "the Kivali VM image"; \
      sh $s/third-party-sources.sh apk "Kernel" $s/kernel-apk-installed "$alpine" linux-virt; \
      sh $s/third-party-sources.sh apk "Initramfs" /lib/apk/db/installed "$alpine" busybox-static; \
      sh $s/third-party-sources.sh apk "Root disk" $s/rootfs-apk-installed "$alpine"; \
      sh $s/third-party-sources.sh k3s "$K3S_VERSION" $s/k3s-version.sh; \
      sh $s/third-party-sources.sh images $s/k3s-images.lock; \
      printf '\nThe GNU licenses: /usr/share/doc/kivali/licenses/ in the root disk.\n'; \
    } > /out/SOURCES.md; \
    cp /out/SOURCES.md /rootfs/usr/share/doc/kivali/SOURCES.md; \
    rm -rf $s
RUN set -e; \
    mksquashfs /rootfs /out/root.squashfs -comp zstd -noappend -all-root -quiet; \
    du -sh /rootfs /out/root.squashfs; \
    unsquashfs -lls /out/root.squashfs | grep -E ' squashfs-root/usr/(libexec/kivali/kivali-guest|local/bin/k3s)$' > /tmp/bins; \
    cat /tmp/bins; \
    [ "$(wc -l < /tmp/bins)" -eq 2 ]

# ---------------------------------------------------------------- uki (amd64)
# The unified kernel image Hyper-V boots: the kernel, the initramfs and
# the command line in one EFI binary, built with systemd's ukify and
# its x86_64 stub (Debian packages both). UKI_CMDLINE is the command
# line the guest boots with; the macOS backend passes its own per boot
# instead. Debian's packages are recorded (packages.txt), not pinned,
# like Alpine's.
FROM ${DEBIAN} AS uki
ARG TARGETARCH
RUN [ "$TARGETARCH" = amd64 ] || { echo "uki: the unified kernel image is amd64 only (got '$TARGETARCH')" >&2; exit 1; }; \
    apt-get update -q \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends systemd-ukify systemd-boot-efi \
 && rm -rf /var/lib/apt/lists/*
ARG UKI_CMDLINE="console=ttyS0,115200 cgroup_no_v1=all panic=10 kivali.data=scsi:0:0:0:1 kivali.format-data=ask"
ARG VM_VERSION=dev
COPY --from=kernel /out/Image /out/kernel.release /in/
COPY --from=initramfs /out/initramfs.gz /in/
RUN set -e; \
    mkdir -p /out; \
    # .osrel names the guest; without it ukify embeds this build
    # container's own (Debian's) os-release.
    printf 'ID=kivali\nNAME="Kivali Desktop VM"\nVERSION_ID=%s\n' "$VM_VERSION" > /in/os-release; \
    ukify build \
      --linux=/in/Image \
      --initrd=/in/initramfs.gz \
      --cmdline="$UKI_CMDLINE" \
      --os-release=@/in/os-release \
      --uname="$(cat /in/kernel.release)" \
      --stub=/usr/lib/systemd/boot/efi/linuxx64.efi.stub \
      --output=/out/kivali.efi; \
    ukify inspect /out/kivali.efi; \
    dpkg-query -W -f '${Package}-${Version}\n' systemd-ukify systemd-boot-efi > /out/uki-packages.txt; \
    { . /etc/os-release; echo "# $PRETTY_NAME"; dpkg-query -W -f '${Package} ${Version} ${source:Package} ${source:Version}\n' systemd-boot-efi; } > /out/uki-sources.txt

# ---------------------------------------------------------------- vhdx (amd64)
# root.vhdx: a dynamic VHDX holding a GPT disk, 1 MiB aligned:
#   partition 1  EFI system partition, FAT32, 64 MiB, type EF00, holding
#                one file, EFI/BOOT/BOOTX64.EFI (the unified kernel image)
#   partition 2  the squashfs root disk, type 8300, named kivali-root
# Built as a sparse raw image without root or loop devices (mkfs.vfat
# and mtools on a file, sgdisk, dd conv=notrunc,sparse), read back, then
# converted with qemu-img. Runs on the build host's platform: nothing
# here is architecture-specific.
FROM --platform=$BUILDPLATFORM ${ALPINE} AS vhdx
RUN apk add --no-cache coreutils dosfstools mtools sgdisk qemu-img squashfs-tools
COPY --from=uki /out/kivali.efi /out/uki-packages.txt /out/uki-sources.txt /work/
COPY --from=src scripts/third-party-sources.sh /work/
COPY --from=rootdisk /out/SOURCES.md /work/
COPY --from=rootdisk /out/root.squashfs /work/
COPY --from=rootfs /out/packages.txt /work/
RUN set -eu; \
    cd /work; \
    export MTOOLS_SKIP_CHECK=1; \
    mib=1048576; \
    esp_mib=64; \
    esp_lba=2048; \
    root_lba=$((esp_lba + esp_mib * 2048)); \
    root_bytes=$(stat -c %s root.squashfs); \
    root_mib=$(( (root_bytes + mib - 1) / mib )); \
    total_mib=$((1 + esp_mib + root_mib + 1)); \
    # The EFI system partition: FAT32 with one 512-byte sector per
    # cluster (at 64 MiB, larger clusters fall below FAT32's minimum
    # cluster count); hidden sectors = its start on the disk.
    truncate -s "${esp_mib}M" esp.img; \
    mkfs.vfat -F 32 -s 1 -S 512 -h "$esp_lba" -n KIVALI-EFI esp.img; \
    mmd -i esp.img ::/EFI ::/EFI/BOOT; \
    mcopy -i esp.img kivali.efi ::/EFI/BOOT/BOOTX64.EFI; \
    # The disk: the ESP at 1 MiB, the squashfs right after it, 1 MiB
    # at the end for the backup GPT.
    truncate -s "${total_mib}M" disk.raw; \
    sgdisk \
      --new="1:$esp_lba:$((root_lba - 1))" --typecode=1:EF00 --change-name='1:EFI system partition' \
      --new="2:$root_lba:$((root_lba + root_mib * 2048 - 1))" --typecode=2:8300 --change-name=2:kivali-root \
      disk.raw; \
    dd if=esp.img of=disk.raw bs=1M seek=1 conv=notrunc,sparse status=none; \
    dd if=root.squashfs of=disk.raw bs=1M seek=$((1 + esp_mib)) conv=notrunc,sparse status=none; \
    # Read back: the table, the EFI file, the squashfs in place.
    sgdisk -v disk.raw; \
    sgdisk -p disk.raw; \
    mdir -i "disk.raw@@$((esp_lba * 512))" ::/EFI/BOOT; \
    mcopy -n -i "disk.raw@@$((esp_lba * 512))" ::/EFI/BOOT/BOOTX64.EFI /tmp/bootx64.efi; \
    cmp /tmp/bootx64.efi kivali.efi; \
    cmp -n "$root_bytes" root.squashfs disk.raw 0 "$((root_lba * 512))"; \
    unsquashfs -o "$((root_lba * 512))" -s disk.raw | head -n 4; \
    mkdir -p /out; \
    qemu-img convert -f raw -O vhdx -o subformat=dynamic disk.raw /out/root.vhdx; \
    qemu-img info /out/root.vhdx; \
    qemu-img compare -f raw -F vhdx disk.raw /out/root.vhdx; \
    cp kivali.efi /out/; \
    { cat packages.txt; echo "# unified kernel image"; cat uki-packages.txt; } > /out/packages.txt; \
    { cat SOURCES.md; sh third-party-sources.sh debian "Unified kernel image (the EFI stub)" uki-sources.txt; } > /out/SOURCES.md; \
    rm -f disk.raw esp.img; \
    ls -l /out

# ---------------------------------------------------------------- artifacts
# What every architecture exports.
FROM scratch AS artifacts
COPY --from=kernel /out/Image /out/kernel.config /out/kernel.release /
COPY --from=initramfs /out/initramfs.gz /
COPY --from=rootdisk /out/root.squashfs /
COPY --from=rootfs /out/packages.txt /
COPY --from=rootdisk /out/SOURCES.md /

# linux/arm64, Virtualization.framework: the files above.
FROM artifacts AS artifacts-arm64

# linux/amd64, Hyper-V: also the bootable disk and, for inspection, the
# unified kernel image inside it; packages.txt adds the EFI stub's
# packages.
FROM artifacts AS artifacts-amd64
COPY --from=vhdx /out/root.vhdx /out/kivali.efi /out/packages.txt /out/SOURCES.md /
