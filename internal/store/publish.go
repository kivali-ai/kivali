package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

// Publishing. Every agent's published tree is public/<slug>/ on the
// data volume (files.PublishedDirName), and core is its only writer:
// agents publish with artifact_publish and unpublish with
// artifact_unpublish, both of which land here. The agent pod mounts
// the trees read-only (see agentpod's manifest).

// Publish limits: the chat's upload limit (200 MB) per file and per
// call, and a file count that keeps one call's reply readable. The
// tool descriptions state them.
const (
	PublishMaxFileBytes  = 200 << 20
	PublishMaxTotalBytes = 200 << 20
	PublishMaxFiles      = 1000
)

// PublishLimits is what one artifact_publish call may carry.
var PublishLimits = files.PublishLimits{
	MaxFileBytes:  PublishMaxFileBytes,
	MaxTotalBytes: PublishMaxTotalBytes,
	MaxFiles:      PublishMaxFiles,
}

// PublishRequest is one artifact_publish call.
type PublishRequest struct {
	// Owner is the durable agent whose published tree is written; a
	// subagent publishes into its parent's.
	Owner string
	// SubagentID, when set, is the subagent calling: Source is then a
	// path in its view of the parent's tree, /files/subagents/<id>/.
	SubagentID string
	// Source is the model path: /files/<path> in the caller's view.
	Source string
	// Dest is the path under the owner's published tree; empty for the
	// default (see publishDefaultDest).
	Dest string
}

// PublishedFile is what the index made of one file after a publish:
// the node it belongs to (the node whose manifest it is, or whose
// payload), its version and status, and anything the index could not
// accept. NodeID is empty when the file is in no node: Dropped then
// says why when the index said.
type PublishedFile struct {
	Path     string // under the owner's published tree
	NodeID   string
	Version  int
	Status   graph.Status
	Payload  bool // the file is its node's payload, not its manifest
	Rejected string
	Problems []string
	Flags    []string
	Dropped  string
}

// PublishReport is the answer to one publish or unpublish.
type PublishReport struct {
	Owner string
	// Files are the files written (publish) or removed (unpublish).
	Files []PublishedFile
	// Skipped lists dot-named entries a directory publish left out.
	Skipped []string
	// Gone lists the node ids an unpublish removed from the graph;
	// Remaining those that still exist (a manifest whose payload went,
	// say).
	Gone      []string
	Remaining []string
	// IndexErr is set when the files were written but the index update
	// failed; Files then carry paths only.
	IndexErr error
	// Warnings are the index update's per-file warnings about these
	// files.
	Warnings []string
}

// Publish copies req.Source into the owner's published tree and
// reports what the knowledge graph made of each file. The source is
// read through files.PlanPublish and files.CopyPublished: no link is
// followed anywhere, only regular files are taken, and nothing is
// written until the whole source has been checked.
func (s *FSStore) Publish(req PublishRequest) (PublishReport, error) {
	rep := PublishReport{Owner: req.Owner}
	src, rel, err := s.openPublishSource(req.Owner, req.SubagentID, req.Source)
	if err != nil {
		return rep, err
	}
	defer func() { _ = src.Close() }()
	dest := publishDefaultDest(rel)
	if strings.TrimSpace(req.Dest) != "" {
		dest = req.Dest
	}
	if dest != "" {
		if dest, err = files.CleanPublishedPath(dest); err != nil {
			return rep, fmt.Errorf("dest %w", err)
		}
	}
	items, skipped, err := files.PlanPublish(src, rel, dest, PublishLimits)
	if err != nil {
		return rep, err
	}
	rep.Skipped = skipped
	if err := files.EnsurePublishedDir(s.root, req.Owner); err != nil {
		return rep, err
	}
	dst, err := files.OpenDirNoFollow(s.path(files.PublishedDirName), req.Owner)
	if err != nil {
		return rep, err
	}
	defer func() { _ = dst.Close() }()
	written, cerr := files.CopyPublished(src, dst, items, PublishLimits)
	if len(written) == 0 && cerr != nil {
		return rep, cerr
	}
	ix, warnings, ierr := s.publishedChanged(req.Owner, written)
	rep.IndexErr = ierr
	rep.Warnings = warnings
	for _, p := range written {
		rep.Files = append(rep.Files, publishedFileIn(ix, req.Owner, p))
	}
	if cerr != nil {
		return rep, fmt.Errorf("%w (the %d file(s) before it were published)", cerr, len(written))
	}
	return rep, nil
}

