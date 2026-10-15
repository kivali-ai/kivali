#!/usr/bin/env bash
#
# release.sh — cut a Kivali release through a pull request.
#
#   scripts/release.sh vX.Y.Z [--no-wait]
#
# main takes changes only through pull requests, so a release is two
# halves, and this script does whichever it finds still to do:
#
#   1. Open. On a branch release/vX.Y.Z made from origin/main, bump every
#      file that carries the release version together
#      (scripts/bump-version.sh; scripts/version-check.sh lists them):
#      charts/kivali/Chart.yaml's appVersion (the image tag the chart
#      installs by default) and the desktop app's version in
#      desktop-app/src-tauri/tauri.conf.json, Cargo.toml, Cargo.lock,
#      desktop-app/package.json and package-lock.json. Commit, push the
#      branch and open a pull request to main.
#   2. Tag. Once that pull request is merged, tag the commit it became on
#      main with an annotated vX.Y.Z and push the tag, which starts
#      .github/workflows/release.yml. A rebase merge gives the commit a
#      new hash, so the tag goes on the commit GitHub reports as the
#      merge, never on the local one, and never on whatever main has
#      moved on to since.
#
# Between the two it waits for the merge, asking GitHub every
# RELEASE_POLL_SECONDS (default 30). --no-wait stops once the pull
# request is open. Stopping at any point is safe: run it again with the
# same version and it carries on (an open pull request is waited for, a
# merged one tagged). A pull request closed without merging stops it.
#
# The work happens in temporary worktrees of origin's commits, so the
# checkout it runs in can be on any branch, with any changes, and is
# left as it was.
#
# Guards, before opening:
#   1. VERSION is semver (v-prefixed)
#   2. the tag does not exist locally or on origin
#   3. charts/kivali/Chart.yaml has the appVersion line the bump expects
#   4. every versioned file agrees with the others before the bump
#      (scripts/version-check.sh): a hand edit of one stops the release
#   5. tauri.conf.json's updater public key is not the development key
#      (scripts/check-updater-key.sh)
#   6. the bump changes something (VERSION is not already main's)
#   7. a patch release (Z above 0) changes nothing on the desktop side
#      since vX.Y.0: desktop-app/, vm/, internal/supervisor/,
#      cmd/kivali-supervisor/ and cmd/kivali-guest/, compared with both
#      trees bumped to VERSION, so the version numbers alone pass. A
#      desktop of X.Y.0 runs every X.Y release
#      (docs/developers/releasing.md, "Versions and compatibility"); a
#      change there is a new minor
# and before tagging:
#   8. the merge commit is on origin/main
#   9. at that commit every versioned file says VERSION and the updater
#      key is the owner's
#
# Needs git and an authenticated gh (gh auth status). GH overrides the gh
# command.
#
# Exit codes:
#   0 — the tag is pushed (or, with --no-wait, the pull request is open)
#   1 — a guard failed, or the pull request was closed without merging

set -euo pipefail

VERSION=""
wait=1
for arg in "$@"; do
  case "$arg" in
    --no-wait) wait="" ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d'; exit 0 ;;
    *) VERSION="$arg" ;;
  esac
done
GH="${GH:-gh}"
POLL="${RELEASE_POLL_SECONDS:-30}"

if [ -z "$VERSION" ]; then
  echo "release: VERSION is required (e.g. make release VERSION=v1.2.0)" >&2
  exit 1
fi

# Guard 1: semver shape.
if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release: VERSION must be vMAJOR.MINOR.PATCH (got: $VERSION)" >&2
  exit 1
fi

branch="release/$VERSION"
top="$(git rev-parse --show-toplevel)"
cd "$top"

if ! "$GH" auth status >/dev/null 2>&1; then
  echo "release: gh is not signed in (gh auth login)" >&2
  exit 1
fi

# Fetch with --tags so the tag check below covers origin's tags too.
echo "==> fetching origin"
git fetch origin --tags --quiet

# Guard 2: the tag is unused.
if git rev-parse --verify --quiet "refs/tags/$VERSION" >/dev/null; then
  echo "release: tag $VERSION already exists" >&2
  echo "  if it is a mistake, delete it locally and on origin:" >&2
  echo "    git tag -d $VERSION && git push origin :refs/tags/$VERSION" >&2
  exit 1
fi

