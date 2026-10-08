package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

// The graph maintainer owns the knowledge-graph index
// (docs/developers/files-and-publishing.md §"The graph maintainer"). Nothing
// outside it runs an index pass or takes its lock.
//
// It lives in the store rather than a package of its own because
// everything it keeps is the store's: graph/index.json and
// versions.jsonl, the attachment store its snapshots go into, the
// org chart and the project-file index it reads, and the published
// trees Publish writes. Publish calls it synchronously, which a
// package importing the store could only offer through a hook.
//
// State. One mutex (mu) serialises every change. Under it the
// maintainer keeps, loaded once: the version log, the org chart, the
// project-file inputs, and a per-file cache of every published file
// (size, mtime, the SHA of its stored snapshot, and for a manifest
// what graph.ReadFacts made of it). A file's bytes are read only when
// it is new or its size or mtime moved; a payload whose stat matches
// its last version record is not read at all.
//
// Reads. Index returns an immutable *graph.Index from an atomic
// pointer and never takes mu once the maintainer has loaded. Nothing
// may write to what it returns.
//
// Changes. Published (every artifact_publish and artifact_unpublish)
// reads just the files named and rebuilds the index from the cache,
// before it returns, so the publisher's reply and its next graph tool
// call see what it published. OrgChanged (hire, archive, reporting
// line) and ProjectFilesChanged rebuild from the cache and walk
// nothing. graph.Build is pure CPU over the whole org, cheap next to
// any read of the trees.
//
// Anti-entropy. Scan stats every published file, rereads what moved
// and drops what vanished: at boot, every GraphScanInterval, and
// after a Reload. Core is the only writer of the published trees, so
// this guards against operator edits, restores and bugs, not agents.
// The stat walk and the reads run without mu, so a publish during a
// scan waits only for the scan's two short critical sections.
//
// Persistence. index.json is written when the sequence moves, by the
// changing caller after it has released mu, under a separate write
// lock: a caller that finds the newest index already written (a
// concurrent change wrote it) writes nothing, so a burst of changes
// costs one write per change at most and never holds up a reader or
// the next change. Version records are appended to versions.jsonl
// before the index that carries them is published, exactly as the
// pass did. Flush writes whatever is not yet on disk.

// GraphScanInterval is how often the anti-entropy scan runs.
const GraphScanInterval = 5 * time.Minute

// graphKey is one published file: owner and slash path under
// public/<owner>/.
type graphKey struct{ owner, rel string }

func (k graphKey) hintKey() string { return k.owner + "\x00" + k.rel }

// graphEntry is the cache's record of one published file.
type graphEntry struct {
	size, mtime int64
	// ino is the file's inode (0 where unknown). Publishing writes a
	// temp file and renames it into place, so a rewrite of the same
	// size inside the filesystem's mtime granularity still moves it.
	ino uint64
	// md marks a manifest candidate: a .md file small enough to parse.
	md bool
	// facts is what the manifest candidate parsed to; nil with readErr.
	facts *graph.Facts
	// readErr is a manifest candidate that could not be read: listed
	// as a bare file under the id its last version recorded, so a
	// transient EACCES does not make a node vanish.
	readErr error
	// sha is the SHA of the bytes in the attachment store; empty until
	// a snapshot has been taken.
	sha string
	// body holds a manifest's bytes only while their snapshot is
	// untaken, so a retry stores the bytes that were parsed.
	body []byte
	// gen is the change that installed the entry; a scan applies what
	// it read only over entries no newer than its start.
	gen uint64
}

// graphStat is what the walk records of one file.
type graphStat struct {
	size, mtime int64
	ino         uint64
}

