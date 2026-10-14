//go:build !unix

package agentshell

import "errors"

// closeOnExec is supported on Unix only, where the install child runs.
func closeOnExec(int) error {
	return errors.New("close-on-exec is supported on Unix only")
}
