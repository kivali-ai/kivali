package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Why this test exists: a server deployment changed by hand to run
// another program (a command over a mounted binary) keeps that command
// through Helm's upgrade, since the chart sets none. The upgrade then
// moved the image and the chart label, every check passed, and the
// team was recorded on the new release while the old program served.
// The preflight must refuse it, naming the program, before anything
// changes.
func TestUpgradeRefusesAServerRunningItsOwnCommand(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	boots := len(h.vm.boots)
	h.vm.serverCommand = []string{"/opt/hotfix/kivali"}
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")

	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "preflight: the server runs /opt/hotfix/kivali in place of the Kivali in its image") ||
		!strings.HasSuffix(err.Error(), "nothing was changed") {
		t.Fatalf("upgrade: %v", err)
	}
	if st := h.sup.State(); st.Kivali != "0.15.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if exists(h.snapPath()) || len(h.vm.boots) != boots || h.disk()[ManifestRel] != before {
		t.Fatal("the refused upgrade changed the org")
	}

	// The baked update, which a start runs, is refused the same way.
	h.bakeRelease("0.16.0", "v0.16.0")
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf()); err == nil || !strings.Contains(err.Error(), "/opt/hotfix/kivali") {
		t.Fatalf("baked upgrade: %v", err)
	}

	// With the command gone the upgrade goes ahead.
	h.vm.serverCommand = nil
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
		t.Fatalf("upgrade without the command: %v", err)
	}
	if h.sup.State().Kivali != "0.16.0" {
		t.Fatalf("state %+v", h.sup.State())
	}
}

// Why this test exists: the image tag and the chart label name a
// release, but only the binary that serves says which one it is. A
// server on the new image that says it is another version must fail
// the upgrade and roll it back, not be recorded as the new release.
func TestUpgradeRollsBackAServerThatSaysAnotherVersion(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	h.vm.serverVersion = "v0.15.0"
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")

	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "the server runs image docker.io/library/kivali:v0.16.0 but says it is Kivali 0.15.0, not 0.16.0") {
		t.Fatalf("upgrade: %v", err)
	}
	if st := h.sup.State(); st.Kivali != "0.15.0" || st.Upgrade != nil || h.disk()[ManifestRel] != before {
		t.Fatalf("not rolled back: %+v", st)
	}
}

// A development build stamps its binary 0.0.0-dev+g<commit>, which no
// chart version equals; and a log with no boot line (a server from
// before it, a log that rotated) cannot say. Neither fails an upgrade.
func TestUpgradeServerVersionsItDoesNotCompare(t *testing.T) {
	for name, set := range map[string]func(*fakeBackend){
		"development build": func(vm *fakeBackend) { vm.serverVersion = "0.0.0-dev+g2b703bf3c1e4" },
		"no boot line":      func(vm *fakeBackend) { vm.noBootLine = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.mustUp(UpOptions{Install: owner})
			set(h.vm)
			feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
			if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			if h.sup.State().Kivali != "0.16.0" {
				t.Fatalf("state %+v", h.sup.State())
			}
		})
	}
}

func TestServerBootVersion(t *testing.T) {
	for log, want := range map[string]string{
		"2026/01/02 03:04:05 kivali boot: version=v1.7.0 env=dev provider=claude credential=\"signed in\"\n": "1.7.0",
		"x\n2026/01/02 03:04:05 kivali boot: version=0.0.0-dev+g2b703bf3c1e4 env=prod provider=claude\n":     "0.0.0-dev+g2b703bf3c1e4",
		"2026/01/02 03:04:05 kivali boot: PRODUCTION MODE\n":                                                 "",
		"": "",
	} {
		got, ok := ServerBootVersion([]byte(log))
		if got != want || ok != (want != "") {
			t.Errorf("ServerBootVersion(%q) = %q, %v; want %q", log, got, ok, want)
		}
	}
}
