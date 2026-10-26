package githubintegration

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// The access form's fields: the repositories an agent may use, and
// whether it may open issues and pull requests, comment and push in
// them.
const (
	repositoriesKey = "repositories"
	writeKey        = "write"
)

// access is an agent's access as the owner's form fills it in. A
// missing list is no repository; a missing switch is off.
type access struct {
	repositories []string
	write        bool
}

// readAccess reads the stored form: every non-blank string of
// repositories (gitrepo.Match ignores one that is not a pattern), and
// write when it is true and nothing else.
func readAccess(m map[string]any) access {
	var a access
	raw, _ := m[repositoriesKey].([]any)
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			a.repositories = append(a.repositories, s)
		}
	}
	a.write, _ = m[writeKey].(bool)
	return a
}

// includes reports whether the repository owner/name is one the
// access names, by gitrepo's rules: * within a segment, case ignored.
func (a access) includes(repo string) bool {
	return gitrepo.Match(a.repositories, strings.ToLower(repo))
}

// writeRefusal is the words of a write action called without write,
// push among them, and writeReason its audit's reason.
const (
	writeRefusal = "Your access to GitHub does not include opening issues and pull requests, commenting or pushing; ask the owner."
	writeReason  = "the access does not include opening issues and pull requests, commenting or pushing"
)

// notIncluded is the refusal of a repository the access does not name.
func notIncluded(repo string) error {
	return refuse(fmt.Sprintf("Your access to GitHub does not include %s; ask the owner.", repo), "the access does not include "+repo)
}

func refuse(text, reason string) error {
	return &builtinintegrations.Refusal{Text: text, Reason: reason}
}

// badArgument is the refusal of an argument that does not read.
func badArgument(text string) error {
	return refuse(text, "the arguments do not read")
}

// Bounds on what an answer carries, in characters.
const (
	bodyClip     = 20000
	diffClip     = 50000
	commentsKept = 20
	// answerClip is the most an answer's text holds, whatever the
	// action.
	answerClip = 60000
	// maxText is the longest text an agent may give an action.
	maxText = 60000
	// titleMax is the longest title of an issue or a pull request.
	titleMax = 256
	// searchMax is the longest search list_issues takes.
	searchMax = 500
)

// args is an action's arguments as the model gave them: a JSON object,
// numbers kept as written so a whole number can be told from another.
type args map[string]any

// readArgs reads raw as an object; empty and null are {}.
func readArgs(raw json.RawMessage) (args, error) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return args{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, badArgument("The arguments are an object.")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badArgument("The arguments are an object.")
	}
	return args(m), nil
}

// repositoryName matches one segment of owner/name.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// cleanRepository is v as owner/name, slashes around it and a .git
// suffix trimmed; a refusal when it is not one. Spaces and tabs around
// it are a slip and go, as the Git integration takes them; any other
// whitespace, such as a newline, refuses it.
func cleanRepository(v any) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", badArgument("Name the repository as owner/name.")
	}
	echoed, _ := clip(strings.TrimSpace(s), 200)
	notOne := badArgument(fmt.Sprintf("%s is not owner/name.", echoed))
	p := strings.Trim(strings.Trim(s, " \t"), "/")
	p = strings.Trim(strings.TrimSuffix(p, ".git"), "/")
	segs := strings.Split(p, "/")
	if len(segs) != 2 || len(p) > 200 {
		return "", notOne
	}
	for _, seg := range segs {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, "-") || !repositoryName.MatchString(seg) {
			return "", notOne
		}
	}
	return p, nil
}

// repository is the call's repository argument, held to the access.
func (a args) repository(acc access) (string, error) {
	repo, err := cleanRepository(a["repository"])
	if err != nil {
		return "", err
	}
	if !acc.includes(repo) {
		return "", notIncluded(repo)
	}
	return repo, nil
}

// wholeNumber reads v as a whole number written without a fraction or
// an exponent.
func wholeNumber(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return i, err == nil
}

