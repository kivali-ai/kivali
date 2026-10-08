package roleicon

import (
	"strings"
	"testing"
)

func TestNamesAreUniqueAndDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, i := range Icons {
		if seen[i.Name] {
			t.Errorf("%q is listed twice", i.Name)
		}
		seen[i.Name] = true
		if strings.TrimSpace(i.Description) == "" {
			t.Errorf("%q has no description", i.Name)
		}
	}
}

func TestCheck(t *testing.T) {
	if err := Check("compass"); err != nil {
		t.Errorf("compass: %v", err)
	}
	for _, bad := range []string{"", "robot", "Compass"} {
		err := Check(bad)
		if err == nil || !strings.Contains(err.Error(), "compass, briefcase") {
			t.Errorf("Check(%q) = %v, want a refusal listing the icons", bad, err)
		}
	}
}

func TestMenuHasOneLinePerIcon(t *testing.T) {
	m := Menu()
	if got := strings.Count(m, "\n"); got != len(Icons) {
		t.Errorf("menu has %d lines, want %d", got, len(Icons))
	}
	if !strings.Contains(m, "- compass: a compass:") {
		t.Errorf("menu lacks the compass line:\n%s", m)
	}
}
