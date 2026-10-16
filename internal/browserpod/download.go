package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// download is one download in flight.
type download struct {
	name string
	tab  *tab
	// cancelled: core's limit stopped it; the tab has been told.
	cancelled bool
}

func (b *browser) downloadDir() string { return filepath.Join(b.dir, "downloads") }

// startDownloads has Chromium save downloads under the pod's
// directory, named by guid, and report them.
func (b *browser) startDownloads(ctx context.Context) error {
	if err := os.MkdirAll(b.downloadDir(), 0o700); err != nil {
		return err
	}
	return b.setDownloadBehavior(ctx, "")
}

// setDownloadBehavior sets the download behaviour of one browser
// context: the default one for "", else the context named. Chromium
// keeps it per context, so a context made for a group needs its own.
func (b *browser) setDownloadBehavior(ctx context.Context, contextID string) error {
	params := map[string]any{
		"behavior":      "allowAndName",
		"downloadPath":  b.downloadDir(),
		"eventsEnabled": true,
	}
	if contextID != "" {
		params["browserContextId"] = contextID
	}
	if _, err := b.conn.Call(ctx, "", "Browser.setDownloadBehavior", params); err != nil {
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
		GUID          string  `json:"guid"`
		State         string  `json:"state"`
		TotalBytes    float64 `json:"totalBytes"`
		ReceivedBytes float64 `json:"receivedBytes"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if ev.State == "inProgress" {
		// Stop a file that is over the limit while it is still
		// arriving, so one large download cannot fill the pod's disk.
		if ev.TotalBytes > MaxDownloadBytes || ev.ReceivedBytes > MaxDownloadBytes {
			b.cancelDownload(ev.GUID)
		}
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

// cancelDownload has Chromium stop a download that grew past
// MaxDownloadBytes. The canceled progress event that follows removes
// what was written; the tab is told once.
func (b *browser) cancelDownload(guid string) {
	b.mu.Lock()
	d := b.downloads[guid]
	if d == nil || d.cancelled {
		b.mu.Unlock()
		return
	}
	d.cancelled = true
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.conn.Call(ctx, "", "Browser.cancelDownload", map[string]any{"guid": guid}); err != nil {
		b.log.Printf("cancel download %s: %v", guid, err)
		return
	}
	if d.tab != nil {
		d.tab.note(fmt.Sprintf("A download of %s was stopped: the limit is %d MB.", cleanFileName(d.name), MaxDownloadBytes>>20))
	}
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
