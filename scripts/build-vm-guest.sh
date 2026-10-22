#!/bin/sh
#
# build-vm-guest.sh — the guest half of scripts/build-vm.sh: runs as root
# inside a Kivali VM (busybox sh), installed there by `build-vm.sh up`
# at /var/lib/kivali/build/bin/guest.sh and called through
# `kivali-supervisor exec`. Nothing here is typed by hand; build-vm.sh
# documents the host side.
#
# The build engine is BuildKit (buildkitd + buildctl, two static
# binaries from the moby/buildkit image, pulled into the VM's containerd
# on first use and copied out of it) run as a plain process of the
# guest, with k3s's containerd as its worker: images it builds land in
# the k8s.io namespace, where the cluster runs them, and its snapshots
# and cache live on the data disk and survive a stop. A plain process,
# not a container: BuildKit mounts each build step's root filesystem
# itself and hands containerd the path, which has to mean the same thing
# to both, and containerd runs on the guest.
#
#   guest.sh imported                 wait until k3s has imported the root disk's image tarballs this boot
#   guest.sh buildkitd                start buildkitd if it is not running (pulls the image once)
#   guest.sh put <name>               a tar on stdin becomes build context ctx/<name> (replacing it)
#   guest.sh get <name>               out/<name> as a tar on stdout
#   guest.sh clean-out <name>         empty out/<name>
#   guest.sh build <buildctl args>    buildctl build; contexts are ctx/<name>, outputs out/<name>;
#                                     each image output's name and digest is recorded in built
#   guest.sh pull <ref>...            pull images for the guest's architecture into containerd
#   guest.sh digest <ref>             the digest containerd recorded for ref (the index's, for a tag)
#   guest.sh export <ref>...          the images for the guest's architecture as one OCI tar on stdout;
#                                     refuses a ref built here that no longer names what was built
#   guest.sh arch                     amd64 or arm64
#   guest.sh stop                     stop buildkitd
#
# Every boot, k3s imports the image tarballs baked into the root disk
# the VM booted (vm/rootfs/usr/libexec/kivali/boot), in the background,
# seconds after the VM is up; an image there with the name of one built
# here takes the name back. A Kivali Desktop installed from `make
# desktop-trial` bakes the same :dev names `make images VERSION=dev`
# builds. So `build-vm.sh up` waits for the import (imported) before
# anything is built or exported, an image is exported in the boot that
# built it, and export checks it against built.
set -eu

# Pinned by digest; bump both together. The digest is the image
# index's, as Docker Hub lists it, so one pin serves both architectures.
BUILDKIT_IMAGE='docker.io/moby/buildkit:v0.33.1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea'

BASE=/var/lib/kivali/build
CTX=$BASE/ctx
OUT=$BASE/out
# One line per image a build here produced: its name and digest.
BUILT=$BASE/built
# This boot's k3s log (vm/rootfs/usr/libexec/kivali/k3s-server keeps the
# previous boot's as .1), and the root disk's image tarballs as k3s
# names them: boot binds /usr/share/kivali/images at its agent/images.
K3S_LOG=/var/lib/kivali/log/k3s.log
K3S_IMAGES=/var/lib/kivali/k3s/agent/images
IMPORT_WAIT_S=300
BK=/var/lib/kivali/buildkit
SOCK=unix:///run/buildkit/buildkitd.sock
NS=k8s.io

ctr() { k3s ctr -n "$NS" "$@"; }
buildctl() { "$BK/bin/buildctl" --addr "$SOCK" "$@"; }

# `ctr images ls` prints REF TYPE DIGEST SIZE PLATFORMS LABELS.
digest_of() { ctr images ls | awk -v r="$1" '$1 == r {print $3; exit}'; }

# The name an --output spec gives its image (type=image,name=<ref>,...);
# nothing for any other output type.
image_name() {
  case ",$1," in
    *,type=image,*) printf '%s\n' "$1" | tr ',' '\n' | sed -n 's/^name=//p' ;;
  esac
}

built_digest() { awk -v r="$1" '$1 == r {print $2}' "$BUILT" 2>/dev/null || true; }

record_built() {
  d=$(digest_of "$1")
  if [ -z "$d" ]; then
    echo "guest: the build named $1, but containerd has no image of that name" >&2
    exit 1
  fi
  { awk -v r="$1" '$1 != r' "$BUILT" 2>/dev/null || true; printf '%s %s\n' "$1" "$d"; } >"$BUILT.new"
  mv "$BUILT.new" "$BUILT"
}

arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    aarch64) echo arm64 ;;
    *) echo "guest: unsupported architecture $(uname -m)" >&2; exit 1 ;;
  esac
}

