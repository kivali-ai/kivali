# Recipes rely on bash features (multi-line `if [ -f ... ]; fi`,
# $(openssl rand), heredocs). Without this override, make on Windows
# defaults to cmd.exe and every such recipe breaks. `bash` must be on
# PATH — Git Bash / WSL / any *nix satisfies this.
SHELL := bash

# Operator-local overrides, set once per machine and never committed
# (gitignored). Included first so its assignments win over the `?=`
# defaults below. Typical contents:
#   DEV_SHELL_EXTRA_PACKAGES = ffmpeg postgresql-client build-essential golang-go
-include local.mk

# An inherited GOROOT is almost always a stale one.
#
# goenv, asdf, and hand-rolled `export GOROOT` lines in a shell profile
# put GOROOT into the environment of every shell. It goes stale the
# moment `go` on PATH starts coming from somewhere else — a Homebrew
# upgrade is enough. The go command trusts the environment over its own
# location, finds a mismatched compiler under the stale GOROOT, and
# refuses:
#
#     compile: version "go1.24.4" does not match go tool version "go1.26.2"
#
# once per package, which is how a one-line environment problem arrives
# as a hundred-line wall in the middle of `make build`.
#
# Since Go 1.9 the toolchain locates its own GOROOT from the binary's
# path, so the fix is to stop handing ours down. This covers the tools
# make shells out to as well.
#
# Note this only cleans the environment make passes to its recipes. A
# plain `go build` typed into the same shell still breaks; fix that at
# the source by dropping GOROOT from the shell profile.
unexport GOROOT

BINARY := kivali
PKG    := ./...

# The golangci-lint CI installs, and therefore the only version whose
# verdict gates a release. `make lint` warns (but does not fail) when
# the binary on PATH disagrees, so a green local run that CI will
# reject is visible at the point it happens rather than at tag time.
# Bump here and in .github/workflows/ci.yml together.
GOLANGCI_VERSION := v2.13.2

# The same for shellcheck, which renames findings between releases (an
# uncalled function is SC2317 before 0.11, SC2329 from it). Bump here and
# in ci.yml together.
SHELLCHECK_VERSION := v0.11.0

# VERSION: `git describe` when available, "dev" as ultimate fallback.
# Lands in the Go binary via -ldflags="-X main.version=$(VERSION)" and
# drives image tags. Override on the command line to force a tag:
#   make images VERSION=v0.1.2
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Image names. REGISTRY prefixes all three (include the trailing slash)
# once a registry exists; empty, the tags are the bare local names.
# Nothing here pushes: a release ships the images as archives.
REGISTRY         ?=
IMAGE            ?= $(REGISTRY)kivali
EGRESS_IMAGE     ?= $(REGISTRY)kivali-egress-proxy
DEV_SHELL_IMAGE  ?= $(REGISTRY)kivali-dev-shell

# Apt packages baked into the dev-shell image on top of its small
# default set (see Dockerfile.dev-shell). Space-separated:
#   make images DEV_SHELL_EXTRA_PACKAGES="ffmpeg postgresql-client"
DEV_SHELL_EXTRA_PACKAGES ?=

# Where the per-architecture image archives of a release are written.
IMAGES_DIR ?= dist/images

# ENGINE is where image builds run: `vm`, BuildKit in a Kivali VM on
# this machine (scripts/build-vm.sh; no Docker needed; the default on
# macOS and Windows), or `docker`, this machine's Docker (the default
# on Linux, which has no Kivali VM; what CI uses). The release targets
# (image-tars, release-bundle-*) are Docker only: they build both
# architectures, under emulation for the foreign one.
ENGINE ?= $(if $(filter Linux,$(shell uname -s)),docker,vm)
ifeq ($(filter $(ENGINE),vm docker),)
$(error ENGINE must be vm or docker (got '$(ENGINE)'))
endif

# =====================================================================
# The targets to type. `make help` lists them (the `##` comments); every
# other rule below is a step one of them, CI or a release runs.
# =====================================================================

.DEFAULT_GOAL := help

.PHONY: help build test ci licenses nightly run dev-vm dev-up dev-load test-vm test-mac \
        images release release-assets api-types ds-sync visual-compare clean

help: ## this list
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-15s %s\n", $$1, $$2}'

# The binary embeds the web app (internal/web/ui/dist), so a build
# builds the frontend first. A bare `go build` still compiles without
# it; every app route then answers 503 saying the frontend is not built.
build: web-build ## the kivali binary, web app embedded, into bin/
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) .

# The quick loop while working: Go unit tests and lint, the web app's
# lint and tests, the wire types. A minute or two. `make ci` is the full
# set CI enforces.
test: unit-test lint web-lint web-test api-types-check ## quick check while working (Go + web)

# What the workflows run, one target per job, so a green `make ci` here
# is a green ci.yml there. The workflows only set up tools (Go, Node,
# Rust, helm, golangci-lint, `make web-install`, Playwright's Chromium)
# and call these. Nothing is skipped for a missing tool: helm-lint fails
# without helm, and lint fails without shellcheck here.
#
#   ci-check    ci.yml's check job
#   ci-e2e      ci.yml's e2e job
#   ci-desktop     ci.yml's desktop jobs (on this machine's OS)
#   license-check  ci.yml's licenses job
ci: ci-check ci-e2e ci-desktop license-check ## everything CI runs on every commit (needs helm, shellcheck, cargo, jq)

