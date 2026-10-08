package agent

import "context"

// FixedSlugExec wraps a ShellExecutor and replaces the per-call slug
// with a fixed value. Used for subagent runs: the subagent shell tool
// receives "" as the caller slug (the subagent has no real slug of
// its own), but the underlying executor keys exec by the parent
// agent's slug. DefaultCwd is the per-subagent working directory
// inside the parent's pod (e.g. /scratch/subagent-<id>/) — applied
// when the model's run_shell call omits cwd.
type FixedSlugExec struct {
	Inner      ShellExecutor
	Slug       string
	DefaultCwd string
}

// Exec discards the supplied slug and calls Inner with the fixed Slug.
// When the caller didn't set cwd, DefaultCwd is supplied so the
// subagent's shell calls land in their per-subagent scratch dir.
func (f FixedSlugExec) Exec(ctx context.Context, _ string, req ShellRequest) (*ShellResult, error) {
	if req.Cwd == "" {
		req.Cwd = f.DefaultCwd
	}
	return f.Inner.Exec(ctx, f.Slug, req)
}

// SyncSkills delegates to Inner with the fixed Slug: the subagent sees
// the parent's skill set.
func (f FixedSlugExec) SyncSkills(ctx context.Context, _ string) error {
	return f.Inner.SyncSkills(ctx, f.Slug)
}
