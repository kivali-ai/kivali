package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

func softwareRuntime(t *testing.T) *Runtime {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	return &Runtime{Store: st}
}

func TestApplySoftwareInstallAddsToTheAgentsOwnList(t *testing.T) {
	r := softwareRuntime(t)
	if err := r.Store.WriteOrgSoftware([]string{"git"}); err != nil {
		t.Fatal(err)
	}
	added, err := r.ApplySoftwareInstall(store.SoftwareInstall{Slug: "alice", Packages: []string{"ffmpeg", "git"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(added, ",") != "ffmpeg" {
		t.Errorf("added = %v, want only what the shell did not already get", added)
	}
	a, _ := r.Store.GetAgent("alice")
	if strings.Join(a.Software, ",") != "ffmpeg" {
		t.Errorf("own list = %v", a.Software)
	}
	// Approving it again adds nothing and is not an error.
	added, err = r.ApplySoftwareInstall(store.SoftwareInstall{Slug: "alice", Packages: []string{"ffmpeg"}})
	if err != nil || len(added) != 0 {
		t.Errorf("repeat = %v, %v", added, err)
	}
}

func TestApplySoftwareInstallRefusals(t *testing.T) {
	r := softwareRuntime(t)
	for name, si := range map[string]store.SoftwareInstall{
		"the owner":    {Slug: CEOSlug, Packages: []string{"ffmpeg"}},
		"unknown":      {Slug: "ghost", Packages: []string{"ffmpeg"}},
		"bad slug":     {Slug: "../etc", Packages: []string{"ffmpeg"}},
		"bad name":     {Slug: "alice", Packages: []string{"--force-yes"}},
		"no such list": {Slug: "ghost", Packages: nil},
	} {
		if _, err := r.ApplySoftwareInstall(si); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInboxSpeechForSoftwareInstall(t *testing.T) {
	yes, no := true, false
	resp := func(ok *bool) store.Message {
		return store.Message{
			Type: store.MsgCEOApprovalResponse, Title: "Approved: Install software: PDF tools",
			From: "ceo", To: store.Recipients{"alice"}, Approved: ok,
			SoftwareInstall: &store.SoftwareInstall{Slug: "alice", Packages: []string{"ffmpeg", "poppler-utils"}},
			InReplyTo:       "messages/2026-04-18/req.md",
		}
	}
	got := RenderInboxBody(resp(&yes), "messages/2026-04-18/resp.md")
	for _, w := range []string{"I approved installing `ffmpeg`, `poppler-utils`", "restarted", "wait until installation finishes"} {
		if !strings.Contains(got, w) {
			t.Errorf("approval missing %q in: %s", w, got)
		}
	}
	got = RenderInboxBody(resp(&no), "messages/2026-04-18/resp.md")
	if !strings.Contains(got, "I denied installing `ffmpeg`, `poppler-utils`") {
		t.Errorf("denial = %s", got)
	}
}

// The approval speech says what happened to the shell, from the
// response's Outcome.
func TestInboxSpeechForSoftwareInstallOutcomes(t *testing.T) {
	yes := true
	for outcome, want := range map[store.SoftwareOutcome][]string{
		store.SoftwareRestarted:        {"I approved installing `ffmpeg`.", "Your shell restarted to install it", "If something cannot be installed"},
		store.SoftwareAlreadyInstalled: {"`ffmpeg` is already in your shell."},
		store.SoftwareSavedNotApplied:  {"I approved installing `ffmpeg`. It is saved and installs the next time your shell starts."},
		store.SoftwareRestartWhenIdle:  {"I approved installing `ffmpeg`.", "Your shell restarts to install it once your current turn and background tasks have ended", "If something cannot be installed"},
	} {
		msg := store.Message{
			Type: store.MsgCEOApprovalResponse, Title: "Approved: Install software",
			From: "ceo", To: store.Recipients{"alice"}, Approved: &yes,
			SoftwareInstall: &store.SoftwareInstall{Slug: "alice", Packages: []string{"ffmpeg"}, Outcome: outcome},
			InReplyTo:       "messages/2026-04-18/req.md",
		}
		got := RenderInboxBody(msg, "messages/2026-04-18/resp.md")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in: %s", outcome, w, got)
			}
		}
		if outcome != store.SoftwareRestarted && strings.Contains(got, "restarted") {
			t.Errorf("%s: says the shell restarted: %s", outcome, got)
		}
	}
}
