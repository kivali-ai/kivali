package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/coder/websocket"
)

func imageName() string {
	if v := os.Getenv("SPIKE_IMAGE"); v != "" {
		return v
	}
	return "kivali-spike-browser:dev"
}

// Browser is one running browser container plus a chromedp connection to it.
type Browser struct {
	Container string
	Port      string
	Mode      string
	Webdriver string // navigator.webdriver right after start

	allocCtx    context.Context
	allocCancel context.CancelFunc
	Ctx         context.Context // first tab
	ctxCancel   context.CancelFunc
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func randSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartBrowser creates a container, copies the profile in (if the file exists),
// starts it, waits for DevTools and connects chromedp.
func StartBrowser(name, mode, profilePath string) (*Browser, error) {
	if mode != "headless" && mode != "xvfb" {
		return nil, fmt.Errorf("mode must be headless or xvfb, got %q", mode)
	}
	cname := fmt.Sprintf("spike-%s-%s", name, randSuffix())
	args := []string{"create", "--name", cname,
		"--shm-size=512m",
		"-p", "127.0.0.1::9223",
		"--add-host=host.docker.internal:host-gateway",
		"-e", "MODE=" + mode,
	}
	if extra := os.Getenv("SPIKE_CHROMIUM_EXTRA_FLAGS"); extra != "" {
		args = append(args, "-e", "CHROMIUM_EXTRA_FLAGS="+extra)
	}
	if os.Getenv("SPIKE_NO_SESSION_RESTORE") == "1" {
		args = append(args, "-e", "SESSION_RESTORE=0")
	}
	if os.Getenv("SPIKE_SANDBOX") == "1" {
		args = append(args, "-e", "SANDBOX=1")
	}
	if opts := os.Getenv("SPIKE_DOCKER_OPTS"); opts != "" {
		args = append(args, strings.Fields(opts)...)
	}
	args = append(args, imageName())
	if _, err := docker(args...); err != nil {
		return nil, err
	}
	b := &Browser{Container: cname, Mode: mode}
	if profilePath != "" {
		if st, err := os.Stat(profilePath); err == nil && st.Size() > 0 {
			if _, err := docker("cp", profilePath, cname+":/profile.tar.zst"); err != nil {
				b.Remove()
				return nil, err
			}
			fmt.Printf("  restoring %s (%.2f MB)\n", profilePath, mb(st.Size()))
		}
	}
	if _, err := docker("start", cname); err != nil {
		b.Remove()
		return nil, err
	}
	out, err := docker("port", cname, "9223/tcp")
	if err != nil {
		b.Remove()
		return nil, err
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
	b.Port = line[strings.LastIndex(line, ":")+1:]

	if err := waitDevTools(b.Port, 30*time.Second); err != nil {
		logs, _ := docker("logs", cname)
		b.Remove()
		return nil, fmt.Errorf("%v\ncontainer logs:\n%s", err, logs)
	}
	b.allocCtx, b.allocCancel = chromedp.NewRemoteAllocator(context.Background(), "ws://127.0.0.1:"+b.Port)

	// Attach to the existing about:blank tab rather than opening another.
	var opts []chromedp.ContextOption
	if id := firstPageTarget(b.Port); id != "" {
		opts = append(opts, chromedp.WithTargetID(target.ID(id)))
	}
	if os.Getenv("SPIKE_DEBUG") != "" {
		opts = append(opts, chromedp.WithDebugf(func(f string, a ...any) {
			s := fmt.Sprintf(f, a...)
			if len(s) > 300 {
				s = s[:300]
			}
			fmt.Println("[cdp]", s)
		}))
	}
	b.Ctx, b.ctxCancel = chromedp.NewContext(b.allocCtx, opts...)
	if err := chromedp.Run(b.Ctx); err != nil {
		b.Remove()
		return nil, fmt.Errorf("connect cdp: %w", err)
	}
	var wd any
	if err := chromedp.Run(b.Ctx, chromedp.Evaluate(`navigator.webdriver`, &wd)); err != nil {
		b.Webdriver = "error: " + err.Error()
	} else {
		b.Webdriver = fmt.Sprint(wd)
	}
	fmt.Printf("  container %s up (%s), devtools on 127.0.0.1:%s, navigator.webdriver=%s\n", cname, mode, b.Port, b.Webdriver)
	return b, nil
}

func firstPageTarget(port string) string {
	resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var ts []struct{ ID, Type string }
	if json.NewDecoder(resp.Body).Decode(&ts) != nil {
		return ""
	}
	for _, t := range ts {
		if t.Type == "page" {
			return t.ID
		}
	}
	return ""
}

func waitDevTools(port string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("devtools on port %s not ready after %s", port, d)
}

// CloseChromium sends Browser.close and waits for the chromium process to exit.
func (b *Browser) CloseChromium() error {
	// chromedp refuses Browser.close through its executor and, with a remote
	// allocator, never sends it on cancel, so send it on a raw connection.
	if err := sendBrowserClose(b.Port); err != nil {
		fmt.Println("  Browser.close:", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := docker("exec", b.Container, "test", "-f", "/tmp/chromium-exited"); err == nil {
			if _, err := docker("exec", b.Container, "pgrep", "-x", "chromium"); err != nil {
				b.ctxCancel()
				b.allocCancel()
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	b.ctxCancel()
	b.allocCancel()
	ps, _ := docker("exec", b.Container, "ps", "-eo", "pid,stat,comm")
	return fmt.Errorf("chromium in %s did not exit within 30s; processes:\n%s", b.Container, ps)
}

var packExcludes = []string{
	"./Default/Cache", "./Default/Code Cache", "./Default/GPUCache",
	"./Default/Service Worker/CacheStorage", "./GrShaderCache", "./ShaderCache",
	"./Default/DawnCache", "./SingletonLock", "./SingletonCookie", "./SingletonSocket",
}

// PackProfile tars /profile (minus caches) through zstd -3 inside the container
// and copies the result to dest. Returns the packed size in bytes.
func (b *Browser) PackProfile(dest string) (int64, error) {
	var ex []string
	for _, e := range packExcludes {
		ex = append(ex, fmt.Sprintf("--exclude='%s'", e))
	}
	script := fmt.Sprintf("cd /profile && tar %s -cf - . | zstd -3 -q -f -o /tmp/out.tar.zst", strings.Join(ex, " "))
	if _, err := docker("exec", b.Container, "sh", "-c", script); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, err
	}
	tmp := dest + ".tmp"
	if _, err := docker("cp", b.Container+":/tmp/out.tar.zst", tmp); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, err
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Remove stops and removes the container.
func (b *Browser) Remove() {
	if b.ctxCancel != nil {
		b.ctxCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	_, _ = docker("rm", "-f", b.Container)
}

// CloseAndPack is the clean shutdown path: Browser.close, wait, pack, remove.
func (b *Browser) CloseAndPack(dest string) (int64, error) {
	defer b.Remove()
	if err := b.CloseChromium(); err != nil {
		fmt.Println("  warning:", err, "(packing anyway)")
	}
	return b.PackProfile(dest)
}

func mb(n int64) float64 { return float64(n) / (1024 * 1024) }

func profilePath(session string) string { return filepath.Join("profiles", session+".tar.zst") }

func sendBrowserClose(port string) error {
	resp, err := http.Get("http://127.0.0.1:" + port + "/json/version")
	if err != nil {
		return err
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if err != nil {
		return err
	}
	u, err := url.Parse(v.WS)
	if err != nil {
		return err
	}
	u.Host = "127.0.0.1:" + port
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.close"}`)); err != nil {
		return err
	}
	// Read the reply (or the connection dropping as the browser exits).
	_, _, _ = conn.Read(ctx)
	return nil
}
