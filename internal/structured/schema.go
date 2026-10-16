// Package structured is the shape a subagent's answer takes when its
// caller asked for one: the JSON Schema subset a task may carry, the
// wrapper every such answer fits, and the validation of a submit_result
// call against both.
//
// Pure functions with no I/O. The subagent layer in internal/web calls
// them for every task kind: at dispatch to refuse a schema outside the
// subset, and on every submit_result to decide whether the answer is
// the caller's to read. The MCP side uses ToolInputSchema to show the
// subagent the exact shape, and nothing else: core validates, the tool
// only relays.
//
// The subset is small on purpose. Every string carries a maximum
// length, every object is closed, and the whole submission has a size
// ceiling, so what a subagent can hand back is bounded by what its
// caller asked for. That bound is what keeps text a web page planted
// in a subagent's context from reaching the calling agent as prose.
package structured

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Limits on a schema and on what fits it. A schema beyond them is
// refused at dispatch with the limit named, so the agent can fix it in
// the same turn.
const (
	// MaxSchemaBytes bounds the schema text itself.
	MaxSchemaBytes = 16 << 10
	// MaxSchemaDepth bounds nesting: a property of an item of a
	// property counts three.
	MaxSchemaDepth = 6
	// MaxSchemaProperties bounds the properties across the whole schema.
	MaxSchemaProperties = 100
	// MaxStringLength is the largest maxLength a string may ask for.
	// Longer text belongs in a file the result names.
	MaxStringLength = 4000
	// MaxEnumValues bounds an enum's values.
	MaxEnumValues = 50
	// MaxURLLength bounds a string with format uri and an
	// alternative's url: room for a long page address with its query,
	// not for a page of text.
	MaxURLLength = 1024
)

// Kind is a schema node's type.
type Kind string

const (
	KindObject  Kind = "object"
	KindArray   Kind = "array"
	KindString  Kind = "string"
	KindNumber  Kind = "number"
	KindInteger Kind = "integer"
	KindBoolean Kind = "boolean"
)

// String formats a string may declare instead of a maxLength: the
// format fixes the length.
const (
	FormatDate     = "date"      // YYYY-MM-DD
	FormatDateTime = "date-time" // RFC 3339
	FormatURI      = "uri"       // an absolute URL with a scheme and a host
)

// Schema is one parsed node of the subset. Build it with Parse; the
// zero value validates nothing.
type Schema struct {
	Kind        Kind
	Description string

	// Properties and Required describe an object. Every object is
	// closed: a value with a key not in Properties is invalid.
	Properties map[string]*Schema
	Required   []string

	// Items, MinItems and MaxItems describe an array. MaxItems 0
	// means no cap.
	Items    *Schema
	MinItems int
	MaxItems int

	// MaxLength, MinLength, Enum and Format describe a string. One of
	// MaxLength, Enum or Format is always set.
	MaxLength int
	MinLength int
	Enum      []string
	Format    string

	// Minimum and Maximum bound a number or integer when set.
	Minimum *float64
	Maximum *float64

	// Raw is the schema as the caller wrote it, compacted. Embedded in
	// the submit_result tool's input schema so the subagent sees the
	// shape the caller wrote, descriptions included.
	Raw json.RawMessage
}

// annotationKeys are accepted on every node and ignored by
// validation: they are for the subagent reading the tool schema.
var annotationKeys = map[string]bool{"type": true, "title": true, "description": true, "examples": true}

// keysByKind are the structural keywords each kind may carry.
var keysByKind = map[Kind]map[string]bool{
	KindObject:  {"properties": true, "required": true, "additionalProperties": true},
	KindArray:   {"items": true, "maxItems": true, "minItems": true},
	KindString:  {"maxLength": true, "minLength": true, "enum": true, "format": true},
	KindNumber:  {"minimum": true, "maximum": true},
	KindInteger: {"minimum": true, "maximum": true},
	KindBoolean: {},
}

