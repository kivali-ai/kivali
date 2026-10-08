#!/usr/bin/env bash
#
# release.sh — cut a Kivali release.
#
# Bumps every file that carries the release version together
# (scripts/bump-version.sh; scripts/version-check.sh lists them):
# charts/kivali/Chart.yaml's appVersion (the image tag the chart
# installs by default, for the server, the egress proxy and the
# dev-shell), and the desktop app's version in desktop-app/src-tauri/tauri.conf.json, Cargo.toml,
# Cargo.lock, desktop-app/package.json and package-lock.json. Commits the
# bump with a templated message, creates an annotated git tag at the
# release commit, pushes the commit + tag to origin/main, and packages
# the chart into dist/kivali-<version>.tgz (`make chart-package`; skipped
# if helm is not installed).
#
# What this script does NOT do:
#   - Build or push container images. The release workflow builds them
#     and attaches the image bundles, the chart and release.json (the
#     feed kivali-supervisor upgrades from) to the GitHub release.
#
# Invoked by `make release VERSION=vX.Y.Z`. Guards:
#   1. VERSION is semver (v-prefixed)
#   2. Current branch is main
#   3. Tree is clean (no uncommitted changes to track)
#   4. local main is aligned with origin/main (prevents us from
#      pushing a release on top of someone else's unmerged work)
#   5. Tag $VERSION does not already exist locally or remotely
#   6. charts/kivali/Chart.yaml exists and has the expected appVersion
#      line (if its shape ever changes, the release fails loudly rather
#      than silently producing a no-op commit)
#   7. every versioned file agrees with the others BEFORE the bump
#      (scripts/version-check.sh): a hand edit of one of them stops the
#      release rather than being papered over
#   8. tauri.conf.json's updater public key is not the development key
#      (scripts/check-updater-key.sh)
#
# Pushing the tag starts .github/workflows/release.yml, which builds and
# uploads the release assets (docs/developers/releasing.md).
#
# Exit codes:
#   0 — commit + tag pushed to origin
#   1 — preconditions failed

set -euo pipefail

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "release: VERSION is required (e.g. make release VERSION=v0.2.0)" >&2
  exit 1
fi

# Guard 1: semver shape.
if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release: VERSION must be vMAJOR.MINOR.PATCH (got: $VERSION)" >&2
  exit 1
fi

# Guard 2: on main.
cur_branch="$(git rev-parse --abbrev-ref HEAD)"
if [ "$cur_branch" != "main" ]; then
  echo "release: must be on 'main' branch (got: $cur_branch)" >&2
  echo "  git checkout main && git pull" >&2
  exit 1
fi

# Guard 3: clean tree.
if ! git diff-index --quiet HEAD --; then
  echo "release: git tree has uncommitted changes; commit or stash first" >&2
  git status --short >&2
  exit 1
fi

# Guard 4: not behind or diverged from origin/main. Fetch with
# --tags so we pull any remote tags into local refs — that lets
# Guard 5 do a purely local tag-collision check instead of a second
# SSH round-trip via ls-remote.
#
# The alignment invariant: a push of local main must be a
# fast-forward. Local-ahead-of-origin is fine (common right after
# merging a feature branch); only block if origin has commits we
# don't, which is either divergent history or we're just behind.
echo "==> fetching origin"
git fetch origin --tags --quiet
if ! git merge-base --is-ancestor origin/main HEAD; then
  echo "release: origin/main has commits not in local main" >&2
  echo "  local:  $(git rev-parse --short HEAD)" >&2
  echo "  origin: $(git rev-parse --short origin/main)" >&2
  echo "  pull or rebase first, then retry" >&2
  exit 1
fi

# Guard 5: tag doesn't already exist. fetch --tags above pulled any
# origin tag into local refs/tags/, so a single local check covers
# both "tag exists locally" and "tag exists on origin" — no second
# SSH hit.
if git rev-parse --verify --quiet "refs/tags/$VERSION" >/dev/null; then
  echo "release: tag $VERSION already exists" >&2
  echo "  delete it first (locally and, if applicable, on origin):" >&2
  echo "    git tag -d $VERSION" >&2
  echo "    git push origin :refs/tags/$VERSION" >&2
  exit 1
