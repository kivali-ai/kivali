#!/bin/sh
#
# third-party-sources.sh — SOURCES.md: where the source code of the GPL
# and LGPL software Kivali ships is published, at the exact versions
# shipped, and Kivali's written offer of the complete corresponding
# source. Each piece is written to stdout as Markdown:
#
#   offer                     the written offer
#   header WHAT               a SOURCES.md title for WHAT, and the offer
#   apk TITLE DB ALPINE [PKG...]
#                             the GPL/LGPL packages in an Alpine installed
#                             database (lib/apk/db/installed), only PKG...
#                             when given, built for Alpine release ALPINE
#                             (/etc/alpine-release)
#   debian TITLE LIST         every Debian source package in LIST, lines
#                             "package version source-package source-version"
#                             (dpkg-query -W -f '${Package} ${Version}
#                             ${source:Package} ${source:Version}\n'); "#"
#                             lines are printed as notes
#   k3s VERSION VERSION_SH    k3s at tag VERSION and the k3s-root release
#                             its scripts/version.sh (VERSION_SH) pins
#   images LOCK               the container images in vm/k3s-images.lock
#   distroless                the egress proxy's base image
#
# Who calls what: the VM image build (vm/Dockerfile) writes the VM's
# SOURCES.md, the kivali and kivali-dev-shell image builds theirs from
# the packages they install, and the release workflow publishes the VM's
# with the images' Debian sections appended.
#
# POSIX sh: it runs in Alpine and Debian build stages.

set -eu
# pipefail where the shell has it (bash, busybox ash, dash from Debian 13;
# not the dash of Debian 12, which the node image is). Every command whose
# failure matters writes a file rather than feeding a pipe.
# shellcheck disable=SC3040
if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi
# One sort order on every machine.
export LC_ALL=C

die() {
  echo "third-party-sources: $*" >&2
  exit 1
}

repo=https://github.com/kivali-ai/kivali

offer() {
  cat <<EOF

## Written offer

Kivali includes software licensed under the GNU General Public License
(GPL) and the GNU Lesser General Public License (LGPL): among it the
Linux kernel, BusyBox and other Alpine Linux packages and k3s in the
Kivali VM image, and Debian packages in the kivali and kivali-dev-shell
images. SOURCES.md lists them, with links to the source code of the
exact versions shipped.

For three years from the date of the Kivali release that contains this
file, the Kivali authors offer to give any third party a complete
machine-readable copy of the corresponding source code of that software,
on a medium customarily used for software interchange, for a charge no
more than the cost of physically performing the source distribution. To
ask for it, open an issue at $repo/issues or a private
advisory at $repo/security/advisories/new titled
"Source request", naming the Kivali release and the components.
EOF
}

header() {
  printf '# Source code of the GPL and LGPL software in %s\n' "$1"
  offer
}

# url_path S: S with the characters a URL path cannot carry as is
# (Debian epochs' ":", "+" and "~") percent-encoded.
url_path() {
  printf '%s' "$1" | sed -e 's/%/%25/g' -e 's/:/%3A/g' -e 's/+/%2B/g' -e 's/~/%7E/g'
}

apk() {
  title=$1
  db=$2
  alpine=$3
  shift 3
  [ -f "$db" ] || die "apk: no $db"
  branch=$(printf '%s' "$alpine" | cut -d. -f1,2)
  [ -n "$branch" ] || die "apk: no Alpine release"
  printf '\n## %s\n\n' "$title"
  printf 'Alpine Linux %s packages. Each APKBUILD is at the aports commit the\n' "$alpine"
  printf 'package was built from; the upstream source archives it names are also\n'
  printf 'mirrored at https://distfiles.alpinelinux.org/distfiles/v%s/.\n\n' "$branch"
  printf '| Package | Version | License | Source |\n|---|---|---|---|\n'
  # The installed database: one paragraph per package, a field per line
  # (P name, V version, L license, o origin, c aports commit, U upstream
  # url). Every package Kivali installs is in aports' main repository.
  awk -v only=" $* " '
    function flush() {
      if (p != "" && (only == "  " || index(only, " " p " ")) && l ~ /GPL/) {
        src = "[APKBUILD](https://gitlab.alpinelinux.org/alpine/aports/-/tree/" c "/main/" o ")"
        if (u != "") src = src ", [upstream](" u ")"
        if (o ~ /^linux-/) {
          kv = v; sub(/-r[0-9]+$/, "", kv); split(kv, n, ".")
          src = src ", [kernel.org](https://cdn.kernel.org/pub/linux/kernel/v" n[1] ".x/linux-" kv ".tar.xz)"
        }
        printf "| %s | %s | %s | %s |\n", p, v, l, src
      }
      p = v = l = o = c = u = ""
    }
    /^$/ { flush(); next }
    /^P:/ { p = substr($0, 3) }
    /^V:/ { v = substr($0, 3) }
    /^L:/ { l = substr($0, 3) }
    /^o:/ { o = substr($0, 3) }
    /^c:/ { c = substr($0, 3) }
    /^U:/ { u = substr($0, 3) }
    END { flush() }
  ' "$db" | sort
}

