#!/usr/bin/env bash
#
# hdiutil-retry.sh — hdiutil, trying `create` and `detach` again when they
# fail with "Resource busy", for the dmg script tauri runs.
#
#   scripts/hdiutil-retry.sh <hdiutil arguments>
#
# tauri's dmg bundler runs create-dmg 1.2.1's script, which calls hdiutil
# by name. That script gives up on the first busy `hdiutil create` (exit
# 1), and on a busy `hdiutil detach` after three tries, so one busy volume
# fails a release build. desktop-app/Makefile puts this script first on
# PATH, named hdiutil, for every `tauri bundle` on macOS. It tries create
# and detach up to five times, waiting 2, 4, 8 and 16 seconds between,
# when their output says "Resource busy"; any other failure is returned at
# once. That is the retry create-dmg 1.3.0 has (its hdiutil_retry). Every
# other subcommand runs unchanged, its output untouched.
#
# A busy create can leave its half-made image attached, mounted under the
# dmg's volume name, and the file on disk. Before trying again this
# detaches every image attached from the output path (forcing it when a
# plain detach fails: the image is the failed attempt's own) and removes
# the file if the failed attempt made it. The next attempt then starts
# clean, and the script's Finder step sees one volume of that name.
#
# HDIUTIL_RETRY_LOG names a file each call is appended to: its arguments,
# its exit status and, for create and detach, its output. tauri discards
# the dmg script's output, so this is where a failed dmg step's hdiutil
# calls can be read; desktop-app/Makefile prints it when the dmg step
# fails or had to try again. HDIUTIL_REAL (default /usr/bin/hdiutil) and
# HDIUTIL_RETRY_SLEEP (default sleep) let the tests stand in for both.
#
# Written for bash 3.2, macOS's /bin/bash.

set -uo pipefail

real="${HDIUTIL_REAL:-/usr/bin/hdiutil}"
pause="${HDIUTIL_RETRY_SLEEP:-sleep}"
log="${HDIUTIL_RETRY_LOG:-}"
attempts=5

note() {
  if [ -n "$log" ]; then printf '%s\n' "$*" >>"$log"; fi
}

case "${1:-}" in
  create | detach) ;;
  *)
    "$real" "$@"
    status=$?
    note "hdiutil $*: exit $status"
    exit "$status"
    ;;
esac
sub="$1"

# -quiet hides the "Resource busy" this looks for, so it is dropped here
# and the output is shown only when -quiet was not asked for.
args=()
quiet=0
for arg in "$@"; do
  if [ "$arg" = -quiet ]; then quiet=1; else args+=("$arg"); fi
done

# create's last argument is the image it writes; hdiutil adds .dmg when
# the name has no extension.
image=""
existed=0
if [ "$sub" = create ]; then
  image="${args[${#args[@]}-1]}"
  case "$image" in *.*) ;; *) image="$image.dmg" ;; esac
  if [ -e "$image" ]; then existed=1; fi
fi

# What a failed create left: images attached from the image's path,
# found by their whole-disk device (the first /dev line under each
# image-path in `hdiutil info`), and the file.
clear_failed_create() {
  local dir path dev
  dir="$(cd "$(dirname "$image")" 2>/dev/null && pwd -P)" || dir=""
  path="$dir/$(basename "$image")"
  for dev in $("$real" info 2>/dev/null | awk -v want="$path" '
    /^image-path[ \t]*:/ { sub(/^image-path[ \t]*:[ \t]*/, ""); match_ = ($0 == want); next }
    match_ && /^\/dev\// { print $1; match_ = 0 }'); do
    if "$real" detach "$dev" >/dev/null 2>&1; then
      note "  detached $dev, left attached from $path"
    elif "$real" detach -force "$dev" >/dev/null 2>&1; then
      note "  detached $dev with -force, left attached from $path"
    else
      note "  could not detach $dev, left attached from $path"
    fi
  done
  if [ "$existed" = 0 ] && [ -e "$image" ]; then
    rm -f "$image"
    note "  removed $image"
  fi
}

# The output goes to files, not a pipe: create-dmg notes that piping
# hdiutil's output can leave diskimages-helper waiting forever.
tmp="$(mktemp -d "${TMPDIR:-/tmp}/hdiutil-retry.XXXXXX")" || exit 1
trap 'rm -rf "$tmp"' EXIT

attempt=0
while :; do
  attempt=$((attempt + 1))
  "$real" "${args[@]}" >"$tmp/out" 2>"$tmp/err"
  status=$?
  if [ "$quiet" = 0 ]; then
    cat "$tmp/out"
    cat "$tmp/err" >&2
  fi
  note "hdiutil ${args[*]}: exit $status (attempt $attempt of $attempts)"
  if [ -n "$log" ]; then sed 's/^/  /' "$tmp/out" "$tmp/err" >>"$log"; fi
  if [ "$status" = 0 ]; then exit 0; fi
  if ! grep -qw 'Resource busy' "$tmp/out" "$tmp/err"; then exit "$status"; fi
  if [ "$attempt" -ge "$attempts" ]; then exit "$status"; fi
  if [ "$sub" = create ]; then clear_failed_create; fi
  wait=$((2 ** attempt))
  echo "hdiutil-retry: $sub: Resource busy; trying again in ${wait}s" >&2
  note "  Resource busy; trying again in ${wait}s"
  "$pause" "$wait"
done