fi

# Guard 6: chart shape.
chart="charts/kivali/Chart.yaml"
# Every file the release commit changes: release.sh bumps them together.
versioned=(
  "$chart"
  desktop-app/src-tauri/tauri.conf.json desktop-app/src-tauri/Cargo.toml
  desktop-app/src-tauri/Cargo.lock desktop-app/package.json
  desktop-app/package-lock.json
)
for f in "${versioned[@]}"; do
  if [ ! -f "$f" ]; then
    echo "release: $f not found" >&2
    exit 1
  fi
done
if ! grep -qE '^appVersion: v[0-9]+\.[0-9]+\.[0-9]+' "$chart"; then
  echo "release: $chart has no 'appVersion: vX.Y.Z' line; chart shape may have changed — adjust release.sh" >&2
  exit 1
fi

# Guard 7: the versioned files agree with each other before we touch
# them. (The chart's appVersion is the reference.)
echo "==> checking the current versions agree"
bash scripts/version-check.sh

# Guard 8: the updater public key is the owner's, not the development
# one. Checked before anything is committed or pushed: the release
# workflow refuses the same thing, but only after the tag (and its
# version number) is spent.
bash scripts/check-updater-key.sh

# Old version (for the commit message), from the chart's appVersion.
old_version=$(grep -oE '^appVersion: v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?' "$chart" | head -1 | sed 's/appVersion: //')

echo "==> releasing $old_version → $VERSION"

# Bump the chart's appVersion (the default tag of every Kivali image the
# chart installs) and the desktop app's version, all in one script that
# ends by running version-check on the result.
bash scripts/bump-version.sh "$VERSION"

# Sanity: the bump should have changed the files. If it didn't, the old
# and new versions are the same (VERSION == old_version) — a no-op commit
# is wrong.
if git diff --quiet "$chart"; then
  echo "release: no changes to $chart — already at $VERSION?" >&2
  git checkout -- "${versioned[@]}"
  exit 1
fi

# Commit the bump.
git add "${versioned[@]}"
git commit -F - <<EOM
Release $VERSION

- chart appVersion: $old_version → $VERSION
- Kivali Desktop (tauri.conf.json, Cargo.toml/lock, package.json/lock): ${old_version#v} → ${VERSION#v}

Pushing the tag starts the release workflow (docs/developers/releasing.md).
Each org upgrades from the release feed: Update in Kivali Desktop, or
    kivali-supervisor upgrade
EOM

# Tag the release commit.
git tag -a "$VERSION" -m "Release $VERSION"

# Push the branch + tag in a single SSH connection. --atomic makes
# the server accept-all-or-reject-all, so if branch protection
# rejects the main push, the tag push is also rejected — origin
# never ends up with a dangling release tag pointing at a commit
# that isn't on main.
echo "==> pushing main + tag $VERSION to origin"
git push origin --atomic main "$VERSION"

# Package the chart for this release into dist/ (gitignored; the release
# workflow builds and publishes the real assets from the tag). Skipped
# with a note if helm is not installed.
if command -v "${HELM:-helm}" >/dev/null 2>&1; then
  echo "==> packaging the chart"
  make chart-package VERSION="$VERSION"
else
  echo "release: helm not found; skipping the chart package (brew install helm, then make chart-package VERSION=$VERSION)" >&2
fi

echo
echo "==> release $VERSION done"
echo "    commit: $(git rev-parse --short HEAD)"
echo "    tag:    $VERSION"
echo
echo "The tag push started the release workflow (.github/workflows/release.yml)."
echo "Once it publishes, orgs take the release with \`kivali-supervisor upgrade\`"
echo "(Update in Kivali Desktop); docs/developers/releasing.md lists the steps after it."
