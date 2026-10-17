// Package browserpod is the browser a web subagent drives: the pod
// that runs Chromium for one of an agent's web sessions, the manifest
// core creates it from, and the protocol between the two.
//
// The pod never listens. It dials core once, over core.sock, and that
// one WebSocket carries everything: core's commands and the pod's
// results, the pod's state, downloads on their way to core, the saved
// profile in both directions, and later the screencast and the user's
// input. Core keeps every byte that outlives the pod; the pod holds
// the profile only in ephemeral storage while it runs.
//
// A web subagent's tools (web_navigate, web_snapshot, ...) never reach
// the pod directly: each is a call to core, which checks the task's
// session and state and relays the command here under the subagent's
// tab group. The pod enforces the group: a command touches only tabs
// of its group, and web_tabs lists only those.
package browserpod

import "encoding/json"

// ConnectPath is the UDS route the pod dials:
// GET /v1/browser/{slug}/{session}/connect, upgraded to a WebSocket.
// The pod sends TokenHeader with the token core put in its environment.
const ConnectPath = "/v1/browser/"

// TokenHeader carries the per-pod token on the connect request. Core
// minted it when it created the pod and accepts the connection only
// when it matches the one it is waiting for.
const TokenHeader = "X-Kivali-Browser"

// Environment variables the pod reads. Core sets them in the manifest.
const (
	EnvSlug    = "KIVALI_BROWSER_SLUG"
	EnvSession = "KIVALI_BROWSER_SESSION"
	EnvToken   = "KIVALI_BROWSER_TOKEN"
	// EnvCoreToken is the token core proves itself with: its hello
	// carries it, and the pod refuses a session whose hello does not.
	EnvCoreToken = "KIVALI_BROWSER_CORE_TOKEN"
	EnvCoreUDS   = "KIVALI_BROWSER_CORE_UDS"
)

// Message is one text frame on the WebSocket, in either direction.
// Type says which fields are set (listed with each type below). Bulk
// bytes never travel in a Message: a message with Bytes set announces
// that exactly that many bytes follow as binary frames, in order, and
// the receiver reads them before the next text frame.
type Message struct {
	Type string `json:"type"`

	// ID pairs a command with its result.
	ID string `json:"id,omitempty"`
	// Group is the subagent whose tabs a command may touch, which a
	// new tab belongs to, and which a download or file is for.
	Group string `json:"group,omitempty"`
	// Tab names a tab within its group ("t1", "t2", ...). On a
	// command, empty means the group's current tab, created on first
	// use.
	Tab string `json:"tab,omitempty"`

	// Name is a command's name (CommandNavigate, ...) and Args its
	// arguments, the matching Args struct encoded.
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`

	// OK, Text, Error, Code and Image are a result. Text is what the
	// model reads; Image, when set, is an image block after it. A
	// refused or failed command has OK false, Error for the model and
	// Code for core.
	OK    bool   `json:"ok,omitempty"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
	Image *Image `json:"image,omitempty"`

	// State is the pod's state on a state message.
	State string `json:"state,omitempty"`
	// Detail is free text for a log line: why a start failed, what
	// the pod noticed.
	Detail string `json:"detail,omitempty"`

	// URL and Title describe a tab on tab and navigated messages.
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	// Opener is the tab that opened a new one, empty when a command did.
	// The pod sends it; core does not read it.
	Opener string `json:"opener,omitempty"`

	// FileName names a download or an uploaded file; Bytes is how many
	// binary bytes follow this message.
	FileName string `json:"file_name,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`

	// Site is the session's registrable domain on hello, empty for the
	// open session. The pod refuses to navigate a tab off it.
	Site string `json:"site,omitempty"`
	// CoreToken is, on hello, the token from the pod's EnvCoreToken.
	CoreToken string `json:"coreToken,omitempty"`
	// Headful on hello asks for a visible Chromium under Xvfb instead
	// of the new headless mode.
	Headful bool `json:"headful,omitempty"`
	// Ephemeral on hello: the session keeps nothing. Every tab group
	// gets a browser context of its own, with its own cookies, storage
	// and cache, disposed with the group, and core never asks for the
	// profile.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Unlocked on site-lock: the group's pages may leave the site.
	Unlocked bool `json:"unlocked,omitempty"`
}

// Image is a screenshot or a screencast frame: raw bytes, base64 on
// the wire, with their media type.
type Image struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

