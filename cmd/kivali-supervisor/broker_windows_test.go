//go:build windows

package main

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/broker"
)

// broker status and setup print the three facts, one per line, in a
// fixed order and wording; asking the broker is the spike's.
func TestSetupReport(t *testing.T) {
	for _, tc := range []struct {
		st   broker.Setup
		want string
	}{
		{broker.Setup{HyperV: true, DefaultSwitch: true, VsockService: true},
			"hyperv: yes\ndefault switch: yes\nvsock service: registered\n"},
		{broker.Setup{HyperV: true, DefaultSwitch: true},
			"hyperv: yes\ndefault switch: yes\nvsock service: not registered\n"},
		{broker.Setup{},
			"hyperv: no\ndefault switch: no\nvsock service: not registered\n"},
	} {
		if got := setupReport(tc.st); got != tc.want {
			t.Errorf("setupReport(%+v) = %q, want %q", tc.st, got, tc.want)
		}
	}
}