// Parse reads a schema and refuses anything outside the subset. The
// top level must be an object. The error names the path of the first
// problem and what to change.
func Parse(raw json.RawMessage) (*Schema, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, fmt.Errorf("schema is empty")
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("schema is %d bytes; the limit is %d", len(raw), MaxSchemaBytes)
	}
	p := &parser{}
	s, err := p.node(raw, "", 1)
	if err != nil {
		return nil, err
	}
	if s.Kind != KindObject {
		return nil, fmt.Errorf("schema: the top level must be an object with properties, not %s", s.Kind)
	}
	return s, nil
}

type parser struct{ properties int }

func (p *parser) node(raw json.RawMessage, path string, depth int) (*Schema, error) {
	at := func(format string, args ...any) error {
		where := "schema"
		if path != "" {
			where = "schema at " + path
		}
		return fmt.Errorf("%s: %s", where, fmt.Sprintf(format, args...))
	}
	if depth > MaxSchemaDepth {
		return nil, at("nested deeper than %d levels", MaxSchemaDepth)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, at("must be a JSON object describing a type")
	}
	s := &Schema{}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, at("is not valid JSON")
	}
	s.Raw = json.RawMessage(compact.Bytes())

	typeRaw, ok := fields["type"]
	if !ok {
		return nil, at(`"type" is required (object, array, string, number, integer or boolean)`)
	}
	var typ string
	if err := json.Unmarshal(typeRaw, &typ); err != nil {
		return nil, at(`"type" must be one name, not a list`)
	}
	s.Kind = Kind(typ)
	allowed, known := keysByKind[s.Kind]
	if !known {
		return nil, at("type %q is not in the subset (object, array, string, number, integer, boolean)", typ)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if annotationKeys[key] || allowed[key] {
			continue
		}
		return nil, at("%q is not supported for %s", key, s.Kind)
	}
	if d, ok := fields["description"]; ok {
		if err := json.Unmarshal(d, &s.Description); err != nil {
			return nil, at(`"description" must be a string`)
		}
	}

	switch s.Kind {
	case KindObject:
		return p.object(s, fields, path, depth, at)
	case KindArray:
		return p.array(s, fields, path, depth, at)
	case KindString:
		return parseString(s, fields, at)
	case KindNumber, KindInteger:
		return parseNumber(s, fields, at)
	default:
		return s, nil
	}
}

type errAt func(format string, args ...any) error

func (p *parser) object(s *Schema, fields map[string]json.RawMessage, path string, depth int, at errAt) (*Schema, error) {
	propsRaw, ok := fields["properties"]
	if !ok {
		return nil, at(`an object needs "properties"`)
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(propsRaw, &props); err != nil || len(props) == 0 {
		return nil, at(`"properties" must be a non-empty object of name to type`)
	}
	if ap, ok := fields["additionalProperties"]; ok && strings.TrimSpace(string(ap)) != "false" {
		return nil, at(`"additionalProperties" may only be false; every object is closed`)
	}
	s.Properties = make(map[string]*Schema, len(props))
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, at("a property name is empty")
		}
		p.properties++
		if p.properties > MaxSchemaProperties {
			return nil, at("more than %d properties in all", MaxSchemaProperties)
		}
		child, err := p.node(props[name], joinPath(path, name), depth+1)
		if err != nil {
			return nil, err
		}
		s.Properties[name] = child
	}
	if reqRaw, ok := fields["required"]; ok {
		if err := json.Unmarshal(reqRaw, &s.Required); err != nil {
			return nil, at(`"required" must be a list of property names`)
		}
		for _, name := range s.Required {
			if _, ok := s.Properties[name]; !ok {
				return nil, at(`"required" names %q, which is not a property`, name)
			}
		}
	}
	return s, nil
}

