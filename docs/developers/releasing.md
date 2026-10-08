# Releasing: procedure and pipeline

One version number cuts one GitHub release. The release carries every
artifact: the three container images (per architecture), the Helm chart,
the VM images, Kivali Desktop for macOS (a signed, notarized dmg) and
Windows (an NSIS installer), and the two feeds that tell running
installs about it. [deployment.md](deployment.md) covers the orgs that
run it (the supervisor installs and upgrades them); this document covers
producing it. A fork or a new maintainer sets up the items in "Before
the first release" first.

## The procedure

1. **Decide whether the upgrade needs steps the app cannot do.** If it
   does, add the tag (one line, `vX.Y.Z`) to `scripts/manual-steps` and
   commit that on `main` first, and write the steps in the release
   notes (see "Publishing a release that needs manual steps").
2. **Cut it** on a clean, up-to-date `main`:

   ```
   make release VERSION=vX.Y.Z
   ```

   `scripts/release.sh` checks the version is semver, the branch is
   `main`, the tree is clean, `origin/main` has nothing local `main`
   lacks, and the tag is unused. It then refuses unless
   every versioned file already agrees (`scripts/version-check.sh`) and
   until tauri.conf.json carries the owner's updater public key
   (`scripts/check-updater-key.sh`), bumps them all together (`scripts/bump-version.sh`), commits, tags
   `vX.Y.Z` and pushes the commit and tag atomically.
3. **The tag push starts `.github/workflows/release.yml`.** Watch it
   under the repository's Actions tab. When it finishes the release is
   published with every asset and `/releases/latest` points at it.
4. **Run the boot test on a Mac.** CI cannot: `make -C vm boottest` needs
   Virtualization.framework and GitHub-hosted runners have no nested
   virtualization. Download the release's `Image`, `initramfs.gz` and
   `root.squashfs` into a directory, put it at `vm/build/out`, and run
   `make -C vm boottest`. Do this before announcing the release.
5. **Upgrade the orgs.** Each takes the release from the feed
   (`kivali-supervisor upgrade`, or Update in Kivali Desktop;
   [deployment.md](deployment.md)), installing the release's own chart
   and image bundle.

### Dev builds

