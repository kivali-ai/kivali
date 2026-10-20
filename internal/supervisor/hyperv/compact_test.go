//go:build windows

package hyperv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// testConfigDir is a team's config directory; its key is 96e3ef74.
const testConfigDir = `C:\Users\Maya\AppData\Local\Kivali`

// discardedDisk writes, as the team's data disk under a test VM root, a
// VHDX whose BAT holds n discarded 32 MiB blocks that still have their
// place in the file, after 4 present ones, and returns the VM root.
func discardedDisk(t *testing.T, n int) string {
	t.Helper()
	v := vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 30}
	for i := range 4 + n {
		state := uint64(vhdx.BlockFullyPresent)
		if i >= 4 {
			state = vhdx.BlockUnmapped
		}
		v.Entries = append(v.Entries, [2]uint64{state, uint64(4 + i*32)})
	}
	return writeVHDX(t, v)
}

// writeVHDX writes v's bytes as the team's data disk under a test VM
// root, and returns the root.
func writeVHDX(t *testing.T, v vhdx.Spec) string {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, host.DirKey(testConfigDir), DataDiskFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, v.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func noProgress(int64, int64) {}

// Under the threshold the disk is left alone: no request, no line.
func TestCompactDataDiskBelowThreshold(t *testing.T) {
	fb := &fakeBroker{}
	b := Backend{Client: serveBroker(t, fb), VMRoot: discardedDisk(t, 31)}
	var log logLines
	began := false
	if err := b.CompactDataDisk(context.Background(), testConfigDir, func() { began = true }, noProgress, log.logf); err != nil {
		t.Fatal(err)
	}
	if began || len(fb.optimizes) != 0 || len(log.get()) != 0 {
		t.Fatalf("began=%v requests=%v log=%q", began, fb.optimizes, log.get())
	}
}

// A compaction asks the broker every progressPoll on the clock how far
// it has got, and reports that; every compactHeartbeat it says it is
// still running; both stop with it.
func TestCompactDataDiskProgressAndHeartbeats(t *testing.T) {
	fb := &fakeBroker{optimizeEntered: make(chan struct{}), optimizeWait: make(chan struct{}),
		optimizeResult: broker.OptimizeResult{SizeBefore: 8627683328, SizeAfter: 6580862976}}
	polls := 0
	fb.progress = func(dr broker.DiskRequest) broker.VHDProgress {
		if dr.ConfigDir != testConfigDir {
			t.Errorf("progress asked for %q", dr.ConfigDir)
		}
		polls++
		return broker.VHDProgress{Op: broker.OpCompact, Done: int64(polls), Total: 100}
	}
	clk := clock.NewFake()
	b := Backend{Client: serveBroker(t, fb), Clock: clk, VMRoot: discardedDisk(t, 40)}
	// Every line and report is taken by the test before the clock moves
	// on, so no tick is dropped on a receiver that has not caught up.
	lines := make(chan string)
	logf := func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }
	reports := make(chan [2]int64)
	progress := func(done, total int64) { reports <- [2]int64{done, total} }
	done := make(chan error, 1)
	go func() { done <- b.CompactDataDisk(context.Background(), testConfigDir, func() {}, progress, logf) }()
	if l := <-lines; !strings.HasPrefix(l, "the data disk's file holds 1.2 GiB") {
		t.Fatalf("first line = %q", l)
	}
	<-fb.optimizeEntered
	clk.BlockUntil(1)
	for i := 1; i <= 30; i++ {
		clk.Advance(progressPoll)
		if r := <-reports; r != [2]int64{int64(i), 100} {
			t.Fatalf("report %d = %v", i, r)
		}
		if i%10 == 0 {
			want := fmt.Sprintf("waiting for the data disk's compaction to finish (%ds so far)", i)
			if l := <-lines; l != want {
				t.Fatalf("heartbeat at %ds = %q, want %q", i, l, want)
			}
		}
	}
	close(fb.optimizeWait)
	if l := <-lines; !strings.Contains(l, "8.0 GiB to 6.1 GiB in 30s") {
		t.Fatalf("last line = %q", l)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if clk.Waiters() != 0 {
		t.Fatalf("%d timers still armed after the compaction", clk.Waiters())
	}
}

// Progress of another operation, or none, is not reported.
func TestCompactDataDiskIgnoresOtherProgress(t *testing.T) {
	fb := &fakeBroker{optimizeEntered: make(chan struct{}), optimizeWait: make(chan struct{})}
	answered := make(chan struct{})
	answers := []broker.VHDProgress{{}, {Op: broker.OpImport, Done: 1, Total: 2}}
	fb.progress = func(broker.DiskRequest) broker.VHDProgress {
		defer func() { answered <- struct{}{} }()
		a := answers[0]
		answers = answers[1:]
		return a
	}
	clk := clock.NewFake()
	b := Backend{Client: serveBroker(t, fb), Clock: clk, VMRoot: discardedDisk(t, 40)}
	reported := false
	done := make(chan error, 1)
	go func() {
		done <- b.CompactDataDisk(context.Background(), testConfigDir, func() {}, func(int64, int64) { reported = true }, func(string, ...any) {})
	}()
	<-fb.optimizeEntered
	clk.BlockUntil(1)
	for range 2 {
		clk.Advance(progressPoll)
		<-answered
	}
	close(fb.optimizeWait)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reported {
		t.Fatal("progress of no operation, or another, was reported")
	}
}

// At the threshold the broker is asked, with the config directory, and
// the sizes it reports are logged.
func TestCompactDataDiskAsksTheBroker(t *testing.T) {
	fb := &fakeBroker{optimizeResult: broker.OptimizeResult{SizeBefore: 8627683328, SizeAfter: 6580862976}}
	b := Backend{Client: serveBroker(t, fb), VMRoot: discardedDisk(t, 32)}
	var log logLines
	began := false
	if err := b.CompactDataDisk(context.Background(), testConfigDir, func() { began = true }, noProgress, log.logf); err != nil {
		t.Fatal(err)
	}
	if !began || len(fb.optimizes) != 1 || fb.optimizes[0].ConfigDir != testConfigDir {
		t.Fatalf("began=%v requests=%v", began, fb.optimizes)
	}
	joined := strings.Join(log.get(), "\n")
	if !strings.Contains(joined, "1.0 GiB of blocks the guest discarded") || !strings.Contains(joined, "8.0 GiB to 6.1 GiB") {
		t.Fatalf("log = %q", log.get())
	}
}

// The broker's refusal is the error, with its sentence.
func TestCompactDataDiskBrokerRefusal(t *testing.T) {
	fb := &fakeBroker{optimizeStatus: http.StatusConflict}
	b := Backend{Client: serveBroker(t, fb), VMRoot: discardedDisk(t, 40)}
	var log logLines
	err := b.CompactDataDisk(context.Background(), testConfigDir, func() {}, noProgress, log.logf)
	if err == nil || !strings.Contains(err.Error(), "compacted only while it is off") {
		t.Fatalf("err = %v", err)
	}
}

// A file that is not a dynamic VHDX is an error before the broker is
// asked.
func TestCompactDataDiskUnreadable(t *testing.T) {
	fb := &fakeBroker{}
	v := vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 30, Flags: vhdx.FlagHasParent}
	b := Backend{Client: serveBroker(t, fb), VMRoot: writeVHDX(t, v)}
	err := b.CompactDataDisk(context.Background(), testConfigDir, func() {}, noProgress, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "differencing") || len(fb.optimizes) != 0 {
		t.Fatalf("err = %v, requests = %v", err, fb.optimizes)
	}
}

