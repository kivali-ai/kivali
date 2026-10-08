package web

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// toolTextCap is how much of a tool call's output, and of each string
// in its input, the chat API sends. The chat is refetched whole after
// every turn, and a call's payload (a file it wrote, a page it read)
// is most of a chat's bytes while the page shows it collapsed. The
// rest is one request away: GET /api/v1/agents/{slug}/tool-calls/{id}.
const toolTextCap = 2000

// toolTextEllipsis marks where a capped text was cut.
const toolTextEllipsis = "…"

// capToolOutput is s cut to toolTextCap bytes on a rune boundary, and
// whether it was cut.
func capToolOutput(s string) (string, bool) {
	if len(s) <= toolTextCap {
		return s, false
	}
	return runePrefix(s, toolTextCap) + toolTextEllipsis, true
}

// runePrefix is the longest prefix of s of at most n bytes that does
// not split a UTF-8 sequence.
func runePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// capToolInput shortens a tool call's JSON input and reports whether it
// changed anything. Each string value longer than toolTextCap is cut,
// in place, so the input stays valid JSON with its keys in the model's
// order: the page pretty-prints it and reads the call's summary (a
// path, a recipient) out of it. Input that is not JSON is cut like an
// output.
func capToolInput(s string) (string, bool) {
	if len(s) <= toolTextCap {
		return s, false
	}
	if !json.Valid([]byte(s)) {
		return capToolOutput(s)
	}
	var b strings.Builder
	b.Grow(toolTextCap * 2)
	cut := false
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '"' {
			b.WriteByte(c)
			i++
			continue
		}
		end := stringLiteralEnd(s, i)
		lit := s[i:end] // with both quotes
		body := lit[1 : len(lit)-1]
		if len(body) <= toolTextCap {
			b.WriteString(lit)
		} else {
			b.WriteByte('"')
			b.WriteString(escapeSafePrefix(body, toolTextCap))
			b.WriteString(toolTextEllipsis)
			b.WriteByte('"')
			cut = true
		}
		i = end
	}
	return b.String(), cut
}

// stringLiteralEnd is the index just past the closing quote of the JSON
// string literal that opens at s[start]. s is valid JSON, so the
// literal is closed.
func stringLiteralEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // the escaped byte cannot close the literal
		case '"':
			return i + 1
		}
	}
	return len(s)
}

// escapeSafePrefix is a prefix of the raw (still escaped) body of a JSON
// string literal of at most n bytes, ending neither inside an escape
// sequence nor inside a UTF-8 sequence.
func escapeSafePrefix(body string, n int) string {
	out := 0
	for i := 0; i < len(body); {
		size := 1
		if body[i] == '\\' {
			size = 2
			if i+1 < len(body) && body[i+1] == 'u' {
				size = 6
			}
		} else if body[i] >= utf8.RuneSelf {
			_, size = utf8.DecodeRuneInString(body[i:])
		}
		if i+size > n {
			break
		}
		i += size
		out = i
	}
	return body[:out]
}
