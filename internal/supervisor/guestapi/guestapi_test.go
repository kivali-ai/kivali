//go:build unix

package guestapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfine(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "..", "../x", "a/../../x"} {
		if _, err := confine("/data", bad); err == nil {
			t.Errorf("confine(%q) accepted", bad)
		}
	}
	got, err := confine("/data", "k3s/./server/manifests/kivali.yaml")
	if err != nil || got != "/data/k3s/server/manifests/kivali.yaml" {
		t.Fatalf("confine = %q, %v", got, err)
	}
}

// serve runs a Server on a Unix socket and returns a Client for it.
func serve(t *testing.T, s *Server) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s := NewServer("test")
	s.RunDir = filepath.Join(dir, "run")
	s.DataDir = filepath.Join(dir, "data")
	s.ImagesDir = filepath.Join(dir, "images")
	s.ChartsDir = filepath.Join(dir, "charts")
	s.VMVersionFile = filepath.Join(dir, "vm-version")
	s.BootIDFile = filepath.Join(dir, "boot_id")
	s.KernelFile = filepath.Join(dir, "osrelease")
	s.Env = []string{"PATH=/usr/bin:/bin"}
	s.NodeReadyArgv = []string{"sh", "-c", "echo boot-1 True"}
	s.K3sVersionArgv = []string{"echo", "k3s version test"}
	s.ImportArgv = []string{"sh", "-c", "wc -c"}
	s.PoweroffArgv = []string{"true"}
	for _, d := range []string{s.RunDir, s.DataDir, s.ImagesDir, s.ChartsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range map[string]string{s.BootIDFile: "boot-1\n", s.VMVersionFile: "0.1.0\n", s.KernelFile: "6.18\n"} {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestStatusStates(t *testing.T) {
	s := testServer(t)
	img := writeTar(t, map[string]string{"manifest.json": `[{"RepoTags":["kivali:dev"]}]`})
	if err := os.WriteFile(filepath.Join(s.ImagesDir, "k.tar"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	chart := gz(t, writeTar(t, map[string]string{"kivali/Chart.yaml": "name: kivali\nversion: 1.2.3\n"}))
	if err := os.WriteFile(filepath.Join(s.ChartsDir, "kivali-1.2.3.tgz"), chart, 0o644); err != nil {
		t.Fatal(err)
	}
	c := serve(t, s)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateBooting || !st.NodeReady || st.BootID != "boot-1" {
		t.Fatalf("status %+v", st)
	}
	if len(st.BakedImages) != 1 || st.BakedImages[0] != "docker.io/library/kivali:dev" {
		t.Fatalf("baked images %v", st.BakedImages)
	}
	if len(st.BakedCharts) != 1 || st.BakedCharts[0].Version != "1.2.3" {
		t.Fatalf("baked charts %v", st.BakedCharts)
	}
	if st.Versions.VMImage != "0.1.0" || st.Versions.K3s != "k3s version test" {
		t.Fatalf("versions %+v", st.Versions)
	}

	// A READY recorded by the previous boot does not count.
	if err := os.WriteFile(filepath.Join(s.RunDir, "ready"), []byte("boot-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ = c.Status(ctx); st.State != StateBooting {
		t.Fatalf("stale ready: state %s", st.State)
	}
	if err := os.WriteFile(filepath.Join(s.RunDir, "ready"), []byte("boot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ = c.Status(ctx); st.State != StateReady {
		t.Fatalf("state %s, want ready", st.State)
	}
	if err := os.WriteFile(filepath.Join(s.RunDir, "fatal"), []byte("ready: no node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ = c.Status(ctx); st.State != StateFatal || st.Fatal != "ready: no node" {
		t.Fatalf("state %s %q, want fatal", st.State, st.Fatal)
	}
}

func TestExecStreams(t *testing.T) {
	c := serve(t, testServer(t))
	var out, errb bytes.Buffer
	code, err := c.Exec(context.Background(), ExecSpec{Argv: []string{"sh", "-c", "cat; echo err >&2; exit 7"}},
		strings.NewReader("hello\n"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || out.String() != "hello\n" || errb.String() != "err\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	code, err = c.Exec(context.Background(), ExecSpec{Argv: []string{"/nonexistent/binary"}}, nil, nil, nil)
	if err == nil || code != -1 {
		t.Fatalf("missing binary: code %d err %v", code, err)
	}
}

func TestWriteFileConfined(t *testing.T) {
	s := testServer(t)
	c := serve(t, s)
	ctx := context.Background()
	if err := c.WriteFile(ctx, "k3s/server/manifests/kivali.yaml", 0o600, strings.NewReader("x: 1\n")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.DataDir, "k3s/server/manifests/kivali.yaml")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "x: 1\n" {
		t.Fatalf("file %q, %v", b, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := c.WriteFile(ctx, "../escape", 0o644, strings.NewReader("x")); err == nil {
		t.Fatal("escaping path accepted")
	}
}

func TestImportAndShutdown(t *testing.T) {
	c := serve(t, testServer(t))
	ctx := context.Background()
	res, err := c.ImportImages(ctx, strings.NewReader("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Output) != "5" {
		t.Fatalf("import %+v", res)
	}
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProxy(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		conn, err := up.Accept()
		if err != nil {
			return
		}
		b := make([]byte, 4)
		_, _ = io.ReadFull(conn, b)
		_, _ = conn.Write(append([]byte("echo:"), b...))
		_ = conn.Close()
	}()
	c := serve(t, testServer(t))
	conn, err := c.Proxy(context.Background(), up.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "echo:ping" {
		t.Fatalf("got %q", got)
	}
}

func TestPTY(t *testing.T) {
	c := serve(t, testServer(t))
	conn, err := c.OpenPTY(context.Background(), ExecSpec{Argv: []string{"sh", "-c", "stty size; read x; echo got:$x"}, Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	fw := NewFrameWriter(conn)
	if err := fw.Frame(FrameStdin, []byte("abc\n")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := RunStream(context.Background(), conn, nil, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(out.String(), "30 100") || !strings.Contains(out.String(), "got:abc") {
		t.Fatalf("code %d output %q", code, out.String())
	}
}

func TestFilterListenerRefusesOthers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "f.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Admit every second connection; record the refusals.
	n := 0
	refused := make(chan string, 4)
	fl := &FilterListener{
		Listener: l,
		Allow:    func(net.Addr) bool { n++; return n%2 == 0 },
		Logf:     func(f string, a ...any) { refused <- f },
	}
	srv := &http.Server{Handler: testServer(t).Handler()}
	go func() { _ = srv.Serve(fl) }()
	defer func() { _ = srv.Close() }()

	first, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(first); len(b) != 0 {
		t.Fatalf("refused connection got %q", b)
	}
	if f := <-refused; !strings.Contains(f, "not the host") {
		t.Fatalf("log %q", f)
	}
	c := &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("admitted connection: %v", err)
	}
}

func TestExecCancelKillsTheGroup(t *testing.T) {
	// The shell forks a child that would hold stdout open forever; a
	// cancelled exec must still end, which needs the whole group killed.
	// With an hour of WaitDelay, the handler can only finish promptly if
	// the backgrounded sleep (which holds the output pipes) was killed.
	s := testServer(t)
	s.KillGrace = time.Hour
	done := make(chan Exit, 1)
	s.execDone = func(ex Exit) { done <- ex }
	c := serve(t, s)
	conn, err := c.OpenExec(context.Background(), ExecSpec{Argv: []string{"sh", "-c", "sleep 1000 & echo started; wait"}})
	if err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(conn)
	if err != nil || typ != FrameStdout || string(p) != "started\n" {
		t.Fatalf("frame %d %q %v", typ, p, err)
	}
	_ = conn.Close() // the host goes away
	if ex := <-done; ex.Code != 128+9 {
		t.Fatalf("exit %+v, want SIGKILL", ex)
	}
}
