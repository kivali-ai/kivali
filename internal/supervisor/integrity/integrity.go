// Package integrity is a crash-consistency oracle for the VM's data disk.
//
// Run writes, under a directory on the disk, data with the durability
// patterns Kivali's own storage relies on, and reports each write on
// its output as "ACK <n>" only once it is durable (fsync returned):
//
//   - records: a file per write, made by temp file + fsync + rename +
//     directory fsync (the store's writeAtomic);
//   - journal: one append-only file, a framed and checksummed entry per
//     write, each followed by an fsync (chat.jsonl, usage.jsonl);
//   - corpus: files written once and never again, so any change to them
//     is corruption of data at rest;
//
// and, alongside, churn workers that write, read back, check and delete
// scratch files and an occasional fstrim, the mixed I/O and discards
// under which hypervisor disk bugs show. Every so often it drops the
// guest's page cache and verifies everything from the disk.
//
// Verify, given the highest n the host saw acknowledged before it cut
// the VM's power, checks that every acknowledged write is present and
// intact and that nothing else is corrupt: a write that was in flight
// may be absent, or (the journal's last entry) torn, and nothing more.
// What each write holds is a pure function of its number, so Verify
// needs nothing from the run but that number.
//
// vm/boottest -integrity-cycles drives it: boot, verify, run, hard-stop
// at a random moment, repeat (vm/README.md).
package integrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Layout under the run's directory.
const (
	corpusDir   = "corpus"
	corpusDone  = "DONE"
	recordsDir  = "records"
	journalFile = "journal"
	churnDir    = "churn"
	tmpPrefix   = ".tmp-"
)

// Journal entry framing: magic, the write's number, the payload's
// length, the payload, then a CRC-32C of everything before it.
var journalMagic = [4]byte{'K', 'V', 'J', '1'}

const journalHeader = 4 + 8 + 4

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// isRecord says whether write n is a record (else a journal entry).
// Writes are numbered from 1.
func isRecord(n uint64) bool { return n%2 == 1 }

// content is the deterministic bytes of kind/n.
func content(kind string, n uint64, size int) []byte {
	seed := sha256.Sum256([]byte(kind + "/" + strconv.FormatUint(n, 10)))
	b := make([]byte, size)
	_, _ = rand.NewChaCha8(seed).Read(b)
	return b
}

func recordSize(n uint64) int  { return 512 + int(n*7919%(128<<10)) }
func journalSize(n uint64) int { return 32 + int(n*131%4000) }

func recordPath(dir string, n uint64) string {
	return filepath.Join(dir, recordsDir, fmt.Sprintf("%05d", n/1000), fmt.Sprintf("%08d", n))
}

func corpusPath(dir string, k int) string {
	return filepath.Join(dir, corpusDir, fmt.Sprintf("c-%04d", k))
}

// Config is a run.
type Config struct {
	Dir string
	// From is the first write's number: the host's acknowledged count
	// plus one, so a run continues the sequence an earlier one cut off.
	From uint64
	// Writes bounds the run (0: until Stop closes or an error).
	Writes uint64
	// CorpusMiB is the corpus's size, in 1 MiB files; it is written
	// (completed) by the first run and checked from then on.
	CorpusMiB int
	// Workers is the number of churn workers.
	Workers int
	// CheckEvery is how many writes pass between full verifications
	// from the disk (0: none during the run).
	CheckEvery uint64
	// TrimEvery is how many writes pass between Trim calls (0: none).
	TrimEvery uint64
	// Out receives "ACK <n>" lines.
	Out io.Writer
	// DropCaches makes the next reads come from the disk (nil: no-op).
	DropCaches func() error
	// Trim discards the filesystem's free space (nil: no-op).
	Trim func() error
	// Stop ends the run cleanly when closed.
	Stop <-chan struct{}
}

