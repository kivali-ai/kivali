package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
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
	// The frame is the one that started the download: a tab's main
	// frame, or an iframe of it (a hidden iframe is a common way to
	// export a file).
	b.mu.Lock()
	t := b.tabs[ev.FrameID]
	if t == nil {
		t = b.frames[ev.FrameID]
	}
	var tabs []*tab
	if t == nil {
		for _, x := range b.tabs {
			tabs = append(tabs, x)
		}
	}
	b.mu.Unlock()
	if t == nil {
		t = b.tabWithFrame(tabs, ev.FrameID)
	}
	b.mu.Lock()
	b.downloads[ev.GUID] = &download{name: ev.SuggestedFilename, tab: t}
	b.mu.Unlock()
	if t != nil && ev.FrameID == t.targetID {
		// The navigation that became this download ends here.
		t.update(func() {
			t.starts++
			t.loading = false
		})
	}
}

// frameTree is Page.getFrameTree's answer: a frame and its children.
type frameTree struct {
	Frame struct {
		ID string `json:"id"`
	} `json:"frame"`
	ChildFrames []frameTree `json:"childFrames"`
}

func (f frameTree) has(id string) bool {
	if f.Frame.ID == id {
		return true
	}
	for _, c := range f.ChildFrames {
		if c.has(id) {
			return true
		}
	}
	return false
}

// tabWithFrame asks each tab for its frame tree and returns the one
// that holds frameID: a frame attached before the pod watched its tab.
func (b *browser) tabWithFrame(tabs []*tab, frameID string) *tab {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, t := range tabs {
		raw, err := b.conn.Call(ctx, t.session, "Page.getFrameTree", nil)
		if err != nil {
			continue
		}
		var res struct {
			FrameTree frameTree `json:"frameTree"`
		}
		if json.Unmarshal(raw, &res) == nil && res.FrameTree.has(frameID) {
			return t
		}
	}
	return nil
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
	if d.tab == nil {
		b.log.Printf("download %s: from no tab of a task; dropped", cleanFileName(d.name))
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
		b.noteOn(d.tab, fmt.Sprintf("A download of %s was stopped: the limit is %d MB.", cleanFileName(d.name), MaxDownloadBytes>>20))
	}
}

// deliverDownload sends a finished download of a tab to core, or drops
// one over the limit with a note on its tab, and removes the file.
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
		b.noteOn(d.tab, fmt.Sprintf("A download of %s (%s) was dropped: the limit is %d MB.", name, megabytes(fi.Size()), MaxDownloadBytes>>20))
		return
	}
	name = b.uniqueName(d.tab.group, name)
	msg := Message{Type: TypeDownload, Group: d.tab.group, Tab: d.tab.id, FileName: name, Bytes: fi.Size()}
	if err := b.send(msg, f); err != nil {
		b.log.Printf("download %s: %v", name, err)
		return
	}
	b.noteOn(d.tab, fmt.Sprintf("Downloaded %s (%s); it is with the task's files.", name, megabytes(fi.Size())))
}

// noteOn notes a download on its tab or, when that tab has closed (a
// popup Chromium closed once it had started the download), on the
// group's current tab, else on the tab that opened it.
func (b *browser) noteOn(t *tab, s string) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if !closed {
		t.note(s)
		return
	}
	b.mu.Lock()
	var on *tab
	if g := b.groups[t.group]; g != nil && g.current != nil {
		on = g.current
	} else if o := b.byID[t.opener]; o != nil {
		on = o
	}
	b.mu.Unlock()
	if on == nil {
		b.log.Printf("download note for %s, whose tab has closed: %s", t.group, s)
		return
	}
	on.note(s)
}

// uniqueName is the name a group's download is sent under: name when
// no download of the group has had it, else stem-2.ext, stem-3.ext and
// so on, kept within core's name length. Core writes the name as it
// is, so the name the model reads is the file it finds.
func (b *browser) uniqueName(group, name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	taken := b.sent[group]
	if taken == nil {
		taken = map[string]bool{}
		b.sent[group] = taken
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	out := name
	for i := 2; taken[out]; i++ {
		suffix := fmt.Sprintf("-%d", i)
		s := stem
		if room := files.MaxDownloadNameLength - len(suffix) - len(ext); len(s) > room {
			s = strings.TrimRight(s[:room], "-.")
		}
		if s == "" {
			s = "download"
		}
		out = s + suffix + ext
	}
	taken[out] = true
	return out
}

func megabytes(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// cleanFileName is the one name a download or a task file is known by
// on both ends of the wire: core's cleaner (files.CleanDownloadName),
// so the name the pod reports is the one core writes and the name core
// sends for a task file is the one web_upload names.
func cleanFileName(name string) string {
	return files.CleanDownloadName(name)
}
