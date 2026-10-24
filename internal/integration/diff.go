package integration

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Diff is what changes from the manifest from to the manifest to, in
// words, for the install screen of a version that would run in place
// of the one in use: the hosts, the hosts a sign-in goes to, the
// software its pod installs, the connection's fields, what a setup
// flow may do (the hosts it sends the browser to, the fields it asks
// for, the keys it keeps), the actions and the access form's fields to
// gains or loses, one line for each kind and direction that has any
// ("Hosts added: api.example.com", "Software added: gh"). A host is
// matched by its pattern, a package by its name, a connection, setup or
// access field by its key, a kept key by itself and an action by its
// name; what is added is listed in to's order and what is removed in
// from's. A secret connection or setup field says so, and a key both
// keep whose secrecy changed is named ("Setup keeps, no longer secret:
// slug"). Nothing of those changed is no lines, and so is a nil
// manifest.
func Diff(from, to *Manifest) []string {
	if from == nil || to == nil {
		return nil
	}
	var out []string
	line := func(what string, items []string) {
		if len(items) > 0 {
			out = append(out, what+": "+strings.Join(items, ", "))
		}
	}
	added, removed := diffKeys(hostItems(from), hostItems(to))
	line("Hosts added", added)
	line("Hosts removed", removed)
	added, removed = diffKeys(signInHostItems(from), signInHostItems(to))
	line("Sign-in hosts added", added)
	line("Sign-in hosts removed", removed)
	added, removed = diffKeys(packageItems(from), packageItems(to))
	line("Software added", added)
	line("Software removed", removed)
	added, removed = diffKeys(connectionItems(from), connectionItems(to))
	line("Connection fields added", added)
	line("Connection fields removed", removed)
	added, removed = diffKeys(setupHostItems(from), setupHostItems(to))
	line("Setup sends your browser to, added", added)
	line("Setup sends your browser to, removed", removed)
	added, removed = diffKeys(setupFieldItems(from), setupFieldItems(to))
	line("Setup asks for, added", added)
	line("Setup asks for, removed", removed)
	added, removed = diffKeys(setupKeepItems(from), setupKeepItems(to))
	line("Setup keeps, added", added)
	line("Setup keeps, removed", removed)
	secret, shown := keepSecrecyChanges(from, to)
	line("Setup keeps, now secret", secret)
	line("Setup keeps, no longer secret", shown)
	added, removed = diffKeys(actionItems(from), actionItems(to))
	line("Actions added", added)
	line("Actions removed", removed)
	added, removed = diffKeys(accessItems(from), accessItems(to))
	line("Access form fields added", added)
	line("Access form fields removed", removed)
	return out
}

// diffItem is one thing a manifest has: the key it is matched by and
// the words it is named with.
type diffItem struct{ key, words string }

// diffKeys names the items of to whose key from does not have, and
// those of from whose key to does not have.
func diffKeys(from, to []diffItem) (added, removed []string) {
	has := func(items []diffItem, key string) bool {
		for _, it := range items {
			if it.key == key {
				return true
			}
		}
		return false
	}
	for _, it := range to {
		if !has(from, it.key) {
			added = append(added, it.words)
		}
	}
	for _, it := range from {
		if !has(to, it.key) {
			removed = append(removed, it.words)
		}
	}
	return added, removed
}

func hostItems(m *Manifest) []diffItem {
	out := make([]diffItem, 0, len(m.Kivali.Hosts))
	for _, h := range m.Kivali.Hosts {
		out = append(out, diffItem{h, h})
	}
	return out
}

func packageItems(m *Manifest) []diffItem {
	out := make([]diffItem, 0, len(m.Kivali.Packages))
	for _, p := range m.Kivali.Packages {
		out = append(out, diffItem{p, p})
	}
	return out
}

// signInHostItems are the hosts of m's OAuth endpoints
// (Connection.SignInHosts).
func signInHostItems(m *Manifest) []diffItem {
	hosts := m.Kivali.Connection.SignInHosts()
	out := make([]diffItem, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, diffItem{h, h})
	}
	return out
}

// connectionItems are the connection's fields by title, a secret one
// marked as the install screen marks it.
func connectionItems(m *Manifest) []diffItem {
	fields := ConnectionFields(m)
	out := make([]diffItem, 0, len(fields))
	for _, f := range fields {
		words := f.Title
		if words == "" {
			words = f.Key
		}
		if f.Sensitive {
			words += " (secret)"
		}
		out = append(out, diffItem{f.Key, words})
	}
	return out
}

// setupOf is m's setup declarations, nil for a connection that is not
// a setup one.
func setupOf(m *Manifest) *Setup {
	if c := m.Kivali.Connection; c.Method == MethodSetup {
		return c.Setup
	}
	return nil
}

func setupHostItems(m *Manifest) []diffItem {
	s := setupOf(m)
	if s == nil {
		return nil
	}
	out := make([]diffItem, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		out = append(out, diffItem{h, h})
	}
	return out
}

// setupFieldItems are a setup's fields by title, a secret one marked
// as the install screen marks it.
func setupFieldItems(m *Manifest) []diffItem {
	s := setupOf(m)
	if s == nil {
		return nil
	}
	out := make([]diffItem, 0, len(s.Fields))
	for _, f := range s.Fields {
		words := SetupFieldTitle(f)
		if f.Secret {
			words += " (secret)"
		}
		out = append(out, diffItem{f.Key, words})
	}
	return out
}

func setupKeepItems(m *Manifest) []diffItem {
	s := setupOf(m)
	if s == nil {
		return nil
	}
	out := make([]diffItem, 0, len(s.Keeps))
	for _, k := range s.Keeps {
		out = append(out, diffItem{k.Key, k.Key})
	}
	return out
}

// keepSecrecyChanges are the keys both setups keep whose secrecy
// changed, in to's order: those secret in to and not in from, and those
// secret in from and not in to.
func keepSecrecyChanges(from, to *Manifest) (secret, shown []string) {
	a, b := setupOf(from), setupOf(to)
	if a == nil || b == nil {
		return nil, nil
	}
	for _, k := range b.Keeps {
		if !a.Keeps.Has(k.Key) {
			continue
		}
		switch was, is := a.Keeps.Secret(k.Key), b.Keeps.Secret(k.Key); {
		case is && !was:
			secret = append(secret, k.Key)
		case was && !is:
			shown = append(shown, k.Key)
		}
	}
	return secret, shown
}

// SetupFieldTitle is what a setup field is called on the form: its
// title, or its key in words (personal_access_key is "Personal access
// key").
func SetupFieldTitle(f SetupField) string {
	if f.Title != "" {
		return f.Title
	}
	return actionWords(f.Key)
}

func actionItems(m *Manifest) []diffItem {
	out := make([]diffItem, 0, len(m.Tools))
	for _, t := range m.Tools {
		out = append(out, diffItem{t.Name, actionWords(t.Name)})
	}
	return out
}

func accessItems(m *Manifest) []diffItem {
	form := AccessForm(m)
	if form == nil {
		return nil
	}
	out := make([]diffItem, 0, len(form.Fields))
	for _, f := range form.Fields {
		out = append(out, diffItem{f.Key, f.Title})
	}
	return out
}

// actionWords is an action's name as the install screen shows it:
// create_post is "Create post".
func actionWords(name string) string {
	words := strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	r, n := utf8.DecodeRuneInString(words)
	if n == 0 {
		return words
	}
	return string(unicode.ToUpper(r)) + words[n:]
}
