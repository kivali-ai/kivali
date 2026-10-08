package devshell

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadPackagesParsesOnePerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pkgs")
	if err := os.WriteFile(path, []byte("bash\n\n# comment\n  git  \njq"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPackages(path)
	if err != nil {
		t.Fatalf("ReadPackages: %v", err)
	}
	if want := []string{"bash", "git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadPackages = %v, want %v", got, want)
	}
}

// A missing file is an image with no list, not a failure.
func TestReadPackagesMissingFileIsNil(t *testing.T) {
	got, err := ReadPackages(filepath.Join(t.TempDir(), "absent"))
	if err != nil || got != nil {
		t.Errorf("ReadPackages(absent) = %v, %v; want nil, nil", got, err)
	}
}

func TestHandlerPackages(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir(), Packages: []string{"bash", "git"}}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/packages")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out PackagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := []string{"bash", "git"}; !reflect.DeepEqual(out.Packages, want) {
		t.Errorf("Packages = %v, want %v", out.Packages, want)
	}
}

// With no list the wire carries [], never null, so a client decoding
// into a slice sees an empty list either way.
func TestHandlerPackagesEmptyIsArray(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir()}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/packages")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["packages"]) != "[]" {
		t.Errorf("packages = %s, want []", raw["packages"])
	}
}

// The client reads the list through the real handler over a UDS.
func TestSidecarShellPackagesRoundTrip(t *testing.T) {
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: t.TempDir(), Packages: []string{"python3", "ripgrep"}})
	defer stop()

	c := &SidecarShell{SocketPath: sock}
	got, err := c.Packages(context.Background())
	if err != nil {
		t.Fatalf("Packages: %v", err)
	}
	if want := []string{"python3", "ripgrep"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Packages = %v, want %v", got, want)
	}
}
