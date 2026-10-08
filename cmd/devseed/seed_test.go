package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func seedInto(t *testing.T, scenario string) (string, *store.FSStore) {
	t.Helper()
	dir := t.TempDir()
	if err := Seed(dir, scenario, testNow); err != nil {
		t.Fatalf("seed %s: %v", scenario, err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, st
}

func TestSeedDemo(t *testing.T) {
	_, st := seedInto(t, ScenarioDemo)

	br, err := st.ReadBranding()
	if err != nil || br.CompanyName != OrgName || !br.HasFavicon {
		t.Errorf("branding = %+v, %v", br, err)
	}
	agents, err := st.ListActiveAgents()
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, a := range agents {
		slugs = append(slugs, a.Slug)
	}
	want := "bookkeeper,ceo,chief-of-staff,engineering-lead,support-lead,test-runner"
	if got := strings.Join(slugs, ","); got != want {
		t.Errorf("agents = %s, want %s", got, want)
	}

	set, err := st.ReadAssignmentSet()
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]*assignments.Assignment{}
	goals := 0
	for _, id := range set.IDs() {
		iss, _ := set.Get(id)
		byTitle[iss.Title] = iss
		if iss.Parent == 0 && iss.Open() && (len(iss.Acceptance) > 0 || len(set.Parts(id)) > 0) {
			goals++
		}
	}
	if goals != 3 {
		t.Errorf("goals = %d, want 3", goals)
	}
	mail := byTitle[GoalMailSwitch]
	items := map[string]assignments.ConditionState{}
	for _, it := range set.Items(mail.ID) {
		items[it.Name] = it.State()
	}
	if items[ItemDelivered] != assignments.ConditionMet || items[ItemSwitched] != assignments.ConditionClaimed || items[ItemBounces] != assignments.ConditionUnclaimed {
		t.Errorf("done-when states = %v", items)
	}
	if sw := byTitle[PartSwitch]; len(sw.BlockedBy) != 1 || sw.BlockedBy[0] != byTitle[PartTestWeek].ID {
		t.Errorf("%s blocked_by = %v", PartSwitch, sw.BlockedBy)
	}
	if !byTitle[PartScreenshots].Held {
		t.Errorf("%s is not held", PartScreenshots)
	}
	closed := 0
	for _, iss := range byTitle {
		if !iss.Open() && iss.Closed != nil && testNow.Sub(*iss.Closed) < 7*24*time.Hour {
			closed++
		}
	}
	if closed != 2 {
		t.Errorf("closed this week = %d, want 2", closed)
	}
	if a := byTitle[CEOAssignment]; a.Assignee != SlugCEO {
		t.Errorf("CEO assignment = %+v", a)
	}

	// The CEO's inbox: the assignment, the hire, the notification.
	hist, err := st.ReadChatHistory(SlugCEO)
	if err != nil {
		t.Fatal(err)
	}
	var hire *store.Message
	var inbox []string
	for _, m := range hist {
		if m.Kind != "ceo_inbox" {
			continue
		}
		inbox = append(inbox, m.Content)
		msg, err := st.ReadMessage(filepath.Join(st.Root(), m.MessageRef))
		if err != nil {
			t.Fatal(err)
		}
		if msg.Hire != nil {
			hire = &msg
		}
	}
	if len(inbox) != 3 {
		t.Errorf("ceo inbox = %q", inbox)
	}
	if hire == nil || hire.Title != HireTitle || hire.Hire.Slug != HireSlug || hire.Hire.ReportsTo != SlugSupportLead {
		t.Errorf("hire proposal = %+v", hire)
	}

	// The queue: two notices and one assignment event.
	q, err := st.ReadMessageQueue()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, aq := range q.Agents {
		for _, p := range aq.Inbox {
			paths[p] = true
		}
	}
	kinds := map[store.MessageType]int{}
	for p := range paths {
		m, err := st.ReadMessage(filepath.Join(st.Root(), p))
		if err != nil {
			t.Fatal(err)
		}
		kinds[m.Type]++
	}
	if len(paths) != 3 || kinds[store.MsgNotice] != 2 || kinds[store.MsgAssignmentEvent] != 1 {
		t.Errorf("queue = %v", kinds)
	}

	// Test runner is held on an error; everyone else owes nothing.
	for _, a := range agents {
		h, _ := st.ReadChatHistory(a.Slug)
		v := store.SpawnDecision(h)
		switch a.Slug {
		case SlugTestRunner:
			if v != store.SpawnHoldOnError {
				t.Errorf("test-runner verdict = %v, want hold on error", v)
			}
		case SlugCEO:
		default:
			if v != store.SpawnIdle {
				t.Errorf("%s verdict = %v, want idle", a.Slug, v)
			}
		}
	}

	eng, err := st.ReadChatHistory(SlugEngLead)
	if err != nil {
		t.Fatal(err)
	}
	var sawAsk, sawReply bool
	for _, m := range eng {
		sawAsk = sawAsk || m.Content == CEOChatAsk
		sawReply = sawReply || m.Content == AgentReplyMD
	}
	if !sawAsk || !sawReply {
		t.Errorf("engineering-lead chat lacks the CEO ask or the reply")
	}
	// The trial notice the Engineering lead published is a sent row naming a
	// message file that exists.
	var sent *store.ChatMessage
	for i := range eng {
		if eng[i].Kind == "doc_published" {
			sent = &eng[i]
		}
	}
	if sent == nil {
		t.Error("engineering-lead chat has no doc_published row")
	} else if m, err := st.ReadMessage(filepath.Join(st.Root(), sent.MessageRef)); err != nil || m.Title != NoticeTrial {
		t.Errorf("doc_published %q: message %+v, %v", sent.MessageRef, m, err)
	}
	if past, err := st.ListArchivedChats(SlugEngLead); err != nil || len(past) != 1 || !st.HasEpisode(SlugEngLead, past[0].Timestamp) {
		t.Errorf("archived chats = %+v, %v", past, err)
	}

	rows, err := st.ReadUsageSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 101 {
		t.Errorf("usage rows = %d, want 101", len(rows))
	}
	unpriced := 0
	for _, r := range rows {
		if r.Model == UnpricedModelID {
			unpriced++
		}
	}
	if unpriced != 1 {
		t.Errorf("unpriced rows = %d", unpriced)
	}

	ix, err := st.ReadGraphIndex()
	if err != nil {
		t.Fatal(err)
	}
	flagged, problem := ix.Nodes["test-runner/test-week-plan"], ix.Nodes["support-lead/help-pages-brief"]
	if flagged == nil || !flagged.Flagged {
		t.Errorf("flagged node = %+v", flagged)
	}
	if problem == nil || len(problem.Problems) == 0 {
		t.Errorf("problem node = %+v", problem)
	}

	if al, _ := st.ReadEgressAllowlist(); !strings.Contains(strings.Join(al.Patterns, ","), EgressHost) {
		t.Errorf("egress = %v", al.Patterns)
	}
	if !st.SkillEnabled(BuiltinSkill) || !st.SkillEnabled(CustomSkill) {
		t.Error("skills not enabled")
	}
	if pfs, _ := st.ListProjectFiles(); len(pfs) != 2 {
		t.Errorf("project files = %d", len(pfs))
	}
	if st.ReadAutoRelease().Enabled {
		t.Error("auto-release is on")
	}
}

func TestSeedEmpty(t *testing.T) {
	_, st := seedInto(t, ScenarioEmpty)
	agents, _ := st.ListActiveAgents()
	if len(agents) != 2 {
		t.Errorf("agents = %+v", agents)
	}
	if _, err := st.GetAgent(SlugChiefOfStaff); err != nil {
		t.Error("no Chief of Staff: setup would not count as done")
	}
	if set, _ := st.ReadAssignmentSet(); set.Len() != 0 {
		t.Errorf("assignments = %d", set.Len())
	}
	if br, _ := st.ReadBranding(); br.CompanyName != OrgName {
		t.Errorf("name = %q", br.CompanyName)
	}
}

func TestSeedSetup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := Seed(dir, ScenarioSetup, testNow); err != nil {
		t.Fatal(err)
	}
	if empty, err := dirEmpty(dir); err != nil || !empty {
		t.Errorf("setup dir empty = %v, %v", empty, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("setup dir: %v", err)
	}
}

func TestSeedRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Seed(dir, ScenarioDemo, testNow); err == nil {
		t.Fatal("seeded a non-empty dir")
	}
	if err := run(dir, ScenarioEmpty, testNow.Format(time.RFC3339), true); err != nil {
		t.Fatalf("-force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Error("-force left the old file")
	}
}

// Two seeds with the same now write the same assignments, messages, queue
// and chats, byte for byte.
func TestSeedDeterministic(t *testing.T) {
	a, _ := seedInto(t, ScenarioDemo)
	b, _ := seedInto(t, ScenarioDemo)
	for _, sub := range []string{"assignments", "messages", "message_queue.json", "usage.jsonl", "agents"} {
		fa, fb := readTree(t, a, sub), readTree(t, b, sub)
		if len(fa) == 0 {
			t.Errorf("%s: nothing written", sub)
		}
		if len(fa) != len(fb) {
			t.Errorf("%s: %d files vs %d", sub, len(fa), len(fb))
		}
		for p, body := range fa {
			if !bytes.Equal(body, fb[p]) {
				t.Errorf("%s differs between runs", p)
			}
		}
	}
}

// readTree reads every regular file under root/sub, keyed by its path
// relative to root. Symlinks (the /files overlay) are skipped.
func readTree(t *testing.T, root, sub string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(filepath.Join(root, sub), func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
