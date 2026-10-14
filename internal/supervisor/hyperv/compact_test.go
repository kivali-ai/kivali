//go:build windows

package hyperv

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// discardedDisk writes a VHDX whose BAT holds n discarded 32 MiB blocks
// that still have their place in the file, after 4 present ones.
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

// writeVHDX writes v's bytes to data.vhdx in a test directory.
func writeVHDX(t *testing.T, v vhdx.Spec) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.vhdx")
	if err := os.WriteFile(p, v.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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

// Under the threshold the disk is left alone: no request, no line.
func TestCompactDataDiskBelowThreshold(t *testing.T) {
	fb := &fakeBroker{}
	b := Backend{Client: serveBroker(t, fb)}
	var log logLines
	began := false
	if err := b.CompactDataDisk(context.Background(), discardedDisk(t, 31), func() { began = true }, log.logf); err != nil {
		t.Fatal(err)
	}
	if began || len(fb.optimizes) != 0 || len(log.get()) != 0 {
		t.Fatalf("began=%v requests=%v log=%q", began, fb.optimizes, log.get())
	}
}

// A compaction that takes long says so every compactHeartbeat, and the
// lines stop with it.
func TestCompactDataDiskHeartbeats(t *testing.T) {
	fb := &fakeBroker{optimizeEntered: make(chan struct{}), optimizeWait: make(chan struct{}),
		optimizeResult: broker.OptimizeResult{SizeBefore: 8627683328, SizeAfter: 6580862976}}
	clk := clock.NewFake()
	b := Backend{Client: serveBroker(t, fb), Clock: clk}
	// Every line is taken by the test before the clock moves on, so no
	// tick is dropped on a receiver that has not caught up.
	lines := make(chan string)
	logf := func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }
	done := make(chan error, 1)
	go func() { done <- b.CompactDataDisk(context.Background(), discardedDisk(t, 40), func() {}, logf) }()
	if l := <-lines; !strings.HasPrefix(l, "the data disk's file holds 1.2 GiB") {
		t.Fatalf("first line = %q", l)
	}
	<-fb.optimizeEntered
	clk.BlockUntil(1)
	for i := range 3 {
		clk.Advance(compactHeartbeat)
		want := fmt.Sprintf("waiting for the data disk's compaction to finish (%ds so far)", 10*(i+1))
		if l := <-lines; l != want {
			t.Fatalf("heartbeat %d = %q, want %q", i+1, l, want)
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

// At the threshold the broker is asked, with the disk's path, and the
// sizes it reports are logged.
func TestCompactDataDiskAsksTheBroker(t *testing.T) {
	fb := &fakeBroker{optimizeResult: broker.OptimizeResult{SizeBefore: 8627683328, SizeAfter: 6580862976}}
	b := Backend{Client: serveBroker(t, fb)}
	p := discardedDisk(t, 32)
	var log logLines
	began := false
	if err := b.CompactDataDisk(context.Background(), p, func() { began = true }, log.logf); err != nil {
		t.Fatal(err)
	}
	if !began || len(fb.optimizes) != 1 || fb.optimizes[0].Path != p {
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
	b := Backend{Client: serveBroker(t, fb)}
	var log logLines
	err := b.CompactDataDisk(context.Background(), discardedDisk(t, 40), func() {}, log.logf)
	if err == nil || !strings.Contains(err.Error(), "compacted only while it is off") {
		t.Fatalf("err = %v", err)
	}
}

// A file that is not a VHDX is an error before the broker is asked.
func TestCompactDataDiskUnreadable(t *testing.T) {
	fb := &fakeBroker{}
	b := Backend{Client: serveBroker(t, fb)}
	v := vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 30, Flags: vhdx.FlagHasParent}
	err := b.CompactDataDisk(context.Background(), writeVHDX(t, v), func() {}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "differencing") || len(fb.optimizes) != 0 {
		t.Fatalf("err = %v, requests = %v", err, fb.optimizes)
	}
}
