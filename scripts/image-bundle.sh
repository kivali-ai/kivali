#!/usr/bin/env bash
#
# image-bundle.sh — turn the three per-image tarballs `make image-tars`
# writes into the release's image bundle for that architecture,
# uncompressed.
#
#   scripts/image-bundle.sh vX.Y.Z ARCH [--images-dir dist/images]
#
# The supervisor imports ONE archive per platform
# (internal/supervisor/release.go, guestapi/images.go): a `docker save` of
# kivali, kivali-egress-proxy and kivali-agent-shell at the release tag, in
# one tar, zstd-compressed. This loads the three tarballs into the local
# Docker daemon, saves them together, and writes:
#
#   <images-dir>/bundle-<arch>/kivali-images.tar  a directory holding only
#                                                 this file, ready to be the
#                                                 VM build's KIVALI_IMAGES_DIR
#                                                 (squashfs compresses it)
#
# It then checks the bundle the way the supervisor will: all three
# references are in it, at the tag. `make release-compress-<arch>`
# compresses it into the release asset,
# dist/kivali-images-linux-<arch>.tar.zst.
#
# Needs docker (28 or later, for docker save --platform) and jq. REGISTRY
# (default empty) prefixes the image names, as in the Makefile.

set -euo pipefail

VERSION="${1:-}"
ARCH="${2:-}"
shift 2 || true
images_dir="dist/images"
while [ $# -gt 0 ]; do
  case "$1" in
    --images-dir) images_dir="${2:?image-bundle: --images-dir needs a directory}"; shift 2 ;;
    *) echo "image-bundle: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "image-bundle: VERSION must be vMAJOR.MINOR.PATCH (got: ${VERSION:-<empty>})" >&2
  exit 1
fi
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "image-bundle: ARCH must be amd64 or arm64 (got: ${ARCH:-<empty>})" >&2; exit 1 ;;
esac
command -v docker >/dev/null 2>&1 || { echo "image-bundle: docker not found" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "image-bundle: jq not found" >&2; exit 1; }

reg="${REGISTRY:-}"
names=(kivali kivali-egress-proxy kivali-agent-shell)
refs=()
for n in "${names[@]}"; do
  tar="$images_dir/$n-$ARCH.tar"
  if [ ! -f "$tar" ]; then
    echo "image-bundle: $tar not found (make image-tars ARCH=$ARCH VERSION=$VERSION)" >&2
    exit 1
  fi
  echo "==> docker load $tar"
  docker load -i "$tar" >/dev/null
  refs+=("$reg$n:$VERSION")
done

bundle_dir="$images_dir/bundle-$ARCH"
mkdir -p "$bundle_dir"
plain="$bundle_dir/kivali-images.tar"
echo "==> docker save --platform linux/$ARCH ${refs[*]}"
# Always the explicit platform, as vm/scripts/fetch-k3s.sh and the vm job
# do on the same runners. Both the containerd image store and the classic
# store accept a --platform that matches the loaded images.
docker save --platform "linux/$ARCH" -o "$plain" "${refs[@]}"

# The supervisor's own check: every reference present, at the tag.
listed="$(tar -xOf "$plain" manifest.json | jq -r '.[].RepoTags[]' | sort)"
for r in "${refs[@]}"; do
  if ! grep -qxF "$r" <<<"$listed"; then
    echo "image-bundle: $plain does not list $r; it lists:" >&2
    echo "$listed" >&2
    exit 1
  fi
done
echo "image-bundle: $plain holds:"
while IFS= read -r line; do echo "  $line"; done <<<"$listed"
ls -l "$plain"
