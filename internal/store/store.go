// Package store provides filesystem-backed persistence for Kivali.
//
// All biz-critical state lives under a single root directory as plain
// files so it remains accessible if the app is offline — the operator
// can cd in, grep, and continue working manually in claude.ai.
//
// The layout is stable and part of the store's contract:
//
//	<root>/
//	  handbook.md                    the handbook, app-wide rules
//	  org_chart.yaml                 materialized org projection
//	  message_queue.json             per-agent inboxes awaiting release
//	  usage.jsonl                    append-only API-call cost log
//	  agents/<slug>/                 active agents
//	    agent.yaml
//	    role.md
//	    agent_memory.md     (optional)
//	    chat.jsonl
//	    memory/                      the agent's /files/ tree
//	  agents/_archived/<slug>/       archived agents (offboarded, history preserved)
//	  messages/<YYYY-MM-DD>/<file>   inter-agent messages, append-only
//	  project_files/<sha>/           content-addressed project-wide uploads
//	    original.<ext>
//	    <canonical>                  converted form for Claude, if any
//	  project_files/index.yaml       upload metadata
//	  attachments/<sha>/             content-addressed blobs referenced by
//	    blob.<ext>                   chat messages / inbox messages / per-agent
//	    canonical.txt                file scopes; no global index — visibility
//	    meta.json                    is by reference
//	  public/<slug>/                 each agent's published files; core
//	                                 is the only writer (publish.go)
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/kivali-ai/kivali/internal/files"
)

// ErrNotFound is returned by read methods when the requested entity does
// not exist in the store.
var ErrNotFound = errors.New("store: not found")

// FSStore is a filesystem-backed implementation of Kivali persistence.
// All methods are safe for concurrent use from a single process.
type FSStore struct {
	root           string
	egressSyncPath string

	// messageQueueMu serializes read-modify-write of message_queue.json
	// across every caller — Messenger.Route / RouteProduced / Bounce /
	// ReleaseAll and the tracker's queue prune. Each caller holds this lock
	// for the entire RMW so concurrent writers can't lose mutations.
	// Acquire via LockMessageQueue / UnlockMessageQueue; the Messenger
	// exposes the same lock through its own Lock/Unlock so the
	// "messenger orchestration" framing still works.
	messageQueueMu sync.Mutex

	// brandingMu serialises branding.yaml's read-modify-writes, so a
	// logo upload racing a settings save cannot drop either's field.
	brandingMu sync.Mutex

	// msgCounts is CountMessages' memory of the messages/ tree.
	msgCounts messageCounts

	// onMessageQueueWrite, when non-nil, is invoked after every
	// successful WriteMessageQueue. Lets the web layer push a fresh
	// sidebar/inbox snapshot to /org/stream subscribers without
	// every caller having to remember to notify. Set via
	// SetOnMessageQueueWrite during server construction; safe to
	// leave nil (no-op).
	//
	// Fires synchronously on the writer's goroutine. Implementations
	// MUST not block — the suggested pattern is "send on a buffered
	// channel and return," which is what Server.NotifyOrgState does.
	onMessageQueueWrite func()

	// pendingAttachments holds, per slug, attachments that
	// SyncAgentAttachments linked ahead of the message carrying them
	// — a message buffered behind a running turn. Every attachment
	// walk visits them after the live chat until AppendChatMessage
	// lands that message, so no sync run in between can reconcile
	// the links away. In-memory on purpose: the buffered message is
	// too, and if the process dies both go together.
	pendingAttachmentsMu sync.Mutex
	pendingAttachments   map[string][]MessageAttachment

	// unlinkedChat marks, per slug, rows appended with
	// AppendChatMessageLinkLater whose attachments LinkChatAttachments
	// has not linked yet.
	unlinkedChatMu sync.Mutex
	unlinkedChat   map[string]bool

	// usageMu serializes appends to usage.jsonl. Per-store rather than
	// package-scoped so two FSStores (test fixtures) don't pointlessly
	// serialize on each other. AppendUsage opens the file with O_APPEND
	// and fsyncs, so kernel-level append ordering is already atomic;
	// this mutex exists to keep our application-level invariant that
	// one call writes exactly one line (a partial-line race could happen
	// if Go's write didn't go through as a single syscall).
	usageMu sync.Mutex

	// skillSettingsMu serializes read-modify-write of
	// skill_settings.json (the enabled/disabled switches). Same
	// contract as messageQueueMu: two toggles landing together without
	// it would read the same state and the second write would drop the
	// first one's change.
	skillSettingsMu sync.Mutex

	// graphM is the knowledge-graph maintainer (graph_maint.go),
	// made on first use by Graph.
	graphOnce sync.Once
	graphM    *GraphMaintainer

	// orgMu is held for writing by ArchiveAgent's move between the
	// active and archived listings, and for reading by the graph
	// maintainer's listing of both, which would otherwise see a
	// mid-move agent twice.
	orgMu sync.RWMutex

	// assignmentsMu serializes every change to an assignment file. Ids and log
	// sequence numbers are derived from the files, so two changes
	// interleaved would allocate the same ones. Acquire via
	// LockAssignments / UnlockAssignments; the tracker holds it from read to
	// write and releases it before routing any wake.
	assignmentsMu sync.Mutex

	// onAssignmentWrite, when non-nil, is invoked after every successful
	// WriteAssignment. Same contract as onMessageQueueWrite: it fires
	// synchronously on the writer's goroutine, while the tracker still
	// holds assignmentsMu, so implementations must not block. Set via
	// SetOnAssignmentWrite during server construction; nil is a no-op.
	onAssignmentWrite func()

	// onUsageAppend, when non-nil, is invoked after every successful
	// AppendUsage, on the appender's goroutine while usageMu is held,
	// so implementations must not block. Set via SetOnUsageAppend
	// during server construction; nil is a no-op.
	onUsageAppend func()
}

