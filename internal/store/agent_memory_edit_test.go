package store

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func newStoreForMemoryTest(t *testing.T) *FSStore {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "r"}, "k"); err != nil {
		t.Fatalf("create: %v", err)
	}
	return s
}

func TestAppendAgentMemoryOntoEmpty(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.AppendAgentMemory("alice", "first learning: X"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.ReadAgentMemory("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(got) != "first learning: X" {
		t.Errorf("got %q", got)
	}
}

func TestAppendAgentMemoryBlankLineSeparator(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.WriteAgentMemory("alice", "pre-existing line\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.AppendAgentMemory("alice", "new entry"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "pre-existing line\n\nnew entry") {
		t.Errorf("blank-line separator missing: %q", got)
	}
}

func TestStrReplaceAgentMemoryExactlyOnce(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.WriteAgentMemory("alice", "keep this; replace THIS_TOKEN now."); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.StrReplaceAgentMemory("alice", "THIS_TOKEN", "REPLACED"); err != nil {
		t.Fatalf("str_replace: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "replace REPLACED now") {
		t.Errorf("replacement missing: %q", got)
	}
}

func TestStrReplaceAgentMemoryNotFound(t *testing.T) {
	s := newStoreForMemoryTest(t)
	_ = s.WriteAgentMemory("alice", "body")
	err := s.StrReplaceAgentMemory("alice", "nonexistent", "x")
	if !errors.Is(err, ErrAgentMemoryStrNotFound) {
		t.Errorf("got %v, want ErrAgentMemoryStrNotFound", err)
	}
}

func TestStrReplaceAgentMemoryMultiple(t *testing.T) {
	s := newStoreForMemoryTest(t)
	_ = s.WriteAgentMemory("alice", "AAA then AAA again")
	err := s.StrReplaceAgentMemory("alice", "AAA", "X")
	if !errors.Is(err, ErrAgentMemoryStrMultiple) {
		t.Errorf("got %v, want ErrAgentMemoryStrMultiple", err)
	}
}

func TestStrReplaceAgentMemoryEmptyOldStr(t *testing.T) {
	s := newStoreForMemoryTest(t)
	_ = s.WriteAgentMemory("alice", "body")
	if err := s.StrReplaceAgentMemory("alice", "", "x"); err == nil {
		t.Error("expected error on empty old_str")
	}
}

func TestStrReplaceAgentMemoryDeleteRange(t *testing.T) {
	s := newStoreForMemoryTest(t)
	_ = s.WriteAgentMemory("alice", "keep-this\nDELETEME\nalso-keep")
	if err := s.StrReplaceAgentMemory("alice", "DELETEME\n", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if strings.Contains(got, "DELETEME") {
		t.Errorf("still contains: %q", got)
	}
	if !strings.Contains(got, "keep-this") || !strings.Contains(got, "also-keep") {
		t.Errorf("lost surrounding context: %q", got)
	}
}

// --- UTF-8 / Unicode normalization coverage ---
//
// These tests guard the NFC + confusables-fold behavior in
// AppendAgentMemory and StrReplaceAgentMemory. Two distinct failure
// modes are covered:
//
//   - Tier 1 (NFC): same character, different bytes (NFD vs NFC). The
//     replacement should succeed transparently.
//   - Tier 2 (fold-only): genuinely different characters that look
//     similar (em dash vs hyphen, curly vs straight quotes, NBSP vs
//     space). The replacement must NOT silently swap characters; we
//     return ErrAgentMemoryStrFoldOnly so the dispatcher can nudge the
//     agent to re-read and copy bytes verbatim.

// e_acute_NFD is the decomposed form of "é": ASCII 'e' followed by
// U+0301 (combining acute accent). Visually identical to NFC U+00E9
// but byte-different.
const e_acute_NFD = "é"

// e_acute_NFC is the precomposed form (U+00E9).
const e_acute_NFC = "é"

func TestStrReplaceAgentMemoryNFCMatchesAcrossForms(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// File written in NFD (the decomposed form some macOS paste flows
	// produce); agent's old_str typed in the more common NFC form.
	if err := s.WriteAgentMemory("alice", "caf"+e_acute_NFD+" au lait\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.StrReplaceAgentMemory("alice", "caf"+e_acute_NFC+" au lait", "espresso"); err != nil {
		t.Fatalf("nfd-vs-nfc replace: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "espresso") {
		t.Errorf("replacement missing: %q", got)
	}
	if !norm.NFC.IsNormalString(got) {
		t.Errorf("file content should be NFC after str_replace, got non-normalized: %q", got)
	}
}

func TestStrReplaceAgentMemoryNFCNormalizesOldStr(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// Reverse direction: file in NFC, agent supplies NFD search string.
	if err := s.WriteAgentMemory("alice", "naïve approach is fine\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// NFD form: 'i' + U+0308 (combining diaeresis).
	oldNFD := "na" + "ï" + "ve"
	if err := s.StrReplaceAgentMemory("alice", oldNFD, "smart"); err != nil {
		t.Fatalf("nfc-vs-nfd replace: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "smart approach") {
		t.Errorf("replacement missing: %q", got)
	}
}

func TestStrReplaceAgentMemoryEmDashFoldOnly(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// File contains an em dash; agent retypes with double hyphen.
	if err := s.WriteAgentMemory("alice", "alpha — beta — keep this only once: zeta"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.StrReplaceAgentMemory("alice", "keep this only once: zeta -- gone", "x")
	// First, the search target literally isn't in the file (whether
	// folded or not), so this should be a plain not-found.
	if !errors.Is(err, ErrAgentMemoryStrNotFound) {
		t.Errorf("synthetic non-match: got %v, want ErrAgentMemoryStrNotFound", err)
	}
	// Now the real fold-only case: agent typed `-` where the file has `—`.
	// (One em dash folds to one hyphen; if the agent had typed `--` it
	// would not even fold-match, which is correct behavior.)
	err = s.StrReplaceAgentMemory("alice", "alpha - beta", "X")
	if !errors.Is(err, ErrAgentMemoryStrFoldOnly) {
		t.Errorf("em-dash fold-only: got %v, want ErrAgentMemoryStrFoldOnly", err)
	}
	// Crucially: the file must NOT have been silently rewritten.
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "alpha — beta") {
		t.Errorf("fold-only error must not mutate file: %q", got)
	}
}

func TestStrReplaceAgentMemoryCurlyQuotesFoldOnly(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// File has curly quotes (e.g. as smart-quote-converted by some
	// editor); agent retypes with straight quotes.
	if err := s.WriteAgentMemory("alice", "the “release” gate is closed\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.StrReplaceAgentMemory("alice", `the "release" gate`, "X")
	if !errors.Is(err, ErrAgentMemoryStrFoldOnly) {
		t.Errorf("curly-quote fold-only: got %v, want ErrAgentMemoryStrFoldOnly", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "“release”") {
		t.Errorf("file mutated despite fold-only error: %q", got)
	}
}

func TestStrReplaceAgentMemoryNBSPFoldOnly(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// File has a no-break space (U+00A0) where agent expects a regular space.
	if err := s.WriteAgentMemory("alice", "deadline 2026-06-01\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := s.StrReplaceAgentMemory("alice", "deadline 2026-06-01", "deadline TBD")
	if !errors.Is(err, ErrAgentMemoryStrFoldOnly) {
		t.Errorf("nbsp fold-only: got %v, want ErrAgentMemoryStrFoldOnly", err)
	}
}

func TestStrReplaceAgentMemoryEmDashExactMatchSucceeds(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// Same em-dash content, but the agent supplies the actual em dash
	// in old_str (the byte-exact path). This must succeed — no fold,
	// no error.
	if err := s.WriteAgentMemory("alice", "alpha — beta\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.StrReplaceAgentMemory("alice", "alpha — beta", "alpha to beta"); err != nil {
		t.Fatalf("byte-exact em dash should match: %v", err)
	}
	got, _ := s.ReadAgentMemory("alice")
	if !strings.Contains(got, "alpha to beta") {
		t.Errorf("replacement missing: %q", got)
	}
}

func TestAppendAgentMemoryNFCNormalizes(t *testing.T) {
	s := newStoreForMemoryTest(t)
	// Append NFD-decomposed text; the file should be stored as NFC so
	// later str_replace calls match cleanly.
	if err := s.AppendAgentMemory("alice", "caf"+e_acute_NFD+" notes"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.ReadAgentMemory("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !norm.NFC.IsNormalString(got) {
		t.Errorf("appended content should be NFC, got non-normalized: %q", got)
	}
	if !strings.Contains(got, "caf"+e_acute_NFC+" notes") {
		t.Errorf("expected NFC é in stored content, got: %q", got)
	}
}
