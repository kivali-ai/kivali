#!/bin/sh
#
# manual-steps.sh — whether a release's upgrade needs steps the app cannot
# do. Prints true or false for the tag given (vX.Y.Z): true when
# MANUAL_STEPS is 1, true or yes, else when the tag is listed in
# scripts/manual-steps (one tag per line, # comments). release-json.sh
# writes the answer into release.json, and `make vm-release` bakes it
# into the VM image (/usr/share/kivali/manual-steps), so the desktop
# applies neither the feed's release nor the image's own on its own.

set -eu
here="$(cd "$(dirname "$0")" && pwd)"
tag="${1:?usage: manual-steps.sh vX.Y.Z}"
case "${MANUAL_STEPS:-}" in
  1|true|yes) echo true; exit 0 ;;
esac
if [ -f "$here/manual-steps" ] && grep -v '^[[:space:]]*#' "$here/manual-steps" | grep -qxF "$tag"; then
  echo true
else
  echo false
fi