// GraphMaintainer owns the knowledge-graph index. Get it with
// FSStore.Graph.
type GraphMaintainer struct {
	s *FSStore

	ix    atomic.Pointer[graph.Index]
	walks atomic.Int64

	mu      sync.Mutex
	clk     clock.Clock
	loaded  bool
	scanned bool
	// started is set by Start; bootScan closes when the boot scan has
	// run, applied or failed. Before it no caller scans on its own:
	// Index answers from what Start loaded, Published waits for it.
	started       bool
	bootScan      chan struct{}
	versions      graphVersions
	files         map[graphKey]*graphEntry
	agents        []graph.AgentInput
	project       []graph.FileInput
	projectLoaded bool
	gen           uint64
	// removed is the gen at which each file was last removed, for the
	// scan in flight: a file an unpublish removed after the scan
	// planned must not come back from what the scan read. Cleared by
	// each scan once applied (scans run one at a time).
	removed map[graphKey]uint64
	// epoch moves on every Reload; a scan planned before one applies
	// nothing.
	epoch uint64

	// scanMu runs Scans one at a time.
	scanMu sync.Mutex

	// wmu serialises index.json writes; written is the index last
	// written (or read from disk).
	wmu     sync.Mutex
	written *graph.Index

	// afterScan, when set before Start, runs after each scan the
	// ticker drives. Tests only.
	afterScan func()
	// afterWalk, when set, runs in a scan between its walk and its
	// plan, without mu: tests land changes during the walk. Tests only.
	afterWalk func()
}

// Graph returns the store's graph maintainer.
func (s *FSStore) Graph() *GraphMaintainer {
	s.graphOnce.Do(func() {
		s.graphM = &GraphMaintainer{s: s, clk: clock.System{}}
	})
	return s.graphM
}

// Start loads the index on disk, so Index answers from it at once,
// then scans in the background and again every GraphScanInterval
// until ctx ends. It does not wait for the first scan: on a large
// install whose snapshot store is empty that scan copies every
// published byte into it once. Until that scan
// has run, Index answers from the index on disk (nil on a fresh
// install) and Published waits for it.
func (m *GraphMaintainer) Start(ctx context.Context, clk clock.Clock) {
	m.mu.Lock()
	if clk != nil {
		m.clk = clk
	}
	tick := m.clk
	warnings, err := m.loadLocked()
	bootScan := make(chan struct{})
	m.started, m.bootScan = true, bootScan
	m.mu.Unlock()
	logGraph("start", warnings, err)
	go func() {
		m.scanLogged(ctx, "boot scan")
		close(bootScan)
		t := tick.NewTicker(GraphScanInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				m.scanLogged(ctx, "scan")
			}
		}
	}()
}

func (m *GraphMaintainer) scanLogged(ctx context.Context, where string) {
	_, warnings, err := m.scan(ctx)
	logGraph(where, warnings, err)
	if m.afterScan != nil {
		m.afterScan()
	}
}

// logGraph logs a maintainer call's warnings and error: the graph is
// derived state, and a failure to index never fails the caller.
func logGraph(where string, warnings []string, err error) {
	for _, w := range warnings {
		log.Printf("graph %s: %s", where, w)
	}
	if err != nil {
		log.Printf("graph %s: %v", where, err)
	}
}

// Index is the current index. After the first load it never waits on
// mu or on disk. Once Start has run it never scans: before the boot
// scan applies it is what Start loaded, nil on an install with no
// index on disk. In a process that never called Start (a CLI, a test)
// the first call loads and scans. The result is shared: never modify
// it.
func (m *GraphMaintainer) Index() *graph.Index {
	if ix := m.ix.Load(); ix != nil {
		return ix
	}
	m.mu.Lock()
	var warnings []string
	var err error
	if m.started {
		warnings, err = m.loadLocked()
	} else {
		warnings, err = m.ensureReadyLocked(context.Background())
	}
	m.mu.Unlock()
	logGraph("load", warnings, err)
	logGraph("write", nil, m.persist())
	return m.ix.Load()
}

// Walks is how many full walks of the published trees have run. Tests
// read it to prove a path runs none.
func (m *GraphMaintainer) Walks() int64 { return m.walks.Load() }