// An import asks the broker with the config directory, reports the
// bytes copied as the broker counts them on the clock, and logs what it
// copied.
func TestImportDataDisk(t *testing.T) {
	fb := &fakeBroker{importEntered: make(chan struct{}), importWait: make(chan struct{}),
		importResult: broker.ImportResult{CopiedBytes: 8 << 30}}
	copied := int64(0)
	fb.progress = func(broker.DiskRequest) broker.VHDProgress {
		copied += 1 << 30
		return broker.VHDProgress{Op: broker.OpImport, Done: copied, Total: 8 << 30}
	}
	clk := clock.NewFake()
	b := Backend{Client: serveBroker(t, fb), Clock: clk}
	reports := make(chan [2]int64)
	var log logLines
	done := make(chan error, 1)
	go func() {
		done <- b.ImportDataDisk(context.Background(), `\\?\`+testConfigDir, func(d, t int64) { reports <- [2]int64{d, t} }, log.logf)
	}()
	<-fb.importEntered
	clk.BlockUntil(1)
	for i := int64(1); i <= 3; i++ {
		clk.Advance(progressPoll)
		if r := <-reports; r != [2]int64{i << 30, 8 << 30} {
			t.Fatalf("report %d = %v", i, r)
		}
	}
	close(fb.importWait)
	if err := <-done; err != nil {
		t.Fatalf("ImportDataDisk: %v", err)
	}
	if len(fb.imports) != 1 || fb.imports[0].ConfigDir != testConfigDir {
		t.Fatalf("imports = %+v", fb.imports)
	}
	if l := log.get(); len(l) != 1 || !strings.Contains(l[0], "8.0 GiB in 3s") {
		t.Fatalf("log = %q", l)
	}
}

// The upgrade's snapshot reports the bytes the broker has copied, on the
// clock, and logs what it copied. Without it, the desktop app's bar sat
// still through a copy of all of data.vhdx, minutes on a team in use.
func TestSnapshotDataDiskReportsProgress(t *testing.T) {
	fb := &fakeBroker{snapshotEntered: make(chan struct{}), snapshotWait: make(chan struct{}),
		snapshotResult: broker.SnapshotResult{CopiedBytes: 8 << 30}}
	copied := int64(0)
	fb.progress = func(broker.DiskRequest) broker.VHDProgress {
		copied += 1 << 30
		return broker.VHDProgress{Op: broker.OpSnapshot, Done: copied, Total: 8 << 30}
	}
	clk := clock.NewFake()
	b := Backend{Client: serveBroker(t, fb), Clock: clk}
	reports := make(chan [2]int64)
	var log logLines
	done := make(chan error, 1)
	go func() {
		done <- b.SnapshotDataDisk(context.Background(), `\\?\`+testConfigDir, func(d, t int64) { reports <- [2]int64{d, t} }, log.logf)
	}()
	<-fb.snapshotEntered
	clk.BlockUntil(1)
	for i := int64(1); i <= 3; i++ {
		clk.Advance(progressPoll)
		if r := <-reports; r != [2]int64{i << 30, 8 << 30} {
			t.Fatalf("report %d = %v", i, r)
		}
	}
	close(fb.snapshotWait)
	if err := <-done; err != nil {
		t.Fatalf("SnapshotDataDisk: %v", err)
	}
	fb.mu.Lock()
	calls := fb.diskCalls
	fb.mu.Unlock()
	if len(calls) != 1 || calls[0] != "POST /v1/vhd/snapshot "+testConfigDir {
		t.Fatalf("broker calls = %q", calls)
	}
	if l := log.get(); len(l) != 1 || !strings.Contains(l[0], "8.0 GiB in 3s") {
		t.Fatalf("log = %q", l)
	}
}

// The broker's already-imported answer is supervisor.ErrAlreadyImported;
// any other refusal is the error, with its sentence.
func TestImportDataDiskAnswers(t *testing.T) {
	fb := &fakeBroker{importStatus: http.StatusConflict, importMsg: broker.AlreadyImported + `: C:\ProgramData\Kivali\broker\vm\96e3ef74 holds a data disk already`}
	b := Backend{Client: serveBroker(t, fb)}
	if err := b.ImportDataDisk(context.Background(), testConfigDir, noProgress, func(string, ...any) {}); !errors.Is(err, supervisor.ErrAlreadyImported) {
		t.Fatalf("err = %v, want ErrAlreadyImported", err)
	}
	fb.mu.Lock()
	fb.importStatus, fb.importMsg = http.StatusForbidden, "refused: the data disk to import: not a dynamic VHDX: a differencing disk"
	fb.mu.Unlock()
	err := b.ImportDataDisk(context.Background(), testConfigDir, noProgress, func(string, ...any) {})
	if err == nil || errors.Is(err, supervisor.ErrAlreadyImported) || !strings.Contains(err.Error(), "differencing") {
		t.Fatalf("err = %v", err)
	}
}
