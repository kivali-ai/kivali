#!/usr/bin/env bash
#
# release-notes.sh — write the top of a GitHub release's notes: the two
# Kivali Desktop installers, linked and named for their platform, ahead of
# everything else the release carries.
#
#   scripts/release-notes.sh vX.Y.Z [--dir assets] [--dev REF SHA]
#
# Prints Markdown on stdout. The links point at the tag's own release
# assets, Kivali_X.Y.Z_aarch64.dmg and Kivali_X.Y.Z_x64-setup.exe, and the
# documentation links at the tag's tree. With --dir, it fails unless both
# installers are in that directory, so a release never links a file it
# does not have. --dev adds the dev build's caveat, naming the ref and
# commit built. The release workflow passes the result to `gh release
# create`, which appends GitHub's generated notes for a release.
# GITHUB_REPOSITORY (set in Actions) names the repository, default
# kivali-ai/kivali.

set -euo pipefail

VERSION=""
dir=""
dev_ref=""
dev_sha=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir="${2:?release-notes: --dir needs a directory}"; shift 2 ;;
    --dev)
      dev_ref="${2:?release-notes: --dev needs a ref and a commit}"
      dev_sha="${3:?release-notes: --dev needs a ref and a commit}"
      shift 3 ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d'; exit 0 ;;
    v*) VERSION="$1"; shift ;;
    *) echo "release-notes: unknown argument: $1" >&2; exit 1 ;;
  esac
done

if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release-notes: VERSION must be vMAJOR.MINOR.PATCH (got: ${VERSION:-<empty>})" >&2
  exit 1
fi
bare="${VERSION#v}"
repo="${GITHUB_REPOSITORY:-kivali-ai/kivali}"
dmg="Kivali_${bare}_aarch64.dmg"
exe="Kivali_${bare}_x64-setup.exe"

if [ -n "$dir" ]; then
  missing=""
  for f in "$dmg" "$exe"; do
    [ -f "$dir/$f" ] || missing="$missing $f"
  done
  if [ -n "$missing" ]; then
    echo "release-notes: not in $dir:$missing" >&2
    exit 1
  fi
fi

download="https://github.com/$repo/releases/download/$VERSION"
docs="https://github.com/$repo/blob/$VERSION/docs"

if [ -n "$dev_ref" ]; then
  cat <<EOF
> [!WARNING]
> A dev build of \`$dev_ref\` at $dev_sha, for testing. Not signed: the macOS app is not notarized, so macOS blocks its first launch until you allow it in **System Settings**, **Privacy & Security**; Windows SmartScreen warns before the installer runs. It does not update itself.

EOF
fi

cat <<EOF
## Download Kivali Desktop

| Your computer | Download |
| --- | --- |
| **Mac** with Apple silicon, macOS 13 or later | [**$dmg**]($download/$dmg) |
| **Windows** 10 or 11 Pro, Enterprise or Education, 64-bit, with Hyper-V | [**$exe**]($download/$exe) |

On a Mac, open the \`.dmg\` and drag **Kivali** into **Applications**. On Windows, turn on Hyper-V first (**Turn Windows features on or off**, **Hyper-V**, restart), then run the installer; it asks for administrator rights once. [Getting started]($docs/getting-started.md) takes you from there to a working team.

The other files on this page are for running Kivali on your own server ([Self-hosting]($docs/self-hosting.md)), for the app's automatic updates, and for license and source compliance. You do not need them to install the app.
EOF