# The one command after a dependency change: the release's notices
# regenerated (third-party-licenses) and every shipped dependency held
# to scripts/license-policy.txt (license-check).
licenses: ## regenerate the license notices + check the license policy
	$(MAKE) third-party-licenses
	$(MAKE) license-check
	@echo "licenses: notices in dist/licenses/THIRD_PARTY_LICENSES.txt"

# integration-test needs a disposable linux/arm64 k3s; on a Mac, run it
# in the test VM (make test-vm SUITE=integration).
nightly: race-test integration-test ## what nightly.yml runs: race detector + integration suite

# A team on http://127.0.0.1:$(DEMO_PORT)/: seeds a temp dir, starts the
# server in DEV_MODE and one agent runtime per agent. No auth, no
# cluster. Ctrl-C stops everything; the temp dir is left for inspection.
#
#   DEMO_SCENARIO  demo (default: realistic data on every screen), empty
#                  (setup done, nothing else) or setup (the setup wizard)
#   CLAUDE         fake (default: cmd/fake-claude answers, no sign-in) or
#                  real: the `claude` CLI on your PATH, billed to
#                  whatever it is signed in to (the server refuses
#                  ANTHROPIC_API_KEY and other credential variables)
#
# Agent pods do not exist here: KIVALI_MCP_LOCAL_FILES has `kivali mcp`
# run the file_* tools and run_shell in-process against $$root/files.
DEMO_PORT ?= 8080
DEMO_SCENARIO ?= demo
CLAUDE ?= fake
run: web-deps web-build e2e-bins ## a team locally, no auth (DEMO_SCENARIO=demo|empty|setup, CLAUDE=fake|real)
	@set -euo pipefail; \
	case "$(CLAUDE)" in fake|real) ;; *) echo "run: CLAUDE must be fake or real (got '$(CLAUDE)')" >&2; exit 1 ;; esac; \
	root=$$(mktemp -d /tmp/kv-demo-XXXXXX); \
	bin/devseed -data "$$root/data" -scenario $(DEMO_SCENARIO); \
	export DEV_MODE=true ADDR=127.0.0.1:$(DEMO_PORT) DATA_DIR="$$root/data" AGENTPOD_UDS_DIR="$$root/uds"; \
	mkdir -p "$$root/files"; \
	export KIVALI_MCP_LOCAL_FILES=1 KIVALI_MCP_LOCAL_FILES_ROOT="$$root/files"; \
	if [ "$(CLAUDE)" = fake ]; then \
		mkdir -p "$$root/home"; \
		export HOME="$$root/home" PATH="$(CURDIR)/bin/fakebin:$$PATH"; \
	fi; \
	pids=(); trap 'kill $${pids[@]} 2>/dev/null; wait' EXIT INT TERM; \
	bin/kivali-e2e & pids+=($$!); \
	slugs=$$(ls "$$root/data/agents" 2>/dev/null || true); \
	for s in chief-of-staff $$slugs; do \
		case "$$s" in ceo|_archived|.*) continue ;; esac; \
		[ -d "$$root/scratch/$$s" ] && continue; \
		mkdir -p "$$root/scratch/$$s"; \
		bin/kivali-e2e agent --slug "$$s" --uds "$$root/uds/core.sock" --scratch "$$root/scratch/$$s" & pids+=($$!); \
	done; \
	echo "Kivali demo: http://127.0.0.1:$(DEMO_PORT)/  (data: $$root/data)"; \
	wait

# ---- the dev team (docs/developers/supervisor.md) -------------------------------
#
# The dev team is a team like any other, run by the supervisor on this
# Mac from a config directory of its own (DEV_CONFIG_DIR), so it never
# mixes with the teams Kivali Desktop runs:
#
#   dev-vm    build the three :dev images and a dev chart, and bake both
#             into the VM image (vm/build/out). Once, and again when the
#             chart or the VM changes.
#   dev-up    boot the dev team from that image; the first one installs
#             it, for DEV_OWNER (the Google account that signs in)
#   dev-load  the loop: rebuild the :dev images and import them into the
#             running dev team, cycling its agent pods and server
#
# Everything else is the supervisor's own CLI, $(DEV_SUP) <command>:
# `down --exit` stops the VM (its data stays), `destroy --yes --exit`
# deletes the team and its data disk, `terminal` opens Claude Code in the
# server container (`terminal --shell` for bash), `backup --out <zip>`
# and `restore` move its data.
# The supervisor binary: on Windows (make under Git Bash, where the
# environment carries OS=Windows_NT) an .exe without cgo, signed by
# nothing; on macOS a cgo binary (Virtualization.framework) ad-hoc
# signed with the virtualization entitlement.
ifeq ($(OS),Windows_NT)
SUPERVISOR_BIN := bin/kivali-supervisor.exe
SUPERVISOR_CGO := 0
SUPERVISOR_SIGN := true
else
SUPERVISOR_BIN := bin/kivali-supervisor
SUPERVISOR_CGO := 1
SUPERVISOR_SIGN := codesign --force --entitlements vm/boottest/entitlements.plist -s - $(SUPERVISOR_BIN)
endif
DEV_CONFIG_DIR ?= $(HOME)/.kivali-dev
DEV_OWNER ?=
DEV_SUP = $(SUPERVISOR_BIN) --config-dir $(DEV_CONFIG_DIR)
DEV_CHART_VERSION := 0.0.0-dev
DEV_CHART := dist/dev/kivali-$(DEV_CHART_VERSION).tgz

dev-vm: dev-images dev-chart ## build the dev team's VM image (:dev images + chart baked in)
	$(MAKE) -C vm dev-images
	$(MAKE) -C vm image KIVALI_CHART=$(abspath $(DEV_CHART))

