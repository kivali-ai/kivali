package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// episodeHarness wires a server whose only Claude call is the episode
// writer's, on a fake clock so the retry delay is driven rather than
// waited out. The writer goroutine is NOT started by newTestServer
// (Claude is nil at construction) — each test starts it, which is what
// lets "an archive alone produces nothing" be asserted.
type episodeHarness struct {
	srv   *Server
	fake  *clock.Fake
	calls chan provider.CompleteRequest
	fail  atomic.Bool
}

const testEpisodeReply = `title: Q3 plan review with the CEO
touched: q3-plan.md, chief-of-staff
---
Asked: The CEO asked whether Q3 was on track.
Did: The agent read q3-plan.md and confirmed the milestones.
Concluded: Q3 is on track.
Left open: nothing`

func newEpisodeHarness(t *testing.T) *episodeHarness {
	t.Helper()
	h := &episodeHarness{
		srv:   newTestServer(t),
		fake:  clock.NewFake(),
		calls: make(chan provider.CompleteRequest, 8),
	}
	h.srv.Clock = h.fake
	h.srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			h.calls <- req
			if h.fail.Load() {
				return nil, errors.New("haiku down")
			}
			return &provider.CompleteResponse{
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: testEpisodeReply}},
			}, nil
		},
	}
	if err := h.srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return h
}

