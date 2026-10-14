//go:build windows

package hyperv

import "testing"

func TestPlainPath(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\Program Files\Kivali\vm`: `C:\Program Files\Kivali\vm`,
		`C:\Program Files\Kivali\vm`:     `C:\Program Files\Kivali\vm`,
		`\\?\UNC\server\share\vm`:        `\\?\UNC\server\share\vm`,
		`\\?\Volume{1234}\x`:             `\\?\Volume{1234}\x`,
		`\\server\share`:                 `\\server\share`,
		`\\?\C:`:                         `\\?\C:`,
	} {
		if got := plainPath(in); got != want {
			t.Errorf("plainPath(%q) = %q, want %q", in, got, want)
		}
	}
}
