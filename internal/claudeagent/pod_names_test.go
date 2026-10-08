package claudeagent_test

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/claudeagent"
)

// TestAgentPodCarriesTheClaudeNames renders an agent pod with the real
// Claude driver's Credentials and pins the names deployments already
// carry: the claude-home volume, mounted at /data/claude-home (where the
// image points the CLI's HOME) from the claude-home subPath of the data
// PVC. Renaming any of them would sign every agent pod out. No
// credential variable reaches the pod: the sign-in under HOME is the
// only one.
func TestAgentPodCarriesTheClaudeNames(t *testing.T) {
	c := agentpod.DefaultsForNamespace("kivali-test")
	c.Image = "ghcr.io/kivali-ai/kivali:test"
	c.UseCredentials(claudeagent.New(claudeagent.Options{}).Credentials())

	pod := c.PodManifest("alice")
	spec := pod["spec"].(map[string]any)
	var agentC map[string]any
	for _, x := range spec["containers"].([]any) {
		if m := x.(map[string]any); m["name"] == "agent" {
			agentC = m
		}
	}
	if agentC == nil {
		t.Fatal("no agent container")
	}

	var mount map[string]any
	for _, m := range agentC["volumeMounts"].([]map[string]any) {
		if m["name"] == "claude-home" {
			mount = m
		}
	}
	if mount == nil || mount["mountPath"] != "/data/claude-home" || mount["subPath"] != "claude-home" {
		t.Errorf("claude-home mount = %v, want mountPath /data/claude-home subPath claude-home", mount)
	}

	var vol map[string]any
	for _, v := range spec["volumes"].([]map[string]any) {
		if v["name"] == "claude-home" {
			vol = v
		}
	}
	if vol == nil {
		t.Error("no claude-home volume")
	} else if pvc, _ := vol["persistentVolumeClaim"].(map[string]any); pvc["claimName"] != "kivali-data" {
		t.Errorf("claude-home volume = %v, want the kivali-data PVC", vol)
	}

	for _, e := range agentC["env"].([]map[string]any) {
		switch e["name"] {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN":
			t.Errorf("agent env carries %s", e["name"])
		}
	}
}
