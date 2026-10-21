package agentshell

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

const (
	bareProxy         = "http://10.43.0.12:3128"
	shellSecretInTest = "c2hlbGwtY3JlZGVudGlhbC1mb3ItdGhlLWRhZW1vbi10ZXN0"
	shellProxy        = "http://alice:" + shellSecretInTest + "@10.43.0.12:3128"
)

// credentialDaemon is a daemon whose credential file is path, with the
// daemon's own proxy variables set to the bare URL, as in a pod.
func credentialDaemon(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	for _, k := range egresscred.ProxyEnvKeys {
		t.Setenv(k, bareProxy)
	}
	t.Setenv("NO_PROXY", "localhost")
	path := filepath.Join(t.TempDir(), ProxyCredentialFile)
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir(), ProxyCredentialPath: path}))
	t.Cleanup(srv.Close)
	return srv, path
}

func execOn(t *testing.T, srv *httptest.Server, command string) ExecResponse {
	t.Helper()
	body, _ := json.Marshal(ExecRequest{Command: command, TimeoutSeconds: 5})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// envCheck prints "<VAR> ok" for each proxy variable equal to want,
// without printing the value, which the daemon would scrub.
func envCheck(want string) string {
	return `for v in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do [ "${!v}" = '` + want + `' ] && echo "$v ok"; done; echo "NO_PROXY=$NO_PROXY"`
}

const allProxyVarsOK = "HTTP_PROXY ok\nHTTPS_PROXY ok\nhttp_proxy ok\nhttps_proxy ok\nNO_PROXY=localhost\n"

// A command's proxy variables carry the shell's credential once the
// runtime has written it, read fresh for each command, and the bare URL
// before. Without it the proxy could not tell this agent's shell from
// another's, and the agent's own hosts would never open. This fails if
// any of the four variables keeps the bare URL with the file present,
// if NO_PROXY is touched, or if a file written after the daemon started
// is not picked up.
func TestExecCarriesTheShellCredentialOnceWritten(t *testing.T) {
	srv, path := credentialDaemon(t)
	if got := execOn(t, srv, envCheck(bareProxy)).Stdout; got != allProxyVarsOK {
		t.Fatalf("before the file, stdout = %q", got)
	}
	if err := os.WriteFile(path, []byte(shellProxy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := execOn(t, srv, envCheck(shellProxy)).Stdout; got != allProxyVarsOK {
		t.Fatalf("with the file, stdout = %q", got)
	}
}

// What a command prints of the credential, on either stream, comes back
// as the placeholder: the output goes into the agent's chat, where it
// would otherwise sit in the clear. This fails if stdout or stderr
// carries the secret.
func TestExecOutputIsScrubbedOfTheShellCredential(t *testing.T) {
	srv, path := credentialDaemon(t)
	if err := os.WriteFile(path, []byte(shellProxy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := execOn(t, srv, `echo "$HTTPS_PROXY"; cat '`+path+`'; echo "`+shellSecretInTest+`" >&2`)
	if want := egresscred.Placeholder + "\n" + egresscred.Placeholder + "\n"; res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}
	if want := egresscred.Placeholder + "\n"; res.Stderr != want {
		t.Errorf("stderr = %q, want %q", res.Stderr, want)
	}
}

// A file that holds no usable credential (no password, not a URL, more
// than one line) leaves the bare URL in place rather than a broken
// proxy URL. This fails if any of them reaches a command's environment.
func TestExecIgnoresAMalformedShellCredential(t *testing.T) {
	srv, path := credentialDaemon(t)
	for _, body := range []string{
		"http://alice@10.43.0.12:3128\n",
		"not a url\n",
		"10.43.0.12:3128\n",
		shellProxy + "\n" + shellProxy + "\n",
		"",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := execOn(t, srv, envCheck(bareProxy)).Stdout; got != allProxyVarsOK {
			t.Errorf("file %q: stdout = %q", body, got)
		}
	}
}