// Run writes until cfg.Writes are done, Stop closes or something fails;
// corruption it sees on the way is an error.
func Run(cfg Config) error {
	if cfg.From == 0 {
		cfg.From = 1
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, recordsDir), filepath.Join(cfg.Dir, churnDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := removeTemps(cfg.Dir); err != nil {
		return err
	}
	if err := writeCorpus(cfg.Dir, cfg.CorpusMiB); err != nil {
		return fmt.Errorf("corpus: %w", err)
	}
	if err := truncateJournal(cfg.Dir, cfg.From-1); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	j, err := os.OpenFile(filepath.Join(cfg.Dir, journalFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()
	if err := syncDir(cfg.Dir); err != nil {
		return err
	}

	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	churnErr := make(chan error, cfg.Workers)
	var wg sync.WaitGroup
	for w := range cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := churn(filepath.Join(cfg.Dir, churnDir), w, stop); err != nil {
				churnErr <- err
				halt()
			}
		}()
	}
	finish := func(err error) error {
		halt()
		wg.Wait()
		select {
		case cerr := <-churnErr:
			if err == nil {
				err = cerr
			}
		default:
		}
		return err
	}

	for n := cfg.From; cfg.Writes == 0 || n < cfg.From+cfg.Writes; n++ {
		select {
		case <-stop:
			return finish(nil)
		case <-cfg.Stop:
			return finish(nil)
		default:
		}
		if isRecord(n) {
			err = writeRecord(cfg.Dir, n)
		} else {
			err = appendJournal(j, n)
		}
		if err != nil {
			return finish(fmt.Errorf("write %d: %w", n, err))
		}
		if _, err := fmt.Fprintf(cfg.Out, "ACK %d\n", n); err != nil {
			return finish(err)
		}
		if cfg.TrimEvery > 0 && n%cfg.TrimEvery == 0 && cfg.Trim != nil {
			if err := cfg.Trim(); err != nil {
				return finish(fmt.Errorf("trim: %w", err))
			}
		}
		if cfg.CheckEvery > 0 && n%cfg.CheckEvery == 0 {
			if cfg.DropCaches != nil {
				if err := cfg.DropCaches(); err != nil {
					return finish(fmt.Errorf("drop caches: %w", err))
				}
			}
			if _, err := Verify(cfg.Dir, n); err != nil {
				return finish(err)
			}
		}
	}
	return finish(nil)
}

// Report is what Verify found.
type Report struct {
	Records, Journal, CorpusFiles int
	// Unacked counts durable writes past the acknowledged count; a torn
	// journal tail is TornBytes long.
	Unacked   int
	TornBytes int64
}

func (r Report) String() string {
	return fmt.Sprintf("%d records, %d journal entries, %d corpus files intact; %d unacknowledged writes landed; %d torn journal bytes",
		r.Records, r.Journal, r.CorpusFiles, r.Unacked, r.TornBytes)
}

// ErrCorrupt wraps every finding of corruption or loss.
var ErrCorrupt = errors.New("integrity violated")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

// Verify checks dir against acked, the highest write number known to
// have been acknowledged (0: none).
func Verify(dir string, acked uint64) (Report, error) {
	var r Report
	var err error
	if r.CorpusFiles, err = verifyCorpus(dir); err != nil {
		return r, err
	}
	if err := verifyRecords(dir, acked, &r); err != nil {
		return r, err
	}
	if err := verifyJournal(dir, acked, &r); err != nil {
		return r, err
	}
	return r, nil
}

func verifyCorpus(dir string) (int, error) {
	_, err := os.Stat(filepath.Join(dir, corpusDir, corpusDone))
	done := err == nil
	ents, err := os.ReadDir(filepath.Join(dir, corpusDir))
	if errors.Is(err, fs.ErrNotExist) {
		if done {
			return 0, corrupt("the corpus directory is gone")
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	want := -1
	if done {
		b, err := os.ReadFile(filepath.Join(dir, corpusDir, corpusDone))
		if err != nil {
			return 0, err
		}
		if want, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
			return 0, corrupt("corpus DONE marker %q", b)
		}
	}
	for _, e := range ents {
		var k int
		if _, err := fmt.Sscanf(e.Name(), "c-%04d", &k); err != nil || e.Name() != fmt.Sprintf("c-%04d", k) {
			continue
		}
		got, err := os.ReadFile(corpusPath(dir, k))
		if err != nil {
			return n, err
		}
		if !bytes.Equal(got, content("corpus", uint64(k), 1<<20)) {
			return n, corrupt("corpus file %s changed at rest (%s)", e.Name(), firstDiff(got, content("corpus", uint64(k), 1<<20)))
		}
		n++
	}
	if done && n != want {
		return n, corrupt("the corpus has %d files intact, its DONE marker says %d", n, want)
	}
	return n, nil
}

func verifyRecords(dir string, acked uint64, r *Report) error {
	seen := map[uint64]bool{}
	err := filepath.WalkDir(filepath.Join(dir, recordsDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == filepath.Join(dir, recordsDir) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		n, err := strconv.ParseUint(d.Name(), 10, 64)
		if err != nil || !isRecord(n) || recordPath(dir, n) != p {
			return corrupt("unexpected file %s among the records", p)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if want := content("record", n, recordSize(n)); !bytes.Equal(got, want) {
			return corrupt("record %d is not what was written (%s)", n, firstDiff(got, want))
		}
		seen[n] = true
		if n > acked {
			r.Unacked++
		}
		return nil
	})
	if err != nil {
		return err
	}
	var missing []uint64
	for n := uint64(1); n <= acked; n++ {
		if isRecord(n) && !seen[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return corrupt("%d acknowledged records are missing (first %d)", len(missing), missing[0])
	}
	for n := range seen {
		if n <= acked {
			r.Records++
		}
	}
	return nil
}

// readJournal parses the journal's valid entries in order: the numbers
// of the journal writes, consecutively. It stops at the first entry that
// is not one; end is where the valid ones end.
func readJournal(b []byte) (ns []uint64, end int64) {
	next := uint64(2)
	off := 0
	for off+journalHeader <= len(b) {
		h := b[off : off+journalHeader]
		if !bytes.Equal(h[:4], journalMagic[:]) {
			break
		}
		n := binary.BigEndian.Uint64(h[4:12])
		size := int(binary.BigEndian.Uint32(h[12:16]))
		if n != next || size != journalSize(n) || off+journalHeader+size+4 > len(b) {
			break
		}
		body := b[off : off+journalHeader+size]
		sum := binary.BigEndian.Uint32(b[off+journalHeader+size:])
		if crc32.Checksum(body, castagnoli) != sum || !bytes.Equal(body[journalHeader:], content("journal", n, size)) {
			break
		}
		ns = append(ns, n)
		off += journalHeader + size + 4
		next += 2
	}
	return ns, int64(off)
}

func verifyJournal(dir string, acked uint64, r *Report) error {
	b, err := os.ReadFile(filepath.Join(dir, journalFile))
	if errors.Is(err, fs.ErrNotExist) {
		b, err = nil, nil
	}
	if err != nil {
		return err
	}
	ns, end := readJournal(b)
	last := uint64(0)
	if len(ns) > 0 {
		last = ns[len(ns)-1]
	}
	// The acknowledged journal writes are 2, 4, ... up to acked.
	wantLast := acked - acked%2
	if last < wantLast {
		why := "it ends"
		if end < int64(len(b)) {
			why = fmt.Sprintf("an invalid entry follows at byte %d", end)
		}
		return corrupt("the journal holds entries up to %d but %d was acknowledged: %s", last, wantLast, why)
	}
	for _, n := range ns {
		if n <= acked {
			r.Journal++
		} else {
			r.Unacked++
		}
	}
	r.TornBytes = int64(len(b)) - end
	return nil
}

// truncateJournal cuts the journal after its last valid entry numbered
// at most upto, so a run from upto+1 appends in sequence: anything after
// it was never acknowledged (Verify has passed before a run).
func truncateJournal(dir string, upto uint64) error {
	p := filepath.Join(dir, journalFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ns, _ := readJournal(b)
	var keep int64
	off := int64(0)
	for _, n := range ns {
		off += int64(journalHeader + journalSize(n) + 4)
		if n <= upto {
			keep = off
		}
	}
	if keep == int64(len(b)) {
		return nil
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(keep); err != nil {
		return err
	}
	return f.Sync()
}

func appendJournal(j *os.File, n uint64) error {
	size := journalSize(n)
	b := make([]byte, journalHeader, journalHeader+size+4)
	copy(b, journalMagic[:])
	binary.BigEndian.PutUint64(b[4:12], n)
	binary.BigEndian.PutUint32(b[12:16], uint32(size))
	b = append(b, content("journal", n, size)...)
	b = binary.BigEndian.AppendUint32(b, crc32.Checksum(b, castagnoli))
	if _, err := j.Write(b); err != nil {
		return err
	}
	return j.Sync()
}

func writeRecord(dir string, n uint64) error {
	return writeAtomic(recordPath(dir, n), content("record", n, recordSize(n)))
}

// writeAtomic is the store's writeAtomic: temp file, fsync, rename,
// directory fsync.
func writeAtomic(p string, b []byte) error {
	d := filepath.Dir(p)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(d, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	if err := syncDir(d); err != nil {
		return err
	}
	// A new records/<bucket> directory's own entry, in its parent.
	return syncDir(filepath.Dir(d))
}

func writeCorpus(dir string, mib int) error {
	if mib <= 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, corpusDir, corpusDone)); err == nil {
		return nil
	}
	for k := range mib {
		p := corpusPath(dir, k)
		if _, err := os.Stat(p); err == nil {
			continue // complete: written by rename
		}
		if err := writeAtomic(p, content("corpus", uint64(k), 1<<20)); err != nil {
			return err
		}
	}
	return writeAtomic(filepath.Join(dir, corpusDir, corpusDone), []byte(strconv.Itoa(mib)+"\n"))
}

func removeTemps(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), tmpPrefix) {
			return os.Remove(p)
		}
		return nil
	})
}

// churn writes scratch files of 64 KiB to 4 MiB, reads each back and
// compares it, then deletes it, until stop closes. A mismatch is
// corruption the disk returned in the moment, without any crash.
func churn(dir string, worker int, stop <-chan struct{}) error {
	rng := rand.New(rand.NewPCG(uint64(worker), uint64(time.Now().UnixNano())))
	var live []string
	for i := uint64(0); ; i++ {
		select {
		case <-stop:
			return nil
		default:
		}
		size := 64<<10 + rng.IntN(4<<20)
		key := fmt.Sprintf("w%d-%d-%d", worker, rng.Uint64(), i)
		want := content("churn/"+key, i, size)
		p := filepath.Join(dir, key)
		if err := os.WriteFile(p, want, 0o644); err != nil {
			return err
		}
		got, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return corrupt("churn file %s read back wrong (%s)", key, firstDiff(got, want))
		}
		live = append(live, p)
		if len(live) > 8 {
			if err := os.Remove(live[0]); err != nil {
				return err
			}
			live = live[1:]
		}
	}
}

// syncDir fsyncs the directory dir, making the entries renamed or
// created in it durable. Windows cannot open a directory for a sync,
// and NTFS journals the rename itself; there it is a no-op.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// firstDiff describes where got first differs from want.
func firstDiff(got, want []byte) string {
	if len(got) != len(want) {
		return fmt.Sprintf("%d bytes, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			zero := bytes.Count(got[i:min(i+4096, len(got))], []byte{0})
			return fmt.Sprintf("first differs at byte %d; %d of the next %d bytes are zero", i, zero, min(4096, len(got)-i))
		}
	}
	return "equal"
}
