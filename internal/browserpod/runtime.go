package browserpod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Runtime is the browser pod: it dials core, starts Chromium when
// core says so, and runs core's commands against it until core asks
// it to save or close.
type Runtime struct {
	Slug    string
	Session string
	Token   string
	// CoreUDS is browser.sock, core's socket for browser pods: the only
	// way to core.
	CoreUDS string
	// Chromium is the browser binary.
	Chromium string
	// Dir holds the profile, downloads and task files while the pod
	// runs.
	Dir    string
	Logger *log.Logger
	Clock  clock.Clock

	// launch starts the browser; tests put a fake in its place.
	launch func(ctx context.Context, o chromeOptions) (browserProcess, error)
}

// errBrowserExited is the cause of a session's end when Chromium died.
var errBrowserExited = errors.New("chromium exited")

// Run serves one browser session. It returns nil after a clean save
// or close, and an error when the session ended any other way.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = log.New(log.Writer(), "browserpod: ", log.LstdFlags)
	}
	if r.Clock == nil {
		r.Clock = clock.New()
	}
	if r.launch == nil {
		r.launch = launchChrome
	}
	if r.Dir == "" {
		r.Dir = filepath.Join(os.TempDir(), "kivali-browser")
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	conn, err := r.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	s := &session{r: r, w: newWire(conn), log: r.Logger, snapshotReq: make(chan struct{}, 1)}
	return s.run(ctx)
}

func (r *Runtime) dial(ctx context.Context) (*websocket.Conn, error) {
	socket := r.CoreUDS
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	u := "http://core" + ConnectPath + url.PathEscape(r.Slug) + "/" + url.PathEscape(r.Session) + "/connect"
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: hc,
		HTTPHeader: http.Header{TokenHeader: []string{r.Token}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("browserpod: connect to core: %w", err)
	}
	return conn, nil
}

// session is one connection's state.
type session struct {
	r   *Runtime
	w   *wire
	log *log.Logger

	site      string
	headful   bool
	ephemeral bool

	proc     browserProcess
	b        *browser
	stopping atomic.Bool

	// cookies is the snapshot core sent before start, set once
	// Chromium is up; snapshotReq wakes the snapshot loop early.
	cookies     []byte
	snapshotReq chan struct{}

	workersMu sync.Mutex
	workers   map[string]*serialQueue
	input     *serialQueue
}

func (s *session) send(msg Message, body io.Reader) error {
	return s.w.writeMessage(context.Background(), msg, body)
}

func (s *session) sendState(state, detail string) {
	if err := s.send(Message{Type: TypeState, State: state, Detail: detail}, nil); err != nil {
		s.log.Printf("send state %s: %v", state, err)
	}
}

func (s *session) profileDir() string { return filepath.Join(s.r.Dir, "profile") }

