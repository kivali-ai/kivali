package integrationpod

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// A bundle's setup, requirements and uninstall: the hooks a built-in
// implements in Go (builtinintegrations.SetupIntegration,
// RequirementsOf, Uninstaller), as requests core sends a bundle's
// server whose manifest names method setup. Each goes through the
// shim as tools/call does, and is answered on the server's output.
//
//   - MethodSetup, SetupParams: one call of the flow, answered with
//     SetupResult. While it runs, the server may send
//     MethodProgress notifications carrying the request's
//     _meta.progressToken; core shows each as the flow's stage.
//   - MethodRequirements, empty params: the integration's own rows,
//     answered with RequirementsResult.
//   - MethodUninstall, empty params: undo the installation on the
//     service, answered with {}.
//
// An error is a JSON-RPC error whose message is the owner's words.
const (
	MethodSetup        = "kivali/setup"
	MethodRequirements = "kivali/requirements"
	MethodUninstall    = "kivali/uninstall"
	// MethodProgress is MCP's progress notification, which the shim
	// passes on to core.
	MethodProgress = "notifications/progress"
)

// SetupParams is MethodSetup's params.
type SetupParams struct {
	// Flow is "setup" for installing, or the Fix a requirement named.
	Flow   string      `json:"flow"`
	Answer SetupAnswer `json:"answer"`
	// Meta carries the progress token the server's progress
	// notifications name.
	Meta *RequestMeta `json:"_meta,omitempty"`
}

// RequestMeta is a request's _meta, as MCP has it.
type RequestMeta struct {
	ProgressToken string `json:"progressToken,omitempty"`
}

// SetupAnswer is builtinintegrations.Answer on the wire. Every map is
// an object, empty where the Answer has none, so a server reads a key
// without first asking whether the object is there.
type SetupAnswer struct {
	State map[string]string `json:"state"`
	Form  map[string]string `json:"form"`
	// Return is the query the service sent the browser back with, each
	// key's values as a list: untrusted.
	Return   map[string][]string `json:"return"`
	Checked  bool                `json:"checked"`
	Went     bool                `json:"went"`
	Leg      int                 `json:"leg"`
	Callback string              `json:"callback"`
	// Addresses is every address the team signs in at, each a bare
	// callback: a list, empty for none.
	Addresses []string `json:"addresses"`
	// Team is the team's name, for naming what setup makes on the
	// service; empty when the team has none.
	Team string `json:"team"`
}

// SetupAnswerOf is the Answer a, as kivali/setup's params carry it.
func SetupAnswerOf(a builtinintegrations.Answer) SetupAnswer {
	out := SetupAnswer{
		State: map[string]string{}, Form: map[string]string{}, Return: map[string][]string{},
		Checked: a.Checked, Went: a.Went, Leg: a.Leg, Callback: a.Callback, Team: a.Team,
		Addresses: append([]string{}, a.Addresses...),
	}
	for k, v := range a.State {
		out.State[k] = v
	}
	for k, v := range a.Form {
		out.Form[k] = v
	}
	for k, vs := range a.Return {
		out.Return[k] = append([]string{}, vs...)
	}
	return out
}

// SetupResult is MethodSetup's result.
type SetupResult struct {
	Step *SetupStep `json:"step"`
}

// SetupStep is builtinintegrations.Step on the wire: one of Form, Go,
// Wait or Done, and what to keep and carry.
type SetupStep struct {
	Form  *SetupForm        `json:"form,omitempty"`
	Go    *SetupGo          `json:"go,omitempty"`
	Wait  *SetupWait        `json:"wait,omitempty"`
	Done  bool              `json:"done,omitempty"`
	Keep  map[string]string `json:"keep,omitempty"`
	State map[string]string `json:"state,omitempty"`
}

// SetupForm is builtinintegrations.Form on the wire.
type SetupForm struct {
	Words  string   `json:"words"`
	Fields []string `json:"fields"`
}

// SetupGo is builtinintegrations.Go on the wire.
type SetupGo struct {
	Words string `json:"words"`
	URL   string `json:"url"`
	// Post is the form posted to URL: each key's value a string or a
	// list of strings.
	Post      PostValues `json:"post,omitempty"`
	NoReturn  bool       `json:"no_return,omitempty"`
	Stateless bool       `json:"stateless,omitempty"`
}

// SetupWait is builtinintegrations.Wait on the wire.
type SetupWait struct {
	Words string `json:"words"`
	URL   string `json:"url,omitempty"`
	Code  string `json:"code,omitempty"`
}

