//go:build windows

package hyperv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

var pipeSeq atomic.Int64

func uniquePipe(kind string) string {
	return fmt.Sprintf(`\\.\pipe\kivali-test-%s-%d-%d`, kind, os.Getpid(), pipeSeq.Add(1))
}

// fakeBroker answers the broker's HTTP interface over a named pipe, so
// the client's production path (HTTP over a pipe) is exercised for real.
type fakeBroker struct {
	mu        sync.Mutex
	id        string // the VM GUID returned (empty to leave the VM idless)
	state     string
	ensureReq broker.VMRequest
	vhds      []broker.VHDRequest
	stops     []bool
	gets      int      // GET /v1/vm/{name} requests: the state polls
	removes   []string // DELETE /v1/vm/{name} names
	// removeStatus, when set, is DELETE's answer instead of 200.
	removeStatus int
	// gracefulIgnored makes a graceful stop answer without the VM
	// stopping (a guest that will not shut down).
	gracefulIgnored bool
	// missing makes GET /v1/vm/{name} answer 404: no VM yet.
	missing bool
}

func (f *fakeBroker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vm", func(w http.ResponseWriter, r *http.Request) {
		var req broker.VMRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.ensureReq = req
		if f.state == "" {
			f.state = broker.StateOff
		}
		st := broker.VMState{ID: f.id, State: f.state}
		f.mu.Unlock()
		writeJSON(w, st)
	})
	mux.HandleFunc("GET /v1/vm/{name}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.gets++
		st := broker.VMState{ID: f.id, State: f.state}
		missing := f.missing
		f.mu.Unlock()
		if missing {
			http.Error(w, "there is no VM", http.StatusNotFound)
			return
		}
		writeJSON(w, st)
	})
	mux.HandleFunc("DELETE /v1/vm/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.removes = append(f.removes, r.PathValue("name"))
		status := f.removeStatus
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		writeJSON(w, struct{}{})
	})
	mux.HandleFunc("POST /v1/vm/{name}/start", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.state = broker.StateRunning
		st := broker.VMState{ID: f.id, State: f.state}
		f.mu.Unlock()
		writeJSON(w, st)
	})
	mux.HandleFunc("POST /v1/vm/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		var sr broker.StopRequest
		_ = json.NewDecoder(r.Body).Decode(&sr)
		f.mu.Lock()
		f.stops = append(f.stops, sr.Graceful)
		if !sr.Graceful || !f.gracefulIgnored {
			f.state = broker.StateOff
		}
		st := broker.VMState{ID: f.id, State: f.state}
		f.mu.Unlock()
		writeJSON(w, st)
	})
	mux.HandleFunc("POST /v1/vhd", func(w http.ResponseWriter, r *http.Request) {
		var vr broker.VHDRequest
		_ = json.NewDecoder(r.Body).Decode(&vr)
		f.mu.Lock()
		f.vhds = append(f.vhds, vr)
		f.mu.Unlock()
		writeJSON(w, broker.VHDResult{Path: vr.Path})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveBroker runs f on a fresh pipe and returns a client dialling it.
func serveBroker(t *testing.T, f *fakeBroker) *broker.Client {
	t.Helper()
	name := uniquePipe("broker")
	ln, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatalf("listen broker pipe: %v", err)
	}
	srv := &http.Server{Handler: f.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return &broker.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, name)
	}}
}

// guest plays the VM's end of the console pipe.
type guest struct {
	answer  chan string
	ready   chan struct{}
	release chan struct{}
	fatal   bool
	noReady bool
}

