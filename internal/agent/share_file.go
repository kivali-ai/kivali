package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// ResolvedShareFile is one file the share_file resolver has validated:
// SHA is the canonical content-addressed id; Name is the default
// display name (basename of the path, or the original upload name for
// project/attachment refs).
type ResolvedShareFile struct {
	SHA  string
	Name string
}

// ResolveShareFilePath validates that the agent may surface the
// referenced file and returns the canonical SHA + default display
// name. Path is one of:
//
//	/files/project/<name>      — project file, by symlink name
//	/files/attachments/<name>  — attachment in this agent's chat history
//	/files/artifacts/private/<path>
//	/files/artifacts/public/<path>
//
// The artifacts cases ingest the file, from the agent's writable
// workspace (private) or its published tree (public), via
// store.AddAttachment, returning the resulting content-addressed SHA. Idempotent: re-sharing identical
// bytes returns the same SHA.
//
// Package-level so the out-of-process MCP server (which only has a
// store handle) can reuse the exact same resolution + access rule
// as the in-runtime chat / release paths.
func ResolveShareFilePath(ctx context.Context, s *store.FSStore, slug, filePath string) (ResolvedShareFile, error) {
	if strings.TrimSpace(filePath) == "" {
		return ResolvedShareFile{}, fmt.Errorf("path is required")
	}
	sha, name, err := resolveByPath(ctx, s, slug, filePath)
	if err != nil {
		return ResolvedShareFile{}, err
	}
	return ResolvedShareFile{SHA: sha, Name: name}, nil
}

// resolveByPath dispatches a path request to the right resolver based
// on which subtree the path lives in. Nesting rules are subtree-
// specific: project/ and attachments/ are flat link farms (no slashes
// after the prefix); artifacts/{private,public} accept nested paths
// because agents organize artifacts into subdirectories.
func resolveByPath(ctx context.Context, s *store.FSStore, slug, filePath string) (string, string, error) {
	clean := strings.TrimPrefix(filePath, "/")
	if !strings.HasPrefix(clean, "files/") && clean != "files" {
		return "", "", fmt.Errorf("path %q: must start with /files/", filePath)
	}
	rel := strings.TrimPrefix(clean, "files/")

	switch {
	case strings.HasPrefix(rel, "project/"):
		name := strings.TrimPrefix(rel, "project/")
		if strings.Contains(name, "/") {
			return "", "", fmt.Errorf("path %q: nested paths under project/ are not supported", filePath)
		}
		pf, err := s.ResolveProjectFileByLinkName(name)
		if err != nil {
			return "", "", fmt.Errorf("project file %q not found", name)
		}
		return pf.SHA, pf.OriginalName, nil

	case strings.HasPrefix(rel, "attachments/"):
		name := strings.TrimPrefix(rel, "attachments/")
		if strings.Contains(name, "/") {
			return "", "", fmt.Errorf("path %q: nested paths under attachments/ are not supported", filePath)
		}
		att, err := s.ResolveAgentAttachmentByLinkName(slug, name)
		if err != nil {
			return "", "", fmt.Errorf("attachment %q not in your chat history", name)
		}
		return att.SHA, att.Name, nil

	case strings.HasPrefix(rel, "artifacts/private/"):
		return ingestAgentFile(ctx, s, slug, rel)

	case strings.HasPrefix(rel, files.PublishedOwnDir+"/"):
		return ingestPublishedFile(ctx, s, slug, strings.TrimPrefix(rel, files.PublishedOwnDir+"/"))
	}

	return "", "", fmt.Errorf("path %q: only /files/project/, /files/attachments/, /files/artifacts/private/ and /files/artifacts/public/ are shareable", filePath)
}

// ingestAgentFile reads a file from the agent's writable workspace
// (artifacts/private), content-addresses it via
// AddAttachment, and returns the resulting SHA + base filename.
// Idempotent at the bytes level: re-sharing identical bytes returns
// the same SHA.
//
// Path safety: this runs on core, which mounts every agent's tree and
// the org's credentials, and the agent can plant a symlink anywhere
// under its own /files/ with run_shell. So Resolve's lexical check is
// only the first gate; the open itself is files.OpenNoFollow, which is
// confined to the agent's storage root and refuses a symbolic link at
// any component. Only a regular file the agent itself wrote can be
// shared from here — a link to a project file or attachment is shared
// by its /files/project/ or /files/attachments/ path instead.
func ingestAgentFile(ctx context.Context, s *store.FSStore, slug, rel string) (string, string, error) {
	agentRoot := filepath.Join(s.Root(), "agents", slug)
	storage := files.StorageRoot(agentRoot)
	b := &files.Backend{Root: storage}
	if _, err := b.Resolve(rel); err != nil {
		return "", "", fmt.Errorf("path /files/%s: %w", rel, err)
	}
	f, err := files.OpenNoFollow(storage, rel)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", "", fmt.Errorf("path /files/%s: not found", rel)
		case errors.Is(err, files.ErrSymlink):
			return "", "", fmt.Errorf("path /files/%s: %v; only regular files can be shared", rel, err)
		case errors.Is(err, files.ErrNotRegular):
			return "", "", fmt.Errorf("path /files/%s: not a regular file", rel)
		}
		return "", "", fmt.Errorf("open /files/%s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(rel)
	att, err := s.AddAttachment(ctx, name, f)
	if err != nil {
		return "", "", fmt.Errorf("ingest /files/%s: %w", rel, err)
	}
	return att.SHA, name, nil
}

// ingestPublishedFile shares a file from the agent's own published
// tree: /files/artifacts/public/<p> is public/<slug>/<p> on the data
// volume (the pod mounts it there). Core wrote every file in it, but it
// is read with the same no-follow open as the agent's workspace, from
// that one agent's tree.
func ingestPublishedFile(ctx context.Context, s *store.FSStore, slug, rel string) (string, string, error) {
	shown := "/files/" + files.PublishedOwnDir + "/" + rel
	rel, err := files.CleanPublishedPath(rel)
	if err != nil {
		return "", "", fmt.Errorf("path %s: %w", shown, err)
	}
	f, err := files.OpenNoFollow(files.PublishedRoot(s.Root()), slug+"/"+rel)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", "", fmt.Errorf("path %s: not found", shown)
		case errors.Is(err, files.ErrSymlink), errors.Is(err, files.ErrNotRegular), errors.Is(err, files.ErrNotDir):
			return "", "", fmt.Errorf("path %s: not a regular file", shown)
		}
		return "", "", fmt.Errorf("open %s: %w", shown, err)
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(rel)
	att, err := s.AddAttachment(ctx, name, f)
	if err != nil {
		return "", "", fmt.Errorf("ingest %s: %w", shown, err)
	}
	return att.SHA, name, nil
}

// ResolveShareFilePath on the runtime uses the runtime's own store.
func (r *Runtime) ResolveShareFilePath(ctx context.Context, slug, filePath string) (ResolvedShareFile, error) {
	return ResolveShareFilePath(ctx, r.Store, slug, filePath)
}
