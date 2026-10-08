package guestapi

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"gopkg.in/yaml.v3"
)

// ImageRefs lists the image references in an image archive as `docker
// save` (legacy manifest.json RepoTags) or an OCI layout (index.json
// annotations) writes it. References are normalised to their full form
// (docker.io/library/kivali:dev), which is how containerd names them on
// import. The archive may be a plain, gzip or zstd tar; plain tars from
// an *os.File are skipped through with seeks, so only the small index
// files are read.
func ImageRefs(r io.Reader, name string) ([]string, error) {
	rd, closeFn, err := Decompress(r, name)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	tr := tar.NewReader(rd)
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		if ref == "" {
			return
		}
		ref = NormalizeRef(ref)
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		switch path.Clean(hdr.Name) {
		case "manifest.json":
			var m []struct {
				RepoTags []string `json:"RepoTags"`
			}
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("%s: manifest.json: %w", name, err)
			}
			for _, e := range m {
				for _, t := range e.RepoTags {
					add(t)
				}
			}
		case "index.json":
			var idx struct {
				Manifests []struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"manifests"`
			}
			if err := json.NewDecoder(tr).Decode(&idx); err != nil {
				return nil, fmt.Errorf("%s: index.json: %w", name, err)
			}
			for _, m := range idx.Manifests {
				add(m.Annotations["io.containerd.image.name"])
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// ImageRefsFile is ImageRefs on a file.
func ImageRefsFile(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ImageRefs(f, p)
}

// Decompress wraps r by the archive's extension: .gz/.tgz gzip,
// .zst zstd, anything else as is.
func Decompress(r io.Reader, name string) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(name, ".gz"), strings.HasSuffix(name, ".tgz"):
		z, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		return z, func() { _ = z.Close() }, nil
	case strings.HasSuffix(name, ".zst"):
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		return z, z.Close, nil
	}
	return r, func() {}, nil
}

// NormalizeRef expands a short reference the way containerd and the
// kubelet do: kivali:dev -> docker.io/library/kivali:dev,
// rancher/x:1 -> docker.io/rancher/x:1.
func NormalizeRef(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return "docker.io/library/" + ref
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return ref
	}
	return "docker.io/" + ref
}

// SplitRef splits a reference into repository and tag. A digest
// reference keeps its digest in the repository part and has no tag.
func SplitRef(ref string) (repo, tag string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// ShortRepo is the shortest form of a repository that still resolves to
// it: docker.io/library/kivali -> kivali.
func ShortRepo(repo string) string {
	return strings.TrimPrefix(repo, "docker.io/library/")
}

// RepoName is a repository's last path element (kivali for
// ghcr.io/kivali-ai/kivali).
func RepoName(repo string) string { return path.Base(repo) }

// ChartMeta is what the supervisor needs from a chart's Chart.yaml.
type ChartMeta struct {
	Name       string `yaml:"name" json:"name"`
	Version    string `yaml:"version" json:"version"`
	AppVersion string `yaml:"appVersion" json:"app_version"`
}

// ChartInfo reads Chart.yaml from a packaged chart (a gzip tar whose
// top-level directory is the chart).
func ChartInfo(r io.Reader) (ChartMeta, error) {
	z, err := gzip.NewReader(r)
	if err != nil {
		return ChartMeta{}, fmt.Errorf("chart: %w", err)
	}
	defer func() { _ = z.Close() }()
	tr := tar.NewReader(z)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return ChartMeta{}, errors.New("chart: no Chart.yaml in the archive")
		}
		if err != nil {
			return ChartMeta{}, fmt.Errorf("chart: %w", err)
		}
		dir, file := path.Split(path.Clean(hdr.Name))
		if file != "Chart.yaml" || strings.Count(strings.Trim(dir, "/"), "/") != 0 || dir == "" {
			continue
		}
		var m ChartMeta
		if err := yaml.NewDecoder(tr).Decode(&m); err != nil {
			return ChartMeta{}, fmt.Errorf("chart: Chart.yaml: %w", err)
		}
		if m.Name == "" || m.Version == "" {
			return ChartMeta{}, errors.New("chart: Chart.yaml has no name or version")
		}
		return m, nil
	}
}

// ChartInfoFile is ChartInfo on a file.
func ChartInfoFile(p string) (ChartMeta, error) {
	f, err := os.Open(p)
	if err != nil {
		return ChartMeta{}, err
	}
	defer func() { _ = f.Close() }()
	return ChartInfo(f)
}
