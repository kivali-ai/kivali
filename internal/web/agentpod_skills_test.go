package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startSkillsServer wires the skills endpoints on a Unix socket and
// seeds one skill so list/read tests have something to find.
func startSkillsServer(t *testing.T) (path string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	skillsRoot := srv.Store.SkillsDir()
	if err := os.MkdirAll(filepath.Join(skillsRoot, "demo"), 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	manifest := "---\ndescription: A demo skill\nwhen_to_use: Whenever asked to demo\n---\n# Demo\n\nbody.\n"
	if err := os.WriteFile(filepath.Join(skillsRoot, "demo", store.SkillManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsRoot, "demo", "extra.sh"), []byte("#!/bin/bash\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write file: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/skills", srv.handleAgentpodSkillsList)
	mux.HandleFunc("GET /v1/skills/{name}", srv.handleAgentpodSkill)
	mux.HandleFunc("GET /v1/skills/{name}/manifest", srv.handleAgentpodSkillManifest)
	mux.HandleFunc("GET /v1/skills/", srv.handleAgentpodSkillFile)

	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return path, srv, cleanup
}

// TestAgentpodSkillsRoundtrip: list shows the seeded demo skill;
// per-name fetch returns it with metadata + file tree; manifest
// returns the raw SKILL.md body; file fetch returns the script bytes.
func TestAgentpodSkillsRoundtrip(t *testing.T) {
	path, _, cleanup := startSkillsServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	skills, err := c.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "demo" {
		t.Fatalf("ListSkills = %+v, want [demo]", skills)
	}
	if skills[0].Description != "A demo skill" {
		t.Errorf("Description = %q, want 'A demo skill'", skills[0].Description)
	}

	sk, err := c.ReadSkill(ctx, "demo")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if sk.Name != "demo" {
		t.Errorf("Name = %q, want demo", sk.Name)
	}
	if len(sk.Files) < 1 {
		t.Errorf("expected at least one file in demo skill, got %d", len(sk.Files))
	}

	manifest, err := c.ReadSkillManifest(ctx, "demo")
	if err != nil {
		t.Fatalf("ReadSkillManifest: %v", err)
	}
	if !strings.Contains(manifest, "demo skill") {
		t.Errorf("manifest body unexpected: %q", manifest)
	}

	body, err := c.ReadSkillFile(ctx, "demo", "extra.sh")
	if err != nil {
		t.Fatalf("ReadSkillFile: %v", err)
	}
	if !strings.Contains(string(body), "echo hi") {
		t.Errorf("file body = %q, want script content", string(body))
	}
}

// TestAgentpodSkillsMissing returns 404.
func TestAgentpodSkillsMissing(t *testing.T) {
	path, _, cleanup := startSkillsServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	if _, err := c.ReadSkill(context.Background(), "ghost"); err == nil {
		t.Error("expected error for missing skill; got nil")
	}
}

// TestAgentpodSkillsEmptyListReturnsEmptyArray locks the never-null
// invariant for the list endpoint.
func TestAgentpodSkillsEmptyListReturnsEmptyArray(t *testing.T) {
	srv := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/skills", srv.handleAgentpodSkillsList)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = httpSrv.Serve(ln)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()
	c := agentpod.NewClient(path, "alice")
	skills, err := c.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if skills == nil {
		t.Error("expected non-nil empty slice; got nil")
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}
