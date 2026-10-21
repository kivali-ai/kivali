package integration

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func goldenManifest(t *testing.T) *Manifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/valid_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The access form is what the integration page draws for one agent's
// grant. A kind read wrong would draw a switch as a text box, or hide
// the options of a choice; the order is the schema's.
func TestAccessFormKinds(t *testing.T) {
	form := AccessForm(goldenManifest(t))
	if form == nil {
		t.Fatal("no form")
	}
	want := []Field{
		{Key: "blogs", Title: "Blogs", Description: "The blogs the agent may write in.", Kind: FieldChoices, Required: true, Options: []string{}, Choices: true},
		{Key: "write", Title: "May publish and edit", Kind: FieldSwitch, Options: []string{}},
		{Key: "region", Title: "Region", Kind: FieldChoice, Options: []string{"eu", "us"}},
		{Key: "tags", Title: "Tags", Kind: FieldList, Options: []string{}},
		{Key: "note", Title: "Note", Kind: FieldText, Options: []string{}},
		{Key: "labels", Title: "Labels", Kind: FieldChoices, Options: []string{"red", "blue"}},
	}
	if !reflect.DeepEqual(form.Fields, want) {
		t.Errorf("fields =\n%+v\nwant\n%+v", form.Fields, want)
	}
}

// A string field whose options the integration lists is a choice with
// no fixed options; a field without a title shows its key.
func TestAccessFormListedChoiceAndUntitled(t *testing.T) {
	m := &Manifest{Kivali: Kivali{Access: &Access{
		Schema:  json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"flag":{"type":"boolean"}}}`),
		Choices: map[string]string{"owner": "list_owners"},
	}}}
	form := AccessForm(m)
	if len(form.Fields) != 2 || form.Fields[0].Kind != FieldChoice || !form.Fields[0].Choices || len(form.Fields[0].Options) != 0 || form.Fields[0].Title != "owner" {
		t.Errorf("fields = %+v", form.Fields)
	}
}

// Access that is all or nothing has no form, so the page shows none
// and the summary says so.
func TestNoAccessForm(t *testing.T) {
	m := &Manifest{}
	if AccessForm(m) != nil {
		t.Error("a form for a manifest without kivali.access")
	}
	if got := AccessSummary(m, json.RawMessage(`{}`)); got != SummaryFullAccess {
		t.Errorf("summary = %q", got)
	}
	for _, ok := range []string{``, `null`, `{}`, ` { } `} {
		if err := ValidateAccess(m, json.RawMessage(ok)); err != nil {
			t.Errorf("ValidateAccess(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{`{"blogs":["a"]}`, `[]`, `"x"`} {
		if err := ValidateAccess(m, json.RawMessage(bad)); err == nil {
			t.Errorf("ValidateAccess(%q) accepted", bad)
		}
	}
}

// ValidateAccess is what stands between the access dialog and a row
// that grants something the form never offered. Each case is one way a
// filled form can be wrong, and the message is the one the dialog
// shows under the field.
func TestValidateAccess(t *testing.T) {
	m := goldenManifest(t)
	ok := []string{
		`{"blogs":["Marketing"]}`,
		`{"blogs":["Marketing","Eng"],"write":true,"region":"eu","tags":[],"note":"","labels":["red"]}`,
		`{"blogs":["Anything the integration lists"],"write":false,"region":""}`,
	}
	for _, raw := range ok {
		if err := ValidateAccess(m, json.RawMessage(raw)); err != nil {
			t.Errorf("ValidateAccess(%s): %v", raw, err)
		}
	}
	bad := map[string]struct{ key, msg string }{
		`{}`:                                    {"blogs", "Blogs is required"},
		`{"blogs":[]}`:                          {"blogs", "Blogs is required"},
		`{"blogs":null}`:                        {"blogs", "Blogs is required"},
		`{"blogs":"Marketing"}`:                 {"blogs", "Blogs must be a list of text"},
		`{"blogs":["a"],"write":"yes"}`:         {"write", "May publish and edit must be on or off"},
		`{"blogs":["a"],"region":"apac"}`:       {"region", "Region must be one of eu, us"},
		`{"blogs":["a"],"region":3}`:            {"region", "Region must be text"},
		`{"blogs":["a"],"labels":["green"]}`:    {"labels", "Labels lists green, which is not one of red, blue"},
		`{"blogs":["a"],"owner":"x","zzz":"y"}`: {"owner", "owner is not a field of this integration's access form"},
		`["blogs"]`:                             {"", "the access must be an object of field values"},
	}
	for raw, want := range bad {
		err := ValidateAccess(m, json.RawMessage(raw))
		var ae *AccessError
		if !errors.As(err, &ae) || ae.Key != want.key || ae.Message != want.msg {
			t.Errorf("ValidateAccess(%s) = %#v, want key %q message %q", raw, err, want.key, want.msg)
		}
	}
}

// The summary is the access in words on the page and the agent's tab:
// field by field in the form's order, an off switch and empty fields
// left out.
func TestAccessSummary(t *testing.T) {
	m := goldenManifest(t)
	cases := map[string]string{
		`{"blogs":["Marketing","Eng"],"write":true}`:                   "Blogs: Marketing, Eng · May publish and edit",
		`{"write":false,"blogs":["Marketing"]}`:                        "Blogs: Marketing",
		`{"blogs":["M"],"region":"eu","note":"weekdays","tags":["a"]}`: "Blogs: M · Region: eu · Tags: a · Note: weekdays",
		`{"blogs":[],"note":""}`:                                       SummaryNothingSet,
		`{"gone":"field"}`:                                             SummaryNothingSet,
	}
	for raw, want := range cases {
		if got := AccessSummary(m, json.RawMessage(raw)); got != want {
			t.Errorf("AccessSummary(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := AccessSummary(m, json.RawMessage(`not json`)); !strings.Contains(got, "Nothing") {
		t.Errorf("summary of junk = %q", got)
	}
}
