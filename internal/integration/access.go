package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The access form. A manifest's kivali.access.schema is a JSON Schema
// of one object, held to what a flat form can show: each property is
// a string (with an optional enum of strings), a boolean, or an array
// of strings (with an optional items.enum). Each takes a title and a
// description, and nothing else: no nesting, no numbers, no
// combinators, no constraints the form would not show. A boolean may
// also take "x-kivali-danger": true, which the form shows as a
// dangerous choice, and any property "x-kivali-section": a heading
// the form draws it under, with the fields after it that name the
// same one. A manifest without kivali.access grants all or nothing.

// FieldKind is what kind of value an access field takes.
type FieldKind string

const (
	// FieldText is one string.
	FieldText FieldKind = "text"
	// FieldChoice is one string from Options, or from the options the
	// integration lists (Choices).
	FieldChoice FieldKind = "choice"
	// FieldSwitch is a boolean.
	FieldSwitch FieldKind = "switch"
	// FieldList is a list of strings, free text.
	FieldList FieldKind = "list"
	// FieldChoices is a list of strings from Options, or from the
	// options the integration lists (Choices).
	FieldChoices FieldKind = "choices"
)

// Form is the access form, field by field in the schema's order.
type Form struct {
	Fields []Field
}

// Field is one field of the access form.
type Field struct {
	Key         string
	Title       string
	Description string
	Kind        FieldKind
	Required    bool
	// Options are a choice or choices field's fixed options, from the
	// schema's enum. Empty for the other kinds and for a field the
	// integration lists the options of.
	Options []string
	// Choices is true when the integration lists the field's options
	// itself, through the tool kivali.access.choices names.
	Choices bool
	// Danger is true for a switch the schema marks "x-kivali-danger":
	// turning it on is a choice the owner makes with care.
	Danger bool
	// Section is the heading the form draws the field under, from
	// the schema's "x-kivali-section"; "" is none. Fields in a row
	// with the same section share one heading.
	Section string
}

// propType is a property's JSON type.
type propType string

const (
	propString  propType = "string"
	propBoolean propType = "boolean"
	propArray   propType = "array"
)

// property is one parsed property of an access schema.
type property struct {
	key, title, description string
	typ                     propType
	// enum is a string's enum or an array's items.enum.
	enum []string
	// danger is a boolean's x-kivali-danger.
	danger bool
	// section is the property's x-kivali-section, trimmed.
	section string
}

// schema is a parsed access schema.
type schema struct {
	props    []property
	required []string
}

func (s *schema) prop(key string) (property, bool) {
	for _, p := range s.props {
		if p.key == key {
			return p, true
		}
	}
	return property{}, false
}

// label is the property's title, or its key when it has none.
func (p property) label() string {
	if p.title != "" {
		return p.title
	}
	return p.key
}

const schemaPath = "kivali.access.schema"

// dangerKeyword marks a boolean property as a dangerous choice.
const dangerKeyword = "x-kivali-danger"

// sectionKeyword names the heading a property is drawn under.
const sectionKeyword = "x-kivali-section"

var (
	schemaKeywords   = []string{"$schema", "additionalProperties", "description", "properties", "required", "title", "type"}
	propertyKeywords = []string{"description", "enum", "items", "title", "type", dangerKeyword, sectionKeyword}
	itemsKeywords    = []string{"enum", "type"}
)

// parseSchema reads an access schema, with a finding for everything it
// does not accept. A property that does not parse is left out of the
// schema it returns.
func parseSchema(raw json.RawMessage) (*schema, Findings) {
	var f Findings
	keys, top, err := orderedObject(raw)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		f.errorf("%s must be a JSON Schema of an object", schemaPath)
		return &schema{}, f
	}
	for _, k := range keys {
		if !slices.Contains(schemaKeywords, k) {
			f.errorf("%s: %s is not supported; the access form is one object of fields", schemaPath, k)
		}
	}
	var typ string
	if err := json.Unmarshal(top["type"], &typ); err != nil || typ != "object" {
		f.errorf("%s: type must be object", schemaPath)
	}
	if ap, ok := top["additionalProperties"]; ok && !bytes.Equal(bytes.TrimSpace(ap), []byte("false")) {
		f.errorf("%s: additionalProperties may only be false", schemaPath)
	}
	s := &schema{}
	propKeys, props, err := orderedObject(top["properties"])
	if err != nil {
		f.errorf("%s: properties must be an object", schemaPath)
	}
	for _, k := range propKeys {
		p, pf := parseProperty(k, props[k])
		f = append(f, pf...)
		if !pf.HasErrors() {
			s.props = append(s.props, p)
		}
	}
	if req, ok := top["required"]; ok {
		if err := json.Unmarshal(req, &s.required); err != nil {
			f.errorf("%s: required must be a list of property names", schemaPath)
			s.required = nil
		}
	}
	for _, r := range s.required {
		if !slices.Contains(propKeys, r) {
			f.errorf("%s: required names %s, which is not a property", schemaPath, r)
		}
	}
	return s, f
}

