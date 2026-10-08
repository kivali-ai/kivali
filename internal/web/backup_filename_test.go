package web

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBackupFilenameNamesTheOrg(t *testing.T) {
	at := time.Date(2026, 10, 6, 19, 23, 33, 0, time.FixedZone("PDT", -7*3600))
	for _, tc := range []struct{ name, want string }{
		{"Acme Corp", "kivali-backup-acme-corp-20261007T022333Z.zip"},
		{"  R&D / Lab #2  ", "kivali-backup-r-d-lab-2-20261007T022333Z.zip"},
		{"Café Ünïcode", "kivali-backup-caf-n-code-20261007T022333Z.zip"},
		{`a"; filename=evil.exe`, "kivali-backup-a-filename-evil-exe-20261007T022333Z.zip"},
		{"", "kivali-backup-20261007T022333Z.zip"},
		{"日本語", "kivali-backup-20261007T022333Z.zip"},
		{strings.Repeat("ab ", 30), "kivali-backup-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-a-20261007T022333Z.zip"},
	} {
		if got := backupFilename(tc.name, at); got != tc.want {
			t.Errorf("backupFilename(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBackupDownloadIsNamedAfterTheOrg(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteCompanyName("Plainsong Labs"); err != nil {
		t.Fatal(err)
	}
	rr := backupZip(t, srv)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	cd := rr.Header().Get("content-disposition")
	if !strings.HasPrefix(cd, "attachment; filename=kivali-backup-plainsong-labs-") || !strings.HasSuffix(cd, "Z.zip") {
		t.Errorf("content-disposition = %q", cd)
	}
}