// Unpublish removes p, a file or a directory, from the owner's
// published tree and reports which nodes went with it.
func (s *FSStore) Unpublish(owner, p string) (PublishReport, error) {
	rep := PublishReport{Owner: owner}
	if err := s.checkPublisher(owner); err != nil {
		return rep, err
	}
	rel, err := files.CleanPublishedPath(p)
	if err != nil {
		return rep, fmt.Errorf("path %w", err)
	}
	if rel == "." {
		return rep, errors.New("name a file or directory under /files/artifacts/public/, not the whole tree")
	}
	dst, err := files.OpenDirNoFollow(s.path(files.PublishedDirName), owner)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("%s is not published", rel)
		}
		return rep, err
	}
	defer func() { _ = dst.Close() }()
	before := s.Graph().Index()
	gone, err := files.RemovePublished(dst, rel)
	if err != nil {
		return rep, err
	}
	var ids []string
	for _, f := range gone {
		if n := nodeForPublishedFile(before, owner, f); n != nil {
			ids = append(ids, n.ID)
		}
		rep.Files = append(rep.Files, PublishedFile{Path: f})
	}
	ix, warnings, ierr := s.publishedChanged(owner, gone)
	rep.IndexErr = ierr
	rep.Warnings = warnings
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if ierr != nil {
			continue
		}
		if _, ok := ix.Get(id); ok {
			rep.Remaining = append(rep.Remaining, id)
		} else {
			rep.Gone = append(rep.Gone, id)
		}
	}
	sort.Strings(rep.Gone)
	sort.Strings(rep.Remaining)
	return rep, nil
}

// publishedChanged is the one place a change to an owner's published
// tree reaches the knowledge graph: every publish and unpublish calls
// it once, with the files written or removed, and reads the index it
// returns. The graph maintainer reads just these files and rebuilds
// before returning. Warnings are the maintainer's, narrowed to these
// files.
func (s *FSStore) publishedChanged(owner string, paths []string) (*graph.Index, []string, error) {
	ix, warnings, err := s.Graph().Published(owner, paths)
	if err != nil {
		return nil, nil, err
	}
	return ix, warningsAbout(owner, paths, warnings), nil
}

// warningsAbout is the warnings that begin "<owner>/<path>:" for one
// of paths.
func warningsAbout(owner string, paths, warnings []string) []string {
	var mine []string
	for _, w := range warnings {
		for _, p := range paths {
			if strings.HasPrefix(w, owner+"/"+p+":") {
				mine = append(mine, w)
				break
			}
		}
	}
	return mine
}

// publishedFileIn reads what the index made of owner's file p.
func publishedFileIn(ix *graph.Index, owner, p string) PublishedFile {
	out := PublishedFile{Path: p}
	if ix == nil {
		return out
	}
	n := nodeForPublishedFile(ix, owner, p)
	if n == nil {
		prefix := p + ": "
		for _, d := range ix.Dropped[owner] {
			if strings.HasPrefix(d, prefix) {
				out.Dropped = strings.TrimPrefix(d, prefix)
			}
		}
		return out
	}
	out.NodeID = n.ID
	out.Version = n.CurrentVersion().N
	out.Status = n.Status
	out.Payload = n.Path != p
	out.Rejected = n.Rejected
	out.Problems = n.Problems
	out.Flags = n.Flags
	return out
}

