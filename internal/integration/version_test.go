package integration

import "testing"

// A version names a directory and orders the version list; a version
// that parses wrongly would either be refused at upload or sort out of
// place on the integration's page.
func TestParseVersion(t *testing.T) {
	for _, ok := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x-y-z.--"} {
		if _, err := ParseVersion(ok); err != nil {
			t.Errorf("ParseVersion(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "v1.2.3", "01.2.3", "1.02.3", "1.2.3-", "1.2.3-01", "1.2.3+build", "1.2.3-a..b", "1.2.3/..", "1.2.3-" + string(make([]byte, 64))} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	v, _ := ParseVersion("2.1.0-rc.1")
	if v.Major != 2 || v.Minor != 1 || v.Patch != 0 || v.Pre != "rc.1" || v.String() != "2.1.0-rc.1" {
		t.Errorf("ParseVersion = %+v", v)
	}
}

// Semantic versioning's precedence, the example list from its spec in
// order, plus numbers past what an int holds: the newest version heads
// the page, and a pending version is offered as newer only when it is.
func TestCompareVersions(t *testing.T) {
	order := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0",
	}
	for i := range order {
		for j := range order {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := CompareVersions(order[i], order[j]); got != want {
				t.Errorf("CompareVersions(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	if CompareVersions("1.0.0-99999999999999999999", "1.0.0-100000000000000000000") != -1 {
		t.Error("long numeric identifiers compare wrongly")
	}
	if CompareVersions("junk", "0.0.1") != -1 || CompareVersions("0.0.1", "junk") != 1 {
		t.Error("an unparseable version does not rank lowest")
	}
}
