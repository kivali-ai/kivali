package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// logRow is the n-th row a test appends.
func logRow(n int) IntegrationLogRow {
	return IntegrationLogRow{At: time.Date(2026, 10, 21, 9, 0, n, 0, time.UTC), Level: "info", Message: fmt.Sprintf("line %d", n)}
}

// The log reads back oldest first, the newest n of it, and skips a line
// an append cut short. A tail that came back newest first would put the
// page's log upside down; one that stopped at a bad line would hide
// every line after it.
func TestIntegrationLogTail(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	if rows, err := s.ReadIntegrationLogTail("crm", 10); err != nil || len(rows) != 0 {
		t.Fatalf("no log yet: %v, %v", rows, err)
	}
	for i := 0; i < 5; i++ {
		if err := s.AppendIntegrationLog("crm", logRow(i)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(s.Root(), "integrations", "crm", "log.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"at":"2026-10-21T09:00:05Z","lev` + "\n")
	_ = f.Close()
	if err := s.AppendIntegrationLog("crm", logRow(6)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReadIntegrationLogTail("crm", 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Message)
	}
	if strings.Join(got, "|") != "line 3|line 4|line 6" {
		t.Errorf("tail = %v", got)
	}
	if _, err := s.ReadIntegrationLogTail("../x", 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad name: %v", err)
	}
	if err := s.AppendIntegrationLog("nope", logRow(0)); !errors.Is(err, ErrNotFound) {
		t.Errorf("append to an integration not installed: %v", err)
	}
}

// Past 4000 lines the log is rewritten with its last 2000, so a server
// that writes without end cannot fill the data volume; the lines kept
// are the newest. A message over 8 KiB is cut. A removed integration's
// count does not carry over to one installed again under its name,
// which would trim the new log early.
func TestIntegrationLogIsCapped(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	path := filepath.Join(s.Root(), "integrations", "crm", "log.jsonl")
	for i := 0; i < integrationLogMax; i++ {
		if err := s.AppendIntegrationLog("crm", logRow(i)); err != nil {
			t.Fatal(err)
		}
	}
	lines := func() int {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Count(b, []byte{'\n'})
	}
	if n := lines(); n != integrationLogMax {
		t.Fatalf("at the cap: %d lines", n)
	}
	if err := s.AppendIntegrationLog("crm", logRow(integrationLogMax)); err != nil {
		t.Fatal(err)
	}
	if n := lines(); n != integrationLogKeep {
		t.Fatalf("past the cap: %d lines, want %d", n, integrationLogKeep)
	}
	rows, _ := s.ReadIntegrationLogTail("crm", 1)
	if len(rows) != 1 || rows[0].Message != fmt.Sprintf("line %d", integrationLogMax) {
		t.Errorf("newest = %+v", rows)
	}
	rows, _ = s.ReadIntegrationLogTail("crm", integrationLogKeep+10)
	if len(rows) != integrationLogKeep || rows[0].Message != fmt.Sprintf("line %d", integrationLogMax-integrationLogKeep+1) {
		t.Errorf("oldest kept = %+v (of %d)", rows[0], len(rows))
	}

	long := IntegrationLogRow{Level: "stderr", Message: strings.Repeat("x", integrationLogMessageMax+100)}
	if err := s.AppendIntegrationLog("crm", long); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ReadIntegrationLogTail("crm", 1)
	if len(rows[0].Message) != integrationLogMessageMax {
		t.Errorf("long message kept %d bytes", len(rows[0].Message))
	}

	if err := s.RemoveIntegration("crm"); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, s, "crm", "1.0.0")
	for i := 0; i < 10; i++ {
		if err := s.AppendIntegrationLog("crm", logRow(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := lines(); n != 10 {
		t.Errorf("after reinstall: %d lines", n)
	}
}

// A scoped credential reaches the proxy's file with its hosts,
// normalized, and leaves it when dropped; it is kept beside the hashes
// in the data dir file, which backups leave out. This fails if the
// proxy never learns the integration's hosts, or keeps admitting a
// credential after its integration stopped.
func TestEgressScopedCredential(t *testing.T) {
	s, sync := networkStore(t)
	h := egresscred.Hash("scoped-secret")
	if err := s.SetEgressScopedCredential("integration-crm", h, []string{" API.Example.com", "*.cdn.example.com", "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	p := readProxyAgents(t, sync)
	want := map[string]EgressScoped{"integration-crm": {Hash: h, Hosts: []string{"*.cdn.example.com", "api.example.com"}}}
	if !reflect.DeepEqual(p.Scoped, want) {
		t.Fatalf("scoped = %+v", p.Scoped)
	}
	if got := s.EgressScopedCredentials(); !reflect.DeepEqual(got, want) {
		t.Errorf("kept = %+v", got)
	}
	if err := s.SetEgressScopedCredential("integration-crm", "not a hash", nil); err == nil {
		t.Error("a bad hash was taken")
	}
	if err := s.SetEgressScopedCredential("a/b", h, nil); err == nil {
		t.Error("a bad id was taken")
	}
	if err := s.DropEgressScopedCredential("integration-crm"); err != nil {
		t.Fatal(err)
	}
	if p := readProxyAgents(t, sync); len(p.Scoped) != 0 {
		t.Errorf("after drop: %+v", p.Scoped)
	}
	if err := s.DropEgressScopedCredential("integration-crm"); err != nil {
		t.Errorf("second drop: %v", err)
	}
}
