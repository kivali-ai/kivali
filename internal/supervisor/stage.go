package supervisor

import (
	"context"
	"sync"
)

// Progress stages. Every log line of an operation carries the stage it
// belongs to (Event.Stage), so the shell can draw a step list without
// parsing the lines (docs/developers/supervisor.md, "Stages").
const (
	StageMakingRoom  = "making-room"  // up: creating the data disk, its first boot to READY
	StageStarting    = "starting"     // booting to READY, the guest agent, the forward
	StageSettingUp   = "setting-up"   // install, rollout, /readyz; a credential change
	StageDownloading = "downloading"  // upgrade: preflight, fetch, verify, import
	StageSnapshot    = "snapshot"     // upgrade: journal, quiesce, stop, snapshot
	StageInstalling  = "installing"   // upgrade: boot, new chart, HelmChart rewrite
	StageRollingBack = "rolling-back" // upgrade: the snapshot goes back
	StagePausing     = "pausing"      // down
	StageDeleting    = "deleting"     // destroy
)

type stageKey struct{}

// stageHolder is the current stage of one operation.
type stageHolder struct {
	mu sync.Mutex
	id string
}

func (h *stageHolder) get() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
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

// setStage moves the operation running under ctx to stage id. Without a
// holder (a call from the command line's own process, a test) it does
// nothing.
func setStage(ctx context.Context, id string) {
	if h := stageHolderOf(ctx); h != nil {
		h.mu.Lock()
		h.id = id
		h.mu.Unlock()
	}
}
