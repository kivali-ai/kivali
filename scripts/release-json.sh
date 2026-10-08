#!/usr/bin/env bash
#
# release-json.sh — write release.json, the feed the supervisor reads
# (internal/supervisor/release.go, docs/developers/supervisor.md "Release feed").
#
#   scripts/release-json.sh vX.Y.Z [--dir dist] [--manual-steps] [--relative]
#
# Reads the release assets from --dir (default dist/):
#
#   kivali-X.Y.Z.tgz                      required (make chart-package)
#   kivali-images-linux-arm64.tar.zst     required (make release-assets)
#   kivali-images-linux-amd64.tar.zst     listed when present
#
# and writes <dir>/release.json with their sha256 digests and sizes. Each
# asset has its name and an absolute url pinned to the tag
# (https://github.com/<repo>/releases/download/vX.Y.Z/<name>).
# internal/supervisor/release.go (assetRef) uses an asset's url when it has
# one; without one it resolves the name against release.json's own
# location, which for a feed fetched through .../releases/latest/download/
# follows "latest" and so can 404 or fail the digest if a newer release is
# published mid-download. --relative omits the urls (a feed in a directory
# beside its assets, for local trials).
#
# Fields:
#   version              X.Y.Z (the tag without the v)
#   min_desktop_version  MIN_DESKTOP_VERSION, default X.Y.Z. The desktop and
#                        the org are cut together, so by default a release
#                        asks for the desktop of the same number. Lower it
#                        by hand only for a release the older app can run.
#   vm_image             VM_IMAGE_VERSION, default X.Y.Z: the VM image the
#                        release was built with (make -C vm image VM_VERSION=...)
#   manual_steps         true with --manual-steps, MANUAL_STEPS=1, or when the
#                        tag is listed in scripts/manual-steps
#   notes_url            NOTES_URL, default the GitHub release page of the tag
#
# The chart inside the .tgz must say version X.Y.Z (the supervisor refuses
# a release whose chart disagrees), so that is checked here too.
#
# Needs jq and shasum (or sha256sum). Runs identically in CI and locally.

set -euo pipefail

VERSION=""
dir="dist"
manual="${MANUAL_STEPS:-}"
relative=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir="${2:?release-json: --dir needs a directory}"; shift 2 ;;
    --manual-steps) manual=1; shift ;;
    --relative) relative=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d'; exit 0 ;;
    v*) VERSION="$1"; shift ;;
    *) echo "release-json: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release-json: VERSION must be vMAJOR.MINOR.PATCH (got: ${VERSION:-<empty>})" >&2
  exit 1
fi
bare="${VERSION#v}"
here="$(cd "$(dirname "$0")" && pwd)"

command -v jq >/dev/null 2>&1 || { echo "release-json: jq not found (brew install jq)" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
else
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi
size() { echo $(( $(wc -c < "$1") )); }

chart="kivali-$bare.tgz"
img_arm64="kivali-images-linux-arm64.tar.zst"
img_amd64="kivali-images-linux-amd64.tar.zst"
for f in "$chart" "$img_arm64"; do
  if [ ! -f "$dir/$f" ]; then
    echo "release-json: $dir/$f not found" >&2
    echo "  make chart-package VERSION=$VERSION; make release-assets VERSION=$VERSION" >&2
    exit 1
  fi
done

chart_version="$(tar -xzOf "$dir/$chart" kivali/Chart.yaml | perl -ne 'print "$1\n" if /^version:\s*(\S+)/' | head -1)"
if [ "$chart_version" != "$bare" ]; then
  echo "release-json: $chart's Chart.yaml says version ${chart_version:-<none>}, not $bare" >&2
  echo "  package it with make chart-package VERSION=$VERSION" >&2
  exit 1
fi

if [ -z "$manual" ] && [ -f "$here/manual-steps" ] && grep -qxF "$VERSION" <(grep -v '^[[:space:]]*#' "$here/manual-steps"); then
  manual=1
fi
case "$manual" in
  1|true|yes) manual=true ;;
  *) manual=false ;;
esac

repo="${GITHUB_REPOSITORY:-kivali-ai/kivali}"
notes="${NOTES_URL:-https://github.com/$repo/releases/tag/$VERSION}"
# Each asset carries an absolute url pinned to THIS tag, so an install that
# is downloading while a newer release is published still fetches this
# release's files (a name resolved against .../releases/latest/download/
# would follow "latest" and 404 or fail the digest). --relative omits the
# urls, for a feed that lives in a directory next to its assets.
base="https://github.com/$repo/releases/download/$VERSION/"
if [ -n "$relative" ]; then base=""; fi

amd64_name="" amd64_sha="" amd64_size=0
if [ -f "$dir/$img_amd64" ]; then
  amd64_name="$img_amd64"; amd64_sha="$(sha "$dir/$img_amd64")"; amd64_size="$(size "$dir/$img_amd64")"
else
  echo "release-json: $dir/$img_amd64 not found; release.json lists linux/arm64 only (the desktop's guest is arm64)" >&2
fi

jq -n \
  --arg version "$bare" \
  --arg chart_name "$chart" --arg chart_sha "$(sha "$dir/$chart")" --argjson chart_size "$(size "$dir/$chart")" \
  --arg arm_name "$img_arm64" --arg arm_sha "$(sha "$dir/$img_arm64")" --argjson arm_size "$(size "$dir/$img_arm64")" \
  --arg amd_name "$amd64_name" --arg amd_sha "$amd64_sha" --argjson amd_size "$amd64_size" \
  --arg min "${MIN_DESKTOP_VERSION:-$bare}" \
  --arg vm "${VM_IMAGE_VERSION:-$bare}" \
  --argjson manual "$manual" \
  --arg notes "$notes" \
  --arg base "$base" \
  'def pin($n): if $base == "" then {} else {url: ($base + $n)} end;
  {
    version: $version,
    chart: ({name: $chart_name, sha256: $chart_sha, size: $chart_size} + pin($chart_name)),
    images: ({"linux/arm64": ({name: $arm_name, sha256: $arm_sha, size: $arm_size} + pin($arm_name))}
             + (if $amd_name == "" then {} else {"linux/amd64": ({name: $amd_name, sha256: $amd_sha, size: $amd_size} + pin($amd_name))} end)),
    min_desktop_version: $min,
    vm_image: $vm,
    manual_steps: $manual,
    notes_url: $notes
  }' > "$dir/release.json"

echo "release-json: wrote $dir/release.json"
cat "$dir/release.json"
