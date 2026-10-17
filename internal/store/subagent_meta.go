package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// SubagentMetaFilename is a background task's record beside its
// transcript: agents/<parent>/subagents/<id>/meta.json.
const SubagentMetaFilename = "meta.json"

// UpdateSubagentMeta changes one background task's meta.json under a
// lock per task. update sees the file's current fields, an empty map
// when there is no file yet, and changes what it must; it returns false
// to leave the file as it is. The result is written whole through a
// temp file and a rename, so a reader never sees a half-written file
// and no writer's change is lost to another's. parent may name an
// archived agent as "_archived/<slug>".
func (s *FSStore) UpdateSubagentMeta(parent, id string, update func(meta map[string]any) bool) error {
	if parent == "" || id == "" {
		return errors.New("UpdateSubagentMeta: parent + id required")
	}
	mu := chatLock(parent + "/subagents/" + id + "/" + SubagentMetaFilename)
	mu.Lock()
	defer mu.Unlock()
	dir := s.path("agents", parent, "subagents", id)
	path := filepath.Join(dir, SubagentMetaFilename)
	meta := map[string]any{}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(body, &meta) != nil {
			// A file nothing can read is replaced by what update
			// writes, not kept as it is.
			meta = map[string]any{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	if !update(meta) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, out, 0o644)
}

// SubagentMetaFiles lists every background task's meta.json under
// agents/, active and archived parents alike, as (parent, id) pairs the
// way UpdateSubagentMeta takes them.
func (s *FSStore) SubagentMetaFiles() ([][2]string, error) {
	var out [][2]string
	for _, pattern := range []string{
		s.path("agents", "*", "subagents", "*", SubagentMetaFilename),
		s.path("agents", "_archived", "*", "subagents", "*", SubagentMetaFilename),
	} {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			rel, err := filepath.Rel(s.path("agents"), filepath.Dir(filepath.Dir(p)))
			if err != nil {
				return nil, err
			}
			// rel is <parent>/subagents or _archived/<parent>/subagents.
			parent := filepath.ToSlash(filepath.Dir(rel))
			out = append(out, [2]string{parent, filepath.Base(filepath.Dir(p))})
		}
	}
	return out, nil
}
