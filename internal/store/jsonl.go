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

// The append-only JSONL files (chat.jsonl, usage.jsonl, the archive
// attachment index) are written one complete line at a time and synced
// before the append is reported. A power cut during an append can still
// leave part of the line on disk: ext4 writes back the first pages of a
// large row and moves the file size past them before the rest. That
// fragment was never acknowledged. Readers ignore it (lines before it
// are intact), and the next append removes it first, so a new row is
// never glued onto it into one unreadable line in the middle of the
// file, which would fail every later read of the whole file.

// jsonlTail splits the file of size bytes into its complete lines, which
// end at keep (just past the last '\n', 0 if none), and the bytes after
// them: a row whose append was cut short, or nil.
func jsonlTail(f *os.File, size int64) (keep int64, tail []byte, err error) {
	const chunk = 64 << 10
	end := size
	for end > 0 {
		start := max(end-chunk, 0)
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			keep = start + int64(i) + 1
			break
		}
		end = start
	}
	if keep == size {
		return keep, nil, nil
	}
	tail = make([]byte, size-keep)
	if _, err := f.ReadAt(tail, keep); err != nil && !errors.Is(err, io.EOF) {
		return 0, nil, err
	}
	return keep, tail, nil
}

// appendJSONL appends data (complete lines, each ending in '\n') to the
// JSONL file at path and syncs it. A final line without its '\n' is
// dealt with first: a whole JSON value (a row whose newline alone was
// lost, or one written by hand) gets its newline; anything else is the
// unacknowledged fragment of a cut append and is truncated away.
func appendJSONL(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > 0 {
		keep, tail, err := jsonlTail(f, fi.Size())
		if err != nil {
			return err
		}
		switch {
		case tail == nil:
		case json.Valid(bytes.TrimSpace(tail)):
			data = append([]byte{'\n'}, data...)
		default:
			if err := f.Truncate(keep); err != nil {
				return err
			}
		}
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// ReadChatFile reads the chat transcript (chat.jsonl rows) at path, with
// the cut-append tolerance of ReadChatHistory, for transcripts the store
// does not name itself (a subagent's, read by path).
func ReadChatFile(path string) ([]ChatMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []ChatMessage
	err = jsonlLines(f, 16*1024*1024, func(line []byte) error {
		var m ChatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("transcript %s: %w", path, err)
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// jsonlLines calls fn with each line of the JSONL file f, in order,
// skipping empty ones. A final line without its '\n' that is not a whole
// JSON value is a cut append (see above) and is skipped; any other line
// fn rejects fails the read.
func jsonlLines(f *os.File, maxLine int, fn func(line []byte) error) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	keep, tail, err := jsonlTail(f, fi.Size())
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(io.NewSectionReader(f, 0, keep))
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if t := bytes.TrimSpace(tail); len(t) > 0 && json.Valid(t) {
		return fn(t)
	}
	return nil
}
