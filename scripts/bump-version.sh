#!/usr/bin/env bash
#
# bump-version.sh — set every file that carries the release version to
# vX.Y.Z. Called by scripts/release.sh; ROOT points it at a copy of the
# files for a dry run.
#
#   scripts/bump-version.sh vX.Y.Z
#
# Edits (perl, not sed -i: macOS and GNU sed disagree on -i):
#   charts/kivali/Chart.yaml                 appVersion
#   desktop-app/src-tauri/tauri.conf.json    version  (X.Y.Z, no v)
#   desktop-app/src-tauri/Cargo.toml         version
#   desktop-app/src-tauri/Cargo.lock         the kivali-desktop package
#   desktop-app/package.json                 version
#   desktop-app/package-lock.json            the two root version fields
# and then runs scripts/version-check.sh against the result, so a regex
# that missed one leaves a loud failure instead of a half-bumped tree.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="${ROOT:-$(cd "$here/.." && pwd)}"
VERSION="${1:-}"
if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "bump-version: VERSION must be vMAJOR.MINOR.PATCH (got: ${VERSION:-<empty>})" >&2
  exit 1
fi
bare="${VERSION#v}"
export V="$VERSION" B="$bare"

perl -pi -e 's|^appVersion: v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?|appVersion: $ENV{V}|' "$root/charts/kivali/Chart.yaml"
perl -pi -e 's|^(  "version": ")[^"]+|$1$ENV{B}|' "$root/desktop-app/src-tauri/tauri.conf.json" "$root/desktop-app/package.json"
perl -pi -e 'if (!$d && s|^(version = ")[^"]+|$1$ENV{B}|) { $d = 1 }' "$root/desktop-app/src-tauri/Cargo.toml"
perl -0pi -e 's|(name = "kivali-desktop"\nversion = ")[^"]+|$1$ENV{B}|' "$root/desktop-app/src-tauri/Cargo.lock"
perl -pi -e 'if (!$a && s|^(  "version": ")[^"]+|$1$ENV{B}|) { $a = 1 } elsif (!$b && s|^(      "version": ")[^"]+|$1$ENV{B}|) { $b = 1 }' "$root/desktop-app/package-lock.json"

ROOT="$root" bash "$here/version-check.sh" "$VERSION"
