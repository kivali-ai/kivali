package web

import (
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/graph"
)

// graphPageOwner is one agent's section of the graph page: the agent
// node, every artifact it owns, and the index findings on its files.
type graphPageOwner struct {
	Node      *graph.Node
	Artifacts []*graph.Node
	Findings  []string
}

// graphPageData is the graph grouped by owner, as GET /api/v1/graph
// lists it. Owners come CEO first, then
// active agents by slug, then archived agents; artifacts within an
// owner sort by id.
type graphPageData struct {
	Seq       int64
	BuiltAt   time.Time
	Owners    []graphPageOwner
	Artifacts int
	Agents    int
	Flagged   int
	Rejected  int
	Problems  int
	Kinds     []graph.Kind
	Stale     bool
}

func buildGraphPage(ix *graph.Index) graphPageData {
	data := graphPageData{Seq: ix.Seq, BuiltAt: ix.BuiltAt, Kinds: graph.Kinds}
	byOwner := map[string][]*graph.Node{}
	var agents []*graph.Node
	for _, n := range ix.Sorted() {
		switch n.Type {
		case graph.TypeAgent:
			agents = append(agents, n)
			data.Agents++
		default:
			byOwner[n.Owner] = append(byOwner[n.Owner], n)
			data.Artifacts++
			if n.Flagged {
				data.Flagged++
			}
			if n.Rejected != "" {
				data.Rejected++
			}
			data.Problems += len(n.Problems)
		}
	}
	for _, drops := range ix.Dropped {
		data.Problems += len(drops)
	}
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		switch {
		case a.ID == graph.CEOSlug:
			return true
		case b.ID == graph.CEOSlug:
			return false
		case (a.Status == graph.StatusArchived) != (b.Status == graph.StatusArchived):
			return a.Status != graph.StatusArchived
		}
		return a.ID < b.ID
	})
	for _, a := range agents {
		data.Owners = append(data.Owners, graphPageOwner{
			Node:      a,
			Artifacts: byOwner[a.ID],
			Findings:  ix.OwnerFindings(a.ID),
		})
		delete(byOwner, a.ID)
	}
	// Artifacts whose owner is not an agent node (a directory left
	// behind by a hand edit): shown under a synthetic owner rather than
	// hidden, since hidden is how the register went unread.
	var orphans []string
	for owner := range byOwner {
		orphans = append(orphans, owner)
	}
	sort.Strings(orphans)
	for _, owner := range orphans {
		data.Owners = append(data.Owners, graphPageOwner{
			Node:      &graph.Node{ID: owner, Type: graph.TypeAgent, Status: "unknown", Summary: "not in the org chart"},
			Artifacts: byOwner[owner],
			Findings:  ix.OwnerFindings(owner),
		})
	}
	return data
}
