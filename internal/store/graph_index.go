package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/graph"
)

// The knowledge graph on disk. Core is the only writer of everything
// here; agents write the files the graph is derived from, never the
// graph itself. See docs/developers/knowledge-graph.md §Mechanics. The graph
// maintainer (graph_maint.go) owns all of it.
//
//	graph/index.json          the current Index, written when its
//	                          sequence moves
//	graph/versions.jsonl      append-only: one line per new version of
//	                          any node; never rewritten, so a lost
//	                          index loses nothing but its sequence,
//	                          and the log carries a floor for that too
//	agents/<slug>/graph_watermark.json
//	                          what the agent has already been told
const (
	graphDirName           = "graph"
	graphIndexFilename     = "index.json"
	graphVersionsFilename  = "versions.jsonl"
	graphWatermarkFilename = "graph_watermark.json"

	// graphManifestMaxBytes bounds what the pass will read as a
	// markdown manifest. Anything larger is treated as a bare file:
	// snapshotted and versioned, never parsed. Nobody's requirement is
	// 10 MB.
	graphManifestMaxBytes = MaxInlinedFileBytes

	// graphProjectFileSource is what an owner's upload's `source` reads
	// as, so the reference rule holds for project files without the
	// owner having to type anything. Recomputed on every scan.
	graphProjectFileSource = "owner upload"
)

// graphVersionRecord is one line of versions.jsonl. It carries the
// node id the version belonged to, the path it was read from, the
// hashes of the bytes actually stored in the attachment store, the
// size and mtime of the file at the time (so the next pass can skip an
// unchanged payload without reading it), and the index sequence the
// pass was about to produce, which is the floor the sequence resumes
// from if the index is ever lost.
type graphVersionRecord struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Path       string    `json:"path"`
	N          int       `json:"n"`
	Seq        int64     `json:"seq,omitempty"`
	TS         time.Time `json:"ts"`
	SHA        string    `json:"sha"`
	Size       int64     `json:"size"`
	MTime      int64     `json:"mtime"`
	Payload    string    `json:"payload,omitempty"`
	PayloadSHA string    `json:"payload_sha,omitempty"`
	PayloadSz  int64     `json:"payload_size,omitempty"`
	PayloadMT  int64     `json:"payload_mtime,omitempty"`
}

func (r graphVersionRecord) version() graph.Version {
	return graph.Version{N: r.N, TS: r.TS, SHA: r.SHA, PayloadSHA: r.PayloadSHA}
}