work=""
since_work=""
cleanup() {
  if [ -n "$work" ]; then
    git worktree remove --force "$work" >/dev/null 2>&1 || true
    work=""
  fi
  if [ -n "$since_work" ]; then
    git worktree remove --force "$since_work" >/dev/null 2>&1 || true
    since_work=""
  fi
}
trap cleanup EXIT

# with_commit COMMIT: a temporary worktree of COMMIT at $work.
with_commit() {
  cleanup
  work="$(mktemp -d "${TMPDIR:-/tmp}/kivali-release.XXXXXX")"
  rmdir "$work"
  git worktree add --quiet --detach "$work" "$1"
}

# What a desktop runs a team with: the app, the VM image, the supervisor
# and the guest agent. Guard 7 holds a patch release to these as they
# were at vX.Y.0.
desktop_side=(desktop-app vm internal/supervisor cmd/kivali-supervisor cmd/kivali-guest)

# check_desktop_side: guard 7, on the bumped tree at $work.
check_desktop_side() {
  local major minor patch since tree changed n
  IFS=. read -r major minor patch <<<"${VERSION#v}"
  patch="${patch%%-*}"
  if [ "$patch" = 0 ]; then
    return 0
  fi
  since="v$major.$minor.0"
  if ! git rev-parse --verify --quiet "refs/tags/$since^{commit}" >/dev/null; then
    echo "release: $VERSION is a patch release, but there is no $since to patch" >&2
    exit 1
  fi
  echo "==> checking the desktop side is as $since had it"
  since_work="$(mktemp -d "${TMPDIR:-/tmp}/kivali-release-since.XXXXXX")"
  rmdir "$since_work"
  git worktree add --quiet --detach "$since_work" "$since"
  ROOT="$since_work" bash "$top/scripts/bump-version.sh" "$VERSION" >/dev/null
  git -C "$since_work" add -A
  tree="$(git -C "$since_work" write-tree)"
  changed="$(git -C "$work" diff --name-only "$tree" -- "${desktop_side[@]}")"
  if [ -n "$changed" ]; then
    n="$(wc -l <<<"$changed" | tr -d ' ')"
    echo "release: $VERSION is a patch release, but the desktop side has changed since $since:" >&2
    head -20 <<<"$changed" | sed 's/^/    /' >&2
    if [ "$n" -gt 20 ]; then
      echo "    … and $((n - 20)) more" >&2
    fi
    echo "  a desktop of $major.$minor.0 must run every $major.$minor release; release this as v$major.$((minor + 1)).0" >&2
    echo "  (docs/developers/releasing.md, \"Versions and compatibility\")" >&2
    exit 1
  fi
}

# The release's pull request: the newest one from its branch that is
# merged, else open, else closed. Prints "NUMBER STATE", or nothing.
find_pr() {
  "$GH" pr list --head "$branch" --base main --state all --limit 20 \
    --json number,state \
    --jq 'sort_by(if .state == "MERGED" then 0 elif .state == "OPEN" then 1 else 2 end) | .[0] | select(. != null) | "\(.number) \(.state)"'
}

