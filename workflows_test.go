package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWorkflowsParse loads every GitHub Actions workflow as YAML. GitHub
// rejects a workflow file that does not parse, and nothing else runs it
// before it is needed: an unquoted ": " in a one-line `run:` value is
// enough to stop the nightly, and the release that calls it.
func TestWorkflowsParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflows under .github/workflows")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var wf map[string]any
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Errorf("%s does not parse: %v", f, err)
			continue
		}
		if _, ok := wf["jobs"]; !ok {
			t.Errorf("%s has no jobs", f)
		}
	}
}

// TestWorkflowsRustToolchain holds every workflow's Rust toolchain, and
// every Rust cache's key, to desktop-app/rust-toolchain.toml's channel.
// The caches are keyed on the compiler through prefix-key, so a workflow
// on another version builds cold and writes a cache nothing else reads.
// rust-cache's environment hash stays on, since turning it off drops the
// lockfile hash with it: the key would never change with the
// dependencies, and a cache that hits its key is never saved again. The
// step before each cache uninstalls every toolchain but the pinned one,
// since the hash covers every installed toolchain and the runner image's
// preinstalled stable would change it with each image update.
func TestWorkflowsRustToolchain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("desktop-app", "rust-toolchain.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^channel\s*=\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("desktop-app/rust-toolchain.toml has no channel")
	}
	channel := string(m[1])

	files, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	pins, caches := 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string            `yaml:"uses"`
					With map[string]string `yaml:"with"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, job := range wf.Jobs {
			for i, step := range job.Steps {
				switch {
				case strings.HasPrefix(step.Uses, "dtolnay/rust-toolchain@"):
					pins++
					if got := step.With["toolchain"]; got != channel {
						t.Errorf("%s, job %s: Rust toolchain %q, rust-toolchain.toml says %q", f, name, got, channel)
					}
				case strings.HasPrefix(step.Uses, "Swatinem/rust-cache@"):
					caches++
					if got, want := step.With["prefix-key"], "v0-rust-"+channel; got != want {
						t.Errorf("%s, job %s: rust-cache prefix-key %q, want %q", f, name, got, want)
					}
					if got := step.With["add-rust-environment-hash-key"]; got != "" && got != "true" {
						t.Errorf("%s, job %s: rust-cache add-rust-environment-hash-key %q: off, the key would not hash the lockfiles", f, name, got)
					}
					if i == 0 || job.Steps[i-1].Uses != "./.github/actions/only-pinned-rust" {
						t.Errorf("%s, job %s: rust-cache is not right after ./.github/actions/only-pinned-rust", f, name)
					}
				}
			}
		}
	}
	if pins == 0 {
		t.Error("no workflow installs Rust with dtolnay/rust-toolchain")
	}
	if caches == 0 {
		t.Error("no workflow caches Rust with Swatinem/rust-cache")
	}
}

// TestWindowsSignCommandFindsItsScript resolves the script that
// desktop-app/Makefile's WINDOWS_SIGN names from src-tauri, the directory
// the tauri CLI runs its sign command in. Nothing else exercises the
// command before a release (dev builds never sign), and a path that does
// not resolve there makes "Package and sign" fail on the first file an
// hour after the tag push.
func TestWindowsSignCommandFindsItsScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("desktop-app", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "WINDOWS_SIGN :=") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("desktop-app/Makefile has no WINDOWS_SIGN")
	}
	m := regexp.MustCompile(`-File (\S+\.ps1)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("WINDOWS_SIGN has no -File <script>: %s", line)
	}
	script := filepath.Join("desktop-app", "src-tauri", filepath.FromSlash(m[1]))
	if _, err := os.Stat(script); err != nil {
		t.Errorf("WINDOWS_SIGN names %s, which does not exist from src-tauri (%s): %v", m[1], script, err)
	}
}
