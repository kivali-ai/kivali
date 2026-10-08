//go:build unix

package guestapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diskServer is a Server whose /proc/mounts, /sys/fs/ext4 and run
// directory are files the test writes.
func diskServer(t *testing.T, mountOpts string, errorsCount, fsckRC, fsckLog string) *Server {
	t.Helper()
	d := t.TempDir()
	s := &Server{
		DataDir:    "/var/lib/kivali",
		RunDir:     filepath.Join(d, "run"),
		MountsFile: filepath.Join(d, "mounts"),
		Ext4SysDir: filepath.Join(d, "ext4"),
	}
	write := func(p, b string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(s.MountsFile, "proc /proc proc rw 0 0\n/dev/vdb /var/lib/kivali ext4 "+mountOpts+" 0 0\n/dev/vdb /var/lib/kubelet ext4 rw 0 0\n")
	if errorsCount != "" {
		write(filepath.Join(s.Ext4SysDir, "vdb", "errors_count"), errorsCount+"\n")
	}
	if fsckRC != "" {
		write(filepath.Join(s.RunDir, "fsck"), fsckRC+"\n")
		write(filepath.Join(s.RunDir, "e2fsck.log"), fsckLog)
	}
	return s
}

func TestDataDiskHealthy(t *testing.T) {
	s := diskServer(t, "rw,noatime,errors=remount-ro", "0", "0", "kivali-data: clean, 11/4194304 files, 300000/16777216 blocks\n")
	if h := s.dataDiskHealth(); h != (DataDiskHealth{}) || h.Problem() != "" {
		t.Fatalf("healthy disk: %+v %q", h, h.Problem())
	}
}

// After an unclean stop e2fsck -p replays the journal; that alone is
// routine, not damage, whatever its exit code.
func TestJournalReplayIsNotARepair(t *testing.T) {
	log := "kivali-data: recovering journal\nkivali-data: 11/4194304 files (0.0% non-contiguous), 300000/16777216 blocks\n"
	if h := diskServer(t, "rw,noatime", "0", "1", log).dataDiskHealth(); h.FsckRepaired {
		t.Fatalf("a journal replay reported as a repair: %+v", h)
	}
}

func TestDataDiskDamageIsReported(t *testing.T) {
	log := "kivali-data: recovering journal\nkivali-data: Inode 1234 extent tree (at level 1) could be shorter.  IGNORED.\n" +
		"kivali-data: Free blocks count wrong (123, counted=124).\nFIXED.\nkivali-data: 11/4194304 files (0.0% non-contiguous), 300000/16777216 blocks\n"
	h := diskServer(t, "ro,noatime,errors=remount-ro", "3", "1", log).dataDiskHealth()
	if !h.ReadOnly || h.Errors != 3 || !h.FsckRepaired {
		t.Fatalf("damaged disk: %+v", h)
	}
	if !strings.Contains(h.FsckDetail, "Inode 1234") || strings.Contains(h.FsckDetail, "recovering journal") {
		t.Fatalf("fsck detail %q", h.FsckDetail)
	}
	p := h.Problem()
	for _, want := range []string{"read-only", "3 error(s)", "e2fsck repaired"} {
		if !strings.Contains(p, want) {
			t.Fatalf("problem %q lacks %q", p, want)
		}
	}
}
