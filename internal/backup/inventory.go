package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// Inventory counts what a data directory, or an archive of one, holds,
// read with the store's own parsers. Audit takes it of a data
// directory.
type Inventory struct {
	Files          int
	Bytes          int64
	ActiveAgents   int
	ArchivedAgents int
	ArchivedChats  int
	// Messages counts message files by stored type, redacted ones too.
	Messages map[string]int
	// Assignments counts assignment files by status.
	Assignments   map[string]int
	PendingWakes  int
	UsageRecords  int
	UsageTokens   int64
	UsageBadLines int

	// Problems are stored data this binary cannot read, or reads wrong.
	Problems []string
	// Notes are stored data it reads but does not recognise all of,
	// such as a front-matter key no field takes.
	Notes []string
}

// NewInventory returns an empty inventory.
func NewInventory() *Inventory {
	return &Inventory{Messages: map[string]int{}, Assignments: map[string]int{}}
}

var (
	assignmentFileRE  = regexp.MustCompile(`^` + store.AssignmentsDirName + `/\d+\.md$`)
	pendingWakeFileRE = regexp.MustCompile(`^` + store.AssignmentsDirName + `/pending/[^/]+\.json$`)
	activeAgentRE     = regexp.MustCompile(`^agents/[^/_][^/]*/agent\.yaml$`)
	archivedAgentRE   = regexp.MustCompile(`^agents/_archived/[^/]+/agent\.yaml$`)
	archivedChatRE    = regexp.MustCompile(`^agents/(_archived/)?[^/]+/chats/[^/]+/chat\.jsonl$`)
	rotationMarkerRE  = regexp.MustCompile(`^agents/(_archived/)?[^/]+/pending_rotation\.json$`)
)

// Wants reports whether Add needs a file's content, not just its size.
func Wants(rel string) bool {
	return isMessage(rel) || assignmentFileRE.MatchString(rel) || pendingWakeFileRE.MatchString(rel) ||
		rotationMarkerRE.MatchString(rel) || rel == "usage.jsonl"
}

func isMessage(rel string) bool {
	return strings.HasPrefix(rel, "messages/") && strings.HasSuffix(rel, ".md")
}

// Add counts one regular file. content must be the file's bytes when
// Wants(rel), and is ignored otherwise.
func (inv *Inventory) Add(rel string, size int64, content []byte) {
	inv.Files++
	inv.Bytes += size
	switch {
	case activeAgentRE.MatchString(rel):
		inv.ActiveAgents++
	case archivedAgentRE.MatchString(rel):
		inv.ArchivedAgents++
	case archivedChatRE.MatchString(rel):
		inv.ArchivedChats++
	case isMessage(rel):
		inv.addMessage(rel, content)
	case assignmentFileRE.MatchString(rel):
		a, err := assignments.Parse(content)
		if err != nil {
			inv.problem("%s: %v", rel, err)
			return
		}
		inv.Assignments[string(a.Status)]++
	case pendingWakeFileRE.MatchString(rel):
		inv.PendingWakes++
		inv.checkJSONKeys(rel, content, reflect.TypeOf(store.PendingWakes{}))
	case rotationMarkerRE.MatchString(rel):
		inv.checkJSONKeys(rel, content, reflect.TypeOf(store.PendingRotation{}))
	case rel == "usage.jsonl":
		inv.addUsage(content)
	}
}

func (inv *Inventory) addMessage(rel string, content []byte) {
	m, err := store.ParseMessage(rel, content)
	if err != nil {
		inv.problem("%s: %v", rel, err)
		return
	}
	inv.Messages[string(m.Type)]++
	known := false
	for _, t := range store.KnownMessageTypes() {
		known = known || m.Type == t
	}
	switch {
	case !known:
		inv.problem("%s: unknown message type %q", rel, m.Type)
	case m.Type == store.MsgAssignmentEvent && m.Assignment == nil:
		inv.problem("%s: an assignment event without its assignment", rel)
	}
	keys, err := frontMatterKeys(content)
	if err != nil {
		inv.problem("%s: %v", rel, err)
		return
	}
	fields := yamlFields(reflect.TypeOf(store.Message{}))
	for _, k := range keys {
		if !fields[k] {
			inv.note("message front-matter key %q, which nothing reads", k)
		}
	}
}

func (inv *Inventory) checkJSONKeys(rel string, content []byte, t reflect.Type) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(content, &m); err != nil {
		inv.problem("%s: %v", rel, err)
		return
	}
	fields := jsonFields(t)
	for k := range m {
		if !fields[k] {
			inv.note("%s key %q, which nothing reads", t.Name(), k)
		}
	}
}

func (inv *Inventory) addUsage(content []byte) {
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r store.UsageRecord
		if err := json.Unmarshal(line, &r); err != nil {
			inv.UsageBadLines++
			continue
		}
		inv.UsageRecords++
		inv.UsageTokens += int64(r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheCreateTokens)
	}
	if err := sc.Err(); err != nil {
		inv.problem("usage.jsonl: %v", err)
	}
}

func (inv *Inventory) problem(format string, a ...any) {
	inv.Problems = append(inv.Problems, fmt.Sprintf(format, a...))
}

func (inv *Inventory) note(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	for _, n := range inv.Notes {
		if n == s {
			return
		}
	}
	inv.Notes = append(inv.Notes, s)
}

// Print writes the counts, one per line, in a fixed order. Problems and
// notes are not part of it: two inventories of the same data print the
// same text.
func (inv *Inventory) Print(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	p("files               %d", inv.Files)
	p("bytes               %d", inv.Bytes)
	p("agents active       %d", inv.ActiveAgents)
	p("agents archived     %d", inv.ArchivedAgents)
	p("archived chats      %d", inv.ArchivedChats)
	for _, k := range sortedKeys(inv.Messages) {
		p("messages %-19s %d", k, inv.Messages[k])
	}
	for _, k := range sortedKeys(inv.Assignments) {
		p("assignments %-16s %d", k, inv.Assignments[k])
	}
	p("pending wakes       %d", inv.PendingWakes)
	p("usage records       %d", inv.UsageRecords)
	p("usage tokens        %d", inv.UsageTokens)
	p("usage bad lines     %d", inv.UsageBadLines)
}

// String is Print into a string.
func (inv *Inventory) String() string {
	var b strings.Builder
	inv.Print(&b)
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var frontMatterRE = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?`)

// frontMatterKeys lists a message's top-level front-matter keys.
func frontMatterKeys(content []byte) ([]string, error) {
	m := frontMatterRE.FindSubmatch(content)
	if m == nil {
		return nil, fmt.Errorf("no front matter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(m[1], &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("front matter is not a mapping")
	}
	var keys []string
	for i := 0; i < len(doc.Content[0].Content); i += 2 {
		keys = append(keys, doc.Content[0].Content[i].Value)
	}
	return keys, nil
}

func yamlFields(t reflect.Type) map[string]bool { return tagNames(t, "yaml") }
func jsonFields(t reflect.Type) map[string]bool { return tagNames(t, "json") }

func tagNames(t reflect.Type, tag string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get(tag), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}
