// Package debpkg holds Debian Policy §5.6.1's package name rule, the
// one check every name passes before it reaches apt-get: the software
// lists an owner saves (store.ValidSoftwareName), the agent shell's
// install (agentshell.ValidPackageName) and the packages an
// integration's manifest declares (integration.Validate). It imports
// nothing of Kivali's, so each of them can use it.
package debpkg

import "regexp"

// nameRE is a Debian package name: lowercase letters, digits, '+', '-'
// and '.', at least two characters, starting with a letter or digit
// and not ending in '-'. Nothing else reaches apt-get as an argument:
// no option (a leading '-'), no removal (apt-get install reads a
// trailing '-' as "remove this package"), no version pin ('='), no
// release ('/'), no path.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*[a-z0-9+.]$`)

// MaxNameLen bounds a name.
const MaxNameLen = 128

// ValidName reports whether name is a Debian package name.
func ValidName(name string) bool {
	return len(name) <= MaxNameLen && nameRE.MatchString(name)
}