// number is the argument name as a positive whole number.
func (a args) number(name string) (int64, error) {
	n, ok := wholeNumber(a[name])
	if !ok || n < 1 {
		return 0, badArgument(name + " is a positive whole number.")
	}
	return n, nil
}

// limit is the limit argument: def when absent, at most most.
func (a args) limit(def, most int) (int, error) {
	v, present := a["limit"]
	if !present {
		return def, nil
	}
	n, ok := wholeNumber(v)
	if !ok || n < 1 {
		return 0, badArgument("limit is a positive whole number.")
	}
	return int(min(n, int64(most))), nil
}

// choice is the argument name, one of choices; def when absent.
func (a args) choice(name string, choices []string, def string) (string, error) {
	v, present := a[name]
	if !present {
		return def, nil
	}
	if s, ok := v.(string); ok {
		for _, c := range choices {
			if s == c {
				return s, nil
			}
		}
	}
	return "", badArgument(fmt.Sprintf("%s is one of %s.", name, strings.Join(choices, ", ")))
}

// text is the argument name as text of at most most characters: ""
// when absent or empty, unless required. oneLine refuses a line break.
func (a args) text(name string, required, oneLine bool, most int) (string, error) {
	v, present := a[name]
	if !present || v == nil || v == "" {
		if required {
			return "", badArgument("Give the " + name + ".")
		}
		return "", nil
	}
	s, ok := v.(string)
	switch {
	case !ok:
		return "", badArgument(name + " is text.")
	case required && strings.TrimSpace(s) == "":
		return "", badArgument("Give the " + name + ".")
	case oneLine && strings.ContainsAny(s, "\n\r"):
		return "", badArgument(name + " is one line.")
	case utf8.RuneCountInString(s) > most:
		return "", badArgument(fmt.Sprintf("%s is longer than %d characters.", name, most))
	}
	return s, nil
}

// branchName matches what a branch argument may hold.
var branchName = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// branch is the argument name as a branch name (or a tag or a commit,
// for compare): "" when absent and not required.
func (a args) branch(name string, required bool) (string, error) {
	v, present := a[name]
	if !present || v == nil || v == "" {
		if required {
			return "", badArgument("Give the " + name + " branch.")
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok || len(s) > 255 || !branchName.MatchString(s) ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") ||
		strings.Contains(s, "..") || strings.Contains(s, "//") {
		return "", badArgument(name + " is not a branch name.")
	}
	return s, nil
}

// boolean is the argument name as true or false; false when absent.
func (a args) boolean(name string) (bool, error) {
	v, present := a[name]
	if !present {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, badArgument(name + " is true or false.")
	}
	return b, nil
}

// labels is the labels argument: label names, none when absent.
func (a args) labels() ([]string, error) {
	v, present := a["labels"]
	if !present || v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, badArgument("labels is a list of label names.")
	}
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		s, ok := l.(string)
		if !ok || strings.TrimSpace(s) == "" || strings.Contains(s, ",") || utf8.RuneCountInString(s) > 100 {
			return nil, badArgument("labels is a list of label names.")
		}
		out = append(out, s)
	}
	return out, nil
}

// whereQualifier is a search qualifier that names where to search.
// The search runs with repo:<the repository> added, and GitHub ORs
// repo: qualifiers and honours the others, so one in the search would
// reach other repositories. A qualifier counts after anything but a
// letter, digit or underscore: a space, a parenthesis, a quote, a
// minus.
var whereQualifier = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])(repo|org|user|owner):`)

// clip is s cut to most characters, and whether it was.
func clip(s string, most int) (string, bool) {
	if utf8.RuneCountInString(s) <= most {
		return s, false
	}
	i, n := 0, 0
	for i = range s {
		if n == most {
			break
		}
		n++
	}
	return s[:i], true
}

// firstLine is s up to its first line break.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