func (s *session) run(ctx context.Context) error {
	msg, body, err := s.w.readMessage(ctx)
	if err != nil {
		return fmt.Errorf("browserpod: waiting for hello: %w", err)
	}
	if msg.Type != TypeHello {
		return fmt.Errorf("browserpod: expected hello, got %q", msg.Type)
	}
	s.site, s.headful, s.ephemeral = msg.Site, msg.Headful, msg.Ephemeral
	if err := s.receiveProfile(msg, body); err != nil {
		return err
	}

	for started := false; !started; {
		msg, body, err := s.w.readMessage(ctx)
		if err != nil {
			return fmt.Errorf("browserpod: waiting for start: %w", err)
		}
		switch msg.Type {
		case TypeStart:
			started = true
		case TypeFile:
			s.receiveFile(msg, body)
		case TypeCookies:
			s.receiveCookies(msg, body)
		case TypeClose:
			return nil
		default:
			s.log.Printf("ignoring %s before start", msg.Type)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return err
		}
	}

	s.sendState(StateStarting, "")
	proc, err := s.r.launch(ctx, chromeOptions{
		Binary: s.r.Chromium, Dir: s.r.Dir, Headful: s.headful,
		Logger: s.log, Clock: s.r.Clock,
	})
	if err != nil {
		s.sendState(StateStopped, err.Error())
		return fmt.Errorf("browserpod: start chromium: %w", err)
	}
	s.proc = proc
	defer func() {
		proc.Kill()
		<-proc.Done()
	}()
	s.b = newBrowser(proc.Conn(), s.r.Clock, s.log, s.site, s.r.Dir, s.send)
	s.b.ephemeral = s.ephemeral
	if err := s.b.start(ctx); err != nil {
		s.sendState(StateStopped, err.Error())
		return fmt.Errorf("browserpod: set up chromium: %w", err)
	}
	s.restoreCookies(ctx)
	detail := ""
	if proc.SandboxOff() {
		detail = "sandbox: off"
	}
	s.sendState(StateRunning, detail)

	loopCtx, cancelLoop := context.WithCancelCause(ctx)
	defer cancelLoop(context.Canceled)
	workCtx, cancelWork := context.WithCancel(loopCtx)
	defer cancelWork()
	s.workers = map[string]*serialQueue{}
	s.input = newSerialQueue()
	go s.input.run(workCtx)
	if !s.ephemeral {
		go s.snapshots(workCtx)
	}

	go func() {
		select {
		case <-proc.Done():
		case <-loopCtx.Done():
			return
		}
		if s.stopping.Load() {
			return
		}
		detail := proc.Detail()
		s.log.Printf("chromium exited: %s", detail)
		s.sendState(StateStopped, "chromium exited: "+detail)
		cancelLoop(errBrowserExited)
	}()

	for {
		msg, body, err := s.w.readMessage(loopCtx)
		if err != nil {
			if errors.Is(context.Cause(loopCtx), errBrowserExited) {
				return fmt.Errorf("browserpod: %w: %s", errBrowserExited, proc.Detail())
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("browserpod: connection to core: %w", err)
		}
		switch msg.Type {
		case TypeCommand:
			s.enqueue(workCtx, msg)
		case TypeFile:
			s.receiveFile(msg, body)
		case TypeCloseGroup:
			// The group's queue goes with its tabs, after whatever is
			// still queued on it.
			group := msg.Group
			q := s.queueFor(workCtx, group)
			q.push(func() {
				s.b.closeGroup(workCtx, group)
				s.b.frontWatched(workCtx, "")
				s.dropQueue(group, q)
			})
		case TypeScreencast:
			m := msg
			s.input.push(func() {
				if m.Tab == "" {
					s.b.watchGroup(workCtx, m.Group, m.OK)
					return
				}
				if t := s.b.tabByID(m.Tab); t != nil {
					if err := s.b.setScreencast(workCtx, t, m.OK); err != nil {
						s.log.Printf("screencast %s: %v", m.Tab, err)
					}
				}
			})
		case TypeInput:
			m := msg
			s.input.push(func() {
				if t := s.b.tabByID(m.Tab); t != nil {
					// The person's tab takes the front for their input,
					// even from a command's tab.
					s.b.bringToFront(workCtx, t)
					if err := s.b.input(workCtx, t, m.Args); err != nil {
						s.log.Printf("input %s: %v", m.Tab, err)
					}
				}
			})
		case TypeSnapshot:
			select {
			case s.snapshotReq <- struct{}{}:
			default:
			}
		case TypeSave:
			s.stopping.Store(true)
			cancelWork()
			return s.save(ctx)
		case TypeClose:
			s.stopping.Store(true)
			cancelWork()
			s.log.Printf("closing without saving")
			return nil
		default:
			s.log.Printf("ignoring %s", msg.Type)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return fmt.Errorf("browserpod: connection to core: %w", err)
		}
	}
}

// receiveProfile unpacks the profile hello carries, or starts a fresh
// one. A profile that does not unpack is discarded so the session can
// still start.
func (s *session) receiveProfile(msg Message, body io.Reader) error {
	dir := s.profileDir()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if msg.Bytes > 0 {
		if err := unpackProfile(body, dir); err != nil {
			s.log.Printf("saved profile did not unpack, starting fresh: %v", err)
			_ = os.RemoveAll(dir)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return fmt.Errorf("browserpod: reading profile: %w", err)
		}
		s.log.Printf("profile: %d bytes from core", msg.Bytes)
	}
	return os.MkdirAll(dir, 0o700)
}

// receiveFile stores a task file for a group's uploads.
func (s *session) receiveFile(msg Message, body io.Reader) {
	if msg.Group == "" || msg.FileName == "" {
		s.log.Printf("file without group or name; dropped")
		return
	}
	dir := filepath.Join(s.r.Dir, "files", cleanFileName(msg.Group))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Printf("file %s: %v", msg.FileName, err)
		return
	}
	path := filepath.Join(dir, cleanFileName(msg.FileName))
	f, err := os.Create(path)
	if err != nil {
		s.log.Printf("file %s: %v", msg.FileName, err)
		return
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.log.Printf("file %s: %v", msg.FileName, err)
		_ = os.Remove(path)
	}
}

