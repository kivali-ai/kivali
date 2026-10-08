#!/usr/bin/env bash
# Import a Kivali Design System drop from the designer's zip into design-system/.
#
#   scripts/ds-import.sh [--dry-run] "<path to Kivali Design System.zip>"
#
# Unzips to a temp dir (removed on exit), drops the designer's own uploads/ and thumbnail files,
# refuses a zip without README.md and tokens/tokens.json at its root, and rsyncs into
# design-system/ with --delete (so removed components disappear) while keeping our top-level
# PROVENANCE.md. --dry-run lists what rsync would change and touches nothing. Review the diff
# afterwards; nothing is committed here.
set -euo pipefail

usage() {
  echo "usage: $0 [--dry-run] <design-system.zip>" >&2
  exit 2
}

dry_run=0
zip=""
for arg in "$@"; do
  case "$arg" in
    --dry-run | -n) dry_run=1 ;;
    -h | --help) usage ;;
    -*) echo "unknown flag: $arg" >&2; usage ;;
    *)
      [ -z "$zip" ] || usage
      zip="$arg"
      ;;
  esac
done
[ -n "$zip" ] || usage

if [ ! -f "$zip" ]; then
  echo "no such file: $zip" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest="$repo_root/design-system"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

unzip -q "$zip" -d "$tmp"

# macOS zips carry a __MACOSX/ sibling; remove it before deciding whether there is a wrapper folder.
rm -rf "$tmp/__MACOSX"

# Some zips wrap everything in one top-level folder; descend into it.
src="$tmp"
shopt -s nullglob dotglob
entries=("$tmp"/*)
shopt -u nullglob dotglob
if [ "${#entries[@]}" -eq 1 ] && [ -d "${entries[0]}" ]; then
  src="${entries[0]}"
fi

for required in README.md tokens/tokens.json; do
  if [ ! -f "$src/$required" ]; then
    echo "refusing: $zip has no $required at its root; is this a Kivali Design System export?" >&2
    exit 1
  fi
done

rm -rf "$src/uploads" "$src/.thumbnail" "$src/thumbnail.html"

# PROVENANCE.md and components/text.css are Kivali's own files; the
# excludes keep --delete from removing them.
rsync_flags=(-a --delete --exclude '/PROVENANCE.md' --exclude '/components/text.css' --exclude '.DS_Store')
if [ "$dry_run" -eq 1 ]; then
  echo "dry run: changes rsync would make to $dest (nothing is written)"
  rsync "${rsync_flags[@]}" --dry-run --itemize-changes "$src"/ "$dest"/
  exit 0
fi

mkdir -p "$dest"
rsync "${rsync_flags[@]}" "$src"/ "$dest"/

git -C "$repo_root" status --short design-system

cat <<'EOF'

Imported. Next:
  1. Review the diff of design-system/.
  2. Run `make ds-sync` to copy kivali.css and tokens into web/src/ds/.
  3. Update the TSX ports in web/src/ds/ where the reference .jsx changed, then run `make web-lint web-test`.
EOF
