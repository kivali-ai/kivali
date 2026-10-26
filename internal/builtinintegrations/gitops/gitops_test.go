package gitops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// forge is a built-in shaped as Git (Basic) is, under a name of its
// own, so words that name the integration show where they come from.
func forge() Integration {
	return Integration{Name: "forge", DisplayName: "Forge", HostWords: "the connection's host", ForceSwitch: true}
}

// hub is a built-in shaped as GitHub's clone, fetch and push are: a
// fixed host, a repository named in every call, never a force.
func hub() Integration {
	return Integration{Name: "hub", DisplayName: "Hub", HostWords: "hub.example.com", NameRepository: true}
}

// pusher is a policy that may push agent/* and every tag to acme/*,
// and force when force is set.
func pusher(force bool) Policy {
	return Policy{Integration: forge(), Host: "git.example.com", Push: []string{"acme/*"},
		Refs: []string{"refs/heads/agent/*", "refs/tags/*"}, Force: force}
}

// pullOnly is p with the repositories it may push to moved to the ones
// it may only pull.
func pullOnly(p Policy) Policy {
	p.Pull, p.Push = p.Push, nil
	return p
}

// anyRef is a Hub policy that may push any branch or tag to acme/*.
func anyRef() Policy {
	return Policy{Integration: hub(), Host: "hub.example.com", Pull: []string{"acme/*"}, Push: []string{"acme/*"}, AnyRef: true}
}

func refusal(t *testing.T, err error) *builtinintegrations.Refusal {
	t.Helper()
	var ref *builtinintegrations.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return ref
}

// Why this test exists: the access can lose a repository to push to,
// or narrow the refs it may push, while a push runs. The remote is
// written all the same, so the second request must not be audited as
// refused: the row must be allowed and name what landed (only the refs
// the call asked for, never one the shell's report adds), and the agent
// must read what landed and that its access changed, naming the
// integration by its own name.
func TestCompletePushWhenTheAccessChangedDuringIt(t *testing.T) {
	args := json.RawMessage(`{"path":"/files/w","refs":["agent/x"]}`)
	landed := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/agent/x", New: "1111111aaaa"},
		{Ref: "refs/heads/main", Old: "2222222bbbb", New: "3333333cccc"},
	}}}
	for name, p := range map[string]Policy{
		"push taken away":   pullOnly(pusher(false)),
		"the access unread": {Integration: forge(), Refused: &builtinintegrations.Refusal{Text: "unread", Reason: "unread"}},
	} {
		res, err := Complete(Push, args, p, "c1", landed)
		if err != nil || res.IsError {
			t.Fatalf("%s: landed under a changed access = %+v, %v", name, res, err)
		}
		if res.Audit != "pushed refs/heads/agent/x to acme/widgets; the access changed during the push" ||
			!strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/x (new) to acme/widgets. Your access to the Forge integration changed during the push") ||
			strings.Contains(res.Content[0], "refs/heads/main") {
			t.Errorf("%s: landed under a changed access = %+v", name, res)
		}
	}
	// A clone is never told as landed: the refusal stands.
	if _, err := Complete(Clone, json.RawMessage(`{"url":"acme/widgets","path":"w"}`), pullOnly(Policy{Integration: forge(), Host: "git.example.com"}), "c1", landed); err == nil {
		t.Error("a clone outside the access was told as done")
	}
}

