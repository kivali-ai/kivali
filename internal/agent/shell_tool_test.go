package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShellToolNamesTheReportedPackages(t *testing.T) {
	d := ShellToolWithPackages(ShellSoftware{Packages: []string{"bash", "fd-find", "git", "imagemagick", "python3-pip", "ripgrep"}}).Description
	want := "Installed packages: bash, fd-find (fd), git, imagemagick (magick, convert), python3-pip (pip3), ripgrep (rg)."
	if !strings.Contains(d, want) {
		t.Errorf("description lacks %q:\n%s", want, d)
	}
}

// The software paragraph sends the agent to propose_software_install,
// keeps pip --user, and names no operator build step.
func TestShellToolSoftwareParagraph(t *testing.T) {
	d := ShellTool().Description
	for _, want := range []string{"propose_software_install", "Debian package names", "pip install --user", "your commands wait until the installation finishes"} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	for _, gone := range []string{"AGENT_SHELL_EXTRA_PACKAGES", "ceo_notification", "rebuild"} {
		if strings.Contains(d, gone) {
			t.Errorf("description still names %q:\n%s", gone, d)
		}
	}
}

// The network paragraph says the shell is held to the allow list and
// the web tools are not.
func TestShellToolNetworkParagraph(t *testing.T) {
	d := ShellTool().Description
	for _, want := range []string{"team's allow list", "you cannot read", "403", "WebFetch and WebSearch are not limited"} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
}

// A proxy refusal is named whichever client hit it and whichever
// stream it printed to; a site's own 403 is not.
func TestShellNetworkNotice(t *testing.T) {
	refused := map[string][2]string{
		"curl":      {"", "curl: (56) CONNECT tunnel failed, response 403\n"},
		"git":       {"", "fatal: unable to access 'https://python.org/x.git/': CONNECT tunnel failed, response 403\n"},
		"old git":   {"", "fatal: unable to access 'https://python.org/': Received HTTP code 403 from proxy after CONNECT\n"},
		"python":    {"", "urllib.error.URLError: <urlopen error Tunnel connection failed: 403 Forbidden>\n"},
		"on stdout": {"Tunnel connection failed: 403 Forbidden\n", ""},
	}
	for name, out := range refused {
		if got := ShellNetworkNotice(out[0], out[1]); got != ShellProxyRefusalNotice {
			t.Errorf("%s: notice = %q", name, got)
		}
	}
	for _, out := range []string{"", "HTTP/1.1 403 Forbidden\n", "curl: (22) The requested URL returned error: 403\n", "CONNECT tunnel failed, response 407\n"} {
		if got := ShellNetworkNotice("", out); got != "" {
			t.Errorf("%q: notice = %q, want none", out, got)
		}
	}
}

// Packages still being installed and ones that failed are named, with
// the reason for each failure. The description outlives the install, so
// what it says about one still running holds once it has finished.
func TestShellToolNamesInstallingAndFailed(t *testing.T) {
	d := ShellToolWithPackages(ShellSoftware{
		Packages:   []string{"bash"},
		Installing: []string{"ffmpeg", "poppler-utils"},
		Failed:     []ShellSoftwareFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}},
	}).Description
	for _, want := range []string{
		"Also installed when the shell starts: ffmpeg, poppler-utils. Commands wait until that installation finishes, and the first command after it says if any of it could not be installed.",
		"Could not be installed: nosuch (Unable to locate package nosuch).",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	for _, stale := range []string{"now", "currently", "being installed"} {
		if strings.Contains(strings.ToLower(shellToolkitText(ShellSoftware{Packages: []string{"bash"}, Installing: []string{"ffmpeg"}})), stale) {
			t.Errorf("the paragraph for a running install says %q, which goes stale once it finishes", stale)
		}
	}
	plain := ShellToolWithPackages(ShellSoftware{Packages: []string{"bash"}}).Description
	if strings.Contains(plain, "installed when the shell starts") || strings.Contains(plain, "Could not be installed") {
		t.Errorf("description with no install names one:\n%s", plain)
	}
}

// Nothing the description names may come from a hand-written list.
func TestShellToolNamesNothingItWasNotTold(t *testing.T) {
	d := ShellToolWithPackages(ShellSoftware{Packages: []string{"bash"}}).Description
	for _, s := range []string{"postgresql", "sqlite3", "ffmpeg", "golang", "build-essential", "gcc", "pandoc", "fd-find", "(rg)"} {
		if strings.Contains(d, s) {
			t.Errorf("description names %q, which is not in the reported list:\n%s", s, d)
		}
	}
}

func TestShellToolFallsBackWithoutAList(t *testing.T) {
	for _, d := range []string{ShellTool().Description, ShellToolWithPackages(ShellSoftware{Packages: []string{}}).Description} {
		if !strings.Contains(d, shellToolkitUnknown) {
			t.Errorf("description lacks the fallback sentence:\n%s", d)
		}
		if strings.Contains(d, "Installed packages") {
			t.Errorf("fallback description claims a package list:\n%s", d)
		}
	}
}

type listerExec struct {
	sw  ShellSoftware
	err error
}

func (l *listerExec) Exec(context.Context, string, ShellRequest) (*ShellResult, error) {
	return nil, errors.New("unused")
}
func (l *listerExec) SyncSkills(context.Context, string) error { return nil }
func (l *listerExec) Packages(context.Context) (ShellSoftware, error) {
	return l.sw, l.err
}

// plainExec is an executor with no Packages method.
type plainExec struct{}

func (plainExec) Exec(context.Context, string, ShellRequest) (*ShellResult, error) {
	return nil, errors.New("unused")
}
func (plainExec) SyncSkills(context.Context, string) error { return nil }

// The fetch must end before the CLI gives up on the MCP server (30s), so
// ShellPackages bounds it at 20s: the deadline the executor sees is set
// and no more than 20s away.
func TestShellPackagesIsBoundedAt20Seconds(t *testing.T) {
	l := &deadlineExec{}
	before := time.Now()
	ShellPackages(context.Background(), l)
	if !l.hasDeadline {
		t.Fatal("ShellPackages put no deadline on the context")
	}
	if l.deadline.After(before.Add(20*time.Second + time.Second)) {
		t.Errorf("deadline %v is more than 20s from %v", l.deadline, before)
	}
}

type deadlineExec struct {
	plainExec
	hasDeadline bool
	deadline    time.Time
}

func (d *deadlineExec) Packages(ctx context.Context) (ShellSoftware, error) {
	d.deadline, d.hasDeadline = ctx.Deadline()
	return ShellSoftware{}, nil
}

func TestShellPackages(t *testing.T) {
	ctx := context.Background()
	sw := ShellSoftware{Packages: []string{"git"}, Installing: []string{"ffmpeg"}}
	if got := ShellPackages(ctx, &listerExec{sw: sw}); !reflect.DeepEqual(got, sw) {
		t.Errorf("lister: got %+v, want %+v", got, sw)
	}
	if got := ShellPackages(ctx, &listerExec{sw: sw, err: errors.New("down")}); !reflect.DeepEqual(got, ShellSoftware{}) {
		t.Errorf("failing lister: got %+v, want zero", got)
	}
	if got := ShellPackages(ctx, plainExec{}); !reflect.DeepEqual(got, ShellSoftware{}) {
		t.Errorf("executor that cannot list: got %+v, want zero", got)
	}
}