// LockMessageQueue blocks until the caller owns the message-queue
// read-modify-write lock. Pair with UnlockMessageQueue (defer
// recommended). Every caller doing `read → mutate → write` of
// message_queue.json MUST hold this; concurrent writers without it
// silently lose mutations (one caller's read predates the other's
// write, both then write back, second write wins).
func (s *FSStore) LockMessageQueue() { s.messageQueueMu.Lock() }

// UnlockMessageQueue releases the message-queue lock acquired via
// LockMessageQueue.
func (s *FSStore) UnlockMessageQueue() { s.messageQueueMu.Unlock() }

// shaWriteMu is a global per-SHA mutex map used to serialize writes
// to attachments/<sha>/* and project_files/<sha>/*. Two writers
// adding the same content (same SHA) would otherwise race on the blob
// rename + canonical extraction + meta.json write — deterministic in
// content, but the meta.json's Name field would last-writer-win, and
// two extractions writing one canonical.txt can corrupt it.
//
// sync.Map keyed by the hex SHA. Mutex is created lazily on first
// LoadOrStore and survives for the process lifetime — the cardinality
// is bounded by the number of distinct SHAs ever touched, which is
// the same as the number of attachments + project files. For a
// long-lived deployment with many attachments this leaks bounded
// memory; if that ever matters, switch to a Mu pool with eviction.
var shaWriteMu sync.Map

// LockSHA acquires the per-SHA write lock. Pair with UnlockSHA (defer
// recommended). Used by AddAttachment / AddProjectFile / the on-demand
// PDF canonicalization to serialize writes within a single SHA's
// directory; concurrent operations on different SHAs run in parallel.
func (s *FSStore) LockSHA(sha string) {
	v, _ := shaWriteMu.LoadOrStore(sha, &sync.Mutex{})
	v.(*sync.Mutex).Lock()
}

// UnlockSHA releases the per-SHA write lock acquired via LockSHA.
// Panics if called without a matching LockSHA.
func (s *FSStore) UnlockSHA(sha string) {
	v, ok := shaWriteMu.Load(sha)
	if !ok {
		panic("UnlockSHA called without LockSHA")
	}
	v.(*sync.Mutex).Unlock()
}

// SetOnMessageQueueWrite installs the post-write callback. Web layer
// wires this to Server.NotifyOrgState in NewServer. Safe to call
// once at startup before any concurrent writers; not safe to mutate
// live.
func (s *FSStore) SetOnMessageQueueWrite(fn func()) {
	s.onMessageQueueWrite = fn
}

// SetOnAssignmentWrite installs the post-write callback for assignment files.
// Same rules as SetOnMessageQueueWrite: set once at startup, never
// mutated live, and the callback must not block.
func (s *FSStore) SetOnAssignmentWrite(fn func()) {
	s.onAssignmentWrite = fn
}

// SetOnUsageAppend installs the post-append callback for usage.jsonl.
// Same rules as SetOnMessageQueueWrite: set once at startup, never
// mutated live, and the callback must not block.
func (s *FSStore) SetOnUsageAppend(fn func()) {
	s.onUsageAppend = fn
}

// New returns a store rooted at dir, creating the directory layout if
// missing. Existing data is preserved.
func New(dir string) (*FSStore, error) {
	subdirs := []string{
		"agents",
		filepath.Join("agents", "_archived"),
		"messages",
		"project_files",
		"attachments",
		files.PublishedDirName,
	}
	for _, sd := range subdirs {
		p := filepath.Join(dir, sd)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", p, err)
		}
	}
	return &FSStore{root: dir}, nil
}

// Open returns a store rooted at dir without creating anything, for a
// reader that must leave the directory as it found it.
func Open(dir string) *FSStore { return &FSStore{root: dir} }

// Root returns the root directory of the store.
func (s *FSStore) Root() string { return s.root }

func (s *FSStore) path(parts ...string) string {
	return filepath.Join(append([]string{s.root}, parts...)...)
}

// writeAtomic writes data to path via a temp file and rename, so the
// target is never visible in a half-written state, and syncs the
// directory after the rename, so once it returns a power cut cannot
// bring back the old content (or, for a new file, no file at all).
func writeAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return syncDir(dir)
}

// syncDir fsyncs the directory dir, making the entries renamed or
// created in it durable. Windows cannot open a directory for a sync,
// and NTFS journals the rename itself; there it is a no-op.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
