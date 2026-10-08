#!/usr/bin/env bash
#
# build-vm.sh — Kivali's build engine without Docker on this machine:
# BuildKit inside a Kivali VM of its own, the build VM, booted by
# kivali-supervisor (macOS, or Windows through the Hyper-V broker). The
# host streams build contexts in and artifacts out through
# `kivali-supervisor exec`; the guest half is scripts/build-vm-guest.sh,
# installed into the VM by `up`. Make targets drive it (`make images`,
# `make -C vm image`, `make test-vm`); the commands are:
#
#   scripts/build-vm.sh up                     boot the build VM and start BuildKit in it
#   scripts/build-vm.sh down                   stop the build VM (its disk, caches included, stays)
#   scripts/build-vm.sh destroy                delete the build VM and its disk
#   scripts/build-vm.sh put <dir> <name>       a directory becomes build context <name> in the VM
#   scripts/build-vm.sh put-tree <dir> <name>  the git-tracked and untracked (not ignored) files of
#                                              <dir>, a directory of this repository
#   scripts/build-vm.sh get <name> <dir>       build output <name> replaces <dir> on this machine
#   scripts/build-vm.sh build <args>           `buildctl build <args>` in the VM: a context is
#                                              $CTX/<name>, an output $OUT/<name> (see below)
#   scripts/build-vm.sh pull <ref>...          pull images into the VM's containerd
#   scripts/build-vm.sh digest <ref>           the digest the VM's containerd recorded for ref
#   scripts/build-vm.sh export <file> <ref>... the images, from the VM's containerd, as one tar
#   scripts/build-vm.sh arch                   the VM's architecture, amd64 or arm64
#   scripts/build-vm.sh exec [-i] -- <argv>    a command in the VM (kivali-supervisor exec)
#
# Paths inside the VM, for `build`: contexts under CTX=/var/lib/kivali/build/ctx,
# outputs under OUT=/var/lib/kivali/build/out, so a build names them
# `--local context=/var/lib/kivali/build/ctx/src` and
# `--output type=local,dest=/var/lib/kivali/build/out/image`.
#
# Environment:
#   KIVALI_BUILD_VM_DIR   the build VM's config directory (default ~/.kivali-build-vm)
#   KIVALI_VM_DIR         the VM image to boot: vm/build/out when it holds one, else the
#                         installed Kivali Desktop's (the bootstrap: the first image on a
#                         machine comes from a release, and builds its successors)
#   KIVALI_BUILD_VM_BOOT  0: never boot or stop; the config directory is a team already
#                         running (the dev team, for `make dev-load`), whose containerd
#                         gets the images straight from the build
#   BUILD_VM_CPUS, BUILD_VM_MEMORY_MB   the VM's size (default all but two CPUs, 6 GiB)
#   SUPERVISOR_BIN        bin/kivali-supervisor (make supervisor)
#
# Needs: the supervisor binary and a VM image. No Docker: the engine, its
# base images and the frontends are pulled by the guest.

set -euo pipefail