// Why this test exists: the result is the agent's pod's own report. A
// push's answer and audit must name only the destinations core asked
// for, so a report that adds a ref cannot write a row saying it was
// pushed; and when the daemon cut its list short (UpdatedTotal above
// the refs listed), the count must still say how many moved.
func TestPushedNamesOnlyWhatWasAsked(t *testing.T) {
	args := json.RawMessage(`{"path":"/files/w","url":"acme/widgets","refs":["agent/x"]}`)
	extra := shelljob.Result{Git: &shelljob.GitResult{Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/main", New: "3333333cccc"},
		{Ref: "refs/heads/agent/x", New: "1111111aaaa"},
	}}}
	res, _ := Complete(Push, args, pusher(false), "c1", extra)
	if res.Audit != "acme/widgets: pushed refs/heads/agent/x" || strings.Contains(res.Content[0], "main") {
		t.Errorf("a report with a ref not asked for = %+v", res)
	}
	only := shelljob.Result{Git: &shelljob.GitResult{Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/main", New: "3333333cccc"}}}}
	res, _ = Complete(Push, args, pusher(false), "c1", only)
	if !strings.HasPrefix(res.Content[0], "Nothing to push") {
		t.Errorf("a report of only refs not asked for = %+v", res)
	}

	many := json.RawMessage(`{"path":"/files/w","url":"acme/widgets","refs":["agent/a","agent/b","agent/c"]}`)
	cut := shelljob.Result{Git: &shelljob.GitResult{UpdatedTotal: 3, Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/agent/a", New: "1111111aaaa"}}}}
	res, _ = Complete(Push, many, pusher(false), "c1", cut)
	if !strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/a (new) and 2 more to acme/widgets.") ||
		res.Audit != "acme/widgets: pushed refs/heads/agent/a and 2 more" {
		t.Errorf("a cut list = %+v", res)
	}
}

// Why this test exists: core writes an audit row as it hands a job
// out, so a push whose second request never comes is still on record.
// For a push that row must name the destinations and whether it
// forces, from core's own reading of the refs (the shell has not run
// yet, and its report is the agent's pod's).
func TestPushStartedRow(t *testing.T) {
	c, err := Check(Push, json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["agent/x","+HEAD:tags/v2"]}`), pusher(true))
	if err != nil || c.Started() != "started: acme/widgets, force push of refs/heads/agent/x, refs/tags/v2" {
		t.Errorf("started = %q, %v", c.Started(), err)
	}
	c, _ = Check(Push, json.RawMessage(`{"path":"w","refs":["agent/x"]}`), pusher(false))
	if c.Started() != "started: the working copy's origin at w, push of refs/heads/agent/x" {
		t.Errorf("started without a url = %q", c.Started())
	}
}

// Why this test exists: the words are shared by every built-in that
// runs these actions, so each must name the built-in it is for, by its
// display name and by its own tools' names, and never another's: a
// refusal saying "Git" to an agent calling another built-in would send
// it to the wrong owner's page or the wrong tool. A job that ran
// through a connection the built-in no longer has must say so in its
// name too.
func TestWordsNameTheIntegration(t *testing.T) {
	for name, c := range map[string]struct {
		action, args string
		p            Policy
		words        string
	}{
		"no action":     {"pull", `{}`, pusher(false), "The Forge integration has no action pull."},
		"bad args":      {Push, `{"path":1}`, pusher(false), "forge_push takes path and refs, a list of strings, and optionally force (true or false) and url."},
		"fetch args":    {Fetch, `{"path":1}`, pusher(false), "forge_fetch takes path, and optionally url and refs, a list of strings."},
		"clone args":    {Clone, `{"url":"acme/x"}`, pusher(false), "forge_clone needs url and path."},
		"outside":       {Clone, `{"url":"other/x","path":"w"}`, pusher(false), "The Forge integration's access for you does not include other/x. Ask the owner if your work needs it."},
		"pull only":     {Push, `{"path":"w","url":"acme/x","refs":["agent/x"]}`, Policy{Integration: forge(), Host: "git.example.com", Pull: []string{"acme/*"}, Push: []string{"other/*"}, Refs: []string{"refs/heads/agent/*"}}, "The Forge integration's access for you lets you pull acme/x but not push to it."},
		"no push":       {Push, `{"path":"w","refs":["agent/x"]}`, pullOnly(pusher(false)), "The Forge integration's access for you does not allow pushing."},
		"other host":    {Clone, `{"url":"https://gitlab.com/acme/x","path":"w"}`, pusher(false), "The Forge integration's connection is for git.example.com"},
		"deletion":      {Push, `{"path":"w","refs":[":agent/x"]}`, pusher(true), "Deleting a remote ref is not something the Forge integration does."},
		"force off":     {Push, `{"path":"w","refs":["+agent/x"]}`, pusher(false), "The Forge integration's access for you does not allow force-pushing."},
		"twice":         {Push, `{"path":"w","refs":["agent/x","HEAD:agent/x"]}`, pusher(false), "forge_push names refs/heads/agent/x twice"},
		"does not read": {Push, `{"path":"w","refs":["agent/x..y"]}`, pusher(false), "forge_push cannot read"},
	} {
		_, err := Check(c.action, json.RawMessage(c.args), c.p)
		if ref := refusal(t, err); !strings.HasPrefix(ref.Text, c.words) || strings.Contains(ref.Text, "Git") {
			t.Errorf("%s: %q, want it to start %q", name, ref.Text, c.words)
		}
	}

	res, err := Complete(Clone, json.RawMessage(`{"url":"acme/widgets","path":"/files/w"}`), pusher(false), "c1",
		shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets"}})
	if err != nil || !strings.Contains(res.Content[0], "and forge_fetch to fetch with the connection") {
		t.Errorf("a clone's answer = %+v, %v", res, err)
	}
	res, err = Complete(Clone, json.RawMessage(`{"url":"acme/widgets","path":"/files/w"}`), pusher(false), "c2",
		shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", ConnectionID: "c1"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0], "The Forge connection changed during the clone") ||
		res.Audit != "acme/widgets: up to date; the connection changed during the clone" {
		t.Errorf("a job through a replaced connection = %+v, %v", res, err)
	}
}

// Why this test exists: Policy.Refused is how a built-in says its
// connection or the caller's access grants nothing. Every action must
// be refused with exactly the built-in's refusal, before the arguments
// are read (so its words, not an argument's, reach the agent), and an
// action that is no action is still refused as one.
func TestRefusedRefusesEveryAction(t *testing.T) {
	why := &builtinintegrations.Refusal{Text: "The Forge integration's connection has no token. Ask the owner to connect it again.", Reason: "the connection has no token"}
	p := Policy{Integration: forge(), Refused: why}
	for _, action := range []string{Clone, Fetch, Push} {
		if _, err := Check(action, json.RawMessage(`{"nonsense":true}`), p); refusal(t, err) != why {
			t.Errorf("%s = %v", action, err)
		}
	}
	if _, err := Check("pull", json.RawMessage(`{}`), p); refusal(t, err).Reason != "no such action" {
		t.Errorf("an unknown action = %v", err)
	}
}

// Why this test exists: a built-in whose credential is narrowed to the
// repository a call names cannot let the daemon read the working
// copy's origin, since nothing would then narrow it. A fetch or a push
// without a url must be refused before any job, in words saying to
// name the repository; with one, the job must carry its URL; and the
// tools the agent is listed must ask for url and never promise the
// origin.
func TestNameRepository(t *testing.T) {
	p := anyRef()
	for action, args := range map[string]string{
		Fetch: `{"path":"w"}`,
		Push:  `{"path":"w","refs":["agent/x"]}`,
	} {
		_, err := Check(action, json.RawMessage(args), p)
		ref := refusal(t, err)
		if ref.Text != "hub_"+action+" needs url: name the repository, as its HTTPS URL on hub.example.com or owner/name." || ref.Reason != "the arguments lack url" {
			t.Errorf("%s without a url = %+v", action, ref)
		}
	}
	c, err := Check(Fetch, json.RawMessage(`{"path":"w","url":"acme/widgets"}`), p)
	if repo, ok := c.Repository(); err != nil || !ok || repo.Path != "acme/widgets" ||
		c.Job(shelljob.GitAuth{}, "c1").URL != "https://hub.example.com/acme/widgets.git" {
		t.Errorf("a fetch with a url = %+v, %v", c, err)
	}
	if _, err := Check(Push, json.RawMessage(`{"path":"w","refs":["agent/x"],"force":false}`), p); !strings.HasPrefix(refusal(t, err).Text, "hub_push takes path, url and refs, a list of strings.") {
		t.Errorf("a push with force = %v", err)
	}

	for _, tl := range Tools(hub()) {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		if strings.Contains(tl.Description, "its origin") || strings.Contains(tl.Description, "working copy's origin") ||
			strings.Contains(string(tl.InputSchema), "origin\"") || !strings.Contains(strings.Join(s.Required, ","), "url") ||
			!strings.Contains(tl.Description, "Hub connection") || !strings.Contains(tl.Description, "hub.example.com") {
			t.Errorf("%s: %q %s", tl.Name, tl.Description, tl.InputSchema)
		}
	}
}

// Why this test exists: a built-in whose access grants pushing without
// naming branches and tags must let a push write any branch or tag,
// nested ones too, which no pattern can say: core must take it and
// hand the daemon a job that says so (AnyBranchOrTag, with no
// AllowedRefs), or the daemon would refuse it. It must widen nothing
// else: a ref outside branches and tags, a deletion and a repository
// outside the access are still refused.
func TestAnyRef(t *testing.T) {
	c, err := Check(Push, json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["feature/a/b","tags/release/v1","HEAD:main"]}`), anyRef())
	if err != nil {
		t.Fatal(err)
	}
	j := c.Job(shelljob.GitAuth{Username: "u", Password: "p"}, "c1")
	if !j.AnyBranchOrTag || j.AllowedRefs != nil || j.Force || j.Auth.Password != "p" || j.ConnectionID != "c1" ||
		strings.Join(j.RefSpecs, " ") != "refs/heads/feature/a/b:refs/heads/feature/a/b refs/tags/release/v1:refs/tags/release/v1 HEAD:refs/heads/main" {
		t.Errorf("job = %+v", j)
	}
	for name, args := range map[string]string{
		"a note":     `{"path":"w","url":"acme/widgets","refs":["HEAD:refs/notes/x"]}`,
		"a deletion": `{"path":"w","url":"acme/widgets","refs":[":main"]}`,
		"outside":    `{"path":"w","url":"other/x","refs":["main"]}`,
	} {
		if _, err := Check(Push, json.RawMessage(args), anyRef()); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	// Without AnyRef, a policy with no refs pushes none.
	p := anyRef()
	p.AnyRef = false
	if _, err := Check(Push, json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["main"]}`), p); refusal(t, err).Reason != "the access does not include refs/heads/main" {
		t.Errorf("no refs = %v", err)
	}
	// The job of a push held to patterns carries them.
	c, _ = Check(Push, json.RawMessage(`{"path":"w","refs":["agent/x"]}`), pusher(false))
	if j := c.Job(shelljob.GitAuth{}, "c1"); j.AnyBranchOrTag || strings.Join(j.AllowedRefs, " ") != "refs/heads/agent/* refs/tags/*" {
		t.Errorf("a job held to patterns = %+v", j)
	}
}

// Why this test exists: a built-in that never force-pushes has no
// switch the owner could turn on, so its words must not send the agent
// to ask for one, and its push must not take force at all: the schema
// has no force, force: false is refused as an argument it does not
// take, and force: true and a + are refused as something it does not
// do, even should its policy say Force. force: true read as an unknown
// argument would tell the agent only that the call is malformed, and it
// might try again another way.
func TestNoForceSwitch(t *testing.T) {
	p := anyRef()
	p.Force = true
	for _, args := range []string{
		`{"path":"w","url":"acme/widgets","refs":["+main"]}`,
		`{"path":"w","url":"acme/widgets","refs":["main"],"force":true}`,
	} {
		_, err := Check(Push, json.RawMessage(args), p)
		if ref := refusal(t, err); ref.Text != "Force-pushing is not something the Hub integration does." || ref.Reason != "the call forces a push" {
			t.Errorf("%s = %+v", args, ref)
		}
	}
	_, err := Check(Push, json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["main"],"force":false}`), p)
	if ref := refusal(t, err); ref.Text != "hub_push takes path, url and refs, a list of strings." || ref.Reason != "the arguments do not read" {
		t.Errorf("force: false = %+v", ref)
	}
	push := Tools(hub())[2]
	if strings.Contains(string(push.InputSchema), "force") || strings.Contains(push.Description, "permission") ||
		!strings.Contains(push.Description, "a + in front of an entry is refused") {
		t.Errorf("push = %q %s", push.Description, push.InputSchema)
	}
	if push := Tools(forge())[2]; !strings.Contains(string(push.InputSchema), `"force"`) || !strings.Contains(push.Description, "needs the owner's separate permission") {
		t.Errorf("a push with a force switch = %q %s", push.Description, push.InputSchema)
	}
}
