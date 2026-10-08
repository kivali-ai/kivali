package integrity

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// run writes n writes from `from` into dir and returns the last ACK.
func run(t *testing.T, dir string, from, n uint64) uint64 {
	t.Helper()
	var out bytes.Buffer
	if err := Run(Config{Dir: dir, From: from, Writes: n, CorpusMiB: 2, Out: &out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return lastAck(t, out.String())
}

func lastAck(t *testing.T, out string) uint64 {
	t.Helper()
	var last uint64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "ACK ")
		if !ok {
			t.Fatalf("unexpected output line %q", sc.Text())
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		last = n
	}
	return last
}

func mustVerify(t *testing.T, dir string, acked uint64) Report {
	t.Helper()
	r, err := Verify(dir, acked)
	if err != nil {
		t.Fatalf("verify (acked %d): %v", acked, err)
	}
	return r
}

func mustCorrupt(t *testing.T, dir string, acked uint64, want string) {
	t.Helper()
	_, err := Verify(dir, acked)
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), want) {
		t.Fatalf("verify: %v, want corruption mentioning %q", err, want)
	}
}

func TestRunThenVerify(t *testing.T) {
	dir := t.TempDir()
	acked := run(t, dir, 1, 200)
	if acked != 200 {
		t.Fatalf("last ACK %d", acked)
	}
	r := mustVerify(t, dir, acked)
	if r.Records != 100 || r.Journal != 100 || r.CorpusFiles != 2 || r.Unacked != 0 || r.TornBytes != 0 {
		t.Fatalf("report %+v", r)
	}
	// The host may have seen fewer ACKs than were made durable.
	if r := mustVerify(t, dir, 150); r.Unacked != 50 {
		t.Fatalf("report at acked 150: %+v", r)
	}
}

// A journal entry cut short by the power cut is the in-flight write:
// allowed, and the next run removes it and continues the sequence.
func TestTornJournalTailIsAllowedAndResumed(t *testing.T) {
	dir := t.TempDir()
	acked := run(t, dir, 1, 100)
	f, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(append(journalMagic[:], 0, 0, 0)) // a header cut off
	_ = f.Close()
	if r := mustVerify(t, dir, acked); r.TornBytes != 7 {
		t.Fatalf("report %+v", r)
	}
	acked = run(t, dir, acked+1, 50)
	if r := mustVerify(t, dir, acked); r.TornBytes != 0 || r.Journal != 75 || r.Records != 75 {
		t.Fatalf("after resuming: %+v", r)
	}
}

// Durable writes past the last ACK the host saw are rewritten in place
// by the next run, which starts at that ACK + 1.
func TestResumeBelowTheDurableHighWater(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, 1, 100)
	acked := run(t, dir, 61, 80) // the host had seen only 60
	mustVerify(t, dir, acked)
}

func TestLostAcknowledgedRecord(t *testing.T) {
	dir := t.TempDir()
	acked := run(t, dir, 1, 50)
	if err := os.Remove(recordPath(dir, 33)); err != nil {
		t.Fatal(err)
	}
	mustCorrupt(t, dir, acked, "acknowledged records are missing (first 33)")
	// Not acknowledged: its absence is allowed.
	mustVerify(t, dir, 32)
}

func TestChangedRecord(t *testing.T) {
	for name, damage := range map[string]func([]byte) []byte{
		"bit flip": func(b []byte) []byte { b[100] ^= 1; return b },
		"zeroed":   func(b []byte) []byte { return make([]byte, len(b)) },
		"empty":    func([]byte) []byte { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			acked := run(t, dir, 1, 50)
			p := recordPath(dir, 21)
			b, _ := os.ReadFile(p)
			if err := os.WriteFile(p, damage(b), 0o644); err != nil {
				t.Fatal(err)
			}
			mustCorrupt(t, dir, acked, "record 21 is not what was written")
			// Even an unacknowledged record that exists must be whole.
			mustCorrupt(t, dir, 10, "record 21 is not what was written")
		})
	}
}

func TestCorruptJournalEntry(t *testing.T) {
	dir := t.TempDir()
	acked := run(t, dir, 1, 100)
	p := filepath.Join(dir, journalFile)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0x40
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	mustCorrupt(t, dir, acked, "the journal holds entries up to")
}

func TestCorpusChangedAtRest(t *testing.T) {
	dir := t.TempDir()
	acked := run(t, dir, 1, 10)
	p := corpusPath(dir, 1)
	b, _ := os.ReadFile(p)
	b[1<<19] ^= 0x80
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	mustCorrupt(t, dir, acked, "corpus file c-0001 changed at rest")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	mustCorrupt(t, dir, acked, "its DONE marker says 2")
}

// A run checks the disk itself as it goes (after dropping caches) and
// runs its churn workers and trims alongside.
func TestRunChecksAsItGoes(t *testing.T) {
	dir := t.TempDir()
	var drops, trims int
	var out bytes.Buffer
	err := Run(Config{
		Dir: dir, From: 1, Writes: 120, CorpusMiB: 1, Workers: 2, CheckEvery: 40, TrimEvery: 30, Out: &out,
		DropCaches: func() error { drops++; return nil },
		Trim:       func() error { trims++; return nil },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if drops != 3 || trims != 4 {
		t.Fatalf("%d cache drops and %d trims, want 3 and 4", drops, trims)
	}
	mustVerify(t, dir, lastAck(t, out.String()))
	ents, _ := os.ReadDir(filepath.Join(dir, churnDir))
	if len(ents) > 2*9 {
		t.Fatalf("churn left %d files", len(ents))
	}
}

// A run that meets corruption mid-way stops with it.
func TestRunStopsOnCorruption(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, 1, 20)
	b, _ := os.ReadFile(recordPath(dir, 5))
	b[0] ^= 1
	if err := os.WriteFile(recordPath(dir, 5), b, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run(Config{Dir: dir, From: 21, Writes: 100, CheckEvery: 10})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("run: %v, want corruption", err)
	}
}
