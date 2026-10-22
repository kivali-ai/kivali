package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// An integration's log: integrations/<name>/log.jsonl, the lines its
// server wrote (its log notifications and its error output) and
// Kivali's own about it (started, exited, restarted), with the
// connection's secret values already replaced by whoever appends. The
// file is capped: past integrationLogMax lines it is rewritten with the
// last integrationLogKeep.

const (
	integrationLogName = "log.jsonl"
	integrationLogMax  = 4000
	integrationLogKeep = 2000
	// integrationLogMessageMax bounds one line's message.
	integrationLogMessageMax = 8 << 10
)

// IntegrationLogRow is one line of an integration's log.
type IntegrationLogRow struct {
	At time.Time `json:"at"`
	// Level is info, warning, error or debug (detail, such as apt-get's
	// output while the integration's software installs), or stderr for a
	// line the server wrote to its error output.
	Level   string `json:"level"`
	Message string `json:"message"`
}

// AppendIntegrationLog appends one line to the integration's log, its
// message cut to 8 KiB. The caller has replaced every secret in it. A
// log line is not synced to disk: losing the last few in a power cut
// costs nothing, and a cut line is skipped when read.
func (s *FSStore) AppendIntegrationLog(name string, row IntegrationLogRow) error {
	unlock := s.lockIntegration(name)
	defer unlock()
	dir, err := s.integrationDir(name)
	if err != nil {
		return err
	}
	if _, err := s.GetIntegration(name); err != nil {
		return err
	}
	row.At = row.At.UTC()
	if len(row.Message) > integrationLogMessageMax {
		row.Message = row.Message[:integrationLogMessageMax]
	}
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, integrationLogName)
	n, ok := s.integrationLogLines.Load(name)
	if !ok {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n = bytes.Count(b, []byte{'\n'})
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	count := n.(int) + 1
	if count > integrationLogMax {
		if count, err = trimJSONL(path, integrationLogKeep); err != nil {
			s.integrationLogLines.Delete(name)
			return err
		}
	}
	s.integrationLogLines.Store(name, count)
	return nil
}

// trimJSONL rewrites the file at path with its last keep lines and
// returns how many it kept.
func trimJSONL(path string, keep int) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lines := bytes.SplitAfter(b, []byte{'\n'})
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	if err := writeAtomic(path, bytes.Join(lines, nil), 0o644); err != nil {
		return 0, err
	}
	return len(lines), nil
}

// ReadIntegrationLogTail returns the integration's latest n log lines,
// oldest first. A line that does not parse (an append cut short) is
// skipped. No log yet is no lines.
func (s *FSStore) ReadIntegrationLogTail(name string, n int) ([]IntegrationLogRow, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	lines, err := tailLines(filepath.Join(dir, integrationLogName), n)
	if err != nil {
		return nil, err
	}
	var newest []IntegrationLogRow
	for _, line := range lines {
		if len(newest) == n {
			break
		}
		var row IntegrationLogRow
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		newest = append(newest, row)
	}
	out := make([]IntegrationLogRow, 0, len(newest))
	for i := len(newest) - 1; i >= 0; i-- {
		out = append(out, newest[i])
	}
	return out, nil
}