// Published updates the index for the files named under owner's
// published tree, written or removed, and returns it with the
// per-file warnings. Synchronous: the index returned, and every Index
// after it, reflects the change. Costs a read of each file named and
// a rebuild from the cache; nothing else is walked.
//
// Before the boot scan has run it waits for that scan rather than
// walking with mu held: the rebuild needs a complete cache, and the
// boot scan is already building one. The wait ends when that scan
// ends, applied or failed (it stops with Start's ctx), so it needs no
// ctx of its own. Only when the boot scan failed, or a Reload's scan
// did, does Published scan itself.
func (m *GraphMaintainer) Published(owner string, paths []string) (*graph.Index, []string, error) {
	m.mu.Lock()
	if m.started && !m.scanned {
		bootScan := m.bootScan
		m.mu.Unlock()
		<-bootScan
		m.mu.Lock()
	}
	warnings, err := m.ensureReadyLocked(context.Background())
	if err != nil {
		m.mu.Unlock()
		return nil, warnings, err
	}
	root, rerr := files.OpenDirNoFollow(m.s.path(files.PublishedDirName), owner)
	if rerr == nil {
		defer func() { _ = root.Close() }()
	}
	for _, p := range paths {
		rel := path.Clean(filepath.ToSlash(p))
		if hiddenPublishedPath(rel) {
			continue
		}
		k := graphKey{owner: owner, rel: rel}
		switch {
		case rerr == nil:
			m.installLocked(k, m.readEntry(root, k, nil, m.hintLocked(k)))
		case publishedGone(rerr):
			m.installLocked(k, nil)
		default:
			warnings = append(warnings, fmt.Sprintf("%s/%s: %v (index unchanged for it)", owner, rel, rerr))
		}
	}
	ix, more, err := m.rebuildLocked(false)
	m.mu.Unlock()
	warnings = append(warnings, more...)
	if err != nil {
		return nil, warnings, err
	}
	if werr := m.persist(); werr != nil {
		warnings = append(warnings, fmt.Sprintf("graph/index.json: %v (the index is current in memory; written with the next change)", werr))
	}
	return ix, warnings, nil
}

// OrgChanged re-reads the org chart and rebuilds: a hire, an archive
// or a reporting-line change. Before the first scan it logs and does
// nothing else; that scan reads the chart, so the change is lost only
// if the boot scan never applies.
func (m *GraphMaintainer) OrgChanged() {
	m.mu.Lock()
	if !m.scanned {
		m.mu.Unlock()
		log.Printf("graph org change: not applied, nothing scanned yet (the first scan reads the org chart)")
		return
	}
	var warnings []string
	agents, owners, err := m.s.graphOrg()
	if err == nil {
		m.agents = agents
		m.dropOwnersLocked(owners)
		_, warnings, err = m.rebuildLocked(false)
	}
	m.mu.Unlock()
	logGraph("org change", warnings, err)
	logGraph("org change", nil, m.persist())
}

// ProjectFilesChanged re-reads the project-file index and rebuilds:
// an upload, a delete, a kind or a summary. Before the first scan it
// logs and does nothing else; that scan reads the project files.
func (m *GraphMaintainer) ProjectFilesChanged() {
	m.mu.Lock()
	if !m.scanned {
		m.mu.Unlock()
		log.Printf("graph project files: not applied, nothing scanned yet (the first scan reads the project files)")
		return
	}
	_, warnings, err := m.rebuildLocked(true)
	m.mu.Unlock()
	logGraph("project files", warnings, err)
	logGraph("project files", nil, m.persist())
}

// Reload forgets everything and loads again from disk, then scans: for
// a restore, which replaced the data directory underneath.
func (m *GraphMaintainer) Reload() error {
	warnings, err := m.reload()
	logGraph("reload", warnings, nil)
	return err
}

func (m *GraphMaintainer) reload() ([]string, error) {
	m.mu.Lock()
	m.wmu.Lock()
	m.loaded, m.scanned, m.projectLoaded = false, false, false
	m.files, m.agents, m.project, m.removed = nil, nil, nil, nil
	m.epoch++
	m.versions = graphVersions{}
	m.ix.Store(nil)
	m.written = nil
	m.wmu.Unlock()
	warnings, err := m.ensureReadyLocked(context.Background())
	m.mu.Unlock()
	if err != nil {
		return warnings, err
	}
	return warnings, m.persist()
}

// Scan is the anti-entropy pass: stat every published file, reread
// what moved, drop what vanished, rebuild. Warnings are logged.
func (m *GraphMaintainer) Scan(ctx context.Context) error {
	_, warnings, err := m.scan(ctx)
	logGraph("scan", warnings, nil)
	return err
}

// Flush writes the current index to graph/index.json if it is not
// there yet.
func (m *GraphMaintainer) Flush() error { return m.persist() }

