package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const ringSize = 200

// recorder keeps the last console messages and exceptions, and the
// in-flight network requests, for the window's debug dump.
type recorder struct {
	mu      sync.Mutex
	console []string
	pending map[network.RequestID]string
	n       int
}

func newRecorder() *recorder { return &recorder{pending: map[network.RequestID]string{}} }

func (r *recorder) push(line string) {
	r.console = append(r.console, time.Now().Format("15:04:05.000")+" "+line)
	if len(r.console) > ringSize {
		r.console = r.console[len(r.console)-ringSize:]
	}
}

func remoteObjString(a *runtime.RemoteObject) string {
	if a == nil {
		return ""
	}
	if len(a.Value) > 0 {
		var s string
		if json.Unmarshal([]byte(a.Value), &s) == nil {
			return s
		}
		return string(a.Value)
	}
	if a.Description != "" {
		return a.Description
	}
	return string(a.Type)
}

// handle is called from the chromedp event listener.
func (r *recorder) handle(ev any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e := ev.(type) {
	case *runtime.EventConsoleAPICalled:
		var parts []string
		for _, a := range e.Args {
			parts = append(parts, remoteObjString(a))
		}
		r.push(fmt.Sprintf("console.%s: %s", e.Type, strings.Join(parts, " ")))
	case *runtime.EventExceptionThrown:
		d := e.ExceptionDetails
		msg := d.Text
		if d.Exception != nil {
			msg += " " + remoteObjString(d.Exception)
		}
		r.push(fmt.Sprintf("EXCEPTION: %s (%s:%d:%d)", msg, d.URL, d.LineNumber, d.ColumnNumber))
	case *network.EventRequestWillBeSent:
		r.pending[e.RequestID] = fmt.Sprintf("%s %s %s (%s)", time.Now().Format("15:04:05.000"), e.Request.Method, e.Request.URL, e.Type)
	case *network.EventLoadingFinished:
		delete(r.pending, e.RequestID)
	case *network.EventLoadingFailed:
		delete(r.pending, e.RequestID)
		r.push(fmt.Sprintf("network failed: %s %s", e.ErrorText, e.BlockedReason))
	}
}

// dump writes results/dump-<session>-<n>/ and returns the directory.
func (r *recorder) dump(ctx context.Context, session string, b *Browser) (string, error) {
	r.mu.Lock()
	r.n++
	n := r.n
	console := append([]string(nil), r.console...)
	var pending []string
	for _, v := range r.pending {
		pending = append(pending, v)
	}
	r.mu.Unlock()
	sort.Strings(pending)

	dir := filepath.Join("results", fmt.Sprintf("dump-%s-%d", session, n))
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		n++
		dir = filepath.Join("results", fmt.Sprintf("dump-%s-%d", session, n))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var url, title string
	var wd any
	var shot []byte
	var stripped string
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := chromedp.Run(tctx,
		chromedp.Evaluate(`location.href`, &url),
		chromedp.Evaluate(`document.title`, &title),
		chromedp.Evaluate(`navigator.webdriver`, &wd),
		chromedp.CaptureScreenshot(&shot),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			stripped, _, err = axSnapshots(ctx)
			return err
		}),
	)
	var errs []string
	if err != nil {
		errs = append(errs, err.Error())
	}
	if len(shot) > 0 {
		_ = os.WriteFile(filepath.Join(dir, "screenshot.png"), shot, 0o644)
	}
	info := map[string]any{
		"time":               time.Now().Format(time.RFC3339),
		"session":            session,
		"mode":               b.Mode,
		"url":                url,
		"title":              title,
		"webdriver":          fmt.Sprint(wd),
		"webdriver_at_start": b.Webdriver,
		"pending_requests":   pending,
		"console_and_errors": len(console),
		"errors_during_dump": errs,
	}
	ib, _ := json.MarshalIndent(info, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "info.json"), ib, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "console.txt"), []byte(strings.Join(console, "\n")+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "pending-requests.txt"), []byte(strings.Join(pending, "\n")+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "ax-stripped.txt"), []byte(stripped), 0o644)
	return dir, nil
}