// PostValues is a Go step's form: a key's value is one string, or a
// list of strings for a key posted more than once.
type PostValues map[string][]string

// UnmarshalJSON reads each value as a string or a list of strings.
func (p *PostValues) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New("post is an object of strings, or lists of strings")
	}
	if raw == nil {
		*p = nil
		return nil
	}
	out := PostValues{}
	for k, v := range raw {
		var one string
		if json.Unmarshal(v, &one) == nil {
			out[k] = []string{one}
			continue
		}
		var many []string
		if err := json.Unmarshal(v, &many); err != nil || many == nil {
			return fmt.Errorf("post.%s is a string or a list of strings", k)
		}
		out[k] = many
	}
	*p = out
	return nil
}

// ParseSetupResult reads a MethodSetup result as a Step. A key the
// shapes do not have, or a value of the wrong type, is an error: a
// step Kivali cannot read is never guessed at. A result with no step
// is the zero Step, which the runner refuses as no next step.
func ParseSetupResult(raw json.RawMessage) (builtinintegrations.Step, error) {
	var res struct {
		Step json.RawMessage `json:"step"`
		Meta json.RawMessage `json:"_meta"`
	}
	if err := strictJSON(raw, &res); err != nil {
		return builtinintegrations.Step{}, err
	}
	if len(res.Step) == 0 || string(res.Step) == "null" {
		return builtinintegrations.Step{}, nil
	}
	var st SetupStep
	if err := strictJSON(res.Step, &st); err != nil {
		return builtinintegrations.Step{}, fmt.Errorf("step: %w", err)
	}
	out := builtinintegrations.Step{Done: st.Done, Keep: st.Keep, State: st.State}
	if st.Form != nil {
		out.Form = &builtinintegrations.Form{Words: st.Form.Words, Fields: st.Form.Fields}
	}
	if st.Go != nil {
		g := &builtinintegrations.Go{Words: st.Go.Words, URL: st.Go.URL, NoReturn: st.Go.NoReturn, Stateless: st.Go.Stateless}
		if st.Go.Post != nil {
			g.Post = url.Values(st.Go.Post)
		}
		out.Go = g
	}
	if st.Wait != nil {
		out.Wait = &builtinintegrations.Wait{Words: st.Wait.Words, URL: st.Wait.URL, Code: st.Wait.Code}
	}
	return out, nil
}

// RequirementsResult is MethodRequirements' result.
type RequirementsResult struct {
	Requirements []Requirement `json:"requirements"`
}

// Requirement is builtinintegrations.Requirement on the wire; a row
// gives every field but Fix (ParseRequirements).
type Requirement struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Words string `json:"words"`
	Met   bool   `json:"met"`
	Fix   string `json:"fix,omitempty"`
}

// ParseRequirements reads a MethodRequirements result, held to its
// shape as a step is.
func ParseRequirements(raw json.RawMessage) ([]builtinintegrations.Requirement, error) {
	// Every field but fix is required: a row missing its met would read
	// as unmet, and one missing its words as saying nothing.
	type row struct {
		ID    *string `json:"id"`
		Title *string `json:"title"`
		Words *string `json:"words"`
		Met   *bool   `json:"met"`
		Fix   string  `json:"fix"`
	}
	var res struct {
		Requirements *[]row          `json:"requirements"`
		Meta         json.RawMessage `json:"_meta"`
	}
	if err := strictJSON(raw, &res); err != nil {
		return nil, err
	}
	if res.Requirements == nil {
		return nil, errors.New("the answer has no list of requirements")
	}
	out := make([]builtinintegrations.Requirement, 0, len(*res.Requirements))
	for i, r := range *res.Requirements {
		if r.ID == nil || r.Title == nil || r.Words == nil || r.Met == nil {
			return nil, fmt.Errorf("requirements[%d] lacks one of id, title, words and met", i)
		}
		out = append(out, builtinintegrations.Requirement{ID: *r.ID, Title: *r.Title, Words: *r.Words, Met: *r.Met, Fix: r.Fix})
	}
	return out, nil
}

// ProgressParams is MethodProgress's params, as MCP has them.
type ProgressParams struct {
	// ProgressToken is the request's _meta.progressToken.
	ProgressToken json.RawMessage `json:"progressToken"`
	// Progress is how far along; Total how far there is to go, absent
	// when not known.
	Progress float64  `json:"progress"`
	Total    *float64 `json:"total,omitempty"`
	// Message is the stage in words.
	Message string `json:"message,omitempty"`
}

// strictJSON decodes raw into v, refusing a key v does not have and
// anything after the value.
func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one value")
	}
	return nil
}