// persist writes the current index unless it is the one last written.
func (m *GraphMaintainer) persist() error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	ix := m.ix.Load()
	if ix == nil || ix == m.written {
		return nil
	}
	if err := m.s.writeGraphIndex(ix); err != nil {
		return err
	}
	m.written = ix
	return nil
}

// loadLocked reads index.json and the version log once. A corrupt
// index is moved aside and rebuilt, with the version log providing
// the sequence floor.
func (m *GraphMaintainer) loadLocked() ([]string, error) {
	if m.loaded {
		return nil, nil
	}
	var warnings []string
	ix, err := m.s.ReadGraphIndex()
	switch {
	case errors.Is(err, ErrNotFound):
		ix = nil
	case err != nil:
		aside := m.s.path(graphDirName, fmt.Sprintf("%s.corrupt-%d", graphIndexFilename, time.Now().UnixNano()))
		if rerr := os.Rename(m.s.path(graphDirName, graphIndexFilename), aside); rerr != nil {
			return nil, fmt.Errorf("graph/index.json unreadable (%v) and could not be moved aside: %w", err, rerr)
		}
		warnings = append(warnings, fmt.Sprintf("graph/index.json unreadable (%v); moved to %s and rebuilt from the version log", err, filepath.Base(aside)))
		ix = nil
	}
	versions, err := m.s.readGraphVersions()
	if err != nil {
		return warnings, err
	}
	m.versions = versions
	m.files = map[graphKey]*graphEntry{}
	m.loaded = true
	if ix != nil {
		m.ix.Store(ix)
		m.wmu.Lock()
		m.written = ix
		m.wmu.Unlock()
	}
	return warnings, nil
}

// ensureReadyLocked loads and, the first time, scans with mu held.
func (m *GraphMaintainer) ensureReadyLocked(ctx context.Context) ([]string, error) {
	warnings, err := m.loadLocked()
	if err != nil || m.scanned {
		return warnings, err
	}
	gen, epoch := m.gen, m.epoch
	w, err := m.walk(ctx)
	warnings = append(warnings, w.warnings...)
	if err != nil {
		return warnings, err
	}
	plan := m.planScanLocked(w, gen, epoch)
	got, err := m.readScan(ctx, plan)
	if err != nil {
		return warnings, err
	}
	_, more, err := m.applyScanLocked(plan, got)
	return append(warnings, more...), err
}

// scan is Scan, returning what it built. The walk and the reads run
// without mu; applying what they found takes it.
func (m *GraphMaintainer) scan(ctx context.Context) (*graph.Index, []string, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	// The plan is as of the walk's start, not its end: a publish,
	// unpublish or Reload landing while the walk lists is newer than
	// anything the walk saw, and must not be taken for what it undoes.
	m.mu.Lock()
	gen, epoch := m.gen, m.epoch
	m.mu.Unlock()
	w, err := m.walk(ctx)
	warnings := w.warnings
	if err != nil {
		return nil, warnings, err
	}
	if m.afterWalk != nil {
		m.afterWalk()
	}
	m.mu.Lock()
	if epoch != m.epoch {
		// A Reload replaced everything while this scan walked; its
		// own scan is the current one.
		m.mu.Unlock()
		return m.ix.Load(), warnings, nil
	}
	lw, err := m.loadLocked()
	warnings = append(warnings, lw...)
	if err != nil {
		m.mu.Unlock()
		return nil, warnings, err
	}
	plan := m.planScanLocked(w, gen, epoch)
	m.mu.Unlock()
	got, err := m.readScan(ctx, plan)
	if err != nil {
		return nil, warnings, err
	}
	m.mu.Lock()
	if plan.epoch != m.epoch {
		// A Reload replaced everything while this scan read; its own
		// scan is the current one.
		m.mu.Unlock()
		return m.ix.Load(), warnings, nil
	}
	ix, more, err := m.applyScanLocked(plan, got)
	m.removed = nil
	m.mu.Unlock()
	warnings = append(warnings, more...)
	if err != nil {
		return nil, warnings, err
	}
	if werr := m.persist(); werr != nil {
		return ix, warnings, werr
	}
	return ix, warnings, nil
}

// graphWalk is one stat-only walk of every published tree.
type graphWalk struct {
	found map[graphKey]graphStat
	// walked are the owners whose tree was listed in full (or has
	// none); a file of theirs the walk did not find is gone. An owner
	// whose tree could not be opened keeps its cached files.
	walked   map[string]bool
	warnings []string
}