dev-up: supervisor ## boot the dev team (DEV_OWNER on first install)
	$(DEV_SUP) up --vm-dir vm/build/out $(if $(DEV_OWNER),--owner $(DEV_OWNER))

# The build runs in the dev team's own VM (ENGINE=vm), straight into the
# containerd the team runs on, so nothing is saved or imported; then
# the agent pods and the server are restarted onto the new images.
dev-load: supervisor ## rebuild the :dev images into the running dev team
	KIVALI_BUILD_VM_DIR=$(DEV_CONFIG_DIR) KIVALI_BUILD_VM_BOOT=0 $(MAKE) images VERSION=dev ENGINE=vm
	$(DEV_SUP) restart

# test-vm runs the Go suites on Linux inside a Kivali VM on this machine
# (scripts/test-vm.sh): SUITE=unit (default; PKG and TESTFLAGS pass
# through) or SUITE=integration, which installs this tree into the VM's
# own k3s. Inside the VM it runs the same targets as natively. The test
# VM is apart from the dev team, booted for each run and stopped after
# it (a running VM never gives memory back); its disk, with Go's caches,
# is kept: `scripts/test-vm.sh destroy` deletes it.
SUITE ?= unit
test-vm: supervisor ## Go suites on Linux in a VM on this machine (SUITE=unit|integration)
	PKG='$(PKG)' TESTFLAGS='$(TESTFLAGS)' scripts/test-vm.sh $(SUITE)

# test-mac is what only a Mac with Virtualization.framework can test (CI
# cannot: GitHub's hosted Macs have no nested virtualization): the
# platform tests, then a VM image built from this tree booting and
# stopping cleanly. Builds the VM image (Docker); run by hand before a
# release.
test-mac: platform-test ## Mac-only checks: supervisor + shell tests, VM image boots
	$(MAKE) -C vm image
	$(MAKE) -C vm boottest

# Builds the three images (server, egress-proxy, dev-shell) at VERSION,
# for this machine's architecture: with ENGINE=vm into the build VM's
# containerd (scripts/build-vm.sh; KIVALI_BUILD_VM_DIR names another
# VM, the dev team's for dev-load), with ENGINE=docker into the local
# docker.
BUILD_VM := scripts/build-vm.sh
BUILD_VM_DOWN := $(if $(KIVALI_BUILD_VM_KEEP),true,$(BUILD_VM) down)
images: ## the three images into the build VM or local docker (VERSION, DEV_SHELL_EXTRA_PACKAGES)
	@echo "dev-shell extra packages: $(if $(strip $(DEV_SHELL_EXTRA_PACKAGES)),$(DEV_SHELL_EXTRA_PACKAGES),none)"
ifeq ($(ENGINE),docker)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .
	docker build -f Dockerfile.egress-proxy -t $(EGRESS_IMAGE):$(VERSION) .
	docker build --build-arg VERSION=$(VERSION) --build-arg DEV_SHELL_EXTRA_PACKAGES="$(DEV_SHELL_EXTRA_PACKAGES)" -f Dockerfile.dev-shell -t $(DEV_SHELL_IMAGE):$(VERSION) .
else
	$(BUILD_VM) up
	$(BUILD_VM) put-tree . src
	$(BUILD_VM) build $(BUILD_VM_CTX) --opt build-arg:VERSION=$(VERSION) --output type=image,name=docker.io/library/$(IMAGE):$(VERSION),unpack=true
	$(BUILD_VM) build $(BUILD_VM_CTX) --opt filename=Dockerfile.egress-proxy --output type=image,name=docker.io/library/$(EGRESS_IMAGE):$(VERSION),unpack=true
	$(BUILD_VM) build $(BUILD_VM_CTX) --opt filename=Dockerfile.dev-shell --opt build-arg:VERSION=$(VERSION) --opt 'build-arg:DEV_SHELL_EXTRA_PACKAGES=$(DEV_SHELL_EXTRA_PACKAGES)' --output type=image,name=docker.io/library/$(DEV_SHELL_IMAGE):$(VERSION),unpack=true
	$(BUILD_VM_DOWN)
endif
# The tree streamed in by put-tree, as a buildctl context (the
# dockerfile is found in it by name).
BUILD_VM_CTX := --frontend dockerfile.v0 --local context=/var/lib/kivali/build/ctx/src --local dockerfile=/var/lib/kivali/build/ctx/src

# release cuts a new Kivali release by bumping the release version
# (the chart's appVersion and the desktop app's), committing the bump,
# creating an annotated tag at the release commit, and pushing commit +
# tag to origin. It does NOT build anything: the tag starts the release
# workflow, which builds and publishes every asset (docs/developers/releasing.md).
release: require-explicit-version ## cut a release: bump, commit, tag, push (VERSION=vX.Y.Z)
	bash scripts/release.sh $(VERSION)

# The GitHub release's Linux assets, built locally with the same targets
# release.yml's jobs run: dist/kivali-images-linux-<arch>.tar.zst for
# RELEASE_ARCHES (a foreign architecture builds under QEMU: slow; limit
# with RELEASE_ARCHES=arm64), kivali-X.Y.Z.tgz and release.json. Then
# `make vm-release` and `make desktop-release` for the VM image and the
# app, as the release's vm and desktop jobs do.
RELEASE_ARCHES ?= amd64 arm64
release-assets: require-release-version require-explicit-version $(addprefix release-bundle-,$(RELEASE_ARCHES)) ## build the release's Linux assets into dist/ (VERSION=vX.Y.Z)
	$(MAKE) chart-package VERSION=$(VERSION)
	$(MAKE) release-feed VERSION=$(VERSION)

