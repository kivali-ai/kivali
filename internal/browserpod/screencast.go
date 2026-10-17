package browserpod

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
)

// The screencast's settings: JPEG at a quality that keeps frames small,
// no larger than the window Chromium runs at. A browser window is
// judged on how quickly it follows the page, not on its colours.
const (
	screencastQuality   = 50
	screencastMaxWidth  = 1280
	screencastMaxHeight = 800
)

// setScreencast starts or stops the screencast of a tab.
func (b *browser) setScreencast(ctx context.Context, t *tab, on bool) error {
	t.mu.Lock()
	was := t.screencast
	t.screencast = on
	t.mu.Unlock()
	if on == was {
		return nil
	}
	if !on {
		_, err := b.conn.Call(ctx, t.session, "Page.stopScreencast", nil)
		return err
	}
	_, err := b.conn.Call(ctx, t.session, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": screencastQuality,
		"maxWidth": screencastMaxWidth, "maxHeight": screencastMaxHeight,
	})
	return err
}

// watchGroup turns a group's screencast on or off. While it is on, the
// group's current tab is cast and the cast follows that tab as the
// group switches, opens and closes tabs. A hidden tab casts no frames,
// so a group that is now watched is brought to the front, unless a
// command is running: its end does that.
func (b *browser) watchGroup(ctx context.Context, name string, on bool) {
	b.mu.Lock()
	b.groupLocked(name).watched = on
	busy := b.running > 0
	b.mu.Unlock()
	b.followWatch(ctx, name)
	if on && !busy {
		b.frontWatched(ctx, name)
	}
}

// onScreencastVisibility hears that a cast tab was shown or hidden. A
// hidden tab has lost the front to another tab and casts no frames, so
// the watched group's current tab is brought back: now when no command
// runs, else when the running command ends.
func (b *browser) onScreencastVisibility(t *tab, raw json.RawMessage) {
	var ev struct {
		Visible bool `json:"visible"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Visible {
		return
	}
	b.mu.Lock()
	if b.front == t {
		b.front = nil
	}
	g := b.groups[t.group]
	shown := g != nil && g.watched && g.current == t
	busy := b.running > 0
	b.mu.Unlock()
	if shown && !busy {
		b.bringToFront(context.Background(), t)
	}
}

// followWatch puts a watched group's screencast on its current tab and
// off every other tab of the group; a group nobody watches has it off
// everywhere. Called wherever a group's current tab may have changed.
func (b *browser) followWatch(ctx context.Context, name string) {
	b.mu.Lock()
	g := b.groups[name]
	if g == nil {
		b.mu.Unlock()
		return
	}
	watched, current := g.watched, g.current
	tabs := append([]*tab(nil), g.tabs...)
	b.mu.Unlock()
	for _, t := range tabs {
		if err := b.setScreencast(ctx, t, watched && t == current); err != nil {
			b.log.Printf("screencast %s: %v", t.id, err)
		}
	}
}

// onScreencastFrame sends a frame to core, the JPEG as binary frames
// after its message, and acknowledges it, which lets Chromium send the
// next.
func (b *browser) onScreencastFrame(t *tab, raw json.RawMessage) {
	var ev struct {
		Data      string `json:"data"`
		SessionID int    `json:"sessionId"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	t.mu.Lock()
	on := t.screencast
	t.mu.Unlock()
	if on {
		if data, err := base64.StdEncoding.DecodeString(ev.Data); err == nil && len(data) > 0 {
			msg := Message{Type: TypeFrame, Group: t.group, Tab: t.id, Bytes: int64(len(data))}
			if err := b.send(msg, bytes.NewReader(data)); err != nil {
				b.log.Printf("frame %s: %v", t.id, err)
			}
		}
	}
	_, _ = b.conn.Call(context.Background(), t.session, "Page.screencastFrameAck", map[string]any{"sessionId": ev.SessionID})
}
