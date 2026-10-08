package agent

// PrimeRunningForTest sets the runtime's per-agent running set to
// exactly the given slug list. Used by tests in other packages (e.g.
// internal/web) that need to assert behavior conditional on
// Runtime.RunningAgents() — without spinning up a full release flow.
//
// Not exported through the regular API surface because production
// callers should never need to forge runtime state. The `ForTest`
// suffix and this comment are the discipline marker.
//
// MarkRunning fires the OnRunningChange callback per slug change; on
// an empty slug list with no transitions to make we still want a
// notify so subscribers can re-check, so we fire the callback once
// at the end if it's installed.
func PrimeRunningForTest(r *Runtime, slugs []string) {
	for _, s := range r.RunningAgents() {
		r.MarkRunning(s, false)
	}
	for _, s := range slugs {
		r.MarkRunning(s, true)
	}
	if len(slugs) == 0 && r.onRunningChange != nil {
		r.onRunningChange()
	}
}
