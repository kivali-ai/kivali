// Package integration is the contract of an integration bundle: the
// manifest a bundle carries, the check every bundle passes before it
// is stored (Validate), the access form a manifest describes and the
// reading of a bundle's zip. It knows nothing of where bundles are
// kept or how they are served: internal/store keeps them and
// internal/web serves them.
//
// A bundle is a zip of a directory: manifest.json at the root (or
// inside one top-level folder, which is stripped), the server's code,
// and optionally README.md and an icon. The manifest is MCPB's
// manifest.json with one more section, kivali, which says how the
// integration is connected, which hosts it reaches and what one
// agent's access looks like. docs/developers/integrations.md is the
// reference.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
)

// ManifestName is the manifest's file name at the bundle's root.
const ManifestName = "manifest.json"

// Manifest is a bundle's manifest.json. Fields Kivali does not read
// are tolerated and kept: the store keeps the manifest byte for byte.
// A built-in integration's manifest (internal/builtinintegrations) has
// the same shape with no server, and is written as this type marshals.
type Manifest struct {
	ManifestVersion string     `json:"manifest_version"`
	Name            string     `json:"name"`
	Version         string     `json:"version"`
	DisplayName     string     `json:"display_name"`
	Description     string     `json:"description"`
	Author          Author     `json:"author"`
	Server          Server     `json:"server,omitzero"`
	UserConfig      UserConfig `json:"user_config"`
	Tools           []Tool     `json:"tools"`
	Kivali          Kivali     `json:"kivali"`
}

// Author is who built the integration.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Server says what runs the integration and how it is started.
type Server struct {
	// Type is the runtime; Kivali runs "python".
	Type string `json:"type"`
	// EntryPoint is the file the server starts from, relative to the
	// bundle's root.
	EntryPoint string    `json:"entry_point"`
	MCPConfig  MCPConfig `json:"mcp_config"`
}

// MCPConfig is the command that starts the server, with its
// environment. An env value may name a connection field as
// ${user_config.<key>}, which is replaced by the field's value.
type MCPConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// Tool is one action the integration gives agents.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Kivali is the manifest's kivali section.
type Kivali struct {
	Connection Connection `json:"connection"`
	// Hosts is every host the integration reaches, in the egress
	// allowlist's pattern language.
	Hosts []string `json:"hosts"`
	// Packages is the Debian packages the integration's pod installs
	// each time it starts, before the server runs: a CLI the server
	// runs per call, say. A built-in integration declares none.
	Packages []string `json:"packages,omitempty"`
	// Access is the form one agent's grant fills in; nil means access
	// is all or nothing.
	Access *Access `json:"access,omitempty"`
	// Verify names the tool Test connection calls.
	Verify string `json:"verify,omitempty"`
	// SetupNotes is what to do on the vendor's side before connecting
	// (register an app, the scopes to tick), Markdown, shown above the
	// connection form.
	SetupNotes string `json:"setup_notes,omitempty"`
}

// The ways a connection is made (Connection.Method).
const (
	// MethodToken: the owner pastes the connection's fields.
	MethodToken = "token"
	// MethodDevice: the owner signs in with a code shown on the page
	// (the device authorization grant).
	MethodDevice = "device"
	// MethodBrowser: the owner signs in on the vendor's site, which
	// sends the browser back to Kivali (the authorization code grant).
	MethodBrowser = "browser"
	// MethodSetup: the integration's own setup flow makes the
	// connection, from the steps core runs (Form, Go, Wait), held to
	// what Connection.Setup declares.
	MethodSetup = "setup"
)

// Whose OAuth app signs a browser or device connection in
// (Connection.Client).
const (
	// ClientOperator: the owner registers an app with the vendor and
	// the connection form takes its client id and secret. Empty is the
	// same.
	ClientOperator = "operator"
	// ClientRegistered: core registers the team as a client with the
	// vendor's authorization server (RFC 7591), found from the metadata
	// at Connection.Discover (RFC 9728, RFC 8414). The owner registers
	// nothing and the form takes no client id or secret.
	ClientRegistered = "registered"
)

// The connection fields core adds to the form of a connection made by
// signing in: the OAuth app the owner registered with the vendor.
const (
	FieldClientID     = "client_id"
	FieldClientSecret = "client_secret"
)

// CredentialAccessToken is the one key of Connection.CredentialKeys:
// the access token's name in the answer an integration's call is
// given.
const CredentialAccessToken = "access_token"

