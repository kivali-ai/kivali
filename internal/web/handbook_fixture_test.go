package web

import "strings"

// shapedHandbook is a handbook with the four sections every write
// door requires (internal/handbook), for tests that save, propose,
// seed, back up or restore one. A fixture without them is refused at
// the door or replaced by the default after a restore.
const shapedHandbook = "# Handbook\n\n## About\n\nThe rules.\n\n## Values\n\nV.\n\n## Communicating\n\nC.\n\n## Working together\n\nW.\n"

// shapedHandbookWith is shapedHandbook with its About body replaced.
func shapedHandbookWith(about string) string {
	return strings.Replace(shapedHandbook, "The rules.", about, 1)
}
