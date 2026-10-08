package web

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These drive POST /api/v1/org/skills, the one upload route: it adds a
// skill or replaces the installed one of the same name (the name comes
// from the SKILL.md frontmatter), refusing a version that is not newer
// unless confirmed. TestAPISkills (api_org_test.go) covers the list,
// enable/disable and delete around it.

// TestSkillsCreateFromZipDerivesNameFromManifest is the happy-path
// create: a zip with a SKILL.md frontmatter `name: alpha` lands on
// disk as data/skills/alpha/ without the user typing the name.
func TestSkillsCreateFromZipDerivesNameFromManifest(t *testing.T) {
	srv := newTestServer(t)
	body, ct := buildSkillUpload(t, "alpha.zip", map[string][]byte{
		"SKILL.md":       []byte("---\nname: alpha\nversion: 1.0.0\ndescription: hi\n---\n"),
		"scripts/run.sh": []byte("#!/bin/bash\necho hi\n"),
	}, true /* zip */)

	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", rr.Code, rr.Body.String())
	}
	skill, err := srv.Store.ReadSkill("alpha")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if skill.Description != "hi" {
		t.Errorf("Description=%q", skill.Description)
	}
	if skill.Version != "1.0.0" {
		t.Errorf("Version=%q want 1.0.0", skill.Version)
	}
}

// A skill that ships with Kivali is refused as an upload target even
// with force: the next deploy re-materialises it from the binary, so a
// replacement would quietly stop working.
func TestSkillsUploadRefusesToReplaceABuiltin(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.InstallBuiltinSkills(); err != nil {
		t.Fatalf("InstallBuiltinSkills: %v", err)
	}
	listed, err := srv.Store.ListSkills()
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, sk := range listed {
		if sk.Builtin {
			name = sk.Name
			break
		}
	}
	if name == "" {
		t.Fatal("no built-in skill installed")
	}
	before, _ := srv.Store.ReadSkill(name)
	body, ct := buildSkillUpload(t, "SKILL.md", map[string][]byte{
		"SKILL.md": []byte("---\nname: " + name + "\nversion: 999.0.0\ndescription: mine\n---\n"),
	}, false)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills?force=1", body, ct)
	if rr.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", rr.Code, rr.Body.String())
	}
	if after, _ := srv.Store.ReadSkill(name); after.Version != before.Version || after.Description != before.Description {
		t.Errorf("built-in %q changed: %+v -> %+v", name, before, after)
	}
}

