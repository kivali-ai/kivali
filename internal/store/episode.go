package store

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Episodes are the third tier of agent memory: one immutable digest per
// archived chat generation, written by the core-side episode writer
// after the rotation lands and never rewritten. The agent recalls them
// through the /files/episodes/<ts>.md symlink farm (grep, then read);
// semantic memory cites them as [[ep:<ts>]].
//
// On disk an episode lives BESIDE its source transcript —
// agents/<slug>/chats/<ts>/episode.md — rather than in a directory of
// its own. The agent pod already RO-mounts agents/<slug>/chats for the
// past-chats farm, so the same mount carries episodes; the backup walk
// and ArchiveAgent's directory move carry them too. The episode id IS
// the archive timestamp, so nothing has to be minted or reconciled.
const episodeFilename = "episode.md"

// WriteEpisode stores the digest for one archived generation. The
// archive directory must already exist (the rotation created it);
// writing an episode for a generation that was never archived is a
// programming error and surfaces as ErrNotFound. Atomic, so a
// concurrent reader never sees a half-written digest.
func (s *FSStore) WriteEpisode(slug, ts, body string) error {
	dir := s.path("agents", slug, "chats", ts)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return writeAtomic(filepath.Join(dir, episodeFilename), []byte(body), 0o644)
}

// ReadEpisode returns one generation's digest, or ErrNotFound when the
// writer has not produced it yet.
func (s *FSStore) ReadEpisode(slug, ts string) (string, error) {
	b, err := os.ReadFile(s.path("agents", slug, "chats", ts, episodeFilename))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// HasEpisode reports whether the digest for ts exists. The writer's
// idempotency check: a generation is digested at most once.
func (s *FSStore) HasEpisode(slug, ts string) bool {
	_, err := os.Stat(s.path("agents", slug, "chats", ts, episodeFilename))
	return err == nil
}

// EpisodeTitle returns the digest's frontmatter title, the label the
// archive browser prefers over a transcript's opening line. Reads only
// the head of the file. ok is false when there is no digest yet or
// the frontmatter carries no title.
func (s *FSStore) EpisodeTitle(slug, ts string) (title string, ok bool) {
	f, err := os.Open(s.path("agents", slug, "chats", ts, episodeFilename))
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4*1024), 64*1024)
	for i := 0; i < 8 && sc.Scan(); i++ {
		line := sc.Text()
		if i > 0 && line == "---" {
			break
		}
		if rest, found := strings.CutPrefix(line, "title:"); found {
			if t := strings.TrimSpace(rest); t != "" {
				return t, true
			}
			return "", false
		}
	}
	return "", false
}

// ListArchivedChatsWithoutEpisode returns the generations that still
// need a digest, OLDEST first — the writer's backfill order, so a
// long-lived agent's history fills in chronologically.
//
// Generations whose transcript is empty are skipped rather than
// listed forever: nothing was said, so there is nothing to digest.
// (An empty archive only arises from a crash-recovery re-archive;
// handleNewChat refuses to rotate an empty chat.)
func (s *FSStore) ListArchivedChatsWithoutEpisode(slug string) ([]ArchivedChat, error) {
	all, err := s.ListArchivedChats(slug)
	if err != nil {
		return nil, err
	}
	var out []ArchivedChat
	for _, a := range all {
		if s.HasEpisode(slug, a.Timestamp) {
			continue
		}
		info, err := os.Stat(s.path("agents", slug, "chats", a.Timestamp, "chat.jsonl"))
		if err != nil || info.Size() == 0 {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out, nil
}
