package files

import (
	"reflect"
	"testing"
)

// The dev-shell names its agent with --agent, which a pod spec from
// before the flag does not pass. Without a slug the per-agent chats
// root is left out, not made into /data/agents/chats; the shared
// roots stay.
func TestPodReadRoots(t *testing.T) {
	shared := []string{"/data/project_files", "/data/skills", "/data/attachments"}
	if got := PodReadRoots(""); !reflect.DeepEqual(got, shared) {
		t.Errorf("PodReadRoots(\"\") = %v, want %v", got, shared)
	}
	want := append(append([]string{}, shared...), "/data/agents/alice/chats")
	if got := PodReadRoots("alice"); !reflect.DeepEqual(got, want) {
		t.Errorf("PodReadRoots(alice) = %v, want %v", got, want)
	}
}
