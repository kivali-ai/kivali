# Kivali Desktop VM image and its boot test.
#
#   make -C vm image       kernel + initramfs + root disk into build/out, for
#                          ARCH: arm64 (the default; Virtualization.framework)
#                          or amd64 (Hyper-V; adds root.vhdx)
#                          (KIVALI_CHART=<tgz> bakes the chart too)
#   make -C vm dev-images  save the :dev Kivali images into
#                          build/kivali-images/<arch> (baked by the next `image`)
#   make -C vm boottest    boot build/out on Virtualization.framework (arm64)
#   make -C vm clean       remove build/
#
# ENGINE is where the Dockerfile runs: `vm`, the build VM
# (scripts/build-vm.sh: BuildKit in a Kivali VM on this machine, no
# Docker; the default on macOS and Windows), or `docker`, this machine's
# Docker (the default on Linux, which has no Kivali VM; CI). The build VM
# builds its own architecture only; the other one needs ENGINE=docker
# with binfmt emulation.
#
# The k3s binary and the guest agent are fetched and built inside the
# build; no Linux executable is ever written under build/. See README.md
# for the boot contract.

# Same reason as the root Makefile: an inherited GOROOT is usually stale.
unexport GOROOT

ARCH ?= arm64
ifeq ($(filter $(ARCH),amd64 arm64),)
$(error ARCH must be arm64 or amd64 (got '$(ARCH)'))
endif
PLATFORM := linux/$(ARCH)

ENGINE ?= $(if $(filter Linux,$(shell uname -s)),docker,vm)
ifeq ($(filter $(ENGINE),vm docker),)
$(error ENGINE must be vm or docker (got '$(ENGINE)'))
endif
BUILD_VM := ../scripts/build-vm.sh
# Where the build VM keeps contexts and outputs (scripts/build-vm.sh).
GUEST_CTX := /var/lib/kivali/build/ctx
GUEST_OUT := /var/lib/kivali/build/out
# KIVALI_BUILD_VM_KEEP=1 leaves the build VM running afterwards.
BUILD_VM_DOWN := $(if $(KIVALI_BUILD_VM_KEEP),true,$(BUILD_VM) down)

K3S_VERSION ?= v1.36.5+k3s1
# The guest agent is built with the Go of the server image.
GO_VERSION := $(shell awk -F= '/^ARG GO_VERSION=/ {sub(/\r$$/, "", $$2); print $$2; exit}' ../Dockerfile)

BUILD := build
OUT := $(BUILD)/out
# Text only: the release's sha256 list and k3s-images.txt.
DL := $(BUILD)/dl/$(K3S_VERSION)
K3S_IMAGES_DIR := $(BUILD)/k3s-images/$(ARCH)
# Where Kivali image tarballs are picked up from, for ARCH. The release
# pipeline points this at its own directory; dev-images fills the default.
KIVALI_IMAGES_DIR ?= $(BUILD)/kivali-images/$(ARCH)
DEV_IMAGES := kivali:dev kivali-egress-proxy:dev kivali-dev-shell:dev
# The same images as containerd names them (what `make images` builds
# into the build VM).
DEV_IMAGE_REFS := $(addprefix docker.io/library/,$(DEV_IMAGES))
# The Kivali chart baked next to the images, so a first install needs no
# download: KIVALI_CHART=../dist/kivali-0.15.1.tgz (from `make
# chart-package`). Empty bakes no chart; the supervisor then needs
# --chart on the first up.
KIVALI_CHART ?=
KIVALI_CHARTS_DIR := $(BUILD)/charts
# The root disk's version, reported by the guest agent.
VM_VERSION ?= dev

BOOTTEST_BIN := $(BUILD)/boottest
BOOTTEST_FLAGS ?=

.PHONY: image dev-images boottest clean

# build/out holds exactly one build: the new one is exported next to it
# and replaces it whole, so no artifact of another architecture or an
# earlier build stays behind, and a failed build leaves the last one.
image:
	@test -n '$(GO_VERSION)' || { echo "image: no ARG GO_VERSION= in ../Dockerfile" >&2; exit 1; }
ifeq ($(ENGINE),vm)
	$(BUILD_VM) up
	@got="$$($(BUILD_VM) arch)"; [ "$$got" = '$(ARCH)' ] || { echo "image: the build VM is $$got; an $(ARCH) image needs ENGINE=docker (with binfmt emulation)" >&2; exit 1; }
