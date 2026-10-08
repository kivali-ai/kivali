//go:build windows

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// powershell runs the broker's embedded scripts. It is the real
// Runner, created at start and NOT exercised by the unit tests (the
// policy is tested with a fake Runner); the Hyper-V backend's own tests
// against a running machine exercise it.
type powershell struct {
	// exe is powershell.exe, resolved once.
	exe string
}

func newPowerShell() (*powershell, error) {
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		sysdir, derr := windows.GetSystemDirectory()
		if derr != nil {
			return nil, fmt.Errorf("find powershell.exe: %w", err)
		}
		exe = sysdir + `\WindowsPowerShell\v1.0\powershell.exe`
	}
	return &powershell{exe: exe}, nil
}

// Run runs the named script with args as the JSON object on its stdin.
// The script is passed as -EncodedCommand (base64 UTF-16LE of its fixed
// text wrapped in the prelude), so nothing a caller sent is ever on the
// command line; args reach the script only through stdin. The process
// has no window (CREATE_NO_WINDOW), no profile and no interaction.
func (p *powershell) Run(ctx context.Context, script string, args any) (json.RawMessage, error) {
	enc, err := EncodedCommand(script)
	if err != nil {
		return nil, err
	}
	in, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.exe, "-NoProfile", "-NonInteractive", "-NoLogo",
		"-ExecutionPolicy", "Bypass", "-OutputFormat", "Text", "-EncodedCommand", enc)
	cmd.Stdin = bytes.NewReader(in)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	runErr := cmd.Run()

	// The script's own output is one compressed JSON object on the last
	// non-empty line; the prelude prints {"error": "..."} and exits 1 on
	// failure.
	last := lastJSONLine(out.Bytes())
	if len(last) > 0 {
		var probe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(last, &probe) == nil && probe.Error != "" {
			return nil, fmt.Errorf("%s", probe.Error)
		}
		if runErr == nil {
			return json.RawMessage(last), nil
		}
	}
	msg := strings.TrimSpace(errBuf.String())
	if msg == "" {
		msg = strings.TrimSpace(out.String())
	}
	return nil, fmt.Errorf("powershell %s failed: %v: %s", script, runErr, msg)
}

// lastJSONLine is the last line that looks like a JSON object.
func lastJSONLine(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		t := bytes.TrimSpace(lines[i])
		if len(t) > 0 && t[0] == '{' {
			return t
		}
	}
	return nil
}
