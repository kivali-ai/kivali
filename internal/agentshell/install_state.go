package agentshell

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Install is the state of the software install the daemon started
// with: running, then finished with an InstallResult. The handler
// reads it to make commands wait for the software, to report it on
// GET /v1/packages, and to tell the agent once about what failed.
//
// A nil *Install is a daemon with nothing to install: every method
// treats it as finished with nothing installed.
type Install struct {
	requested []string
	done      chan struct{}
	once      sync.Once

	mu          sync.Mutex
	result      InstallResult
	noticeTaken bool
}

// NewInstall returns the state of an install of pkgs that is running.
// Finish records its result.
func NewInstall(pkgs []string) *Install {
	return &Install{requested: append([]string{}, pkgs...), done: make(chan struct{})}
}

// FailedInstall returns a finished install in which every package
// failed for reason: the daemon could not run the installer at all.
func FailedInstall(pkgs []string, reason string) *Install {
	i := NewInstall(pkgs)
	i.Finish(failAll(pkgs, reason))
	return i
}

// Finish records the install's result and releases every waiter. Only
// the first call counts.
func (i *Install) Finish(res InstallResult) {
	i.once.Do(func() {
		i.mu.Lock()
		i.result = res
		i.mu.Unlock()
		close(i.done)
	})
}

// Done is closed when the install has finished.
func (i *Install) Done() <-chan struct{} {
	if i == nil {
		return closedChan
	}
	return i.done
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// Finished reports whether the install has finished.
func (i *Install) Finished() bool {
	select {
	case <-i.Done():
		return true
	default:
		return false
	}
}

// Wait blocks until the install finishes or ctx ends, returning
// ctx's error in the second case.
func (i *Install) Wait(ctx context.Context) error {
	select {
	case <-i.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Requested is the packages the install was started with.
func (i *Install) Requested() []string {
	if i == nil {
		return nil
	}
	return append([]string{}, i.requested...)
}

// State reports the install as GET /v1/packages does: the packages
// still being installed (all of them, until it finishes), the ones
// installed and the ones that failed.
func (i *Install) State() (installing, installed []string, failed []PackageFailure) {
	if i == nil {
		return nil, nil, nil
	}
	if !i.Finished() {
		return append([]string{}, i.requested...), nil, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return nil, append([]string{}, i.result.Installed...), append([]PackageFailure{}, i.result.Failed...)
}

// TakeNotice returns, once, the sentence telling the agent which
// software could not be installed and why. It is empty while the
// install runs, when nothing failed, and on every call after the one
// that returned it.
func (i *Install) TakeNotice() string {
	if i == nil || !i.Finished() {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.noticeTaken || len(i.result.Failed) == 0 {
		return ""
	}
	i.noticeTaken = true
	return FailureNotice(i.result.Failed)
}

// FailureNotice is the sentence naming the software that could not be
// installed in this shell, and why.
func FailureNotice(failed []PackageFailure) string {
	parts := make([]string, len(failed))
	for k, f := range failed {
		parts[k] = f.Name + " (" + f.Error + ")"
	}
	return "This software could not be installed when your shell started, so commands that need it will fail: " + strings.Join(parts, "; ") + "."
}

// mergePackages is the sorted union of the image's package list and
// the packages installed at start.
func mergePackages(image, installed []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range [][]string{image, installed} {
		for _, p := range list {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}
