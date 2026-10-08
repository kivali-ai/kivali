//go:build darwin && cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

const (
	guestBin    = "/usr/libexec/kivali/kivali-guest"
	fsckForce   = "kivali.fsck=force"
	oracleDir   = guestapi.DataDir + "/integrity"
	ackedRecord = "integrity-acked"
)

// integrityCycles is -integrity-cycles: the data disk's crash test. Each
// cycle boots the VM on the same data disk with a forced full e2fsck
// after the journal replay (any damage is a FATAL), waits for READY (so
// k3s and its datastore came back too), verifies the oracle against the
// highest write acknowledged before the last power cut, then runs it
// (writes, churn, trims, its own checks from the disk) and cuts the
// power at a random moment. The last cycle verifies and stops cleanly.
// Any lost acknowledged write, damage or corruption fails the test.
//
// The acknowledged count is kept in <run>/integrity-acked, so a test
// stopped part way can go on with -keep-data.
func integrityCycles(o options) error {
	if err := os.MkdirAll(o.run, 0o755); err != nil {
		return err
	}
	dataPath := filepath.Join(o.run, "data.img")
	created, err := prepareDataDisk(dataPath, o.keepData, o.dataSizeGiB<<30)
	if err != nil {
		return err
	}
	var acked uint64
	if !created {
		if b, err := os.ReadFile(filepath.Join(o.run, ackedRecord)); err == nil {
			acked, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	seed := o.integritySeed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	fmt.Printf("boottest: integrity: %d cycles, power cut after %s..%s of writes, seed %d, starting at acknowledged write %d\n",
		o.integrityCycles, o.integrityMinRun, o.integrityMaxRun, seed, acked)

	for c := 1; c <= o.integrityCycles; c++ {
		format := created && c == 1
		last := c == o.integrityCycles
		cut := o.integrityMinRun + time.Duration(rng.Int64N(int64(o.integrityMaxRun-o.integrityMinRun)+1))
		n, err := integrityCycle(o, dataPath, c, format, last, acked, cut)
		if err != nil {
			return fmt.Errorf("cycle %d: %w", c, err)
		}
		acked = n
		if err := os.WriteFile(filepath.Join(o.run, ackedRecord), []byte(strconv.FormatUint(acked, 10)+"\n"), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("boottest: integrity: %d power cuts survived; %d writes acknowledged and intact\n", o.integrityCycles-1, acked)
	return nil
}

// integrityCycle is one boot; it returns the highest acknowledged write.
func integrityCycle(o options, dataPath string, cycle int, format, last bool, acked uint64, cut time.Duration) (uint64, error) {
	cmdline := baseCmdline + " " + fsckForce
	if format {
		cmdline += " " + formatFlag
	}
	consolePath := filepath.Join(o.run, fmt.Sprintf("console-%03d.log", cycle))
	logFile, err := os.Create(consolePath)
	if err != nil {
		return acked, err
	}
	defer func() { _ = logFile.Close() }()
	hostToGuestR, hostToGuestW, err := os.Pipe()
	if err != nil {
		return acked, err
	}
	guestToHostR, guestToHostW, err := os.Pipe()
	if err != nil {
		return acked, err
	}
	vm, err := newVM(o, cmdline, dataPath, hostToGuestR, guestToHostW)
	if err != nil {
		return acked, err
	}
	events := make(chan string, 16)
	go watchConsole(guestToHostR, logFile, events, "")
	states := vm.StateChangedNotify()
	start := time.Now()
	if err := vm.Start(); err != nil {
		return acked, fmt.Errorf("start: %w", err)
	}
	fail := func(err error) (uint64, error) {
		if vm.CanStop() {
			_ = vm.Stop()
		}
		dumpTail(consolePath, 60)
		return acked, err
	}

	deadline := time.After(o.timeout)
wait:
	for {
		select {
		case ev := <-events:
			switch ev {
			case readyLine:
				break wait
			case fatalPrefix:
				return fail(errors.New("guest reported a fatal error (a forced e2fsck finding damage is one)"))
			}
		case st := <-states:
			if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
				return fail(fmt.Errorf("VM stopped before %s", readyLine))
			}
		case <-deadline:
			return fail(fmt.Errorf("no %s within %s", readyLine, o.timeout))
		}
	}
	fmt.Printf("boottest: cycle %d: %s after %s; forced e2fsck clean\n", cycle, readyLine, time.Since(start).Round(time.Second))

	c := &guestapi.Client{Dial: func(context.Context) (net.Conn, error) {
		devs := vm.SocketDevices()
		if len(devs) == 0 {
			return nil, errors.New("no vsock device")
		}
		return devs[0].Connect(guestapi.Port)
	}}
	vctx, vcancel := context.WithTimeout(context.Background(), 10*time.Minute)
	var out strings.Builder
	code, err := c.Exec(vctx, guestapi.ExecSpec{Argv: []string{guestBin, "integrity", "verify", "-dir", oracleDir, "-acked", strconv.FormatUint(acked, 10)}}, nil, &out, &out)
	vcancel()
	if err != nil {
		return fail(fmt.Errorf("verify: %w", err))
	}
	if code != 0 {
		return fail(fmt.Errorf("verify against acknowledged write %d exited %d: %s", acked, code, strings.TrimSpace(out.String())))
	}
	fmt.Printf("boottest: cycle %d: %s\n", cycle, strings.TrimSpace(out.String()))

	if last {
		if _, err := io.WriteString(hostToGuestW, poweroffVerb); err != nil {
			return fail(err)
		}
		for {
			select {
			case st := <-states:
				if st == vz.VirtualMachineStateStopped {
					return acked, nil
				}
			case <-time.After(2 * time.Minute):
				return fail(errors.New("VM did not stop within two minutes of the poweroff verb"))
			}
		}
	}

	// Run, and cut the power part way.
	var high atomic.Uint64
	high.Store(acked)
	acks := &ackWriter{high: &high}
	var stderr strings.Builder
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	ran := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, err := c.Exec(rctx, guestapi.ExecSpec{Argv: []string{
			guestBin, "integrity", "run", "-dir", oracleDir, "-from", strconv.FormatUint(acked+1, 10),
			"-corpus-mib", strconv.Itoa(o.integrityCorpusMiB),
		}}, nil, acks, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("run exited %d: %s%s", code, acks.corrupt(), strings.TrimSpace(stderr.String()))
		}
		ran <- err
	}()
	select {
	case err := <-ran:
		if err == nil {
			err = errors.New("run ended before the power cut")
		}
		return fail(err)
	case <-time.After(cut):
	}
	if err := vm.Stop(); err != nil {
		return fail(fmt.Errorf("hard stop: %w", err))
	}
	n := high.Load()
	fmt.Printf("boottest: cycle %d: power cut %s into the run, at acknowledged write %d (+%d)\n", cycle, cut.Round(time.Millisecond), n, n-acked)
	rcancel()
	wg.Wait()
	if bad := acks.corrupt(); bad != "" {
		return n, errors.New(bad)
	}
	if n == acked {
		return n, errors.New("no write was acknowledged before the power cut; lengthen -integrity-min-run")
	}
	for {
		st := vm.State()
		if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
			return n, nil
		}
		select {
		case <-states:
		case <-time.After(10 * time.Second):
			return n, fmt.Errorf("VM still %v 10 s after the hard stop", st)
		}
	}
}

// ackWriter records the highest "ACK <n>" line the run printed, and any
// CORRUPT line.
type ackWriter struct {
	high *atomic.Uint64
	mu   sync.Mutex
	buf  []byte
	bad  string
}

func (a *ackWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf = append(a.buf, p...)
	for {
		i := strings.IndexByte(string(a.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(a.buf[:i])
		a.buf = a.buf[i+1:]
		if v, ok := strings.CutPrefix(line, "ACK "); ok {
			if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > a.high.Load() {
				a.high.Store(n)
			}
		} else if strings.HasPrefix(line, "CORRUPT") && a.bad == "" {
			a.bad = line
		}
	}
}

func (a *ackWriter) corrupt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bad
}
