package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// archiveAttachmentIndexFilename is the per-agent index of every
// attachment referenced from an ARCHIVED chat.jsonl. Append-only
// JSONL, one entry per (archive, attachment) pair, lazily backfilled
// by ensureArchiveAttachmentIndex so SyncAgentFilesystem does not
// walk every archive on every call.
//
// Scope — what is and isn't in this index:
//
//   - Archived chats only. Each entry is keyed by ArchiveTS, the
//     directory name under agents/<slug>/chats/ (i.e., the moment
//     ArchiveChat ran on that generation, NOT the message or upload
//     time). The live chat.jsonl has no archive ts and is intentionally
//     absent from this index — walkAllChatAttachments re-reads it
//     directly on every sync.
//   - Why the asymmetry: live chat.jsonl mutates on every message
//     append and must be re-read for sync to see new attachments
//     anyway; archives are immutable, so caching them is the only
//     side that benefits.
const archiveAttachmentIndexFilename = "archive_attachments.jsonl"

// archiveAttachmentEntry indexes one (archived chat, attachment)
// pair.
//
// ArchiveTS is the archive directory name (UTC timestamp from
// ArchiveChat — e.g. "20260513T143022.123456789Z"). It is the chat
// rotation timestamp, NOT the attachment's upload or message
// timestamp.
//
// SHA == "" is a sentinel marking "this archive was scanned and had
// no attachments" — keeps an empty archive from re-scanning on every
// sync. Sentinels are filtered out before the entry list is returned
// to consumers.
type archiveAttachmentEntry struct {
	ArchiveTS string `json:"archive_ts"`
	SHA       string `json:"sha,omitempty"`
	Name      string `json:"name,omitempty"`
}

func (s *FSStore) archiveAttachmentIndexPath(slug string) string {
	return s.path("agents", slug, archiveAttachmentIndexFilename)
}

// readArchiveAttachmentIndex returns every entry on disk in
// file-append order. Missing file is not an error (returns nil).
func (s *FSStore) readArchiveAttachmentIndex(slug string) ([]archiveAttachmentEntry, error) {
	f, err := os.Open(s.archiveAttachmentIndexPath(slug))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []archiveAttachmentEntry
	err = jsonlLines(f, 1*1024*1024, func(line []byte) error {
		var e archiveAttachmentEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("archive attachment index %s: %w", slug, err)
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appendArchiveAttachmentEntries appends entries as a single contiguous
// write. POSIX guarantees the write is atomic when it fits a single
// syscall and the file is opened O_APPEND, which keeps concurrent
// writers from interleaving bytes; logical duplicates between racing
// writers are tolerated by the SHA-keyed dedup in the consumers.
func (s *FSStore) appendArchiveAttachmentEntries(slug string, entries []archiveAttachmentEntry) error {
	if len(entries) == 0 {
		return nil
	}
	path := s.archiveAttachmentIndexPath(slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return appendJSONL(path, buf.Bytes())
}

// ensureArchiveAttachmentIndex returns archived-chat attachment
// entries grouped newest-archive-first, with entries inside each
// archive in original message-append order. Any archive not yet
// represented in the on-disk index is read once and appended.
// Sentinel entries (SHA == "") are written for archives with zero
// attachments so they don't re-scan on every call; sentinels are
// filtered from the returned slice.
//
// Steady state: one open + scan of the index file plus ReadDir of
// chats/. Cost does not grow with archive count — only with the
// number of distinct attachments ever seen. Backfill on first call
// after a new archive lands is one ReadArchivedChat for that archive.
//
// We do not lock. Concurrent calls can both backfill the same
// archive, leaving duplicate entries; collectAttachmentRefs /
// AttachmentSHAsForAgent dedup by SHA, so duplicates are benign and
// bounded (one extra per race window per archive).
func (s *FSStore) ensureArchiveAttachmentIndex(slug string) ([]archiveAttachmentEntry, error) {
	raw, err := s.readArchiveAttachmentIndex(slug)
	if err != nil {
		return nil, err
	}
	indexed := map[string]bool{}
	for _, e := range raw {
		indexed[e.ArchiveTS] = true
	}
	archives, err := s.ListArchivedChats(slug)
	if err != nil {
		return nil, err
	}
	var pending []archiveAttachmentEntry
	for _, a := range archives {
		if indexed[a.Timestamp] {
			continue
		}
		hist, rerr := s.ReadArchivedChat(slug, a.Timestamp)
		if rerr != nil {
			if errors.Is(rerr, ErrNotFound) {
				continue
			}
			return nil, rerr
		}
		added := 0
		for _, m := range hist {
			for _, att := range m.Attachments {
				if att.SHA == "" {
					continue
				}
				pending = append(pending, archiveAttachmentEntry{
					ArchiveTS: a.Timestamp,
					SHA:       att.SHA,
					Name:      att.Name,
				})
				added++
			}
		}
		if added == 0 {
			pending = append(pending, archiveAttachmentEntry{ArchiveTS: a.Timestamp})
		}
	}
	if len(pending) > 0 {
		if err := s.appendArchiveAttachmentEntries(slug, pending); err != nil {
			return nil, err
		}
		raw = append(raw, pending...)
	}

	byTS := map[string][]archiveAttachmentEntry{}
	for _, e := range raw {
		if e.SHA == "" {
			continue
		}
		byTS[e.ArchiveTS] = append(byTS[e.ArchiveTS], e)
	}
	tss := make([]string, 0, len(byTS))
	for ts := range byTS {
		tss = append(tss, ts)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(tss)))
	out := make([]archiveAttachmentEntry, 0, len(raw))
	for _, ts := range tss {
		out = append(out, byTS[ts]...)
	}
	return out, nil
}
