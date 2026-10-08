package guestapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
)

// The protocol and image helpers are the same on every OS the host
// runs; guestapi_test.go (unix) covers the guest-side server.

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	big := bytes.Repeat([]byte("x"), MaxFrame+10)
	if err := fw.Frame(FrameStdout, big); err != nil {
		t.Fatal(err)
	}
	if err := fw.JSON(FrameExit, Exit{Code: 3}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		typ, p, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if typ == FrameExit {
			var ex Exit
			if err := json.Unmarshal(p, &ex); err != nil || ex.Code != 3 {
				t.Fatalf("exit frame %s, %v", p, err)
			}
			break
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("got %d bytes, want %d", len(got), len(big))
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	hdr := []byte{FrameStdout, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func writeTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	_, _ = z.Write(b)
	_ = z.Close()
	return buf.Bytes()
}

func TestImageRefs(t *testing.T) {
	docker := writeTar(t, map[string]string{
		"manifest.json": `[{"RepoTags":["kivali:dev","rancher/mirrored-pause:3.10"]},{"RepoTags":["ghcr.io/kivali-ai/kivali-dev-shell:v1"]}]`,
		"blobs/x":       "layer",
	})
	refs, err := ImageRefs(bytes.NewReader(docker), "x.tar")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker.io/library/kivali:dev", "docker.io/rancher/mirrored-pause:3.10", "ghcr.io/kivali-ai/kivali-dev-shell:v1"}
	if strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Fatalf("refs %v, want %v", refs, want)
	}
	oci := gz(t, writeTar(t, map[string]string{
		"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"docker.io/library/kivali-egress-proxy:dev"}}]}`,
	}))
	refs, err = ImageRefs(bytes.NewReader(oci), "x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "docker.io/library/kivali-egress-proxy:dev" {
		t.Fatalf("oci refs %v", refs)
	}
}

func TestRefHelpers(t *testing.T) {
	cases := map[string][2]string{
		"docker.io/library/kivali:dev":   {"docker.io/library/kivali", "dev"},
		"localhost:5000/kivali:v1":       {"localhost:5000/kivali", "v1"},
		"localhost:5000/kivali":          {"localhost:5000/kivali", ""},
		"ghcr.io/x/kivali@sha256:abcdef": {"ghcr.io/x/kivali@sha256:abcdef", ""},
	}
	for ref, want := range cases {
		repo, tag := SplitRef(ref)
		if repo != want[0] || tag != want[1] {
			t.Errorf("SplitRef(%q) = %q, %q; want %q, %q", ref, repo, tag, want[0], want[1])
		}
	}
	if got := ShortRepo("docker.io/library/kivali"); got != "kivali" {
		t.Errorf("ShortRepo = %q", got)
	}
	if got := NormalizeRef("localhost:5000/x:1"); got != "localhost:5000/x:1" {
		t.Errorf("NormalizeRef = %q", got)
	}
}

func TestChartInfo(t *testing.T) {
	tgz := gz(t, writeTar(t, map[string]string{
		"kivali/templates/Chart.yaml": "name: wrong\nversion: 9.9.9\n",
		"kivali/Chart.yaml":           "apiVersion: v2\nname: kivali\nversion: 0.15.1\nappVersion: v0.15.1\n",
	}))
	m, err := ChartInfo(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "kivali" || m.Version != "0.15.1" || m.AppVersion != "v0.15.1" {
		t.Fatalf("meta %+v", m)
	}
}