// chat appends a transcript to alice's live chat: the CEO asking, the
// agent reading a file, the answer — plus the rotation prompt, which
// the writer must strip.
func (h *episodeHarness) chat(t *testing.T, topic string) {
	t.Helper()
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "how's " + topic + "?", Kind: "direct_chat"},
		{Role: store.RoleSent, Content: `{"path":"/files/project/q3-plan.md"}`, Kind: "tool_use", ToolName: "file_view", ToolUseID: "tu1", ToolInput: `{"path":"/files/project/q3-plan.md"}`},
		{Role: store.RoleReceived, Content: "milestone 1 done, milestone 2 done", Kind: "tool_result", ToolUseID: "tu1"},
		{Role: store.RoleSent, Content: topic + " is on track", Kind: "direct_chat"},
		{Role: store.RoleReceived, Content: "ROTATION-BOILERPLATE reconcile your memory", Kind: "rotation_prompt"},
	} {
		if err := h.srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func (h *episodeHarness) archive(t *testing.T) string {
	t.Helper()
	ts, err := h.srv.Store.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	return ts
}

func (h *episodeHarness) awaitCall(t *testing.T) provider.CompleteRequest {
	t.Helper()
	select {
	case req := <-h.calls:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("episode writer never called Claude")
		return provider.CompleteRequest{}
	}
}

func (h *episodeHarness) assertNoCall(t *testing.T, when string) {
	t.Helper()
	select {
	case <-h.calls:
		t.Fatalf("%s: Claude was called and should not have been", when)
	default:
	}
}

// waitForEpisode blocks until the digest for ts is on disk AND linked
// under /files/episodes/. The Claude call is the only synchronization
// point the mock exposes; the write and the link happen after it
// returns, so poll for the artifacts with a deadline.
func waitForEpisode(t *testing.T, srv *Server, slug, ts string) string {
	t.Helper()
	link := filepath.Join(files.StorageRoot(filepath.Join(srv.Store.Root(), "agents", slug)), "episodes", ts+".md")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Store.HasEpisode(slug, ts) {
			if _, err := os.Stat(link); err == nil {
				body, err := srv.Store.ReadEpisode(slug, ts)
				if err != nil {
					t.Fatalf("ReadEpisode: %v", err)
				}
				return body
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("episode %s/%s never landed (or never linked at %s)", slug, ts, link)
	return ""
}

func promptText(req provider.CompleteRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// TestEpisodeWriterBootSweepDigestsExistingArchive is the backfill
// contract: an archive that exists before the writer starts is
// digested by the boot sweep with no rotation involved, the file has
// the frontmatter the search relies on, and the agent can read it at
// /files/episodes/<ts>.md.
func TestEpisodeWriterBootSweepDigestsExistingArchive(t *testing.T) {
	h := newEpisodeHarness(t)
	h.chat(t, "Q3")
	ts := h.archive(t)

	// The archive alone produces nothing.
	h.assertNoCall(t, "archive with no writer running")

	go h.srv.runEpisodeWriter()
	req := h.awaitCall(t)
	if req.Purpose != "episode" || req.Model != h.srv.SummaryModel {
		t.Errorf("request purpose=%q model=%q", req.Purpose, req.Model)
	}
	prompt := promptText(req)
	for _, want := range []string{"USER: how's Q3?", "AGENT: Q3 is on track", "TOOL CALL file_view", "TOOL RESULT: milestone 1 done"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("transcript missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "ROTATION-BOILERPLATE") {
		t.Errorf("transcript should drop the rotation prompt:\n%s", prompt)
	}

	body := waitForEpisode(t, h.srv, "alice", ts)
	for _, want := range []string{"---\nts: " + ts + "\n", "title: Q3 plan review with the CEO\n", "touched: q3-plan.md, chief-of-staff\n", "Concluded: Q3 is on track."} {
		if !strings.Contains(body, want) {
			t.Errorf("episode missing %q:\n%s", want, body)
		}
	}
	link := filepath.Join(files.StorageRoot(filepath.Join(h.srv.Store.Root(), "agents", "alice")), "episodes", ts+".md")
	linked, err := os.ReadFile(link)
	if err != nil || string(linked) != body {
		t.Errorf("/files/episodes/%s.md = (%q, %v), want the episode body", ts, linked, err)
	}

	// A successful sweep leaves no retry armed and digests nothing twice.
	waitForWaiters(t, h.fake, 0)
	h.assertNoCall(t, "after the boot sweep")
}

// TestEpisodeWriterNotificationIsIdempotent pins the steady-state
// path: finalizeRotation's NotifyEpisode digests exactly that
// generation, and a repeated notification for a digested generation
// costs no call.
func TestEpisodeWriterNotificationIsIdempotent(t *testing.T) {
	h := newEpisodeHarness(t)
	go h.srv.runEpisodeWriter()
	waitForWaiters(t, h.fake, 0)
	h.assertNoCall(t, "boot sweep with nothing archived")

	h.chat(t, "Q3")
	ts := h.archive(t)
	h.srv.NotifyEpisode("alice", ts)
	if !strings.Contains(promptText(h.awaitCall(t)), "how's Q3?") {
		t.Fatal("first notification digested the wrong transcript")
	}
	waitForEpisode(t, h.srv, "alice", ts)

	// Duplicate, then a genuinely new generation. The loop is
	// sequential, so if the duplicate had produced a call it would be
	// the one received first — and it would carry Q3, not Q4.
	h.srv.NotifyEpisode("alice", ts)
	h.chat(t, "Q4")
	ts2 := h.archive(t)
	h.srv.NotifyEpisode("alice", ts2)
	if prompt := promptText(h.awaitCall(t)); !strings.Contains(prompt, "how's Q4?") {
		t.Fatalf("expected the Q4 digest next, got:\n%s", prompt)
	}
	waitForEpisode(t, h.srv, "alice", ts2)
	h.assertNoCall(t, "after the duplicate notification")
}

// TestEpisodeWriterRetriesAfterAFailedCall: a transient Haiku failure
// leaves a gap, the retry sweep fills it on the writer's clock, and
// the gap is never visible as a half-written file.
func TestEpisodeWriterRetriesAfterAFailedCall(t *testing.T) {
	h := newEpisodeHarness(t)
	h.chat(t, "Q3")
	ts := h.archive(t)
	h.fail.Store(true)

	go h.srv.runEpisodeWriter()
	h.awaitCall(t)
	waitForWaiters(t, h.fake, 1) // retry armed
	if h.srv.Store.HasEpisode("alice", ts) {
		t.Fatal("a failed call must not leave an episode behind")
	}

	h.fail.Store(false)
	h.fake.Advance(episodeRetryDelay)
	h.awaitCall(t)
	waitForEpisode(t, h.srv, "alice", ts)
	waitForWaiters(t, h.fake, 0)
}

// TestEpisodeWriterSkipsEmptyTranscript: a generation nobody spoke in
// is neither digested nor retried forever.
func TestEpisodeWriterSkipsEmptyTranscript(t *testing.T) {
	h := newEpisodeHarness(t)
	ts := h.archive(t)
	go h.srv.runEpisodeWriter()
	waitForWaiters(t, h.fake, 0)
	h.assertNoCall(t, "boot sweep over an empty transcript")
	h.srv.NotifyEpisode("alice", ts)
	// Give a wrongly-issued call every chance to show up before
	// declaring there was none: a second, real generation is digested
	// after it in queue order.
	h.chat(t, "Q3")
	ts2 := h.archive(t)
	h.srv.NotifyEpisode("alice", ts2)
	if prompt := promptText(h.awaitCall(t)); !strings.Contains(prompt, "how's Q3?") {
		t.Fatalf("empty generation produced a call:\n%s", prompt)
	}
	// Let the real generation land before the temp dir is torn down
	// under the writer's link step.
	waitForEpisode(t, h.srv, "alice", ts2)
	if h.srv.Store.HasEpisode("alice", ts) {
		t.Fatal("empty transcript was digested")
	}
}

func TestRenderEpisodeTranscriptElidesTheMiddle(t *testing.T) {
	var msgs []store.ChatMessage
	for i := 0; i < 400; i++ {
		msgs = append(msgs, store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    "direct_chat",
			Content: fmt.Sprintf("message %03d %s", i, strings.Repeat("x", 500)),
		})
	}
	got := renderEpisodeTranscript(msgs)
	if len(got) > episodeInputCharCap+200 {
		t.Fatalf("rendered %d chars, cap is %d", len(got), episodeInputCharCap)
	}
	if !strings.HasPrefix(got, "USER: message 000") {
		t.Errorf("opening message dropped:\n%.120s", got)
	}
	if !strings.Contains(got, "USER: message 399") {
		t.Errorf("closing message dropped")
	}
	if !strings.Contains(got, "messages elided ...]") {
		t.Errorf("no elision marker in a shortened transcript")
	}
	// Whole messages only: no block is cut mid-line.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "USER: message") && !strings.HasSuffix(line, strings.Repeat("x", 500)) {
			t.Fatalf("message cut mid-block: %.60s…", line)
		}
	}

	short := renderEpisodeTranscript(msgs[:3])
	if strings.Contains(short, "elided") {
		t.Errorf("short transcript should not be elided:\n%s", short)
	}
}

func TestParseEpisode(t *testing.T) {
	ep := parseEpisode(testEpisodeReply)
	if ep.title != "Q3 plan review with the CEO" || ep.touched != "q3-plan.md, chief-of-staff" {
		t.Errorf("parsed title=%q touched=%q", ep.title, ep.touched)
	}
	if !strings.HasPrefix(ep.body, "Asked: The CEO asked") || !strings.HasSuffix(ep.body, "Left open: nothing") {
		t.Errorf("body = %q", ep.body)
	}

	// A reply that ignored the format is a body with no title; the
	// writer then falls back to the transcript's opening line rather
	// than retrying a reply the model will keep giving.
	loose := parseEpisode("The agent looked at the plan and said it was fine.")
	if loose.title != "" || loose.body != "The agent looked at the plan and said it was fine." {
		t.Errorf("loose parse = %+v", loose)
	}
	msgs := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "rotation_prompt", Content: "boilerplate"},
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "  please check the Q3 plan\nsecond line"},
	}
	if got := fallbackEpisodeTitle(msgs, "ts"); got != "please check the Q3 plan" {
		t.Errorf("fallback title = %q", got)
	}
	if got := fallbackEpisodeTitle(nil, "20260101T000000.000000000Z"); got != "Chat archived 20260101T000000.000000000Z" {
		t.Errorf("fallback title with nothing said = %q", got)
	}

	file := renderEpisodeFile("TS", episode{title: " T ", body: "B\n"})
	if file != "---\nts: TS\ntitle: T\ntouched: -\n---\nB\n" {
		t.Errorf("rendered file = %q", file)
	}
}

// TestTruncateForSummaryKeepsValidUTF8 — transcripts carry em dashes
// and agent names with accents; a byte-slice cut mid-rune hands the
// model a replacement char right where the text stops, which reads as
// corruption.
func TestTruncateForSummaryKeepsValidUTF8(t *testing.T) {
	body := "status — señor agent finished the résumé review and is awaiting sign-off"
	for n := 1; n < len(body); n++ {
		got := truncateForSummary(body, n)
		if len(got) > n {
			t.Fatalf("n=%d: returned %d chars", n, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d: invalid UTF-8: %q", n, got)
		}
		if !strings.HasPrefix(body, got) {
			t.Fatalf("n=%d: %q is not a prefix of the body", n, got)
		}
	}
	if got := truncateForSummary(body, len(body)); got != body {
		t.Fatalf("exact-fit body was modified")
	}
	if got := truncateForSummary(body, 0); got != "" {
		t.Fatalf("zero budget returned %q", got)
	}
}