To exercise the whole pipeline without cutting a release or holding any
signing material, run the release workflow by hand: Actions, **Release**,
**Run workflow**, on any branch (or `gh workflow run release.yml --ref
<branch>`). It runs every test and build a release does and publishes
the pre-release `vX.Y.Z-dev.<run number>` (X.Y.Z the tree's version),
creating its tag on the commit it built. It differs from a release in
what it skips:

- No secrets are read. The macOS app is ad-hoc signed and not notarized
  (macOS blocks its first launch until the user allows it in System
  Settings, Privacy & Security); the Windows installer is the same as a
  release's, which is never Authenticode-signed.
- No updater artifacts and no `latest.json`: a dev build does not update
  itself. Being a pre-release, it is skipped by `/releases/latest`, so
  no installed app or org is ever offered it.
- The version is stamped into the desktop's files in the build only
  (`scripts/bump-version.sh`, not committed); `make desktop-release DEV=1`
  builds with `desktop-app`'s `build-trial`.

Delete a dev build when done with it: `gh release delete
vX.Y.Z-dev.N --cleanup-tag`.

### One version

| File | Carries | Source |
| --- | --- | --- |
| `charts/kivali/Chart.yaml` | `appVersion: vX.Y.Z` (the default image tag of the server, egress proxy and dev-shell) | the tag |
| `desktop-app/src-tauri/tauri.conf.json` | `"version": "X.Y.Z"` | **the desktop's source** |
| `desktop-app/src-tauri/Cargo.toml`, `Cargo.lock` | `version` | follows tauri.conf.json (`build.rs` stops the build otherwise) |
| `desktop-app/package.json`, `package-lock.json` | `"version"` | follows tauri.conf.json (same) |

`make version-check` asserts they agree (with `VERSION=vX.Y.Z`, that they
all equal it, the desktop's without the `v`). CI runs it on every pull request and every push to `main`;
`release.sh` runs it before the bump (and `bump-version.sh` after); the release workflow runs
it first with the pushed tag and fails the whole run, before building
anything, if the tag and tauri.conf.json's version differ.

`make supervisor` and desktop-app's `make sidecar` both stamp the Go
supervisor with tauri.conf.json's version, so the app, its sidecar and the
supervisor binary report one number. The Go server image is stamped with
the tag (`-X main.version=vX.Y.Z`). The tree sits at the last released
version between releases; `release.sh` is what moves it, so do not edit
one of these files by hand.

## What the workflow does

Jobs, in order (`needs` in parentheses):

| Job | Runner | Produces |
| --- | --- | --- |
| `verify` | ubuntu-latest | tag equals tauri.conf.json's version; `make version-check VERSION=$tag` |
| `images` (verify), amd64 and arm64 | ubuntu-latest, ubuntu-24.04-arm (native; no QEMU) | `kivali-images-linux-<arch>.tar.zst`; the kivali and kivali-dev-shell images' Debian package lists (for `SOURCES.md`) |
| `chart` (verify) | ubuntu-latest | `kivali-X.Y.Z.tgz` (`make chart-package`) |
| `licenses` (verify) | ubuntu-latest | `THIRD_PARTY_LICENSES.txt`, the notices of everything a release ships (`make third-party-licenses`) |
| `vm` (images, chart), arm64 and amd64 | ubuntu-24.04-arm, ubuntu-latest (native) | arm64, for macOS: `Image`, `initramfs.gz`, `root.squashfs`; amd64, for Windows: `root.vhdx`; each with `packages.txt` and `SOURCES.md`, and that architecture's images and the chart baked in |
| `feed` (images, chart) | ubuntu-latest | `release.json` (`scripts/release-json.sh`) |
| `desktop-macos` (verify, vm) | macos-15 (Apple Silicon) | `Kivali_X.Y.Z_aarch64.dmg`, `Kivali.app.tar.gz`, `Kivali.app.tar.gz.sig` |
| `desktop-windows` (verify, vm) | windows-latest (x64) | `Kivali_X.Y.Z_x64-setup.exe` and its `.sig` |
| `publish` (all, plus `checks` and `nightly`) | ubuntu-latest | `latest.json` (both platforms, `scripts/latest-json.sh --require`); `SOURCES.md` (the arm64 VM's, with the images' Debian packages and the egress proxy's base appended), `vm-amd64-SOURCES.md` and `vm-amd64-packages.txt`; one release: created as a draft, every asset uploaded, then published |

Choices worth knowing:

- **The image bundle** is what the supervisor imports
  (`internal/supervisor/release.go`, `guestapi/images.go`): one
  `docker save` archive holding `kivali`, `kivali-egress-proxy` and
  `kivali-dev-shell` at `:vX.Y.Z`, zstd-compressed.
  `scripts/image-bundle.sh` makes it from the per-image tarballs of
  `make image-tars ARCH=<arch>` (run by `release-bundle-<arch>`) and checks all three references are listed. The
  archive carries the images' own names, not a registry's: the chart's
  image references (`kivali:vX.Y.Z`) resolve to what containerd imported.
- **The VM images** are built from each architecture's *bundle
  uncompressed* (squashfs compresses it), so a first install needs no
  download: arm64 for macOS, amd64 for Windows, where `root.vhdx` holds
  the unified kernel image and the squashfs. The arm64 image's release
  asset names are the ones the supervisor's `--vm-dir` expects (`Image`,
  `initramfs.gz`, `root.squashfs`); `packages.txt` records the installed
  packages. The amd64 image ships only inside the Windows installer; its
  `packages.txt` and `SOURCES.md` (which adds the EFI stub's Debian
  packages) are published as `vm-amd64-packages.txt` and
  `vm-amd64-SOURCES.md`.
- **The desktop jobs** build the Go supervisor sidecar (stamped with the
  version) and the app, and bundle `vm/build/out`. Both sign the updater
  artifacts with `TAURI_SIGNING_PRIVATE_KEY`, and `publish` writes
  `latest.json` from both, refusing a release that lacks either platform.
- **Windows** builds the per-machine NSIS installer, which is also the
  updater's artifact. It is not Authenticode-signed, so SmartScreen warns
  on its first run; updates are verified by the updater's signature
  either way.
- **macOS** signs with the Developer ID certificate from an imported
  temporary keychain. Tauri signs, **notarizes and staples the `.app`** (it does so when its
  credentials are in the environment) and then builds the dmg around the
  stapled app; the dmg itself is not separately notarized. The `.app`
  carries the ticket, so it passes Gatekeeper offline once copied out of the
  dmg, and the dmg opens because the app inside it is notarized. The
  updater's `Kivali.app.tar.gz` is that same stapled app.
- **Early checks.** Everything that can be known before a minute is spent
  is checked in the `verify` job (and, for the updater key, by
  `release.sh` before it pushes): the tag equals tauri.conf.json's version,
  every versioned file agrees, every signing secret is set (the error lists
  each missing one), and tauri.conf.json's updater public key is not the
  development key. A failure there costs seconds; a failure after the images
  and VM image are built would cost over an hour and a version number.
- **`make desktop-release`** also refuses (locally and in CI) when
  `vm/build/out` has no root disk for the platform (`root.squashfs` on
  macOS, `root.vhdx` on Windows), or its `VERSION` file (written by
  `make -C vm image` from `VM_VERSION`) differs from the desktop version, so
  an app can never bundle another version's VM image.
- **Asset URLs.** `release.json` gives every asset an absolute URL pinned
  to the tag. `internal/supervisor/release.go` uses an asset's `url` when
  it has one; without one it resolves the name against `release.json`'s
  own URL, and since the feed is fetched through `releases/latest/download/`
  that would follow `latest`: a newer release published while an install is
  downloading could 404 or fail the digest. Pinned URLs avoid that.
- **Publishing** is draft first: `/releases/latest` (what the updater and
  `latest.json` resolve through) never points at a half-uploaded release.
  A tag with a suffix (`vX.Y.Z-rc1`) is published as a pre-release, which
  `/latest` skips, so neither the updater nor a feed check offers it.
- **Tests gate publishing.** The workflow calls `ci.yml` (every
  per-commit check, Kivali Desktop's on macOS and Windows among them)
  and `nightly.yml` (race detector, integration suite on k3s) on the
  tag; `publish` waits for both, and a failure publishes nothing.
- **Caches.** Every cache is written from `main` only: a cache written on
  a tag is readable by that tag alone, and one written on a branch is
  a copy of main's. The desktop jobs restore the release-profile Rust
  cache that `nightly.yml`'s `package` job (a release-profile bundle on
  macOS and Windows, unsigned) saves on `main`; `ci.yml`'s desktop jobs
  keep a debug-profile one per OS, built with line tables only.

The same scripts run locally:

```
make release-assets VERSION=vX.Y.Z   # dist/: image bundles for RELEASE_ARCHES (default amd64 arm64;
                                     #   RELEASE_ARCHES=arm64 skips QEMU), the chart, release.json
make vm-release VERSION=vX.Y.Z       # the VM image for VM_ARCH (arm64, the default; amd64 for
                                     #   Windows), with dist's images of that arch and the chart baked in
make desktop-release VERSION=vX.Y.Z  # this platform's installer and updater artifacts in dist/
scripts/latest-json.sh vX.Y.Z        # latest.json from the updater artifacts in dist/
```

`make desktop-release` lists which signing variables are set (names only)
and, through `desktop-app`'s `release-preflight`, refuses with every reason
when development signing material would ship: no `TAURI_SIGNING_PRIVATE_KEY`
(or the development one), the development public key still in
tauri.conf.json, on macOS an ad-hoc identity or no notarization
credentials, a version mismatch, or the fake supervisor. `ALLOW_DEV_KEYS=1` overrides that for a
local trial, never for distribution.

## Licenses and the source offer

Every binary artifact carries the license notices of the third-party
software in it, and the GPL and LGPL software comes with links to its
source and a written offer of it:

- **`THIRD_PARTY_LICENSES`.** `scripts/third-party-licenses.sh` writes
  Kivali's `NOTICE` and the license files of every Go module, npm
  package and Rust crate an artifact links, taken from the module cache,
  `node_modules` and the Cargo registry the build used. Each image
  writes its own during `docker build`, into
  `/usr/share/doc/kivali/THIRD_PARTY_LICENSES` (the kivali image's also
  says Claude Code is Anthropic's proprietary software, under Anthropic's
  terms). The release has one file covering everything it ships:
  `make third-party-licenses` writes it to
  `dist/licenses/THIRD_PARTY_LICENSES.txt`, and `desktop-app`'s
  `make build` writes the same file and bundles it as the resource
  `THIRD_PARTY_LICENSES.txt`, which **Kivali, Open source licenses**
  opens (on Windows, **Help, Open source licenses**).
- **`SOURCES.md`.** `scripts/third-party-sources.sh` lists the GPL and
  LGPL packages at their exact versions with links to their source
  (Alpine's aports at the commit each package was built from, kernel.org,
  the k3s and k3s-root tags; snapshot.debian.org for Debian), followed by
  the written offer. Each VM image has one at
  `/usr/share/doc/kivali/SOURCES.md` (with the GNU license texts beside it
  in `licenses/`), the kivali and kivali-dev-shell images one for their
  Debian packages beside `packages.txt`. The release's `SOURCES.md` is the
  arm64 VM's with the images' Debian packages appended, and
  `vm-amd64-SOURCES.md` is the amd64 VM's, which adds the EFI stub.

The offer is valid for three years from each release: a source request
arrives as an issue or a private advisory titled "Source request". Answer
it with the source archives of that release's versions (the links in its
`SOURCES.md`), or put them on a release, and keep being able to do so for
three years after the last release that shipped them.

A crate or npm package published without a license file is listed with
its declared license, and the script names it on stderr; check those
when the dependencies change.

After any dependency change, run `make licenses`: it regenerates the
notices and holds every shipped dependency to `scripts/license-policy.txt`
(`make license-check`, which CI also runs on every pull request and every
push to `main`).

## Secrets

Repository secrets (Settings, Secrets and variables, Actions). The macOS
desktop job needs all of them but one of the notarization options; the
Windows one needs the two `TAURI_SIGNING_*`. `publish` uses the automatic
`GITHUB_TOKEN`.

| Secret | What it is | Where it comes from |
| --- | --- | --- |
| `APPLE_CERTIFICATE` | the Developer ID Application certificate and its private key, as base64 of a `.p12` | Apple Developer account, Certificates: create a Developer ID Application certificate, import it in Keychain Access, export it as `.p12`, then `base64 -i cert.p12 \| pbcopy` |
| `APPLE_CERTIFICATE_PASSWORD` | the password chosen when exporting the `.p12` | you |
| `KEYCHAIN_PASSWORD` | any string; locks the job's temporary keychain | you (e.g. `openssl rand -hex 16`) |
| `APPLE_SIGNING_IDENTITY` | the certificate's name, e.g. `Developer ID Application: Name (TEAMID)` | `security find-identity -v -p codesigning` after importing |
| *notarization, option A* | | |
| `APPLE_ID` | the Apple ID of the developer account | you |
| `APPLE_PASSWORD` | an app-specific password for that Apple ID | appleid.apple.com, Sign-In and Security |
| `APPLE_TEAM_ID` | the 10-character team id | Apple Developer account, Membership |
| *notarization, option B (instead of A)* | | |
| `APPLE_API_KEY` | App Store Connect API key id | App Store Connect, Users and Access, Integrations |
| `APPLE_API_ISSUER` | the issuer id on that page | same |
| `APPLE_API_KEY_P8` | the downloaded `AuthKey_<id>.p8`, base64 | `base64 -i AuthKey_X.p8 \| pbcopy` |
| `TAURI_SIGNING_PRIVATE_KEY` | the updater's private key (the file's contents) | `npx tauri signer generate -w kivali-updater.key` in `desktop-app`; keep the file somewhere safe |
| `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` | its password (may be empty) | chosen at generation |