// nodeForPublishedFile finds the artifact node owner's file p belongs
// to: the one it is the manifest (or bare file) of, else the one it is
// the payload of.
func nodeForPublishedFile(ix *graph.Index, owner, p string) *graph.Node {
	if ix == nil {
		return nil
	}
	var payloadOf *graph.Node
	for _, n := range ix.Nodes {
		if n.Owner != owner || n.Type != graph.TypeArtifact {
			continue
		}
		if n.Path == p {
			return n
		}
		if n.Payload == p {
			payloadOf = n
		}
	}
	return payloadOf
}

// publishDefaultDest is where a source lands when the call names no
// dest: its path relative to artifacts/private/ when it lives there
// (so /files/artifacts/private/specs/x.md publishes as specs/x.md, and
// artifacts/private/ itself as the whole tree), else its base name.
func publishDefaultDest(rel string) string {
	if rel == "artifacts/private" {
		return ""
	}
	if rest, ok := strings.CutPrefix(rel, "artifacts/private/"); ok {
		return rest
	}
	return path.Base(rel)
}

// publishRefused reports whether rel lies in a part of /files/ a
// publish may not take from: core's own views, the published trees
// among them.
func publishRefused(rel string) bool {
	for _, d := range files.CoreManagedDirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// openPublishSource maps the caller's model path to a directory core
// opens and the source's path beneath it.
//
// For a durable agent that directory is its /files/ root. For a
// subagent it is its view, /files/subagents/<id>/ in the parent's tree,
// whose own real directories (artifacts/private/, and anything else it
// made) are taken; its background/ is the parent's, and is read there,
// at the directory core linked it to, rather than through the link.
// Every other link in the view points at something publishing refuses.
//
// The directory is opened without following a link at any component.
func (s *FSStore) openPublishSource(owner, subagentID, source string) (*os.Root, string, error) {
	if err := s.checkPublisher(owner); err != nil {
		return nil, "", err
	}
	rel, err := modelRel(source)
	if err != nil {
		return nil, "", err
	}
	if rel == "" {
		return nil, "", errors.New("source: name a file or directory under /files/, not the whole tree")
	}
	if publishRefused(rel) {
		return nil, "", fmt.Errorf("source /files/%s: core's views and the published trees cannot be published from; copy what you want into /files/artifacts/private/ first", rel)
	}
	storage := files.StorageRoot(s.path("agents", owner))
	if subagentID == "" || rel == files.SharedWorkspaceDir || strings.HasPrefix(rel, files.SharedWorkspaceDir+"/") {
		root, err := files.OpenDirNoFollow(storage, "")
		return root, rel, err
	}
	if !validPublishSlug(subagentID) {
		return nil, "", fmt.Errorf("bad subagent id %q", subagentID)
	}
	root, err := files.OpenDirNoFollow(storage, "subagents/"+subagentID)
	if err != nil {
		return nil, "", fmt.Errorf("subagent %s has no workspace: %w", subagentID, err)
	}
	return root, rel, nil
}

// modelRel turns a model path, "/files/<p>" or "<p>", into <p> cleaned
// and slash-separated ("" for the root), refusing one that climbs out.
func modelRel(p string) (string, error) {
	orig := p
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("source is required")
	}
	switch {
	case p == files.ModelRootPath:
		p = ""
	case strings.HasPrefix(p, files.ModelRootPath+"/"):
		p = strings.TrimPrefix(p, files.ModelRootPath+"/")
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("source %q: must be a path under /files/", orig)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("source %q: .. is not allowed", orig)
		}
	}
	if clean := path.Clean(p); clean != "." {
		return clean, nil
	}
	return "", nil
}

// checkPublisher refuses an owner that is not an active agent with a
// tree of its own: only those publish.
func (s *FSStore) checkPublisher(owner string) error {
	if !validPublishSlug(owner) || owner == graph.CEOSlug {
		return fmt.Errorf("%q cannot publish", owner)
	}
	if _, err := s.GetAgent(owner); err != nil {
		return fmt.Errorf("%q is not an active agent", owner)
	}
	return nil
}

// validPublishSlug is the lexical check for a slug or subagent id used
// as one path component.
func validPublishSlug(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}