// queueFor is the group's command queue, started on first use.
func (s *session) queueFor(ctx context.Context, group string) *serialQueue {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	q := s.workers[group]
	if q == nil {
		q = newSerialQueue()
		s.workers[group] = q
		go q.run(ctx)
	}
	return q
}

// dropQueue forgets a group's queue and stops its goroutine, once the
// queue has nothing left to run. A command for the group after this
// gets a fresh queue; core refuses such commands anyway.
func (s *session) dropQueue(group string, q *serialQueue) {
	s.workersMu.Lock()
	if s.workers[group] == q {
		delete(s.workers, group)
	}
	s.workersMu.Unlock()
	q.stop()
}

// enqueue runs a command on its group's queue and answers it. The
// watched group's tab gets the front back once the command is done.
func (s *session) enqueue(ctx context.Context, msg Message) {
	s.queueFor(ctx, msg.Group).push(func() {
		s.b.beginCommand()
		res := s.b.runCommand(ctx, msg)
		s.b.endCommand(ctx, msg.Group)
		if err := s.send(res, nil); err != nil {
			s.log.Printf("send result %s: %v", msg.ID, err)
		}
	})
}

// save closes Chromium, sends the packed profile, and waits for core
// to close the connection.
func (s *session) save(ctx context.Context) error {
	s.sendState(StateSaving, "")
	if err := s.proc.Close(ctx); err != nil {
		s.log.Printf("save: %v", err)
	}
	packed := filepath.Join(s.r.Dir, "profile.tar.zst")
	if err := s.packTo(packed); err != nil {
		s.sendState(StateStopped, "packing the profile failed: "+err.Error())
		return fmt.Errorf("browserpod: pack profile: %w", err)
	}
	defer func() { _ = os.Remove(packed) }()
	f, err := os.Open(packed)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := s.send(Message{Type: TypeProfile, Bytes: fi.Size()}, f); err != nil {
		return fmt.Errorf("browserpod: send profile: %w", err)
	}
	if err := s.send(Message{Type: TypeSaved}, nil); err != nil {
		return fmt.Errorf("browserpod: send saved: %w", err)
	}
	s.sendState(StateStopped, "")
	s.log.Printf("profile saved: %d bytes", fi.Size())
	for {
		_, body, err := s.w.readMessage(ctx)
		if err != nil {
			return nil
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return nil
		}
	}
}

func (s *session) packTo(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := packProfile(s.profileDir(), f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// serialQueue runs functions one at a time, in the order pushed, until
// its context ends or it is stopped.
type serialQueue struct {
	mu       sync.Mutex
	items    []func()
	wake     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newSerialQueue() *serialQueue {
	return &serialQueue{wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// stop ends the queue's goroutine once it is idle. What is pushed
// after this never runs.
func (q *serialQueue) stop() { q.stopOnce.Do(func() { close(q.done) }) }

func (q *serialQueue) push(fn func()) {
	q.mu.Lock()
	q.items = append(q.items, fn)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *serialQueue) run(ctx context.Context) {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			select {
			case <-q.wake:
				continue
			case <-q.done:
				return
			case <-ctx.Done():
				return
			}
		}
		fn := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		fn()
	}
}
