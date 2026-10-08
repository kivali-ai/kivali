#!/usr/bin/env bash
#
# latest-json.sh — write latest.json, the feed tauri-plugin-updater reads
# (docs/developers/desktop-app.md "Update states"; the app checks
# https://github.com/<repo>/releases/latest/download/latest.json).
#
#   scripts/latest-json.sh vX.Y.Z [--dir dist] [--notes TEXT] [--require]
#
# Reads the updater artifacts `tauri build` writes when
# createUpdaterArtifacts is on, each with its .sig beside it, from <dir>:
#
#   darwin-aarch64   Kivali.app.tar.gz
#   windows-x86_64   Kivali_X.Y.Z_x64-setup.exe (the NSIS installer)
#
# and writes <dir>/latest.json with an entry for each one present:
#
#   {"version": "X.Y.Z", "pub_date": "<UTC RFC 3339>", "notes": "...",
#    "platforms": {"darwin-aarch64": {"url": "<release asset URL>",
#                                     "signature": "<the .sig's contents>"},
#                  "windows-x86_64": {...}}}
#
# --require fails unless every platform is present (the release workflow
# passes it). The URLs point at the tag's own release assets, so a later
# release never changes what an older latest.json fetches. NOTES defaults
# to a line naming the release. GITHUB_REPOSITORY (set in Actions) names
# the repository, default kivali-ai/kivali.

set -euo pipefail

VERSION=""
dir="dist"
notes=""
require=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir="${2:?latest-json: --dir needs a directory}"; shift 2 ;;
    --notes) notes="${2:?latest-json: --notes needs text}"; shift 2 ;;
    --require) require=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d'; exit 0 ;;
    v*) VERSION="$1"; shift ;;
    *) echo "latest-json: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "latest-json: VERSION must be vMAJOR.MINOR.PATCH (got: ${VERSION:-<empty>})" >&2
  exit 1
fi
bare="${VERSION#v}"
command -v jq >/dev/null 2>&1 || { echo "latest-json: jq not found (brew install jq)" >&2; exit 1; }

repo="${GITHUB_REPOSITORY:-kivali-ai/kivali}"
: "${notes:=Kivali $bare}"

platforms='{}'
missing=""
for entry in "darwin-aarch64 Kivali.app.tar.gz" "windows-x86_64 Kivali_${bare}_x64-setup.exe"; do
  platform="${entry%% *}"
  file="${entry#* }"
  if [ ! -f "$dir/$file" ] || [ ! -f "$dir/$file.sig" ]; then
    missing="$missing $platform ($file and its .sig)"
    continue
  fi
  platforms="$(jq \
    --arg platform "$platform" \
    --arg url "https://github.com/$repo/releases/download/$VERSION/$file" \
    --rawfile signature "$dir/$file.sig" \
    '. + {($platform): {url: $url, signature: ($signature | rtrimstr("\n"))}}' <<<"$platforms")"
done

if [ "$platforms" = '{}' ] || { [ -n "$require" ] && [ -n "$missing" ]; }; then
  echo "latest-json: updater artifacts missing from $dir:$missing" >&2
  exit 1
fi

jq -n \
  --arg version "$bare" \
  --arg pub_date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg notes "$notes" \
  --argjson platforms "$platforms" \
  '{version: $version, pub_date: $pub_date, notes: $notes, platforms: $platforms}' > "$dir/latest.json"

echo "latest-json: wrote $dir/latest.json"
cat "$dir/latest.json"
