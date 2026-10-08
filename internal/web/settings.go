package web

import (
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

// saveHandbook replaces the handbook. A blank one is refused:
// every agent's system prompt opens with it.
func (s *Server) saveHandbook(body string) error {
	if strings.TrimSpace(body) == "" {
		return refuse(http.StatusBadRequest, "the handbook is empty")
	}
	return s.Store.WriteHandbook(body)
}

// addProjectFiles canonicalizes and stores each upload under one graph
// kind, then does what every project-file change needs: refresh every
// agent's /files/project/ view, re-index the graph, and summarize the
// new files in the background. Empty kind means "unset", which the
// graph reads as reference; store.ProjectFileKindArtifact is the
// explicit no-kind choice. It stops at the first file that fails;
// the files before it are kept.
func (s *Server) addProjectFiles(ctx context.Context, uploads []*multipart.FileHeader, kind string) error {
	if len(uploads) == 0 {
		return refuse(http.StatusBadRequest, "no files")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "" && kind != store.ProjectFileKindArtifact && !graph.ValidKind(graph.Kind(kind)) {
		return refuse(http.StatusBadRequest, fmt.Sprintf("kind %q is not one of the graph's kinds", kind))
	}
	var summarize []string
	var failed error
	for _, fh := range uploads {
		sha, err := s.processUpload(ctx, fh, kind)
		if err != nil {
			failed = fmt.Errorf("upload failed: %s: %w", fh.Filename, err)
			break
		}
		if sha != "" {
			summarize = append(summarize, sha)
		}
	}
	s.afterProjectFilesChanged("project file upload")
	// One Haiku summarization per newly canonicalized file, in the
	// background: the file shows at once and its summary follows.
	for _, sha := range summarize {
		go s.summarizeProjectFile(sha)
	}
	return failed
}

// removeProjectFiles deletes each named project file and returns the
// ones that failed, as "sha: error" lines. A SHA that is not 64 hex
// digits is refused before anything is touched, since it names a
// directory.
func (s *Server) removeProjectFiles(shas []string, reason string) ([]string, error) {
	for _, sha := range shas {
		if sha != "" && !validFileSHA(sha) {
			return nil, refuse(http.StatusBadRequest, fmt.Sprintf("%q is not a file id", sha))
		}
	}
	var failed []string
	for _, sha := range shas {
		if sha == "" {
			continue
		}
		if err := s.Store.RemoveProjectFile(sha); err != nil {
			failed = append(failed, sha+": "+err.Error())
		}
	}
	s.afterProjectFilesChanged(reason)
	return failed, nil
}

// afterProjectFilesChanged syncs every active agent's filesystem (the
// project files are symlinks there) and tells the graph maintainer,
// where project files are the CEO's nodes. A sync failure is logged,
// not returned: agents pick the change up on their next sync edge.
func (s *Server) afterProjectFilesChanged(reason string) {
	if err := s.Store.SyncAllFilesystems(); err != nil {
		fmt.Printf("files sync (%s): %v\n", reason, err)
	}
	s.Store.Graph().ProjectFilesChanged()
}

// validFileSHA reports whether sha is a project file id: a lowercase
// hex sha256.
func validFileSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
