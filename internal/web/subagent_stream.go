package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// subagentStreamPoll is the cadence at which the SSE handler checks
// chat.jsonl for new bytes and meta.json for terminal status. 200ms
// is a small fraction of the human-perceptible UI lag floor; total
// CPU on a 30-second subagent is ~150 reads of two small files.
const subagentStreamPoll = 200 * time.Millisecond

// subagentStreamHeartbeat keeps idle SSE connections alive past
// middlebox timeouts. Mirrors orgStreamHeartbeat.
const subagentStreamHeartbeat = 25 * time.Second

// subagentStreamRetryMs is the EventSource auto-reconnect backoff hint.
// Same value as orgStreamRetryMs for consistency.
const subagentStreamRetryMs = 1500

// handleSubagentStream tails a subagent's chat.jsonl + meta.json and
// emits SSE events: one `chat_message` per appended JSONL line, plus
// `meta` snapshots whenever the meta JSON changes (so the client can
// update the task's status). When meta.json reports a terminal status
// AND the chat.jsonl tail is caught up, we emit `done` and close.
//
// The stream starts where the client left off. A client that already
// holds N rows of the file passes ?from=N and the first N rows are
// skipped; without it the stream replays from row 0. Every
// chat_message carries an SSE id equal to the number of rows sent so
// far, so when the browser's EventSource reconnects it sends that id
// back as Last-Event-ID and the stream resumes from the last row the
// client received, never replaying a row it already holds.
//
// Implementation choice: poll, don't watch. fsnotify would be slightly
// more efficient but adds a dependency for what is, in practice, a
// small file polled for a short window. The wire format and contract
// are what matter — the inner loop is replaceable.
func (s *Server) handleSubagentStream(w http.ResponseWriter, r *http.Request) {
	parent := r.PathValue("slug")
	id := r.PathValue("id")
	if parent == "" || id == "" {
		http.NotFound(w, r)
		return
	}
	subagentDir := filepath.Join(s.Store.Root(), "agents", parent, "subagents", id)
	if _, err := os.Stat(subagentDir); err != nil {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")

	_, _ = fmt.Fprintf(w, "retry: %d\n\n", subagentStreamRetryMs)
	flusher.Flush()

	transcript := filepath.Join(subagentDir, "chat.jsonl")
	metaPath := subagentMetaPath(s.Store.Root(), parent, id)

	tailer := &jsonlTailer{path: transcript, skip: subagentStreamResumeFrom(r)}
	// sent is the id on the next chat_message: how many rows of the
	// file the client holds once it has that row. It starts at the
	// rows skipped, so ids stay file positions across reconnects.
	sent := tailer.skip
	var lastMeta []byte

	emitChatMessages := func() {
		lines, err := tailer.readNew()
		if err != nil {
			return
		}
		for _, line := range lines {
			sent++
			writeIDSSE(w, flusher, strconv.Itoa(sent), "chat_message", line)
		}
	}
	emitMeta := func() (terminal bool) {
		body, err := os.ReadFile(metaPath)
		if err != nil {
			return false
		}
		if bytes.Equal(body, lastMeta) {
			// no change
		} else {
			lastMeta = append(lastMeta[:0], body...)
			writeRawSSE(w, flusher, "meta", body)
		}
		return metaIsTerminal(body)
	}

	// Initial catch-up: send every row the client does not yet have
	// before we enter the poll loop, so a client that joins late gets
	// the rest of the transcript without race-against-tailer logic.
	emitChatMessages()
	terminal := emitMeta()

	if terminal {
		// One final read in case the writer appended after we read
		// meta but before we read chat. Standard order-of-writes
		// guarantee: meta.json is written AFTER the runner returns,
		// which is AFTER all chat events have been flushed to disk.
		emitChatMessages()
		writeRawSSE(w, flusher, "done", []byte(`{}`))
		return
	}

	tick := s.clk().NewTicker(subagentStreamPoll)
	defer tick.Stop()
	hb := s.clk().NewTicker(subagentStreamHeartbeat)
	defer hb.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C():
			emitChatMessages()
			if emitMeta() {
				emitChatMessages() // catch any post-meta tail
				writeRawSSE(w, flusher, "done", []byte(`{}`))
				return
			}
		case <-hb.C():
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// subagentStreamResumeFrom is how many rows of chat.jsonl the client
// already holds. A reconnecting EventSource sends the id of the last
// chat_message it received as Last-Event-ID; that outranks ?from
// because it is never earlier. A value that is
// missing or does not parse falls through to the next, and to zero
// when neither is usable: replaying everything is the safe default
// for a client that said nothing.
func subagentStreamResumeFrom(r *http.Request) int {
	for _, raw := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("from")} {
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			continue
		}
		return n
	}
	return 0
}

// writeIDSSE is writeRawSSE with an id field. The browser's
// EventSource remembers the last id it saw and sends it back as the
// Last-Event-ID header when it reconnects.
func writeIDSSE(w http.ResponseWriter, f http.Flusher, id, event string, payload []byte) {
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, event, payload)
	f.Flush()
}

// jsonlTailer reads a JSONL file from a stable offset, yielding each
// new complete line on every readNew. Partial trailing lines (no
// newline) are kept buffered for the next poll.
type jsonlTailer struct {
	path    string
	offset  int64
	pending []byte
	// skip is how many valid rows to drop before yielding any: the
	// rows the client already holds. Counted down as rows are read,
	// so it is spent once and later appends flow through.
	skip int
}

func (t *jsonlTailer) readNew() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(t.offset, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, 64*1024)
	var out [][]byte
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			t.pending = append(t.pending, buf[:n]...)
			t.offset += int64(n)
			for {
				idx := bytes.IndexByte(t.pending, '\n')
				if idx < 0 {
					break
				}
				line := t.pending[:idx]
				if len(line) > 0 {
					// Validate JSON shape — drop garbage lines silently
					// rather than emit something that crashes the client
					// renderer. The page's row count (readSubagentHistory)
					// counts the same lines: it refuses to render at all
					// when one does not parse, so on a rendered page
					// every non-empty line is a row on both sides.
					var probe json.RawMessage
					if err := json.Unmarshal(line, &probe); err == nil {
						if t.skip > 0 {
							t.skip--
						} else {
							out = append(out, append([]byte(nil), line...))
						}
					}
				}
				t.pending = t.pending[idx+1:]
			}
		}
		if rerr != nil {
			break
		}
	}
	return out, nil
}

// metaIsTerminal reports whether a meta.json body indicates the
// subagent is done (either way) or is still running.
func metaIsTerminal(body []byte) bool {
	var m struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	switch m.Status {
	case "completed", "errored":
		return true
	default:
		return false
	}
}
