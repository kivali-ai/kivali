# Kivali Desktop, the Tauri shell. docs/developers/desktop-app.md is the reference.
#
#   make dev          run the app against DEV_CONFIG_DIR (default: a -dev config dir)
#   make build        release bundles + updater artifacts (needs the real supervisor):
#                     .app + .dmg on macOS, the per-machine NSIS installer on Windows
#                     (tauri.windows.conf.json; it registers the broker service)
#   make build-debug  debug bundle under src-tauri/target/debug/bundle (macOS: .app, ad-hoc signed;
#                     Windows: NSIS); needs no updater key (without one, no updater artifacts)
#   make build-trial  release-profile bundle for a trial install, no updater artifacts, no keys checked
#   make test         Rust unit tests + TypeScript tests
#   make lint         typecheck + clippy, warnings as errors (CI runs this)
#   make fmt-check    rustfmt --check; not part of lint (the Rust tree is not rustfmt-formatted)
#   make icons        regenerate the app icon and the tray icons (needs rsvg-convert)
#   make clean
#
# Rust is expected at ~/.cargo/bin when cargo is not on PATH.

CARGO ?= $(shell command -v cargo 2>/dev/null || echo $(HOME)/.cargo/bin/cargo)
CARGO_BIN := $(patsubst %/,%,$(dir $(CARGO)))
export PATH := $(CARGO_BIN):$(PATH)
NPM ?= npm
TRIPLE ?= $(shell PATH="$(CARGO_BIN):$$PATH" rustc -vV | sed -n 's/^host: //p')
REPO := $(abspath ..)

# The platform the build is for (the shell's `platform` module is the
# code side of the same split): macOS signs and notarizes and bundles an
# .app; Windows binaries end in .exe and bundle as NSIS.
ifneq ($(findstring windows,$(TRIPLE)),)
  EXE := .exe
  IS_DARWIN :=
  DEBUG_BUNDLES := nsis
  GO_ENV := GOOS=windows GOARCH=$(if $(findstring aarch64,$(TRIPLE)),arm64,amd64) CGO_ENABLED=0
else ifneq ($(findstring darwin,$(TRIPLE)),)
  EXE :=
  IS_DARWIN := 1
  DEBUG_BUNDLES := app
  GO_ENV := CGO_ENABLED=1
else
  EXE :=
  IS_DARWIN :=
  DEBUG_BUNDLES := deb
  GO_ENV := CGO_ENABLED=1
endif

# The one desktop version: tauri.conf.json's. Cargo.toml and package.json
# follow it (build.rs refuses a mismatch) and the supervisor is stamped
# with it, so the app and its sidecar carry one number.
DESKTOP_VERSION := $(shell node -p "require('$(CURDIR)/src-tauri/tauri.conf.json').version")

# The sidecar. `real` builds cmd/kivali-supervisor with Go; `fake` builds
# desktop-app/fake-supervisor; `auto` picks real once that package exists.
SUPERVISOR ?= auto
SIDECAR := src-tauri/binaries/kivali-supervisor-$(TRIPLE)$(EXE)
HAVE_REAL := $(wildcard $(REPO)/cmd/kivali-supervisor/main.go)
ifeq ($(SUPERVISOR),auto)
  SUPERVISOR_KIND := $(if $(HAVE_REAL),real,fake)
else
  SUPERVISOR_KIND := $(SUPERVISOR)
endif

# Development keeps its own config dir so it never touches a real org's
# files or endpoint. (Unix socket paths are limited to 104 bytes on
# macOS, which rules out a dir under this repository.)
ifneq ($(EXE),)
  DEV_CONFIG_DIR ?= $(LOCALAPPDATA)/Kivali-dev
else
  DEV_CONFIG_DIR ?= $(HOME)/Library/Application Support/Kivali-dev
endif

# The updater's signing key: the development key from `tauri signer
# generate` unless the environment already carries the owner's.
UPDATER_KEY := $(CURDIR)/.keys/updater.key

