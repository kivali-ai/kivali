package githubintegration

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The run actions control GitHub Actions: rerun a run or a job, cancel a
// run, start a workflow. Each is a REST action in core. Each needs the
// run_workflows switch as well as write (access.runsWorkflows), and each
// alone asks for a token with actions write (runToken); every read of
// Actions keeps asking for actions read.

// The run actions, unprefixed.
const (
	actionRerunWorkflowRun  = "rerun_workflow_run"
	actionRerunJob          = "rerun_job"
	actionCancelWorkflowRun = "cancel_workflow_run"
	actionDispatchWorkflow  = "dispatch_workflow"
)

// needsRunWorkflows ends the owner's description of every run action:
// the run_workflows switch and the write switch, in their titles' words.
const needsRunWorkflows = " Only when the agent's access may run workflows and may create, comment, push, merge and close."

// The bounds on dispatch_workflow's inputs, as characters except for
// inputsMost, the most properties GitHub takes.
const (
	inputsMost     = 25
	inputNameMost  = 100
	inputValueMost = 10000
)

// runActions are the run actions, in the manifest's order after the
// others (actions).
var runActions = []action{
	{
		name: actionRerunWorkflowRun,
		description: "Run a GitHub Actions workflow run of a GitHub repository again, by its id (github_list_workflow_runs names it): all its jobs, or with failed_only the jobs that failed and those that depend on them. " +
			"The workflow runs on GitHub with the repository's Actions secrets. A run still in progress cannot be rerun. github_get_workflow_run follows the new attempt.",
		ownerDescription: "Run a workflow run again, all its jobs or only the failed ones; it runs on GitHub with the repository's Actions secrets." + needsRunWorkflows,
		schema: schema(map[string]any{
			"repository":  repositoryProperty,
			"run_id":      map[string]any{"type": "integer", "description": "The workflow run's id."},
			"failed_only": map[string]any{"type": "boolean", "description": "Rerun only the jobs that failed and those that depend on them."},
		}, "repository", "run_id"),
		write:        true,
		runsWorkflow: true,
		run:          rerunWorkflowRun,
	},
	{
		name: actionRerunJob,
		description: "Run one job of a GitHub Actions workflow run of a GitHub repository again, with the jobs that depend on it, by the job's id (github_get_workflow_run names it). " +
			"The workflow runs on GitHub with the repository's Actions secrets. A job whose run is still in progress cannot be rerun. github_get_workflow_run follows the new attempt.",
		ownerDescription: "Run one job of a workflow run again, with the jobs that depend on it; it runs on GitHub with the repository's Actions secrets." + needsRunWorkflows,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"job_id":     map[string]any{"type": "integer", "description": "The job's id."},
		}, "repository", "job_id"),
		write:        true,
		runsWorkflow: true,
		run:          rerunJob,
	},
	{
		name: actionCancelWorkflowRun,
		description: "Ask GitHub to cancel a GitHub Actions workflow run of a GitHub repository that has not finished, by its id. GitHub cancels it after answering, so github_get_workflow_run reads whether it stopped. " +
			"Steps the workflow runs whatever happens still run, on GitHub with the repository's Actions secrets. A run that has finished cannot be cancelled.",
		ownerDescription: "Ask GitHub to cancel a workflow run that has not finished; steps the workflow runs whatever happens still run, with the repository's Actions secrets." + needsRunWorkflows,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"run_id":     map[string]any{"type": "integer", "description": "The workflow run's id."},
		}, "repository", "run_id"),
		write:        true,
		runsWorkflow: true,
		run:          cancelWorkflowRun,
	},
	{
		name: actionDispatchWorkflow,
		description: "Start a GitHub Actions workflow of a GitHub repository on a branch or a tag, by the workflow's id or its file's name (github_list_workflows names them). " +
			"The workflow's file on that branch or tag must have a workflow_dispatch trigger. The workflow runs on GitHub with the repository's Actions secrets. " +
			"Answers the new run's id and address; github_get_workflow_run follows it.",
		ownerDescription: "Start a workflow that has a workflow_dispatch trigger, on a branch or a tag, with inputs the agent gives; it runs on GitHub with the repository's Actions secrets." + needsRunWorkflows,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"workflow":   map[string]any{"type": "string", "description": "The workflow's id, or its file's name, such as ci.yml."},
			"ref":        map[string]any{"type": "string", "description": "The branch or tag to run it on."},
			"inputs": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": []string{"string", "number", "boolean"}},
				"description":          "The workflow's inputs by name, at most 25, each a string, a number or a boolean.",
			},
		}, "repository", "workflow", "ref"),
		write:        true,
		runsWorkflow: true,
		run:          dispatchWorkflow,
	},
}

