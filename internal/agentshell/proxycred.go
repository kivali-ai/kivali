package agentshell

import (
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// proxyCredentialMaxBytes bounds a read of the ProxyCredentialFile: a
// proxy URL with a slug and a secret is far shorter.
const proxyCredentialMaxBytes = 4 << 10

// shellCredential is the shell's egress proxy credential as the daemon
// read it for one command: the credentialed proxy URL and the scrubber
// that keeps it out of the command's output. The zero value is no
// credential.
type shellCredential struct {
	url      string
	scrubber *egresscred.Scrubber
}

// readShellCredential reads the ProxyCredentialFile at path. A missing,
// unreadable or malformed file is no credential: the command keeps the
// bare proxy URL, which reaches the team's list only.
func readShellCredential(path string) shellCredential {
	if path == "" {
		return shellCredential{}
	}
	f, err := os.Open(path)
	if err != nil {
		return shellCredential{}
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, proxyCredentialMaxBytes))
	if err != nil {
		return shellCredential{}
	}
	raw := strings.TrimSpace(string(b))
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User == nil || u.User.Username() == "" {
		return shellCredential{}
	}
	secret, ok := u.User.Password()
	if !ok || secret == "" {
		return shellCredential{}
	}
	return shellCredential{url: raw, scrubber: egresscred.NewScrubber(u.User.Username(), secret, raw)}
}

// scrub replaces the credential in each of the response's texts the
// command or its environment produced.
func (c shellCredential) scrub(res *ExecResponse) {
	if c.scrubber == nil {
		return
	}
	res.Stdout = c.scrubber.String(res.Stdout)
	res.Stderr = c.scrubber.String(res.Stderr)
	res.Err = c.scrubber.String(res.Err)
}
