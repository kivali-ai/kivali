package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// The two tools through which an agent's files reach the rest of the
// org. Core is the only writer of the published trees
// (docs/developers/files-and-publishing.md): the agent names a file in its
// workspace, core copies it, indexes it and answers with what the
// knowledge graph made of it.
const (
	ArtifactPublishToolName   = "artifact_publish"
	ArtifactUnpublishToolName = "artifact_unpublish"
)

// ArtifactPublishTool is the artifact_publish definition, the same for
// an agent and its subagents (a subagent publishes into its caller's
// area; core knows which from the calling process).
func ArtifactPublishTool() provider.Tool {
	return provider.Tool{
		Name: ArtifactPublishToolName,
		Description: fmt.Sprintf(`Publish a file or directory from your workspace to the org. Core copies it into your published area, /files/artifacts/public/ (read-only to you and to run_shell; this tool and %s are the only way in or out), where every agent reads it at /files/artifacts/shared/<your-slug>/<path>, and indexes it in the knowledge graph. A subagent publishes into its caller's area.

source: a file or directory you wrote, e.g. /files/artifacts/private/specs/api.md or /files/background/report/. A directory publishes everything in it. Only regular files: a symbolic link anywhere refuses the whole call, naming it. Names starting with "." are skipped inside a directory and refused when named. Not from project/, skills/, attachments/, past-chats/, episodes/ or the published trees.
dest (optional): the path under /files/artifacts/public/. Default: the source's path under /files/artifacts/private/, else its base name. Publishing a file over a published file replaces it. Publishing a directory adds and overwrites files but removes none: unpublish a file you deleted, or unpublish the directory and publish it again.
Limits: %d MB per file and per call, %d files per call.

The reply lists each file published with its graph node id, version and status, and anything the index could not accept (rejected front matter, problems): fix the source and publish again.`,
			ArtifactUnpublishToolName, store.PublishMaxFileBytes>>20, store.PublishMaxFiles),
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "source": {"type": "string", "description": "/files/ path of the file or directory to publish"},
    "dest":   {"type": "string", "description": "optional path under /files/artifacts/public/"}
  },
  "required": ["source"]
}`),
	}
}

// ArtifactUnpublishTool is the artifact_unpublish definition.
func ArtifactUnpublishTool() provider.Tool {
	return provider.Tool{
		Name:        ArtifactUnpublishToolName,
		Description: `Remove a file or directory from your published area, /files/artifacts/public/ (a subagent: its caller's). Peers stop seeing it at once. The reply names the graph nodes that went with it; anything that depended on them is flagged.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "path under /files/artifacts/public/ (relative, or the full /files/artifacts/public/<path>)"}
  },
  "required": ["path"]
}`),
	}
}

// fileS is the plural suffix for n files.
func fileS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// publishReportMaxLines bounds the per-file lines of a reply: a
// directory of a thousand files is summarised, not listed.
const publishReportMaxLines = 50

// RenderPublishReport is the model-facing reply to artifact_publish.
func RenderPublishReport(rep store.PublishReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Published %d file%s to /files/artifacts/public/ (peers read them at /files/artifacts/shared/%s/):\n", len(rep.Files), fileS(len(rep.Files)), rep.Owner)
	for i, f := range rep.Files {
		if i == publishReportMaxLines {
			fmt.Fprintf(&b, "- … and %d more\n", len(rep.Files)-i)
			break
		}
		b.WriteString("- " + f.Path)
		if rep.IndexErr == nil {
			b.WriteString(" → " + publishedFileLine(f))
		}
		b.WriteByte('\n')
	}
	if len(rep.Skipped) > 0 {
		skipped := rep.Skipped
		if len(skipped) > publishReportMaxLines {
			skipped = append(skipped[:publishReportMaxLines:publishReportMaxLines], fmt.Sprintf("… and %d more", len(rep.Skipped)-publishReportMaxLines))
		}
		fmt.Fprintf(&b, "Skipped (names starting with \".\"): %s\n", strings.Join(skipped, ", "))
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(&b, "Index warning: %s\n", w)
	}
	if rep.IndexErr != nil {
		fmt.Fprintf(&b, "The graph index could not be updated (%v); graph_query shows these files once it is.\n", rep.IndexErr)
	}
	return strings.TrimRight(b.String(), "\n")
}

// publishedFileLine is what the index made of one published file.
func publishedFileLine(f store.PublishedFile) string {
	if f.NodeID == "" {
		if f.Dropped != "" {
			// The index's own wording: "not indexed, its id … is taken by …".
			return f.Dropped
		}
		return "not in the graph"
	}
	var b strings.Builder
	if f.Payload {
		b.WriteString("payload of ")
	}
	b.WriteString("node " + f.NodeID)
	if f.Version > 0 {
		fmt.Fprintf(&b, " v%d", f.Version)
	} else {
		b.WriteString(" (no version recorded yet)")
	}
	fmt.Fprintf(&b, ", %s", f.Status)
	if len(f.Flags) > 0 {
		fmt.Fprintf(&b, ", flagged: %s", strings.Join(f.Flags, "; "))
	}
	if f.Rejected != "" {
		fmt.Fprintf(&b, "; front matter rejected: %s", f.Rejected)
	}
	for _, p := range f.Problems {
		fmt.Fprintf(&b, "; problem: %s", p)
	}
	return b.String()
}

// RenderUnpublishReport is the model-facing reply to
// artifact_unpublish.
func RenderUnpublishReport(rep store.PublishReport) string {
	var b strings.Builder
	paths := make([]string, 0, len(rep.Files))
	for i, f := range rep.Files {
		if i == publishReportMaxLines {
			paths = append(paths, fmt.Sprintf("… and %d more", len(rep.Files)-i))
			break
		}
		paths = append(paths, f.Path)
	}
	fmt.Fprintf(&b, "Unpublished %d file%s from /files/artifacts/public/: %s.\n", len(rep.Files), fileS(len(rep.Files)), strings.Join(paths, ", "))
	switch {
	case rep.IndexErr != nil:
		fmt.Fprintf(&b, "The graph index could not be updated (%v); the nodes go once it is.\n", rep.IndexErr)
	case len(rep.Gone) == 0 && len(rep.Remaining) == 0:
		b.WriteString("No graph node was built from these files.\n")
	default:
		if len(rep.Gone) > 0 {
			fmt.Fprintf(&b, "Removed from the graph: %s.\n", strings.Join(rep.Gone, ", "))
		}
		if len(rep.Remaining) > 0 {
			fmt.Fprintf(&b, "Still in the graph (other files of theirs are still published): %s.\n", strings.Join(rep.Remaining, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