Not a secret but required before the first release: the **public** half
(`kivali-updater.key.pub`) goes in `desktop-app/src-tauri/tauri.conf.json`
under `plugins.updater.pubkey`, replacing the development key's. `release.sh` and
the workflow both refuse while the development key is still there. An app signed
with one updater key never accepts an update signed with the other, so this
must happen before the first public release, and the private key must never
be lost: a release signed with a new one cannot update installed apps.

## How the releases are consumed

**The desktop updater** (`tauri-plugin-updater`) fetches
`https://github.com/kivali-ai/kivali/releases/latest/download/latest.json`
at launch (the URL is `plugins.updater.endpoints` in tauri.conf.json). The
file has `version`, `pub_date`, `notes` and one `{url, signature}` per
platform: `platforms."darwin-aarch64"`, the tag's own `Kivali.app.tar.gz`,
and `platforms."windows-x86_64"`, its `Kivali_X.Y.Z_x64-setup.exe`;
`signature` is the contents of the artifact's `.sig`. The app verifies the
signature against the public key compiled in, replaces itself (on Windows
by running the installer) and restarts; it stops the local org and the
supervisor first.

**The org upgrade** (the supervisor, [supervisor.md](supervisor.md)) fetches
`release.json` from the feed in `local.json` (by default the same latest
release) and, on an upgrade, downloads the assets it names, which resolve
relative to `release.json`'s own URL. It verifies the sha256 of the chart
and the image bundle before importing anything, and refuses a chart whose
`Chart.yaml` version differs from `release.json`'s. Fields:

