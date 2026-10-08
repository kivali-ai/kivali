// Package backup writes, reads and checks Kivali backups: the zip the
// app downloads from the Org page and restores. It carries a
// manifest, the last entry, listing every file with its size and
// SHA-256, and a restore verifies every file it writes against it. A
// backup that lost or damaged a file therefore fails its restore,
// rather than restoring into an org that is quietly missing something.
package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ManifestName is the manifest's path inside an archive. It is never
// written into a data directory, and a backup never carries a file of
// that name from one.
const ManifestName = ".kivali-backup-manifest.jsonl"

// ManifestFormat names the manifest's shape, on its first line.
const ManifestFormat = "kivali-backup-manifest/1"

// Manifest is what an archive says it holds.
type Manifest struct {
	Format  string    `json:"format"`
	Created time.Time `json:"created"`
	// Version is the binary that wrote the archive, or a tool's name.
	Version string `json:"version,omitempty"`
	Files   []File `json:"-"`
}

// File is one regular file in an archive. Path is slash-separated and
// relative to the data directory.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Marshal renders the manifest as JSON lines: the header, then one line
// per file, sorted by path so two manifests of the same data compare
// line for line.
func (m *Manifest) Marshal() []byte {
	var buf bytes.Buffer
	head, _ := json.Marshal(m)
	buf.Write(head)
	buf.WriteByte('\n')
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		line, _ := json.Marshal(f)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// ParseManifest reads what Marshal wrote.
func ParseManifest(b []byte) (*Manifest, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		return nil, fmt.Errorf("manifest: empty")
	}
	var m Manifest
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("manifest: header: %w", err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("manifest: format %q, want %q", m.Format, ManifestFormat)
	}
	seen := map[string]bool{}
	for n := 2; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var f File
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("manifest: line %d: %w", n, err)
		}
		if f.Path == "" || len(f.SHA256) != 64 || f.Size < 0 {
			return nil, fmt.Errorf("manifest: line %d: incomplete entry", n)
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("manifest: %s listed twice", f.Path)
		}
		seen[f.Path] = true
		m.Files = append(m.Files, f)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// Diff compares what an archive says it holds with what was found, and
// describes every difference, most useful first. Empty means equal.
func Diff(want, got []File) []string {
	w := map[string]File{}
	for _, f := range want {
		w[f.Path] = f
	}
	g := map[string]File{}
	for _, f := range got {
		g[f.Path] = f
	}
	var out []string
	for p, wf := range w {
		gf, ok := g[p]
		switch {
		case !ok:
			out = append(out, "missing: "+p)
		case gf.Size != wf.Size:
			out = append(out, fmt.Sprintf("size differs: %s (%d, want %d)", p, gf.Size, wf.Size))
		case gf.SHA256 != wf.SHA256:
			out = append(out, "content differs: "+p)
		}
	}
	for p := range g {
		if _, ok := w[p]; !ok {
			out = append(out, "not in the manifest: "+p)
		}
	}
	sort.Strings(out)
	return out
}

// summarize joins the first few problems into one line for an error.
func summarize(problems []string) string {
	const show = 5
	if len(problems) <= show {
		return strings.Join(problems, "; ")
	}
	return strings.Join(problems[:show], "; ") + fmt.Sprintf("; and %d more", len(problems)-show)
}
