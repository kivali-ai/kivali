#!/usr/bin/env bash
#
# notary-progress.sh — print Apple's notarization progress once a minute
# while something else (tauri bundle) submits and waits in silence.
#
#   scripts/notary-progress.sh [interval-seconds]
#
# Runs until killed. Each line gives the minutes since it started and the
# submissions made since then, as `xcrun notarytool history` reports them
# (name, id, status, created). It reads the credentials tauri reads:
# APPLE_ID + APPLE_PASSWORD + APPLE_TEAM_ID, or APPLE_API_KEY +
# APPLE_API_ISSUER + APPLE_API_KEY_PATH. It prints no credential, and it
# never fails: a history it cannot read is reported and asked for again
# next time.
#
# Needs xcrun (Xcode's command line tools) and jq.

set -uo pipefail

interval="${1:-60}"

if [ -n "${APPLE_ID:-}" ]; then
  auth=(--apple-id "$APPLE_ID" --password "${APPLE_PASSWORD:-}" --team-id "${APPLE_TEAM_ID:-}")
elif [ -n "${APPLE_API_KEY:-}" ]; then
  auth=(--key "${APPLE_API_KEY_PATH:-}" --key-id "$APPLE_API_KEY" --issuer "${APPLE_API_ISSUER:-}")
else
  echo "notary-progress: no notarization credentials; nothing to watch"
  exit 0
fi

# notarytool's createdDate is ISO 8601 in UTC, so it compares as a string.
since="$(date -u +%Y-%m-%dT%H:%M:%S)"
started=$SECONDS

while sleep "$interval"; do
  minutes=$(( (SECONDS - started) / 60 ))
  if ! history="$(xcrun notarytool history "${auth[@]}" --output-format json 2>/dev/null)"; then
    echo "notary-progress: ${minutes} min; Apple's submission history is unavailable"
    continue
  fi
  lines="$(jq -r --arg since "$since" \
    '.history[] | select(.createdDate >= $since) | "\(.name) \(.id): \(.status) (submitted \(.createdDate))"' \
    <<<"$history" 2>/dev/null)"
  if [ -z "$lines" ]; then
    echo "notary-progress: ${minutes} min; nothing submitted yet (signing, or uploading)"
  else
    while IFS= read -r line; do echo "notary-progress: ${minutes} min; $line"; done <<<"$lines"
  fi
done
