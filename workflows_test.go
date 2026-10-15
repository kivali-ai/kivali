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
// The cache's environment hash stays off: it covers every toolchain on
// the runner, the image's preinstalled one among them, so a runner image
// update would miss every cache.
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
			for _, step := range job.Steps {
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
					if got := step.With["add-rust-environment-hash-key"]; got != "false" {
						t.Errorf("%s, job %s: rust-cache add-rust-environment-hash-key %q, want false", f, name, got)
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
