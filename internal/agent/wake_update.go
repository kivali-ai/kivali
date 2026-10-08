package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

// WakeUpdateStore is the slice of the store the wake note reads and
// writes: the per-agent watermark, the three things it reports from
// outside the graph (handbook, role, skills), the assignment set, and
// the nudge flags the note clears once it has said them. Narrow so the
// builder is testable against a bare FSStore without a server.
type WakeUpdateStore interface {
	ReadGraphWatermark(slug string) (store.GraphWatermark, bool, error)
	WriteGraphWatermark(slug string, wm store.GraphWatermark) error
	ReadHandbook() (string, error)
	ReadRole(slug string) (string, error)
	ListEnabledSkills() ([]store.Skill, error)
	ReadAssignmentSet() (*assignments.Set, error)
	ClearAssignmentNudges(ids []int) error
}

// Watermark fingerprint keys. The skills entry is the readable list
// rather than a hash so the note can name what moved.
const (
	graphFPHandbook = "handbook"
	graphFPRole     = "role"
	graphFPSkills   = "skills"
	graphFPFindings = "findings"
)

// WakeUpdateLead opens every wake note. Tests and the page match on
// it.
const WakeUpdateLead = "Update from the runtime at this wake. Nothing here needs a reply; act on it only if it changes what you are about to do."

// WakeUpdate is a built note: the text to append to the agent's chat
// (empty when there is nothing to say), the watermark to record once
// that text has actually landed, and the assignments whose one-time nudge
// the text carries, to be cleared at the same moment. Building and
// committing are separate so the caller commits only after a
// successful append; a note that failed to reach the chat is reported
// again next wake rather than lost.
type WakeUpdate struct {
	Text      string
	Watermark store.GraphWatermark
	Nudged    []int
}

// BuildWakeUpdate builds the runtime's note for slug's wake: one
// received chat entry appended behind the deliveries that woke the
// agent, so the model reads it in the same user message as the work
// it woke for. It reads the store and writes nothing; CommitWakeUpdate
// records the watermark it returns.
//
// What it says, in order (docs/developers/knowledge-graph.md §"The wake note",
// docs/developers/assignments.md §Wakes): nodes the agent owns, nodes about things it
// owns, and nodes it rests on that changed since the watermark, one
// line each; the agent's own index findings when they differ from
// what it was last shown; one line each for the handbook, the
// agent's role and the skills when their fingerprint moved; and the
// assignments the agent holds right now, each marked ready, on hold or
// blocked, ending with the one-time nudge for any that gained parts
// without Done-when conditions. The graph sections are a diff against
// the watermark. The assignments section is the current state, not a diff: it
// is the agent's working set, short by construction, and the point is
// that the agent never has to remember to look. The nudge is the one
// part of it told once, and its flag is cleared at commit.
//
// The first wake sets the watermark silently rather than dumping the
// whole graph, but still lists the agent's open assignments: an agent
// restored or upgraded into a tracker that already assigns it work
// must see that work. It deliberately does not record the findings
// fingerprint: findings that already exist on day one (front matter
// the graph rejects) must still reach the owner once, at the wake
// after.
//
// With no index (the pass failed) the note carries the assignments section
// alone and the watermark stays put.
func BuildWakeUpdate(s WakeUpdateStore, ix *graph.Index, slug string) (WakeUpdate, error) {
	if slug == "" {
		return WakeUpdate{}, nil
	}
	held, nudged := openAssignmentsSection(s, slug)
	if ix == nil {
		return WakeUpdate{Text: assembleWakeUpdate(nil, held), Nudged: nudged}, nil
	}
	prev, seen, err := s.ReadGraphWatermark(slug)
	if err != nil {
		return WakeUpdate{}, err
	}
	findings := ix.OwnerFindings(slug)
	next := store.GraphWatermark{
		Seq: ix.Seq,
		Fingerprints: map[string]string{
			graphFPHandbook: hashOrEmpty(s.ReadHandbook()),
			graphFPRole:     hashOrEmpty(s.ReadRole(slug)),
			graphFPSkills:   skillsFingerprint(s),
			graphFPFindings: hashLines(findings),
		},
	}
	if !seen {
		delete(next.Fingerprints, graphFPFindings)
		return WakeUpdate{Text: assembleWakeUpdate(nil, held), Watermark: next, Nudged: nudged}, nil
	}

	var sections []string
	var relevant []*graph.Node
	if prev.Seq > ix.Seq {
		// The index was rebuilt below the sequence this agent last
		// saw (its file was lost or restored separately from the
		// watermarks). ChangedSince cannot say what moved in between;
		// say so and rebase rather than stay silent until the counter
		// climbs back.
		sections = append(sections, "The graph index was rebuilt since your last turn, so the list of what changed is not available this once. Re-check anything you rely on with graph_query or graph_node.")
	} else {
		for _, n := range ix.ChangedSince(prev.Seq) {
			if n.Type == graph.TypeArtifact && n.Owner == slug {
				// The agent's own artifacts change for one of two
				// reasons: the agent edited them, which it knows, or
				// the index derived something about them, which it
				// does not. Only the second is news: a premise it rests
				// on was withdrawn (flagged), or a peer superseded it.
				// Its own edits are not echoed back, and are not
				// counted among "elsewhere" either. Rejections and
				// problems on its files travel in the findings section.
				if n.Flagged || n.Status == graph.StatusSuperseded {
					relevant = append(relevant, n)
				}
				continue
			}
			// A change that is not about this agent's objects, not
			// something it rests on, and not one of its own flagged or
			// superseded nodes is not news to it, and is not counted
			// either: a count is a card in the CEO's view and a
			// paragraph in the model's context that neither can act
			// on. graph_query is there for whoever wants the rest.
			if ix.RelevantTo(slug, n) {
				relevant = append(relevant, n)
			}
		}
	}
	if len(relevant) > 0 {
		var b strings.Builder
		b.WriteString("Changed in the knowledge graph since your last turn (your nodes that were flagged or superseded, nodes about your objects, and nodes you rest on):\n")
		for _, n := range relevant {
			b.WriteString("- ")
			b.WriteString(n.Line())
			b.WriteByte('\n')
		}
		sections = append(sections, strings.TrimRight(b.String(), "\n"))
	}
	if len(findings) > 0 && prev.Fingerprints[graphFPFindings] != next.Fingerprints[graphFPFindings] {
		var b strings.Builder
		b.WriteString("The index found problems in your published files (artifact_publish reports them when you publish); fix the source and publish again:\n")
		for _, f := range findings {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteByte('\n')
		}
		sections = append(sections, strings.TrimRight(b.String(), "\n"))
	}
	var outside []string
	if moved(prev, next, graphFPHandbook) {
		outside = append(outside, "The handbook changed since your last turn.")
	}
	if moved(prev, next, graphFPRole) {
		outside = append(outside, "Your role changed since your last turn.")
	}
	if moved(prev, next, graphFPSkills) {
		outside = append(outside, "Skills changed: "+skillsDiff(prev.Fingerprints[graphFPSkills], next.Fingerprints[graphFPSkills])+".")
	}
	if len(outside) > 0 {
		sections = append(sections, strings.Join(outside, " "))
	}
	return WakeUpdate{Text: assembleWakeUpdate(sections, held), Watermark: next, Nudged: nudged}, nil
}

