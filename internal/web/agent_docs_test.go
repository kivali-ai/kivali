package web

import (
	"strings"
	"testing"
	"time"
)

// archiveTS builds a rotation stamp in the format ArchiveChat uses, so
// the page under test parses it back into a real date.
func archiveTS(day int) string {
	return time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC).Format(archiveTSLayout)
}

// TestTrimExcerpt: rows are one line tall, so a multi-paragraph opening
// message has to flatten and truncate rather than push the row apart.
func TestTrimExcerpt(t *testing.T) {
	if got := trimExcerpt("  two   lines\nof   prose\n"); got != "two lines of prose" {
		t.Errorf("trimExcerpt = %q", got)
	}
	long := strings.Repeat("a", excerptMaxRunes+50)
	got := trimExcerpt(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long excerpt not truncated: %q", got)
	}
	if n := len([]rune(got)); n != excerptMaxRunes+1 {
		t.Errorf("truncated excerpt = %d runes, want %d + ellipsis", n, excerptMaxRunes)
	}
}

// TestParseArchiveTS: the archive stamp is a directory name that the UI
// renders as a date. A name that doesn't parse still has to list.
func TestParseArchiveTS(t *testing.T) {
	ts := archiveTS(7)
	got, ok := parseArchiveTS(ts)
	if !ok {
		t.Fatalf("parseArchiveTS(%q) failed", ts)
	}
	if want := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("parsed = %v, want %v", got, want)
	}
	if _, ok := parseArchiveTS("not-a-stamp"); ok {
		t.Error("parseArchiveTS accepted a non-stamp")
	}
}