// runToken is a token narrowed to repo that may write its Actions runs.
// GitHub mints no token at all for a permission the installation lacks,
// which is what an app reads as until the owner approves actions write
// (requirements.go).
func (c *call) runToken(ctx context.Context, repo string) (string, error) {
	tok, err := c.token(ctx, repo, perms("actions", "write"))
	var te *tokenError
	if errors.As(err, &te) && !te.account && te.api.Status == http.StatusUnprocessableEntity {
		return "", failure("GitHub refused the GitHub App a token that may run workflows in " + repo + " (HTTP 422): " + sentence(te.api.Message) +
			". The app's installation may lack the Actions write permission; ask the owner to approve it on GitHub.")
	}
	return tok, err
}

// runRefused is GitHub's refusal of a run action in words the agent can
// act on: GitHub's own, then note, what is so of the action whichever
// refusal this is, since GitHub's documents bind no status to a run in
// progress or a workflow without a trigger. A limit on the app's
// requests, and any other failure, stay as they are.
func runRefused(err error, what, note string) error {
	var ae *apiError
	if !errors.As(err, &ae) || ae.RateLimited {
		return err
	}
	switch ae.Status {
	case http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity:
		return failure(refusedWords(ae, what, note))
	}
	return err
}

// refusedWords is GitHub's refusal of what in its own words, then note,
// then the owner for anything else.
func refusedWords(ae *apiError, what, note string) string {
	return fmt.Sprintf("GitHub did not %s (HTTP %d): %s. %s For anything else, ask the owner.", what, ae.Status, sentence(ae.Message), note)
}

// post sends path a POST with no body, with a token that may write
// repo's Actions runs.
func (c *call) post(ctx context.Context, repo, path string) error {
	tok, err := c.runToken(ctx, repo)
	if err != nil {
		return err
	}
	_, _, err = c.g.do(ctx, request{method: http.MethodPost, path: path, auth: tok})
	return err
}

// The notes runRefused adds.
const (
	rerunNote  = "A run still in progress cannot be rerun; github_get_workflow_run reads its status."
	cancelNote = "A run that has finished cannot be cancelled; github_get_workflow_run reads its status."
	startNote  = "The workflow's file on that branch or tag must have a workflow_dispatch trigger, and the inputs must be ones it declares."
)

// rerunWorkflowRun reruns a run (POST .../actions/runs/{id}/rerun), or
// with failed_only its failed jobs (.../rerun-failed-jobs).
func rerunWorkflowRun(ctx context.Context, c *call) (any, error) {
	repo, id, err := repoAndID(c, "run_id")
	if err != nil {
		return nil, err
	}
	failedOnly, err := c.args.boolean("failed_only")
	if err != nil {
		return nil, err
	}
	run := fmt.Sprintf("workflow run %d of %s", id, repo)
	path := fmt.Sprintf("%s/actions/runs/%d/rerun", repoPath(repo), id)
	said := "started " + run + " again"
	if failedOnly {
		path += "-failed-jobs"
		said = "started the failed jobs of " + run + " again"
	}
	if err := c.post(ctx, repo, path); err != nil {
		return nil, runRefused(err, "rerun "+run, rerunNote)
	}
	return done(said), nil
}

// rerunJob reruns a job (POST .../actions/jobs/{id}/rerun).
func rerunJob(ctx context.Context, c *call) (any, error) {
	repo, id, err := repoAndID(c, "job_id")
	if err != nil {
		return nil, err
	}
	job := fmt.Sprintf("job %d of %s", id, repo)
	if err := c.post(ctx, repo, fmt.Sprintf("%s/actions/jobs/%d/rerun", repoPath(repo), id)); err != nil {
		return nil, runRefused(err, "rerun "+job, rerunNote)
	}
	return done("started " + job + " again"), nil
}

