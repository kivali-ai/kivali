package web

import (
	"context"
	"fmt"
	"mime/multipart"
)

// addSetupFiles stores and canonicalizes setup's project file uploads,
// then summarizes each in the background, for POST
// /api/v1/setup/files. Setup uploads carry no kind
// choice; unset reads as reference in the graph, which is what business
// context is. The first failure stops the batch; files before it stay.
func (s *Server) addSetupFiles(ctx context.Context, fhs []*multipart.FileHeader) error {
	var summarizeSHAs []string
	for _, fh := range fhs {
		sha, err := s.processUpload(ctx, fh, "")
		if err != nil {
			return fmt.Errorf("upload failed: %s: %w", fh.Filename, err)
		}
		if sha != "" {
			summarizeSHAs = append(summarizeSHAs, sha)
		}
	}
	for _, sha := range summarizeSHAs {
		go s.summarizeProjectFile(sha)
	}
	return nil
}
