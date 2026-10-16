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
// general task that carries web fields. Shared by both dispatch paths
// like validateTasks. The web-only checks need the parent: a file the
// task may upload must be a regular file in the parent's /files/.
func (s *SubagentService) validateWebTasks(parent string, tasks []SubagentTaskInput) error {
	for i, t := range tasks {
		switch t.Kind {
		case "", agentpod.SubagentKindGeneral:
			if t.Session != "" || t.StartURL != "" || len(t.Files) > 0 {
				return fmt.Errorf("tasks[%d]: session, start_url and files are for a web task; set kind to \"web\"", i)
			}
		case agentpod.SubagentKindWeb:
			if err := s.validateWebTask(parent, i, t); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tasks[%d].kind %q: \"general\" or \"web\"", i, t.Kind)
		}
	}
	return nil
}

func (s *SubagentService) validateWebTask(parent string, i int, t SubagentTaskInput) error {
	if s.Browsers == nil || !s.Browsers.Enabled() {
		return fmt.Errorf("tasks[%d]: web tasks are not available in this deployment", i)
	}
	if !t.hasSchema() {
		return fmt.Errorf("tasks[%d]: a web task needs a schema; its answer comes back as data, never as the page's text", i)
	}
	if t.session() != agentpod.OpenWebSession {
		if _, err := webSessionSite(t.session()); err != nil {
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
		if !s.parentFileExists(parent, p) {
			return fmt.Errorf("tasks[%d].files[%d]: %q is not a file under your /files/", i, j, p)
		}
	}
	return nil
}

// maxWebUploadBytes bounds a file a web task may upload to a page.
const maxWebUploadBytes = 50 << 20

// webSessionSite is the site a session name stands for: its registrable
// domain (vendor.com for shop.vendor.com), lower case, without a
// trailing dot. The open session has none. An address, a bare public
// suffix such as co.uk, and a name with a scheme, a path or a port are
// refused: a session is named by a site, not by a URL.
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
	if net.ParseIP(name) != nil {
		return "", errors.New("a site is a domain name, not an address")
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return "", fmt.Errorf("%q is not a site; give its registrable domain, such as vendor.com", name)
	}
	if err := store.ValidBrowserSessionName(site); err != nil {
		return "", fmt.Errorf("%q is not a site name core can keep", site)
	}
	return site, nil
}

// parentFileExists reports whether modelPath names a regular file the
// parent can read through its /files/, with file_view's confinement:
// a link that leaves the parent's read roots is no file.
func (s *SubagentService) parentFileExists(parent, modelPath string) bool {
	f, info, err := s.openParentFile(parent, modelPath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return info.Mode().IsRegular()
}

// openParentFile opens a file under the parent's /files/ the way
// file_view reads it: confined to the parent's tree and its read
// roots, links followed only within them.
func (s *SubagentService) openParentFile(parent, modelPath string) (*os.File, os.FileInfo, error) {
	backend := files.Dispatcher{DataDir: s.Store.Root()}.BackendFor(parent)
	return backend.OpenRead(modelPath)
}
