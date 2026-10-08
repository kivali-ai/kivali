#!/bin/sh
#
# third-party-licenses.sh — the third-party license notices Kivali
# carries (THIRD_PARTY_LICENSES).
#
#   scripts/third-party-licenses.sh all OUTDIR
#       OUTDIR/THIRD_PARTY_LICENSES.txt, the release's one file: every
#       component of a Kivali release, from this checkout
#       (make third-party-licenses)
#   scripts/third-party-licenses.sh release [TRIPLE]
#       the same file on stdout, with Kivali Desktop's components for the
#       Rust target TRIPLE (default aarch64-apple-darwin): what the app
#       bundles (desktop-app/Makefile)
#
# and the pieces those are made of, each on stdout (the Dockerfiles call
# them in the stages that hold the dependencies, so an image's own file
# lists exactly what was built into it):
#
#   header ARTIFACT        Kivali's NOTICE and what ARTIFACT bundles besides
#                          (release, kivali, kivali-dev-shell,
#                          kivali-egress-proxy)
#   go PKG...              the Go modules linked into PKG..., for the GOOS,
#                          GOARCH and CGO_ENABLED in the environment, and the
#                          Go standard library
#   npm DIR                the production packages of DIR/package-lock.json,
#                          as installed in DIR/node_modules
#   cargo MANIFEST TARGET  the crates a build of MANIFEST for the target
#                          triple TARGET links (normal dependencies; not
#                          build scripts, proc macros or dev dependencies)
#
# and the dependency lists the npm and cargo sections are made from, one
# component per line (scripts/license-check.sh checks their licenses):
#
#   npm-deps DIR                 NAME@VERSION|DIR|DECLARED LICENSE
#   cargo-deps MANIFEST TARGET   NAME VERSION|DIR|LICENSE|LICENSE FILE
#
# Each component's license files are reproduced from the source the build
# used (the Go module cache, node_modules, the Cargo registry); identical
# texts are printed once, under the names of every component that ships
# them. A component without a license file is listed with its declared
# license and reported on stderr.
#
# POSIX sh (the Go image stages are Alpine, without bash). Needs go for
# `go`, node for `npm`, cargo and jq for `cargo`.

set -eu
# pipefail where the shell has it (bash, busybox ash, dash from Debian 13;
# not the dash of Debian 12, which the node image is). Every command whose
# failure matters writes a file rather than feeding a pipe.
# shellcheck disable=SC3040
if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi
# One sort order on every machine.
export LC_ALL=C

root=$(cd "$(dirname "$0")/.." && pwd)
# Field separator of the lists below (not a tab: read collapses empty
# fields between whitespace separators).
sep='|'
rule='================================================================================'
thin='--------------------------------------------------------------------------------'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

die() {
  echo "third-party-licenses: $*" >&2
  exit 1
}

# license_files DIR: the license and notice files directly in DIR.
license_files() {
  find "$1" -maxdepth 1 -type f 2>/dev/null |
    grep -Ei '/(licen[cs]e|copying|notice|unlicense|copyright)([-._][a-z0-9._-]*)?$' |
    grep -Ev '\.(go|rs|js|cjs|mjs|ts|json|py|c|h|html|toml|sh)$' |
    sort || true
}

# emit TITLE LIST: one section. LIST holds lines
# "component|license file|declared license"; the file is empty
# for a component that ships none.
emit() {
  printf '\n%s\n%s\n%s\n' "$rule" "$1" "$rule"
  [ -s "$2" ] || { printf '\n(none)\n'; return 0; }
  sort -u "$2" | while IFS="$sep" read -r comp file decl; do
    if [ -n "$file" ]; then
      key=$(tr -d '\r' < "$file" | cksum | tr ' ' '-')
    else
      key="none-$decl"
      echo "third-party-licenses: $comp ships no license file (declared: ${decl:-nothing})" >&2
    fi
    printf '%s|%s|%s|%s\n' "$key" "$comp" "$file" "$decl"
  done > "$tmp/keyed"
  # Groups in the order of their first component's name.
  sort -t "$sep" -k2,2 "$tmp/keyed" | awk -F "$sep" '!seen[$1]++ {print $1}' > "$tmp/keys"
  while read -r key; do
    awk -F "$sep" -v k="$key" '$1 == k' "$tmp/keyed" | sort -t "$sep" -k2,2 > "$tmp/group"
    printf '\n%s\n' "$thin"
    cut -d "$sep" -f2 "$tmp/group" | sort -u
    printf '%s\n\n' "$thin"
    file=$(head -n 1 "$tmp/group" | cut -d "$sep" -f3)
    if [ -n "$file" ]; then
      tr -d '\r' < "$file"
      [ -z "$(tail -c 1 "$file")" ] || echo
    else
      decl=$(head -n 1 "$tmp/group" | cut -d "$sep" -f4)
      echo "No license file is published with these components. Declared license: ${decl:-none}"
      echo "(the license texts: https://spdx.org/licenses/)."
    fi
  done < "$tmp/keys"
}

