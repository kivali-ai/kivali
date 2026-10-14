//go:build windows

package hyperv

import "strings"

// plainPath is p without the \\?\ prefix Windows puts on a canonical
// local path (Tauri's resource directory arrives that way), which the
// broker refuses (broker.localAbs) and host.DirKey would hash as a
// different directory. Only a local drive's prefix is removed; a UNC or
// device path is returned as it is.
func plainPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) && len(p) >= 7 && p[5] == ':' && p[6] == '\\' {
		return p[4:]
	}
	return p
}
