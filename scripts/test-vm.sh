#!/usr/bin/env bash
#
# test-vm.sh — run Kivali's Go suites on Linux inside a Kivali VM, on the
# developer's own machine: the platform's VM, booted by kivali-supervisor
# (macOS, or Windows through the Hyper-V broker), is the Linux the
# server runs on.
#
#   scripts/test-vm.sh unit          make unit-test in the VM (PKG, TESTFLAGS from the environment)
#   scripts/test-vm.sh integration   make integration-test in the VM
#   scripts/test-vm.sh destroy       delete the VM and its disk
#
# The VM is the build VM (scripts/build-vm.sh; KIVALI_BUILD_VM_DIR,
# default ~/.kivali-build-vm), an org-less VM of its own, apart from the
# dev org and the desktop app's teams. Each run boots it and stops it
# again: a running VM keeps every page it has touched until it stops
# (Virtualization.framework gives none back; vm/README.md),
# and a boot is seconds. Its disk is kept, and with it Go's module and
# build caches and BuildKit's; `destroy` deletes it. Each run:
#
#   1. boots it (`build-vm.sh up`: k3s, no Kivali installed, BuildKit running),
#   2. pulls the Go image into the VM's containerd, and for `integration`
#      builds this tree's server and egress-proxy images there,
#   3. copies this tree in (tracked and untracked, not ignored, files),
#   4. runs the suite as root in a privileged container on the VM's k3s
#      containerd, with the VM's k3s state, kubeconfig and containerd
#      socket mounted: the integration suite installs Kivali into the
#      VM's own k3s exactly as on a CI runner (scripts/ci-k3s.sh), with
#      the images present and the chart packaged here (the VM has no
#      helm),
#   5. stops it, whatever the outcome.
#
# Needs: bin/kivali-supervisor (make supervisor) and a VM image
# (make -C vm image, or an installed Kivali Desktop; KIVALI_VM_DIR
# overrides). No Docker.

set -euo pipefail

cd "$(dirname "$0")/.."

sup_bin=${SUPERVISOR_BIN:-bin/kivali-supervisor}
cfg=${KIVALI_BUILD_VM_DIR:-$HOME/.kivali-build-vm}
build_vm=scripts/build-vm.sh
go_version=$(awk -F= '/^ARG GO_VERSION=/ {sub(/\r$/, "", $2); print $2; exit}' Dockerfile)
go_image="golang:${go_version}-bookworm"
data=/var/lib/kivali
src=$data/test-src
cache=$data/test-cache

# Under Git Bash on Windows (MSYS), an argument that looks like a POSIX
# path is rewritten into a Windows path before a native program sees it,
# which turns the guest paths handed to `exec` (/var/lib/kivali/...)
# into C:\Program Files\Git\var\... and fails the run in the guest. The
# paths meant for Windows are made Windows paths here, and the rewriting
# is then switched off for everything that follows.
if command -v cygpath >/dev/null 2>&1; then
  cfg=$(cygpath -w "$cfg")
  export MSYS_NO_PATHCONV=1
fi

sup() { "$sup_bin" --config-dir "$cfg" "$@"; }

suite=${1:-}
[ $# -gt 0 ] && shift
case "$suite" in
destroy)
  "$build_vm" destroy
  echo "test-vm: destroyed"
  exit 0
  ;;
unit | integration) ;;
*)
  echo "usage: [PKG=... TESTFLAGS=...] scripts/test-vm.sh unit | integration | destroy" >&2
  exit 2
  ;;
esac

if [ ! -x "$sup_bin" ]; then
  echo "test-vm: $sup_bin is missing (make supervisor)" >&2
  exit 1
fi

# Size: the suites are CPU-bound (the VM's default is 4 CPUs), so the
# VM takes all but two of the host's, and 8 GiB, while it runs.
export BUILD_VM_MEMORY_MB="${TEST_VM_MEMORY_MB:-8192}"
if [ -n "${TEST_VM_CPUS:-}" ]; then
  export BUILD_VM_CPUS="$TEST_VM_CPUS"
