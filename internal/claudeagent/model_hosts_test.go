package claudeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/clock"
)

// fakeSignIn is a sign-in the test changes: the status the CLI would
// report, and how many times it was asked.
type fakeSignIn struct {
	mu    sync.Mutex
	st    claudeauth.Status
	err   error
	calls int
}

func (f *fakeSignIn) status(context.Context) (claudeauth.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.st, f.err
}

func (f *fakeSignIn) set(st claudeauth.Status, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st, f.err = st, err
}

func (f *fakeSignIn) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func writeSettings(t *testing.T, home, body string) {
	t.Helper()
	p := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestHostWatch(home string, sign *fakeSignIn, clk clock.Clock) *modelHostWatch {
	w := newModelHostWatch("claude", home, clk)
	w.status = sign.status
	w.logf = func(string, ...any) {}
	return w
}

var anthropicHosts = []string{"api.anthropic.com", "platform.claude.com"}

func TestModelHostWatchReportsOnSignInChangeOnly(t *testing.T) {
	home := t.TempDir()
	sign := &fakeSignIn{st: claudeauth.Status{LoggedIn: true, APIProvider: "firstParty"}}
	w := newTestHostWatch(home, sign, clock.NewFake())
	var reports [][]string
	report := func(h []string) { reports = append(reports, h) }

	w.check(context.Background(), report)
	if len(reports) != 1 || !reflect.DeepEqual(reports[0], anthropicHosts) {
		t.Fatalf("first reports = %v", reports)
	}

	// No sign-in file changed: the CLI is not asked again.
	w.check(context.Background(), report)
	if sign.asked() != 1 || len(reports) != 1 {
		t.Fatalf("asked %d times, %d reports, with nothing changed", sign.asked(), len(reports))
	}

	// A switch to Bedrock writes the settings file.
	sign.set(claudeauth.Status{LoggedIn: true, APIProvider: "bedrock"}, nil)
	writeSettings(t, home, `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1","AWS_REGION":"us-east-1"}}`)
	w.check(context.Background(), report)
	want := []string{"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.amazonaws.com", "sts.amazonaws.com", "sts.us-east-1.amazonaws.com"}
	if len(reports) != 2 || !reflect.DeepEqual(reports[1], want) {
		t.Fatalf("after the switch, reports = %v", reports)
	}

	// A file rewritten with the same sign-in asks the CLI and reports
	// nothing new.
	writeSettings(t, home, `{"env":{"AWS_REGION":"us-east-1","CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	w.check(context.Background(), report)
	if sign.asked() != 3 || len(reports) != 2 {
		t.Fatalf("asked %d times, %d reports, for the same hosts", sign.asked(), len(reports))
	}
}

func TestModelHostWatchRetriesAFailedStatus(t *testing.T) {
	sign := &fakeSignIn{err: errors.New("claude: not found")}
	w := newTestHostWatch(t.TempDir(), sign, clock.NewFake())
	var reports [][]string
	report := func(h []string) { reports = append(reports, h) }
	w.check(context.Background(), report)
	if len(reports) != 0 {
		t.Fatalf("reported %v from a failed status", reports)
	}
	sign.set(claudeauth.Status{LoggedIn: true, APIProvider: "firstParty"}, nil)
	w.check(context.Background(), report)
	if len(reports) != 1 || !reflect.DeepEqual(reports[0], anthropicHosts) {
		t.Fatalf("reports = %v", reports)
	}
}

// run reports at start and then on its ticker, until its context ends.
func TestModelHostWatchRunsOnItsClock(t *testing.T) {
	home := t.TempDir()
	sign := &fakeSignIn{st: claudeauth.Status{LoggedIn: true, APIProvider: "firstParty"}}
	clk := clock.NewFake()
	w := newTestHostWatch(home, sign, clk)
	reports := make(chan []string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.run(ctx, 10*time.Second, func(h []string) { reports <- h })
		close(done)
	}()
	if got := <-reports; !reflect.DeepEqual(got, anthropicHosts) {
		t.Fatalf("first report = %v", got)
	}
	clk.BlockUntil(1)
	sign.set(claudeauth.Status{LoggedIn: true, APIProvider: "foundry"}, nil)
	writeSettings(t, home, `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"res"}}`)
	clk.Advance(10 * time.Second)
	if got := <-reports; !reflect.DeepEqual(got, []string{"login.microsoftonline.com", "res.services.ai.azure.com"}) {
		t.Fatalf("report after the tick = %v", got)
	}
	cancel()
	<-done
}
