package integration

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The server smoke test: a bundle's server started the way its pod
// starts it, but in its author's shell with no network, and asked what
// Kivali asks at every start: initialize, then tools/list. SmokeCommand
// is the bash command that runs it from the bundle's directory;
// SmokeFindings reads what it printed against the manifest. It checks
// that the server starts and lists the actions the manifest declares.
// It is not a review of what the code does: the install screen is.

// SmokeTimeout bounds the smoke test, from the server's start to its
// tools/list answer.
const SmokeTimeout = 20 * time.Second

// SmokeShellTimeout is the shell's bound on the smoke test's command:
// SmokeTimeout, the driver's own start and the server's stop.
const SmokeShellTimeout = SmokeTimeout + 10*time.Second

// smokeProtocolVersion is the MCP protocol version the smoke test's
// initialize names, the one the shim's names.
const smokeProtocolVersion = "2025-06-18"

// smokeDriver is the Python program the smoke test runs; see smoke.py.
//
//go:embed smoke.py
var smokeDriver string

// smokeHeredoc ends the driver's here-document in SmokeCommand.
const smokeHeredoc = "KIVALI_SMOKE_END"

type smokeSpec struct {
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	Timeout         float64           `json:"timeout"`
	ProtocolVersion string            `json:"protocol_version"`
}

// SmokeCommand is the bash command that runs m's smoke test from the
// bundle's directory: python3 runs the driver, which starts
// mcp_config's command and args with PATH, a HOME of its own, LANG and
// the manifest's env with every connection field empty, and no proxy
// variable, and prints one JSON line.
func SmokeCommand(m *Manifest) string {
	spec, _ := json.Marshal(smokeSpec{
		Command:         m.Server.MCPConfig.Command,
		Args:            nonNil(m.Server.MCPConfig.Args),
		Env:             ServerEnv(m, nil),
		Timeout:         SmokeTimeout.Seconds(),
		ProtocolVersion: smokeProtocolVersion,
	})
	return "python3 - " + shellQuote(string(spec)) + " <<'" + smokeHeredoc + "'\n" + smokeDriver + smokeHeredoc + "\n"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// shellQuote is s as one bash word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SmokeRun is what the shell gave back for SmokeCommand.
type SmokeRun struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is the shell's timeout ending the command.
	TimedOut bool
	// Err is a shell that could not run the command at all.
	Err string
}

// smokeOutput is the driver's JSON line.
type smokeOutput struct {
	Started    bool            `json:"started"`
	StartError *string         `json:"start_error"`
	Initialize json.RawMessage `json:"initialize"`
	ToolsList  json.RawMessage `json:"tools_list"`
	NotJSON    *string         `json:"not_json"`
	Ended      bool            `json:"ended"`
	TimedOut   bool            `json:"timed_out"`
	ExitCode   *int            `json:"exit_code"`
	Stderr     string          `json:"stderr"`
}

// parseSmokeOutput is the last line of stdout that is the driver's.
func parseSmokeOutput(stdout string) (smokeOutput, bool) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var out smokeOutput
		var keys map[string]json.RawMessage
		line := strings.TrimSpace(lines[i])
		if json.Unmarshal([]byte(line), &keys) != nil || keys["started"] == nil {
			continue
		}
		if json.Unmarshal([]byte(line), &out) == nil {
			return out, true
		}
	}
	return smokeOutput{}, false
}

// smokeClip bounds a quoted piece of a finding.
const smokeClip = 300