header() {
  artifact=$1
  case "$artifact" in
    kivali) what='the kivali image (the Kivali server and its web app)' ;;
    kivali-dev-shell) what='the kivali-dev-shell image (the agents'"'"' shell)' ;;
    kivali-egress-proxy) what='the kivali-egress-proxy image' ;;
    release) what='Kivali: Kivali Desktop, the Kivali VM image, and the kivali, kivali-dev-shell and kivali-egress-proxy images' ;;
    *) die "unknown artifact '$artifact'" ;;
  esac
  printf 'Third-party software in %s\n%s\n\n' "$what" "$rule"
  cat <<'EOF'
Kivali is licensed under the Apache License, Version 2.0
(https://www.apache.org/licenses/LICENSE-2.0). This file reproduces its
NOTICE and the license notices of the third-party software it includes.
Each third-party component is distributed under its own license.

EOF
  tr -d '\r' < "$root/NOTICE"
  case "$artifact" in
    kivali)
      printf '\n%s\nSIL Open Font License 1.1 (the fonts named above)\n%s\n\n' "$thin" "$thin"
      tr -d '\r' < "$root/design-system/fonts/OFL.txt"
      cat <<'EOF'

--------------------------------------------------------------------------------
Claude Code
--------------------------------------------------------------------------------

This image includes Claude Code (the npm package @anthropic-ai/claude-code),
unmodified, at /usr/local/bin/claude. Claude Code is proprietary software
of Anthropic PBC: copyright Anthropic PBC, all rights reserved, its use
subject to Anthropic's terms (https://www.anthropic.com/legal/commercial-terms
and, for consumer plans, https://www.anthropic.com/legal/consumer-terms).
It is not part of Kivali and not covered by Kivali's license.

--------------------------------------------------------------------------------
Debian packages
--------------------------------------------------------------------------------

The operating system is Debian. Each package's copyright and license are
in /usr/share/doc/<package>/copyright; the installed packages and the
offer of their source code are in /usr/share/doc/kivali/packages.txt and
/usr/share/doc/kivali/SOURCES.md.
EOF
      ;;
    kivali-dev-shell)
      cat <<'EOF'

--------------------------------------------------------------------------------
Debian packages
--------------------------------------------------------------------------------

The operating system and the agents' tools are Debian packages. Each
package's copyright and license are in /usr/share/doc/<package>/copyright;
the installed packages and the offer of their source code are in
/usr/share/doc/kivali/packages.txt and /usr/share/doc/kivali/SOURCES.md.
EOF
      ;;
    kivali-egress-proxy)
      cat <<'EOF'

--------------------------------------------------------------------------------
Base image
--------------------------------------------------------------------------------

The image is built on gcr.io/distroless/static
(https://github.com/GoogleContainerTools/distroless): CA certificates,
time zone data and /etc files from Debian packages, whose copyright
files are under /usr/share/doc/.
EOF
      ;;
    release)
      printf '\n%s\nSIL Open Font License 1.1 (the fonts named above)\n%s\n\n' "$thin" "$thin"
      tr -d '\r' < "$root/design-system/fonts/OFL.txt"
      cat <<'EOF'

--------------------------------------------------------------------------------
Claude Code
--------------------------------------------------------------------------------

The kivali image includes Claude Code (the npm package
@anthropic-ai/claude-code), unmodified, at /usr/local/bin/claude. Claude
Code is proprietary software of Anthropic PBC: copyright Anthropic PBC,
all rights reserved, its use subject to Anthropic's terms
(https://www.anthropic.com/legal/commercial-terms and, for consumer
plans, https://www.anthropic.com/legal/consumer-terms). It is not part of
Kivali and not covered by Kivali's license.

--------------------------------------------------------------------------------
The Kivali VM image and the operating systems of the images
--------------------------------------------------------------------------------

Kivali Desktop includes the Kivali VM image: a Linux kernel and Alpine
Linux packages, k3s and the container images k3s runs, and the kivali,
kivali-egress-proxy and kivali-dev-shell images. The kivali and
kivali-dev-shell images are built on Debian, the kivali-egress-proxy
image on gcr.io/distroless/static. Much of this is licensed under the
GNU GPL or LGPL. Each Debian package's copyright and license are in
/usr/share/doc/<package>/copyright inside its image. The exact packages
and versions, where their source code is published, and Kivali's written
offer of the complete corresponding source are in SOURCES.md, inside the
VM and each image at /usr/share/doc/kivali/SOURCES.md and published with
every release at https://github.com/kivali-ai/kivali/releases.
EOF
      sh "$root/scripts/third-party-sources.sh" offer
      ;;
  esac
}

