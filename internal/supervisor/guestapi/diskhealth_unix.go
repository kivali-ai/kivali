//go:build unix

package guestapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func (s *Server) dataDiskHealth() DataDiskHealth {
	var h DataDiskHealth
	if dev, opts, ok := mountOf(s.MountsFile, s.DataDir); ok {
		for _, o := range strings.Split(opts, ",") {
			if o == "ro" {
				h.ReadOnly = true
			}
		}
		if n, err := strconv.Atoi(readTrim(filepath.Join(s.Ext4SysDir, filepath.Base(dev), "errors_count"))); err == nil {
			h.Errors = n
		}
	}
	if rc, err := strconv.Atoi(readTrim(filepath.Join(s.RunDir, "fsck"))); err == nil && rc != 0 {
		b, _ := os.ReadFile(filepath.Join(s.RunDir, "e2fsck.log"))
		h.FsckDetail = fsckRepairs(string(b))
		h.FsckRepaired = h.FsckDetail != "" || rc >= 4
	}
	return h
}

// mountOf finds the mount at dir in a /proc/mounts file: its device and
// options. The last such line wins (it is the one on top).
func mountOf(mounts, dir string) (dev, opts string, ok bool) {
	b, err := os.ReadFile(mounts)
	if err != nil {
		return "", "", false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[1] == dir {
			dev, opts, ok = f[0], f[3], true
		}
	}
	return dev, opts, ok
}

// fsckRoutine matches the lines e2fsck -p prints for a filesystem it
// only had to replay the journal of: the recovery, "clean", and the
// summary of files and blocks.
var fsckRoutine = regexp.MustCompile(`(?i)recovering journal|: clean,|^\S+: \d+/\d+ files \(.*\), \d+/\d+ blocks$`)

// fsckRepairs is what an e2fsck -p log says beyond the routine: the
// repairs, at most a few, joined; "" when there were none.
func fsckRepairs(log string) string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || fsckRoutine.MatchString(l) {
			continue
		}
		out = append(out, l)
		if len(out) == 5 {
			break
		}
	}
	return strings.Join(out, " / ")
}