| Field | Value |
| --- | --- |
| `version` | `X.Y.Z` |
| `chart` | `{name: kivali-X.Y.Z.tgz, sha256, size}` |
| `images` | `{"linux/arm64": {...}, "linux/amd64": {...}}` for the bundles (the guest runs arm64; amd64 is listed for other hosts) |
| `min_desktop_version` | `MIN_DESKTOP_VERSION`, default `X.Y.Z`. Cut together, the org asks for the desktop of the same number; lower it only for a release an older app can run (re-run `release-json.sh` with `MIN_DESKTOP_VERSION=...` and re-upload; see below) |
| `vm_image` | `VM_IMAGE_VERSION`, default `X.Y.Z` |
| `manual_steps` | true when the tag is in `scripts/manual-steps` |
| `notes_url` | the release page (override with `NOTES_URL`) |

The desktop and the org version are cut together but may drift: an org
whose release needs a newer desktop shows "Update Kivali Desktop first"; an
older org keeps running under a newer app.

## Verifying the first signed release

Check the first signed release by hand on a Mac (and again whenever
signing changes). Download the dmg, then:

```
spctl -a -t open --context context:primary-signature -v Kivali_X.Y.Z_aarch64.dmg
# expect: accepted, source=Notarized Developer ID   (or Developer ID; see below)
xcrun stapler validate /Applications/Kivali.app     # after dragging it out of the dmg
# expect: The validate action worked!
spctl -a -vv /Applications/Kivali.app               # expect: accepted, source=Notarized Developer ID
codesign -dv --entitlements - /Applications/Kivali.app/Contents/MacOS/kivali-supervisor
# expect com.apple.security.virtualization
```

