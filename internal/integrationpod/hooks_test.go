package integrationpod

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Why this test exists: kivali/setup's params are what a bundle's
// Python reads to decide its next step, and they must carry every part
// of the Answer a built-in's Setup is given, under the names the docs
// and the skill's template give them. Every map is an object, empty on
// the first call, so a server reads answer["form"].get(...) without a
// check; a nil State or Return written as null would make the
// template's code fail on its first call. addresses is a list, empty
// for none, so a server registers each without a check either.
func TestSetupParamsOnTheWire(t *testing.T) {
	first, err := json.Marshal(SetupParams{Flow: "setup", Answer: SetupAnswerOf(builtinintegrations.Answer{Leg: 1, Callback: "https://k.example/connect/callback?state=s1", Addresses: []string{"https://k.example/connect/callback", "https://team.example/connect/callback"}, Team: "Acme"}), Meta: &RequestMeta{ProgressToken: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"flow":"setup","answer":{"state":{},"form":{},"return":{},"checked":false,"went":false,"leg":1,"callback":"https://k.example/connect/callback?state=s1","addresses":["https://k.example/connect/callback","https://team.example/connect/callback"],"team":"Acme"},"_meta":{"progressToken":"p1"}}`; string(first) != want {
		t.Errorf("first call =\n%s\nwant\n%s", first, want)
	}
	later, err := json.Marshal(SetupAnswerOf(builtinintegrations.Answer{
		State: map[string]string{"at": "go"}, Form: map[string]string{"account": "acme"},
		Return: url.Values{"code": {"c1"}, "scope": {"a", "b"}}, Checked: true, Went: true, Leg: 4,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"state":{"at":"go"},"form":{"account":"acme"},"return":{"code":["c1"],"scope":["a","b"]},"checked":true,"went":true,"leg":4,"callback":"","addresses":[],"team":""}`; string(later) != want {
		t.Errorf("a later call =\n%s\nwant\n%s", later, want)
	}
}

// Why this test exists: a bundle's step is read into the same Step a
// built-in returns, so the runner holds both to the same declarations.
// Each kind's fields must arrive under their snake_case names, a Go's
// post may give a key one value or several, and a step that is not one
// of the shapes (a misspelt key, a number for a string) must be an
// error rather than a step with a part silently missing: a Go read
// without its no_return would wait for a callback that never comes. A
// kept null reads as an empty value, which forgets the key, as try.py
// reads it.
func TestParseSetupResult(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      builtinintegrations.Step
	}{
		{"form", `{"step":{"form":{"words":"Paste the key.","fields":["key"]},"state":{"at":"key"}}}`,
			builtinintegrations.Step{Form: &builtinintegrations.Form{Words: "Paste the key.", Fields: []string{"key"}}, State: map[string]string{"at": "key"}}},
		{"form by hand", `{"step":{"form":{"words":"Register the address.","fields":[],"url":"https://app.example/apps/new","copy":["https://team.example/connect/callback"]}}}`,
			builtinintegrations.Step{Form: &builtinintegrations.Form{Words: "Register the address.", Fields: []string{}, URL: "https://app.example/apps/new", Copy: []string{"https://team.example/connect/callback"}}}},
		{"go", `{"step":{"go":{"words":"Approve it.","url":"https://app.example/new","post":{"manifest":"{}","scope":["a","b"]},"no_return":true,"stateless":true},"keep":{"app_id":"7"}}}`,
			builtinintegrations.Step{Go: &builtinintegrations.Go{Words: "Approve it.", URL: "https://app.example/new", Post: url.Values{"manifest": {"{}"}, "scope": {"a", "b"}}, NoReturn: true, Stateless: true}, Keep: map[string]string{"app_id": "7"}}},
		{"wait", `{"step":{"wait":{"words":"Enter the code.","url":"https://app.example/device","code":"WDJB-MJHT"}},"_meta":{}}`,
			builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Enter the code.", URL: "https://app.example/device", Code: "WDJB-MJHT"}}},
		{"done", `{"step":{"done":true,"keep":{"token":"t0k3n-1"}}}`, builtinintegrations.Step{Done: true, Keep: map[string]string{"token": "t0k3n-1"}}},
		{"keep forgets", `{"step":{"done":true,"keep":{"token":null}}}`, builtinintegrations.Step{Done: true, Keep: map[string]string{"token": ""}}},
		{"none", `{}`, builtinintegrations.Step{}},
	} {
		got, err := ParseSetupResult(json.RawMessage(tc.raw))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, %v", tc.name, got, err)
		}
	}
	for _, tc := range []struct{ raw, want string }{
		{`{"step":{"go":{"url":"https://app.example","noreturn":true}}}`, `unknown field "noreturn"`},
		{`{"step":{"done":true},"next":1}`, `unknown field "next"`},
		{`{"step":{"keep":{"token":7}}}`, "cannot unmarshal number"},
		{`{"step":{"go":{"url":"https://app.example","post":{"a":1}}}}`, "post.a is a string or a list of strings"},
		{`{"step":{"form":{"fields":"key"}}}`, "cannot unmarshal string"},
	} {
		if _, err := ParseSetupResult(json.RawMessage(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.raw, err, tc.want)
		}
	}
}

// Why this test exists: kivali/requirements' rows become the page's
// health rows and Needs you items, read as a built-in's are. A row's
// fields must arrive under their names, and an answer with no list (a
// server that answered {} or misspelt the key) must be an error the
// page shows as the integration's checks failing, not a healthy
// integration with no rows. So must a row lacking its id, title, words
// or met (an absent met would read as unmet), as the smoke test and
// try.py hold it.
func TestParseRequirements(t *testing.T) {
	got, err := ParseRequirements(json.RawMessage(`{"requirements":[{"id":"token","title":"Token","words":"It works.","met":true},{"id":"scopes","title":"Scopes","words":"One is missing.","met":false,"fix":"rescope"}]}`))
	want := []builtinintegrations.Requirement{{ID: "token", Title: "Token", Words: "It works.", Met: true}, {ID: "scopes", Title: "Scopes", Words: "One is missing.", Fix: "rescope"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v, %v", got, err)
	}
	if got, err := ParseRequirements(json.RawMessage(`{"requirements":[]}`)); err != nil || len(got) != 0 {
		t.Errorf("none = %+v, %v", got, err)
	}
	for _, raw := range []string{`{}`, `{"requirements":null}`, `{"rows":[]}`, `{"requirements":[{"id":"a","ok":true}]}`, `{"requirements":[{"id":"a","title":"A","words":"w"}]}`, `{"requirements":[{"title":"A","words":"w","met":true}]}`} {
		if _, err := ParseRequirements(json.RawMessage(raw)); err == nil {
			t.Errorf("%s read as rows", raw)
		}
	}
}
