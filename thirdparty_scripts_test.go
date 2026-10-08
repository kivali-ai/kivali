package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The release's license and source scripts (scripts/third-party-*.sh),
// run against the fixtures in scripts/testdata: an Alpine installed
// database, a Debian package list, k3s's version.sh and a small
// node_modules.

func runScript(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func TestThirdPartySourcesAlpineListsGPLPackagesAtTheirCommit(t *testing.T) {
	got := runScript(t, "scripts/third-party-sources.sh", "apk", "Root disk",
		"scripts/testdata/third-party-sources/rootfs-installed", "3.23.6")
	for _, want := range []string{
		"## Root disk",
		"https://distfiles.alpinelinux.org/distfiles/v3.23/",
		"| busybox | 1.37.0-r30 | GPL-2.0-only | [APKBUILD](https://gitlab.alpinelinux.org/alpine/aports/-/tree/1e823a60eb85606954b3a5af5f8e5bbd1ea680cf/main/busybox), [upstream](https://busybox.net/) |",
		"| readline | 8.3.1-r0 | GPL-3.0-or-later |",
		"| libblkid | 2.41.6-r1 | LGPL-2.1-or-later | [APKBUILD](https://gitlab.alpinelinux.org/alpine/aports/-/tree/96f05433b217acf92bee031df551b10d8b3a1ec2/main/util-linux)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// musl is MIT, the CA bundle MPL-2.0 AND MIT: not GPL or LGPL.
	for _, absent := range []string{"| musl |", "| ca-certificates-bundle |"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q is listed:\n%s", absent, got)
		}
	}
}

func TestThirdPartySourcesAlpineOnlyNamedPackagesWithKernelOrg(t *testing.T) {
	got := runScript(t, "scripts/third-party-sources.sh", "apk", "Kernel",
		"scripts/testdata/third-party-sources/kernel-installed", "3.23.6", "linux-virt")
	if !strings.Contains(got, "| linux-virt | 6.18.55-r0 | GPL-2.0-only |") ||
		!strings.Contains(got, "[kernel.org](https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.55.tar.xz)") {
		t.Errorf("no linux-virt row with its kernel.org source:\n%s", got)
	}
	// Installed in the same stage, but not named.
	if strings.Contains(got, "| busybox-static |") || strings.Contains(got, "| kmod |") {
		t.Errorf("a package not named is listed:\n%s", got)
	}
}

func TestThirdPartySourcesDebianListsSourcePackagesOnce(t *testing.T) {
	got := runScript(t, "scripts/third-party-sources.sh", "debian", "Debian packages",
		"scripts/testdata/third-party-sources/debian-packages.txt")
	if !strings.Contains(got, "Debian GNU/Linux 13 (trixie)") {
		t.Errorf("the list's note is missing:\n%s", got)
	}
	// Two binary packages of util-linux: one row, by the source version.
	if n := strings.Count(got, "| util-linux |"); n != 1 {
		t.Errorf("util-linux listed %d times:\n%s", n, got)
	}
	for _, want := range []string{
		"| bash | 5.2.37-2 | [snapshot.debian.org](https://snapshot.debian.org/package/bash/5.2.37-2/)",
		"https://snapshot.debian.org/package/util-linux/2.41.5-0%2Bdeb13u1/",
		"https://sources.debian.org/src/shadow/1%3A4.17.4-2/",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestThirdPartySourcesK3sNamesK3sRoot(t *testing.T) {
	got := runScript(t, "scripts/third-party-sources.sh", "k3s", "v1.36.5+k3s1",
		"scripts/testdata/third-party-sources/k3s-version.txt")
	for _, want := range []string{
		"https://github.com/k3s-io/k3s/tree/v1.36.5%2Bk3s1",
		"https://github.com/k3s-io/k3s-root/tree/v0.15.2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestThirdPartyLicensesNpmProductionPackagesGroupedByText(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	got := runScript(t, "scripts/third-party-licenses.sh", "npm", "scripts/testdata/third-party-licenses/npm")
	// alpha and beta ship the same text (LICENSE, LICENSE.md): one block
	// under both names.
	shared := "alpha@1.0.0\nbeta@2.0.0\n" + strings.Repeat("-", 80) + "\n\nShared License\n"
	if !strings.Contains(got, shared) || strings.Count(got, "Shared License") != 1 {
		t.Errorf("alpha and beta are not one block:\n%s", got)
	}
	if !strings.Contains(got, "bare@3.0.0\n"+strings.Repeat("-", 80)+"\n\nNo license file is published with these components. Declared license: ISC") {
		t.Errorf("bare's declared license is missing:\n%s", got)
	}
	// A dev dependency, and an optional package for another platform
	// (not installed), are not shipped.
	if strings.Contains(got, "devtool") || strings.Contains(got, "other-platform") {
		t.Errorf("a package that does not ship is listed:\n%s", got)
	}
}