endif
	sh scripts/fetch-k3s.sh '$(K3S_VERSION)' '$(ARCH)' '$(DL)' '$(K3S_IMAGES_DIR)' k3s-images.lock '$(ENGINE)'
	mkdir -p '$(KIVALI_IMAGES_DIR)'
	rm -rf '$(KIVALI_CHARTS_DIR)'
	mkdir -p '$(KIVALI_CHARTS_DIR)'
	if [ -n '$(KIVALI_CHART)' ]; then cp '$(KIVALI_CHART)' '$(KIVALI_CHARTS_DIR)/'; fi
	rm -rf '$(OUT).new'
ifeq ($(ENGINE),docker)
	docker build --platform '$(PLATFORM)' --target 'artifacts-$(ARCH)' \
		--build-arg 'VM_VERSION=$(VM_VERSION)' \
		--build-arg 'K3S_VERSION=$(K3S_VERSION)' \
		--build-arg 'GO_VERSION=$(GO_VERSION)' \
		--build-context 'k3sdl=$(DL)' \
		--build-context 'k3simages=$(K3S_IMAGES_DIR)' \
		--build-context 'kivaliimages=$(KIVALI_IMAGES_DIR)' \
		--build-context 'kivalicharts=$(KIVALI_CHARTS_DIR)' \
		--build-context 'src=..' \
		--output 'type=local,dest=$(OUT).new' .
else
	$(BUILD_VM) put-tree . vm
	$(BUILD_VM) put '$(DL)' k3sdl
	$(BUILD_VM) put '$(K3S_IMAGES_DIR)' k3simages
	$(BUILD_VM) put '$(KIVALI_IMAGES_DIR)' kivaliimages
	$(BUILD_VM) put '$(KIVALI_CHARTS_DIR)' kivalicharts
	$(BUILD_VM) put-tree .. src
	$(BUILD_VM) exec -- sh /var/lib/kivali/build/bin/guest.sh clean-out image
	$(BUILD_VM) build --frontend dockerfile.v0 \
		--local 'context=$(GUEST_CTX)/vm' --local 'dockerfile=$(GUEST_CTX)/vm' \
		--opt 'target=artifacts-$(ARCH)' --opt 'platform=$(PLATFORM)' \
		--opt 'build-arg:VM_VERSION=$(VM_VERSION)' \
		--opt 'build-arg:K3S_VERSION=$(K3S_VERSION)' \
		--opt 'build-arg:GO_VERSION=$(GO_VERSION)' \
		--opt context:k3sdl=local:k3sdl --local 'k3sdl=$(GUEST_CTX)/k3sdl' \
		--opt context:k3simages=local:k3simages --local 'k3simages=$(GUEST_CTX)/k3simages' \
		--opt context:kivaliimages=local:kivaliimages --local 'kivaliimages=$(GUEST_CTX)/kivaliimages' \
		--opt context:kivalicharts=local:kivalicharts --local 'kivalicharts=$(GUEST_CTX)/kivalicharts' \
		--opt context:src=local:src --local 'src=$(GUEST_CTX)/src' \
		--output 'type=local,dest=$(GUEST_OUT)/image'
	$(BUILD_VM) get image '$(OUT).new'
	$(BUILD_VM_DOWN)
endif
	printf '%s\n' '$(VM_VERSION)' > '$(OUT).new/VERSION'
	rm -rf '$(OUT)'
	mv '$(OUT).new' '$(OUT)'
	ls -la '$(OUT)'

# The :dev images for baking: from this machine's Docker, or from the
# build VM's containerd, where `make images` built them.
dev-images:
	mkdir -p '$(KIVALI_IMAGES_DIR)'
ifeq ($(ENGINE),docker)
	docker save --platform '$(PLATFORM)' -o '$(KIVALI_IMAGES_DIR)/kivali-images.tar' $(DEV_IMAGES)
else
	$(BUILD_VM) up
	$(BUILD_VM) export '$(KIVALI_IMAGES_DIR)/kivali-images.tar' $(DEV_IMAGE_REFS)
	$(BUILD_VM_DOWN)
endif
	ls -la '$(KIVALI_IMAGES_DIR)'

# CGO is required by github.com/Code-Hex/vz; the binary must carry the
# com.apple.security.virtualization entitlement, ad-hoc signed.
$(BOOTTEST_BIN): $(wildcard boottest/*.go) boottest/entitlements.plist $(wildcard ../internal/supervisor/vz/*.go ../internal/supervisor/guestapi/*.go)
	mkdir -p '$(BUILD)'
	CGO_ENABLED=1 go -C .. build -o vm/$(BOOTTEST_BIN) ./vm/boottest
	codesign --force --entitlements boottest/entitlements.plist -s - '$(BOOTTEST_BIN)'

boottest: $(BOOTTEST_BIN)
	'$(BOOTTEST_BIN)' -dir '$(OUT)' -run '$(BUILD)/run' $(BOOTTEST_FLAGS)

clean:
	rm -rf '$(BUILD)'