func (p *parser) array(s *Schema, fields map[string]json.RawMessage, path string, depth int, at errAt) (*Schema, error) {
	itemsRaw, ok := fields["items"]
	if !ok {
		return nil, at(`an array needs "items"`)
	}
	child, err := p.node(itemsRaw, path+"[]", depth+1)
	if err != nil {
		return nil, err
	}
	s.Items = child
	n, ok, err := intField(fields, "maxItems")
	if err != nil || (ok && n < 1) {
		return nil, at(`"maxItems" must be a positive integer`)
	}
	if ok {
		s.MaxItems = n
	}
	n, ok, err = intField(fields, "minItems")
	if err != nil || (ok && n < 0) {
		return nil, at(`"minItems" must be a non-negative integer`)
	}
	if ok {
		s.MinItems = n
	}
	if s.MaxItems > 0 && s.MinItems > s.MaxItems {
		return nil, at(`"minItems" is above "maxItems"`)
	}
	return s, nil
}

func parseString(s *Schema, fields map[string]json.RawMessage, at errAt) (*Schema, error) {
	if enumRaw, ok := fields["enum"]; ok {
		if err := json.Unmarshal(enumRaw, &s.Enum); err != nil || len(s.Enum) == 0 {
			return nil, at(`"enum" must be a non-empty list of strings`)
		}
		if len(s.Enum) > MaxEnumValues {
			return nil, at(`"enum" has more than %d values`, MaxEnumValues)
		}
		for _, v := range s.Enum {
			if len(v) > MaxStringLength {
				return nil, at(`an "enum" value is longer than %d characters`, MaxStringLength)
			}
		}
	}
	if fRaw, ok := fields["format"]; ok {
		if err := json.Unmarshal(fRaw, &s.Format); err != nil {
			return nil, at(`"format" must be a string`)
		}
		switch s.Format {
		case FormatDate, FormatDateTime, FormatURI:
		default:
			return nil, at(`"format" %q is not supported (date, date-time, uri)`, s.Format)
		}
	}
	n, ok, err := intField(fields, "maxLength")
	if err != nil || (ok && n < 1) {
		return nil, at(`"maxLength" must be a positive integer`)
	}
	if ok {
		if n > MaxStringLength {
			return nil, at(`"maxLength" %d is above the limit of %d; put longer text in a file and name it in "files"`, n, MaxStringLength)
		}
		s.MaxLength = n
	}
	n, ok, err = intField(fields, "minLength")
	if err != nil || (ok && n < 0) {
		return nil, at(`"minLength" must be a non-negative integer`)
	}
	if ok {
		s.MinLength = n
	}
	if s.MaxLength == 0 && len(s.Enum) == 0 && s.Format == "" {
		return nil, at(`a string needs "maxLength" (or an "enum", or a "format" of date, date-time or uri)`)
	}
	if s.MaxLength > 0 && s.MinLength > s.MaxLength {
		return nil, at(`"minLength" is above "maxLength"`)
	}
	return s, nil
}

func parseNumber(s *Schema, fields map[string]json.RawMessage, at errAt) (*Schema, error) {
	for _, key := range []string{"minimum", "maximum"} {
		v, ok := fields[key]
		if !ok {
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, at("%q must be a number", key)
		}
		bound := f
		if key == "minimum" {
			s.Minimum = &bound
		} else {
			s.Maximum = &bound
		}
	}
	if s.Minimum != nil && s.Maximum != nil && *s.Minimum > *s.Maximum {
		return nil, at(`"minimum" is above "maximum"`)
	}
	return s, nil
}

