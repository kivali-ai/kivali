//go:build integration

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/backup"
	"github.com/kivali-ai/kivali/internal/supervisor"
)

const (
	// The suite installs Kivali the way kivali-supervisor does: the
	// chart in k3s's static-charts directory and the HelmChart the
	// supervisor renders in its manifests directory, on a k3s started
	// with the VM's flags (scripts/ci-k3s.sh, which the nightly
	// workflow runs on a Linux runner). It wipes what it installs on
	// every run, so it only runs where KIVALI_IT_DISPOSABLE_K3S=1 says
	// the k3s on this host is a disposable one.
	namespace   = supervisor.Namespace
	deployName  = "kivali"
	serviceName = "kivali"
	chartDir    = "charts/kivali"

	// testEmail is the owner the install names, so the run admits a
	// synthetic owner without a real Google account.
	testEmail = "integration-test@kivali.local"
)

var (
	imageTag       string
	egressImageTag string

	// k3sDir is the k3s data directory: K3S_DATA_DIR, else k3s's
	// default. The VM's is on its data disk; the layout under it is the
	// same.
	k3sDir = cmp.Or(os.Getenv("K3S_DATA_DIR"), "/var/lib/rancher/k3s")
	// kubeconfig is the one k3s writes (scripts/ci-k3s.sh makes it
	// readable).
	kubeconfig = cmp.Or(os.Getenv("KIVALI_IT_KUBECONFIG"), "/etc/rancher/k3s/k3s.yaml")

	// testSessionKey signs the session cookie that test requests carry
	// AND is the install's session key — the codec the pod constructs
	// from SESSION_KEY must match the one the test process uses to mint
	// cookies, or the middleware rejects every request. auth.NewCodec
	// requires ≥16 bytes.
	testSessionKey = []byte("integration-test-session-key-do-not-use-anywhere-real-padded123")

	// testCookie is the pre-signed session cookie shared by every test
	// HTTP request. Built once in setup() after the codec is verified.
	testCookie *http.Cookie
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setup() error {
	if os.Getenv("KIVALI_IT_DISPOSABLE_K3S") != "1" {
		return fmt.Errorf("the integration suite installs into the k3s on this host and wipes it on every run: " +
			"run it on a disposable k3s (scripts/ci-k3s.sh up) with KIVALI_IT_DISPOSABLE_K3S=1")
	}
	if err := kubectl("get", "nodes"); err != nil {
		return fmt.Errorf("no k3s answering through %s: %w", kubeconfig, err)
	}
	// CI passes pre-built tags via env so we don't rebuild what the
	// `images` workflow job already produced. Local runs leave them
	// unset and fall back to building here.
	imageTag = os.Getenv("KIVALI_TEST_IMAGE_TAG")
	if imageTag == "" {
		imageTag = fmt.Sprintf("kivali:test-%d", time.Now().Unix())
		if err := runCmd("docker", "build", "-t", imageTag, "."); err != nil {
			return fmt.Errorf("docker build image: %w", err)
		}
	}
	egressImageTag = os.Getenv("KIVALI_TEST_EGRESS_IMAGE_TAG")
	if egressImageTag == "" {
		egressImageTag = fmt.Sprintf("kivali-egress-proxy:test-%d", time.Now().Unix())
		if err := runCmd("docker", "build", "-f", "Dockerfile.egress-proxy",
			"-t", egressImageTag, "."); err != nil {
			return fmt.Errorf("docker build egress-proxy: %w", err)
		}
	}
	// make test-vm imports the images into the VM's k3s itself
	// (kivali-supervisor load-images); there is no docker in the VM.
	if os.Getenv("KIVALI_IT_IMAGES_LOADED") != "1" {
		if err := importImages(imageTag, egressImageTag); err != nil {
			return fmt.Errorf("import images: %w", err)
		}
	}
	if err := uninstall(); err != nil {
		return fmt.Errorf("remove the previous install: %w", err)
	}
	if err := install(); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	// k3s's Helm controller installs the chart in its own time, as it
	// does for the supervisor: wait for the Deployment to exist, then
	// for its rollout.
	if err := waitForDeployment(5 * time.Minute); err != nil {
		return err
	}
	if err := kubectl("-n", namespace, "rollout", "status", "deployment/"+deployName, "--timeout=300s"); err != nil {
		return fmt.Errorf("rollout: %w", err)
	}
	// Mint the cookie used by every test request.
	codec, err := auth.NewCodec(testSessionKey)
	if err != nil {
		return fmt.Errorf("build codec: %w", err)
	}
	tok, err := codec.EncodeSession(auth.NewSession(testEmail))
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	testCookie = &http.Cookie{Name: auth.CookieName, Value: tok}
	return nil
}

// importImages loads the images into k3s's containerd, as
// kivali-supervisor load-images does in the VM.
func importImages(refs ...string) error {
	tar := filepath.Join(os.TempDir(), fmt.Sprintf("kivali-it-images-%d.tar", time.Now().UnixNano()))
	defer func() { _ = os.Remove(tar) }()
	if err := runCmd("docker", append([]string{"save", "-o", tar}, refs...)...); err != nil {
		return err
	}
	return asRoot("k3s", "ctr", "images", "import", tar)
}

// uninstall removes the HelmChart the last run wrote and its namespace.
func uninstall() error {
	if err := asRoot("rm", "-f", k3sPath(supervisor.ManifestRel)); err != nil {
		return err
	}
	_ = kubectl("-n", "kube-system", "delete", "helmchart", "kivali", "--ignore-not-found", "--wait=true")
	return kubectl("delete", "namespace", namespace, "--ignore-not-found", "--wait=true")
}

// install does what kivali-supervisor's install does with the chart
// from this tree: place the packaged chart where k3s serves static
// charts, and write the HelmChart rendered from the supervisor's own
// install values into k3s's manifests directory, where the Helm
// controller installs it. The image references are set per run because
// the suite builds timestamped tags.
func install() error {
	dir, err := os.MkdirTemp("", "kivali-it-chart-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// KIVALI_IT_CHART is the chart packaged already (make test-vm does
	// it on the host; the VM has no helm), else it is packaged here.
	if pre := os.Getenv("KIVALI_IT_CHART"); pre != "" {
		if err := runCmd("cp", pre, dir); err != nil {
			return fmt.Errorf("the packaged chart: %w", err)
		}
	} else if err := runCmd(cmp.Or(os.Getenv("HELM"), "helm"), "package", chartDir, "-d", dir); err != nil {
		return fmt.Errorf("package the chart: %w", err)
	}
	pkgs, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil || len(pkgs) != 1 {
		return fmt.Errorf("package the chart: want one .tgz in %s, got %v", dir, pkgs)
	}
	chart := filepath.Base(pkgs[0])
	if err := asRoot("install", "-D", "-m", "0644", pkgs[0], k3sPath(supervisor.StaticChartsRel, chart)); err != nil {
		return fmt.Errorf("place the chart: %w", err)
	}
	busybox, err := bakedBusybox()
	if err != nil {
		return err
	}
	vals, err := supervisor.InstallValues(supervisor.InstallOptions{Owner: testEmail},
		supervisor.Images{Kivali: fullRef(imageTag), Egress: fullRef(egressImageTag), Busybox: busybox},
		string(testSessionKey))
	if err != nil {
		return err
	}
	manifest, err := supervisor.RenderManifest(chart, vals)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "kivali.yaml")
	if err := os.WriteFile(tmp, manifest, 0o600); err != nil {
		return err
	}
	return asRoot("install", "-D", "-m", "0600", tmp, k3sPath(supervisor.ManifestRel))
}

// fullRef is ref as containerd lists it: a bare name lives under
// docker.io/library/, as `docker save` and `ctr images import` put it.
func fullRef(ref string) string {
	if strings.Contains(ref, "/") {
		return ref
	}
	return "docker.io/library/" + ref
}

// bakedBusybox is k3s's busybox helper image among the ones k3s
// imported from its airgap tarball, the init image the supervisor's
// install names.
func bakedBusybox() (string, error) {
	out, err := rootCommand("k3s", "ctr", "images", "ls", "-q").Output()
	if err != nil {
		return "", fmt.Errorf("list k3s images: %w", err)
	}
	for _, ref := range strings.Fields(string(out)) {
		if strings.Contains(ref, "/mirrored-library-busybox:") {
			return ref, nil
		}
	}
	return "", errors.New("k3s's busybox helper image is not imported (scripts/ci-k3s.sh places k3s's airgap images)")
}

// waitForDeployment polls until the Helm controller has created the
// server's Deployment, or the bound passes (with the controller's job
// log, which says why).
func waitForDeployment(bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", namespace, "get", "deployment/"+deployName).Run()
		if err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	_ = kubectl("-n", "kube-system", "logs", "job/helm-install-kivali", "--tail=50")
	return fmt.Errorf("the Helm controller did not create deployment/%s in %s within %s", deployName, namespace, bound)
}

// k3sPath is where a path the supervisor names under the VM's data
// directory (ManifestRel, StaticChartsRel: "k3s/server/...", the VM's
// k3s data dir being <data>/k3s) lives on this host, under k3sDir.
func k3sPath(rel string, more ...string) string {
	return filepath.Join(append([]string{k3sDir, strings.TrimPrefix(rel, "k3s/")}, more...)...)
}

// rootCommand runs argv as root: through sudo on a CI runner, as is
// inside the VM (make test-vm), where the suite already runs as root.
func rootCommand(argv ...string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.Command(argv[0], argv[1:]...)
	}
	return exec.Command("sudo", argv...)
}

