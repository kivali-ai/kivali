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
	// Access is the form one agent's grant fills in; nil means access
	// is all or nothing.
	Access *Access `json:"access,omitempty"`
	// Verify names the tool Test connection calls.
	Verify string `json:"verify,omitempty"`
}

// Connection says how the integration is connected.
type Connection struct {
	// Method is "token": the owner pastes the connection's fields.
	Method string `json:"method"`
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
