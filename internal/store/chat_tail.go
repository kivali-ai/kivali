package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// The longest chat row each reader accepts: ReadChatHistory's for an
// agent's chat.jsonl, ReadChatFile's for a transcript read by path.
const (
	chatHistoryMaxLine = 8 << 20
	chatFileMaxLine    = 16 << 20
)

// ChatTail reads a chat transcript as it grows. Each Read parses only
// the rows appended since the previous one, under the rules of
// ReadChatHistory and ReadChatFile: empty lines are skipped, a complete
// line that is not a chat row fails the read (and every later one, as it
// fails every later full read), and a final line without its '\n'
// counts only when it is a whole JSON value (see the cut-append note in
// jsonl.go). That last row is reported apart, as Tail, because the next
// append completes it: it is read again then, as a row.
//
// A chat file only grows, except that a cut append is truncated away
// (past every complete line, so past what was read) and a rotation moves
// the file away for a new one. A file that is no longer the one read so
// far, or is shorter than what was read, is read again from the top, and
// the Read says so.
//
// A ChatTail is not safe for concurrent use.
type ChatTail struct {
	path    string
	label   string
	maxLine int
	// off is just past the last complete line read; fi is the file it
	// was read from, nil before the first read.
	off int64
	fi  os.FileInfo
}

// ChatTailRead is what one Read found.
type ChatTailRead struct {
	// Reset: what earlier reads returned no longer stands (the file was
	// replaced, shrank or went away). Rows start from the top of the
	// file. Set even when the Read fails.
	Reset bool
	// Rows are the complete rows appended since the previous read.
	Rows []ChatMessage
	// Tail is a final row without its '\n' that is a whole chat row,
	// part of the transcript for this read only.
	Tail *ChatMessage
}

// ChatHistoryTail follows slug's chat.jsonl, the file ReadChatHistory
// reads, with its rules.
func (s *FSStore) ChatHistoryTail(slug string) *ChatTail {
	return &ChatTail{path: s.path("agents", slug, "chat.jsonl"), label: "chat.jsonl " + slug, maxLine: chatHistoryMaxLine}
}

// NewChatFileTail follows the transcript at path, the file ReadChatFile
// would read, with its rules.
func NewChatFileTail(path string) *ChatTail {
	return &ChatTail{path: path, label: "transcript " + path, maxLine: chatFileMaxLine}
}

// Read returns what was appended since the previous Read. A missing
// file is ErrNotFound.
func (t *ChatTail) Read() (ChatTailRead, error) {
	var r ChatTailRead
	f, err := os.Open(t.path)
	if errors.Is(err, os.ErrNotExist) {
		r.Reset = t.fi != nil
		t.off, t.fi = 0, nil
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return r, err
	}
	if t.fi != nil && (!os.SameFile(t.fi, fi) || fi.Size() < t.off) {
		r.Reset = true
		t.off = 0
	}
	t.fi = fi
	keep, tail, err := jsonlTailFrom(f, t.off, fi.Size())
	if err != nil {
		return r, err
	}
	// Parse everything before taking any of it: a line that fails
	// leaves off where it was, so the next Read fails on it again.
	var rows []ChatMessage
	sc := bufio.NewScanner(io.NewSectionReader(f, t.off, keep-t.off))
	sc.Buffer(make([]byte, 0, 64*1024), t.maxLine)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m ChatMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return r, fmt.Errorf("%s: %w", t.label, err)
		}
		rows = append(rows, m)
	}
	if err := sc.Err(); err != nil {
		return r, err
	}
	if v := bytes.TrimSpace(tail); len(v) > 0 && json.Valid(v) {
		var m ChatMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return r, fmt.Errorf("%s: %w", t.label, err)
		}
		r.Tail = &m
	}
	t.off = keep
	r.Rows = rows
	return r, nil
}