// walk lists every owner's published tree without reading a byte.
func (m *GraphMaintainer) walk(ctx context.Context) (graphWalk, error) {
	m.walks.Add(1)
	w := graphWalk{found: map[graphKey]graphStat{}, walked: map[string]bool{}}
	_, owners, err := m.s.graphOrg()
	if err != nil {
		return w, err
	}
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return w, err
		}
		m.walkOwner(owner, &w)
	}
	return w, nil
}

// walkOwner lists the regular files under one owner's published tree.
// Dotfiles and dot-directories are skipped (atomic-write leftovers;
// publishing refuses them), as are symlinks. The tree is opened with
// files.OpenDirNoFollow and walked through that root, so a link at any
// depth is never followed out of it.
func (m *GraphMaintainer) walkOwner(owner string, w *graphWalk) {
	dir := m.s.path(files.PublishedDirName, owner)
	root, err := files.OpenDirNoFollow(m.s.path(files.PublishedDirName), owner)
	if err != nil {
		if publishedGone(err) {
			w.walked[owner] = true
		} else {
			w.warnings = append(w.warnings, fmt.Sprintf("%s: %v", dir, err))
		}
		return
	}
	defer func() { _ = root.Close() }()
	w.walked[owner] = true
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			w.warnings = append(w.warnings, fmt.Sprintf("%s: %v", filepath.Join(dir, filepath.FromSlash(p)), walkErr))
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			w.warnings = append(w.warnings, fmt.Sprintf("%s: %v", filepath.Join(dir, filepath.FromSlash(p)), err))
			return nil
		}
		w.found[graphKey{owner: owner, rel: p}] = graphStat{size: info.Size(), mtime: info.ModTime().UnixNano(), ino: fileIno(info)}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		w.warnings = append(w.warnings, fmt.Sprintf("%s: %v", dir, err))
	}
}

// graphScanPlan is what a scan has to read, check and drop, decided
// against the cache as it stood at gen.
type graphScanPlan struct {
	walk  graphWalk
	gen   uint64
	epoch uint64
	// read are new, changed or unreadable files, and files with no
	// snapshot yet, with the hint that may spare reading a payload.
	read map[graphKey]graphHashHint
	// verify are unchanged files whose snapshot must still exist.
	verify map[graphKey]string
	// gone are cached files the walk did not find.
	gone []graphKey
}

// planScanLocked plans against the cache as it stood at gen and epoch,
// read under mu before the walk began: an entry installed since is
// newer than the walk and is neither re-read over nor dropped.
func (m *GraphMaintainer) planScanLocked(w graphWalk, gen, epoch uint64) graphScanPlan {
	p := graphScanPlan{walk: w, gen: gen, epoch: epoch, read: map[graphKey]graphHashHint{}, verify: map[graphKey]string{}}
	for k, st := range w.found {
		e := m.files[k]
		switch {
		case e == nil || e.size != st.size || e.mtime != st.mtime || e.ino != st.ino || e.readErr != nil || e.sha == "":
			// sha is empty when the read or the snapshot failed
			// (a payload that could not be opened has no readErr).
			p.read[k] = m.hintLocked(k)
		case e.sha != "":
			p.verify[k] = e.sha
		}
	}
	for k := range m.files {
		if _, found := w.found[k]; !found && w.walked[k.owner] {
			p.gone = append(p.gone, k)
		}
	}
	return p
}

// readScan reads what the plan names, without mu. A nil entry in the
// result is a file that is gone.
func (m *GraphMaintainer) readScan(ctx context.Context, p graphScanPlan) (map[graphKey]*graphEntry, error) {
	got := map[graphKey]*graphEntry{}
	roots := map[string]*os.Root{}
	defer func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}()
	rootFor := func(owner string) (*os.Root, error) {
		if r, ok := roots[owner]; ok {
			return r, nil
		}
		r, err := files.OpenDirNoFollow(m.s.path(files.PublishedDirName), owner)
		if err != nil {
			return nil, err
		}
		roots[owner] = r
		return r, nil
	}
	read := func(k graphKey, hint graphHashHint) {
		root, err := rootFor(k.owner)
		if err != nil {
			if publishedGone(err) {
				got[k] = nil
			}
			return
		}
		st := p.walk.found[k]
		got[k] = m.readEntry(root, k, &st, hint)
	}
	for k, hint := range p.read {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read(k, hint)
	}
	for k, sha := range p.verify {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := m.s.GetAttachment(sha); err == nil {
			continue
		}
		// The snapshot went missing (a backup taken between two
		// writes, a disk that filled): take it again.
		read(k, graphHashHint{})
	}
	return got, nil
}

