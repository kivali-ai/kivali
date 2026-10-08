package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kivali-ai/kivali/internal/store"
)

// Audit reads a data directory the way the server does and reports what
// it holds and what it cannot read. It writes nothing: run it on a
// restored directory before the first boot, or on any unpacked backup.
func Audit(dataDir string) (*Inventory, error) {
	fi, err := os.Stat(dataDir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dataDir)
	}
	inv := NewInventory()
	err = Walk(dataDir, func(rel string, info fs.FileInfo) error {
		if info.IsDir() {
			return nil
		}
		var content []byte
		if Wants(rel) {
			b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			content = b
		}
		inv.Add(rel, info.Size(), content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	auditReaders(dataDir, inv)
	return inv, nil
}

// auditReaders runs the store's own readers over the directory; any
// error one returns is a problem. Opened without store.New, which
// creates missing directories.
func auditReaders(dataDir string, inv *Inventory) {
	s := store.Open(dataDir)
	fail := func(what string, err error) {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			inv.problem("%s: %v", what, err)
		}
	}
	active, err := s.ListActiveAgents()
	fail("agents", err)
	archived, err := s.ListArchivedAgents()
	fail("archived agents", err)
	for _, a := range active {
		dir := "agents/" + a.Slug
		_, err := s.ReadChatHistory(a.Slug)
		fail(dir+"/chat.jsonl", err)
		_, err = s.ReadRole(a.Slug)
		fail(dir+"/role.md", err)
		_, err = s.ReadAgentMemory(a.Slug)
		fail(dir+"/agent_memory.md", err)
		_, err = s.ReadAgentHabits(a.Slug)
		fail(dir+"/"+store.AgentMemoryHabitsFilename, err)
		chats, err := s.ListArchivedChats(a.Slug)
		fail(dir+"/chats", err)
		for _, c := range chats {
			_, err := s.ReadArchivedChat(a.Slug, c.Timestamp)
			fail(dir+"/chats/"+c.Timestamp, err)
		}
	}
	// The store reads an archived agent's record but not its documents,
	// so their transcripts are checked here: every line a JSON object.
	for _, a := range archived {
		base := filepath.Join(dataDir, "agents", "_archived", a.Slug)
		transcripts, _ := filepath.Glob(filepath.Join(base, "chats", "*", "chat.jsonl"))
		for _, p := range append([]string{filepath.Join(base, "chat.jsonl")}, transcripts...) {
			rel, _ := filepath.Rel(dataDir, p)
			fail(filepath.ToSlash(rel), jsonLines(p))
		}
	}
	_, err = s.ListMessages(store.MessageFilter{})
	fail("messages", err)
	_, err = s.ListAssignments()
	fail(store.AssignmentsDirName, err)
	_, err = s.ListPendingWakes()
	fail(store.AssignmentsDirName+"/pending", err)
	q, err := s.ReadMessageQueue()
	fail("message_queue.json", err)
	for slug, a := range q.Agents {
		for _, p := range a.Inbox {
			if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(p))); err != nil {
				inv.problem("message_queue.json: %s's inbox names %s, which is not there", slug, p)
			}
		}
	}
	_, err = s.ReadGraphIndex()
	fail("graph", err)
	_, err = s.ListProjectFiles()
	fail("project_files", err)
	_, err = s.ReadHandbook()
	fail(store.HandbookFilename, err)
}

// jsonLines checks that every non-empty line of a transcript is a JSON
// object. A missing file is not an error.
func jsonLines(p string) error {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(line, &v); err != nil {
			return fmt.Errorf("line %d: %v", i+1, err)
		}
	}
	return nil
}