# build-debug needs no key: without one (TAURI_SIGNING_PRIVATE_KEY unset
# or empty, no .keys/updater.key) it leaves out the updater artifacts
# instead of failing to sign them. tauri merges repeated --config values
# in order, so this rides beside VM_BUNDLE's. `build` still refuses.
ifeq ($(TAURI_SIGNING_PRIVATE_KEY)$(wildcard $(UPDATER_KEY)),)
  DEBUG_NO_UPDATER := --config '{"bundle":{"createUpdaterArtifacts":false}}'
endif

# The VM image (`make -C vm image`; ARCH=amd64 for Windows) is bundled
# as the `vm` resource when its root disk has been built: on Windows
# root.vhdx (installed as <install dir>\vm, beside Kivali.exe), elsewhere
# Image, initramfs.gz and root.squashfs (macOS: Contents/Resources/vm);
# VERSION with either when present (the supervisor reads "dev" without
# it). Without the root disk the app still builds and the supervisor
# looks for the image itself (KIVALI_VM_DIR).
VM_IMAGE_DIR ?= $(REPO)/vm/build/out
ifneq ($(EXE),)
  VM_ROOT := root.vhdx
  VM_FILES := "$(VM_IMAGE_DIR)/root.vhdx":"vm/root.vhdx"
else
  VM_ROOT := root.squashfs
  VM_FILES := "$(VM_IMAGE_DIR)/Image":"vm/Image","$(VM_IMAGE_DIR)/initramfs.gz":"vm/initramfs.gz","$(VM_IMAGE_DIR)/root.squashfs":"vm/root.squashfs"
endif
ifneq ($(wildcard $(VM_IMAGE_DIR)/VERSION),)
  VM_FILES := $(VM_FILES),"$(VM_IMAGE_DIR)/VERSION":"vm/VERSION"
endif
ifneq ($(wildcard $(VM_IMAGE_DIR)/$(VM_ROOT)),)
  VM_BUNDLE := --config '{"bundle":{"resources":{$(VM_FILES)}}}'
endif

# The third-party license notices every bundle carries as the resource
# THIRD_PARTY_LICENSES.txt (Kivali > Open source licenses; Windows: Help),
# written for TRIPLE before each bundle: the whole release's notices, the
# VM's images and the app alike (scripts/third-party-licenses.sh release;
# needs go, cargo, jq, node, and the web app's npm install). Merged with
# VM_BUNDLE's resources.
LICENSES := $(CURDIR)/src-tauri/target/THIRD_PARTY_LICENSES.txt
LICENSES_BUNDLE := --config '{"bundle":{"resources":{"$(LICENSES)":"THIRD_PARTY_LICENSES.txt"}}}'

# A stale GOROOT in some shells breaks `go build` (see the root Makefile).
unexport GOROOT

.PHONY: dev build build-debug build-trial test lint fmt-check clean sidecar deps release-preflight icons licenses

deps: node_modules/.installed

node_modules/.installed: package.json package-lock.json
	$(NPM) ci
	@touch $@

sidecar:
	@mkdir -p src-tauri/binaries
ifeq ($(SUPERVISOR_KIND),real)
	@echo "==> sidecar: go build ./cmd/kivali-supervisor -> $(SIDECAR)"
	cd $(REPO) && $(GO_ENV) go build -trimpath -ldflags "-X main.version=$(DESKTOP_VERSION)" -o desktop-app/$(SIDECAR) ./cmd/kivali-supervisor
else
	@echo "==> sidecar: the FAKE supervisor (desktop-app/fake-supervisor) -> $(SIDECAR)"
	$(CARGO) build --release --target $(TRIPLE) --manifest-path fake-supervisor/Cargo.toml
	cp fake-supervisor/target/$(TRIPLE)/release/kivali-supervisor$(EXE) $(SIDECAR)
endif
ifneq ($(IS_DARWIN),)
	@# Virtualization.framework refuses a process without this entitlement.
	codesign --force --sign - --options runtime --entitlements src-tauri/Entitlements.plist $(SIDECAR)
endif

