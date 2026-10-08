package devshell

import (
	"errors"
	"os"
	"strings"
)

// ReadPackages reads the package list Dockerfile.dev-shell records at
// build time: one package per line. Blank lines and #-comments are
// skipped. A missing file is not an error, it is an image with no list
// (nil); the tool description then says the list is unknown rather than
// guessing. Any other read failure is returned.
func ReadPackages(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parsePackages(string(b)), nil
}

func parsePackages(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
