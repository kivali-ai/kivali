// Package fakeintegration is a stdio MCP server for tests: what
// cmd/fake-integration runs, and what a test binary runs when it
// re-executes itself as an integration's server. It answers
// initialize, ping, tools/list, tools/call and kivali/setup (one
// progress notification with the request's progress token, then one
// notifications/tools/list_changed, then a step that is done), with
// three tools:
//
//   - echo returns its arguments and the _meta.kivali the call carried,
//     as JSON text, so a test can see what was stamped on a call.
//   - whoami returns "fake account " and the value of FAKE_TOKEN. With
//     FAKE_ASK_CREDENTIAL=1 it asks for the connection's values
//     (kivali/credential) instead and names the keys it got back.
//   - list_colors returns ["red","green"] as JSON text.
//
// It writes "fake-integration ready" to its error output on start.
// Other switches, all environment variables: FAKE_PRINT_ENV=1 writes
// the names of its environment's variables to its error output on
// start, FAKE_PRINT_PROXY=1 its HTTPS_PROXY, FAKE_NO_INITIALIZE=1
// leaves initialize unanswered, FAKE_GARBAGE=1 writes a line that is
// not JSON after the handshake, FAKE_EXIT=<n> exits with status n after
// the handshake, and FAKE_ASK=<method> makes whoami send a request of
// that method first and name the error code it got back.
package fakeintegration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// EnvMarker, set to 1, is what a test binary's TestMain looks for to
// run Run instead of its tests. A test bundle's manifest puts it in
// mcp_config.env, the only environment the shim passes on.
const EnvMarker = "KIVALI_FAKE_INTEGRATION"

// Tools is what tools/list answers.
var Tools = []map[string]any{
	{"name": "echo", "description": "Return the arguments.", "inputSchema": map[string]any{"type": "object"}},
	{"name": "whoami", "description": "Name the connected account.", "inputSchema": map[string]any{"type": "object"}},
	{"name": "list_colors", "description": "List the colors.", "inputSchema": map[string]any{"type": "object"}},
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type fake struct {
	out    io.Writer
	getenv func(string) string

	wmu sync.Mutex
	mu  sync.Mutex
	// asked holds this server's own requests waiting for a response.
	asked map[string]chan message
	seq   int
}

// Run serves MCP on in and out until in ends, and returns the exit
// status.
func Run(in io.Reader, out, errOut io.Writer, getenv func(string) string, environ []string) int {
	f := &fake{out: out, getenv: getenv, asked: map[string]chan message{}}
	_, _ = fmt.Fprintln(errOut, "fake-integration ready")
	if getenv("FAKE_PRINT_ENV") == "1" {
		var names []string
		for _, kv := range environ {
			k, _, _ := strings.Cut(kv, "=")
			names = append(names, k)
		}
		sort.Strings(names)
		_, _ = fmt.Fprintln(errOut, "env: "+strings.Join(names, ","))
	}
	if getenv("FAKE_PRINT_PROXY") == "1" {
		_, _ = fmt.Fprintln(errOut, "proxy: "+getenv("HTTPS_PROXY"))
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var calls sync.WaitGroup
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			_, _ = fmt.Fprintln(errOut, "fake-integration: not JSON: "+sc.Text())
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0:
			f.mu.Lock()
			ch := f.asked[string(m.ID)]
			delete(f.asked, string(m.ID))
			f.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "" && len(m.ID) > 0:
			calls.Add(1)
			go func() {
				defer calls.Done()
				f.handle(m)
			}()
		case m.Method == "notifications/initialized" && getenv("FAKE_GARBAGE") == "1":
			f.writeLine([]byte("this is not JSON"))
		case m.Method == "notifications/initialized" && getenv("FAKE_EXIT") != "":
			code, _ := strconv.Atoi(getenv("FAKE_EXIT"))
			return code
		}
	}
	calls.Wait()
	return 0
}

func (f *fake) writeLine(b []byte) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_, _ = f.out.Write(append(b, '\n'))
}

func (f *fake) reply(id json.RawMessage, result any) {
	b, _ := json.Marshal(result)
	out, _ := json.Marshal(message{JSONRPC: "2.0", ID: id, Result: b})
	f.writeLine(out)
}

func (f *fake) fail(id json.RawMessage, code int, msg string) {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	f.writeLine(out)
}

func text(s string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isError}
}

func (f *fake) handle(m message) {
	switch m.Method {
	case "initialize":
		if f.getenv("FAKE_NO_INITIALIZE") == "1" {
			return
		}
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.reply(m.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-integration", "version": "1.0.0"},
		})
	case "ping":
		f.reply(m.ID, map[string]any{})
	case "tools/list":
		f.reply(m.ID, map[string]any{"tools": Tools})
	case "kivali/setup":
		// A setup step's stage, then a notification the shim drops,
		// then the step.
		var p struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{
			"progressToken": p.Meta.ProgressToken, "progress": 1, "total": 2, "message": "Building the app",
		}})
		f.writeLine(note)
		other, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		f.writeLine(other)
		f.reply(m.ID, map[string]any{"step": map[string]any{"done": true}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				Kivali json.RawMessage `json:"kivali"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.reply(m.ID, f.call(p.Name, p.Arguments, p.Meta.Kivali))
	default:
		f.fail(m.ID, -32601, "method not found: "+m.Method)
	}
}

func (f *fake) call(name string, args, kivali json.RawMessage) map[string]any {
	switch name {
	case "echo":
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if len(kivali) == 0 {
			kivali = json.RawMessage(`null`)
		}
		b, _ := json.Marshal(map[string]json.RawMessage{"arguments": args, "kivali": kivali})
		return text(string(b), false)
	case "whoami":
		if method := f.getenv("FAKE_ASK"); method != "" {
			resp := f.ask(method)
			code := 0
			if resp.Error != nil {
				code = resp.Error.Code
			}
			return text(fmt.Sprintf("asked %s: %d", method, code), false)
		}
		if f.getenv("FAKE_ASK_CREDENTIAL") == "1" {
			return f.askCredential()
		}
		return text("fake account "+f.getenv("FAKE_TOKEN"), false)
	case "list_colors":
		return text(`["red","green"]`, false)
	}
	return text("unknown tool "+name, true)
}

// ask sends a request of this server's own and waits for its response.
func (f *fake) ask(method string) message {
	f.mu.Lock()
	f.seq++
	id, _ := json.Marshal(fmt.Sprintf("fake-%d", f.seq))
	ch := make(chan message, 1)
	f.asked[string(id)] = ch
	f.mu.Unlock()
	req, _ := json.Marshal(message{JSONRPC: "2.0", ID: id, Method: method})
	f.writeLine(req)
	return <-ch
}

// askCredential asks for the connection's values and names the keys
// of what came back.
func (f *fake) askCredential() map[string]any {
	resp := f.ask("kivali/credential")
	if resp.Error != nil {
		return text(fmt.Sprintf("no credential: %d %s", resp.Error.Code, resp.Error.Message), true)
	}
	var cred struct {
		Env    map[string]string `json:"env"`
		Fields map[string]string `json:"fields"`
	}
	_ = json.Unmarshal(resp.Result, &cred)
	return text("env: "+keys(cred.Env)+"; fields: "+keys(cred.Fields), false)
}

func keys(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// Main is Run on the process's own standard streams and environment.
func Main() int {
	return Run(os.Stdin, os.Stdout, os.Stderr, os.Getenv, os.Environ())
}
