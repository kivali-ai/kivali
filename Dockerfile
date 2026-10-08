# syntax=docker/dockerfile:1.7
#
# Kivali main image. Multi-stage: Go builder produces a static binary,
# runtime stage carries only what the binary needs at runtime — the
# Go toolchain doesn't ship with the image.
#
# The image tag is the unit of release; manifests pull by tag. See the
# Makefile `release` target.

ARG GO_VERSION=1.24
# Pin the Claude Code CLI so image builds don't silently accept a new
# upstream release. Bump intentionally. Override at build with
# `--build-arg CLAUDE_CODE_VERSION=x.y.z`.
#
# This MUST be a concrete version, never `latest`. `latest` reads as
# "always current" but does the opposite: the npm install line below is
# the whole cache key, so a literal `latest` never changes, the layer is
# never rebuilt, and the image freezes on whichever release happened to
# be current the first time anyone built it — and that CLI rejects every
# newer model with "please upgrade".
#
# The floor is set by the newest model ID the picker offers: the CLI
# validates --model against a list compiled into the binary, so shipping
# a selectable model in internal/claudeagent/catalog.go means shipping a
# CLI that knows it.
# claude-fable-5-1 lands in 2.1.257 (2.1.252 does not have it).
# claude-opus-5-5 lands in 2.1.280 (2.1.278 does not have it).
ARG CLAUDE_CODE_VERSION=2.1.280
# Stamp injected into the binary. Makefile populates this from git
# describe; standalone docker builds fall back to "dev".
ARG VERSION=dev

# The Kivali web app. Vite writes its build to ../internal/web/ui/dist
# (web/vite.config.ts), which the Go stage below copies in before
# `go build` so the binary embeds it. package-lock.json is copied with
# package.json so the `npm ci` layer is cached until dependencies
# change. The build reads web/ and the API's golden fixtures: the app
# imports the design system's copies under web/src/ds and web/public
# (`make ds-sync`), but `tsc` type-checks every file under web/src,
# including the tests: web/src/state/fixtures imports
# internal/web/apitypes/testdata/*.json and two design-system tests
# (src/ds/barrel.test.ts, src/ds/roleCatalogue.test.ts) import the
# vendored design-system/ manifest and role catalogue to prove the
# ports match the package. Both trees must sit at the same relative
# path here as in the checkout.
# Multi-arch (linux/amd64 + linux/arm64). The two stages that produce
# architecture-neutral or cross-compilable output (web-build, build) run
# on the BUILD host's own architecture ($BUILDPLATFORM) and cross-compile
# for $TARGETARCH, so an arm64 build on an amd64 runner (or the reverse)
# never runs the Go compiler or Vite under QEMU. Only claude-build and the
# runtime stage run on the target platform, because the Claude Code
# postinstall fetches the native binary for the architecture it runs on.
FROM --platform=$BUILDPLATFORM node:22-slim AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY internal/web/apitypes/testdata/ /src/internal/web/apitypes/testdata/
COPY design-system/ /src/design-system/
COPY web/ ./
RUN npm run build
# The npm packages the web app ships, for the image's notices (below).
COPY scripts/third-party-licenses.sh /src/scripts/
RUN sh /src/scripts/third-party-licenses.sh npm /src/web > /src/npm-licenses.txt

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG VERSION
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# BuildKit cache mounts let `go mod` and the build cache survive across
# CI runs even when the COPY layer below busts on a source change.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# .dockerignore keeps any local build out of the context; this is the
# one from the web-build stage.
COPY --from=web-build /src/internal/web/ui/dist ./internal/web/ui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/kivali .
# The image's third-party notices: Kivali's NOTICE, the Go modules linked
# into the binary for this platform and the web app's npm packages
# (scripts/third-party-licenses.sh).
COPY --from=web-build /src/npm-licenses.txt /tmp/
RUN --mount=type=cache,target=/go/pkg/mod \
    set -eu; \
    { sh scripts/third-party-licenses.sh header kivali; \
      GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 sh scripts/third-party-licenses.sh go .; \
      cat /tmp/npm-licenses.txt; } > /out/THIRD_PARTY_LICENSES

# Claude Code now ships as a self-contained native binary; the npm
# package's postinstall script downloads the platform-specific
# executable matching this builder's OS+arch (see PLATFORMS in
# cli-wrapper.cjs). Node + npm are needed only to trigger that
# postinstall — they don't ship to runtime.
#
# `cp -L` resolves the global `claude` symlink to the actual binary
# so the runtime stage doesn't have to know npm's install prefix
# (which is /usr/local for the official node image but /usr for
# distro packages — this insulates us from either).
FROM node:22-slim AS claude-build
ARG CLAUDE_CODE_VERSION
RUN npm install -g --no-progress @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    && cp -L "$(command -v claude)" /claude

# Runtime — Debian slim with only what the kivali binary needs at run
# time. Office-format conversion is in-process Go (internal/convert);
# pdftotext is the one external tool, used only when an actual PDF is
# uploaded. Claude Code is the native binary copied from claude-build
# above; ldd shows it only links the glibc family that ships with
# Debian. The base is pinned by digest (the same one as
# Dockerfile.dev-shell and vm/Dockerfile) so SOURCES.md names exactly
# what the image was built from.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
LABEL org.opencontainers.image.licenses="Apache-2.0"

RUN apt-get update && apt-get install -y --no-install-recommends \
        poppler-utils \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/* \
    && groupadd -r -g 65532 kivali \
    && useradd -r -u 65532 -g 65532 -s /usr/sbin/nologin kivali \
    && mkdir -p /data && chown 65532:65532 /data

# /usr/share/doc/kivali: the installed Debian packages (packages.txt),
# where their source is and Kivali's offer of it (SOURCES.md), and the
# third-party notices, Claude Code's among them (THIRD_PARTY_LICENSES).
RUN --mount=type=bind,source=scripts/third-party-sources.sh,target=/tmp/third-party-sources.sh \
    set -eu; \
    d=/usr/share/doc/kivali; \
    mkdir -p "$d"; \
    { . /etc/os-release; echo "# $PRETTY_NAME"; dpkg-query -W -f '${Package} ${Version} ${source:Package} ${source:Version}\n' | sort; } > "$d/packages.txt"; \
    { sh /tmp/third-party-sources.sh header "the kivali image"; \
      sh /tmp/third-party-sources.sh debian "Debian packages" "$d/packages.txt"; } > "$d/SOURCES.md"
COPY --from=build /out/THIRD_PARTY_LICENSES /usr/share/doc/kivali/
COPY --from=claude-build /claude /usr/local/bin/claude
COPY --from=build /out/kivali /usr/local/bin/kivali
USER 65532:65532
# HOME lives on the data PVC so the CLI's sign-in (under
# $HOME/.claude/) survives pod restarts. Operators sign in once by
# starting Claude, which opens its sign-in menu when not signed in
# (`/login` switches later):
#   kubectl exec -it deploy/kivali -c kivali -- claude
ENV HOME=/data/claude-home
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kivali"]
