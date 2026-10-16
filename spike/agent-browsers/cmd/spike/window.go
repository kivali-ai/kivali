package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/coder/websocket"
)

//go:embed window.html
var windowHTML []byte

const listenAddr = "127.0.0.1:7788"

// startHTTP serves /attack/ (static pages the container reaches via
// host.docker.internal:7788) plus any extra handlers.
func startHTTP(extra map[string]http.Handler) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.Handle("/attack/", http.StripPrefix("/attack/", http.FileServer(http.Dir("attack"))))
	for p, h := range extra {
		mux.Handle(p, h)
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}

// hub fans screencast frames and URL updates out to connected window pages.
type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	lastURL   []byte
	lastFrame []byte
}

func (h *hub) add() chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan []byte, 8)
	h.clients[c] = struct{}{}
	if h.lastURL != nil {
		c <- h.lastURL
	}
	if h.lastFrame != nil {
		c <- h.lastFrame // static pages may not repaint for a while
	}
	return c
}

func (h *hub) remove(c chan []byte) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) send(msg []byte, isURL bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if isURL {
		h.lastURL = msg
	} else {
		h.lastFrame = msg
	}
	for c := range h.clients {
		select {
		case c <- msg:
		default: // slow client: drop the frame
		}
	}
}

type inMsg struct {
	Type      string  `json:"type"` // mouse, wheel, key, paste, save, navigate
	Event     string  `json:"event"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	DX        float64 `json:"dx"`
	DY        float64 `json:"dy"`
	Button    string  `json:"button"`
	Buttons   int64   `json:"buttons"`
	Clicks    int64   `json:"clicks"`
	Key       string  `json:"key"`
	Code      string  `json:"code"`
	KeyCode   int64   `json:"keyCode"`
	Modifiers int64   `json:"modifiers"`
	Text      string  `json:"text"`
	URL       string  `json:"url"`
}

func cmdWindow(args []string) error {
	fs := flag.NewFlagSet("window", flag.ExitOnError)
	session := fs.String("session", "", "session name (profile file profiles/<name>.tar.zst)")
	mode := fs.String("mode", "headless", "headless or xvfb")
	startURL := fs.String("url", "", "start URL")
	_ = fs.Parse(args)
	if *session == "" {
		return fmt.Errorf("--session is required")
	}

	b, err := StartBrowser(*session, *mode, profilePath(*session))
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if !removed {
			b.Remove()
		}
	}()

	h := &hub{clients: map[chan []byte]struct{}{}}
	ctx := b.Ctx
	exec := func(a chromedp.Action) {
		if err := chromedp.Run(ctx, a); err != nil {
			fmt.Println("  cdp:", err)
		}
	}

	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *page.EventScreencastFrame:
			msg, _ := json.Marshal(map[string]any{
				"type": "frame", "data": e.Data,
				"dw": e.Metadata.DeviceWidth, "dh": e.Metadata.DeviceHeight,
			})
			h.send(msg, false)
			sid := e.SessionID
			go exec(page.ScreencastFrameAck(sid))
		case *page.EventFrameNavigated:
			if e.Frame.ParentID == "" {
				msg, _ := json.Marshal(map[string]any{"type": "url", "url": e.Frame.URL + e.Frame.URLFragment})
				h.send(msg, true)
				fmt.Println("  url:", e.Frame.URL+e.Frame.URLFragment)
			}
		}
	})

	if err := chromedp.Run(ctx,
		page.Enable(),
		page.StartScreencast().WithFormat(page.ScreencastFormatJpeg).WithQuality(70).
			WithMaxWidth(1280).WithMaxHeight(800).WithEveryNthFrame(1),
	); err != nil {
		return fmt.Errorf("start screencast: %w", err)
	}

	saveCh := make(chan struct{}, 1)
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(4 << 20)
		c := h.add()
		defer h.remove(c)
		cctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			for {
				select {
				case <-cctx.Done():
					return
				case m := <-c:
					wctx, wc := context.WithTimeout(cctx, 5*time.Second)
					err := conn.Write(wctx, websocket.MessageText, m)
					wc()
					if err != nil {
						cancel()
						return
					}
				}
			}
		}()
		for {
			_, data, err := conn.Read(cctx)
			if err != nil {
				return
			}
			var m inMsg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Type == "save" {
				select {
				case saveCh <- struct{}{}:
				default:
				}
				continue
			}
			dispatch(exec, m)
		}
	})

	srv, err := startHTTP(map[string]http.Handler{
		"/": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(windowHTML)
		}),
		"/ws": wsHandler,
	})
	if err != nil {
		return err
	}

	if *startURL != "" {
		go exec(chromedp.Navigate(*startURL))
	}

	fmt.Printf("\nWindow ready: http://%s/\nSign in, then click \"Save and close\" (or press Ctrl-C here).\n\n", listenAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
		fmt.Println("\n  interrupt: saving")
	case <-saveCh:
		fmt.Println("  save requested from page")
	}
	signal.Stop(sig)

	exec(page.StopScreencast())
	size, err := b.CloseAndPack(profilePath(*session))
	removed = true
	sctx, sc := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(sctx)
	sc()
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s: %.2f MB\n", profilePath(*session), mb(size))
	return nil
}

func dispatch(exec func(chromedp.Action), m inMsg) {
	mods := input.Modifier(m.Modifiers)
	switch m.Type {
	case "mouse":
		var t input.MouseType
		switch m.Event {
		case "down":
			t = input.MousePressed
		case "up":
			t = input.MouseReleased
		default:
			t = input.MouseMoved
		}
		btn := input.None
		switch m.Button {
		case "left":
			btn = input.Left
		case "middle":
			btn = input.Middle
		case "right":
			btn = input.Right
		}
		clicks := m.Clicks
		if clicks == 0 && t != input.MouseMoved {
			clicks = 1
		}
		a := input.DispatchMouseEvent(t, m.X, m.Y).WithButton(btn).WithModifiers(mods)
		if clicks > 0 {
			a = a.WithClickCount(clicks)
		}
		if m.Buttons != 0 {
			a = a.WithButtons(m.Buttons)
		}
		exec(a)
	case "wheel":
		exec(input.DispatchMouseEvent(input.MouseWheel, m.X, m.Y).WithDeltaX(m.DX).WithDeltaY(m.DY).WithModifiers(mods))
	case "key":
		if m.Event == "up" {
			exec(input.DispatchKeyEvent(input.KeyUp).WithKey(m.Key).WithCode(m.Code).
				WithWindowsVirtualKeyCode(m.KeyCode).WithNativeVirtualKeyCode(m.KeyCode).WithModifiers(mods))
			return
		}
		text := ""
		if len([]rune(m.Key)) == 1 && mods&(input.ModifierCtrl|input.ModifierMeta|input.ModifierAlt) == 0 {
			text = m.Key
		}
		if m.Key == "Enter" {
			text = "\r"
		}
		t := input.KeyRawDown
		if text != "" {
			t = input.KeyDown
		}
		a := input.DispatchKeyEvent(t).WithKey(m.Key).WithCode(m.Code).
			WithWindowsVirtualKeyCode(m.KeyCode).WithNativeVirtualKeyCode(m.KeyCode).WithModifiers(mods)
		if text != "" {
			a = a.WithText(text).WithUnmodifiedText(text)
		}
		exec(a)
	case "paste":
		if m.Text != "" {
			exec(input.InsertText(m.Text))
		}
	case "navigate":
		if m.URL != "" {
			go exec(chromedp.Navigate(m.URL))
		}
	}
}
