// Package web (subagent_web_validate.go) — what a web task must look
// like before anything is spawned, beyond what every task needs.

package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// Limits on a web task's dispatch fields.
const (
	maxWebTaskFiles    = 10
	maxWebStartURLSize = 2048
)

// validateWebTasks refuses a batch whose web tasks cannot run, and a
// general task that carries web fields, for a dispatch by the agent
// itself. The web-only checks need the parent: a file the task may
// upload must be a regular file in the parent's /files/.
func (s *SubagentService) validateWebTasks(parent string, tasks []SubagentTaskInput) error {
	return s.validateWebTasksFor(parent, "", tasks)
}

// validateWebTasksFor is validateWebTasks for a dispatch by callerID,
// one of parent's subagents, or by parent itself when callerID is
// empty. A file is looked for in the caller's own /files/: a sub-lead
// names a path in its view, where artifacts/private is its own and the
// agent's memory is absent.
func (s *SubagentService) validateWebTasksFor(parent, callerID string, tasks []SubagentTaskInput) error {
	for i, t := range tasks {
		switch t.Kind {
		case "", agentpod.SubagentKindGeneral:
			if t.Session != "" || t.StartURL != "" || len(t.Files) > 0 {
				return fmt.Errorf("tasks[%d]: session, start_url and files are for a web task; set kind to \"web\"", i)
			}
		case agentpod.SubagentKindWeb:
			if err := s.validateWebTask(parent, callerID, i, t); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tasks[%d].kind %q: \"general\" or \"web\"", i, t.Kind)
		}
	}
	return nil
}

func (s *SubagentService) validateWebTask(parent, callerID string, i int, t SubagentTaskInput) error {
	if s.Browsers == nil || !s.Browsers.Enabled() {
		return fmt.Errorf("tasks[%d]: web tasks are not available in this deployment", i)
	}
	if !t.hasSchema() {
		return fmt.Errorf("tasks[%d]: a web task needs a schema; its answer comes back as data, never as the page's text", i)
	}
	var site string
	if t.session() != agentpod.OpenWebSession {
		var err error
		if site, err = webSessionSite(t.session()); err != nil {
			return fmt.Errorf("tasks[%d].session %q: %v", i, t.Session, err)
		}
	}
	if u := strings.TrimSpace(t.StartURL); u != "" {
		if len(u) > maxWebStartURLSize {
			return fmt.Errorf("tasks[%d].start_url is longer than %d characters", i, maxWebStartURLSize)
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("tasks[%d].start_url must be an http or https URL", i)
		}
		if site != "" && !onSite(site, parsed.Hostname()) {
			return fmt.Errorf("tasks[%d].start_url is off the session's site, %s; a task in a site session stays on it and its subdomains", i, site)
		}
	}
	if len(t.Files) > maxWebTaskFiles {
		return fmt.Errorf("tasks[%d].files has %d entries; the limit is %d", i, len(t.Files), maxWebTaskFiles)
	}
	seen := map[string]bool{}
	for j, p := range t.Files {
		if !strings.HasPrefix(p, files.ModelRootPath+"/") || path.Clean(p) != p {
			return fmt.Errorf("tasks[%d].files[%d]: %q must be an absolute, clean path under /files/", i, j, p)
		}
		name := path.Base(p)
		if seen[name] {
			return fmt.Errorf("tasks[%d].files: two files are named %q; the task uploads files by name", i, name)
		}
		seen[name] = true
		if !s.callerFileExists(parent, callerID, p) {
			return fmt.Errorf("tasks[%d].files[%d]: %q is not a file under your /files/", i, j, p)
		}
	}
	return nil
}

// maxWebUploadBytes bounds a file a web task may upload to a page.
const maxWebUploadBytes = 50 << 20

// webSessionSite is the site a session name stands for: its registrable
// domain (vendor.com for shop.vendor.com), lower case, without a
// trailing dot. The open session has none. An address in any spelling
// a browser reads as one, a bare public suffix such as co.uk, a name
// under no public top-level domain (localhost, corp, a misspelt
// suffix), and a name with a scheme, a path or a port are refused: a
// session is named by a site on the public web, not by a URL.
func webSessionSite(session string) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(session), "."))
	if name == agentpod.OpenWebSession {
		return "", nil
	}
	if name == "" {
		return "", errors.New("a session is \"open\" or a site such as vendor.com")
	}
	if strings.ContainsAny(name, "/:@?# \\") {
		return "", errors.New("a site is a bare domain name, such as vendor.com, without a scheme, a port or a path")
	}
	if readsAsAddress(name) {
		return "", errors.New("a site is a domain name, not an address")
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return "", fmt.Errorf("%q is not a site; give its registrable domain, such as vendor.com", name)
	}
	// The public suffix list answers for any name, with the last label
	// as the suffix of one it does not know; only a top-level domain
	// ICANN delegates is a site on the public web.
	if _, icann := publicsuffix.PublicSuffix(name[strings.LastIndex(name, ".")+1:]); !icann {
		return "", fmt.Errorf("%q is not a site on the public web; give its registrable domain, such as vendor.com", name)
	}
	if err := store.ValidBrowserSessionName(site); err != nil {
		return "", fmt.Errorf("%q is not a site name core can keep", site)
	}
	return site, nil
}

// readsAsAddress reports whether a browser would take name for an IP
// address rather than look it up: an address in any notation, or a
// name whose last label is a number (127.1, 0x7f.1, 0177.0.0.1 and
// 2130706433 all reach 127.0.0.1), as the URL standard's host parser
// reads it.
func readsAsAddress(name string) bool {
	if net.ParseIP(strings.Trim(name, "[]")) != nil {
		return true
	}
	last := name[strings.LastIndex(name, ".")+1:]
	if last == "" {
		return false
	}
	digits := "0123456789"
	if rest, ok := strings.CutPrefix(last, "0x"); ok {
		last, digits = rest, "0123456789abcdef"
		if last == "" {
			return true
		}
	}
	return strings.Trim(last, digits) == ""
}

// onSite reports whether host is site or a subdomain of it: the rule
// the pod applies to every navigation in a site session, applied by
// core before the command is relayed.
func onSite(site, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == site || strings.HasSuffix(host, "."+site)
}

// callerFileExists reports whether modelPath names a regular file the
// caller can read through its /files/, with file_view's confinement:
// a link that leaves the caller's read roots is no file.
func (s *SubagentService) callerFileExists(parent, callerID, modelPath string) bool {
	f, info, err := s.openCallerFile(parent, callerID, modelPath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return info.Mode().IsRegular()
}

// openCallerFile opens a file under the caller's /files/ the way
// file_view reads it: confined to the caller's tree and its read
// roots, links followed only within them. The caller is parent, or
// parent's subagent callerID, whose view is its overlay
// (files.Dispatcher.SubagentBackendFor).
func (s *SubagentService) openCallerFile(parent, callerID, modelPath string) (*os.File, os.FileInfo, error) {
	d := files.Dispatcher{DataDir: s.Store.Root()}
	backend := d.BackendFor(parent)
	if callerID != "" {
		backend = d.SubagentBackendFor(parent, callerID)
	}
	return backend.OpenRead(modelPath)
}