// Message types core sends.
const (
	// TypeHello opens the session once the pod has connected. Bytes is
	// the size of the saved profile that follows as binary frames, 0
	// for a fresh profile. Site, Headful and Ephemeral describe the
	// session. CoreToken proves the sender is core.
	TypeHello = "hello"
	// TypeStart tells the pod the profile, and the cookies if any, are
	// complete: unpack the profile and start Chromium. TypeCookies
	// before it carries the session's last cookie snapshot, which the
	// pod sets once Chromium is up.
	TypeStart = "start"
	// TypeCommand is one web tool call: ID, Group, Tab, Name, Args.
	TypeCommand = "command"
	// TypeFile hands the pod a file a tab may upload: Group, FileName,
	// Bytes, then the bytes. The pod keeps it for the group.
	TypeFile = "file"
	// TypeSave asks the pod to close Chromium cleanly, pack the profile
	// and send it (TypeProfile), then stay until core closes the
	// connection.
	TypeSave = "save"
	// TypeClose asks the pod to exit without saving.
	TypeClose = "close"
	// TypeScreencast turns a screencast on or off, OK as the switch.
	// With Group alone it is the group's: its current tab is cast and
	// the cast follows the group's current tab as it changes. With Tab
	// it is that one tab's. Frames come back as TypeFrame.
	TypeScreencast = "screencast"
	// TypeInput is one input event from the browser window for a tab:
	// Tab, Args as InputArgs.
	TypeInput = "input"
	// TypeCloseGroup closes every tab of a group, when its subagent
	// has finished.
	TypeCloseGroup = "close-group"
	// TypeSnapshot asks the pod for its cookies now (TypeCookies) if
	// they have changed since the last it sent: a window has just
	// closed, so a person may have signed in.
	TypeSnapshot = "snapshot"
	// TypeSiteLock unlocks or relocks a group of a site session: Group,
	// Unlocked. Core unlocks a group while its task waits for a person
	// and a browser window is on it, so a sign-in that goes through an
	// identity provider on another site can finish; then the group's
	// pages may go anywhere. Commands stay on the site either way. On
	// relock every tab of the group whose page is off the site is sent
	// to https://<site>/. A pod starts with every group locked; core
	// sends the state only when it changes, and after start for each
	// group it holds unlocked.
	TypeSiteLock = "site-lock"
)

// Message types the pod sends.
const (
	// TypeState reports the pod's state: StateStarting while Chromium
	// comes up, StateRunning once it answers, StateSaving while the
	// profile is packed, StateStopped with Detail when Chromium died
	// or failed to start.
	TypeState = "state"
	// TypeResult answers a command: ID, OK, Text, Error, Code, Image.
	TypeResult = "result"
	// TypeTab reports a tab the pod now tracks: Group, Tab, Opener,
	// URL. Sent for tabs a command made and for tabs a page opened,
	// which join their opener's group.
	TypeTab = "tab"
	// TypeTabClosed reports a tab gone: Group, Tab.
	TypeTabClosed = "tab-closed"
	// TypeNavigated reports a tab's main frame at a new URL: Group,
	// Tab, URL, Title. Core counts these as pages loaded and keeps the
	// last as the task's final origin.
	TypeNavigated = "navigated"
	// TypeDownload announces a finished download: Group, Tab, FileName
	// (the browser's suggested name, cleaned), Bytes, then the bytes.
	TypeDownload = "download"
	// TypeProfile carries the packed profile after a save: Bytes, then
	// the bytes, then TypeSaved.
	TypeProfile = "profile"
	// TypeSaved closes a save: the profile was sent in full.
	TypeSaved = "saved"
	// TypeFrame is one screencast frame: Group, Tab and Bytes, the JPEG
	// following as binary frames. Never base64: frames are the busiest
	// thing on the connection.
	TypeFrame = "frame"
	// TypeCookies carries a site session's cookies as JSON, the array
	// Storage.setCookies takes: Bytes, then the bytes. The pod sends it
	// when the cookies have changed since the last it sent, every
	// snapshotEvery and at once on TypeSnapshot; core keeps the latest
	// beside the profile and sends it back before TypeStart on the
	// next browser. A browser that dies before its save loses minutes
	// of cookies, not a sign-in.
	TypeCookies = "cookies"
)

// Pod states, on TypeState.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateSaving   = "saving"
	StateStopped  = "stopped"
)

// Command names: the web_* tools without their prefix, plus the two
// core issues itself.
const (
	CommandNavigate   = "navigate"
	CommandSnapshot   = "snapshot"
	CommandScreenshot = "screenshot"
	CommandClick      = "click"
	CommandType       = "type"
	CommandSelect     = "select"
	CommandPress      = "press"
	CommandScroll     = "scroll"
	CommandBack       = "back"
	CommandTabs       = "tabs"
	CommandWait       = "wait"
	CommandEval       = "eval"
	CommandUpload     = "upload"
)

// KnownCommand reports whether name is a command a web tool stands
// for. Core relays nothing else.
func KnownCommand(name string) bool {
	switch name {
	case CommandNavigate, CommandSnapshot, CommandScreenshot, CommandClick, CommandType, CommandSelect,
		CommandPress, CommandScroll, CommandBack, CommandTabs, CommandWait, CommandEval, CommandUpload:
		return true
	}
	return false
}

