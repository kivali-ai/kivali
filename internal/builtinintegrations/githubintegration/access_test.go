package githubintegration

import (
	"encoding/json"
	"strings"
	"testing"
)

// Why this test exists: the model's arguments arrive as JSON, and a
// number it writes as 7.0 or "7", a limit past the action's most, or a
// switch written as text would otherwise reach GitHub as something
// else. Each check takes exactly what the shipped bundle took.
func TestArgumentChecks(t *testing.T) {
	read := func(s string) args {
		t.Helper()
		a, err := readArgs(json.RawMessage(s))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return a
	}
	for s, ok := range map[string]bool{`{"number":7}`: true, `{"number":7.0}`: false, `{"number":"7"}`: false, `{"number":0}`: false, `{"number":-1}`: false, `{"number":true}`: false, `{"number":1e3}`: false, `{}`: false} {
		if _, err := read(s).number("number"); (err == nil) != ok {
			t.Errorf("number %s: %v", s, err)
		}
	}
	if n, err := read(`{}`).limit(30, 50); n != 30 || err != nil {
		t.Errorf("limit absent = %d, %v", n, err)
	}
	if n, err := read(`{"limit":500}`).limit(30, 50); n != 50 || err != nil {
		t.Errorf("limit past the most = %d, %v", n, err)
	}
	if _, err := read(`{"limit":null}`).limit(30, 50); err == nil {
		t.Error("limit null read")
	}
	if s, err := read(`{}`).choice("state", []string{"open", "all"}, "open"); s != "open" || err != nil {
		t.Errorf("choice absent = %q, %v", s, err)
	}
	if _, err := read(`{"state":"OPEN"}`).choice("state", []string{"open", "all"}, "open"); err == nil || err.Error() != "state is one of open, all." {
		t.Errorf("choice = %v", err)
	}
	for s, want := range map[string]string{
		`{"title":""}`:     "Give the title.",
		`{"title":" "}`:    "Give the title.",
		`{"title":3}`:      "title is text.",
		`{"title":"a\rb"}`: "title is one line.",
		`{"title":"` + strings.Repeat("é", 257) + `"}`: "title is longer than 256 characters.",
	} {
		if _, err := read(s).text("title", true, true, titleMax); err == nil || err.Error() != want {
			t.Errorf("text %.40s: %v, want %q", s, err, want)
		}
	}
	if s, err := read(`{"title":"`+strings.Repeat("é", 256)+`"}`).text("title", true, true, titleMax); err != nil || len([]rune(s)) != 256 {
		t.Errorf("256 characters of two bytes each: %v", err)
	}
	for _, b := range []string{"main", "agent/fix-1", "release/1.2+build", "v1.2.0", "a1b2c3"} {
		if _, err := read(`{"base":"`+b+`"}`).branch("base", true); err != nil {
			t.Errorf("branch %q: %v", b, err)
		}
	}
	for _, s := range []string{`{"labels":["bug","ui"]}`, `{}`, `{"labels":null}`} {
		if _, err := read(s).labels(); err != nil {
			t.Errorf("labels %s: %v", s, err)
		}
	}
	for _, s := range []string{`{"labels":"bug"}`, `{"labels":[""]}`, `{"labels":["a,b"]}`, `{"labels":[1]}`, `{"labels":["` + strings.Repeat("l", 101) + `"]}`} {
		if _, err := read(s).labels(); err == nil {
			t.Errorf("labels %s read", s)
		}
	}
}

// Why this test exists: GitHub's answers are text in any script, and
// a cut made by bytes could split a character and hand the agent text
// that is not UTF-8; the bounds are characters, as the shipped bundle
// counted them.
func TestClipCountsCharacters(t *testing.T) {
	s := strings.Repeat("é", 10)
	if got, cut := clip(s, 4); got != "éééé" || !cut {
		t.Errorf("clip = %q %v", got, cut)
	}
	if got, cut := clip(s, 10); got != s || cut {
		t.Errorf("clip at its length = %q %v", got, cut)
	}
	text, err := answerText(strings.Repeat("é", answerClip+1))
	if err != nil || !strings.HasSuffix(text, clipNote) || len([]rune(strings.TrimSuffix(text, clipNote))) != answerClip {
		t.Errorf("answer clip: %v %d", err, len([]rune(text)))
	}
	// Compact JSON, with <, > and & as they are.
	if text, _ := answerText(map[string]string{"t": "a <b> & c"}); text != `{"t":"a <b> & c"}` {
		t.Errorf("answer %s", text)
	}
}

// Why this test exists: the access form's stored value may hold
// anything a past version or a hand edit left. Only a write switch that
// is true allows writing, and only strings are patterns.
func TestReadAccess(t *testing.T) {
	a := readAccess(map[string]any{"repositories": []any{"acme/*", 7, " ", nil, "Other/X"}, "write": "yes"})
	if a.write || len(a.repositories) != 2 || !a.includes("other/x") || a.includes("other/y") {
		t.Errorf("access %+v", a)
	}
	if a := readAccess(map[string]any{"repositories": "acme/*", "write": true}); !a.write || a.includes("acme/x") {
		t.Errorf("a string for the list: %+v", a)
	}
	if a := readAccess(nil); a.write || len(a.repositories) != 0 {
		t.Errorf("no access %+v", a)
	}
}
