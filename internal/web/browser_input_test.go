package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestBrowserWindowInputDecodesWhatThePageSends: the page's input
// messages, spelled as web/src/screens/browser/BrowserWindow.tsx sends
// them (testdata/browser_window_input.json), decode with every field
// in place.
func TestBrowserWindowInputDecodesWhatThePageSends(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "browser_window_input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []apitypes.BrowserWindowInput
	if err := json.Unmarshal(body, &msgs); err != nil {
		t.Fatal(err)
	}
	want := []apitypes.BrowserInput{
		{Kind: "mouse", Action: "pressed", X: 120.5, Y: 44, Button: "left", Modifiers: 8},
		{Kind: "mouse", Action: "wheel", X: 10, Y: 20, DeltaX: -3.5, DeltaY: 120},
		{Kind: "key", Action: "down", Key: "a", Code: "KeyA", Text: "a", Modifiers: 2},
		{Kind: "paste", Text: "hello"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("%d messages, want %d", len(msgs), len(want))
	}
	for i, m := range msgs {
		if m.Type != apitypes.BrowserWindowInputTypeInput {
			t.Errorf("messages[%d].type = %q", i, m.Type)
		}
		if m.Input != want[i] {
			t.Errorf("messages[%d].input = %+v, want %+v", i, m.Input, want[i])
		}
	}
}

// TestBrowserInputMatchesThePodsInputArgs: the window's input and the
// pod's InputArgs are one shape on the wire. Core relays the one as the
// other, so every field, in the same JSON name, must be on both.
func TestBrowserInputMatchesThePodsInputArgs(t *testing.T) {
	jsonNames := func(typ reflect.Type) map[string]reflect.Kind {
		out := map[string]reflect.Kind{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := splitTag(f.Tag.Get("json"))
			out[name] = f.Type.Kind()
		}
		return out
	}
	core, pod := jsonNames(reflect.TypeFor[apitypes.BrowserInput]()), jsonNames(reflect.TypeFor[browserpod.InputArgs]())
	if !reflect.DeepEqual(core, pod) {
		t.Errorf("fields differ:\ncore %v\npod  %v", core, pod)
	}

	full := apitypes.BrowserInput{Kind: "mouse", Action: "wheel", X: 1.5, Y: 2.5, Button: "middle", DeltaX: -3, DeltaY: 4, Key: "k", Code: "KeyK", Text: "t", Modifiers: 15}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var args browserpod.InputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	back, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(raw) {
		t.Errorf("through InputArgs:\n%s\nwant\n%s", back, raw)
	}
}

// splitTag is a json struct tag's name and options.
func splitTag(tag string) (name string, opts string, ok bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", tag != ""
}
