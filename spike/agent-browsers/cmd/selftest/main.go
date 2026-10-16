// Command selftest drives a running `spike window` with no person: it waits
// for a screencast frame, clicks the big link on attack/visible.html, checks
// the URL changes, then asks the window to save and close.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	addr := flag.String("ws", "ws://127.0.0.1:7788/ws", "window WebSocket")
	x := flag.Float64("x", 300, "click x")
	y := flag.Float64("y", 330, "click y")
	flag.Parse()
	if err := run(*addr, *x, *y); err != nil {
		fmt.Println("SELFTEST FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("SELFTEST PASS")
}

func run(addr string, x, y float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var conn *websocket.Conn
	var err error
	for i := 0; i < 120; i++ {
		conn, _, err = websocket.Dial(ctx, addr, nil)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	send := func(m map[string]any) error {
		b, _ := json.Marshal(m)
		return conn.Write(ctx, websocket.MessageText, b)
	}

	frames := 0
	url := ""
	clicked := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read (frames=%d url=%q): %w", frames, url, err)
		}
		var m struct {
			Type, URL, Data string
			DW              float64 `json:"dw"`
			DH              float64 `json:"dh"`
		}
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "frame":
			frames++
			if frames == 1 {
				fmt.Printf("first frame: %d bytes base64 JPEG, device %vx%v\n", len(m.Data), m.DW, m.DH)
			}
		case "url":
			url = m.URL
			fmt.Println("url:", url)
		}
		if !clicked && frames > 0 && strings.Contains(url, "visible.html") {
			clicked = true
			fmt.Printf("clicking at %v,%v\n", x, y)
			_ = send(map[string]any{"type": "mouse", "event": "move", "x": x, "y": y})
			_ = send(map[string]any{"type": "mouse", "event": "down", "x": x, "y": y, "button": "left", "buttons": 1, "clicks": 1})
			_ = send(map[string]any{"type": "mouse", "event": "up", "x": x, "y": y, "button": "left", "clicks": 1})
		}
		if clicked && strings.Contains(url, "second.html") {
			fmt.Printf("URL changed after click (%d frames so far); sending save\n", frames)
			if err := send(map[string]any{"type": "save"}); err != nil {
				return err
			}
			// the window closes the socket when it shuts down
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return nil
				}
			}
		}
	}
}