go_section() {
  [ "$#" -gt 0 ] || die "go: no packages"
  go list -deps -f '{{if not .Standard}}{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}|{{.Dir}}|{{$.Dir}}{{end}}{{end}}{{end}}' "$@" > "$tmp/go-deps"
  grep -v '^$' "$tmp/go-deps" | sort -u > "$tmp/go-pkgs" || true
  : > "$tmp/go-list"
  while IFS="$sep" read -r mod moddir pkgdir; do
    # The package's own directory up to its module's root: a license
    # beside vendored code applies as well as the module's.
    d=$pkgdir
    found=
    while :; do
      license_files "$d" > "$tmp/files"
      while read -r f; do
        printf '%s|%s|\n' "$mod" "$f" >> "$tmp/go-list"
        found=1
      done < "$tmp/files"
      [ "$d" != "$moddir" ] && [ "$d" != / ] || break
      d=$(dirname "$d")
    done
    [ -n "$found" ] || printf '%s||\n' "$mod" >> "$tmp/go-list"
  done < "$tmp/go-pkgs"
  # A module whose package directories have no license but whose root has
  # one lists it from the root; drop its empty entries.
  awk -F "$sep" 'NR == FNR { if ($2 != "") has[$1] = 1; next } $2 != "" || !has[$1]' \
    "$tmp/go-list" "$tmp/go-list" > "$tmp/go-list2"
  # The standard library's license is GOROOT's (Homebrew moves it to the
  # directory above).
  goroot=$(go env GOROOT)
  golicense=
  for f in "$goroot/LICENSE" "$goroot/../LICENSE"; do
    if [ -f "$f" ]; then golicense=$f; break; fi
  done
  printf 'Go standard library %s|%s|BSD-3-Clause\n' "$(go env GOVERSION)" "$golicense" >> "$tmp/go-list2"
  emit "Go modules ($(go env GOOS)/$(go env GOARCH))" "$tmp/go-list2"
}