// Result codes on a refused or failed command. Core reads them; the
// model reads Error.
const (
	// CodeOffSite: the navigation would leave the session's site.
	CodeOffSite = "off_site"
	// CodeTabLimit: the browser has its full count of tabs.
	CodeTabLimit = "tab_limit"
	// CodeTimeout: the page did not settle, or the wait ran out.
	CodeTimeout = "timeout"
	// CodeNotFound: no such ref, tab or file.
	CodeNotFound = "not_found"
	// CodeBadArgs: the arguments did not decode or were out of range.
	CodeBadArgs = "bad_args"
	// CodeBrowser: Chromium refused or the page errored.
	CodeBrowser = "browser"
)

// MaxTabsPerBrowser is how many tabs one browser holds across every
// group using it. A command that would open a ninth tab is refused
// with CodeTabLimit.
const MaxTabsPerBrowser = 8

// MaxSnapshotChars bounds a snapshot's text; a longer one is cut with a
// line saying so.
const MaxSnapshotChars = 40000

// MaxDownloadBytes bounds one download the pod sends to core; a larger
// file is dropped and reported in the tab's next result.
const MaxDownloadBytes = 50 << 20

// MaxProfileBytes bounds a packed profile in either direction: the pod
// refuses to unpack more, and core refuses to store more.
const MaxProfileBytes = 2 << 30

// SocketName is core's socket for browser pods, in the hostPath
// directory UDSSubdir under the agent pods' UDS directory. It serves
// the connect route and nothing else: a browser pod never sees the
// socket the agent pods' routes answer on.
const (
	SocketName = "browser.sock"
	UDSSubdir  = "browser"
)

// MaxEvalChars bounds the text an eval returns.
const MaxEvalChars = 8000

// Arguments of each command, as the tool input the model wrote and
// core relayed. Decoded by the pod; the MCP tool schemas describe the
// same shapes to the model.

// NavigateArgs: load URL in the tab and wait for it to settle.
type NavigateArgs struct {
	URL string `json:"url"`
}

// ClickArgs: click the element with this ref from the last snapshot.
// AcceptDialog, on a click, a keystroke or a press, accepts a
// confirm, prompt or beforeunload dialog the action opens in a site
// session, which dismisses one otherwise.
type ClickArgs struct {
	Ref          string `json:"ref"`
	AcceptDialog bool   `json:"accept_dialog,omitempty"`
}

// TypeArgs: focus the element and type Text; Submit presses Enter after.
type TypeArgs struct {
	Ref    string `json:"ref"`
	Text   string `json:"text"`
	Submit bool   `json:"submit,omitempty"`
	// AcceptDialog as on ClickArgs.
	AcceptDialog bool `json:"accept_dialog,omitempty"`
}

// SelectArgs: choose the option with this value or visible label in a
// select element.
type SelectArgs struct {
	Ref   string `json:"ref"`
	Value string `json:"value"`
}

// PressArgs: press a key or chord ("Enter", "Escape", "Tab",
// "ArrowDown", "Control+a").
type PressArgs struct {
	Key string `json:"key"`
	// AcceptDialog as on ClickArgs.
	AcceptDialog bool `json:"accept_dialog,omitempty"`
}

// ScrollArgs: scroll the element with Ref, or the page, by Direction;
// Amount is in viewport heights, 1 when 0.
type ScrollArgs struct {
	Ref       string  `json:"ref,omitempty"`
	Direction string  `json:"direction"`
	Amount    float64 `json:"amount,omitempty"`
}

// TabsArgs: list the group's tabs, switch to one, open a new one at an
// optional URL, or close one.
type TabsArgs struct {
	Action string `json:"action"`
	Tab    string `json:"tab,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Tab actions.
const (
	TabsList   = "list"
	TabsSwitch = "switch"
	TabsOpen   = "open"
	TabsClose  = "close"
)

// WaitArgs: wait up to TimeoutMS (default and maximum 30000) for text
// to appear, a ref to exist, or the next navigation to finish.
type WaitArgs struct {
	For       string `json:"for"`
	Value     string `json:"value,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// Wait conditions.
const (
	WaitText       = "text"
	WaitRef        = "ref"
	WaitNavigation = "navigation"
)

// EvalArgs: run Script in an isolated world of the tab's main frame
// and return its JSON value. Open session only; core refuses it
// elsewhere.
type EvalArgs struct {
	Script string `json:"script"`
}

// UploadArgs: set the file input with Ref to the task file File, one
// of the names core handed the pod with TypeFile.
type UploadArgs struct {
	Ref  string `json:"ref"`
	File string `json:"file"`
}

// InputArgs is one event from the browser window, on TypeInput.
type InputArgs struct {
	// Kind: "mouse" (Action pressed, released, moved, wheel),
	// "key" (Action down, up; Key, Code, Text), "paste" (Text).
	Kind   string  `json:"kind"`
	Action string  `json:"action,omitempty"`
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"`
	DeltaX float64 `json:"delta_x,omitempty"`
	DeltaY float64 `json:"delta_y,omitempty"`
	Key    string  `json:"key,omitempty"`
	Code   string  `json:"code,omitempty"`
	Text   string  `json:"text,omitempty"`
	// Modifiers is a bit set: 1 alt, 2 control, 4 meta, 8 shift.
	Modifiers int `json:"modifiers,omitempty"`
}
