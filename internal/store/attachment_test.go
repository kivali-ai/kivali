package store

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestAddAttachmentRoundtrip(t *testing.T) {
	s := mustStore(t)
	att, err := s.AddAttachment(context.Background(), "spec.md", strings.NewReader("# hi\n"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if att.SHA == "" {
		t.Error("SHA empty")
	}
	if att.Name != "spec.md" {
		t.Errorf("Name = %q", att.Name)
	}
	if att.Size == 0 {
		t.Error("Size zero")
	}
	if att.CanonicalName == "" {
		t.Error("canonical should be set for text kinds")
	}

	got, err := s.GetAttachment(att.SHA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SHA != att.SHA || got.Name != att.Name {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, att)
	}

	text, err := s.ReadAttachmentText(att.SHA)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if text != "# hi\n" {
		t.Errorf("text = %q", text)
	}

	rc, err := s.OpenAttachmentOriginal(att.SHA)
	if err != nil {
		t.Fatalf("OpenOriginal: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "# hi\n" {
		t.Errorf("original = %q", b)
	}
}

func TestAddAttachmentDedupByContent(t *testing.T) {
	s := mustStore(t)
	a, err := s.AddAttachment(context.Background(), "one.md", strings.NewReader("same"))
	if err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	b, err := s.AddAttachment(context.Background(), "two.md", strings.NewReader("same"))
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	if a.SHA != b.SHA {
		t.Errorf("dedup failed: %q vs %q", a.SHA, b.SHA)
	}
	// Name preserved from first upload (second is a no-op).
	if b.Name != "one.md" {
		t.Errorf("dedup Name = %q, want %q (first upload wins)", b.Name, "one.md")
	}
}

func TestGetAttachmentNotFound(t *testing.T) {
	s := mustStore(t)
	if _, err := s.GetAttachment("deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAddAttachmentBinaryNoCanonical(t *testing.T) {
	s := mustStore(t)
	// Pretend-PNG bytes (not a real image, but the classifier goes by ext).
	att, err := s.AddAttachment(context.Background(), "logo.png", strings.NewReader("\x89PNG\r\n\x1a\n---not-real---"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if att.CanonicalName != "" {
		t.Errorf("images should not get a text canonical: %q", att.CanonicalName)
	}
	// Original still readable.
	rc, _ := s.OpenAttachmentOriginal(att.SHA)
	_ = rc.Close()
}

func TestAddAttachmentZipGetsManifestCanonical(t *testing.T) {
	s := mustStore(t)
	// Build a tiny real zip to upload.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"readme.md": "# hi\n", "data/nums.csv": "1,2,3\n"} {
		fw, _ := zw.Create(name)
		_, _ = fw.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip: %v", err)
	}
	att, err := s.AddAttachment(context.Background(), "bundle.zip", &buf)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if att.CanonicalName != "canonical.txt" {
		t.Fatalf("canonical = %q, want auto-manifest", att.CanonicalName)
	}
	manifest, err := s.ReadAttachmentText(att.SHA)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if !strings.Contains(manifest, "readme.md") || !strings.Contains(manifest, "data/nums.csv") {
		t.Errorf("manifest missing entries: %q", manifest)
	}
}

func TestSetAttachmentCanonicalTextOverrides(t *testing.T) {
	s := mustStore(t)
	att, err := s.AddAttachment(context.Background(), "bin.dat", strings.NewReader("\x00\x01\x02"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if att.CanonicalName != "" {
		t.Fatalf("bin should have no canonical initially: %q", att.CanonicalName)
	}
	if err := s.SetAttachmentCanonicalText(att.SHA, "manual override"); err != nil {
		t.Fatalf("SetCanonical: %v", err)
	}
	text, _ := s.ReadAttachmentText(att.SHA)
	if text != "manual override" {
		t.Errorf("text = %q", text)
	}
}

func TestAddAttachmentFromText(t *testing.T) {
	s := mustStore(t)
	att, err := s.AddAttachmentFromText("notes", "hello world")
	if err != nil {
		t.Fatalf("AddFromText: %v", err)
	}
	// ensureTextExt should append .txt when missing.
	if !strings.HasSuffix(att.Name, ".txt") {
		t.Errorf("Name = %q, expected .txt suffix", att.Name)
	}
	text, _ := s.ReadAttachmentText(att.SHA)
	if text != "hello world" {
		t.Errorf("text = %q", text)
	}
}

// TestAddAttachmentConcurrentSameContent locks in the per-SHA lock
// fix: 16 goroutines racing to add the SAME bytes all converge on a
// SINGLE attachment dir, with consistent meta.json (the first
// caller's name wins; subsequent calls are no-op dedupe). Without
// the lock, concurrent writers raced on blob.<ext> and meta.json:
// the canonical-text computation ran multiple times against the
// same outpath (corrupting it), and meta.json's Name field
// last-writer-won non-deterministically.
func TestAddAttachmentConcurrentSameContent(t *testing.T) {
	s := mustStore(t)
	const N = 16
	results := make(chan Attachment, N)
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := "from-" + intStr(idx) + ".md"
			a, err := s.AddAttachment(context.Background(), name, strings.NewReader("dedupe me"))
			if err != nil {
				errs <- err
				return
			}
			results <- a
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent add: %v", err)
	}

	var firstSHA string
	count := 0
	for a := range results {
		count++
		if firstSHA == "" {
			firstSHA = a.SHA
		} else if a.SHA != firstSHA {
			t.Errorf("got SHA %q, want %q (all writers should converge)", a.SHA, firstSHA)
		}
	}
	if count != N {
		t.Errorf("only %d of %d goroutines returned an attachment", count, N)
	}

	// On disk: exactly one attachment dir, one consistent meta.json.
	att, err := s.GetAttachment(firstSHA)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	// The winning Name must be one of the caller-supplied names —
	// non-empty, deterministic-looking. Not asserting WHICH one (any
	// of N is correct), just that meta isn't corrupted.
	if att.Name == "" || !strings.HasPrefix(att.Name, "from-") {
		t.Errorf("meta.Name = %q, want one of from-N.md", att.Name)
	}
	if att.Size != int64(len("dedupe me")) {
		t.Errorf("meta.Size = %d, want %d", att.Size, len("dedupe me"))
	}
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