// parseProperty reads one property of the schema.
func parseProperty(key string, raw json.RawMessage) (property, Findings) {
	var f Findings
	at := schemaPath + ".properties." + key
	p := property{key: key}
	keys, kw, err := orderedObject(raw)
	if err != nil {
		f.errorf("%s must be an object", at)
		return p, f
	}
	for _, k := range keys {
		if !slices.Contains(propertyKeywords, k) {
			f.errorf("%s: %s is not supported; a field takes type, title, description, enum, items, %s and %s", at, k, dangerKeyword, sectionKeyword)
		}
	}
	if t, ok := kw["title"]; ok {
		if err := json.Unmarshal(t, &p.title); err != nil {
			f.errorf("%s: title must be text", at)
		}
	}
	if d, ok := kw["description"]; ok {
		if err := json.Unmarshal(d, &p.description); err != nil {
			f.errorf("%s: description must be text", at)
		}
	}
	if s, ok := kw[sectionKeyword]; ok {
		if err := json.Unmarshal(s, &p.section); err != nil || strings.TrimSpace(p.section) == "" {
			f.errorf("%s: %s must be a heading, as text", at, sectionKeyword)
		}
		p.section = strings.TrimSpace(p.section)
	}
	var typ string
	if err := json.Unmarshal(kw["type"], &typ); err != nil {
		f.errorf("%s: type must be string, boolean or array", at)
		return p, f
	}
	p.typ = propType(typ)
	if _, ok := kw[dangerKeyword]; ok && p.typ != propBoolean {
		f.errorf("%s: %s marks a switch; it goes on a boolean field", at, dangerKeyword)
	}
	switch p.typ {
	case propString:
		if _, ok := kw["items"]; ok {
			f.errorf("%s: items belongs to an array field", at)
		}
		if e, ok := kw["enum"]; ok {
			p.enum = parseEnum(at+".enum", e, &f)
		}
	case propBoolean:
		for _, k := range []string{"enum", "items"} {
			if _, ok := kw[k]; ok {
				f.errorf("%s: a boolean field takes no %s", at, k)
			}
		}
		if d, ok := kw[dangerKeyword]; ok {
			if err := json.Unmarshal(d, &p.danger); err != nil {
				f.errorf("%s: %s must be true or false", at, dangerKeyword)
			}
		}
	case propArray:
		if _, ok := kw["enum"]; ok {
			f.errorf("%s: an array field's options go in items.enum", at)
		}
		ikeys, items, err := orderedObject(kw["items"])
		if err != nil || len(ikeys) == 0 {
			f.errorf("%s: an array field needs items of type string", at)
			break
		}
		for _, k := range ikeys {
			if !slices.Contains(itemsKeywords, k) {
				f.errorf("%s.items: %s is not supported; items take type and enum", at, k)
			}
		}
		var it string
		if err := json.Unmarshal(items["type"], &it); err != nil || it != "string" {
			f.errorf("%s.items: type must be string; an array field is a list of text", at)
		}
		if e, ok := items["enum"]; ok {
			p.enum = parseEnum(at+".items.enum", e, &f)
		}
	default:
		f.errorf("%s: type %s is not supported; a field is a string, a boolean or an array of strings", at, typ)
	}
	if p.title == "" && !f.HasErrors() {
		f.warnf("%s has no title; the form shows %s", at, key)
	}
	return p, f
}

// parseEnum reads an enum: a non-empty list of distinct strings.
func parseEnum(at string, raw json.RawMessage, f *Findings) []string {
	var e []string
	if err := json.Unmarshal(raw, &e); err != nil || len(e) == 0 {
		f.errorf("%s must be a non-empty list of text", at)
		return nil
	}
	seen := map[string]bool{}
	for _, v := range e {
		if seen[v] {
			f.errorf("%s lists %q twice", at, v)
		}
		seen[v] = true
	}
	return e
}

// accessSchema is m's parsed access schema, nil when access is all or
// nothing.
func accessSchema(m *Manifest) *schema {
	if m == nil || m.Kivali.Access == nil {
		return nil
	}
	s, _ := parseSchema(m.Kivali.Access.Schema)
	return s
}

// AccessForm is the access form m describes, nil when access is all or
// nothing.
func AccessForm(m *Manifest) *Form {
	s := accessSchema(m)
	if s == nil {
		return nil
	}
	form := &Form{Fields: make([]Field, 0, len(s.props))}
	for _, p := range s.props {
		_, listed := m.Kivali.Access.Choices[p.key]
		fd := Field{
			Key:         p.key,
			Title:       p.label(),
			Description: p.description,
			Required:    slices.Contains(s.required, p.key),
			Options:     []string{},
			Choices:     listed,
			Section:     p.section,
		}
		switch {
		case p.typ == propBoolean:
			fd.Kind = FieldSwitch
			fd.Danger = p.danger
		case p.typ == propString && (len(p.enum) > 0 || listed):
			fd.Kind = FieldChoice
		case p.typ == propString:
			fd.Kind = FieldText
		case len(p.enum) > 0 || listed:
			fd.Kind = FieldChoices
		default:
			fd.Kind = FieldList
		}
		if !listed && len(p.enum) > 0 {
			fd.Options = append(fd.Options, p.enum...)
		}
		form.Fields = append(form.Fields, fd)
	}
	return form
}