licenses: deps
	$(MAKE) -C $(REPO) web-deps
	@mkdir -p $(dir $(LICENSES))
	CARGO="$(CARGO)" sh $(REPO)/scripts/third-party-licenses.sh release $(TRIPLE) > $(LICENSES).part
	mv $(LICENSES).part $(LICENSES)

dev: deps sidecar
	KIVALI_CONFIG_DIR="$(DEV_CONFIG_DIR)" npx tauri dev

# A release build refuses development signing material: the updater key
# in .keys/ (private, or its public half still in tauri.conf.json) and
# the ad-hoc identity. ALLOW_DEV_KEYS=1 overrides, for a local trial only.
# build-debug stays permissive.
release-preflight:
	@# One version: the chart and every desktop
	@# file must agree (scripts/version-check.sh; never overridable).
	bash $(REPO)/scripts/version-check.sh
	@if [ "$(SUPERVISOR_KIND)" != real ] && [ -z "$(ALLOW_FAKE_SUPERVISOR)" ]; then \
		echo "refusing a release build with the fake supervisor (set ALLOW_FAKE_SUPERVISOR=1 to try one)" >&2; exit 1; fi
	@if [ -z "$(ALLOW_DEV_KEYS)" ]; then \
		p=""; \
		if [ -z "$$TAURI_SIGNING_PRIVATE_KEY" ]; then \
			p="$$p\n  - TAURI_SIGNING_PRIVATE_KEY is unset, so updates would be signed with the development key in .keys/"; \
		elif [ "$$TAURI_SIGNING_PRIVATE_KEY" = "$(UPDATER_KEY)" ] || { [ -f "$(UPDATER_KEY)" ] && [ "$$TAURI_SIGNING_PRIVATE_KEY" = "$$(cat "$(UPDATER_KEY)")" ]; }; then \
			p="$$p\n  - TAURI_SIGNING_PRIVATE_KEY is the development key in .keys/"; fi; \
		if [ -f "$(UPDATER_KEY).pub" ] && [ "$$(node -p "require('$(CURDIR)/src-tauri/tauri.conf.json').plugins.updater.pubkey")" = "$$(cat "$(UPDATER_KEY).pub")" ]; then \
			p="$$p\n  - tauri.conf.json's updater pubkey is the development key's (put the project owner's there)"; fi; \
		if [ -n "$(IS_DARWIN)" ]; then \
			if [ -z "$$APPLE_SIGNING_IDENTITY" ] || [ "$$APPLE_SIGNING_IDENTITY" = "-" ]; then \
				p="$$p\n  - the signing identity is ad-hoc (set APPLE_SIGNING_IDENTITY to the Developer ID Application identity)"; fi; \
			if { [ -z "$$APPLE_ID" ] || [ -z "$$APPLE_PASSWORD" ] || [ -z "$$APPLE_TEAM_ID" ]; } && { [ -z "$$APPLE_API_KEY" ] || [ -z "$$APPLE_API_ISSUER" ] || [ -z "$$APPLE_API_KEY_PATH" ]; }; then \
				p="$$p\n  - no notarization credentials, so the app would ship un-notarized (set APPLE_ID + APPLE_PASSWORD + APPLE_TEAM_ID, or APPLE_API_KEY + APPLE_API_ISSUER + APPLE_API_KEY_PATH)"; fi; \
		fi; \
		if [ -n "$$p" ]; then \
			printf "refusing a release build with development signing material:$$p\nset ALLOW_DEV_KEYS=1 to build anyway (never for distribution); make build-debug needs none of this\n" >&2; exit 1; fi; \
	fi

build: release-preflight deps sidecar licenses
	@if [ -z "$$TAURI_SIGNING_PRIVATE_KEY" ] && [ ! -f "$(UPDATER_KEY)" ]; then \
		echo "no updater key: set TAURI_SIGNING_PRIVATE_KEY or run npx tauri signer generate -w .keys/updater.key" >&2; exit 1; fi
	TAURI_SIGNING_PRIVATE_KEY="$${TAURI_SIGNING_PRIVATE_KEY:-$(UPDATER_KEY)}" \
	TAURI_SIGNING_PRIVATE_KEY_PASSWORD="$${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}" \
		npx tauri build $(VM_BUNDLE) $(LICENSES_BUNDLE)

