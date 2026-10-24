// Package shelljob is the work core asks an agent's shell to do for
// one integration call, and what the shell gives back: the wire shapes
// of agentpod.IntegrationShellJob and agentpod.IntegrationShellJobResult,
// which are these types under the names core.sock's protocol gives
// them. It is a leaf package, so builtinintegrations, which agentpod's
// own dependencies' tests import, can name them without a cycle.
package shelljob

// Job is the work core asks the agent's shell to do for one call. One
// field is set.
type Job struct {
	// Git is a clone, a fetch or a push with the Git integration's connection,
	// run by the agent-shell daemon (POST /v1/git).
	Git *GitJob `json:"git,omitempty"`
}

// GitJob is one git operation through a connection, as the agent-shell
// daemon runs it (agentshell.GitRequest has the same fields).
//
//   - Op is clone, fetch or push. Path is the working copy, under
//     /files/ or the shell's home.
//   - URL is the repository's URL with no user info, built by core from
//     the connection's host. A clone always has one. A fetch or a push
//     without one has the daemon read the working copy's origin, which
//     it uses only when it is on Host and one of AllowedRepositories
//     (owner/name patterns from the agent's access: the repositories it
//     may pull or push to, or for a push only the ones it may push to).
//   - RefSpecs are a fetch's refspecs (empty: every branch) or a push's,
//     normalised by core ([+]src:dst, gitrepo.ParsePushRefSpec).
//   - AllowedRefs are the branches and tags a push may write, the
//     access's push_refs normalised (gitrepo.NormalizeRefPatterns); the
//     daemon refuses a push refspec whose destination is outside them.
//   - Force on a fetch lets a ref move other than fast-forward. On a
//     push it is the owner's permission to force: the daemon refuses a
//     refspec with + when it is false.
//   - Auth is the connection's token as HTTP basic auth; Proxy the
//     connection's scoped egress proxy credential, which the daemon
//     puts on its own bare proxy URL.
//   - TimeoutSeconds bounds the operation in the daemon.
//   - ConnectionID is the connection the job runs through, which the
//     daemon echoes in its result (GitResult.ConnectionID), so the
//     call's second request knows it even when the integration has
//     another connection by then.
//
// It carries credentials: nothing on its path logs it.
type GitJob struct {
	Op                  string   `json:"op"`
	Path                string   `json:"path"`
	URL                 string   `json:"url,omitempty"`
	RefSpecs            []string `json:"refspecs,omitempty"`
	Force               bool     `json:"force,omitempty"`
	AllowedRepositories []string `json:"allowed_repositories,omitempty"`
	AllowedRefs         []string `json:"allowed_refs,omitempty"`
	Host                string   `json:"host"`
	Auth                GitAuth  `json:"auth"`
	Proxy               GitProxy `json:"proxy"`
	TimeoutSeconds      int      `json:"timeout_seconds,omitempty"`
	ConnectionID        string   `json:"connection_id,omitempty"`
}

// GitAuth is the HTTP basic auth a git operation presents to the host.
type GitAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// GitProxy is the egress proxy credential a git operation presents.
type GitProxy struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Result is what the agent's shell gave back for a Job, or Skipped, why
// nothing ran: an agent with no shell.
type Result struct {
	Skipped string     `json:"skipped,omitempty"`
	Git     *GitResult `json:"git,omitempty"`
}

// GitResult is a GitJob's outcome: the repository it reached
// (owner/name), the refs it moved (a push's: the remote's refs it
// wrote), its error, how long it took and the connection it ran
// through (the job's ConnectionID, echoed). Updated is at most the
// daemon's first 200 refs by name; UpdatedTotal counts every ref that
// moved.
type GitResult struct {
	Repository   string         `json:"repository,omitempty"`
	Updated      []GitRefUpdate `json:"updated,omitempty"`
	UpdatedTotal int            `json:"updated_total,omitempty"`
	Error        string         `json:"error,omitempty"`
	DurationMs   int64          `json:"duration_ms,omitempty"`
	TimedOut     bool           `json:"timed_out,omitempty"`
	ConnectionID string         `json:"connection_id,omitempty"`
}

// GitRefUpdate is one ref a git operation moved: its name and the
// object it pointed at before (empty for a new ref) and after.
type GitRefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new"`
}
