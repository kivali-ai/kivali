package gitops

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Tools is clone, fetch and push as an agent is given them, unprefixed,
// worded for in: its display name, the host as it names it, whether a
// fetch and a push must name the repository, and whether a push may
// force. They are the same for every caller: the access is checked on
// each call, against the repository and the refs it names.
func Tools(in Integration) []builtinintegrations.Tool {
	return []builtinintegrations.Tool{
		{Name: Clone, Description: cloneDescription(in), InputSchema: cloneSchema(in)},
		{Name: Fetch, Description: fetchDescription(in), InputSchema: fetchSchema(in)},
		{Name: Push, Description: pushDescription(in), InputSchema: pushSchema(in)},
	}
}

func cloneDescription(in Integration) string {
	return fmt.Sprintf("Clones a repository with the owner's %s connection into path, a directory under /files or your home that does not exist yet or is empty. "+
		"url is the repository's HTTPS URL on %s, or owner/name. The clone's origin is the repository's URL without the token. "+
		"Afterwards use plain git in run_shell for everything local: status, branch, commit, diff, log, merge, rebase. "+
		"Plain git reaches only hosts on the team's allow list; this reaches %s with its token. Refused for a repository outside your access.",
		in.DisplayName, in.HostWords, in.HostWords)
}

func fetchDescription(in Integration) string {
	if in.NameRepository {
		return fmt.Sprintf("Fetches from the repository url names with the owner's %s connection into the working copy at path, updating refs/remotes/origin/* and tags; then merge, rebase or check out with plain git. "+
			"url is the repository's HTTPS URL on %s, or owner/name. refs are refspecs; empty fetches every branch. Refused for a repository outside your access.",
			in.DisplayName, in.HostWords)
	}
	return fmt.Sprintf("Fetches from the repository's origin with the owner's %s connection into the working copy at path, updating refs/remotes/origin/* and tags; then merge, rebase or check out with plain git. "+
		"refs are refspecs; empty fetches every branch. url, when given, is the repository's HTTPS URL on %s, or owner/name; empty, the repository is the working copy's origin. Refused for a repository outside your access.",
		in.DisplayName, in.HostWords)
}

func pushDescription(in Integration) string {
	var b strings.Builder
	to := "its origin"
	if in.NameRepository {
		to = "the repository url names"
	}
	fmt.Fprintf(&b, "Pushes refs from the working copy at path to %s with the owner's %s connection. ", to, in.DisplayName)
	b.WriteString("Each entry is a branch, a tag, or a refspec such as HEAD:refs/heads/agent/x; the destination must be one the owner allowed for you. " +
		"A name without refs/ is a branch (agent/x is refs/heads/agent/x); a tag is written tags/v1.2.0 or refs/tags/v1.2.0. ")
	if in.ForceSwitch {
		b.WriteString("force, or a + in front of an entry, overwrites the remote ref and needs the owner's separate permission. ")
	} else {
		b.WriteString("A push never overwrites the remote's history: a + in front of an entry is refused. ")
	}
	b.WriteString("Deleting a remote ref is refused. ")
	if in.NameRepository {
		fmt.Fprintf(&b, "url is the repository's HTTPS URL on %s, or owner/name. ", in.HostWords)
	} else {
		fmt.Fprintf(&b, "url, when given, is the repository's HTTPS URL on %s, or owner/name; empty, the repository is the working copy's origin. ", in.HostWords)
	}
	if in.ForceSwitch {
		b.WriteString("Refused for a repository, a ref or a force push outside your access.")
	} else {
		b.WriteString("Refused for a repository or a ref outside your access.")
	}
	return b.String()
}

// The schemas' properties are written in order (a map would sort
// them), with each description a JSON string.

func cloneSchema(in Integration) json.RawMessage {
	return schema([]property{
		{"url", `{"type":"string","description":` + jsonString("the repository's HTTPS URL on "+in.HostWords+", or owner/name") + `}`},
		{"path", `{"type":"string","description":"a directory under /files or your home (~/...) that does not exist yet or is empty"}`},
	}, "url", "path")
}

func fetchSchema(in Integration) json.RawMessage {
	props := []property{
		{"path", `{"type":"string","description":"the working copy, under /files or your home (~/...)"}`},
		{"url", urlProperty(in)},
		{"refs", `{"type":"array","items":{"type":"string"},"description":"optional refspecs, each writing under refs/remotes/origin/ or refs/tags/, such as +refs/heads/main:refs/remotes/origin/main"}`},
	}
	if in.NameRepository {
		return schema(props, "path", "url")
	}
	return schema(props, "path")
}

func pushSchema(in Integration) json.RawMessage {
	props := []property{
		{"path", `{"type":"string","description":"the working copy, under /files or your home (~/...)"}`},
		{"refs", `{"type":"array","items":{"type":"string"},"description":"what to push: a branch (agent/x), a tag (tags/v1.2.0) or a refspec such as HEAD:refs/heads/agent/x"}`},
	}
	if in.ForceSwitch {
		props = append(props, property{"force", `{"type":"boolean","description":"optional: overwrite the remote refs even where the push is not a fast-forward"}`})
	}
	props = append(props, property{"url", urlProperty(in)})
	if in.NameRepository {
		return schema(props, "path", "refs", "url")
	}
	return schema(props, "path", "refs")
}

// urlProperty is a fetch's or a push's url: optional, the working
// copy's origin when empty, unless in must name the repository.
func urlProperty(in Integration) string {
	words := "optional: the repository's HTTPS URL on " + in.HostWords + ", or owner/name; empty, the working copy's origin"
	if in.NameRepository {
		words = "the repository's HTTPS URL on " + in.HostWords + ", or owner/name"
	}
	return `{"type":"string","description":` + jsonString(words) + `}`
}

type property struct{ name, schema string }

func schema(props []property, required ...string) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i, p := range props {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(jsonString(p.name) + ":" + p.schema)
	}
	b.WriteString(`},"required":[`)
	for i, r := range required {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(jsonString(r))
	}
	b.WriteString("]}")
	return json.RawMessage(b.String())
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
