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
#   guest.sh buildkitd                start buildkitd if it is not running (pulls the image once)
#   guest.sh put <name>               a tar on stdin becomes build context ctx/<name> (replacing it)
#   guest.sh get <name>               out/<name> as a tar on stdout
#   guest.sh clean-out <name>         empty out/<name>
#   guest.sh build <buildctl args>    buildctl build; contexts are ctx/<name>, outputs out/<name>
#   guest.sh pull <ref>...            pull images for the guest's architecture into containerd
#   guest.sh digest <ref>             the digest containerd recorded for ref (the index's, for a tag)
#   guest.sh export <ref>...          the images for the guest's architecture as one OCI tar on stdout
#   guest.sh arch                     amd64 or arm64
#   guest.sh stop                     stop buildkitd
set -eu

# Pinned by digest; bump both together. The digest is the image
# index's, as Docker Hub lists it, so one pin serves both architectures.
BUILDKIT_IMAGE='docker.io/moby/buildkit:v0.33.1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea'

BASE=/var/lib/kivali/build
CTX=$BASE/ctx
OUT=$BASE/out
BK=/var/lib/kivali/buildkit
SOCK=unix:///run/buildkit/buildkitd.sock
NS=k8s.io

ctr() { k3s ctr -n "$NS" "$@"; }
buildctl() { "$BK/bin/buildctl" --addr "$SOCK" "$@"; }

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
  exec "$BK/bin/buildctl" --addr "$SOCK" build "$@"
  ;;
pull)
  for ref in "$@"; do
    ctr images pull --platform "linux/$(arch)" "$ref" >/dev/null
    echo "guest: pulled $ref"
  done
  ;;
digest)
  # `ctr images ls` prints REF TYPE DIGEST SIZE PLATFORMS LABELS.
  ctr images ls | awk -v r="$1" '$1 == r {print $3; exit}'
  ;;
export)
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
