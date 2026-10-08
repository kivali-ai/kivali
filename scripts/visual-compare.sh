#!/usr/bin/env bash
# Visual regression check that never commits baselines.
#
#   scripts/visual-compare.sh [--allow-dirty] <base-ref> [<target-ref>]
#
# Generates baselines from <base-ref> in a temporary git worktree (inside the
# Playwright linux/amd64 image, via `make web-e2e-baselines`), copies them into
# this checkout's web/e2e/__screenshots__/, then compares the CURRENT checkout
# against them (`make web-e2e-visual-check`). The target is always the current
# checkout; nothing is ever checked out here.
set -euo pipefail

usage() {
	echo "usage: $0 [--allow-dirty] <base-ref> [<target-ref>]" >&2
	exit 2
}

allow_dirty=0
refs=()
for arg in "$@"; do
	case "$arg" in
	--allow-dirty) allow_dirty=1 ;;
	-h | --help) usage ;;
	-*)
		echo "unknown option: $arg" >&2
		usage
		;;
	*) refs+=("$arg") ;;
	esac
done
if [ "${#refs[@]}" -lt 1 ] || [ "${#refs[@]}" -gt 2 ]; then
	usage
fi
base_ref="${refs[0]}"
target_ref="${refs[1]:-}"

step() { echo "==> $*"; }

if ! command -v docker >/dev/null 2>&1; then
	echo "docker is required: baselines are generated inside the Playwright linux/amd64 image." >&2
	exit 1
fi
if ! docker info >/dev/null 2>&1; then
	echo "docker is installed but its daemon is not reachable; start Docker and retry." >&2
	exit 1
fi

root="$(git rev-parse --show-toplevel)"
cd "$root"

base_sha="$(git rev-parse --verify --quiet "${base_ref}^{commit}")" || {
	echo "base ref not found: $base_ref" >&2
	exit 1
}
head_sha="$(git rev-parse HEAD)"

if [ -n "$target_ref" ]; then
	target_sha="$(git rev-parse --verify --quiet "${target_ref}^{commit}")" || {
		echo "target ref not found: $target_ref" >&2
		exit 1
	}
	if [ "$target_sha" != "$head_sha" ]; then
		echo "target $target_ref ($target_sha) is not the current checkout (HEAD $head_sha)." >&2
		echo "This script never switches checkouts: check out $target_ref, then rerun:" >&2
		echo "  scripts/visual-compare.sh $base_ref" >&2
		exit 1
	fi
fi

if [ "$allow_dirty" -eq 0 ] && [ -n "$(git status --porcelain -- web/)" ]; then
	echo "uncommitted changes under web/ would blur the comparison:" >&2
	git status --short -- web/ >&2
	echo "commit them, or pass --allow-dirty." >&2
	exit 1
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/kivali-visual-base.XXXXXX")"
worktree="$tmp/base"
# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap (0.11 says SC2329, older SC2317)
cleanup() {
	if [ -d "$worktree" ]; then
		git -C "$root" worktree remove --force "$worktree" >/dev/null 2>&1 || true
	fi
	git -C "$root" worktree prune >/dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap cleanup EXIT

step "1/4 temporary worktree at base $base_ref ($base_sha)"
git worktree add --detach "$worktree" "$base_sha"

step "2/4 generating baselines at base (Docker; this takes a while)"
make -C "$worktree" web-e2e-baselines

produced="$worktree/web/e2e/__screenshots__"
if [ ! -d "$produced" ]; then
	echo "baseline generation produced no $produced" >&2
	exit 1
fi

step "3/4 copying baselines into $root/web/e2e/__screenshots__"
rm -rf "$root/web/e2e/__screenshots__"
mkdir -p "$root/web/e2e"
cp -R "$produced" "$root/web/e2e/__screenshots__"
git worktree remove --force "$worktree"

step "4/4 comparing $head_sha against baselines from $base_sha"
status=0
make -C "$root" web-e2e-visual-check || status=$?

echo
echo "Playwright report: $root/web/playwright-report/index.html"
echo "  open it with: cd $root/web && npx playwright show-report"
echo "Diff images (expected/actual/diff): $root/web/e2e/test-results-linux/"
if [ "$status" -eq 0 ]; then
	echo "Result: no visual differences versus $base_ref."
else
	echo "Result: visual differences found (exit $status); review the report."
fi
exit "$status"
