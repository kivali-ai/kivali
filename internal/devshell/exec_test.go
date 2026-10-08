package devshell

import (
	"path/filepath"
	"testing"
	"time"
)

// TestResolveCwd checks the cwd-escape rules (relative-only, no `..`)
// match what LocalShell enforced. A subagent's writable view must
// not be escapable via cwd to expose another agent's tree.
func TestResolveCwd(t *testing.T) {
	root := "/files"
	cases := []struct {
		name    string
		cwd     string
		want    string
		wantErr bool
	}{
		{"empty", "", "/files", false},
		{"dot", ".", "/files", false},
		{"sub", "artifacts/private", filepath.Join(root, "artifacts/private"), false},
		{"absolute", "/etc/passwd", "", true},
		{"dotdot", "..", "", true},
		{"escape", "../etc", "", true},
		{"buried escape", "a/../../b", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCwd(root, tc.cwd)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ResolveCwd(%q) = %q, want error", tc.cwd, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ResolveCwd(%q) error: %v", tc.cwd, err)
			}
			if got != tc.want {
				t.Errorf("ResolveCwd(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestTimeoutFor pins the clamping behavior for the request-side
// timeout — defaults, max cap, negative protection.
func TestTimeoutFor(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{0, DefaultTimeout * time.Second},
		{-5, DefaultTimeout * time.Second},
		{30, 30 * time.Second},
		{MaxTimeout, MaxTimeout * time.Second},
		{MaxTimeout + 1, MaxTimeout * time.Second},
		{MaxTimeout * 10, MaxTimeout * time.Second},
	}
	for _, tc := range cases {
		got := TimeoutFor(tc.seconds)
		if got != tc.want {
			t.Errorf("TimeoutFor(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

// TestCappedBufferTruncation pins the 256 KiB ceiling + the
// truncation marker. The Session readers feed a cappedBuffer for
// each stream; without this guard a runaway command would send
// megabytes back to the model.
func TestCappedBufferTruncation(t *testing.T) {
	var b cappedBuffer
	b.cap = 1024
	for i := 0; i < 10; i++ {
		_, _ = b.Write(make([]byte, 200))
	}
	out := b.String()
	if len(out) <= 1024 {
		t.Errorf("expected overflow marker; got %d bytes", len(out))
	}
}