// asRoot runs argv as root with the suite's output.
func asRoot(argv ...string) error {
	cmd := rootCommand(argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// kubectl runs kubectl against the suite's k3s.
func kubectl(args ...string) error {
	return runCmd("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- tests ---

func TestHealthzDeployed(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)

	_, status := getWithRetry(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), 30*time.Second)
	if status != http.StatusOK {
		t.Errorf("GET /healthz: status = %d, want 200", status)
	}
}

func TestReadyzDeployed(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)

	_, status := getWithRetry(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port), 30*time.Second)
	if status != http.StatusOK {
		t.Errorf("GET /readyz: status = %d, want 200", status)
	}
}

func TestHomeDeployed(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)

	// The image embeds the web app, served at the root.
	body, status := getWithRetry(t, fmt.Sprintf("http://127.0.0.1:%d/", port), 30*time.Second)
	if status != http.StatusOK {
		t.Errorf("GET /: status = %d, want 200", status)
	}
	if !strings.Contains(body, `id="root"`) {
		t.Errorf("GET / is not the web app's index: %.200s", body)
	}
}

func TestOrgAPIDeployed(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)

	body, status := getWithRetry(t, fmt.Sprintf("http://127.0.0.1:%d/api/v1/org", port), 30*time.Second)
	if status != http.StatusOK {
		t.Errorf("GET /api/v1/org: status = %d, want 200", status)
	}
	var org struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &org); err != nil {
		t.Errorf("GET /api/v1/org is not JSON: %v: %.200s", err, body)
	}
}