// TestSkillsCreateRejectsMalformedManifest hits the frontmatter
// validator through the handler — a SKILL.md without `name:` should
// bounce with 400.
func TestSkillsCreateRejectsMalformedManifest(t *testing.T) {
	srv := newTestServer(t)
	body, ct := buildSkillUpload(t, "x.zip", map[string][]byte{
		"SKILL.md": []byte("---\ndescription: no name here\nversion: 1.0.0\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "name:") {
		t.Errorf("error should mention missing name, got %q", rr.Body.String())
	}
}

// TestSkillsCreateRejectsMissingVersion guards the version-required
// invariant. A SKILL.md without `version:` must bounce with 400 — old
// skills can grandfather-in as "0.0.0" on read, but new uploads have
// to declare a version explicitly.
func TestSkillsCreateRejectsMissingVersion(t *testing.T) {
	srv := newTestServer(t)
	body, ct := buildSkillUpload(t, "noversion.zip", map[string][]byte{
		"SKILL.md": []byte("---\nname: noversion\ndescription: forgot the version\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "version:") {
		t.Errorf("error should mention missing version, got %q", rr.Body.String())
	}
}

// TestSkillsCreateRejectsBadVersion enforces semver-ish parsing —
// "1.2" / "v1" / "latest" all bounce so the downgrade comparator
// never sees garbage.
func TestSkillsCreateRejectsBadVersion(t *testing.T) {
	srv := newTestServer(t)
	for _, bad := range []string{"1.2", "v1.0.0", "latest", "1.0.0.0"} {
		body, ct := buildSkillUpload(t, "v.zip", map[string][]byte{
			"SKILL.md": []byte("---\nname: badver\nversion: " + bad + "\n---\n"),
		}, true)
		rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("version=%q code=%d body=%s", bad, rr.Code, rr.Body.String())
		}
	}
}

// TestSkillsReplaceHappyPath updates an existing skill in place when
// the upload carries a newer version.
func TestSkillsReplaceHappyPath(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteSkillFromManifest("gamma", "---\nname: gamma\nversion: 1.0.0\ndescription: v1\n---\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildSkillUpload(t, "gamma.zip", map[string][]byte{
		"SKILL.md":       []byte("---\nname: gamma\nversion: 1.0.1\ndescription: v2\n---\n"),
		"scripts/run.sh": []byte("#!/bin/bash\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusOK {
		t.Fatalf("replace: code=%d body=%s", rr.Code, rr.Body.String())
	}
	got, err := srv.Store.ReadSkill("gamma")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.Description != "v2" {
		t.Errorf("Description=%q (expected v2)", got.Description)
	}
	if got.Version != "1.0.1" {
		t.Errorf("Version=%q (expected 1.0.1)", got.Version)
	}
	if got.FileCount != 2 {
		t.Errorf("FileCount=%d (expected 2)", got.FileCount)
	}
}

// TestSkillsReplaceBlocksDowngrade: a replace with an older version
// returns 409 + downgrade headers instead of silently overwriting. The
// on-disk bytes must be left untouched so the confirm dialog has a
// stable target to re-post against.
func TestSkillsReplaceBlocksDowngrade(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteSkillFromManifest("eps", "---\nname: eps\nversion: 2.0.0\ndescription: current\n---\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildSkillUpload(t, "eps.zip", map[string][]byte{
		"SKILL.md": []byte("---\nname: eps\nversion: 1.5.0\ndescription: older\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusConflict {
		t.Fatalf("downgrade: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Kivali-Downgrade") != "1" {
		t.Errorf("missing X-Kivali-Downgrade header")
	}
	if got := rr.Header().Get("X-Kivali-Skill-Old-Version"); got != "2.0.0" {
		t.Errorf("X-Kivali-Skill-Old-Version=%q", got)
	}
	if got := rr.Header().Get("X-Kivali-Skill-New-Version"); got != "1.5.0" {
		t.Errorf("X-Kivali-Skill-New-Version=%q", got)
	}
	// On-disk version must not have moved.
	got, err := srv.Store.ReadSkill("eps")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.Version != "2.0.0" || got.Description != "current" {
		t.Errorf("downgrade still wrote to disk: Version=%q Description=%q", got.Version, got.Description)
	}
}

// TestSkillsReplaceBlocksEqualVersion treats a same-version upload of
// an installed skill as a downgrade for confirmation purposes —
// otherwise "edit and re-upload the same version" silently overwrites
// the bytes with no audit trail.
func TestSkillsReplaceBlocksEqualVersion(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteSkillFromManifest("zeta", "---\nname: zeta\nversion: 1.0.0\ndescription: a\n---\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildSkillUpload(t, "zeta.zip", map[string][]byte{
		"SKILL.md": []byte("---\nname: zeta\nversion: 1.0.0\ndescription: b\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusConflict {
		t.Fatalf("equal version: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Kivali-Downgrade") != "1" {
		t.Errorf("missing X-Kivali-Downgrade header")
	}
	if got, _ := srv.Store.ReadSkill("zeta"); got.Description != "a" {
		t.Errorf("equal-version upload was written: Description=%q", got.Description)
	}
}

// TestSkillsReplaceForceOverridesDowngrade lets the confirm dialog
// land an older version when the user explicitly asks: `?force=1` on
// the URL.
func TestSkillsReplaceForceOverridesDowngrade(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteSkillFromManifest("eta", "---\nname: eta\nversion: 2.0.0\ndescription: current\n---\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, ct := buildSkillUpload(t, "eta.zip", map[string][]byte{
		"SKILL.md": []byte("---\nname: eta\nversion: 1.5.0\ndescription: rollback\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills?force=1", body, ct)
	if rr.Code != http.StatusOK {
		t.Fatalf("force replace: code=%d body=%s", rr.Code, rr.Body.String())
	}
	got, err := srv.Store.ReadSkill("eta")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.Version != "1.5.0" || got.Description != "rollback" {
		t.Errorf("force replace didn't land: Version=%q Description=%q", got.Version, got.Description)
	}
}

// TestSkillsReplaceFromUnversioned verifies the grace floor: a skill
// on disk with no `version:` reads as 0.0.0, so the first
// upload with any valid version is always a bump — no force flag
// needed.
func TestSkillsReplaceFromUnversioned(t *testing.T) {
	srv := newTestServer(t)
	// Seed without a version: line. loadSkill stamps 0.0.0.
	if err := srv.Store.WriteSkillFromManifest("legacy", "---\nname: legacy\ndescription: pre-version\n---\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	existing, err := srv.Store.ReadSkill("legacy")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if existing.Version != "0.0.0" {
		t.Fatalf("legacy version=%q want 0.0.0 floor", existing.Version)
	}
	body, ct := buildSkillUpload(t, "legacy.zip", map[string][]byte{
		"SKILL.md": []byte("---\nname: legacy\nversion: 0.1.0\ndescription: first versioned\n---\n"),
	}, true)
	rr := postSkillUpload(t, srv, "/api/v1/org/skills", body, ct)
	if rr.Code != http.StatusOK {
		t.Fatalf("replace: code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// ---- helpers ----

func buildSkillUpload(t *testing.T, filename string, files map[string][]byte, asZip bool) (io.Reader, string) {
	t.Helper()
	var upload bytes.Buffer
	if asZip {
		zw := zip.NewWriter(&upload)
		for name, body := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatalf("zip create %s: %v", name, err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatalf("zip write %s: %v", name, err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zip close: %v", err)
		}
	} else {
		// Single file upload (SKILL.md path) — only one entry expected.
		for _, body := range files {
			upload.Write(body)
			break
		}
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(upload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &form, mw.FormDataContentType()
}

// postSkillUpload POSTs a multipart upload to an API path from this
// origin, with the session cookie.
func postSkillUpload(t *testing.T, srv *Server, path string, body io.Reader, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("content-type", ct)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}
