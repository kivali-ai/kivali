package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/convert"
	"github.com/kivali-ai/kivali/internal/graph"
)

// GraphSnapshot is one version of a node as the store kept it. Text
// is the version's content when the bytes are text, capped like any
// file inlined into a prompt; Binary is set instead when they are
// not, and Size says how much was there either way.
type GraphSnapshot struct {
	Text   string
	Size   int64
	Binary bool
}

// ErrGraphSnapshotGone is returned when a version is on record but
// its bytes are not: a project upload the CEO deleted (the version
// list keeps its number so pins stay pins; the store does not keep
// the bytes), or an attachment snapshot lost between a backup and a
// restore. The graph maintainer's scan re-takes a missing snapshot of
// the current version only; older ones cannot be re-taken from
// anything.
var ErrGraphSnapshotGone = errors.New("graph: the bytes of this version are no longer in the store")

// ReadGraphSnapshot returns the content of one version of n. An
// agent's node was snapshotted into the attachment store under the
// version's SHA by the graph maintainer when it recorded it; a project file's
// versions are the CEO's uploads, read through the project-file
// store so a PDF comes back as its extracted text, the same form
// file_view serves for the current upload.
func (s *FSStore) ReadGraphSnapshot(n *graph.Node, v graph.Version) (GraphSnapshot, error) {
	if n.Owner == graph.CEOSlug {
		return s.readProjectFileSnapshot(v.SHA)
	}
	att, err := s.GetAttachment(v.SHA)
	if errors.Is(err, ErrNotFound) {
		return GraphSnapshot{}, ErrGraphSnapshotGone
	}
	if err != nil {
		return GraphSnapshot{}, err
	}
	r, err := s.OpenAttachmentOriginal(v.SHA)
	if err != nil {
		return GraphSnapshot{}, err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, MaxInlinedFileBytes+1))
	if err != nil {
		return GraphSnapshot{}, err
	}
	if !looksLikeText(b) {
		return GraphSnapshot{Size: att.Size, Binary: true}, nil
	}
	if len(b) > MaxInlinedFileBytes {
		return GraphSnapshot{Text: truncate(string(b[:MaxInlinedFileBytes]), MaxInlinedFileBytes), Size: att.Size}, nil
	}
	return GraphSnapshot{Text: string(b), Size: att.Size}, nil
}

func (s *FSStore) readProjectFileSnapshot(sha string) (GraphSnapshot, error) {
	pf, err := s.GetProjectFile(sha)
	if errors.Is(err, ErrNotFound) {
		return GraphSnapshot{}, ErrGraphSnapshotGone
	}
	if err != nil {
		return GraphSnapshot{}, err
	}
	text, err := s.ReadCanonicalText(context.Background(), sha)
	if err != nil {
		return GraphSnapshot{}, err
	}
	if text == "" && pf.CanonicalName == "" && convert.Classify(strings.ToLower(pf.OriginalExt)) == convert.KindText {
		// A text upload the sync farm has not yet backfilled a
		// canonical name for: the original is the text.
		r, err := s.OpenOriginal(sha)
		if err != nil {
			return GraphSnapshot{}, err
		}
		defer func() { _ = r.Close() }()
		if text, err = readCapped(r, MaxInlinedFileBytes); err != nil {
			return GraphSnapshot{}, err
		}
	}
	if text == "" {
		return GraphSnapshot{Size: pf.Size, Binary: true}, nil
	}
	return GraphSnapshot{Text: text, Size: pf.Size}, nil
}

// looksLikeText reports whether b reads as text: no NUL byte and
// valid UTF-8 across its first 8 KiB, allowing for a multi-byte rune
// the sample boundary may have cut.
func looksLikeText(b []byte) bool {
	const sample = 8 << 10
	if len(b) > sample {
		b = b[:sample]
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}
