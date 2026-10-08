#!/bin/sh
# Fetches what the VM image build needs from a k3s release for one
# architecture, except the k3s binary itself:
#   - the release's sha256sum-<arch>.txt and k3s-images.txt, and the
#     tag's scripts/version.sh (which pins k3s-root, named in the image's
#     SOURCES.md) (text) into <download-dir>;
#   - the five k3s images Kivali needs, pulled for linux/<arch> and
#     saved as one airgap tarball, <images-dir>/k3s-images.tar.
# The binary is downloaded and checked against that sha256 list inside
# the image build (Dockerfile, stage k3s), so no Linux executable is
# ever written here (README.md, "Built inside Docker"). Idempotent:
# anything already present and current is kept.
#
# The images are pulled and saved by <engine>: `docker` (this machine's
# Docker; `docker save` writes a docker-archive tar) or `vm` (the build
# VM's containerd through scripts/build-vm.sh, which must be up; `ctr
# images export` writes an OCI tar). k3s imports either.
#
# Image digests are pinned in a checked-in lock file. A ref missing
# from it is resolved once and appended (commit the file); a ref whose
# tag now resolves to a different digest fails the build. The digest is
# the one both engines record for a tag, which for these multi-arch tags
# is the image index's: the same for every architecture, so one lock
# serves both. The tarball holds only the requested platform;
# <images-dir> is per architecture.
#
# usage: fetch-k3s.sh <k3s-version> <arch> <download-dir> <images-dir> <lock-file> [docker|vm]
set -eu

version=$1
arch=$2
dl=$3
images=$4
lock=$5
engine=${6:-docker}

case "$arch" in
  amd64 | arm64) ;;
  *)
    echo "fetch: unsupported architecture '$arch' (amd64 or arm64)" >&2
    exit 1
    ;;
esac
platform="linux/$arch"
build_vm="$(dirname "$0")/../../scripts/build-vm.sh"

case "$engine" in
  docker | vm) ;;
  *)
    echo "fetch: unknown engine '$engine' (docker or vm)" >&2
    exit 1
    ;;
esac

# The images k3s needs with traefik, servicelb and metrics-server
# disabled and the Helm controller left on. Versions come from the
# release's own k3s-images.txt so they always match the binary.
needed='rancher/mirrored-pause rancher/mirrored-coredns-coredns rancher/local-path-provisioner rancher/mirrored-library-busybox rancher/klipper-helm'

base="https://github.com/k3s-io/k3s/releases/download/$(printf '%s' "$version" | sed 's/+/%2B/')"

mkdir -p "$dl" "$images"
touch "$lock"

for f in "sha256sum-$arch.txt" k3s-images.txt; do
  if [ ! -s "$dl/$f" ]; then
    echo "fetch: $base/$f"
    curl -fsSL -o "$dl/$f.part" "$base/$f"
    mv "$dl/$f.part" "$dl/$f"
  fi
done
if [ ! -s "$dl/version.sh" ]; then
  src="https://raw.githubusercontent.com/k3s-io/k3s/$(printf '%s' "$version" | sed 's/+/%2B/')/scripts/version.sh"
  echo "fetch: $src"
  curl -fsSL -o "$dl/version.sh.part" "$src"
  mv "$dl/version.sh.part" "$dl/version.sh"
fi

refs=''
for name in $needed; do
  ref=$(tr -d '\r' < "$dl/k3s-images.txt" | grep -E "^docker.io/$name:" | head -n 1)
  if [ -z "$ref" ]; then
    echo "fetch: $name is not in $version's k3s-images.txt" >&2
    exit 1
  fi
  refs="$refs $ref"
done

# The tarball is current when it was saved from exactly the locked
# digests of exactly these refs. (A Windows checkout gives the lock
# file CRLF line endings; they are ignored.)
locked() { awk -v r="$1" '{sub(/\r$/, "")} $1 == r {print $2}' "$lock"; }
tarball="$images/k3s-images.tar"
stamp="$images/.k3s-images.lock"
expect="$dl/expect-$arch.lock"
for ref in $refs; do echo "$ref $(locked "$ref")"; done > "$expect"
if [ -s "$tarball" ] && cmp -s "$expect" "$stamp" && ! grep -q ' $' "$expect"; then
  echo "fetch: $tarball ($platform) is current"
  exit 0
fi

pull() {
  case "$engine" in
    docker) docker pull -q --platform "$platform" "$1" >/dev/null ;;
    vm) "$build_vm" pull "$1" >/dev/null ;;
  esac
}
digest() {
  case "$engine" in
    docker) docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$1" | head -n 1 | sed 's/.*@//' ;;
    vm) "$build_vm" digest "$1" ;;
  esac
}

for ref in $refs; do
  echo "fetch: $engine pull $platform $ref"
  pull "$ref"
  digest=$(digest "$ref")
  if [ -z "$digest" ]; then
    echo "fetch: no digest for $ref" >&2
    exit 1
  fi
  pinned=$(locked "$ref")
  if [ -z "$pinned" ]; then
    echo "$ref $digest" >> "$lock"
    echo "fetch: pinned $ref at $digest in $lock (new; commit it)"
  elif [ "$pinned" != "$digest" ]; then
    echo "fetch: $ref now resolves to $digest but $lock pins $pinned; the tag moved" >&2
    exit 1
  else
    echo "fetch: $ref matches pinned $digest"
  fi
done

echo "fetch: $engine save $platform -> $tarball"
case "$engine" in
  docker)
    # shellcheck disable=SC2086
    docker save --platform "$platform" -o "$tarball.part" $refs
    mv "$tarball.part" "$tarball"
    ;;
  vm)
    # shellcheck disable=SC2086
    "$build_vm" export "$tarball" $refs
    ;;
esac
for ref in $refs; do echo "$ref $(locked "$ref")"; done > "$stamp"