// SmokeFindings reads the smoke test's run against m: errors for a
// server that does not start, writes a line that is not JSON-RPC, does
// not answer initialize or tools/list in time, or answers either with
// an error, and for a declared action it does not list (worded for
// kivali.verify's and kivali.access.choices' tools); warnings for an
// action it lists that the manifest does not declare or whose input
// schema is not an object's, and for a run that could not check
// anything. listed is the actions the server listed, nil when it
// listed none.
func SmokeFindings(m *Manifest, run SmokeRun) (f Findings, listed []string) {
	out, ok := parseSmokeOutput(run.Stdout)
	if !ok {
		switch {
		case run.TimedOut:
			f.warnf("the server's start could not be checked: the check did not finish within %d seconds", int(SmokeShellTimeout/time.Second))
		default:
			f.warnf("the server's start could not be checked: %s", clipLine(firstNonBlank(run.Err, lastLines(run.Stderr), fmt.Sprintf("the check exited with status %d and printed nothing", run.ExitCode)), smokeClip))
		}
		return f, nil
	}
	if !out.Started {
		why := "it could not be run"
		if out.StartError != nil {
			why = *out.StartError
		}
		f.errorf("the server did not start (%s %s): %s", m.Server.MCPConfig.Command, strings.Join(m.Server.MCPConfig.Args, " "), clipLine(why, smokeClip))
		return f, nil
	}
	answer := func(method string, raw json.RawMessage) (json.RawMessage, bool) {
		if len(raw) == 0 || string(raw) == "null" {
			switch {
			case out.NotJSON != nil:
				f.errorf("the server wrote a line that is not JSON-RPC before it answered %s, and Kivali stops a server that does: %s", method, clipLine(*out.NotJSON, smokeClip))
			case out.Ended:
				f.errorf("the server ended%s before it answered %s%s", exitWords(out.ExitCode), method, stderrWords(out.Stderr))
			default:
				f.errorf("the server did not answer %s within %d seconds%s", method, int(SmokeTimeout/time.Second), stderrWords(out.Stderr))
			}
			return nil, false
		}
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &resp)
		if resp.Error != nil {
			f.errorf("the server answered %s with an error: %s", method, clipLine(resp.Error.Message, smokeClip))
			return nil, false
		}
		return resp.Result, true
	}
	if _, ok := answer("initialize", out.Initialize); !ok {
		return f, nil
	}
	res, ok := answer("tools/list", out.ToolsList)
	if !ok {
		return f, nil
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if json.Unmarshal(res, &list) != nil || list.Tools == nil {
		f.errorf("the server's tools/list answer has no list of tools")
		return f, nil
	}
	declared := map[string]bool{}
	for _, t := range m.Tools {
		declared[t.Name] = true
	}
	have := map[string]bool{}
	var extra []string
	for _, t := range list.Tools {
		if t.Name == "" || have[t.Name] {
			continue
		}
		have[t.Name] = true
		listed = append(listed, t.Name)
		if !declared[t.Name] {
			extra = append(extra, t.Name)
			continue
		}
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" && !objectSchema(t.InputSchema) {
			f.warnf("the server gives %s an input schema that is not an object's; agents are never given it", t.Name)
		}
	}
	worded := map[string]bool{}
	if v := m.Kivali.Verify; v != "" && !have[v] {
		f.errorf("kivali.verify names %s, which the server does not list; Test connection calls it", v)
		worded[v] = true
	}
	if a := m.Kivali.Access; a != nil {
		for _, key := range sortedKeys(a.Choices) {
			if tool := a.Choices[key]; tool != "" && !have[tool] {
				f.errorf("kivali.access.choices.%s names %s, which the server does not list; the access form lists its options with it", key, tool)
				worded[tool] = true
			}
		}
	}
	for _, t := range m.Tools {
		if t.Name != "" && !have[t.Name] && !worded[t.Name] {
			f.errorf("tools declares %s, which the server does not list; agents are given only the actions it lists", t.Name)
			worded[t.Name] = true
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		f.warnf("the server lists %s, which tools does not declare; agents are never given it", name)
	}
	sort.Strings(listed)
	return f, slices.Clip(listed)
}

// objectSchema reports whether raw is a JSON Schema of an object.
func objectSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}
	var typ string
	return json.Unmarshal(schema["type"], &typ) == nil && typ == "object"
}

func exitWords(code *int) string {
	if code == nil {
		return ""
	}
	return fmt.Sprintf(" with status %d", *code)
}

// stderrWords is the end of the server's error output for a finding,
// or nothing when it wrote none.
func stderrWords(stderr string) string {
	if tail := lastLines(stderr); tail != "" {
		return "; its error output ends: " + clipLine(tail, smokeClip)
	}
	return ""
}

// lastLines is the last few lines of s that are not blank, joined on
// one line, at most smokeClip bytes from the end.
func lastLines(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > 3 {
		keep = keep[len(keep)-3:]
	}
	out := strings.Join(keep, " | ")
	if len(out) > smokeClip {
		cut := len(out) - smokeClip
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		out = "..." + out[cut:]
	}
	return out
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// clipLine is s on one line, at most n bytes.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