# A name is one path component, as the host side makes them.
name() {
  case "$1" in
    '' | . | .. | */* | .*) echo "guest: bad name '$1'" >&2; exit 2 ;;
  esac
}

# The binaries, copied out of the pinned image through a short-lived
# container (the image's own cp, with the target directory mounted).
binaries() {
  if [ -x "$BK/bin/buildkitd" ] && [ -x "$BK/bin/buildctl" ] && [ "$(cat "$BK/bin/.image" 2>/dev/null)" = "$BUILDKIT_IMAGE" ]; then
    return 0
  fi
  echo "guest: fetching BuildKit ($BUILDKIT_IMAGE)"
  mkdir -p "$BK/bin"
  ctr images pull "$BUILDKIT_IMAGE" >/dev/null
  ctr containers delete kivali-buildkit-extract >/dev/null 2>&1 || true
  ctr run --mount "type=bind,src=$BK/bin,dst=/out,options=rbind:rw" "$BUILDKIT_IMAGE" kivali-buildkit-extract \
    cp -a /usr/bin/buildkitd /usr/bin/buildctl /out/
  ctr containers delete kivali-buildkit-extract >/dev/null
  printf '%s\n' "$BUILDKIT_IMAGE" > "$BK/bin/.image"
  "$BK/bin/buildkitd" --version
}

running() { buildctl debug workers >/dev/null 2>&1; }

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
imported)
  # k3s logs each tarball's import as "Imported <n> images from <file>
  # in <time>", or its failure as "failed to import <file>: ...".
  i=0
  while :; do
    pending=
    for f in "$K3S_IMAGES"/*; do
      [ -f "$f" ] || continue
      grep -qF -e "images from $f in " -e "failed to import $f:" "$K3S_LOG" 2>/dev/null || pending="$pending ${f##*/}"
    done
    [ -n "$pending" ] || exit 0
    if [ $i -ge "$IMPORT_WAIT_S" ]; then
      echo "guest: k3s has not imported$pending after ${IMPORT_WAIT_S}s (its log: $K3S_LOG)" >&2
      exit 1
    fi
    [ $i -ne 0 ] || echo "guest: waiting for k3s to import$pending"
    i=$((i + 1))
    sleep 1
  done
  ;;
buildkitd)
  mkdir -p "$BK/root" "$CTX" "$OUT" /run/buildkit
  binaries
  if running; then
    echo "guest: buildkitd is running"
    exit 0
  fi
  # setsid: its own session, so the exec that started it can end
  # without taking it along (an exec's process group is killed when
  # the stream closes).
  setsid "$BK/bin/buildkitd" \
    --oci-worker=false --containerd-worker=true \
    --containerd-worker-addr=/run/k3s/containerd/containerd.sock \
    --containerd-worker-namespace="$NS" \
    --root="$BK/root" \
    --addr="$SOCK" \
    >"$BASE/buildkitd.log" 2>&1 </dev/null &
  i=0
  while [ $i -lt 30 ]; do
    if running; then
      echo "guest: buildkitd started"
      exit 0
    fi
    i=$((i + 1))
    sleep 1
  done
  echo "guest: buildkitd did not start; its log:" >&2
  tail -n 20 "$BASE/buildkitd.log" >&2
  exit 1
  ;;
put)
  name "$1"
  rm -rf "${CTX:?}/$1"
  mkdir -p "$CTX/$1"
  tar -xf - -C "$CTX/$1"
  ;;
get)
  name "$1"
  tar -C "$OUT/$1" -cf - .
  ;;
clean-out)
  name "$1"
  rm -rf "${OUT:?}/$1"
  mkdir -p "$OUT/$1"
  ;;
build)
  cd "$CTX"
  names=
  prev=
  for a in "$@"; do
    case "$prev" in --output | -o) names="$names $(image_name "$a")" ;; esac
    case "$a" in --output=*) names="$names $(image_name "${a#--output=}")" ;; esac
    prev=$a
  done
  buildctl build "$@"
  for n in $names; do
    record_built "$n"
  done
  ;;
pull)
  for ref in "$@"; do
    ctr images pull --platform "linux/$(arch)" "$ref" >/dev/null
    echo "guest: pulled $ref"
  done
  ;;
digest)
  digest_of "$1"
  ;;
export)
  # A ref never built here (pulled, or baked) is exported as it is.
  stale=
  moved=
  for ref in "$@"; do
    want=$(built_digest "$ref")
    [ -n "$want" ] || continue
    got=$(digest_of "$ref")
    if [ -z "$got" ]; then
      echo "guest: $ref is gone; the last build of it here produced $want" >&2
      stale=1
    elif [ "$got" != "$want" ]; then
      echo "guest: $ref is $got, not $want, the image the last build of it here produced" >&2
      stale=1
      moved=1
    fi
  done
  if [ -n "$moved" ]; then
    echo "guest: the VM booted after that build, and k3s gave the name back to the root disk's image of that name; build and export in one boot (make dev-vm does)" >&2
  fi
  [ -z "$stale" ] || exit 1
  ctr images export --platform "linux/$(arch)" - "$@"
  ;;
arch)
  arch
  ;;
stop)
  pkill -x buildkitd >/dev/null 2>&1 || true
  echo "guest: buildkitd stopped"
  ;;
*)
  sed -n '2,/^set -eu/{/^#/s/^# \{0,1\}//p}' "$0" >&2
  exit 2
  ;;
esac
