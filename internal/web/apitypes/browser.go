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
	// BrowserWindowEventTypeNotice carries Reason without ending the
	// window: an input the browser could not take, such as a paste
	// over the size limit or one sent before the browser has a page.
	BrowserWindowEventTypeNotice BrowserWindowEventType = "notice"
)

// WebSignInReason is why a web task waits for a person in its browser
// window: the page wants a sign-in, a second factor, a CAPTCHA solved
// or a consent given.
type WebSignInReason string

const (
	WebSignInReasonSignIn  WebSignInReason = "sign_in"
	WebSignInReasonMFA     WebSignInReason = "mfa"
	WebSignInReasonCaptcha WebSignInReason = "captcha"
	WebSignInReasonConsent WebSignInReason = "consent"
)

// BrowserWindowEvent is a text message from core on the browser
// window's WebSocket, GET /api/v1/agents/{slug}/subagents/{id}/browser.
// Frames travel beside it as binary messages, one JPEG of the shown
// tab each. A state event also says whether the window takes input
// (Input: the task waits for a person to sign in, in a site session),
// the session's site, so the page can flag a page off it, and whether
// Chromium runs without its sandbox (SandboxOff: the cluster would not
// let it start one, and the page says so). Reason is an end's reason
// and a notice's; on a state event it is a note the page shows in
// place of its own caption.
type BrowserWindowEvent struct {
	Type       BrowserWindowEventType `json:"type"`
	Browser    BrowserState           `json:"browser,omitempty"`
	Input      bool                   `json:"input,omitempty"`
	Site       string                 `json:"site,omitempty"`
	SandboxOff bool                   `json:"sandbox_off,omitempty"`
	Tab        *BrowserWindowTab      `json:"tab,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
}

// BrowserWindowInput is a text message from the page on the browser
// window's WebSocket: one input event for the shown tab. Core forwards
// it to the browser only while the window takes input, and drops it
// otherwise.
type BrowserWindowInput struct {
	Type  BrowserWindowInputType `json:"type"`
	Input BrowserInput           `json:"input"`
}

// BrowserWindowInputType discriminates BrowserWindowInput.
type BrowserWindowInputType string

const (
	BrowserWindowInputTypeInput BrowserWindowInputType = "input"
)

// BrowserInput is one event in the shown tab, in the frame's own
// pixels. Kind is mouse (Action pressed, released, moved or wheel, with
// X, Y, Button and the wheel's deltas), key (Action down or up, with
// Key, Code and Text) or paste (Text). Modifiers is a bit set: 1 alt,
// 2 control, 4 meta, 8 shift.
type BrowserInput struct {
	Kind      string  `json:"kind"`
	Action    string  `json:"action,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Button    string  `json:"button,omitempty"`
	DeltaX    float64 `json:"delta_x,omitempty"`
	DeltaY    float64 `json:"delta_y,omitempty"`
	Key       string  `json:"key,omitempty"`
	Code      string  `json:"code,omitempty"`
	Text      string  `json:"text,omitempty"`
	Modifiers int     `json:"modifiers,omitempty"`
}

// BrowserWindowTab is the tab a browser window shows: its id within
// the task, its URL and its title.
type BrowserWindowTab struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}
