package structured

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxSubmissionBytes bounds one submit_result call. Every string in
// it is capped by the schema; this caps the arrays and the whole.
const MaxSubmissionBytes = 64 << 10

// MaxAlternatives is how many alternatives a result may list.
const MaxAlternatives = 5

// MaxFiles is how many files a result may list, MaxFilePathLength how
// long each path may be and MaxFileSegmentLength how long each of its
// names may be.
const (
	MaxFiles             = 20
	MaxFilePathLength    = 256
	MaxFileSegmentLength = 64
)

// Outcome is how a task ended.
type Outcome string

const (
	// OutcomeFound: the result is what was asked for.
	OutcomeFound Outcome = "found"
	// OutcomePartial: the result is what was asked for, with fields
	// the subagent could not fill left out, required ones included.
	OutcomePartial Outcome = "partial"
	// OutcomeNotFound: the subagent looked and there is no such thing.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeAlternative: not the thing asked for, but alternatives
	// that fill the same schema.
	OutcomeAlternative Outcome = "alternative"
	// OutcomeBlocked: the subagent could not proceed; blocked_on says why.
	OutcomeBlocked Outcome = "blocked"
	// OutcomeFailed is written by core alone: the task ended without
	// a valid result, and Error says why.
	OutcomeFailed Outcome = "failed"
)

// submittableOutcomes are the outcomes a subagent may write.
var submittableOutcomes = []Outcome{OutcomeFound, OutcomePartial, OutcomeNotFound, OutcomeAlternative, OutcomeBlocked}

// BlockedOn is what stopped a blocked task.
type BlockedOn string

const (
	BlockedLoginRequired BlockedOn = "login_required"
	BlockedMFA           BlockedOn = "mfa"
	BlockedCaptcha       BlockedOn = "captcha"
	BlockedConsent       BlockedOn = "consent"
	BlockedPaywall       BlockedOn = "paywall"
	BlockedOffSite       BlockedOn = "off_site"
	BlockedNotReachable  BlockedOn = "not_reachable"
	// BlockedUserDidNotRespond is written by core alone, when a task
	// waited for a person who never came.
	BlockedUserDidNotRespond BlockedOn = "user_did_not_respond"
	// BlockedUploadDenied is written by core alone, when a person denied
	// a task's uploads; Reason carries what they said.
	BlockedUploadDenied BlockedOn = "upload_denied"
)

// submittableBlockedOn are the reasons a subagent may write.
var submittableBlockedOn = []BlockedOn{BlockedLoginRequired, BlockedMFA, BlockedCaptcha, BlockedConsent, BlockedPaywall, BlockedOffSite, BlockedNotReachable}

// AlternativeReason says how an alternative differs from what was asked.
type AlternativeReason string

const (
	ReasonDifferentItem   AlternativeReason = "different_item"
	ReasonDifferentSource AlternativeReason = "different_source"
	ReasonOlder           AlternativeReason = "older"
	ReasonNewer           AlternativeReason = "newer"
	ReasonBroader         AlternativeReason = "broader"
	ReasonNarrower        AlternativeReason = "narrower"
	ReasonPartialMatch    AlternativeReason = "partial_match"
)

var alternativeReasons = []AlternativeReason{ReasonDifferentItem, ReasonDifferentSource, ReasonOlder, ReasonNewer, ReasonBroader, ReasonNarrower, ReasonPartialMatch}

// Alternative is one answer that fills the schema without being the
// thing asked for.
type Alternative struct {
	Result    json.RawMessage   `json:"result"`
	URL       string            `json:"url,omitempty"`
	DiffersOn []string          `json:"differs_on,omitempty"`
	Reason    AlternativeReason `json:"reason"`
}

// Result is the wrapper every structured answer fits. A subagent
// writes Outcome, Result, Alternatives, BlockedOn and Files through
// submit_result; Error is core's, on a task it ended itself, and the
// WebFacts are core's on a web task. A submission that names any of
// core's fields is refused as unknown.
type Result struct {
	Outcome      Outcome         `json:"outcome"`
	Result       json.RawMessage `json:"result,omitempty"`
	Alternatives []Alternative   `json:"alternatives,omitempty"`
	BlockedOn    BlockedOn       `json:"blocked_on,omitempty"`
	// Reason says why a result core wrote ended as it did: a person's
	// own words when they denied a task's uploads, or core's when a
	// restart ended a task. A subagent cannot submit it.
	Reason string   `json:"reason,omitempty"`
	Files  []string `json:"files,omitempty"`
	Error  string   `json:"error,omitempty"`
	WebFacts
}