build-debug: deps sidecar licenses
ifneq ($(DEBUG_NO_UPDATER),)
	@echo "==> no updater key: the debug bundle leaves out the updater artifacts"
endif
	TAURI_SIGNING_PRIVATE_KEY="$${TAURI_SIGNING_PRIVATE_KEY:-$(wildcard $(UPDATER_KEY))}" \
	TAURI_SIGNING_PRIVATE_KEY_PASSWORD="$${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}" \
		npx tauri build --debug --bundles $(DEBUG_BUNDLES) $(VM_BUNDLE) $(LICENSES_BUNDLE) $(DEBUG_NO_UPDATER)

# build-trial is the release profile without the release: no updater
# artifacts, no signing material checked, under src-tauri/target/release/
# bundle. It is what to install for a trial on a machine, where a debug
# bundle will not do: the debug Kivali.exe is a console program (its log
# lines go to a console window, which closes the app when it is closed),
# the release one a windowed program whose lines go to logs/shell.log.
# Never for distribution: a release is `make build` with the keys.
build-trial: deps sidecar licenses
	npx tauri build $(VM_BUNDLE) $(LICENSES_BUNDLE) --config '{"bundle":{"createUpdaterArtifacts":false}}'

# The end-to-end test drives the fake supervisor's debug build, and the
# real one too when KIVALI_REAL_SUPERVISOR names its binary.
test: deps sidecar
	$(CARGO) build --manifest-path fake-supervisor/Cargo.toml
	$(CARGO) test --manifest-path fake-supervisor/Cargo.toml
	$(CARGO) test --manifest-path src-tauri/Cargo.toml
	$(NPM) test

lint: deps sidecar
	$(NPM) run typecheck
	@$(CARGO) clippy --version >/dev/null 2>&1 || { echo "lint: clippy is not installed (rustup component add clippy)" >&2; exit 1; }
	@# -D warnings after `--` fails on compiler warnings as well as clippy's.
	@# Not RUSTFLAGS: a different RUSTFLAGS rebuilds every dependency, and
	@# the shell's are Tauri's (on Linux, ~530 crates).
	$(CARGO) clippy --all-targets --manifest-path src-tauri/Cargo.toml -- -D warnings
	RUSTFLAGS="-D warnings" $(CARGO) check --manifest-path fake-supervisor/Cargo.toml

# Not part of lint: the Rust tree is not rustfmt-formatted.
fmt-check:
	$(CARGO) fmt --manifest-path src-tauri/Cargo.toml -- --check

# The app icon is the official Kivali icon (web/public/logos) on the
# 1024px canvas with the margin macOS icons keep (src-tauri/icons/
# source.svg); `tauri icon` derives every size from source.png. The
# output is committed; rerun only when the logo changes. The tray icons
# (four states, light and dark inks, the starting pulse's frames; a
# 40x44 menu-bar set and a 32x32 notification-area set) are rendered
# from SVG by scripts/make-icons.mjs, also with rsvg-convert.
ICONS := src-tauri/icons
icons:
	@command -v rsvg-convert >/dev/null 2>&1 || { echo "icons: rsvg-convert not found (brew install librsvg)" >&2; exit 1; }
	cp $(REPO)/web/public/logos/kivali-icon.svg $(ICONS)/kivali-icon.svg
	rsvg-convert -w 1024 -h 1024 $(ICONS)/source.svg -o $(ICONS)/source.png
	rm -f $(ICONS)/kivali-icon.svg
	npx tauri icon $(ICONS)/source.png -o $(ICONS)
	rm -rf $(ICONS)/android $(ICONS)/ios $(ICONS)/Square*.png $(ICONS)/StoreLogo.png
	node scripts/make-icons.mjs

clean:
	rm -rf dist src-tauri/target src-tauri/gen src-tauri/binaries fake-supervisor/target