// Connection says how the integration is connected.
type Connection struct {
	// Method is how the connection is made: MethodToken, MethodDevice
	// or MethodBrowser.
	Method string `json:"method"`
	// The vendor's OAuth endpoints, for a connection made by signing
	// in. Core talks to them; the integration never does.
	DeviceEndpoint        string `json:"device_endpoint,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	RevocationEndpoint    string `json:"revocation_endpoint,omitempty"`
	// Scopes are what a sign-in asks the vendor for.
	Scopes []string `json:"scopes,omitempty"`
	// PKCE says whether browser sign-in sends a PKCE challenge; nil is
	// true.
	PKCE *bool `json:"pkce,omitempty"`
	// Client is whose OAuth app signs in: ClientOperator (empty is the
	// same), the app the owner registers and whose id and secret the
	// connection form takes, or ClientRegistered, the client core
	// registers itself.
	Client string `json:"client,omitempty"`
	// Discover is, for ClientRegistered, the address whose metadata
	// names the authorization server: the protected resource's (RFC
	// 9728) or the authorization server's own (RFC 8414). Core reads the
	// endpoints there, so the manifest gives none.
	Discover string `json:"discover,omitempty"`
	// Hosts is, for ClientRegistered, every host the discovered
	// endpoints may be on: where the sign-in sends the owner's browser
	// and where core takes the tokens from. The install screen lists
	// them, and core refuses metadata naming any other.
	Hosts []string `json:"hosts,omitempty"`
	// Setup is, for MethodSetup, what the integration's setup flow may
	// do. The install screen lists it and core holds every step to it.
	Setup *Setup `json:"setup,omitempty"`
	// ExtraAuthorizationParams are added to the vendor's authorization
	// address as they are.
	ExtraAuthorizationParams map[string]string `json:"extra_authorization_params,omitempty"`
	// CredentialKeys names what the integration's calls are given:
	// access_token's value is the name the access token goes under.
	CredentialKeys map[string]string `json:"credential_keys,omitempty"`
}

// Setup is what a setup connection's flow may do
// (kivali.connection.setup): the declarations the install screen shows
// before anything runs, and the bounds core holds every step to. A
// bundle's setup and a built-in's are held to the same ones.
type Setup struct {
	// Summary is what setup does, in words for the owner ("Creates a
	// private GitHub App on your account that may write contents,
	// issues and pull requests").
	Summary string `json:"summary"`
	// Hosts is every host a Go step may send the browser to, or a Wait
	// step may name a page on. Lowercase, no scheme, port or path.
	Hosts []string `json:"hosts,omitempty"`
	// Fields is every field a Form step may ask for, in the order
	// declared.
	Fields SetupFields `json:"fields,omitempty"`
	// Keeps is every key setup may store in the connection's Secret, in
	// the order declared. The kept values are the connection's fields,
	// every one stored in the Secret, and secret unless declared not
	// (SetupKeep.NotSecret); a kept client_id and client_secret are the
	// app's, which core reads from the Secret for the sign-in Then
	// names.
	Keeps SetupKeeps `json:"keeps,omitempty"`
	// Then is the standard sign-in setup hands over to once it has
	// kept the app's client id and secret: MethodBrowser or
	// MethodDevice, with the vendor's endpoints beside it in the
	// connection; empty for none.
	Then string `json:"then,omitempty"`
}

// SetupField is one field a Form step may ask the owner for.
type SetupField struct {
	Key string
	// Title is what the field is called on the form; empty, the key is
	// shown in words.
	Title string
	// Words say what to enter, and where it comes from.
	Words string
	// Secret fields are entered as secrets and never shown again.
	Secret bool
	// SetupOnly fields live in the flow's memory alone and are never
	// kept: a key setup trades for something else.
	SetupOnly bool
	// notBool names "secret" or "setup_only" when the manifest gives
	// either a value that is not a boolean.
	notBool []string
}

// SetupFields is a setup's fields, in the manifest's order.
type SetupFields []SetupField

// Field returns the field with key, if the setup declares one.
func (s SetupFields) Field(key string) (SetupField, bool) {
	for _, f := range s {
		if f.Key == key {
			return f, true
		}
	}
	return SetupField{}, false
}

// UnmarshalJSON reads the fields object keeping its order.
func (s *SetupFields) UnmarshalJSON(b []byte) error {
	keys, values, err := orderedObject(b)
	if err != nil {
		return fmt.Errorf("fields: %w", err)
	}
	out := make(SetupFields, 0, len(keys))
	for _, k := range keys {
		var aux struct {
			Title     string          `json:"title"`
			Words     string          `json:"words"`
			Secret    json.RawMessage `json:"secret"`
			SetupOnly json.RawMessage `json:"setup_only"`
		}
		if err := json.Unmarshal(values[k], &aux); err != nil {
			return fmt.Errorf("fields.%s: %w", k, err)
		}
		f := SetupField{Key: k, Title: aux.Title, Words: aux.Words}
		for _, b := range []struct {
			name string
			raw  json.RawMessage
			dst  *bool
		}{{"secret", aux.Secret, &f.Secret}, {"setup_only", aux.SetupOnly, &f.SetupOnly}} {
			if len(b.raw) == 0 {
				continue
			}
			if err := json.Unmarshal(b.raw, b.dst); err != nil {
				f.notBool = append(f.notBool, b.name)
			}
		}
		out = append(out, f)
	}
	*s = out
	return nil
}

// MarshalJSON writes the fields back as an object in their order.
func (s SetupFields) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range s {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(f.Key)
		v, err := json.Marshal(struct {
			Title     string `json:"title,omitempty"`
			Words     string `json:"words"`
			Secret    bool   `json:"secret,omitempty"`
			SetupOnly bool   `json:"setup_only,omitempty"`
		}{f.Title, f.Words, f.Secret, f.SetupOnly})
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// SetupKeep is one key setup may store in the connection's Secret, with
// words for the owner. A kept value is secret unless NotSecret says
// otherwise: an identifier the service shows publicly (an app's id, its
// name in the service's addresses), which the owner may read in a
// step's words and an agent in an answer. Every kept value is stored in
// the Secret all the same.
type SetupKeep struct {
	Key   string
	Words string
	// NotSecret is "secret": false in the manifest's object form: the
	// value is not replaced with [secret] in what the owner, an agent or
	// the log reads.
	NotSecret bool
	// bad names what of the object form is not of its type ("words",
	// "secret") or a key the form does not have, for the check.
	bad []string
}

// SetupKeeps is a setup's keeps, in the manifest's order.
type SetupKeeps []SetupKeep

// Has reports whether key is one setup may keep.
func (s SetupKeeps) Has(key string) bool {
	return slices.ContainsFunc(s, func(k SetupKeep) bool { return k.Key == key })
}

// Secret reports whether key is one setup may keep whose value is
// secret: declared, and not declared "secret": false.
func (s SetupKeeps) Secret(key string) bool {
	return slices.ContainsFunc(s, func(k SetupKeep) bool { return k.Key == key && !k.NotSecret })
}

// Keys is every key, in order.
func (s SetupKeeps) Keys() []string {
	out := make([]string, 0, len(s))
	for _, k := range s {
		out = append(out, k.Key)
	}
	return out
}

// UnmarshalJSON reads the keeps object, keeping its order. Each value
// is the key's words (a secret keep), or an object of its words and
// whether it is secret ({"words": "...", "secret": false}); what of the
// object is not of its type is left for the check to name.
func (s *SetupKeeps) UnmarshalJSON(b []byte) error {
	keys, values, err := orderedObject(b)
	if err != nil {
		return fmt.Errorf("keeps: %w", err)
	}
	out := make(SetupKeeps, 0, len(keys))
	for _, k := range keys {
		raw := bytes.TrimSpace(values[k])
		var words string
		if err := json.Unmarshal(raw, &words); err == nil {
			out = append(out, SetupKeep{Key: k, Words: words})
			continue
		}
		var obj map[string]json.RawMessage
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil {
			return fmt.Errorf("keeps.%s: the value is the key's words, or an object of its words and whether it is secret", k)
		}
		keep := SetupKeep{Key: k}
		for _, name := range sortedKeys(obj) {
			v := bytes.TrimSpace(obj[name])
			switch name {
			case "words":
				if bytes.Equal(v, []byte("null")) || json.Unmarshal(v, &keep.Words) != nil {
					keep.bad = append(keep.bad, "words")
				}
			case "secret":
				var secret bool
				if bytes.Equal(v, []byte("null")) || json.Unmarshal(v, &secret) != nil {
					keep.bad = append(keep.bad, "secret")
					secret = true
				}
				keep.NotSecret = !secret
			default:
				keep.bad = append(keep.bad, name)
			}
		}
		out = append(out, keep)
	}
	*s = out
	return nil
}

// MarshalJSON writes the keeps back as an object in their order: a
// secret keep as its words, one that is not as {"words": ..., "secret":
// false}.
func (s SetupKeeps) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range s {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(k.Key)
		var v []byte
		if k.NotSecret {
			v, _ = json.Marshal(struct {
				Words  string `json:"words"`
				Secret bool   `json:"secret"`
			}{k.Words, false})
		} else {
			v, _ = json.Marshal(k.Words)
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// SignIn reports whether the connection is made by signing in (device
// or browser) rather than with a pasted token or a setup flow.
func (c Connection) SignIn() bool { return c.Method == MethodDevice || c.Method == MethodBrowser }

// OAuth reports whether core holds OAuth tokens for the connection and
// renews them: a sign-in connection, or a setup connection whose flow
// hands over to a sign-in (Setup.Then).
func (c Connection) OAuth() bool { return c.SignIn() || c.ThenMethod() != "" }

// ThenMethod is the sign-in a setup connection hands over to
// (Setup.Then): MethodBrowser or MethodDevice, or "" for a connection
// that is not a setup one or whose setup keeps the connection itself.
func (c Connection) ThenMethod() string {
	if c.Method != MethodSetup || c.Setup == nil {
		return ""
	}
	return c.Setup.Then
}

// Registered reports whether core registers itself as the vendor's
// client (ClientRegistered) rather than the owner pasting an app's id
// and secret.
func (c Connection) Registered() bool { return c.Client == ClientRegistered }

// Ways is how the owner may sign in, the manifest's method first: a
// device connection that also gives an authorization endpoint offers
// browser too, and a browser one that gives a device endpoint offers
// device. Empty for a token connection.
func (c Connection) Ways() []string {
	switch c.Method {
	case MethodDevice:
		if c.AuthorizationEndpoint != "" {
			return []string{MethodDevice, MethodBrowser}
		}
		return []string{MethodDevice}
	case MethodBrowser:
		if c.DeviceEndpoint != "" {
			return []string{MethodBrowser, MethodDevice}
		}
		return []string{MethodBrowser}
	}
	return []string{}
}

// SignInHosts are the hosts of the connection's OAuth endpoints (the
// authorization, token, device and revocation endpoints), and for a
// registered client of its discover address and its Hosts, lower-cased,
// each once, sorted: where a sign-in sends the owner's browser and
// where core takes the tokens from. Empty for a token connection and
// for a setup connection that signs nothing in; a setup flow's own
// hosts are Setup.Hosts.
func (c Connection) SignInHosts() []string {
	out := []string{}
	for _, ep := range []string{c.AuthorizationEndpoint, c.TokenEndpoint, c.DeviceEndpoint, c.RevocationEndpoint, c.Discover} {
		if h := EndpointHost(ep); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, h := range c.Hosts {
		if h = strings.ToLower(h); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return out
}

// EndpointHost is the host (and port, when it names one) of an endpoint
// address, lower-cased; "" for none or an address that is not one.
func EndpointHost(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// UsesPKCE reports whether browser sign-in sends a PKCE challenge.
func (c Connection) UsesPKCE() bool { return c.PKCE == nil || *c.PKCE }

// CredentialKey is the name the access token goes under in a call's
// answer; empty for a token connection.
func (c Connection) CredentialKey() string { return c.CredentialKeys[CredentialAccessToken] }

// ConnectionFields is the connection form: the manifest's user_config
// followed, for a connection made by signing in with an app the owner
// registers, by the two fields core adds, the client id and the client
// secret. It is what the page draws, a save writes and a call reads. A
// registered client's form takes neither (core registers itself), and a
// setup connection's form is empty: its fields are what its flow asks
// for and keeps (ConnectionKeys).
func ConnectionFields(m *Manifest) UserConfig {
	if m == nil {
		return nil
	}
	out := append(UserConfig{}, m.UserConfig...)
	if c := m.Kivali.Connection; c.SignIn() && !c.Registered() {
		out = append(out,
			UserConfigField{Key: FieldClientID, Type: "string", Title: "Client id", Required: true},
			UserConfigField{Key: FieldClientSecret, Type: "string", Title: "Client secret", Sensitive: true, Required: true},
		)
	}
	return out
}

// ConnectionKeySecret reports whether key, one of ConnectionKeys, is
// kept in the connection's Secret: a sensitive form field, or a key a
// setup flow keeps, secret or not (every kept value is stored there).
func ConnectionKeySecret(m *Manifest, key string) bool {
	if m == nil {
		return false
	}
	if f, ok := ConnectionFields(m).Field(key); ok && f.Sensitive {
		return true
	}
	c := m.Kivali.Connection
	return c.Method == MethodSetup && c.Setup != nil && c.Setup.Keeps.Has(key)
}

// ConnectionKeyScrubbed reports whether key's value, key one of
// ConnectionKeys, is replaced with [secret] wherever the owner, an agent
// or the log reads it: a sensitive form field, or a key a setup flow
// keeps that is not declared "secret": false.
func ConnectionKeyScrubbed(m *Manifest, key string) bool {
	if m == nil {
		return false
	}
	if f, ok := ConnectionFields(m).Field(key); ok && f.Sensitive {
		return true
	}
	c := m.Kivali.Connection
	return c.Method == MethodSetup && c.Setup != nil && c.Setup.Keeps.Secret(key)
}

// ConnectionKeys is every key a connection's values may hold, as a
// call reads them: the connection form's fields (ConnectionFields)
// and, for a setup connection, the keys its flow keeps.
func ConnectionKeys(m *Manifest) []string {
	if m == nil {
		return nil
	}
	out := []string{}
	for _, f := range ConnectionFields(m) {
		out = append(out, f.Key)
	}
	if c := m.Kivali.Connection; c.Method == MethodSetup && c.Setup != nil {
		for _, k := range c.Setup.Keeps.Keys() {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// Access is the access form: a JSON Schema of one object whose
// properties are the form's fields (see access.go for what a field may
// be), and the fields whose options the integration lists itself.
type Access struct {
	Schema json.RawMessage `json:"schema"`
	// Choices maps a field's key to the tool that lists its options.
	Choices map[string]string `json:"choices,omitempty"`
}

// UserConfig is the manifest's user_config, the connection's fields,
// in the order the bundle lists them.
type UserConfig []UserConfigField

// UserConfigField is one field of the connection.
type UserConfigField struct {
	Key         string
	Type        string
	Title       string
	Description string
	// Sensitive fields are secrets.
	Sensitive bool
	Required  bool
	// notBool names "sensitive" or "required" when the manifest gives
	// either a value that is not a boolean.
	notBool []string
}

// Field returns the field with key, if the manifest has one.
func (u UserConfig) Field(key string) (UserConfigField, bool) {
	for _, f := range u {
		if f.Key == key {
			return f, true
		}
	}
	return UserConfigField{}, false
}

// UnmarshalJSON reads user_config's object keeping its order.
func (u *UserConfig) UnmarshalJSON(b []byte) error {
	keys, values, err := orderedObject(b)
	if err != nil {
		return fmt.Errorf("user_config: %w", err)
	}
	out := make(UserConfig, 0, len(keys))
	for _, k := range keys {
		var f UserConfigField
		if err := f.unmarshal(values[k]); err != nil {
			return fmt.Errorf("user_config.%s: %w", k, err)
		}
		f.Key = k
		out = append(out, f)
	}
	*u = out
	return nil
}

// MarshalJSON writes user_config back as an object in its order.
func (u UserConfig) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range u {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(f.Key)
		v, err := json.Marshal(struct {
			Type        string `json:"type"`
			Title       string `json:"title"`
			Description string `json:"description,omitempty"`
			Sensitive   bool   `json:"sensitive,omitempty"`
			Required    bool   `json:"required,omitempty"`
		}{f.Type, f.Title, f.Description, f.Sensitive, f.Required})
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (f *UserConfigField) unmarshal(raw json.RawMessage) error {
	var aux struct {
		Type        string          `json:"type"`
		Title       string          `json:"title"`
		Description string          `json:"description"`
		Sensitive   json.RawMessage `json:"sensitive"`
		Required    json.RawMessage `json:"required"`
	}
	if err := json.Unmarshal(raw, &aux); err != nil {
		return err
	}
	f.Type, f.Title, f.Description = aux.Type, aux.Title, aux.Description
	for _, b := range []struct {
		name string
		raw  json.RawMessage
		dst  *bool
	}{{"sensitive", aux.Sensitive, &f.Sensitive}, {"required", aux.Required, &f.Required}} {
		if len(b.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(b.raw, b.dst); err != nil {
			f.notBool = append(f.notBool, b.name)
		}
	}
	return nil
}

// ParseManifest reads a manifest.json. Malformed JSON, or a field of
// the wrong JSON type, is an error; what the manifest says is checked
// by Validate.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", ManifestName, err)
	}
	return &m, nil
}

// errDuplicateKey is orderedObject's refusal of an object that names
// a key twice: which value wins would depend on the reader.
var errDuplicateKey = errors.New("names a key twice")

// orderedObject reads a JSON object's keys in order with their raw
// values. null reads as an empty object.
func orderedObject(b []byte) ([]string, map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if t := bytes.TrimSpace(b); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, values, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("is not an object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, nil, errors.New("is not an object")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := values[k]; dup {
			return nil, nil, fmt.Errorf("%w: %s", errDuplicateKey, k)
		}
		keys = append(keys, k)
		values[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("has content after the object")
	}
	return keys, values, nil
}