// intField reads an integer keyword. ok is false when absent.
func intField(fields map[string]json.RawMessage, key string) (n int, ok bool, err error) {
	raw, present := fields[key]
	if !present {
		return 0, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > math.MaxInt32 {
		return 0, true, fmt.Errorf("not an integer")
	}
	return int(f), true, nil
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// PropertyNames returns an object schema's property names, sorted.
// Empty for any other kind.
func (s *Schema) PropertyNames() []string {
	if s == nil || s.Kind != KindObject {
		return nil
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// walker carries what one validation pass needs beyond the schema:
// where errors go, and whether a required field may be missing (a
// partial answer leaves out what it could not fill).
type walker struct {
	errs         *errorList
	allowMissing bool
}

// validateValue checks one decoded JSON value against s, adding an
// error per problem. Values come from a decoder with UseNumber so
// integers are told from fractions exactly.
func (s *Schema) validateValue(v any, path string, w *walker) {
	errs := w.errs
	at := func(format string, args ...any) { errs.add(path + ": " + fmt.Sprintf(format, args...)) }
	switch s.Kind {
	case KindObject:
		obj, ok := v.(map[string]any)
		if !ok {
			at("expected an object")
			return
		}
		if !w.allowMissing {
			for _, name := range s.Required {
				if _, ok := obj[name]; !ok {
					at("missing required field %q", name)
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child, ok := s.Properties[k]
			if !ok {
				at("unknown field %q", k)
				continue
			}
			if obj[k] == nil {
				at("%q is null; leave a value you do not have out instead", k)
				continue
			}
			child.validateValue(obj[k], joinPath(path, k), w)
		}
	case KindArray:
		arr, ok := v.([]any)
		if !ok {
			at("expected an array")
			return
		}
		if s.MaxItems > 0 && len(arr) > s.MaxItems {
			at("has %d items; the limit is %d", len(arr), s.MaxItems)
		}
		if len(arr) < s.MinItems {
			at("has %d items; at least %d are required", len(arr), s.MinItems)
		}
		for i, item := range arr {
			if item == nil {
				at("item %d is null", i)
				continue
			}
			s.Items.validateValue(item, fmt.Sprintf("%s[%d]", path, i), w)
		}
	case KindString:
		str, ok := v.(string)
		if !ok {
			at("expected a string")
			return
		}
		s.validateString(str, at)
	case KindNumber, KindInteger:
		num, ok := v.(json.Number)
		if !ok {
			at("expected a number")
			return
		}
		f, err := num.Float64()
		if err != nil {
			at("%s is not a usable number", num)
			return
		}
		if s.Kind == KindInteger {
			if _, err := num.Int64(); err != nil {
				at("%s is not an integer", num)
				return
			}
		}
		if s.Minimum != nil && f < *s.Minimum {
			at("%s is below the minimum of %g", num, *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			at("%s is above the maximum of %g", num, *s.Maximum)
		}
	case KindBoolean:
		if _, ok := v.(bool); !ok {
			at("expected true or false")
		}
	}
}

// validateString checks a string against its enum, its format and its
// length bounds. A format fixes the shape; a maxLength or minLength
// beside it still applies.
func (s *Schema) validateString(str string, at func(format string, args ...any)) {
	if len(s.Enum) > 0 {
		for _, e := range s.Enum {
			if e == str {
				return
			}
		}
		at("%q is not one of %s", str, strings.Join(s.Enum, ", "))
		return
	}
	switch s.Format {
	case FormatDate:
		if _, err := time.Parse("2006-01-02", str); err != nil {
			at("%q is not a date in YYYY-MM-DD form", str)
			return
		}
	case FormatDateTime:
		if _, err := time.Parse(time.RFC3339, str); err != nil {
			at("%q is not an RFC 3339 date-time", str)
			return
		}
	case FormatURI:
		if why := checkURL(str); why != "" {
			at("URL %s", why)
			return
		}
	}
	n := len([]rune(str))
	if s.MaxLength > 0 && n > s.MaxLength {
		at("is %d characters; the limit is %d", n, s.MaxLength)
	} else if n < s.MinLength {
		at("is %d characters; at least %d are required", n, s.MinLength)
	}
}

// errorList collects validation errors up to a cap, so a wildly wrong
// submission reports its first problems rather than thousands.
type errorList struct {
	errs    []string
	dropped int
}

const maxReportedErrors = 20

func (l *errorList) add(msg string) {
	if len(l.errs) >= maxReportedErrors {
		l.dropped++
		return
	}
	l.errs = append(l.errs, msg)
}

func (l *errorList) list() []string {
	if l.dropped > 0 {
		return append(l.errs, fmt.Sprintf("and %d more", l.dropped))
	}
	return l.errs
}