// dialConsole connects to the console pipe the machine serves, as
// Hyper-V's worker does, retrying until Start has created it.
func dialConsole(pipe string) (net.Conn, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		timeout := 200 * time.Millisecond
		c, err := winio.DialPipe(pipe, &timeout)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startGuest connects to pipe (the VM's COM1, served by the machine)
// once it exists and then asks the format question, records the
// answer, then prints READY (or FATAL), and on release prints the clean
// line and closes.
func startGuest(t *testing.T, pipe string, fatal, noReady bool) *guest {
	t.Helper()
	g := &guest{answer: make(chan string, 1), ready: make(chan struct{}), release: make(chan struct{}), fatal: fatal, noReady: noReady}
	go func() {
		conn, err := dialConsole(pipe)
		if err != nil {
			t.Errorf("the guest could not connect to %s: %v", pipe, err)
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, askFormatLine+"\n")
		line, _ := br.ReadString('\n')
		g.answer <- strings.TrimSpace(line)
		switch {
		case g.fatal:
			_, _ = io.WriteString(conn, supervisor.FatalPrefix+": the boot failed\n")
		case g.noReady:
		default:
			_, _ = io.WriteString(conn, supervisor.ReadyLine+"\n")
		}
		close(g.ready)
		// Keep the connection so RequestPoweroff can be read by the test.
		<-g.release
		_, _ = io.WriteString(conn, supervisor.CleanLine+"\n")
	}()
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	return g
}

func (g *guest) stop() { close(g.release) }

func newMachine(t *testing.T, client *broker.Client, format bool) (*machine, *bytes.Buffer, string) {
	t.Helper()
	configDir := t.TempDir()
	abs, _ := filepath.Abs(configDir)
	imageDir := t.TempDir()
	var log bytes.Buffer
	b := Backend{Client: client}
	mAny, err := b.NewMachine(supervisor.MachineConfig{
		ImageDir:   imageDir,
		DataDisk:   filepath.Join(abs, DataDiskFile),
		FormatData: format,
		CPUs:       4,
		MemoryMB:   4096,
		ConsoleLog: &log,
	})
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	// The guest, which runs as this account, is admitted to the console
	// pipe as this account is (Hyper-V connects as LocalSystem).
	return mAny.(*machine), &log, host.DirKey(abs)
}

func waitEvent(t *testing.T, m *machine, want supervisor.EventKind) {
	t.Helper()
	select {
	case ev := <-m.Events():
		if ev.Kind != want {
			t.Fatalf("event kind = %d, want %d (%q)", ev.Kind, want, ev.Line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
}

func TestFirstBootAnswersOne(t *testing.T) {
	client := serveBroker(t, &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000001"})
	m, _, key := newMachine(t, client, true)
	g := startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := <-g.answer; got != "KIVALI-FORMAT-DATA 1" {
		t.Fatalf("format answer = %q", got)
	}
	waitEvent(t, m, supervisor.EventReady)
}

func TestLaterBootAnswersZero(t *testing.T) {
	client := serveBroker(t, &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000002"})
	m, _, key := newMachine(t, client, false)
	g := startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := <-g.answer; got != "KIVALI-FORMAT-DATA 0" {
		t.Fatalf("format answer = %q", got)
	}
	waitEvent(t, m, supervisor.EventReady)
}

func TestFatalBecomesEvent(t *testing.T) {
	client := serveBroker(t, &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000003"})
	m, _, key := newMachine(t, client, false)
	startGuest(t, broker.ComPipe(key), true, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEvent(t, m, supervisor.EventFatal)
}

func TestPoweroffVerbWritten(t *testing.T) {
	client := serveBroker(t, &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000004"})
	m, _, key := newMachine(t, client, false)
	pipe := broker.ComPipe(key)

	// A guest that records every line it reads after the format answer.
	got := make(chan string, 4)
	go func() {
		conn, err := dialConsole(pipe)
		if err != nil {
			t.Errorf("the guest could not connect to %s: %v", pipe, err)
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, askFormatLine+"\n")
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				got <- strings.TrimSpace(line)
			}
			if err != nil {
				return
			}
		}
	}()
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if first := <-got; first != "KIVALI-FORMAT-DATA 0" {
		t.Fatalf("first line = %q", first)
	}
	if err := m.RequestPoweroff(); err != nil {
		t.Fatalf("RequestPoweroff: %v", err)
	}
	select {
	case line := <-got:
		if line != "KIVALI-POWEROFF" {
			t.Fatalf("second line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poweroff verb was not written")
	}
}

func TestDoneOnPipeClose(t *testing.T) {
	client := serveBroker(t, &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000005", state: broker.StateRunning})
	m, _, key := newMachine(t, client, false)
	g := startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-g.ready
	g.stop() // the guest writes the clean line and closes the pipe
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close when the console pipe ended")
	}
}

func TestDoneOnBrokerReportsOff(t *testing.T) {
	// The console pipe stays open (noReady guest holds it), but the
	// broker reports the VM Off: Done still closes, from the poll (made
	// quick here; its period is statePoll).
	fb := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000006", state: broker.StateRunning}
	client := serveBroker(t, fb)
	m, _, key := newMachine(t, client, false)
	m.pollEvery = 50 * time.Millisecond
	startGuest(t, broker.ComPipe(key), false, true)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fb.mu.Lock()
	fb.state = broker.StateOff
	fb.mu.Unlock()
	select {
	case <-m.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done did not close when the broker reported the VM Off")
	}
}

// The poll is a backstop, not the signal: a running VM is not queried
// every couple of seconds (each query is a PowerShell process in the
// broker), and Done still closes at once when the console pipe ends.
// Start's own state query (settleLeftover) is not a poll, so the count
// starts once Start returns.
func TestStatePollIsABackstop(t *testing.T) {
	if statePoll < 15*time.Second || stateQueryTimeout < statePoll {
		t.Fatalf("statePoll %s, stateQueryTimeout %s", statePoll, stateQueryTimeout)
	}
	fb := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000009", state: broker.StateOff}
	client := serveBroker(t, fb)
	m, _, key := newMachine(t, client, false)
	if m.pollEvery != statePoll {
		t.Fatalf("pollEvery = %s", m.pollEvery)
	}
	g := startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fb.mu.Lock()
	started := fb.gets
	fb.mu.Unlock()
	<-g.ready
	time.Sleep(500 * time.Millisecond)
	g.stop()
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close when the console pipe ended")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if polls := fb.gets - started; polls != 0 {
		t.Fatalf("the broker was polled %d times in half a second", polls)
	}
}

// RemoveMachine deletes the VM of the config directory's key through
// the broker; a VM already gone (404) is no error, any other refusal is.
func TestRemoveMachine(t *testing.T) {
	const configDir = `C:\Users\Maya\AppData\Local\Kivali`
	fb := &fakeBroker{}
	b := Backend{Client: serveBroker(t, fb)}
	if err := b.RemoveMachine(configDir); err != nil {
		t.Fatalf("RemoveMachine: %v", err)
	}
	fb.mu.Lock()
	fb.removeStatus = http.StatusNotFound
	fb.mu.Unlock()
	if err := b.RemoveMachine(configDir); err != nil {
		t.Fatalf("RemoveMachine of a VM already gone: %v", err)
	}
	fb.mu.Lock()
	fb.removeStatus = http.StatusInternalServerError
	fb.mu.Unlock()
	if err := b.RemoveMachine(configDir); err == nil || !strings.Contains(err.Error(), "kivali-96e3ef74") {
		t.Fatalf("RemoveMachine refused by the broker: err = %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	want := []string{"kivali-96e3ef74", "kivali-96e3ef74", "kivali-96e3ef74"}
	if strings.Join(fb.removes, ",") != strings.Join(want, ",") {
		t.Fatalf("removes = %v, want %v", fb.removes, want)
	}
}

func TestDialAgentWithoutID(t *testing.T) {
	// The broker returns no id, so the VM has none: DialAgent refuses
	// cleanly rather than dialling a zero GUID.
	client := serveBroker(t, &fakeBroker{id: ""})
	m, _, _ := newMachine(t, client, false)
	if _, err := m.DialAgent(context.Background()); err == nil {
		t.Fatal("DialAgent without a VM id did not fail")
	}
}

func TestNewMachineDerivation(t *testing.T) {
	// The name, MAC and console pipe derive from the config directory's
	// key, the shared vector (sha256 of the lower-cased path starts
	// 96e3ef74).
	fb := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000007"}
	client := serveBroker(t, fb)
	b := Backend{Client: client}
	const configDir = `C:\Users\Maya\AppData\Local\Kivali`
	_, err := b.NewMachine(supervisor.MachineConfig{
		ImageDir: `C:\Program Files\Kivali\vm`,
		DataDisk: filepath.Join(configDir, DataDiskFile),
		CPUs:     4, MemoryMB: 4096,
	})
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	fb.mu.Lock()
	req := fb.ensureReq
	fb.mu.Unlock()
	if req.Name != "kivali-96e3ef74" {
		t.Errorf("name = %q", req.Name)
	}
	if req.MAC != "024B5696E3EF" {
		t.Errorf("mac = %q", req.MAC)
	}
	if req.ComPipe != `\\.\pipe\kivali-96e3ef74-com1` {
		t.Errorf("com pipe = %q", req.ComPipe)
	}
	if req.ConfigDir != configDir || req.RootVHDX != `C:\Program Files\Kivali\vm\root.vhdx` {
		t.Errorf("config/root = %q, %q", req.ConfigDir, req.RootVHDX)
	}
}

func TestCreateDataDisk(t *testing.T) {
	fb := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-000000000008"}
	client := serveBroker(t, fb)
	b := Backend{Client: client}
	dir := t.TempDir()
	p := filepath.Join(dir, DataDiskFile)
	// An existing file is removed before the broker is asked.
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateDataDisk(p, 64<<30); err != nil {
		t.Fatalf("CreateDataDisk: %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.vhds) != 1 || fb.vhds[0].Path != p || fb.vhds[0].SizeBytes != 64<<30 {
		t.Fatalf("vhd requests = %+v", fb.vhds)
	}
}

func TestImage(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Backend{}).Image(dir); err == nil {
		t.Fatal("Image accepted a directory without root.vhdx")
	}
	if err := os.WriteFile(filepath.Join(dir, RootFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := (Backend{}).Image(dir)
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if info.Arch != "amd64" || info.Version != "dev" {
		t.Fatalf("info = %+v", info)
	}
	if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte("0.16.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := (Backend{}).Image(dir); info.Version != "0.16.0" {
		t.Fatalf("version = %q", info.Version)
	}
}

func (f *fakeBroker) setState(st string) {
	f.mu.Lock()
	f.state = st
	f.mu.Unlock()
}

func (f *fakeBroker) stopsSeen() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.stops...)
}

// leftover makes a machine for a VM an earlier supervisor left in state,
// with the waits for it to stop shortened.
func leftover(t *testing.T, f *fakeBroker, state string) (*machine, string) {
	t.Helper()
	f.state = state
	client := serveBroker(t, f)
	m, _, key := newMachine(t, client, false)
	m.offPoll = time.Millisecond
	m.leftoverWait = 200 * time.Millisecond
	return m, key
}

// A VM left running by a supervisor that died (a logoff, a crash) is
// shut down through the guest agent's clean shutdown before it is
// started again, never turned off.
func TestLeftoverVMIsShutDownByTheAgent(t *testing.T) {
	f := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-0000000000a1"}
	m, key := leftover(t, f, broker.StateRunning)
	asked := false
	m.guestShutdown = func(context.Context) error {
		asked = true
		f.setState(broker.StateOff)
		return nil
	}
	startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEvent(t, m, supervisor.EventReady)
	if !asked || len(f.stopsSeen()) != 0 {
		t.Fatalf("agent shutdown asked %v, broker stops %v: want the agent only", asked, f.stopsSeen())
	}
}

// Without the agent, Hyper-V's shutdown integration service (a graceful
// stop) shuts the leftover VM down; still never a turn-off.
func TestLeftoverVMFallsBackToAGracefulStop(t *testing.T) {
	f := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-0000000000a2"}
	m, key := leftover(t, f, broker.StateRunning)
	m.guestShutdown = func(context.Context) error { return errors.New("no agent") }
	startGuest(t, broker.ComPipe(key), false, false)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := f.stopsSeen(); len(got) != 1 || !got[0] {
		t.Fatalf("broker stops %v, want one graceful stop", got)
	}
}

// A leftover VM that will not shut down cleanly, or one Hyper-V paused
// (a full host disk) or saved, is an error for a person: turning it off
// would lose what the guest has not written.
func TestLeftoverVMIsNeverTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		state           string
		gracefulIgnored bool
	}{
		{broker.StateRunning, true},
		{"PausedCritical", false},
		{"Saved", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-0000000000a3", gracefulIgnored: tc.gracefulIgnored}
			m, _ := leftover(t, f, tc.state)
			m.guestShutdown = func(context.Context) error { return errors.New("no agent") }
			err := m.Start()
			if err == nil || !strings.Contains(err.Error(), "not turned off") {
				t.Fatalf("Start: %v, want a refusal", err)
			}
			for _, graceful := range f.stopsSeen() {
				if !graceful {
					t.Fatalf("the VM was turned off: broker stops %v", f.stopsSeen())
				}
			}
			select {
			case <-m.Done():
			default:
				t.Fatal("Done still open after a refused start")
			}
		})
	}
}

// Up stops a leftover VM before anything touches the data disk; with no
// VM, or one that is off, there is nothing to do.
func TestStopLeftover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       *fakeBroker
		stopped bool
	}{
		{"no VM", &fakeBroker{missing: true}, false},
		{"off", &fakeBroker{state: broker.StateOff}, false},
		{"running", &fakeBroker{state: broker.StateRunning, id: "d1e2f3a4-0000-0000-0000-0000000000b1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := serveBroker(t, tc.f)
			var lines []string
			b := Backend{Client: client, guestShutdown: func(context.Context) error { return errors.New("no agent") }}
			err := b.StopLeftover(t.TempDir(), func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			})
			if err != nil {
				t.Fatalf("StopLeftover: %v", err)
			}
			got := tc.f.stopsSeen()
			if tc.stopped && (len(got) != 1 || !got[0]) || !tc.stopped && len(got) != 0 {
				t.Fatalf("broker stops %v (log %q)", got, lines)
			}
		})
	}
}

// A VM Hyper-V paused (a full host disk) is frozen intact; a hard stop
// refuses to turn it off.
func TestHardStopNeverTurnsOffAPausedVM(t *testing.T) {
	f := &fakeBroker{id: "d1e2f3a4-0000-0000-0000-0000000000c1"}
	client := serveBroker(t, f)
	m, _, _ := newMachine(t, client, false)
	f.setState("PausedCritical")
	if err := m.HardStop(); err == nil || !strings.Contains(err.Error(), "not turned off") {
		t.Fatalf("HardStop: %v, want a refusal", err)
	}
	if got := f.stopsSeen(); len(got) != 0 {
		t.Fatalf("broker stops %v, want none", got)
	}
}