# Paths given on the command line are relative to where the caller is;
# the script itself works from the repository root.
orig=$PWD
cd "$(dirname "$0")/.."
abs() {
  case "$1" in
  /* | [A-Za-z]:*) printf '%s' "$1" ;;
  *) printf '%s/%s' "$orig" "$1" ;;
  esac
}

sup_bin=${SUPERVISOR_BIN:-bin/kivali-supervisor}
cfg=${KIVALI_BUILD_VM_DIR:-$HOME/.kivali-build-vm}
boot=${KIVALI_BUILD_VM_BOOT:-1}
guest_script=scripts/build-vm-guest.sh
guest_bin=/var/lib/kivali/build/bin/guest.sh

# Under Git Bash on Windows (MSYS) a POSIX-looking argument is rewritten
# into a Windows path before a native program sees it, which would turn
# the guest paths handed to `exec` into C:\Program Files\Git\...; the
# paths meant for Windows are converted here and the rewriting switched
# off for everything after.
if command -v cygpath >/dev/null 2>&1; then
  cfg=$(cygpath -w "$cfg")
  export MSYS_NO_PATHCONV=1
fi

sup() { "$sup_bin" --config-dir "$cfg" "$@"; }
guest() { sup exec -- sh "$guest_bin" "$@"; }
guest_in() { sup exec -i -- sh "$guest_bin" "$@"; }
die() { echo "build-vm: $*" >&2; exit 1; }

# The VM image directory, for the first boot.
image_dir() {
  local d
  if [ -n "${KIVALI_VM_DIR:-}" ]; then
    d=$KIVALI_VM_DIR
  elif [ -f vm/build/out/root.vhdx ] || [ -f vm/build/out/root.squashfs ]; then
    d=vm/build/out
  else
    case "$(uname -s)" in
    Darwin) d=/Applications/Kivali.app/Contents/Resources/vm ;;
    *) d="${ProgramFiles:-C:\\Program Files}\\Kivali\\vm" ;;
    esac
    [ -d "$d" ] || die "no VM image: build one (make -C vm image) or install Kivali Desktop, or set KIVALI_VM_DIR"
  fi
  if command -v cygpath >/dev/null 2>&1; then
    d=$(cygpath -w "$d")
  fi
  printf '%s' "$d"
}

vm_running() { sup status 2>/dev/null | grep -q '^vm: running'; }

ensure_supervisor() {
  [ -x "$sup_bin" ] || die "$sup_bin is missing (make supervisor)"
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
up)
  ensure_supervisor
  if ! vm_running; then
    [ "$boot" != 0 ] || die "the VM at $cfg is not running (KIVALI_BUILD_VM_BOOT=0: it is not booted here)"
    mkdir -p "$cfg"
    host_cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 6)
    cpus=${BUILD_VM_CPUS:-$(( host_cpus > 6 ? host_cpus - 2 : 4 ))}
    echo "==> build-vm: booting the build VM ($cfg)"
    sup up --prepare --vm-dir "$(image_dir)" --cpus "$cpus" --memory-mb "${BUILD_VM_MEMORY_MB:-6144}"
  fi
  sup exec -- mkdir -p "$(dirname "$guest_bin")"
  # A Windows checkout has CRLF line endings; the guest reads the script line by line.
  tr -d '\r' <"$guest_script" | sup exec -i -- tee "$guest_bin" >/dev/null
  guest buildkitd
  ;;
down)
  [ "$boot" != 0 ] || exit 0
  ensure_supervisor
  sup down --exit
  ;;
destroy)
  [ "$boot" != 0 ] || die "KIVALI_BUILD_VM_BOOT=0: not a VM this script owns"
  ensure_supervisor
  if [ -d "$cfg" ]; then
    sup destroy --yes --exit || true
    rm -rf "$cfg"
  fi
  echo "build-vm: destroyed"
  ;;
put)
  [ $# -eq 2 ] || die "usage: put <dir> <name>"
  dir=$(abs "$1")
  [ -d "$dir" ] || die "$1 is not a directory"
  tar -C "$dir" -cf - . | guest_in put "$2"
  ;;
put-tree)
  [ $# -eq 2 ] || die "usage: put-tree <dir> <name>"
  dir=$(abs "$1")
  [ -d "$dir" ] || die "$1 is not a directory"
  # COPYFILE_DISABLE: macOS's tar would add an AppleDouble ._<name>
  # beside every file with extended attributes.
  (cd "$dir" && git ls-files -co --exclude-standard -z . | COPYFILE_DISABLE=1 tar --null -T - -cf -) | guest_in put "$2"
  ;;
get)
  [ $# -eq 2 ] || die "usage: get <name> <dir>"
  dir=$(abs "$2")
  rm -rf "$dir.new"
  mkdir -p "$dir.new"
  guest get "$1" | tar -xf - -C "$dir.new"
  rm -rf "$dir"
  mv "$dir.new" "$dir"
  ;;
build)
  guest build "$@"
  ;;
pull)
  guest pull "$@"
  ;;
digest)
  guest digest "$@"
  ;;
export)
  [ $# -ge 2 ] || die "usage: export <file> <ref>..."
  out=$(abs "$1")
  shift
  guest export "$@" >"$out.part"
  mv "$out.part" "$out"
  ;;
arch)
  guest arch
  ;;
exec)
  sup exec "$@"
  ;;
*)
  sed -n '2,/^set -euo/{/^#/s/^# \{0,1\}//p}' "$0" >&2
  exit 2
  ;;
esac