debian() {
  title=$1
  list=$2
  [ -f "$list" ] || die "debian: no $list"
  printf '\n## %s\n\n' "$title"
  grep '^#' "$list" | sed 's/^# *//' || true
  printf '\nEvery Debian source package the installed packages were built from\n'
  printf '(not only the GPL and LGPL ones), at the version installed:\n\n'
  printf '| Source package | Version | Source |\n|---|---|---|\n'
  grep -v '^#' "$list" | awk 'NF >= 4 {print $3, $4}' | sort -u | while read -r src ver; do
    printf '| %s | %s | [snapshot.debian.org](https://snapshot.debian.org/package/%s/%s/), [sources.debian.org](https://sources.debian.org/src/%s/%s/) |\n' \
      "$src" "$ver" "$src" "$(url_path "$ver")" "$src" "$(url_path "$ver")"
  done
}

k3s() {
  version=$1
  versionsh=$2
  root=$(sed -n 's/^VERSION_ROOT="\(.*\)"$/\1/p' "$versionsh" | head -n 1)
  [ -n "$root" ] || die "k3s: no VERSION_ROOT in $versionsh"
  tag=$(url_path "$version")
  cat <<EOF

## k3s

k3s $version (Apache-2.0), with the containerd, runc, CNI plugins and the
other components its source tree pins, and k3s-root $root: the
userspace k3s unpacks (BusyBox, iptables, ipset, conntrack and their
libraries, mostly GPL), built with Buildroot.

- k3s: https://github.com/k3s-io/k3s/tree/$tag
- k3s-root: https://github.com/k3s-io/k3s-root/tree/$root (the Buildroot
  configuration; the package sources are the upstream releases it names)
EOF
}

images() {
  lock=$1
  [ -f "$lock" ] || die "images: no $lock"
  cat <<'EOF'

## Container images k3s runs

Saved unmodified from their publishers (Rancher's mirrors of the upstream
images) and pinned by digest. BusyBox and the other GPL software in them
is covered by the offer above.

EOF
  tr -d '\r' < "$lock" | awk 'NF >= 2 {printf "- `%s@%s`\n", $1, $2}'
  cat <<'EOF'

The kivali, kivali-egress-proxy and kivali-dev-shell images beside them
carry their own SOURCES.md at /usr/share/doc/kivali/; the release's
SOURCES.md includes their Debian packages.
EOF
}

distroless() {
  cat <<'EOF'

## Base image

The kivali-egress-proxy image is built on gcr.io/distroless/static, which
holds CA certificates, time zone data and /etc files from Debian
packages, listed in the image under /var/lib/dpkg/status.d/. Their
source: https://github.com/GoogleContainerTools/distroless and
https://snapshot.debian.org/.
EOF
}

cmd=${1:-}
[ "$#" -eq 0 ] || shift
case "$cmd" in
  offer) offer ;;
  header) [ "$#" -eq 1 ] || die "usage: header WHAT"; header "$1" ;;
  apk) [ "$#" -ge 3 ] || die "usage: apk TITLE DB ALPINE [PKG...]"; apk "$@" ;;
  debian) [ "$#" -eq 2 ] || die "usage: debian TITLE LIST"; debian "$1" "$2" ;;
  k3s) [ "$#" -eq 2 ] || die "usage: k3s VERSION VERSION_SH"; k3s "$1" "$2" ;;
  images) [ "$#" -eq 1 ] || die "usage: images LOCK"; images "$1" ;;
  distroless) distroless ;;
  *) die "usage: $0 offer | header WHAT | apk TITLE DB ALPINE [PKG...] | debian TITLE LIST | k3s VERSION VERSION_SH | images LOCK | distroless" ;;
esac
