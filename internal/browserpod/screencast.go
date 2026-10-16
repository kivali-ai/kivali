package browserpod

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		"format": "jpeg", "quality": 60, "maxWidth": 1280, "maxHeight": 800,
	})
	return err
}

// onScreencastFrame sends a frame to core and acknowledges it, which
// lets Chromium send the next.
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
		if data, err := base64.StdEncoding.DecodeString(ev.Data); err == nil {
			b.sendMsg(Message{Type: TypeFrame, Group: t.group, Tab: t.id, Image: &Image{MIME: "image/jpeg", Data: data}})
		}
	}
	_, _ = b.conn.Call(context.Background(), t.session, "Page.screencastFrameAck", map[string]any{"sessionId": ev.SessionID})
}
