#!/usr/bin/env bash
#
# check-updater-key.sh — refuse a release while tauri.conf.json still
# carries the DEVELOPMENT updater public key.
#
#   scripts/check-updater-key.sh
#
# An app built with the development public key rejects every update signed
# with the owner's key, so a release cut that way can never update the
# apps that installed it. Run by scripts/release.sh before it pushes and
# by the release workflow's verify job. ROOT overrides the repository root.

set -euo pipefail

root="${ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
conf="$root/desktop-app/src-tauri/tauri.conf.json"
dev='dW50cnVzdGVkIGNvbW1lbnQ6IG1pbmlzaWduIHB1YmxpYyBrZXk6IEMxMEYxMzQ1QTU5ODQ0Q0EKUldUS1JKaWxSUk1Qd1NUU2RkbjA4bTZJYVhLaW55N1lCajZub05EN3hKaTB3WUxreFVDZ1I3Y2MK'

if grep -qF "$dev" "$conf"; then
  echo "::error title=Development updater key::desktop-app/src-tauri/tauri.conf.json still carries the development updater public key; put the owner's public key (the .pub of TAURI_SIGNING_PRIVATE_KEY) in plugins.updater.pubkey before the first release." >&2
  exit 1
fi
