package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArchivedChat identifies one archived generation of an agent's chat.
// Directory layout: agents/<slug>/chats/<Timestamp>/chat.jsonl
type ArchivedChat struct {
	Slug      string
	Timestamp string
}

// NewArchiveTimestamp mints the name of the next archive generation.
// The rotation flow mints it BEFORE the memory-update turn so the
// agent can cite the coming episode as [[ep:<ts>]] while the chat is
// still open, then archives under that same name in finalize.
func NewArchiveTimestamp() string {
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}

// ArchiveChat moves the agent's current chat.jsonl into a freshly
// minted timestamped subdirectory under agents/<slug>/chats/ and
// starts an empty chat.jsonl. Returns the archive timestamp (also the
// subdirectory name). Callers that already hold a timestamp — the
// rotation flow — use ArchiveChatAs.
func (s *FSStore) ArchiveChat(slug string) (string, error) {
	ts := NewArchiveTimestamp()
	if err := s.ArchiveChatAs(slug, ts); err != nil {
		return "", err
	}
	return ts, nil
}

// ArchiveChatAs archives the current chat under a caller-supplied
// generation name. Idempotent for a fixed ts: if that generation
// already holds a transcript, the live chat.jsonl is left where it is
// rather than moved over it. That is what makes the crash-recovery
// re-run safe — a Kivali restart between archive and marker-clear
// re-finds the pending rotation, calls this again with the same ts,
// and must not swallow whatever the fresh chat has accumulated since.
func (s *FSStore) ArchiveChatAs(slug, ts string) error {
	if slug == "" {
		return errors.New("archive chat: empty slug")
	}
	if ts == "" || ts != filepath.Base(ts) {
		return fmt.Errorf("archive chat %s: bad generation name %q", slug, ts)
	}
	if _, err := os.Stat(s.path("agents", slug)); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}

	mu := chatLock(slug)
	mu.Lock()
	defer mu.Unlock()

	archiveDir := s.path("agents", slug, "chats", ts)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return err
	}
	src := s.path("agents", slug, "chat.jsonl")
	dst := filepath.Join(archiveDir, "chat.jsonl")
	switch _, err := os.Stat(dst); {
	case err == nil:
		// Already archived under this name; nothing to move.
	case !errors.Is(err, os.ErrNotExist):
		return err
	default:
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("archive chat %s: %w", slug, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			// No chat yet; create empty archived file for consistency.
			if err := os.WriteFile(dst, nil, 0o644); err != nil {
				return err
			}
		}
	}
	// Recreate fresh chat.jsonl (a no-op when it already exists).
	f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// ListArchivedChats returns archived chat generations for slug, newest
// first. Returns nil without error if the agent has no archive.
func (s *FSStore) ListArchivedChats(slug string) ([]ArchivedChat, error) {
	entries, err := os.ReadDir(s.path("agents", slug, "chats"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ArchivedChat
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, ArchivedChat{Slug: slug, Timestamp: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	return out, nil
}

// WriteArchivedAgentMemory saves a snapshot of an agent's memory
// alongside a specific archived chat generation. Used by the new-chat
// flow to keep the pre-rotation memory recoverable even after the
// live one is replaced.
func (s *FSStore) WriteArchivedAgentMemory(slug, ts, body string) error {
	path := s.path("agents", slug, "chats", ts, agentMemoryFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, []byte(body), 0o644)
}

// ReadArchivedAgentMemory returns the pre-rotation memory snapshot
// stored beside generation ts, or ErrNotFound when there is none (the
// memory was empty at the time).
func (s *FSStore) ReadArchivedAgentMemory(slug, ts string) (string, error) {
	return s.readArchivedFile(slug, ts, agentMemoryFilename)
}

// WriteArchivedAgentPrinciples is WriteArchivedAgentMemory for the
// principles file — the pre-rotation snapshot that makes a principle
// the agent dropped during reconcile recoverable.
func (s *FSStore) WriteArchivedAgentHabits(slug, ts, body string) error {
	path := s.path("agents", slug, "chats", ts, AgentMemoryHabitsFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, []byte(body), 0o644)
}

// ReadArchivedAgentPrinciples returns the pre-rotation principles
// snapshot beside generation ts, or ErrNotFound.
func (s *FSStore) ReadArchivedAgentHabits(slug, ts string) (string, error) {
	return s.readArchivedFile(slug, ts, AgentMemoryHabitsFilename)
}

func (s *FSStore) readArchivedFile(slug, ts, name string) (string, error) {
	b, err := os.ReadFile(s.path("agents", slug, "chats", ts, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// archiveExcerptScanLines bounds how far into an archived chat.jsonl
// ArchivedChatExcerpt reads before giving up. The opening exchange sits
// within the first few lines of every real transcript; the cap is what
// keeps rendering a page of excerpts cheap even when the generations
// behind them are megabytes each.
const archiveExcerptScanLines = 200

// ArchivedChatExcerpt returns the opening line of an archived chat
// generation — what was said TO the agent first, falling back to what
// the agent itself said first. Returns "" (and no error) for a
// generation archived before anyone spoke.
//
// This is the label that makes a long archive browsable: a row
// identified only by its rotation timestamp tells the reader nothing
// about which chat it was. Only the head of the file is read, so
// listing a page of generations costs far less than opening one.
func (s *FSStore) ArchivedChatExcerpt(slug, ts string) (string, error) {
	path := s.path("agents", slug, "chats", ts, "chat.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	fallback := ""
	for i := 0; i < archiveExcerptScanLines && sc.Scan(); i++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m ChatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			// One unreadable head line shouldn't cost the row its
			// label — keep looking for a usable one.
			continue
		}
		// Tool traffic is machinery, not something anyone said.
		if m.Kind == "tool_use" || m.Kind == "tool_result" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		if m.Role == RoleReceived {
			return text, nil
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback, sc.Err()
}

// ReadArchivedChat returns the messages of one archived chat generation.
func (s *FSStore) ReadArchivedChat(slug, ts string) ([]ChatMessage, error) {
	path := s.path("agents", slug, "chats", ts, "chat.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []ChatMessage
	err = jsonlLines(f, 8*1024*1024, func(line []byte) error {
		var m ChatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("archived chat %s/%s: %w", slug, ts, err)
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
