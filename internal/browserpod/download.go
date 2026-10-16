package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// download is one download in flight.
type download struct {
	name string
	tab  *tab
}

func (b *browser) downloadDir() string { return filepath.Join(b.dir, "downloads") }

// startDownloads has Chromium save downloads under the pod's
// directory, named by guid, and report them.
func (b *browser) startDownloads(ctx context.Context) error {
	if err := os.MkdirAll(b.downloadDir(), 0o700); err != nil {
		return err
	}
	_, err := b.conn.Call(ctx, "", "Browser.setDownloadBehavior", map[string]any{
		"behavior":      "allowAndName",
		"downloadPath":  b.downloadDir(),
		"eventsEnabled": true,
	})
	if err != nil {
		return fmt.Errorf("Browser.setDownloadBehavior: %w", err)
	}
	return nil
}

func (b *browser) onDownloadWillBegin(raw json.RawMessage) {
	var ev struct {
		FrameID           string `json:"frameId"`
		GUID              string `json:"guid"`
		URL               string `json:"url"`
		SuggestedFilename string `json:"suggestedFilename"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.GUID == "" {
		return
	}
	b.mu.Lock()
	t := b.tabs[ev.FrameID]
	b.downloads[ev.GUID] = &download{name: ev.SuggestedFilename, tab: t}
	b.mu.Unlock()
	if t != nil {
		// The navigation that became this download ends here.
		t.update(func() {
			t.starts++
			t.loading = false
		})
	}
}

func (b *browser) onDownloadProgress(raw json.RawMessage) {
	var ev struct {
		GUID  string `json:"guid"`
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if ev.State != "completed" && ev.State != "canceled" {
		return
	}
	b.mu.Lock()
	d := b.downloads[ev.GUID]
	delete(b.downloads, ev.GUID)
	b.mu.Unlock()
	path := filepath.Join(b.downloadDir(), filepath.Base(ev.GUID))
	if d == nil || ev.State == "canceled" {
		_ = os.Remove(path)
		return
	}
	// Sending may take a while; the event loop moves on.
	go b.deliverDownload(d, path)
}

// deliverDownload sends a finished download to core, or drops one
// over the limit with a note on its tab, and removes the file.
func (b *browser) deliverDownload(d *download, path string) {
	defer func() { _ = os.Remove(path) }()
	name := cleanFileName(d.name)
	f, err := os.Open(path)
	if err != nil {
		b.log.Printf("download %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	if fi.Size() > MaxDownloadBytes {
		if d.tab != nil {
			d.tab.note(fmt.Sprintf("A download of %s (%s) was dropped: the limit is %d MB.", name, megabytes(fi.Size()), MaxDownloadBytes>>20))
		}
		return
	}
	if d.tab == nil {
		b.log.Printf("download %s: from no tab of a task; dropped", name)
		return
	}
	msg := Message{Type: TypeDownload, Group: d.tab.group, Tab: d.tab.id, FileName: name, Bytes: fi.Size()}
	if err := b.send(msg, f); err != nil {
		b.log.Printf("download %s: %v", name, err)
		return
	}
	d.tab.note(fmt.Sprintf("Downloaded %s (%s); it is with the task's files.", name, megabytes(fi.Size())))
}

func megabytes(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// cleanFileName reduces a name to one safe path element: letters,
// digits, dots, dashes and underscores, spaces turned into dashes.
// download.bin stands in when nothing is left.
func cleanFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r == ' ':
			sb.WriteRune('-')
		case r == '.' || r == '-' || r == '_':
			sb.WriteRune(r)
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			sb.WriteRune(r)
		}
	}
	out := strings.TrimLeft(sb.String(), ".")
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	if out == "" || strings.Trim(out, "-_") == "" {
		return "download.bin"
	}
	return out
}