// applyScanLocked installs what a scan read, drops what it found gone,
// re-reads the org chart and the project files, and rebuilds. A file a
// publish changed after the scan planned is left as the publish left
// it. The cache is complete afterwards.
func (m *GraphMaintainer) applyScanLocked(p graphScanPlan, got map[graphKey]*graphEntry) (*graph.Index, []string, error) {
	fresh := func(k graphKey) bool {
		if m.removed[k] > p.gen {
			return false
		}
		e := m.files[k]
		return e == nil || e.gen <= p.gen
	}
	for k, e := range got {
		if fresh(k) {
			m.installLocked(k, e)
		}
	}
	for _, k := range p.gone {
		if e := m.files[k]; e != nil && e.gen <= p.gen {
			m.installLocked(k, nil)
		}
	}
	agents, owners, err := m.s.graphOrg()
	if err != nil {
		return nil, nil, err
	}
	m.agents = agents
	m.dropOwnersLocked(owners)
	m.scanned = true
	return m.rebuildLocked(true)
}

// dropOwnersLocked forgets the files of anyone no longer an owner.
func (m *GraphMaintainer) dropOwnersLocked(owners []string) {
	keep := make(map[string]bool, len(owners))
	for _, o := range owners {
		keep[o] = true
	}
	for k := range m.files {
		if !keep[k.owner] {
			delete(m.files, k)
		}
	}
}

// installLocked puts e in the cache as k, or removes k when e is nil.
func (m *GraphMaintainer) installLocked(k graphKey, e *graphEntry) {
	m.gen++
	if e == nil {
		if _, had := m.files[k]; had {
			if m.removed == nil {
				m.removed = map[graphKey]uint64{}
			}
			m.removed[k] = m.gen
		}
		delete(m.files, k)
		return
	}
	e.gen = m.gen
	m.files[k] = e
}

// hintLocked is what is known of k's last snapshot: the cache's, else
// the version log's.
func (m *GraphMaintainer) hintLocked(k graphKey) graphHashHint {
	if e := m.files[k]; e != nil && e.sha != "" {
		return graphHashHint{sha: e.sha, size: e.size, mtime: e.mtime, ino: e.ino}
	}
	return m.versions.lastByPath[k.hintKey()]
}

// readEntry reads one published file into a cache entry, snapshotting
// its bytes. Nil when the file is not there (or is not a regular file,
// which the walk would not list either). A manifest candidate is read
// and parsed; anything else is streamed into the snapshot store,
// unless its size, mtime and inode (where hint knows it) match hint
// and hint's snapshot exists, in
// which case nothing is read. A snapshot that fails leaves sha empty,
// for the rebuild to retry and report.
//
// Every read goes through files.OpenNoFollowIn: no link is followed
// at any depth.
func (m *GraphMaintainer) readEntry(root *os.Root, k graphKey, walked *graphStat, hint graphHashHint) *graphEntry {
	isMD := strings.EqualFold(path.Ext(k.rel), ".md")
	fh, err := files.OpenNoFollowIn(root, k.rel)
	if err != nil {
		if publishedGone(err) {
			return nil
		}
		e := &graphEntry{}
		if walked != nil {
			e.size, e.mtime, e.ino = walked.size, walked.mtime, walked.ino
		}
		if isMD && e.size <= graphManifestMaxBytes {
			e.md, e.readErr = true, err
		}
		return e
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		return &graphEntry{md: isMD, readErr: err}
	}
	e := &graphEntry{size: info.Size(), mtime: info.ModTime().UnixNano(), ino: fileIno(info)}
	if isMD && e.size <= graphManifestMaxBytes {
		e.md = true
		body, err := io.ReadAll(fh)
		if err != nil {
			e.readErr = err
			return e
		}
		facts := graph.ReadFacts(body)
		e.facts = &facts
		sum := sha256.Sum256(body)
		sha := hex.EncodeToString(sum[:])
		if _, err := m.s.GetAttachment(sha); err == nil {
			e.sha = sha
			return e
		}
		if att, err := m.s.AddAttachmentSnapshot(path.Base(k.rel), bytes.NewReader(body)); err == nil {
			e.sha = att.SHA
		} else {
			e.body = body
		}
		return e
	}
	if hint.sha != "" && hint.size == e.size && hint.mtime == e.mtime && (hint.ino == 0 || hint.ino == e.ino) {
		if _, err := m.s.GetAttachment(hint.sha); err == nil {
			e.sha = hint.sha
			return e
		}
	}
	if att, err := m.s.AddAttachmentSnapshot(path.Base(k.rel), fh); err == nil {
		e.sha = att.SHA
	}
	return e
}