func TestHandbookSaveDeployed(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// Warm up.
	getWithRetry(t, base+"/api/v1/org/handbook", 30*time.Second)

	put, _ := json.Marshal(map[string]string{"content": "# Integration test handbook\n\nrules."})
	req, err := http.NewRequest(http.MethodPut, base+"/api/v1/org/handbook", strings.NewReader(string(put)))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	// Writes must come from the app's own origin.
	req.Header.Set("Origin", base)
	req.AddCookie(testCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d: %s", resp.StatusCode, b)
	}
	// Fetch back; body should contain the saved text.
	body, _ := getWithRetry(t, base+"/api/v1/org/handbook", 5*time.Second)
	if !strings.Contains(body, "Integration test handbook") {
		t.Errorf("persisted body missing text")
	}
}

// The app's backup download works in the runtime image: a whole zip,
// its manifest last, from the deployed server.
func TestBackupDownloadInPod(t *testing.T) {
	port, stop := portForward(t)
	t.Cleanup(stop)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	getWithRetry(t, base+"/api/v1/org/handbook", 30*time.Second)
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/org/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.AddCookie(testCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("backup: status %d, %v", resp.StatusCode, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("backup is not a zip: %v", err)
	}
	if n := len(zr.File); n == 0 || zr.File[n-1].Name != backup.ManifestName {
		t.Errorf("the zip's last member is not the manifest (%d members)", n)
	}
}

// --- helpers ---

var portRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

func portForward(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", namespace,
		"port-forward", "service/"+serviceName, ":80")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("port-forward start: %v", err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	portCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := portRE.FindStringSubmatch(scanner.Text()); m != nil {
				p, _ := strconv.Atoi(m[1])
				portCh <- p
				return
			}
		}
		errCh <- fmt.Errorf("port-forward exited without announcing port")
	}()

	select {
	case p := <-portCh:
		return p, stop
	case err := <-errCh:
		stop()
		t.Fatalf("port-forward: %v", err)
	case <-time.After(15 * time.Second):
		stop()
		t.Fatalf("port-forward: timed out waiting for announcement")
	}
	return 0, stop
}

func getWithRetry(t *testing.T, url string, timeout time.Duration) (string, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build GET: %v", err)
		}
		if testCookie != nil {
			req.AddCookie(testCookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return string(body), resp.StatusCode
	}
	t.Fatalf("GET %s never succeeded: %v", url, lastErr)
	return "", 0
}

// silence "imported and not used" if this file is trimmed in the future.
var _ = json.Marshal
