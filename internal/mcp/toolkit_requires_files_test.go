package mcp

import (
	"strings"
	"testing"
)

// A toolkit without a file_* dispatcher is refused when it is built,
// at MCP server startup, rather than registering tools whose first
// call would panic inside the server. A dispatcher built around
// nothing, or a nil pointer in the interface, is no dispatcher.
func TestToolkitsRequireFiles(t *testing.T) {
	for label, files := range map[string]FilesDispatcher{
		"no Files":                       nil,
		"NewSidecarFilesDispatcher(nil)": NewSidecarFilesDispatcher(nil),
		"NewLocalFilesDispatcher(nil)":   NewLocalFilesDispatcher(nil),
		"a nil *recordingFiles":          (*recordingFiles)(nil),
	} {
		for name, build := range map[string]func(){
			"FullAgentToolkit": func() { FullAgentToolkit(FullAgentDeps{Client: newTestClient("alice"), Files: files}) },
			"SubagentToolkit":  func() { SubagentToolkit(SubagentDeps{Client: newTestClient("alice"), SubagentID: "s1", Files: files}) },
		} {
			func() {
				defer func() {
					r := recover()
					if msg, _ := r.(string); !strings.Contains(msg, "Files is nil") {
						t.Errorf("%s with %s: recovered %v, want a panic naming Files", name, label, r)
					}
				}()
				build()
			}()
		}
	}
}
