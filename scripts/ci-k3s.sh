#!/usr/bin/env bash
#
# ci-k3s.sh — run the VM's k3s directly on a disposable Linux host, for
# the integration suite (integration_test.go) on a CI runner.
#
#   KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh up
#   KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh down
#
# `up` fetches the k3s binary and airgap images the VM image bakes (the
# K3S_VERSION in vm/Makefile, through vm/scripts/fetch-k3s.sh and its
# pinned digests), places the images where k3s imports them at start,
# and starts `k3s server` with the VM's flags (vm/rootfs/usr/libexec/
# kivali/k3s-server), so the suite installs Kivali into the same k3s an
# org runs on. linux/arm64 only, like the VM. The kubeconfig is
# /etc/rancher/k3s/k3s.yaml, made readable.
#
# `down` stops k3s and deletes its state. Both refuse to run unless
# KIVALI_IT_DISPOSABLE_K3S=1 says this host's k3s may be wiped.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ "${KIVALI_IT_DISPOSABLE_K3S:-}" != "1" ]; then
  echo "ci-k3s: this starts and wipes k3s on this host; set KIVALI_IT_DISPOSABLE_K3S=1 on a disposable one" >&2
  exit 1
fi
if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "aarch64" ]; then
  echo "ci-k3s: needs linux/arm64, the VM's platform (this is $(uname -s)/$(uname -m))" >&2
  exit 1
fi

version=$(awk '$1 == "K3S_VERSION" && $2 == "?=" {print $3}' vm/Makefile)
if [ -z "$version" ]; then
  echo "ci-k3s: no K3S_VERSION in vm/Makefile" >&2
  exit 1
fi
dl="vm/build/dl/$version"
images="vm/build/k3s-images/arm64"
data=/var/lib/rancher/k3s

case "${1:-}" in
up)
  sh vm/scripts/fetch-k3s.sh "$version" arm64 "$dl" "$images" vm/k3s-images.lock
  # fetch-k3s.sh leaves the binary to the VM image build (vm/Dockerfile,
  # stage k3s). This runner is the disposable Linux host it runs on, so
  # it is downloaded here, checked against the release's sha256 list
  # the same way.
  want=$(awk '{sub(/\r$/, "")} $2 == "k3s-arm64" {print $1}' "$dl/sha256sum-arm64.txt")
  if [ -z "$want" ]; then
    echo "ci-k3s: no sha256 for k3s-arm64 in $dl/sha256sum-arm64.txt" >&2
    exit 1
  fi
  bin="$(mktemp -d)/k3s"
  curl -fsSL --retry 3 -o "$bin" "https://github.com/k3s-io/k3s/releases/download/${version//+/%2B}/k3s-arm64"
  echo "$want  $bin" | sha256sum -c -
  sudo install -m 0755 "$bin" /usr/local/bin/k3s
  # The suite calls kubectl; k3s is one when invoked by that name.
  command -v kubectl >/dev/null || sudo ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
  sudo install -D -m 0644 "$images/k3s-images.tar" "$data/agent/images/k3s-images.tar"
  echo "ci-k3s: starting k3s $version (log /tmp/k3s.log)"
  # The VM's flags, but its data dir is k3s's default here and the
  # kubeconfig is readable by the runner's user.
  sudo sh -c 'nohup /usr/local/bin/k3s server </dev/null \
    --disable=traefik,servicelb,metrics-server \
    --node-name=kivali \
    --write-kubeconfig-mode=0644 \
    --prefer-bundled-bin \
    >/tmp/k3s.log 2>&1 &'
  for _ in $(seq 1 90); do
    if sudo k3s kubectl get node kivali -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -qx True; then
      echo "ci-k3s: node kivali is Ready"
      exit 0
    fi
    sleep 2
  done
  echo "ci-k3s: k3s did not become Ready; the end of its log:" >&2
  sudo tail -n 50 /tmp/k3s.log >&2 || true
  exit 1
  ;;
down)
  sudo pkill -f '/usr/local/bin/k3s server' || true
  sleep 2
  sudo rm -rf "$data" /etc/rancher/k3s
  echo "ci-k3s: k3s stopped and its state removed"
  ;;
*)
  echo "usage: KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh up|down" >&2
  exit 2
  ;;
esac
