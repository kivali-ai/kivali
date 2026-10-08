package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SubagentOverlay is the on-disk shape the subagent runs against. It
// lives under the parent's /files/ tree at
// agents/<parent>/<storage>/subagents/<id>/ and mirrors the parent's
// layout via relative symlinks — same /files/ namespace as the parent
// (per docs/developers/files-and-publishing.md) so file_view paths and shell paths
// resolve to the same physical bytes.
//
// The subagent's file_* Backend (built by the dev-shell daemon for
// root "subagents/<id>") is rooted at this directory; run_shell's cwd
// defaults to it. Symlink targets are relative so the
// overlay resolves correctly when reached through either the agent
// pod's /files/ mount or the on-disk path on core.
//
// Built once per subagent run by SubagentService.runOne; left in place
// after the run so the parent can read whatever the subagent wrote
// under artifacts/private/. Periodic GC removes stale overlays — see
// docs/developers/files-and-publishing.md §"Open questions".
type SubagentOverlay struct {
	// Root is the absolute host path the overlay is built at, e.g.
	// data/agents/<parent>/<storage>/subagents/<id>/.
	Root string
}

// SubagentParentRoot returns the parent's /files/ root for an overlay
// rooted at overlayRoot — the directory its relative links resolve
// into (overlayRoot is <parent>/subagents/<id>). A subagent's Backend
// names it as a WriteRoot: background/ and artifacts/public/ are the
// parent's, reached through those links.
func SubagentParentRoot(overlayRoot string) string {
	return filepath.Dir(filepath.Dir(filepath.Clean(overlayRoot)))
}

// BuildSubagentOverlay creates the directory and symlink farm for one
// subagent run. Idempotent: re-calling on an existing overlay leaves
// it intact (any user-written artifacts/private/ content survives).
//
// Symlink layout — all targets are relative to the symlink location
// so the overlay works through any mount path that exposes the parent
// /files/ tree (the agent pod's /files/ mount, the on-disk core path,
// a test temp dir, etc.):
//
//	subagents/<id>/artifacts/private/    real dir (subagent's scratch)
//	subagents/<id>/artifacts/public      → ../../../artifacts/public
//	subagents/<id>/artifacts/shared      → ../../../artifacts/shared
//	subagents/<id>/project               → ../../project
//	subagents/<id>/skills                → ../../skills
//	subagents/<id>/attachments           → ../../attachments
//	subagents/<id>/background           → ../../background   (WRITABLE)
//
// background/ is the one place siblings meet. Everything else in this
// layout is either private to the subagent or read-only, which was the
// right default while a subagent was a leaf doing one lookup; it is
// the wrong default once several of them are working on parts of the
// same deliverable and one of them is a sub-lead that has to collect
// the pieces. They share a directory rather than messages on purpose:
// files are already the medium every tool addresses, the result is
// inspectable by the CEO in the UI and by any agent via file_view, it
// appends rather than races, and it cannot deadlock the way N-way
// messaging between blocked processes can.
//
// artifacts/shared/ is linked read-only because subagents have the
// knowledge-graph tools, and graph_node prints a peer's node as
// /files/artifacts/shared/<owner>/<path>: a path the tool hands out
// has to resolve where the tool's caller runs. Excluded by design:
// past-chats/ and episodes/ — the parent's memory is the parent's.
func BuildSubagentOverlay(layout SubagentOverlay) error {
	if layout.Root == "" {
		return errors.New("BuildSubagentOverlay: Root is required")
	}
	// Core builds this inside the parent's writable tree, where the
	// agent could have left a link at subagents/ or subagents/<id>/.
	// Those are made real directories first, and every removal goes
	// through an os.Root on the parent tree, so a link cannot carry
	// core's writes into someone else's directory.
	parentRoot := SubagentParentRoot(layout.Root)
	rel, err := filepath.Rel(parentRoot, filepath.Clean(layout.Root))
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	// The shared workspace must exist before the link points at it.
	// Sync creates it for every agent, but an overlay can be built for
	// a subagent before that has run, and a dangling background/ symlink
	// fails writes with ENOENT — which the model would read as "I may
	// not write here" rather than "this is not set up yet".
	if err := os.MkdirAll(parentRoot, 0o755); err != nil {
		return err
	}
	if err := ensureDir(parentRoot, SharedWorkspaceDir); err != nil {
		return fmt.Errorf("%s: %w", SharedWorkspaceDir, err)
	}
	if err := ensureRealDir(parentRoot, rel+"/artifacts/private"); err != nil {
		return fmt.Errorf("artifacts/private: %w", err)
	}
	proot, err := os.OpenRoot(parentRoot)
	if err != nil {
		return err
	}
	defer func() { _ = proot.Close() }()
	links := map[string]string{
		"project":          "../../project",
		"skills":           "../../skills",
		"attachments":      "../../attachments",
		"artifacts/public": "../../../artifacts/public",
		"artifacts/shared": "../../../artifacts/shared",
		// Writable by omission rather than by exception: background/ is
		// simply not in DefaultReadOnlyRoots. Adding it there would
		// silently turn the shared workspace into a second read-only
		// mirror and the sub-lead pattern would stop working with no
		// error anyone could see.
		SharedWorkspaceDir: "../../" + SharedWorkspaceDir,
	}
	for name, target := range links {
		dst := filepath.Join(layout.Root, name)
		// If the symlink already points at the right target, leave it.
		if existing, err := os.Readlink(dst); err == nil && existing == target {
			continue
		}
		// Drop any stale entry (file, empty dir, or wrong-target
		// symlink) so the next Symlink call doesn't fail with EEXIST.
		_ = proot.Remove(rel + "/" + name)
		// By path: os.Root has Symlink only from Go 1.25. The parent
		// directories were just made real; a link swapped in since can
		// misplace this one symlink, never remove or overwrite anything.
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", name, target, err)
		}
	}
	return nil
}
