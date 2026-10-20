package supervisor

import (
	"context"
	"sync"
)

// Progress stages. Every log line of an operation carries the stage it
// belongs to (Event.Stage), so the shell can draw a step list without
// parsing the lines (docs/developers/supervisor.md, "Stages").
const (
	StageUpdatingData = "updating-data"   // up: the data disk moved to where the backend now keeps it
	StageMakingRoom   = "making-room"     // up: creating the data disk, its first boot to READY; compacting the data disk before a boot
	StageStarting     = "starting"        // booting to READY, the guest agent, the forward
	StageSettingUp    = "setting-up"      // install, rollout, /readyz; a credential change
	StageDownloading  = "downloading"     // upgrade: preflight, fetch, verify, import
	StageSnapshot     = "snapshot"        // upgrade: journal, quiesce, stop, snapshot
	StageInstalling   = "installing"      // upgrade: boot, new chart, HelmChart rewrite
	StageAgents       = "starting-agents" // upgrade: agent pods up and connected (waitAgents); progress: connected of agent pods
	StageRollingBack  = "rolling-back"    // upgrade: the snapshot goes back
	StagePausing      = "pausing"         // down
	StageDeleting     = "deleting"        // destroy
)

type stageKey struct{}

// stageHolder is the current stage of one operation, and how far the
// measurable work of that stage has got (Event.Progress), if it has
// any.
type stageHolder struct {
	mu       sync.Mutex
	id       string
	progress *EventProgress
	// emit sends an event with no log line, for a change of progress;
	// the operation's stream sets it.
	emit func(Event)
}

func (h *stageHolder) get() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

// now is the stage and its progress (nil without any), for an event.
func (h *stageHolder) now() (string, *EventProgress) {
	if h == nil {
		return "", nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.progress == nil {
		return h.id, nil
	}
	p := *h.progress
	return h.id, &p
}

func (h *stageHolder) setEmit(emit func(Event)) {
	h.mu.Lock()
	h.emit = emit
	h.mu.Unlock()
}

// withStage gives an operation's context a stage holder: its log lines
// carry whatever setStage last put there.
func withStage(ctx context.Context) (context.Context, *stageHolder) {
	h := &stageHolder{}
	return context.WithValue(ctx, stageKey{}, h), h
}

func stageHolderOf(ctx context.Context) *stageHolder {
	h, _ := ctx.Value(stageKey{}).(*stageHolder)
	return h
}

// setStage moves the operation running under ctx to stage id, which
// has no progress until reportProgress gives it some. Without a holder
// (a call from the command line's own process, a test) it does nothing.
func setStage(ctx context.Context, id string) {
	if h := stageHolderOf(ctx); h != nil {
		h.mu.Lock()
		if h.id != id {
			h.progress = nil
		}
		h.id = id
		h.mu.Unlock()
	}
}

// progressOf is the Progress of the stage the operation under ctx is
// in: each report becomes the stage's progress, which every later line
// of the stage carries, and is sent at once as an event of its own.
// Without a holder it does nothing.
func progressOf(ctx context.Context) Progress {
	h := stageHolderOf(ctx)
	return func(done, total int64) {
		if h == nil || total <= 0 {
			return
		}
		done = min(max(done, 0), total)
		h.mu.Lock()
		h.progress = &EventProgress{Done: done, Total: total}
		ev := Event{Stage: h.id, Progress: &EventProgress{Done: done, Total: total}}
		emit := h.emit
		h.mu.Unlock()
		if emit != nil {
			emit(ev)
		}
	}
}
