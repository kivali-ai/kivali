package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestShellToolNamesTheReportedPackages(t *testing.T) {
	d := ShellToolWithPackages([]string{"bash", "fd-find", "git", "imagemagick", "python3-pip", "ripgrep"}).Description
	want := "Preinstalled packages: bash, fd-find (fd), git, imagemagick (magick, convert), python3-pip (pip3), ripgrep (rg)."
	if !strings.Contains(d, want) {
		t.Errorf("description lacks %q:\n%s", want, d)
	}
	if !strings.Contains(d, "DEV_SHELL_EXTRA_PACKAGES") {
		t.Errorf("description does not name the build arg:\n%s", d)
	}
}

// Nothing the description names may come from a hand-written list.
func TestShellToolNamesNothingItWasNotTold(t *testing.T) {
	d := ShellToolWithPackages([]string{"bash"}).Description
	for _, s := range []string{"postgresql", "sqlite3", "ffmpeg", "golang", "build-essential", "gcc", "pandoc", "fd-find", "(rg)"} {
		if strings.Contains(d, s) {
			t.Errorf("description names %q, which is not in the reported list:\n%s", s, d)
		}
	}
}

func TestShellToolFallsBackWithoutAList(t *testing.T) {
	for _, d := range []string{ShellTool().Description, ShellToolWithPackages([]string{}).Description} {
		if !strings.Contains(d, shellToolkitUnknown) {
			t.Errorf("description lacks the fallback sentence:\n%s", d)
		}
		if strings.Contains(d, "Preinstalled packages") {
			t.Errorf("fallback description claims a package list:\n%s", d)
		}
	}
}

type listerExec struct {
	pkgs []string
	err  error
}

func (l *listerExec) Exec(context.Context, string, ShellRequest) (*ShellResult, error) {
	return nil, errors.New("unused")
}
func (l *listerExec) SyncSkills(context.Context, string) error { return nil }
func (l *listerExec) Packages(context.Context) ([]string, error) {
	return l.pkgs, l.err
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

func (d *deadlineExec) Packages(ctx context.Context) ([]string, error) {
	d.deadline, d.hasDeadline = ctx.Deadline()
	return nil, nil
}

func TestShellPackages(t *testing.T) {
	ctx := context.Background()
	if got := ShellPackages(ctx, &listerExec{pkgs: []string{"git"}}); len(got) != 1 || got[0] != "git" {
		t.Errorf("lister: got %v, want [git]", got)
	}
	if got := ShellPackages(ctx, &listerExec{err: errors.New("down")}); got != nil {
		t.Errorf("failing lister: got %v, want nil", got)
	}
	if got := ShellPackages(ctx, plainExec{}); got != nil {
		t.Errorf("executor that cannot list: got %v, want nil", got)
	}
}