# ---- API wire types ------------------------------------------------
#
# internal/web/apitypes is the JSON contract. tygo (a Go tool
# dependency, `go tool tygo`; config in tygo.yaml) writes its
# TypeScript to web/src/api/types.gen.ts, and the apitypes golden test
# writes one JSON fixture per response type to
# internal/web/apitypes/testdata, which the web tests type-check
# against the generated types. Run after any apitypes change.
API_TYPES_OUT := web/src/api/types.gen.ts internal/web/apitypes/testdata

api-types: ## regenerate the TypeScript wire types and fixtures (after an apitypes change)
	go tool tygo generate
	go test ./internal/web/apitypes -run '^TestGolden$$' -count=1 -update

# Copies the design system's CSS, tokens, fonts and logos into the web
# workspace. design-system/ is vendored verbatim and never edited; the
# copies under web/ are what the app imports and serves. Rerun after
# scripts/ds-import.sh brings in a new drop.
WEB_DIR := web
ds-sync: ## copy the vendored design system into web/
	mkdir -p $(WEB_DIR)/src/ds/tokens $(WEB_DIR)/src/ds/fonts $(WEB_DIR)/public/logos
	cp design-system/components/kivali.css $(WEB_DIR)/src/ds/kivali.css
	cp design-system/components/text.css $(WEB_DIR)/src/ds/text.css
	cp design-system/tokens/*.css $(WEB_DIR)/src/ds/tokens/
	cp -R design-system/fonts/. $(WEB_DIR)/src/ds/fonts/
	cp design-system/assets/logos/*.svg design-system/assets/logos/*.png $(WEB_DIR)/public/logos/

# Visual baselines are per platform, and CI is linux/amd64, so they are
# made inside the official Playwright image of the version the lockfile
# pins, against linux builds of the three binaries. Baselines are NEVER
# committed (web/e2e/__screenshots__/ is git-ignored): this generates them
# on BASE (default the parent commit) in a temporary worktree, then
# compares this checkout against them (scripts/visual-compare.sh). Works
# from any checkout: web deps are installed when missing.
BASE ?= HEAD~1
visual-compare: ## screenshot diff of this checkout against BASE (default HEAD~1)
	scripts/visual-compare.sh $(BASE)

# Everything a build writes: binaries, dist/, the embedded web build,
# the e2e results, the VM image (vm/build) and the desktop app's build
# (desktop-app: Rust target dirs, so the next build is a long one).
clean: ## remove every build output (bin, dist, web, VM image, desktop app)
	rm -rf bin dist web/dist web/e2e/test-results* web/e2e/playwright-report
	find internal/web/ui/dist -mindepth 1 ! -name .gitkeep -delete
	$(MAKE) -C vm clean
	$(MAKE) -C desktop-app clean

# =====================================================================
# Steps. Not listed by `make help`; the targets above, CI, the release
# workflow and the docs call them.
# =====================================================================

.PHONY: vet lint tidy-check version-check cross-check unit-test race-test integration-test \
        ci-check ci-e2e ci-desktop supervisor-test platform-test \
        web-install require-web-deps web-build web-lint web-test web-deps \
        e2e-bins e2e-bins-linux web-e2e web-e2e-baselines web-e2e-visual-check api-types-check \
        require-release-version require-explicit-version image-tars release-feed \
        vm-release desktop-release third-party-licenses license-check helm-lint helm-template chart-package \
        supervisor dev-images dev-chart

# ---- Go ------------------------------------------------------------

vet:
	go vet $(PKG)

# Fails loud if go.mod / go.sum drifted out of tidy — belongs on CI so
# a missed `go mod tidy` lights up before it ships.
tidy-check:
	go mod tidy
	git diff --exit-code go.mod go.sum

# golangci-lint must be on PATH (docs/developers/README.md), at
# GOLANGCI_VERSION; one way to install it:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
# `fmt --diff` exits non-zero on any gofmt drift (formatters live in a
# separate v2 namespace from linters, so `run` doesn't catch them).
lint:
	@golangci-lint version 2>/dev/null | grep -q ' $(GOLANGCI_VERSION:v%=%) ' || \
		echo "lint: golangci-lint on PATH is not $(GOLANGCI_VERSION) (what CI runs) — results may differ"
	golangci-lint run $(PKG)
	golangci-lint fmt --diff $(PKG)
	@# Second pass with the integration build tag. Without it the
	@# `//go:build integration` files are invisible to the linter, so
	@# the suite that gates a release was itself never checked.
	golangci-lint run --build-tags=integration $(PKG)
	@# shellcheck on every shell script under scripts/, so a script bug
	@# (set -u with an empty array expansion, say) surfaces in CI rather
	@# than mid-release. Skipped (with a warning) if shellcheck isn't on
	@# PATH so contributors without it can still `make lint`; under
	@# `make ci-check` (REQUIRE_SHELLCHECK) a missing shellcheck fails.
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck --version | grep -q '^version: $(SHELLCHECK_VERSION:v%=%)$$' || \
			echo "lint: shellcheck on PATH is not $(SHELLCHECK_VERSION) (what CI runs) — results may differ"; \
		find scripts -name '*.sh' -type f -print0 | xargs -0 shellcheck; \
	elif [ -n "$(REQUIRE_SHELLCHECK)" ]; then \
		echo "lint: shellcheck not on PATH (brew install shellcheck)" >&2; exit 1; \
	else \
		echo "lint: shellcheck not on PATH; skipping shell scripts (brew install shellcheck)"; \
	fi

# Unit + service-level tests. No build tag; run on every change.
# PKG narrows the packages and TESTFLAGS adds go test flags, here and in
# `make test-vm`, which runs this same target inside a Kivali VM:
#   make unit-test PKG=./internal/web TESTFLAGS='-run TestX -count=1'
unit-test:
	go test $(TESTFLAGS) $(PKG)

# The same tests under the race detector. Several times slower than
# `unit-test`, so it runs nightly, not in the quick loop. Not optional:
# much of the server is concurrent, and a data race there is exactly the
# kind of bug that passes a thousand serial runs and then corrupts state
# under a real fleet.
race-test:
	go test -race -count=1 $(PKG)

# The deployed server, installed the way kivali-supervisor installs it
# (integration_test.go), into the VM's k3s run directly on a disposable
# linux/arm64 host: CI's runner, via scripts/ci-k3s.sh. Slow; tagged
# `integration` so it does not run as part of `unit-test`. It refuses
# to run unless KIVALI_IT_DISPOSABLE_K3S=1, because it wipes what it
# installed on every run.
#
#   KIVALI_IT_DISPOSABLE_K3S=1 scripts/ci-k3s.sh up
#   KIVALI_IT_DISPOSABLE_K3S=1 make integration-test
integration-test:
	HELM="$(HELM)" go test -tags=integration -count=1 -v -timeout=20m .

# The supervisor's OS-specific code sits behind build tags, one
# implementation per OS (docs/developers/supervisor.md, "Platform boundary"), so
# the whole module is built for every OS and the supervisor vetted (vet
# compiles test files, and some server tests are Unix-only). The cgo
# macOS build with the Virtualization.framework backend is `make
# supervisor`, on a Mac.
cross-check:
	@for os in windows linux darwin; do \
		echo "== GOOS=$$os"; \
		CGO_ENABLED=0 GOOS=$$os go build ./... || exit 1; \
		CGO_ENABLED=0 GOOS=$$os go vet ./internal/supervisor/... ./cmd/kivali-supervisor/... || exit 1; \
	done

# ---- CI jobs (see `ci` above) -------------------------------------

ci-check: REQUIRE_SHELLCHECK = 1
ci-check: version-check vet tidy-check lint unit-test web-lint web-test api-types-check cross-check helm-lint helm-template

ci-e2e: web-e2e

# Kivali Desktop on the OS that runs it (ci.yml's macOS and Windows
# desktop jobs): the supervisor tests, then the shell's lint and tests.
ci-desktop: supervisor-test
	$(MAKE) -C desktop-app lint test

# The supervisor with its native backend compiled in: Virtualization.
# framework through cgo on macOS, Hyper-V (pure Go) on Windows.
supervisor-test:
	CGO_ENABLED=$(if $(filter Windows_NT,$(OS)),0,1) go test -count=1 ./internal/supervisor/... ./cmd/kivali-supervisor/...

# The supervisor and the desktop shell, tested on the OS that runs them
# (test-mac).
platform-test: supervisor-test
	$(MAKE) -C desktop-app test

# ---- Kivali web app ----------------------------------------------
#
# web/ is the frontend workspace (React, Vite, TypeScript). Its build
# lands in internal/web/ui/dist, which the Go binary embeds and serves
# at the site root. web/package-lock.json is committed; `web-install` is `npm
# ci`, so every machine and the image build get the same tree.
#
# These targets do not skip when dependencies are
# missing: a skipped frontend lint or test reads as a pass, and the
# frontend is part of the product. require-web-deps fails loudly.

web-install:
	cd $(WEB_DIR) && npm ci

require-web-deps:
	@[ -d $(WEB_DIR)/node_modules ] || { \
		echo "$(WEB_DIR)/node_modules is missing: run 'make web-install' first" >&2 ; exit 1 ; }

web-deps:
	@[ -d $(WEB_DIR)/node_modules ] || $(MAKE) web-install

web-build: require-web-deps
	cd $(WEB_DIR) && npm run build

web-lint: require-web-deps
	cd $(WEB_DIR) && npm run lint
	cd $(WEB_DIR) && npm run typecheck

web-test: require-web-deps
	cd $(WEB_DIR) && npm run test

# Regenerates and fails if that changed anything: the committed types
# or fixtures were stale. Compares against a copy taken first, so it
# gives the same answer on a dirty tree as on CI. On failure the files
# are left regenerated, ready to commit.
api-types-check:
	@tmp=$$(mktemp -d) ; trap 'rm -rf "$$tmp"' EXIT ; \
	mkdir -p "$$tmp/before" ; \
	for p in $(API_TYPES_OUT) ; do \
		if [ -e "$$p" ] ; then mkdir -p "$$tmp/before/$$(dirname $$p)" ; cp -R "$$p" "$$tmp/before/$$p" ; fi ; \
	done ; \
	$(MAKE) --no-print-directory api-types >/dev/null || exit 1 ; \
	stale=0 ; \
	for p in $(API_TYPES_OUT) ; do \
		diff -r "$$tmp/before/$$p" "$$p" >/dev/null 2>&1 || { echo "api-types-check: $$p was stale (now regenerated; commit it)" >&2 ; stale=1 ; } ; \
	done ; \
	exit $$stale

# ---- end-to-end ---------------------------------------------------
#
# Playwright (web/e2e) against the real binary in DEV_MODE. Chat turns
# run through agent runtimes (`<binary> agent`), each spawning
# cmd/fake-claude, which is built under the name `claude` in
# bin/fakebin so a PATH prefix is all it takes to stand in for the
# CLI. cmd/devseed builds the DATA_DIR. See web/e2e/README.md.
#
# The server binary embeds internal/web/ui/dist, so it is rebuilt after
# web-build every time: a stale bin/kivali-e2e would test an old UI.
e2e-bins:
	go build -o bin/kivali-e2e .
	go build -o bin/devseed ./cmd/devseed
	go build -o bin/fakebin/claude ./cmd/fake-claude

web-e2e: require-web-deps web-build e2e-bins
	cd $(WEB_DIR) && npm run test:e2e

# The visual-compare steps (scripts/visual-compare.sh runs them).
PLAYWRIGHT_VERSION = $(shell node -p "require('./$(WEB_DIR)/node_modules/@playwright/test/package.json').version")
PLAYWRIGHT_DOCKER = docker run --rm --platform linux/amd64 --ipc=host \
	-v "$(CURDIR)":/work -w /work/$(WEB_DIR) \
	-e KIVALI_VISUAL=1 -e KIVALI_E2E_BIN_DIR=/work/bin/linux -e CI=1 \
	mcr.microsoft.com/playwright:v$(PLAYWRIGHT_VERSION)-noble \
	npx playwright test -c e2e/playwright.config.ts visual.spec.ts --output=e2e/test-results-linux

e2e-bins-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/linux/kivali-e2e .
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/linux/devseed ./cmd/devseed
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/linux/fakebin/claude ./cmd/fake-claude

# Generates baselines into web/e2e/__screenshots__/linux/.
web-e2e-baselines: web-deps web-build e2e-bins-linux
	$(PLAYWRIGHT_DOCKER) --update-snapshots=all

# Compares against the baselines already in web/e2e/__screenshots__/linux/.
# Report: web/playwright-report/; diffs: web/e2e/test-results-linux/.
web-e2e-visual-check: web-deps web-build e2e-bins-linux
	$(PLAYWRIGHT_DOCKER)

# ---- release ----------------------------------------------------------
#
# release.yml runs one of these per job; `make release-assets` runs the
# Linux ones locally. docs/developers/releasing.md is the procedure.
#
#   release-bundle-<arch>  dist/kivali-images-linux-<arch>.tar.zst (images job)
#   chart-package          dist/kivali-X.Y.Z.tgz (chart job)
#   release-feed           dist/release.json (feed job)
#   vm-release             the VM image for VM_ARCH (arm64, or amd64 for
#                          Windows) with that architecture's images and
#                          the chart baked in (vm jobs)
#   desktop-release        this platform's installer and updater artifacts
#                          (macOS: signed and notarized), when the signing
#                          environment is present (desktop-macos and
#                          desktop-windows jobs)
#   third-party-licenses   dist/licenses/THIRD_PARTY_LICENSES.txt
#                          (licenses job)

# require-release-version refuses non-semver tags, so a dirty-tree or
# "dev" VERSION never ends up in a release asset.
require-release-version:
	@if ! echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$'; then \
		echo "release: VERSION must be vMAJOR.MINOR.PATCH (got: $(VERSION))" >&2; \
		echo "  build targets accept any VERSION; release targets require a semver tag" >&2; \
		exit 1; \
	fi

# require-explicit-version refuses recipes that inherit VERSION
# from the git-describe default rather than a command-line
# argument. Catches the common typo
#     make release v0.2.5
# where `v0.2.5` is a positional target and VERSION silently
# defaults to `git describe` output (e.g. v0.2.4-8-gsha), which
# then gets half-applied before failing deeper in the flow.
#
# Uses `$(origin VERSION)` (make builtin): returns "command line"
# only when the user passed VERSION=... explicitly.
require-explicit-version:
	@if [ "$(origin VERSION)" != "command line" ]; then \
		echo "error: VERSION must be passed as key=value, not positional." >&2 ; \
		echo "  expected:  make <target> VERSION=vX.Y.Z" >&2 ; \
		echo "  got:       VERSION defaulted to '$(VERSION)' from git describe." >&2 ; \
		echo "            (if you typed something like 'make release v0.2.5', the v0.2.5" >&2 ; \
		echo "             was parsed as a separate target, not the version.)" >&2 ; \
		exit 1 ; \
	fi

# version-check asserts every file that carries the release version says
# the same thing (scripts/version-check.sh lists them): the chart's
# appVersion and the desktop app's tauri.conf.json,
# Cargo.toml/lock and package.json/lock. With VERSION=vX.Y.Z on the
# command line they must equal it (the release workflow passes the
# pushed tag); without, they must agree with each other. CI runs it on
# every push; release.sh runs it before and after the bump.
version-check:
	bash scripts/version-check.sh $(if $(filter command line,$(origin VERSION)),$(VERSION))

# image-tars builds all three images for ARCH (amd64 or arm64) and writes
# one docker-archive tarball per image: $(IMAGES_DIR)/<name>-<arch>.tar,
# tagged $(IMAGE):$(VERSION) etc. so `docker load -i` makes them usable.
# A non-native architecture builds under QEMU/binfmt (the Go and web
# stages still run natively and cross-compile; only the runtime stages'
# package installs are emulated).
image-tars:
ifeq ($(filter $(ARCH),amd64 arm64),)
	$(error image-tars: ARCH must be amd64 or arm64 (got '$(ARCH)'))
endif
	mkdir -p $(IMAGES_DIR)
	docker buildx build --platform linux/$(ARCH) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) -o type=docker,dest=$(IMAGES_DIR)/kivali-$(ARCH).tar .
	docker buildx build --platform linux/$(ARCH) -f Dockerfile.egress-proxy \
		-t $(EGRESS_IMAGE):$(VERSION) -o type=docker,dest=$(IMAGES_DIR)/kivali-egress-proxy-$(ARCH).tar .
	docker buildx build --platform linux/$(ARCH) --build-arg VERSION=$(VERSION) \
		--build-arg DEV_SHELL_EXTRA_PACKAGES="$(DEV_SHELL_EXTRA_PACKAGES)" -f Dockerfile.dev-shell \
		-t $(DEV_SHELL_IMAGE):$(VERSION) -o type=docker,dest=$(IMAGES_DIR)/kivali-dev-shell-$(ARCH).tar .

# One prerequisite target per architecture (rather than a shell loop that
# calls $(MAKE)) so that `make -n release-assets` is a true dry run: make
# executes any recipe line containing $(MAKE) even under -n. Needs zstd.
release-bundle-%: require-release-version require-explicit-version
	mkdir -p dist
	$(MAKE) image-tars ARCH=$* VERSION=$(VERSION)
	bash scripts/image-bundle.sh $(VERSION) $* --images-dir $(IMAGES_DIR) --out dist

# release.json, the supervisor's update feed, from the chart and the
# image bundles already in dist/.
release-feed: require-release-version
	bash scripts/release-json.sh $(VERSION) --dir dist

# The third-party license notices of a whole release, one file, from
# this checkout (scripts/third-party-licenses.sh): the release asset, and
# what Kivali Desktop bundles. Each image also writes its own as it is
# built.
# Needs the web and desktop-app npm installs, go, cargo and jq.
third-party-licenses: require-web-deps
	$(MAKE) -C desktop-app deps
	CARGO="$$(command -v cargo || echo $(HOME)/.cargo/bin/cargo)" sh scripts/third-party-licenses.sh all dist/licenses

# Every third-party component a release ships, held to the license
# policy in scripts/license-policy.txt (scripts/license-check.sh); the
# same components third-party-licenses lists. Same needs.
license-check: require-web-deps
	$(MAKE) -C desktop-app deps
	CARGO="$$(command -v cargo || echo $(HOME)/.cargo/bin/cargo)" sh scripts/license-check.sh

# The VM image for VM_ARCH: arm64 for macOS (Virtualization.framework),
# amd64 for Windows (Hyper-V; adds root.vhdx). Built on a machine of that
# architecture.
VM_ARCH ?= arm64

vm-release: require-release-version require-explicit-version
	@test -f dist/kivali-$(VERSION:v%=%).tgz || { echo "vm-release: dist/kivali-$(VERSION:v%=%).tgz missing (make chart-package VERSION=$(VERSION))" >&2; exit 1; }
	@test -f $(IMAGES_DIR)/bundle-$(VM_ARCH)/kivali-images.tar || { echo "vm-release: $(IMAGES_DIR)/bundle-$(VM_ARCH)/kivali-images.tar missing (make release-assets VERSION=$(VERSION) RELEASE_ARCHES=$(VM_ARCH))" >&2; exit 1; }
	$(MAKE) -C vm image ARCH=$(VM_ARCH) VM_VERSION=$(VERSION:v%=%) KIVALI_CHART=$(abspath dist/kivali-$(VERSION:v%=%).tgz) KIVALI_IMAGES_DIR=$(abspath $(IMAGES_DIR)/bundle-$(VM_ARCH))

# The one desktop version, tauri.conf.json's (what the supervisor sidecar
# is stamped with, here and in desktop-app/Makefile).
DESKTOP_VERSION = $(shell perl -ne 'print "$$1" and exit if /^  "version": "([^"]+)"/' desktop-app/src-tauri/tauri.conf.json)

# What desktop-release builds and collects on this machine. macOS: the
# dmg, and Kivali.app.tar.gz with its .sig for the updater, from an arm64
# VM image (root.squashfs). Windows: the NSIS installer, which is also
# the updater's artifact, and its .sig, from an amd64 one (root.vhdx).
ifeq ($(OS),Windows_NT)
  DESKTOP_VM_ROOT := root.vhdx
  DESKTOP_VM_ARCH := amd64
  DESKTOP_INSTALLER = nsis/Kivali_$(VERSION:v%=%)_x64-setup.exe
  DESKTOP_UPDATER = nsis/Kivali_$(VERSION:v%=%)_x64-setup.exe.sig
else
  DESKTOP_VM_ROOT := root.squashfs
  DESKTOP_VM_ARCH := arm64
  DESKTOP_INSTALLER = dmg/Kivali_$(VERSION:v%=%)_aarch64.dmg
  DESKTOP_UPDATER = macos/Kivali.app.tar.gz macos/Kivali.app.tar.gz.sig
endif

# desktop-release runs desktop-app's release build, which refuses (and
# lists why) unless the signing environment is complete:
# TAURI_SIGNING_PRIVATE_KEY (+ _PASSWORD) everywhere, and on macOS
# APPLE_SIGNING_IDENTITY, the notarization credentials (APPLE_ID +
# APPLE_PASSWORD + APPLE_TEAM_ID, or APPLE_API_KEY + APPLE_API_ISSUER +
# APPLE_API_KEY_PATH) and the Developer ID certificate in the keychain.
# It first checks that VERSION is the desktop's version, then collects
# the installer and the updater artifacts into dist/. latest.json, which
# covers both platforms, is written when the release is published
# (scripts/latest-json.sh).
#
# DEV=1 is a dev build (release.yml run by hand): desktop-app's
# build-trial, the release profile with no signing material checked or
# used (macOS: ad-hoc signed, not notarized) and no updater artifacts;
# only the installer is collected.
desktop-release: require-release-version require-explicit-version
	@bash scripts/version-check.sh $(VERSION) >/dev/null || { bash scripts/version-check.sh $(VERSION); exit 1; }
	@echo "desktop-release: signing environment (names only; values are never printed):"
	@for v in APPLE_SIGNING_IDENTITY APPLE_ID APPLE_PASSWORD APPLE_TEAM_ID APPLE_API_KEY APPLE_API_ISSUER APPLE_API_KEY_PATH TAURI_SIGNING_PRIVATE_KEY TAURI_SIGNING_PRIVATE_KEY_PASSWORD; do \
		if [ -n "$${!v}" ]; then echo "  set      $$v"; else echo "  not set  $$v"; fi; done
	@# The app must ship the VM image of its own version: refuse a missing
	@# image, or one built for another version (vm/build/out/VERSION is
	@# written by `make -C vm image` from VM_VERSION).
	@test -f vm/build/out/$(DESKTOP_VM_ROOT) || { echo "desktop-release: vm/build/out has no $(DESKTOP_VM_ROOT); the app would ship without a VM image (make vm-release VERSION=$(VERSION) VM_ARCH=$(DESKTOP_VM_ARCH))" >&2; exit 1; }
	@vmv="$$(cat vm/build/out/VERSION 2>/dev/null || true)"; if [ "$$vmv" != "$(DESKTOP_VERSION)" ]; then \
		echo "desktop-release: vm/build/out is VM image version '$${vmv:-unknown (no VERSION file)}', the desktop is $(DESKTOP_VERSION) (make vm-release VERSION=$(VERSION) VM_ARCH=$(DESKTOP_VM_ARCH))" >&2; exit 1; fi
	@mkdir -p dist
ifneq ($(DEV),)
	$(MAKE) -C desktop-app build-trial
	cp $(addprefix desktop-app/src-tauri/target/release/bundle/,$(DESKTOP_INSTALLER)) dist/
else
	$(MAKE) -C desktop-app build
	cp $(addprefix desktop-app/src-tauri/target/release/bundle/,$(DESKTOP_INSTALLER) $(DESKTOP_UPDATER)) dist/
endif

# ---- Helm chart -------------------------------------------------------
#
# charts/kivali is what the supervisor installs into the VM's k3s (as a
# HelmChart it renders; internal/supervisor/install.go), the same chart
# for every team. These targets lint and render it offline and package it
# for a release; nothing here deploys. helm is a prerequisite
# (`brew install helm`); point HELM at another binary if it is not on
# PATH:
#   make helm-lint HELM=/path/to/helm
HELM ?= helm

# The chart renders of its two examples.
HELM_RENDERS := example-dev:charts/kivali/examples/values-dev.yaml:kivali-dev \
                example-prod:charts/kivali/examples/values-prod.yaml:kivali-prod

helm-lint:
	@command -v $(HELM) >/dev/null 2>&1 || { echo "helm-lint: helm not found (brew install helm), or set HELM=/path/to/helm" >&2; exit 1; }
	$(HELM) lint charts/kivali
	$(HELM) lint charts/kivali -f charts/kivali/examples/values-dev.yaml
	$(HELM) lint charts/kivali -f charts/kivali/examples/values-prod.yaml

# helm-template renders the chart with both chart examples into dist/helm-template/<name>.yaml (release name
# "kivali", as the chart requires). Offline; touches no cluster.
helm-template:
	@command -v $(HELM) >/dev/null 2>&1 || { echo "helm-template: helm not found (brew install helm), or set HELM=/path/to/helm" >&2; exit 1; }
	@mkdir -p dist/helm-template
	@set -e; for spec in $(HELM_RENDERS); do \
		name=$${spec%%:*}; rest=$${spec#*:}; values=$${rest%%:*}; ns=$${rest#*:}; \
		echo "==> helm template $$name ($$values, namespace $$ns) -> dist/helm-template/$$name.yaml"; \
		$(HELM) template kivali charts/kivali -n $$ns -f $$values > dist/helm-template/$$name.yaml; \
	done

# chart-package writes dist/kivali-<version>.tgz, with the chart version
# and appVersion both set to VERSION (the tarball version follows the
# release, not Chart.yaml's own version), so the default image tag of the
# packaged chart is the release's. Needs a semver VERSION.
chart-package: require-release-version
	@command -v $(HELM) >/dev/null 2>&1 || { echo "chart-package: helm not found (brew install helm), or set HELM=/path/to/helm" >&2; exit 1; }
	@mkdir -p dist
	$(HELM) package charts/kivali --version $(patsubst v%,%,$(VERSION)) --app-version $(VERSION) -d dist

# ---- supervisor and the dev team's pieces ------------------------------

# $(SUPERVISOR_BIN) (macOS with cgo and the ad-hoc signature, Windows
# without either), stamped with the desktop version (tauri.conf.json,
# as desktop-app/Makefile sidecar does).
supervisor:
	@mkdir -p bin
	CGO_ENABLED=$(SUPERVISOR_CGO) go build -trimpath -ldflags="-s -w -X main.version=$(DESKTOP_VERSION)" -o $(SUPERVISOR_BIN) ./cmd/kivali-supervisor
	$(SUPERVISOR_SIGN)

dev-images:
	$(MAKE) images VERSION=dev

dev-chart:
	@command -v $(HELM) >/dev/null 2>&1 || { echo "dev-chart: helm not found (brew install helm), or set HELM=/path/to/helm" >&2; exit 1; }
	@mkdir -p dist/dev
	$(HELM) package charts/kivali --version $(DEV_CHART_VERSION) --app-version dev -d dist/dev