open_pr() {
  with_commit origin/main
  local chart="$work/charts/kivali/Chart.yaml"

  # Guard 3: chart shape.
  if ! grep -qE '^appVersion: v[0-9]+\.[0-9]+\.[0-9]+' "$chart"; then
    echo "release: charts/kivali/Chart.yaml has no 'appVersion: vX.Y.Z' line; its shape may have changed — adjust release.sh" >&2
    exit 1
  fi
  # Guard 4: the versioned files agree before the bump.
  echo "==> checking main's versions agree"
  ROOT="$work" bash "$top/scripts/version-check.sh"
  # Guard 5: the owner's updater key. Checked before anything is pushed:
  # the release workflow refuses the same thing, but only once the tag
  # (and its version number) is spent.
  ROOT="$work" bash "$top/scripts/check-updater-key.sh"

  local old_version
  old_version=$(grep -oE '^appVersion: v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?' "$chart" | head -1 | sed 's/appVersion: //')
  echo "==> bumping $old_version → $VERSION on $branch"
  ROOT="$work" bash "$top/scripts/bump-version.sh" "$VERSION"

  # Guard 6: the bump changed something.
  if git -C "$work" diff --quiet; then
    echo "release: main is already at $VERSION; nothing to release" >&2
    exit 1
  fi
  # Guard 7: a patch release leaves the desktop side alone.
  check_desktop_side

  # Committed on the worktree's detached HEAD and pushed straight to the
  # branch on origin: no local branch is made, so none is left behind.
  git -C "$work" commit --quiet -a -F - <<EOM
Release $VERSION

- chart appVersion: $old_version → $VERSION
- Kivali Desktop (tauri.conf.json, Cargo.toml/lock, package.json/lock): ${old_version#v} → ${VERSION#v}

Once this is merged, scripts/release.sh tags the merge $VERSION, which
starts the release workflow (docs/developers/releasing.md). Each org
upgrades from the release feed: Update in Kivali Desktop, or
    kivali-supervisor upgrade
EOM
  git -C "$work" push --quiet origin "HEAD:refs/heads/$branch"
  "$GH" pr create --base main --head "$branch" --title "Release $VERSION" --body "$(cat <<EOM
Bumps every versioned file from $old_version to $VERSION (\`scripts/bump-version.sh\`).

Merging this lets \`scripts/release.sh $VERSION\` tag the merged commit \`$VERSION\`; the tag starts the release workflow. See [releasing.md](docs/developers/releasing.md).
EOM
)"
  cleanup
}

tag_merge() {
  local number="$1" sha
  sha="$("$GH" pr view "$number" --json mergeCommit --jq '.mergeCommit.oid // empty')"
  if [ -z "$sha" ]; then
    echo "release: pull request #$number is merged but GitHub reports no merge commit" >&2
    exit 1
  fi
  git fetch origin --quiet
  # Guard 8: the merge is on main.
  if ! git merge-base --is-ancestor "$sha" origin/main; then
    echo "release: #$number's merge commit $sha is not on origin/main" >&2
    exit 1
  fi
  # Guard 9: what is about to be tagged says VERSION, with the owner's key.
  with_commit "$sha"
  ROOT="$work" bash "$top/scripts/version-check.sh" "$VERSION"
  ROOT="$work" bash "$top/scripts/check-updater-key.sh"
  cleanup

  echo "==> tagging $(git rev-parse --short "$sha") (#$number) $VERSION"
  git tag -a "$VERSION" -m "Release $VERSION" "$sha"
  git push --quiet origin "refs/tags/$VERSION"
  # The branch has served its purpose. GitHub may have deleted it already.
  git push --quiet origin --delete "$branch" >/dev/null 2>&1 || true

  echo
  echo "==> release $VERSION tagged"
  echo "    commit: $(git rev-parse --short "$sha")"
  echo
  echo "The tag push started the release workflow (.github/workflows/release.yml)."
  echo "Once it publishes, orgs take the release with \`kivali-supervisor upgrade\`"
  echo "(Update in Kivali Desktop); docs/developers/releasing.md lists the steps after it."
}

branch_on_origin() {
  git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1
}

# Nothing to resume, or only a pull request that was closed unmerged:
# open one, unless the branch is still on origin (someone may be using it).
pr="$(find_pr)"
if [ -z "$pr" ] || [ "${pr#* }" = CLOSED ]; then
  if branch_on_origin; then
    if [ -z "$pr" ]; then
      echo "release: origin has a branch $branch but no pull request from it" >&2
      echo "  open one (gh pr create --base main --head $branch), or delete the branch and run this again:" >&2
    else
      echo "release: #${pr%% *} from $branch was closed without merging" >&2
      echo "  to start over, delete the branch and run this again:" >&2
    fi
    echo "    git push origin --delete $branch" >&2
    exit 1
  fi
  open_pr
  pr="$(find_pr)"
  if [ -z "$pr" ]; then
    echo "release: opened a pull request from $branch, but gh cannot find it" >&2
    exit 1
  fi
fi

number="${pr%% *}"
state="${pr#* }"
url="$("$GH" pr view "$number" --json url --jq .url)"
while [ "$state" = OPEN ]; do
  if [ -z "$wait" ]; then
    echo "==> $url is open. Merge it, then run this again to tag it:"
    echo "    make release VERSION=$VERSION"
    exit 0
  fi
  echo "==> waiting for $url to be merged (checking every ${POLL}s; Ctrl-C is safe, run this again to carry on)"
  sleep "$POLL"
  state="$("$GH" pr view "$number" --json state --jq .state)"
done

case "$state" in
  MERGED) tag_merge "$number" ;;
  *)
    echo "release: $url was closed without merging" >&2
    echo "  to start over, delete the branch and run this again:" >&2
    echo "    git push origin --delete $branch" >&2
    exit 1 ;;
esac