// WebFacts are what core knows about a web task's run and adds to its
// result: the session it ran in, the origin of the last page it
// loaded, how long it ran and how many pages it loaded.
type WebFacts struct {
	Session         string `json:"session,omitempty"`
	Origin          string `json:"origin,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Pages           int    `json:"pages,omitempty"`
}

// WithWebFacts returns body, a result as Body rendered it, with the
// facts set and the downloads added to its files. A body that does not
// parse is returned as it was.
func WithWebFacts(body string, facts WebFacts, downloads []string) string {
	var r Result
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return body
	}
	r.WebFacts = facts
	seen := map[string]bool{}
	for _, f := range r.Files {
		seen[f] = true
	}
	for _, d := range downloads {
		if !seen[d] {
			seen[d] = true
			r.Files = append(r.Files, d)
		}
	}
	return r.Body()
}

// Rules are the per-task facts validation needs beyond the schema.
type Rules struct {
	// Files maps each directory a result may name a file in, as the
	// subagent sees it (an absolute prefix with a trailing slash, such
	// as /files/artifacts/private/), to the same directory as the
	// caller sees it (/files/subagents/<id>/artifacts/private/). The
	// result delivered to the caller carries the caller's paths. Empty
	// refuses files altogether.
	Files map[string]string
	// FileExists, when set, reports whether the caller's path names a
	// regular file; a result may not name a file the task did not make.
	FileExists func(callerPath string) bool
}

// submissionKeys are the wrapper fields a subagent may write.
var submissionKeys = map[string]bool{"outcome": true, "result": true, "alternatives": true, "blocked_on": true, "files": true}

// Validate checks one submit_result call against the schema and the
// wrapper's rules. It returns the result as core will deliver it and
// no errors, or the errors for the subagent to fix. The errors name
// paths in the submission and say what to change.
func Validate(s *Schema, raw json.RawMessage, rules Rules) (Result, []string) {
	var errs errorList
	if len(raw) > MaxSubmissionBytes {
		errs.add(fmt.Sprintf("the result is %d bytes; the limit is %d. Put bulk in a file and name it in files", len(raw), MaxSubmissionBytes))
		return Result{}, errs.list()
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil || top == nil {
		errs.add("the result must be a JSON object with an outcome")
		return Result{}, errs.list()
	}
	if _, err := dec.Token(); err != io.EOF {
		errs.add("the result must be one JSON object with nothing after it")
		return Result{}, errs.list()
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !submissionKeys[k] {
			errs.add(fmt.Sprintf("unknown field %q; the wrapper takes outcome, result, alternatives, blocked_on and files", k))
		}
	}

	var out Result
	outcomeStr, _ := top["outcome"].(string)
	out.Outcome = Outcome(outcomeStr)
	if !containsOutcome(submittableOutcomes, out.Outcome) {
		if out.Outcome == OutcomeFailed {
			errs.add(`outcome "failed" is written only by core; use "blocked" or "not_found"`)
		} else {
			errs.add(fmt.Sprintf("outcome must be one of %s", joinOutcomes(submittableOutcomes)))
		}
		return Result{}, errs.list()
	}
	// A partial answer may leave required fields out: that is what
	// partial means, and the caller reads the outcome before the fields.
	w := &walker{errs: &errs, allowMissing: out.Outcome == OutcomePartial}

	resultVal, hasResult := top["result"]
	switch out.Outcome {
	case OutcomeFound, OutcomePartial:
		if !hasResult || resultVal == nil {
			errs.add(fmt.Sprintf("result is required with outcome %q", out.Outcome))
		}
	case OutcomeNotFound, OutcomeBlocked, OutcomeAlternative:
		if hasResult && resultVal != nil {
			errs.add(fmt.Sprintf("result must be left out with outcome %q", out.Outcome))
		}
	}
	if hasResult && resultVal != nil {
		s.validateValue(resultVal, "result", w)
		out.Result = compactValue(resultVal)
	}

	altsVal, hasAlts := top["alternatives"]
	if hasAlts && altsVal != nil {
		alts, ok := altsVal.([]any)
		if !ok {
			errs.add("alternatives must be an array")
		} else {
			switch out.Outcome {
			case OutcomeNotFound, OutcomeBlocked:
				if len(alts) > 0 {
					errs.add(fmt.Sprintf("alternatives must be left out with outcome %q", out.Outcome))
				}
			}
			if len(alts) > MaxAlternatives {
				errs.add(fmt.Sprintf("alternatives has %d entries; the limit is %d", len(alts), MaxAlternatives))
			}
			// An alternative is a whole answer of its own, never partial.
			aw := &walker{errs: &errs}
			for i, a := range alts {
				if alt, ok := validateAlternative(s, a, fmt.Sprintf("alternatives[%d]", i), aw); ok {
					out.Alternatives = append(out.Alternatives, alt)
				}
			}
		}
	}
	if out.Outcome == OutcomeAlternative && len(out.Alternatives) == 0 {
		errs.add(`outcome "alternative" needs at least one entry in alternatives`)
	}

	blockedVal, hasBlocked := top["blocked_on"]
	if out.Outcome == OutcomeBlocked {
		str, _ := blockedVal.(string)
		out.BlockedOn = BlockedOn(str)
		if !containsBlocked(submittableBlockedOn, out.BlockedOn) {
			if out.BlockedOn == BlockedUserDidNotRespond || out.BlockedOn == BlockedUploadDenied {
				errs.add(fmt.Sprintf("blocked_on %q is written only by core", out.BlockedOn))
			} else {
				errs.add(fmt.Sprintf("blocked_on is required with outcome \"blocked\" and must be one of %s", joinBlocked(submittableBlockedOn)))
			}
		}
	} else if hasBlocked && blockedVal != nil {
		errs.add(`blocked_on goes only with outcome "blocked"`)
	}

	if filesVal, ok := top["files"]; ok && filesVal != nil {
		out.Files = validateFiles(filesVal, rules, &errs)
	}

	if list := errs.list(); len(list) > 0 {
		return Result{}, list
	}
	return out, nil
}

func validateAlternative(s *Schema, v any, where string, w *walker) (Alternative, bool) {
	errs := w.errs
	obj, ok := v.(map[string]any)
	if !ok {
		errs.add(where + ": expected an object with result and reason")
		return Alternative{}, false
	}
	var alt Alternative
	before := len(errs.errs) + errs.dropped
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "result", "url", "differs_on", "reason":
		default:
			errs.add(fmt.Sprintf("%s: unknown field %q; an alternative takes result, url, differs_on and reason", where, k))
		}
	}
	res, ok := obj["result"]
	if !ok || res == nil {
		errs.add(where + ".result is required")
	} else {
		s.validateValue(res, where+".result", w)
		alt.Result = compactValue(res)
	}
	if u, ok := obj["url"]; ok && u != nil {
		str, isStr := u.(string)
		if !isStr {
			errs.add(where + ".url must be a string")
		} else if why := checkURL(str); why != "" {
			errs.add(where + ".url " + why)
		} else {
			alt.URL = str
		}
	}
	if d, ok := obj["differs_on"]; ok && d != nil {
		list, isList := d.([]any)
		if !isList {
			errs.add(where + ".differs_on must be a list of field names from the schema")
		}
		for _, item := range list {
			name, isStr := item.(string)
			if !isStr {
				errs.add(where + ".differs_on must be a list of field names from the schema")
				continue
			}
			if _, known := s.Properties[name]; !known {
				errs.add(fmt.Sprintf("%s.differs_on names %q, which is not a field of the schema (%s)", where, name, strings.Join(s.PropertyNames(), ", ")))
				continue
			}
			alt.DiffersOn = append(alt.DiffersOn, name)
		}
	}
	reason, _ := obj["reason"].(string)
	alt.Reason = AlternativeReason(reason)
	if !containsReason(alternativeReasons, alt.Reason) {
		errs.add(fmt.Sprintf("%s.reason is required and must be one of %s", where, joinReasons(alternativeReasons)))
	}
	return alt, len(errs.errs)+errs.dropped == before
}

// fileSegment is what one name in a file path may look like: a letter
// or digit first, then letters, digits, dots, dashes and underscores.
// Narrow on purpose: a path is the one wrapper field that is not an
// enum or capped by the schema, and this keeps it from carrying prose.
var fileSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateFiles checks the files list and maps each entry from the
// subagent's view to the caller's, so a result can only name what the
// task made and the caller reads paths it can open.
func validateFiles(v any, rules Rules, errs *errorList) []string {
	list, ok := v.([]any)
	if !ok {
		errs.add("files must be a list of paths")
		return nil
	}
	if len(list) > MaxFiles {
		errs.add(fmt.Sprintf("files has %d entries; the limit is %d", len(list), MaxFiles))
		return nil
	}
	if len(rules.Files) == 0 && len(list) > 0 {
		errs.add("files must be left out: this task may not name files")
		return nil
	}
	prefixes := make([]string, 0, len(rules.Files))
	for p := range rules.Files {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	var out []string
	for i, item := range list {
		p, ok := item.(string)
		where := fmt.Sprintf("files[%d]", i)
		if !ok {
			errs.add(where + ": expected a path")
			continue
		}
		if why := checkFilePath(p); why != "" {
			errs.add(fmt.Sprintf("%s: %q %s", where, p, why))
			continue
		}
		mapped := ""
		for _, prefix := range prefixes {
			if strings.HasPrefix(p, prefix) {
				mapped = rules.Files[prefix] + strings.TrimPrefix(p, prefix)
				break
			}
		}
		if mapped == "" {
			errs.add(fmt.Sprintf("%s: %q is outside the directories a result may name (%s)", where, p, strings.Join(prefixes, ", ")))
			continue
		}
		if rules.FileExists != nil && !rules.FileExists(mapped) {
			errs.add(fmt.Sprintf("%s: %q is not a file this task made", where, p))
			continue
		}
		out = append(out, mapped)
	}
	return out
}

// checkFilePath says what is wrong with a path a result names, or
// nothing: absolute, clean, bounded, and made of plain names.
func checkFilePath(p string) string {
	switch {
	case len(p) > MaxFilePathLength:
		return fmt.Sprintf("is longer than %d characters", MaxFilePathLength)
	case !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.HasSuffix(p, "/"):
		return "must be an absolute, clean file path"
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if len(seg) > MaxFileSegmentLength {
			return fmt.Sprintf("has a name longer than %d characters", MaxFileSegmentLength)
		}
		if !fileSegment.MatchString(seg) {
			return "may use only letters, digits, dots, dashes and underscores in its names"
		}
	}
	return ""
}

// compactValue re-encodes a decoded value as compact JSON, numbers in
// canonical form and text as written.
func compactValue(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonical(v)); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// canonical rewrites every number in a decoded value from the text the
// subagent wrote to the shortest form that means the same, so a result
// cannot carry a kilobyte of digits in one field, and shares no other
// quirk of the submission's spelling.
func canonical(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = canonical(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = canonical(item)
		}
		return out
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, err := x.Float64()
		if err != nil {
			return 0
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	default:
		return v
	}
}

// Failed is the result core writes when it ends a task itself: no
// valid submission after the allowed attempts, a run that ended
// without one, or a lost browser. reason is core's text, never the
// subagent's.
func Failed(reason string) Result {
	return Result{Outcome: OutcomeFailed, Error: reason}
}

// Blocked is the result core writes when a task waited for a person
// who never came.
func Blocked(on BlockedOn) Result {
	return Result{Outcome: OutcomeBlocked, BlockedOn: on}
}

// UploadDenied is the result core writes when a person denied a task's
// uploads, with what they said, if anything.
func UploadDenied(reason string) Result {
	return Result{Outcome: OutcomeBlocked, BlockedOn: BlockedUploadDenied, Reason: reason}
}

// Body renders the result as the JSON the calling agent reads: indented,
// keys in the wrapper's order, text as written.
func (r Result) Body() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return `{"outcome":"failed","error":"the result could not be encoded"}`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// urlChars are the bytes RFC 3986 lets a URL carry: unreserved,
// reserved and the percent of an escape. Anything else, a space or a
// letter outside ASCII above all, is prose, not a URL.
const urlChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~:/?#[]@!$&'()*+,;=%"

// checkURL says what is wrong with a URL a result names, or nothing:
// bounded, made only of URL characters, http or https, with a host.
func checkURL(raw string) string {
	if len(raw) > MaxURLLength {
		return fmt.Sprintf("is longer than %d characters", MaxURLLength)
	}
	for i := 0; i < len(raw); i++ {
		if strings.IndexByte(urlChars, raw[i]) < 0 {
			return fmt.Sprintf("has a character a URL cannot carry at position %d", i+1)
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "must be an http or https URL with a host"
	}
	return ""
}

// ToolInputSchema is the JSON Schema of the submit_result tool's input
// for a task with schema s: the wrapper, with result and each
// alternative's result typed as the caller's schema. The subagent reads
// this as the tool's definition, so it sees exactly what to fill.
func ToolInputSchema(s *Schema) json.RawMessage {
	resultSchema := string(s.Raw)
	fields := strings.Join(quoteAll(s.PropertyNames()), ", ")
	schema := fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "outcome": {
      "type": "string",
      "enum": [%s],
      "description": "found: result is what was asked for. partial: result is what was asked for with fields you could not fill left out. not_found: you looked and there is no such thing. alternative: not the thing asked for, but alternatives that fit the same schema. blocked: you could not proceed; say why in blocked_on."
    },
    "result": %s,
    "alternatives": {
      "type": "array",
      "maxItems": %d,
      "description": "Up to %d answers that fit the schema without being the thing asked for. Required with outcome alternative; allowed with found and partial.",
      "items": {
        "type": "object",
        "properties": {
          "result": %s,
          "url": {"type": "string", "maxLength": %d, "description": "the http or https URL this alternative was found at"},
          "differs_on": {"type": "array", "items": {"type": "string", "enum": [%s]}, "description": "the result fields on which this alternative differs from what was asked for"},
          "reason": {"type": "string", "enum": [%s], "description": "how it differs: a different item, a different source, an older or newer version, broader or narrower in scope, or a partial match"}
        },
        "required": ["result", "reason"]
      }
    },
    "blocked_on": {"type": "string", "enum": [%s], "description": "with outcome blocked only: what stopped you"},
    "files": {"type": "array", "maxItems": %d, "items": {"type": "string", "maxLength": %d}, "description": "files this task made, by the absolute path you see them at (under /files/artifacts/private/ or /files/background/; a web task's downloads under /files/untrusted-downloads/); names use letters, digits, dots, dashes and underscores"}
  },
  "required": ["outcome"]
}`,
		strings.Join(quoteAll(outcomeStrings(submittableOutcomes)), ", "),
		resultSchema,
		MaxAlternatives, MaxAlternatives,
		resultSchema,
		MaxURLLength,
		fields,
		strings.Join(quoteAll(reasonStrings(alternativeReasons)), ", "),
		strings.Join(quoteAll(blockedStrings(submittableBlockedOn)), ", "),
		MaxFiles, MaxFilePathLength,
	)
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(schema)); err != nil {
		return json.RawMessage(schema)
	}
	return json.RawMessage(compact.Bytes())
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		b, _ := json.Marshal(s)
		out[i] = string(b)
	}
	return out
}

func containsOutcome(list []Outcome, o Outcome) bool {
	for _, x := range list {
		if x == o {
			return true
		}
	}
	return false
}

func containsBlocked(list []BlockedOn, b BlockedOn) bool {
	for _, x := range list {
		if x == b {
			return true
		}
	}
	return false
}

func containsReason(list []AlternativeReason, r AlternativeReason) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

func outcomeStrings(list []Outcome) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = string(x)
	}
	return out
}

func blockedStrings(list []BlockedOn) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = string(x)
	}
	return out
}

func reasonStrings(list []AlternativeReason) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = string(x)
	}
	return out
}

func joinOutcomes(list []Outcome) string  { return strings.Join(outcomeStrings(list), ", ") }
func joinBlocked(list []BlockedOn) string { return strings.Join(blockedStrings(list), ", ") }
func joinReasons(list []AlternativeReason) string {
	return strings.Join(reasonStrings(list), ", ")
}
