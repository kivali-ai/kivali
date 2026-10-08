package web

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// A message and the turn it starts run no filesystem sync and no
// graph walk: the farms are kept by the edges that change them, the
// index by the maintainer. A project link removed behind core's back
// stays removed across a whole turn, where a per-turn sync would have
// put it back.
func TestMessageTurnRunsNoFilesystemSyncOrGraphWalk(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponse(deltaResponse("ok"))
	pf, err := srv.Store.AddProjectFile("plan.md", strings.NewReader("the plan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.SetCanonical(pf.SHA, "original.md", "text/markdown"); err != nil {
		t.Fatal(err)
	}
	srv.afterProjectFilesChanged("test upload")
	link := filepath.Join(srv.Store.Root(), "agents", "alice", "memory", "project", "plan.md")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("precondition: the upload's edge linked plan.md: %v", err)
	}
	srv.Store.Graph().Index() // load, as boot would
	walks := srv.Store.Graph().Walks()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	sendMessage(t, srv, fake, "hello")

	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the turn re-synced the project farm (link back: %v)", err)
	}
	if got := srv.Store.Graph().Walks(); got != walks {
		t.Errorf("the turn walked the published trees %d times", got-walks)
	}
}

// A row appended under streamMu (a flush, a fold landing, a restore)
// leaves its files to be linked once the lock is released. Whatever is
// still unlinked when a turn spawns is linked before the turn reads
// the row: the files resolve by the time the pod has the turn.
func TestSpawnLinksAttachmentsAppendedUnderTheStreamLock(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	att, err := srv.Store.AddAttachmentFromText("brief.txt", "the brief")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(srv.Store.Root(), "agents", "alice", "memory", "attachments", "brief.txt")
	if err := srv.Store.AppendChatMessageLinkLater("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "see attached",
		Attachments: []store.MessageAttachment{{SHA: att.SHA, Name: "brief.txt"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: the append left the link to its caller (%v)", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	linked := make(chan error, 1)
	fake.SetResponseFunc(func(agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		_, err := os.ReadFile(link)
		linked <- err
		return deltaResponse("ok")
	})

	// Spawn on the row itself: a message posted now would land through
	// AppendChatMessage, whose own link would cover this row too.
	if !srv.spawnChatLoopIfIdle("alice", "chat") {
		t.Fatal("no turn spawned for the received row")
	}
	fake.AwaitTurn(t, 5*time.Second)
	fake.AwaitFinished(t, 5*time.Second)

	if err := <-linked; err != nil {
		t.Errorf("the turn reached the pod before the row's files were linked: %v", err)
	}
}
