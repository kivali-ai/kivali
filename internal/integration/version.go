package integration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed integration version: semantic versioning's
// MAJOR.MINOR.PATCH with an optional pre-release. A version names a
// directory in the store, so build metadata ("+...") is not accepted.
type Version struct {
	Major, Minor, Patch int
	// Pre is the pre-release ("beta.1"), empty for a release.
	Pre string
}

// String is the version as written.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// maxVersionLen bounds a version string, which names a directory.
const maxVersionLen = 64

var versionRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?$`)

// ParseVersion reads a version: MAJOR.MINOR.PATCH, no leading zeros,
// with an optional -pre-release of dot-separated identifiers.
func ParseVersion(s string) (Version, error) {
	if len(s) > maxVersionLen {
		return Version{}, fmt.Errorf("version %q is longer than %d characters", s, maxVersionLen)
	}
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH, such as 1.2.0 or 1.3.0-beta.1", s)
	}
	var v Version
	var err error
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if *dst, err = strconv.Atoi(m[i+1]); err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
	}
	v.Pre = m[4]
	return v, nil
}

// ValidVersion reports whether s parses as a version.
func ValidVersion(s string) bool {
	_, err := ParseVersion(s)
	return err == nil
}

// Compare is -1, 0 or +1 as v is older than, the same as or newer
// than w, in semantic versioning's precedence: a pre-release ranks
// below its release, and pre-release identifiers compare one by one,
// numbers numerically and below words.
func (v Version) Compare(w Version) int {
	for _, d := range [][2]int{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Pre == w.Pre:
		return 0
	case v.Pre == "":
		return 1
	case w.Pre == "":
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(w.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := comparePreIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// comparePreIdent compares two pre-release identifiers: numbers (all
// digits, which the version pattern holds to no leading zero) by
// length and then digit by digit, so no size overflows, and below
// every identifier with a letter or hyphen, which compare as text.
func comparePreIdent(a, b string) int {
	numA, numB := allDigits(a), allDigits(b)
	switch {
	case numA && numB:
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case numA:
		return -1
	case numB:
		return 1
	}
	return strings.Compare(a, b)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// CompareVersions is -1, 0 or +1 as a is older than, the same as or
// newer than b. A string that does not parse ranks below every one
// that does, and two that do not parse compare as text.
func CompareVersions(a, b string) int {
	va, errA := ParseVersion(a)
	vb, errB := ParseVersion(b)
	switch {
	case errA != nil && errB != nil:
		return strings.Compare(a, b)
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	return va.Compare(vb)
}