// cancelWorkflowRun asks GitHub to cancel a run (POST
// .../actions/runs/{id}/cancel). GitHub answers 202 and cancels it
// after, so the answer says it was asked.
func cancelWorkflowRun(ctx context.Context, c *call) (any, error) {
	repo, id, err := repoAndID(c, "run_id")
	if err != nil {
		return nil, err
	}
	run := fmt.Sprintf("workflow run %d of %s", id, repo)
	if err := c.post(ctx, repo, fmt.Sprintf("%s/actions/runs/%d/cancel", repoPath(repo), id)); err != nil {
		return nil, runRefused(err, "cancel "+run, cancelNote)
	}
	return done("asked GitHub to cancel " + run), nil
}

// inputs is the inputs argument: at most inputsMost names, each one line
// of at most inputNameMost characters, to a string of at most
// inputValueMost characters, a number or a boolean, at most maxText
// characters in all; nil when absent or empty.
func (a args) inputs() (map[string]any, error) {
	v, present := a["inputs"]
	if !present || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badArgument("inputs is an object of names and values.")
	}
	if len(m) > inputsMost {
		return nil, badArgument(fmt.Sprintf("inputs holds at most %d values.", inputsMost))
	}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > inputNameMost || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, badArgument(fmt.Sprintf("An input's name is one line of at most %d characters.", inputNameMost))
		}
		switch value := m[name].(type) {
		case string:
			if utf8.RuneCountInString(value) > inputValueMost {
				return nil, badArgument(fmt.Sprintf("Each of inputs is at most %d characters.", inputValueMost))
			}
		case json.Number, bool:
		default:
			return nil, badArgument("Each of inputs is a string, a number or a boolean.")
		}
	}
	if encodedLen(m) > maxText {
		return nil, badArgument(fmt.Sprintf("inputs is longer than %d characters.", maxText))
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// dispatchWorkflow starts a workflow (POST
// .../actions/workflows/{workflow}/dispatches). Any success has started
// it, so an answer that names no run still says so, and sends the agent
// to the listing rather than to start it again. A request GitHub did not
// answer may have started it too.
func dispatchWorkflow(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	workflow, err := c.args.workflow()
	if err != nil {
		return nil, err
	}
	if workflow == "" {
		return nil, badArgument("Give the workflow: its id or its file's name, such as ci.yml.")
	}
	ref, err := c.args.branch("ref", true)
	if err != nil {
		return nil, badArgument("ref is a branch or a tag.")
	}
	inputs, err := c.args.inputs()
	if err != nil {
		return nil, err
	}
	tok, err := c.runToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	// apiVersion answers a dispatch with the new run's id only when asked.
	payload := map[string]any{"ref": ref, "return_run_details": true}
	if inputs != nil {
		payload["inputs"] = inputs
	}
	started := fmt.Sprintf("the workflow %s of %s on %s", workflow, repo, ref)
	b, _, err := c.g.do(ctx, request{method: http.MethodPost, path: repoPath(repo) + "/actions/workflows/" + workflow + "/dispatches", auth: tok, body: payload})
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 0 {
			return nil, failure("GitHub did not answer, so " + started + " may have started; github_list_workflow_runs shows whether it did.")
		}
		return nil, runRefused(err, "start "+started, startNote)
	}
	var out struct {
		WorkflowRunID int64  `json:"workflow_run_id"`
		HTMLURL       string `json:"html_url"`
	}
	if json.Unmarshal(b, &out) != nil || out.WorkflowRunID <= 0 {
		return done("started " + started + "; GitHub named no run, and github_list_workflow_runs lists it"), nil
	}
	address := cmp.Or(out.HTMLURL, fmt.Sprintf("%s/%s/actions/runs/%d", webBase, repo, out.WorkflowRunID))
	return done(fmt.Sprintf("started %s as run %d (%s)", started, out.WorkflowRunID, address)), nil
}
