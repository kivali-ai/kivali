package apitypes

// BrowserState is where a web task's browser is: starting (its pod is
// being created), running (frames can flow) or stopped (none runs; a
// window opened now starts one).
type BrowserState string

const (
	BrowserStateStarting BrowserState = "starting"
	BrowserStateRunning  BrowserState = "running"
	BrowserStateStopped  BrowserState = "stopped"
)

// BrowserWindowEventType discriminates BrowserWindowEvent.
type BrowserWindowEventType string

const (
	// BrowserWindowEventTypeState carries Browser.
	BrowserWindowEventTypeState BrowserWindowEventType = "state"
	// BrowserWindowEventTypeTab carries Tab, the tab the window shows:
	// sent when the window attaches, when the task moves to another
	// tab, and when that tab's URL or title changes.
	BrowserWindowEventTypeTab BrowserWindowEventType = "tab"
	// BrowserWindowEventTypeEnd ends the window, with Reason. Nothing
	// follows it; the page shows the reason instead of reconnecting.
	BrowserWindowEventTypeEnd BrowserWindowEventType = "end"
)

// BrowserWindowEvent is a text message on the browser window's
// WebSocket, GET /api/v1/agents/{slug}/subagents/{id}/browser. Frames
// travel beside it as binary messages, one JPEG of the shown tab each,
// and nothing goes the other way: the window watches.
type BrowserWindowEvent struct {
	Type    BrowserWindowEventType `json:"type"`
	Browser BrowserState           `json:"browser,omitempty"`
	Tab     *BrowserWindowTab      `json:"tab,omitempty"`
	Reason  string                 `json:"reason,omitempty"`
}

// BrowserWindowTab is the tab a browser window shows: its id within
// the task, its URL and its title.
type BrowserWindowTab struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}