// openAssignmentsSection is what the agent holds right now and the
// assignments whose nudge that section carries, or one line saying the
// tracker could not be read. A tracker paused on a file the CEO must
// fix by hand fails every read the same way; the note says so instead
// of dropping the rest of what it has to say, and the agent's
// assignment tools will say the same when it reaches for them.
func openAssignmentsSection(s WakeUpdateStore, slug string) (string, []int) {
	set, err := s.ReadAssignmentSet()
	if err != nil {
		return "Your open assignments could not be listed this wake: " + err.Error(), nil
	}
	return set.OpenAssignments(slug), set.Nudges(slug)
}

// assembleWakeUpdate joins the note: lead, the change sections, then
// what the agent holds. Empty when there is nothing to say at all.
func assembleWakeUpdate(sections []string, held string) string {
	if held != "" {
		sections = append(sections, held)
	}
	if len(sections) == 0 {
		return ""
	}
	return WakeUpdateLead + "\n\n" + strings.Join(sections, "\n\n")
}

// CommitWakeUpdate records that u has been shown to slug: the nudges it
// carried are cleared so they are never repeated, and the watermark is
// written (or left alone when there was no index to work from). Call
// it after the text has been appended to the chat, never before.
func CommitWakeUpdate(s WakeUpdateStore, slug string, u WakeUpdate) error {
	if len(u.Nudged) > 0 {
		if err := s.ClearAssignmentNudges(u.Nudged); err != nil {
			return err
		}
	}
	if u.Watermark.Seq == 0 && u.Watermark.Fingerprints == nil {
		return nil // BuildWakeUpdate had no index to work from
	}
	return s.WriteGraphWatermark(slug, u.Watermark)
}

// PrepareWakeUpdate is BuildWakeUpdate followed by CommitWakeUpdate,
// for callers that append synchronously and cannot fail in between.
func PrepareWakeUpdate(s WakeUpdateStore, ix *graph.Index, slug string) (string, error) {
	u, err := BuildWakeUpdate(s, ix, slug)
	if err != nil {
		return "", err
	}
	if err := CommitWakeUpdate(s, slug, u); err != nil {
		return "", err
	}
	return u.Text, nil
}

// moved reports whether a fingerprint the previous watermark held has
// a different value now. A key the previous watermark lacked is not a
// move: it is recorded now and compared next time.
func moved(prev, next store.GraphWatermark, key string) bool {
	old, had := prev.Fingerprints[key]
	return had && old != next.Fingerprints[key]
}

// hashOrEmpty fingerprints a document, or returns "" when it could not
// be read (absent or otherwise): a document the note cannot see is one
// it cannot claim changed.
func hashOrEmpty(text string, err error) string {
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func hashLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// skillsFingerprint is the enabled skills as "name@version" pairs,
// sorted and comma-joined. Readable on purpose: skillsDiff names what
// changed from the two strings alone.
func skillsFingerprint(s WakeUpdateStore) string {
	skills, err := s.ListEnabledSkills()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(skills))
	for _, sk := range skills {
		parts = append(parts, sk.Name+"@"+sk.Version)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func skillsDiff(before, after string) string {
	parse := func(s string) map[string]string {
		out := map[string]string{}
		for _, p := range strings.Split(s, ",") {
			if p == "" {
				continue
			}
			name, ver, _ := strings.Cut(p, "@")
			out[name] = ver
		}
		return out
	}
	old, cur := parse(before), parse(after)
	var added, removed, updated []string
	for name, ver := range cur {
		if oldVer, ok := old[name]; !ok {
			added = append(added, name+"@"+ver)
		} else if oldVer != ver {
			updated = append(updated, name+" "+oldVer+"→"+ver)
		}
	}
	for name := range old {
		if _, ok := cur[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(updated)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(updated) > 0 {
		parts = append(parts, "updated "+strings.Join(updated, ", "))
	}
	return strings.Join(parts, "; ")
}