# npm_deps DIR: the production packages of DIR/package-lock.json, as
# installed in DIR/node_modules: NAME@VERSION|DIR|DECLARED LICENSE. The
# declared license is the lock's, else the installed package.json's.
npm_deps() {
  dir=$1
  [ -f "$dir/package-lock.json" ] || die "npm: no $dir/package-lock.json"
  node -e '
    const fs = require("fs"), path = require("path");
    const dir = process.argv[1];
    const lock = JSON.parse(fs.readFileSync(path.join(dir, "package-lock.json"), "utf8"));
    // A package.json license: an SPDX string, or the older {type} object
    // or list of them.
    const declared = (p) => {
      if (typeof p.license === "string") return p.license;
      if (p.license && typeof p.license.type === "string") return p.license.type;
      if (Array.isArray(p.licenses)) return p.licenses.map((l) => l.type || l).join(" OR ");
      return "";
    };
    for (const [k, v] of Object.entries(lock.packages || {})) {
      if (!k.includes("node_modules/") || v.dev || v.devOptional || v.link) continue;
      const d = path.join(dir, k);
      if (!fs.existsSync(d)) {
        // An optional package for another platform is not installed.
        if (v.optional) continue;
        console.error("third-party-licenses: " + k + " is not installed (npm ci in " + dir + ")");
        process.exit(1);
      }
      let lic = declared(v);
      if (!lic) {
        try {
          lic = declared(JSON.parse(fs.readFileSync(path.join(d, "package.json"), "utf8")));
        } catch (e) {
          lic = "";
        }
      }
      console.log(k.replace(/^.*node_modules\//, "") + "@" + v.version + "|" + d + "|" + lic);
    }
  ' "$dir"
}

npm_section() {
  dir=$1
  npm_deps "$dir" > "$tmp/npm-pkgs"
  : > "$tmp/npm-list"
  while IFS="$sep" read -r pkg pdir decl; do
    license_files "$pdir" > "$tmp/files"
    [ -s "$tmp/files" ] || printf '%s||%s\n' "$pkg" "$decl" >> "$tmp/npm-list"
    while read -r f; do printf '%s|%s|%s\n' "$pkg" "$f" "$decl" >> "$tmp/npm-list"; done < "$tmp/files"
  done < "$tmp/npm-pkgs"
  emit "npm packages ($(basename "$(cd "$dir" && pwd)"))" "$tmp/npm-list"
}

# cargo_deps MANIFEST TARGET: the crates a build of MANIFEST for TARGET
# links: NAME VERSION|DIR|LICENSE|LICENSE FILE, the last two as the
# crate's Cargo.toml declares them.
cargo_deps() {
  manifest=$1
  target=$2
  "${CARGO:-cargo}" metadata --format-version 1 --locked --filter-platform "$target" --manifest-path "$manifest" > "$tmp/meta.json"
  # Walks the resolve graph from the root through normal dependencies.
  # A proc macro runs in the compiler and is not linked, so neither it
  # nor what only it depends on ships; nor do workspace (path) crates.
  jq -r '
    (.packages | map({key: .id, value: .}) | from_entries) as $pk
    | (.resolve.nodes | map({key: .id, value: .}) | from_entries) as $nd
    | def procmacro($id): any($pk[$id].targets[]; .kind | index("proc-macro"));
    {seen: {}, todo: [.resolve.root]}
    | until(.todo | length == 0;
        .todo[0] as $i
        | .todo |= .[1:]
        | if .seen[$i] then .
          else .seen[$i] = true
            | if procmacro($i) then .
              else .todo += [$nd[$i].deps[] | select(any(.dep_kinds[]; .kind == null)) | .pkg]
              end
          end)
    | .seen | keys[]
    | $pk[.]
    | select(.source != null and (procmacro(.id) | not))
    | [.name + " " + .version, (.manifest_path | rtrimstr("/Cargo.toml")), (.license // ""), (.license_file // "")]
    | join("|")
  ' "$tmp/meta.json"
}

cargo_section() {
  target=$2
  cargo_deps "$1" "$target" > "$tmp/crates"
  : > "$tmp/cargo-list"
  while IFS="$sep" read -r crate cdir decl lfile; do
    license_files "$cdir" > "$tmp/files"
    if [ -n "$lfile" ] && [ -f "$cdir/$lfile" ]; then
      echo "$cdir/$lfile" >> "$tmp/files"
    fi
    [ -s "$tmp/files" ] || printf '%s||%s\n' "$crate" "$decl" >> "$tmp/cargo-list"
    sort -u "$tmp/files" | while read -r f; do printf '%s|%s|%s\n' "$crate" "$f" "$decl"; done >> "$tmp/cargo-list"
  done < "$tmp/crates"
  emit "Rust crates ($target)" "$tmp/cargo-list"
}

# release TRIPLE: every component of a release. The Linux side (the
# server and its web app, the agents' shell, the egress proxy, the VM's
# guest agent) is the same on amd64 and arm64; Kivali Desktop's
# supervisor sidecar is built for TRIPLE, with cgo on macOS only
# (desktop-app/Makefile GO_ENV).
release() {
  triple=$1
  case "$triple" in
    *-apple-darwin) goos=darwin cgo=1 ;;
    *-windows-*) goos=windows cgo=0 ;;
    *) die "release: no supervisor build for $triple" ;;
  esac
  case "$triple" in
    aarch64-*) goarch=arm64 ;;
    x86_64-*) goarch=amd64 ;;
    *) die "release: unknown architecture in $triple" ;;
  esac
  self="$root/scripts/third-party-licenses.sh"
  header release
  (cd "$root" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 sh "$self" go . ./cmd/dev-shell ./cmd/egress-proxy ./cmd/kivali-guest)
  (cd "$root" && GOOS=$goos GOARCH=$goarch CGO_ENABLED=$cgo sh "$self" go ./cmd/kivali-supervisor)
  npm_section "$root/web"
  cargo_section "$root/desktop-app/src-tauri/Cargo.toml" "$triple"
  npm_section "$root/desktop-app"
}

cmd=${1:-}
[ "$#" -eq 0 ] || shift
case "$cmd" in
  header) [ "$#" -eq 1 ] || die "usage: header ARTIFACT"; header "$1" ;;
  go) go_section "$@" ;;
  npm) [ "$#" -eq 1 ] || die "usage: npm DIR"; npm_section "$1" ;;
  cargo) [ "$#" -eq 2 ] || die "usage: cargo MANIFEST TARGET"; cargo_section "$1" "$2" ;;
  npm-deps) [ "$#" -eq 1 ] || die "usage: npm-deps DIR"; npm_deps "$1" ;;
  cargo-deps) [ "$#" -eq 2 ] || die "usage: cargo-deps MANIFEST TARGET"; cargo_deps "$1" "$2" ;;
  release) [ "$#" -le 1 ] || die "usage: release [TRIPLE]"; release "${1:-aarch64-apple-darwin}" ;;
  all)
    [ "$#" -eq 1 ] || die "usage: all OUTDIR"
    out=$1
    mkdir -p "$out"
    release aarch64-apple-darwin > "$tmp/release"
    mv "$tmp/release" "$out/THIRD_PARTY_LICENSES.txt"
    ls -l "$out"
    ;;
  *)
    die "usage: $0 all OUTDIR | release [TRIPLE] | header ARTIFACT | go PKG... | npm DIR | cargo MANIFEST TARGET | npm-deps DIR | cargo-deps MANIFEST TARGET"
    ;;
esac