// GraphWatermark is what an agent has already been shown: the index
// sequence at its last wake, and fingerprints of the things the wake
// note reports from outside the graph (handbook, role, skills).
type GraphWatermark struct {
	Seq          int64             `json:"seq"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ReadGraphIndex returns the index last written to disk, or
// ErrNotFound before any has been. A live reader wants Graph().Index();
// this is for the maintainer's load and for offline readers.
func (s *FSStore) ReadGraphIndex() (*graph.Index, error) {
	b, err := os.ReadFile(s.path(graphDirName, graphIndexFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var ix graph.Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("graph/index.json: %w", err)
	}
	if ix.Nodes == nil {
		ix.Nodes = map[string]*graph.Node{}
	}
	return &ix, nil
}

// graphProjectFileInputs projects the CEO's uploads into the graph: one
// node per original name, versioned through the same append-only log
// as every other node so deleting an old upload never renumbers the
// survivors and a pin stays a pin. The first time a name is seen its
// uploads are recorded oldest-first; after that a record is appended
// whenever the newest upload of that name changes.
//
// Path is the link name the newest upload has under /files/project/,
// computed with the sync farm's own collision rule, so the path the
// graph prints is the path the agent can open. Two distinct original
// names that sanitise alike therefore stay two nodes, the second under
// its short-SHA-prefixed link.
//
// Project-file versions are records only: the bytes live in the
// project-file store, and deleting an upload deletes them.
//
// known is read, never written: the caller appends the records it
// returns to the log and only then to its own copy.
func (s *FSStore) graphProjectFileInputs(known graphVersions, now time.Time, seq int64) ([]graph.FileInput, []graphVersionRecord, error) {
	pfs, err := s.ListProjectFiles() // oldest first
	if err != nil {
		return nil, nil, err
	}
	links := projectFileLinkNames(pfs)
	byName := map[string][]ProjectFile{}
	var order []string
	for _, pf := range pfs {
		if _, seen := byName[pf.OriginalName]; !seen {
			order = append(order, pf.OriginalName)
		}
		byName[pf.OriginalName] = append(byName[pf.OriginalName], pf)
	}
	sort.Strings(order)
	var inputs []graph.FileInput
	var records []graphVersionRecord
	seenIDs := map[string]bool{}
	for _, name := range order {
		uploads := byName[name]
		latest := uploads[len(uploads)-1]
		link := links[latest.SHA]
		// The node's name comes from the FIRST upload's link, which is
		// the unprefixed one; a re-upload's link gains a SHA prefix and
		// must not move the node.
		nodeName := graph.NameForPath(links[uploads[0].SHA])
		id := graph.NodeID(graph.CEOSlug, nodeName)
		if seenIDs[id] {
			// Build will drop the later of two names that derive the
			// same id and report it; do not version it under the other's id.
			continue
		}
		seenIDs[id] = true
		history := append([]graphVersionRecord(nil), known.byID[id]...)
		if len(history) == 0 {
			for i, pf := range uploads {
				rec := graphVersionRecord{ID: id, Owner: graph.CEOSlug, Path: links[pf.SHA], N: i + 1, Seq: seq, TS: pf.UploadedAt, SHA: pf.SHA, Size: pf.Size}
				history = append(history, rec)
				records = append(records, rec)
			}
		} else if last := history[len(history)-1]; last.SHA != latest.SHA && !historyHasSHA(history, latest.SHA) {
			// Bytes never seen under this name: a new version. Bytes
			// already on record are the older upload showing again
			// because the newest was deleted — not a version, or the
			// sequence would claim the CEO re-uploaded the old file.
			rec := graphVersionRecord{ID: id, Owner: graph.CEOSlug, Path: link, N: last.N + 1, Seq: seq, TS: now, SHA: latest.SHA, Size: latest.Size}
			history = append(history, rec)
			records = append(records, rec)
		}
		kind := graph.Kind(latest.Kind)
		switch latest.Kind {
		case "":
			kind = graph.KindReference
		case ProjectFileKindArtifact:
			kind = ""
		}
		in := graph.FileInput{
			Owner: graph.CEOSlug, Path: link, Name: nodeName, Project: true,
			Kind: kind, Summary: latest.Summary, Source: graphProjectFileSource,
		}
		for _, rec := range history {
			in.Versions = append(in.Versions, rec.version())
		}
		inputs = append(inputs, in)
	}
	return inputs, records, nil
}

// graphHashHint is the size, mtime and hash last recorded for a path,
// the cheap check that keeps the pass from re-reading a large payload
// nobody touched. For a manifest it also carries the node id the path
// was last versioned under, so a pass that cannot read the file still
// knows what it is.
type graphHashHint struct {
	sha   string
	size  int64
	mtime int64
	// ino is 0 when unknown: the version log does not record it.
	ino uint64
	id  string
}

// historyHasSHA reports whether any version on record carries sha.
func historyHasSHA(history []graphVersionRecord, sha string) bool {
	for _, rec := range history {
		if rec.SHA == sha {
			return true
		}
	}
	return false
}

type graphVersions struct {
	byID       map[string][]graphVersionRecord
	lastByPath map[string]graphHashHint // owner\x00path
	maxSeq     int64
}

// add folds records already appended to the log into v, as reading
// the log back would.
func (v *graphVersions) add(recs []graphVersionRecord) {
	for _, rec := range recs {
		v.byID[rec.ID] = append(v.byID[rec.ID], rec)
		v.note(rec)
	}
}

// note records rec's path hints and sequence.
func (v *graphVersions) note(rec graphVersionRecord) {
	v.lastByPath[rec.Owner+"\x00"+rec.Path] = graphHashHint{sha: rec.SHA, size: rec.Size, mtime: rec.MTime, id: rec.ID}
	if rec.Payload != "" {
		v.lastByPath[rec.Owner+"\x00"+rec.Payload] = graphHashHint{sha: rec.PayloadSHA, size: rec.PayloadSz, mtime: rec.PayloadMT}
	}
	if rec.Seq > v.maxSeq {
		v.maxSeq = rec.Seq
	}
}

// readGraphVersions loads versions.jsonl. A line that does not parse
// is skipped rather than failing the pass: the log is append-only and
// a torn final line from a crash mid-append must not wedge every
// future index.
func (s *FSStore) readGraphVersions() (graphVersions, error) {
	out := graphVersions{byID: map[string][]graphVersionRecord{}, lastByPath: map[string]graphHashHint{}}
	f, err := os.Open(s.path(graphDirName, graphVersionsFilename))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec graphVersionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		out.byID[rec.ID] = append(out.byID[rec.ID], rec)
		out.note(rec)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	for id := range out.byID {
		recs := out.byID[id]
		sort.Slice(recs, func(i, j int) bool { return recs[i].N < recs[j].N })
	}
	return out, nil
}

// appendGraphVersions adds records to versions.jsonl. If a crash left
// the file without a trailing newline, one is written first so the new
// record does not glue itself onto the torn line and become a second
// unparsable one.
func (s *FSStore) appendGraphVersions(recs []graphVersionRecord) error {
	dir := s.path(graphDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, graphVersionsFilename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var tail [1]byte
		if _, err := f.ReadAt(tail[:], info.Size()-1); err == nil && tail[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	w := bufio.NewWriter(f)
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

func (s *FSStore) writeGraphIndex(ix *graph.Index) error {
	dir := s.path(graphDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, graphIndexFilename), b, 0o644)
}

// ReadGraphWatermark returns what slug was last shown. ok is false
// before the agent's first wake note, and also when the file is
// unreadable: a torn watermark re-bootstraps rather than disabling the
// note's graph sections for that agent forever.
func (s *FSStore) ReadGraphWatermark(slug string) (GraphWatermark, bool, error) {
	b, err := os.ReadFile(s.path("agents", slug, graphWatermarkFilename))
	if errors.Is(err, os.ErrNotExist) {
		return GraphWatermark{}, false, nil
	}
	if err != nil {
		return GraphWatermark{}, false, err
	}
	var wm GraphWatermark
	if err := json.Unmarshal(b, &wm); err != nil {
		return GraphWatermark{}, false, nil
	}
	return wm, true, nil
}

// WriteGraphWatermark records what slug has now been shown.
func (s *FSStore) WriteGraphWatermark(slug string, wm GraphWatermark) error {
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if wm.UpdatedAt.IsZero() {
		wm.UpdatedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(wm, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, graphWatermarkFilename), b, 0o644)
}

// ProjectFileKindArtifact is the explicit "no kind" choice for a CEO
// upload: authored primary material rather than reference. Distinct
// from an empty Kind, which is "never chosen" and reads as reference.
const ProjectFileKindArtifact = "artifact"

// SetProjectFileKind records the graph kind of a CEO upload: one of
// graph.Kinds, ProjectFileKindArtifact for none, or empty to clear the
// choice (the graph then reads it as a reference).
func (s *FSStore) SetProjectFileKind(sha, kind string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != ProjectFileKindArtifact && !graph.ValidKind(graph.Kind(kind)) {
		return fmt.Errorf("kind %q is not one of the graph's kinds", kind)
	}
	return s.mutateProjectFile(sha, func(pf *ProjectFile) {
		pf.Kind = kind
	})
}
