package web

import (
	"context"
	"io"
	"log"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Caps for the summarizer. 50k input chars is ~12k tokens, plenty to
// describe most files; beyond that we truncate the head and let
// Haiku judge from the opening. Summary tokens stay tiny — the
// prompt asks for one sentence.
const (
	summaryInputCharCap = 50_000
	summaryMaxTokens    = 120
	// Anthropic accepts images up to 5 MiB inline. We cap lower to
	// keep Haiku latency snappy — most useful images (diagrams,
	// screenshots, scanned pages) are well under 2 MiB.
	summaryImageByteCap = 4 << 20
)

// summarizeProjectFile asks Haiku for a one-sentence description of
// what the uploaded file is about. Shape depends on file type:
//
//   - Text-extractable (text/markdown/code, PDFs, office docs) — we
//     read the canonical text (produced at upload time by
//     internal/convert) and pass it as a text content block.
//   - Images (PNG/JPEG/GIF/WebP) — we pass the ORIGINAL bytes as an
//     image content block. Claude's vision models describe what's
//     in the image. No canonical text exists or is needed.
//   - Other binaries (no canonical, not an image) — no summary.
//
// Runs asynchronously (the upload response doesn't wait). On error
// we log and leave Summary empty — missing summaries show up as
// "(no summary yet)" in list_project_files; the catalog still works.
//
// Why Haiku 4.5: the call is on the hot path for every upload. The
// job ("one-sentence description") is well within Haiku's abilities,
// and Haiku supports vision so images go through the same tool.
func (s *Server) summarizeProjectFile(sha string) {
	if s.Claude == nil {
		return
	}
	pf, err := s.Store.GetProjectFile(sha)
	if err != nil {
		log.Printf("summarize %s: metadata: %v", sha, err)
		return
	}

	content, ok := s.buildSummaryInputContent(pf)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := provider.CompleteRequest{
		Model:     s.SummaryModel,
		MaxTokens: summaryMaxTokens,
		Purpose:   "project_file_summary",
		Agent:     "", // not agent-scoped
		System: []provider.SystemBlock{
			{Text: summaryPromptSystem},
		},
		Messages: []provider.Message{
			{
				Role:    provider.RoleUser,
				Content: content,
			},
		},
	}
	resp, err := s.Claude.Complete(ctx, req)
	if err != nil {
		log.Printf("summarize %s: Claude: %v", sha, err)
		return
	}
	summary := flattenTextContent(resp.Content)
	summary = strings.TrimSpace(summary)
	// Guardrails against the model ignoring the "one sentence" ask —
	// take the first sentence, cap total length at 300 chars.
	if i := strings.Index(summary, "\n"); i > 0 {
		summary = summary[:i]
	}
	if len(summary) > 300 {
		summary = strings.TrimSpace(summary[:300]) + "…"
	}
	if summary == "" {
		return
	}
	if err := s.Store.SetProjectFileSummary(sha, summary); err != nil {
		log.Printf("summarize %s: persist: %v", sha, err)
		return
	}
	// The summary is the CEO node's summary in the graph.
	s.Store.Graph().ProjectFilesChanged()
	log.Printf("summarize %s (%s): %s", sha, pf.OriginalName, summary)
}

// buildSummaryInputContent produces the user-message content blocks
// for summarizing pf. Text branch reads the canonical plain-text
// extraction; image branch reads the original bytes. Returns
// (nil, false) when there's nothing to summarize (no canonical, not
// an image).
func (s *Server) buildSummaryInputContent(pf store.ProjectFile) ([]provider.ContentBlock, bool) {
	if isSummarizableImage(pf.MIME) {
		rc, err := s.Store.OpenOriginal(pf.SHA)
		if err != nil {
			log.Printf("summarize %s: open original image: %v", pf.SHA, err)
			return nil, false
		}
		defer func() { _ = rc.Close() }()
		body, err := io.ReadAll(io.LimitReader(rc, summaryImageByteCap))
		if err != nil {
			log.Printf("summarize %s: read original image: %v", pf.SHA, err)
			return nil, false
		}
		return []provider.ContentBlock{
			{Type: provider.ContentText, Text: "FILENAME: " + pf.OriginalName + "\n\nDescribe what this image is, following the one-sentence rule in your system prompt."},
			{Type: provider.ContentImage, ImageMediaType: pf.MIME, ImageData: body},
		}, true
	}

	// Text-extractable path (canonical is populated for text,
	// PDFs with successful pdftotext, office docs via
	// libreoffice+pdftotext).
	rc, err := s.Store.OpenCanonical(pf.SHA)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, summaryInputCharCap*2))
	if err != nil {
		log.Printf("summarize %s: read canonical: %v", pf.SHA, err)
		return nil, false
	}
	text := string(body)
	if len(text) > summaryInputCharCap {
		text = text[:summaryInputCharCap]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false
	}
	return []provider.ContentBlock{
		{
			Type: provider.ContentText,
			Text: "FILENAME: " + pf.OriginalName + "\n\nCONTENT:\n\n" + text,
		},
	}, true
}

// isSummarizableImage reports whether mime is one of the image types
// Anthropic's vision models accept as inline base64 content.
func isSummarizableImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func flattenTextContent(blocks []provider.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == provider.ContentText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

const summaryPromptSystem = `You are summarizing project files for an agent catalog. For each file, respond with EXACTLY ONE sentence (≤ 30 words) that tells a reader what the file is about and why someone might want to open it.

Rules:
  - ONE sentence. No preamble, no quotes, no "this file...", no "this document contains...".
  - Lead with the SUBJECT (what it's about), not the document type.
  - Be specific about content: mention key topics, entities, the domain. Avoid generic framing like "a comprehensive overview".
  - Stay under 30 words.

Good: "Q3 go-to-market plan for a consumer software product, covering pricing tiers, customer-segment targeting, and channel strategy."
Good: "Load test of the email reminder service; measures send throughput and bounce rate under peak traffic."
Bad: "This document is a comprehensive overview of the Q3 plan." (preamble + generic)
Bad: "A plan." (too short, tells a reader nothing)`
