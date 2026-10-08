package graph

// Placement is where one public file lands in the graph before
// versions are known: its node id, its payload if it names one, or
// the reason it is not a node (folded into another's payload, or
// dropped by an id collision).
//
// The store needs this before Build because versions are keyed by
// node id and a version has to be appended before the index that
// carries it is built. Plan and Build share the same naming and
// payload code, so the id Plan returns is the id Build will use.
type Placement struct {
	Owner   string
	Path    string
	ID      string
	Payload string
	Folded  bool
	Dropped bool
}

// Plan resolves names and payloads for every file in in.Files, without
// versions, agents or edges. Same ordering rules as Build.
func Plan(in Input) []Placement {
	files := make([]*fileState, 0, len(in.Files))
	for i := range in.Files {
		files = append(files, &fileState{in: in.Files[i]})
	}
	sortFiles(files)
	parseAll(files)
	claimPayloads(files)
	scratch := &Index{Nodes: map[string]*Node{}, Dropped: map[string][]string{}}
	assignNames(scratch, files)
	out := make([]Placement, 0, len(files))
	for _, f := range files {
		p := Placement{Owner: f.in.Owner, Path: f.in.Path, Folded: f.folded, Dropped: f.dropped, Payload: f.payload}
		if !f.folded && !f.dropped {
			p.ID = NodeID(f.in.Owner, f.name)
		}
		out = append(out, p)
	}
	return out
}
