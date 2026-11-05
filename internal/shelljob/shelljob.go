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
	// Git is a clone, a fetch or a push through a built-in integration's
	// connection (internal/builtinintegrations/gitops), run by the
	// agent-shell daemon (POST /v1/git).
	Git *GitJob `json:"git,omitempty"`
	// Download is one file fetched through a built-in integration's
	// connection into the agent's files, run by the agent-shell daemon
	// (POST /v1/download).
	Download *DownloadJob `json:"download,omitempty"`
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
//   - AllowedRefs are the branches and tags a push may write, patterns
//     normalised (gitrepo.NormalizeRefPatterns; Git (Basic)'s are the
//     access's push_refs); the daemon refuses a push refspec whose
//     destination is outside them.
//   - AnyBranchOrTag lets a push write any branch or tag (every ref
//     under refs/heads/ or refs/tags/) in place of AllowedRefs, which is
//     then empty: for an access that lets an agent push without naming
//     branches and tags. It widens nothing else: the URL, force and
//     deletions are held as for any push.
//   - Force on a fetch lets a ref move other than fast-forward. On a
//     push it is the owner's permission to force: the daemon refuses a
//     refspec with + when it is false.
//   - Identity, on a clone, is the git identity the daemon writes into
//     the new working copy's .git/config (user.name and user.email)
//     once the clone has succeeded, so commits made there are authored
//     and committed as it unless the agent changes it: GitHub's is its
//     app's bot user. Nil writes none (Git (Basic)'s), and a fetch or
//     a push ignores it.
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
	Op                  string       `json:"op"`
	Path                string       `json:"path"`
	URL                 string       `json:"url,omitempty"`
	RefSpecs            []string     `json:"refspecs,omitempty"`
	Force               bool         `json:"force,omitempty"`
	AllowedRepositories []string     `json:"allowed_repositories,omitempty"`
	AllowedRefs         []string     `json:"allowed_refs,omitempty"`
	AnyBranchOrTag      bool         `json:"any_branch_or_tag,omitempty"`
	Identity            *GitIdentity `json:"identity,omitempty"`
	Host                string       `json:"host"`
	Auth                GitAuth      `json:"auth"`
	Proxy               GitProxy     `json:"proxy"`
	TimeoutSeconds      int          `json:"timeout_seconds,omitempty"`
	ConnectionID        string       `json:"connection_id,omitempty"`
	LFS                 *GitLFS      `json:"lfs,omitempty"`
}

// GitLFS has the job move the Git LFS files its commits name, through
// the repository's LFS API with the job's token: a push uploads them
// before any ref moves, a clone or a fetch downloads them into
// .git/lfs/objects (agentshell.GitLFS). Hosts are the hosts besides the
// repository's own where the LFS API may keep a file's content, which
// the connection's proxy credential admits too. Nil, as for Git
// (Basic), moves none.
type GitLFS struct {
	Hosts []string `json:"hosts,omitempty"`
}

// GitIdentity is the git identity a clone's working copy is given: its
// user.name and user.email.
type GitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
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

// DownloadJob is one file fetched through a connection, as the
// agent-shell daemon runs it (agentshell.DownloadRequest has the same
// fields).
//
//   - URL is GET with Token as a bearer credential and Accept, if any.
//     The token is narrowed to this one job and is sent nowhere else.
//   - Hosts are the host patterns (as kivali.hosts reads them) a redirect
//     from URL may lead to. A redirect is followed once, with no
//     credential of the job's; anywhere else is refused.
//   - Folder is the directory under /files/ the file lands in, made when
//     missing; Name its plain file name, to which NameZip adds .zip when
//     the body starts as a zip.
//   - MaxBytes bounds the file, TimeoutSeconds the job in the daemon.
//   - Proxy is the connection's scoped egress proxy credential.
//   - ConnectionID is echoed in DownloadResult.ConnectionID, as a GitJob's.
//
// It carries credentials: nothing on its path logs it.
type DownloadJob struct {
	URL            string   `json:"url"`
	Accept         string   `json:"accept,omitempty"`
	Token          string   `json:"token"`
	Hosts          []string `json:"hosts,omitempty"`
	Folder         string   `json:"folder"`
	Name           string   `json:"name"`
	NameZip        bool     `json:"name_zip,omitempty"`
	MaxBytes       int64    `json:"max_bytes,omitempty"`
	Proxy          GitProxy `json:"proxy"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	ConnectionID   string   `json:"connection_id,omitempty"`
}

// Result is what the agent's shell gave back for a Job, or Skipped, why
// nothing ran: an agent with no shell.
type Result struct {
	Skipped  string          `json:"skipped,omitempty"`
	Git      *GitResult      `json:"git,omitempty"`
	Download *DownloadResult `json:"download,omitempty"`
}

// Connection is the connection the result's job ran through, as the
// job echoed it; "" when it gives none.
func (r Result) Connection() string {
	switch {
	case r.Git != nil:
		return r.Git.ConnectionID
	case r.Download != nil:
		return r.Download.ConnectionID
	}
	return ""
}

// Duration is how long the result says its job ran, in milliseconds.
func (r Result) Duration() int64 {
	switch {
	case r.Git != nil:
		return r.Git.DurationMs
	case r.Download != nil:
		return r.Download.DurationMs
	}
	return 0
}

// DownloadResult is a DownloadJob's outcome: the path the file landed
// at and its size, whether it is a zip, or the error. Status is the
// answer of the job's URL when that was not a success, and TooLarge a
// body over MaxBytes. No file is left unless Error is empty.
type DownloadResult struct {
	Path         string `json:"path,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Zip          bool   `json:"zip,omitempty"`
	Status       int    `json:"status,omitempty"`
	Error        string `json:"error,omitempty"`
	TooLarge     bool   `json:"too_large,omitempty"`
	TimedOut     bool   `json:"timed_out,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
}

// GitResult is a GitJob's outcome: the repository it reached
// (owner/name), the refs it moved (a push's: the remote's refs it
// wrote), its error, how long it took and the connection it ran
// through (the job's ConnectionID, echoed). Updated is at most the
// daemon's first 200 refs by name; UpdatedTotal counts every ref that
// moved. Identity says a clone's working copy was given the job's
// Identity: false for a clone without one, and for one whose identity
// could not be written though the clone landed. LFSFiles and LFSBytes
// count the LFS files moved (a push's uploaded, a clone's or a fetch's
// downloaded); LFSError is why a clone or a fetch that landed did not
// get them.
type GitResult struct {
	Repository   string         `json:"repository,omitempty"`
	Updated      []GitRefUpdate `json:"updated,omitempty"`
	UpdatedTotal int            `json:"updated_total,omitempty"`
	Error        string         `json:"error,omitempty"`
	DurationMs   int64          `json:"duration_ms,omitempty"`
	TimedOut     bool           `json:"timed_out,omitempty"`
	ConnectionID string         `json:"connection_id,omitempty"`
	Identity     bool           `json:"identity,omitempty"`
	LFSFiles     int            `json:"lfs_files,omitempty"`
	LFSBytes     int64          `json:"lfs_bytes,omitempty"`
	LFSError     string         `json:"lfs_error,omitempty"`
}

// GitRefUpdate is one ref a git operation moved: its name and the
// object it pointed at before (empty for a new ref) and after.
type GitRefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new"`
}