fi
# Stop it on the way out, however the run ends: that is when its memory
# goes back to this computer.
trap '"$build_vm" down >/dev/null 2>&1 || true' EXIT
echo "==> test-vm: the VM ($cfg)"
"$build_vm" up

# The host's architecture is the guest's (arm64 on Apple Silicon, amd64
# on Windows under Hyper-V), so the guest pulls its own platform.
echo "==> test-vm: $go_image"
"$build_vm" pull "docker.io/library/$go_image"

tag="test-$(date +%s)"
chart=""
if [ "$suite" = integration ]; then
  echo "==> test-vm: building this tree's server and egress-proxy images in the VM ($tag)"
  "$build_vm" put-tree . src
  for img in kivali:Dockerfile kivali-egress-proxy:Dockerfile.egress-proxy; do
    "$build_vm" build --frontend dockerfile.v0 \
      --local context=/var/lib/kivali/build/ctx/src --local dockerfile=/var/lib/kivali/build/ctx/src \
      --opt "filename=${img#*:}" \
      --output "type=image,name=docker.io/library/${img%%:*}:$tag,unpack=true" >/dev/null
  done
  # A packaged chart is a gzipped tarball of the chart directory under
  # its name, named <name>-<version>.tgz (what `helm package` writes);
  # built here so the run needs no helm.
  chart_version=$(awk '$1 == "version:" {print $2; exit}' charts/kivali/Chart.yaml)
  mkdir -p "$cfg/chart"
  rm -f "$cfg"/chart/*.tgz
  chart="$cfg/chart/kivali-$chart_version.tgz"
  COPYFILE_DISABLE=1 tar -czf "$chart" -C charts kivali
fi

echo "==> test-vm: copying the tree in"
# COPYFILE_DISABLE: macOS's tar would add an AppleDouble ._<name> beside
# every file with extended attributes, which the tests then trip over.
git ls-files -co --exclude-standard -z | COPYFILE_DISABLE=1 tar --null -T - -czf - |
  sup exec -i -- sh -c "rm -rf $src && mkdir -p $src $cache && tar -xzf - -C $src"
if [ -n "$chart" ]; then
  chart_name=$(basename "$chart")
  sup exec -i -- sh -c "mkdir -p $src/.test-vm && cat > $src/.test-vm/$chart_name" <"$chart"
fi

envs=(--env GOFLAGS=-buildvcs=false --env "GOMODCACHE=$cache/mod" --env "GOCACHE=$cache/build")
mounts=(
  --mount "type=bind,src=$data,dst=$data,options=rbind:rw"
  --mount "type=bind,src=/etc/rancher,dst=/etc/rancher,options=rbind:ro"
  --mount "type=bind,src=/run/k3s,dst=/run/k3s,options=rbind:rw"
  --mount "type=bind,src=/usr/local/bin/k3s,dst=/usr/local/bin/k3s,options=rbind:ro"
)
case "$suite" in
unit)
  # The same target as a native run, with the same knobs.
  cmd="make unit-test PKG='${PKG:-./...}' TESTFLAGS='${TESTFLAGS:-}'"
  ;;
integration)
  envs+=(
    --env KIVALI_IT_DISPOSABLE_K3S=1
    --env KIVALI_IT_IMAGES_LOADED=1
    --env "KIVALI_IT_CHART=$src/.test-vm/$(basename "$chart")"
    --env "KIVALI_TEST_IMAGE_TAG=kivali:$tag"
    --env "KIVALI_TEST_EGRESS_IMAGE_TAG=kivali-egress-proxy:$tag"
    --env "K3S_DATA_DIR=$data/k3s"
    --env KIVALI_IT_KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  )
  # The suite calls kubectl; k3s is one when invoked by that name.
  cmd="ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl && make integration-test"
  ;;
esac

echo "==> test-vm: go test ($suite) in the VM"
sup exec -- k3s ctr run --rm --privileged --net-host "${mounts[@]}" "${envs[@]}" \
  --cwd "$src" "docker.io/library/$go_image" "kivali-test-$tag" sh -c "$cmd"
