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

// TestWorkflowsRustToolchain holds every workflow's Rust toolchain to
// desktop-app/rust-toolchain.toml's channel. The Rust caches are keyed on
// the compiler, so a workflow on another version builds cold and writes a
// cache nothing else reads.
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
	pins := 0
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
				if !strings.HasPrefix(step.Uses, "dtolnay/rust-toolchain@") {
					continue
				}
				pins++
				if got := step.With["toolchain"]; got != channel {
					t.Errorf("%s, job %s: Rust toolchain %q, rust-toolchain.toml says %q", f, name, got, channel)
				}
			}
		}
	}
	if pins == 0 {
		t.Error("no workflow installs Rust with dtolnay/rust-toolchain")
	}
}
