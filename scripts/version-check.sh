#!/usr/bin/env bash
#
# version-check.sh — every file that carries the release version agrees.
#
#   scripts/version-check.sh [vX.Y.Z]
#
# One release is one number. The files below carry it, and a release is
# only coherent when they all say the same thing:
#
#   charts/kivali/Chart.yaml                    appVersion: vX.Y.Z
#   desktop-app/src-tauri/tauri.conf.json       "version": "X.Y.Z"  (the desktop's source)
#   desktop-app/src-tauri/Cargo.toml            version = "X.Y.Z"
#   desktop-app/src-tauri/Cargo.lock            the kivali-desktop package
#   desktop-app/package.json                    "version": "X.Y.Z"
#   desktop-app/package-lock.json               the root and packages[""] entries
#
# With an argument (the release tag), all of them must equal it, the
# desktop ones without the leading v. Without one they must agree with
# each other (the chart's appVersion is the reference). Run by
# `make version-check`, CI, scripts/release.sh (before the bump) and
# scripts/bump-version.sh (after it) and the release workflow (with the pushed tag).
#
# ROOT overrides the repository root (scripts/bump-version.sh passes it
# through, so both can be pointed at a copy of the six files).

set -euo pipefail

root="${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
want="${1:-}"
if [ -n "$want" ] && ! [[ "$want" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "version-check: VERSION must be vMAJOR.MINOR.PATCH (got: $want)" >&2
  exit 1
fi

chart="$root/charts/kivali/Chart.yaml"
tauri="$root/desktop-app/src-tauri/tauri.conf.json"
cargo="$root/desktop-app/src-tauri/Cargo.toml"
clock="$root/desktop-app/src-tauri/Cargo.lock"
pkg="$root/desktop-app/package.json"
plock="$root/desktop-app/package-lock.json"

for f in "$chart" "$tauri" "$cargo" "$clock" "$pkg" "$plock"; do
  if [ ! -f "$f" ]; then
    echo "version-check: $f not found" >&2
    exit 1
  fi
done

# first VALUE of PATTERN's capture group in FILE ("" when absent).
first() { perl -ne 'if (/'"$1"'/) { print "$1\n"; exit }' "$2"; }

fail=0
declare -a rows=()
# check LABEL ACTUAL EXPECTED
check() {
  local label="$1" got="$2" exp="$3" mark="ok"
  if [ -z "$got" ]; then
    got="(none found)"; mark="MISSING"; fail=1
  elif [ "$got" != "$exp" ]; then
    mark="MISMATCH (want $exp)"; fail=1
  fi
  rows+=("$(printf '  %-52s %-12s %s' "$label" "$got" "$mark")")
}

chart_app="$(first '^appVersion: (v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)' "$chart")"
ref="${want:-$chart_app}"
if [ -z "$ref" ]; then
  echo "version-check: $chart has no 'appVersion: vX.Y.Z' line" >&2
  exit 1
fi
bare="${ref#v}"

check "charts/kivali/Chart.yaml appVersion" "$chart_app" "$ref"

check "desktop-app/src-tauri/tauri.conf.json version" "$(first '^  "version": "([^"]+)"' "$tauri")" "$bare"
check "desktop-app/src-tauri/Cargo.toml version" "$(first '^version = "([^"]+)"' "$cargo")" "$bare"
check "desktop-app/src-tauri/Cargo.lock kivali-desktop" "$(perl -0ne 'print "$1\n" if /name = "kivali-desktop"\nversion = "([^"]+)"/' "$clock")" "$bare"
check "desktop-app/package.json version" "$(first '^  "version": "([^"]+)"' "$pkg")" "$bare"
check "desktop-app/package-lock.json version" "$(first '^  "version": "([^"]+)"' "$plock")" "$bare"
check "desktop-app/package-lock.json packages[\"\"] version" "$(perl -0ne 'print "$1\n" if /"packages": \{\n    "": \{\n      "name": "kivali-desktop",\n      "version": "([^"]+)"/' "$plock")" "$bare"

printf '%s\n' "${rows[@]}"
if [ "$fail" -ne 0 ]; then
  echo "version-check: the release version is not one number (reference: $ref)" >&2
  echo "  scripts/release.sh bumps every one of these together; a hand edit of one is the usual cause" >&2
  exit 1
fi
echo "version-check: all agree on $ref"