The ticket is stapled to the `.app`, not the dmg, so `stapler validate`
belongs on the app; `spctl` on the dmg checks the dmg's own signature
(Tauri signs the dmg with the same identity). If `spctl` on the dmg says
"rejected, source=Unnotarized Developer ID", notarize and staple the dmg
too: `xcrun notarytool submit ... --wait` then `xcrun stapler staple` on it,
and re-upload it with `gh release upload --clobber`.

## A run failed before publish

The `publish` job creates the release as a draft and publishes it last, so
a run that fails earlier leaves nothing public: `/releases/latest` still
points at the previous release.

- **A transient failure** (a runner, a flaky download, a half-uploaded
  draft): re-run the failed jobs from the Actions tab ("Re-run failed
  jobs"). Re-running `publish` alone recovers a half-uploaded draft: it skips
  creating the release because the draft exists, `gh release upload
  --clobber` replaces whatever partially uploaded, and `--draft=false`
  publishes. Upstream artifacts live 7 days; after that re-run the whole
  workflow.
- **A fix to the workflow or the scripts**: a re-run uses the workflow and
  scripts at the tagged commit, so edits on `main` do not reach it. Cut a
  new tag (`make release VERSION=vX.Y.(Z+1)`) for the fix, and leave the
  failed draft alone or delete it.
- **Start the same version over**: delete the draft release and the tag
  (`gh release delete vX.Y.Z --cleanup-tag`, or delete the draft in the UI
  and `git push origin :refs/tags/vX.Y.Z; git tag -d vX.Y.Z`), fix and
  commit, then run `make release VERSION=vX.Y.Z` again. Only do this when
  nothing was published: never reuse a version that already shipped.

## Publishing a release that needs manual steps

When an upgrade needs work the app cannot do (a data change to run by
hand, a setting to change), `release.json` must say `"manual_steps": true`; the app
then shows "needs steps the app can't do yet; see the upgrade notes"
(linking `notes_url`) instead of offering the one-click upgrade.

- **Before cutting**: add `vX.Y.Z` on its own line to `scripts/manual-steps`
  and commit it on `main`. `release-json.sh` reads the file, so the workflow
  writes `manual_steps: true`. Put the steps in the release notes on
  `https://github.com/kivali-ai/kivali/releases`.
- **After a release is out** (you forgot, or found out later):

  ```
  gh release download vX.Y.Z -p 'kivali-*' -D /tmp/rel
  scripts/release-json.sh vX.Y.Z --dir /tmp/rel --manual-steps
  gh release upload vX.Y.Z /tmp/rel/release.json --clobber
  ```

  Clients re-read `release.json` on every check, so the change is live as
  soon as the asset is replaced. The same three lines, without
  `--manual-steps` (and with `MIN_DESKTOP_VERSION=...` if needed), correct
  any other `release.json` field.

## Rolling a release back

Nothing in a release is deleted to roll it back; point new checks away
from it.

1. **Stop new installs and upgrades from taking it.** `/releases/latest`
   is the newest published, non-pre-release release. Either mark the bad
   release a pre-release (`gh release edit vX.Y.Z --prerelease`), which
   makes `/latest` fall back to the previous one, or delete it
   (`gh release delete vX.Y.Z`; the tag stays unless you add
   `--cleanup-tag`). Orgs and apps that have not yet
   upgraded then see the previous release.
2. **Orgs already upgraded** roll back by themselves when the upgrade
   failed (the supervisor's journal restores the snapshot,
   [supervisor.md](supervisor.md)).
   An upgrade that completed but is bad is fixed by a *newer* release: the
   feed only moves forward (an older `version` than installed reads as up to
   date), so cut `vX.Y.(Z+1)` with the fix.
3. **Apps already updated** do the same: the updater only moves to a higher
   `version`. A broken desktop build is superseded by a fixed one; never
   re-point `latest.json` at an older version expecting a downgrade.
4. **Do not re-push or move the tag**; release a new version instead. The
   workflow replaces assets on a re-run (`--clobber`), but digests change
   and installs that already cached the old ones will see a mismatch.

## Before the first release

What a maintainer sets up before a tag can publish a complete release:

- **The repository** at `github.com/kivali-ai/kivali`: the updater
  endpoint and the `release.json` URLs assume it.
- **The Apple Developer ID certificate and notarization credentials**, as
  the repository secrets in "Secrets".
- **The updater keypair**: the private key and its password as secrets,
  the public key committed to `tauri.conf.json` (see "Secrets").
- **The `ubuntu-24.04-arm` runner** for the arm64 image and VM jobs
  (free for public repositories). The macOS and Windows jobs run on
  GitHub's hosted `macos-15` and `windows-latest`.
- **Private vulnerability reporting** (Settings, Security), which
  [SECURITY.md](../../SECURITY.md) points reporters to.

Then check the first signed release by hand ("Verifying the first signed
release"), and run `make -C vm boottest` on a Mac against the `vm` job's
artifacts: nothing in the workflow boots the VM image.
