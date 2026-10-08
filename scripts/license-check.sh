#!/bin/sh
#
# license-check.sh — holds every third-party component Kivali ships to the
# license policy in scripts/license-policy.txt (make license-check).
#
#   scripts/license-check.sh
#
# The components are the ones scripts/third-party-licenses.sh lists in a
# release's notices:
#
#   Go     the modules linked into the Linux programs (the server, the
#          agents' shell, the egress proxy, the VM's guest agent) and into
#          kivali-supervisor for macOS (arm64, cgo) and Windows (amd64);
#          license identifiers detected from the license files by
#          go-licenses, pinned below
#   npm    the production packages of web/ and desktop-app/ (npm-deps):
#          their declared licenses
#   cargo  the crates desktop-app/src-tauri links for aarch64-apple-darwin
#          and x86_64-pc-windows-msvc (cargo-deps): their declared licenses
#
# scripts/licensecheck evaluates every license expression against the
# policy and prints the components that fail with how to fix each.
#
# Needs go, node with web/node_modules and desktop-app/node_modules
# installed, cargo (or $CARGO) and jq. The first run downloads go-licenses
# and the crates' metadata.

set -eu
# One sort order on every machine.
export LC_ALL=C

GO_LICENSES_VERSION=v2.0.1

root=$(cd "$(dirname "$0")/.." && pwd)
deps_script="$root/scripts/third-party-licenses.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

die() {
  echo "license-check: $*" >&2
  exit 1
}

# go-licenses is built for this machine, then run for each target.
GOBIN="$tmp/bin" go install "github.com/google/go-licenses/v2@$GO_LICENSES_VERSION"
mod=$(cd "$root" && go list -m)
# One line per license go-licenses finds; none found is an empty license.
printf '%s\n%s' '{{range .}}go|{{.Name}}|{{.Version}}|{{if ne .LicenseName "Unknown"}}{{.LicenseName}}{{end}}' '{{end}}' > "$tmp/go.tpl"

# golicenses GOOS GOARCH CGO_ENABLED PKG...: the Go lines for one build.
golicenses() {
  goos=$1 goarch=$2 cgo=$3
  shift 3
  if ! (cd "$root" && GOOS=$goos GOARCH=$goarch CGO_ENABLED=$cgo \
    "$tmp/bin/go-licenses" report --template "$tmp/go.tpl" --ignore "$mod" "$@") \
    >> "$tmp/deps" 2> "$tmp/go-licenses.log"; then
    cat "$tmp/go-licenses.log" >&2
    die "go-licenses failed for $goos/$goarch $*"
  fi
}

: > "$tmp/deps"
golicenses linux amd64 0 . ./cmd/dev-shell ./cmd/egress-proxy ./cmd/kivali-guest
golicenses darwin arm64 1 ./cmd/kivali-supervisor
golicenses windows amd64 0 ./cmd/kivali-supervisor

for dir in web desktop-app; do
  sh "$deps_script" npm-deps "$root/$dir" > "$tmp/npm"
  # NAME@VERSION (NAME may start with @) -> NAME|VERSION
  awk -F '|' '{ match($1, /@[^@]*$/); print "npm|" substr($1, 1, RSTART - 1) "|" substr($1, RSTART + 1) "|" $3 }' \
    "$tmp/npm" >> "$tmp/deps"
done

for target in aarch64-apple-darwin x86_64-pc-windows-msvc; do
  sh "$deps_script" cargo-deps "$root/desktop-app/src-tauri/Cargo.toml" "$target" > "$tmp/cargo"
  # NAME VERSION -> NAME|VERSION
  awk -F '|' '{ split($1, nv, " "); print "cargo|" nv[1] "|" nv[2] "|" $3 }' "$tmp/cargo" >> "$tmp/deps"
done

[ -s "$tmp/deps" ] || die "no dependencies found"
sort -u "$tmp/deps" > "$tmp/deps-sorted"
cd "$root"
go run ./scripts/licensecheck -policy scripts/license-policy.txt < "$tmp/deps-sorted"