// snapshotLocked retries a snapshot a read could not take.
func (m *GraphMaintainer) snapshotLocked(k graphKey, e *graphEntry) (string, error) {
	if e.sha != "" {
		return e.sha, nil
	}
	var att Attachment
	var err error
	if e.body != nil {
		att, err = m.s.AddAttachmentSnapshot(path.Base(k.rel), bytes.NewReader(e.body))
	} else {
		var fh *os.File
		fh, err = files.OpenNoFollow(m.s.path(files.PublishedDirName), k.owner+"/"+k.rel)
		if err != nil {
			return "", err
		}
		att, err = m.s.AddAttachmentSnapshot(path.Base(k.rel), fh)
		_ = fh.Close()
	}
	if err != nil {
		return "", err
	}
	e.sha, e.body = att.SHA, nil
	return e.sha, nil
}

// rebuildLocked builds the index from the cache: plan names, cut a
// version for every node whose bytes moved, append those to the log,
// build, and publish the index if its sequence moved. refreshProject
// re-reads the project-file index; otherwise the inputs from the last
// read stand.
//
// A version is recorded only once its bytes are in the attachment
// store, and the SHA recorded is the SHA of the bytes stored. A file
// that cannot be read or stored keeps its previous versions and is
// reported in the warnings.
func (m *GraphMaintainer) rebuildLocked(refreshProject bool) (*graph.Index, []string, error) {
	now := m.clk.Now().UTC()
	cur := m.ix.Load()
	floor := m.versions.maxSeq
	nextSeq := floor + 1
	if cur != nil && cur.Seq+1 > nextSeq {
		nextSeq = cur.Seq + 1
	}
	var warnings []string
	inputs := make([]graph.FileInput, 0, len(m.files))
	for k, e := range m.files {
		in := graph.FileInput{Owner: k.owner, Path: k.rel}
		switch {
		case e.readErr != nil:
			// A manifest that could not be read keeps the id its last
			// version recorded. Without this a file with a declared id
			// would be planned under its path-derived name, its node
			// would seem to vanish, and everything resting on it would
			// be flagged, then un-flagged when it reads again.
			if hint, ok := m.versions.lastByPath[k.hintKey()]; ok && hint.id != "" {
				in.Name = strings.TrimPrefix(hint.id, k.owner+"/")
			}
		case e.md:
			in.Markdown = true
			in.Facts = e.facts
		}
		inputs = append(inputs, in)
	}

	// Plan first: versions are keyed by node id, and the id of a file
	// with a declared id is only known once its manifest is parsed.
	placements := graph.Plan(graph.Input{Files: inputs})
	placed := make(map[string]graph.Placement, len(placements))
	for _, p := range placements {
		placed[p.Owner+"\x00"+p.Path] = p
	}
	var records []graphVersionRecord
	for _, p := range placements {
		if p.ID == "" {
			continue
		}
		k := graphKey{owner: p.Owner, rel: p.Path}
		e := m.files[k]
		if e == nil {
			continue
		}
		if e.readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s/%s: %v (keeping its previous versions)", p.Owner, p.Path, e.readErr))
			continue
		}
		sha, err := m.snapshotLocked(k, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s/%s: snapshot: %v (no version cut this pass)", p.Owner, p.Path, err))
			continue
		}
		var payload *graphEntry
		payloadSHA := ""
		if p.Payload != "" {
			pk := graphKey{owner: p.Owner, rel: p.Payload}
			payload = m.files[pk]
			if payload == nil {
				continue
			}
			if payload.readErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s/%s: %v (no version cut this pass)", p.Owner, p.Payload, payload.readErr))
				continue
			}
			payloadSHA, err = m.snapshotLocked(pk, payload)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s/%s: snapshot: %v (no version cut this pass)", p.Owner, p.Payload, err))
				continue
			}
		}
		history := m.versions.byID[p.ID]
		last := graphVersionRecord{}
		if len(history) > 0 {
			last = history[len(history)-1]
		}
		if last.N > 0 && last.SHA == sha && last.PayloadSHA == payloadSHA {
			continue
		}
		rec := graphVersionRecord{
			ID: p.ID, Owner: p.Owner, Path: p.Path, N: last.N + 1, Seq: nextSeq, TS: now,
			SHA: sha, Size: e.size, MTime: e.mtime,
		}
		if payload != nil {
			rec.Payload = p.Payload
			rec.PayloadSHA = payloadSHA
			rec.PayloadSz = payload.size
			rec.PayloadMT = payload.mtime
		}
		records = append(records, rec)
	}

	project := m.project
	if refreshProject || !m.projectLoaded {
		var projectRecords []graphVersionRecord
		var err error
		project, projectRecords, err = m.s.graphProjectFileInputs(m.versions, now, nextSeq)
		if err != nil {
			return nil, warnings, err
		}
		records = append(records, projectRecords...)
	}
	if len(records) > 0 {
		if err := m.s.appendGraphVersions(records); err != nil {
			return nil, warnings, err
		}
		m.versions.add(records)
	}
	m.project, m.projectLoaded = project, true

	for i := range inputs {
		p := placed[inputs[i].Owner+"\x00"+inputs[i].Path]
		if p.ID == "" {
			continue
		}
		for _, rec := range m.versions.byID[p.ID] {
			inputs[i].Versions = append(inputs[i].Versions, rec.version())
		}
	}
	inputs = append(inputs, project...)

	ix := graph.Build(graph.Input{Now: now, Prev: cur, SeqFloor: floor, Agents: m.agents, Files: inputs})
	if cur != nil && ix.Seq == cur.Seq {
		// Nothing moved: the current index is already this one.
		return cur, warnings, nil
	}
	m.ix.Store(ix)
	return ix, warnings, nil
}

