package web

import (
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// An upload's graph kind is recorded on the file and read by the graph:
// a decision is a decision, the explicit no-kind ("artifact") upload
// has none, and an unlisted kind is refused.
func TestFilesUploadRecordsTheGraphKind(t *testing.T) {
	srv := newTestServer(t)
	post := func(kind string) *http.Response {
		body, ct := orgMultipart(t, []multipartFile{{"file", kind + ".md", []byte("content for " + kind)}}, map[string]string{"kind": kind})
		return orgDo(t, srv, "", http.MethodPost, "/api/v1/org/files", ct, body).Result()
	}
	if rr := post("decision"); rr.StatusCode != http.StatusOK {
		t.Fatalf("decision upload = %d", rr.StatusCode)
	}
	if rr := post("artifact"); rr.StatusCode != http.StatusOK {
		t.Fatalf("artifact upload = %d", rr.StatusCode)
	}
	if rr := post("datasheet"); rr.StatusCode != http.StatusBadRequest {
		t.Fatalf("unlisted kind should be refused, got %d", rr.StatusCode)
	}
	kinds := map[string]string{}
	pfs, _ := srv.Store.ListProjectFiles()
	for _, pf := range pfs {
		kinds[pf.OriginalName] = pf.Kind
	}
	if kinds["decision.md"] != "decision" || kinds["artifact.md"] != store.ProjectFileKindArtifact {
		t.Errorf("kinds = %v", kinds)
	}
	ix := srv.Store.Graph().Index()
	if n, ok := ix.Get("ceo/decision"); !ok || string(n.Kind) != "decision" {
		t.Errorf("decision node = %+v", n)
	}
	if n, ok := ix.Get("ceo/artifact"); !ok || n.Kind != "" {
		t.Errorf("artifact node = %+v", n)
	}
}
