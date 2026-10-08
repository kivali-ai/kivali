package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// stampSettle is how long a file must have gone unmodified before its
// stamp may stand for its content. File timestamps come from a coarse
// clock (a few milliseconds on Linux), so a write landing in the same
// tick as the read before it can leave size and time unchanged; past
// this window no later write can. Git trusts its index the same way.
const stampSettle = 2 * time.Second

// FileStamp is one version of a file as its metadata tells it: whether
// it exists, its size and its modification time. A reader that cached
// what it derived from a file can skip the next read when the file's
// stamp still Matches the one taken before that read.
type FileStamp struct {
	exists  bool
	size    int64
	modNano int64
	// settled: last modified longer than stampSettle before the stamp
	// was taken, so a write since would have moved size or time.
	settled bool
}

// Matches reports whether a cache taken under s still holds for a file
// stamped now as cur: both describe the same version, and s was settled
// when taken.
func (s FileStamp) Matches(cur FileStamp) bool {
	return s.settled && s.exists == cur.exists && s.size == cur.size && s.modNano == cur.modNano
}

func stampOf(path string) FileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		// Missing (or unreadable: the read that follows reports it). A
		// missing file is settled: creating it changes the stamp.
		return FileStamp{settled: true}
	}
	return stampInfo(fi)
}

func stampInfo(fi fs.FileInfo) FileStamp {
	mod := fi.ModTime()
	return FileStamp{
		exists:  true,
		size:    fi.Size(),
		modNano: mod.UnixNano(),
		settled: time.Since(mod) > stampSettle,
	}
}

// ChatHistoryStamp stamps slug's chat.jsonl, the file ReadChatHistory
// reads. Take it before the read.
func (s *FSStore) ChatHistoryStamp(slug string) FileStamp {
	return stampOf(s.path("agents", slug, "chat.jsonl"))
}

// ContextWindowStamp stamps slug's context-window sidecar, the file
// ReadContextWindow reads. Take it before the read.
func (s *FSStore) ContextWindowStamp(slug string) FileStamp {
	return stampOf(s.path("agents", slug, contextWindowFilename))
}

// messageCounts remembers, per directory under messages/, how many
// message files it holds directly and which subdirectories it has, so
// CountMessages lists only the directories whose entries changed since
// (an added, removed or renamed entry moves its directory's stamp).
type messageCounts struct {
	mu   sync.Mutex
	dirs map[string]dirCount
	// listed counts directory listings, for tests.
	listed int
}

type dirCount struct {
	stamp   FileStamp
	files   int
	subdirs []string
}

// CountMessages is the number of *.md files under messages/, skipping
// dot directories (.archived, .redacted, reset backups) as
// ListMessages does, without parsing any of them. Used as a cheap
// "did anything new land?" signal in every org snapshot, so it lists
// only the directories that changed since the last count: one stat
// per message bucket rather than a walk of every message ever sent.
func (s *FSStore) CountMessages() (int, error) {
	s.msgCounts.mu.Lock()
	defer s.msgCounts.mu.Unlock()
	if s.msgCounts.dirs == nil {
		s.msgCounts.dirs = map[string]dirCount{}
	}
	seen := map[string]bool{}
	n, err := s.countMessagesIn(s.path("messages"), seen)
	for dir := range s.msgCounts.dirs {
		if !seen[dir] {
			delete(s.msgCounts.dirs, dir)
		}
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// countMessagesIn counts dir and everything below it. Caller holds
// msgCounts.mu.
func (s *FSStore) countMessagesIn(dir string, seen map[string]bool) (int, error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	seen[dir] = true
	stamp := stampInfo(fi)
	c, ok := s.msgCounts.dirs[dir]
	if !ok || !c.stamp.Matches(stamp) {
		s.msgCounts.listed++
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		c = dirCount{stamp: stamp}
		for _, e := range entries {
			switch {
			case e.IsDir():
				if !strings.HasPrefix(e.Name(), ".") {
					c.subdirs = append(c.subdirs, filepath.Join(dir, e.Name()))
				}
			case strings.HasSuffix(e.Name(), ".md"):
				c.files++
			}
		}
		s.msgCounts.dirs[dir] = c
	}
	total := c.files
	for _, sub := range c.subdirs {
		n, err := s.countMessagesIn(sub, seen)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// ForgetReadCaches drops what the store remembers about files it read,
// for a restore: it puts back files with the modification times they
// had when backed up.
func (s *FSStore) ForgetReadCaches() {
	s.msgCounts.mu.Lock()
	s.msgCounts.dirs = nil
	s.msgCounts.mu.Unlock()
}