// AccessError is ValidateAccess's refusal: the field it is about (empty
// for the access as a whole) and what is wrong, in words.
type AccessError struct {
	Key     string
	Message string
}

func (e *AccessError) Error() string { return e.Message }

// isEmptyObject reports whether raw is absent, null or {}.
func isEmptyObject(raw json.RawMessage) bool {
	keys, _, err := orderedObject(raw)
	return err == nil && len(keys) == 0
}

// ValidateAccess checks one agent's filled access form against m's.
// Without a form (all or nothing) only an empty object is valid. With
// one, every key must be a field, every value of its field's kind and
// from its options, and every required field filled: a required text
// or list is not filled by "" or []. A field whose options the
// integration lists itself is checked for its kind only.
func ValidateAccess(m *Manifest, raw json.RawMessage) error {
	s := accessSchema(m)
	if s == nil {
		if !isEmptyObject(raw) {
			return &AccessError{Message: "this integration's access is all or nothing, so it takes no fields"}
		}
		return nil
	}
	keys, values, err := orderedObject(raw)
	if err != nil {
		return &AccessError{Message: "the access must be an object of field values"}
	}
	unknown := []string{}
	for _, k := range keys {
		if _, ok := s.prop(k); !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return &AccessError{Key: unknown[0], Message: unknown[0] + " is not a field of this integration's access form"}
	}
	for _, p := range s.props {
		v, present := values[p.key]
		if present && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			present = false
		}
		filled := false
		if present {
			var err *AccessError
			filled, err = checkValue(m, p, v)
			if err != nil {
				return err
			}
		}
		if !filled && slices.Contains(s.required, p.key) {
			return &AccessError{Key: p.key, Message: p.label() + " is required"}
		}
	}
	return nil
}

// checkValue checks one field's value; filled is false for "" and [].
func checkValue(m *Manifest, p property, v json.RawMessage) (filled bool, _ *AccessError) {
	_, listed := m.Kivali.Access.Choices[p.key]
	fail := func(msg string) (bool, *AccessError) { return false, &AccessError{Key: p.key, Message: msg} }
	switch p.typ {
	case propBoolean:
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fail(p.label() + " must be on or off")
		}
		return true, nil
	case propString:
		var str string
		if err := json.Unmarshal(v, &str); err != nil {
			return fail(p.label() + " must be text")
		}
		if str != "" && !listed && len(p.enum) > 0 && !slices.Contains(p.enum, str) {
			return fail(fmt.Sprintf("%s must be one of %s", p.label(), strings.Join(p.enum, ", ")))
		}
		return str != "", nil
	default:
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			return fail(p.label() + " must be a list of text")
		}
		if !listed && len(p.enum) > 0 {
			for _, item := range list {
				if !slices.Contains(p.enum, item) {
					return fail(fmt.Sprintf("%s lists %s, which is not one of %s", p.label(), item, strings.Join(p.enum, ", ")))
				}
			}
		}
		return len(list) > 0, nil
	}
}

// Summary words for AccessSummary.
const (
	// SummaryFullAccess is the summary of access that is all or nothing.
	SummaryFullAccess = "Full access"
	// SummaryNothingSet is the summary of a form with no field filled.
	SummaryNothingSet = "Nothing set"
)

// AccessSummary is one agent's access in words, field by field in the
// form's order and joined by " · ": a switch that is on by its title,
// a text as "Title: value", a list as "Title: a, b". A switch that is
// off and an empty field are left out. Access without a form is "Full
// access". It reads what it can of access that no longer fits the
// form.
func AccessSummary(m *Manifest, access json.RawMessage) string {
	s := accessSchema(m)
	if s == nil {
		return SummaryFullAccess
	}
	_, values, err := orderedObject(access)
	if err != nil {
		return SummaryNothingSet
	}
	var parts []string
	for _, p := range s.props {
		v, ok := values[p.key]
		if !ok {
			continue
		}
		switch p.typ {
		case propBoolean:
			var b bool
			if json.Unmarshal(v, &b) == nil && b {
				parts = append(parts, p.label())
			}
		case propString:
			var str string
			if json.Unmarshal(v, &str) == nil && str != "" {
				parts = append(parts, p.label()+": "+str)
			}
		default:
			var list []string
			if json.Unmarshal(v, &list) == nil && len(list) > 0 {
				parts = append(parts, p.label()+": "+strings.Join(list, ", "))
			}
		}
	}
	if len(parts) == 0 {
		return SummaryNothingSet
	}
	return strings.Join(parts, " · ")
}