// graphOrg lists every agent, active and archived, as org-chart input,
// and the owners whose published trees the graph indexes: everyone but
// the CEO, whose nodes are project files. An archived agent's nodes
// stay in the graph, owned by an archived agent; deleting them would
// dangle every edge that pointed at them.
//
// Read under orgMu, which ArchiveAgent holds for its rename: the
// active and archived listings are two reads, and a rename landing
// between them would list the agent twice.
func (s *FSStore) graphOrg() ([]graph.AgentInput, []string, error) {
	s.orgMu.RLock()
	defer s.orgMu.RUnlock()
	active, err := s.ListActiveAgents()
	if err != nil {
		return nil, nil, err
	}
	archived, err := s.ListArchivedAgents()
	if err != nil {
		return nil, nil, err
	}
	var agents []graph.AgentInput
	var owners []string
	for _, a := range active {
		agents = append(agents, graph.AgentInput{Slug: a.Slug, Role: a.Role, ReportsTo: a.ReportsTo})
		if a.Slug != graph.CEOSlug {
			owners = append(owners, a.Slug)
		}
	}
	for _, a := range archived {
		agents = append(agents, graph.AgentInput{Slug: a.Slug, Role: a.Role, ReportsTo: a.ReportsTo, Archived: true})
		owners = append(owners, a.Slug)
	}
	return agents, owners, nil
}

// publishedGone reports whether err means the path is not a regular
// file under the tree: missing, a link, a directory, or under
// something that is not a directory. The walk lists none of those.
func publishedGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, files.ErrSymlink) ||
		errors.Is(err, files.ErrNotRegular) || errors.Is(err, files.ErrNotDir)
}

// hiddenPublishedPath reports whether any component of rel is
// dot-named; the walk skips those and publishing refuses them.
func hiddenPublishedPath(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
