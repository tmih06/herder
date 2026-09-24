package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/deliver"
	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/validation"
)

// ensureSandbox resolves the task's container and workspace, restarting a
// stopped container so validation can exec into it after a restart. The
// workspace comes from the container's /workspace mount, falling back to
// the deterministic path under root.
func ensureSandbox(ctx context.Context, provider *sandbox.DockerProvider,
	task *tasks.Task, root string,
) (container, workspace string, err error) {
	container = task.SandboxID
	if container == "" {
		container = sandbox.ContainerName(task.ID)
	}
	sb, err := provider.Inspect(ctx, container)
	if err != nil {
		return "", "", fmt.Errorf("sandbox %s not ready: %w", container, err)
	}
	if sb.Status != "running" {
		if err := provider.EnsureRunning(ctx, container); err != nil {
			return "", "", fmt.Errorf("sandbox %s not running: %w", container, err)
		}
	}
	workspace = sb.Workspace
	if workspace == "" {
		workspace = sandbox.WorkspacePath(root, task.ID)
	}
	return container, workspace, nil
}

// gateTimeout bounds one validation run: sandbox commands plus the
// host-side git checks. Configured commands can legitimately take a
// while, so this is generous but finite.
const gateTimeout = 30 * time.Minute

// deliverTimeout bounds the push/PR/comment/label sequence: every call is
// a local CLI invocation and must fail fast, never hang delivery.
const deliverTimeout = 5 * time.Minute

// prepareTask resolves the shared gate preamble for validate/deliver:
// the task must exist and sit in one of states, and its repository must
// be configured. The returned code is the exit code to return on refusal
// (already reported on ew); -1 means proceed.
func prepareTask(cfg *config.Config, store *storage.Store, args []string,
	verb string, states []tasks.State, ew io.Writer,
) (tasks.Task, config.RepositoryConfig, *sandbox.DockerProvider, int) {
	fail := func(code int, format string, a ...any) (tasks.Task, config.RepositoryConfig, *sandbox.DockerProvider, int) {
		fmt.Fprintf(ew, format, a...)
		return tasks.Task{}, config.RepositoryConfig{}, nil, code
	}
	if len(args) != 1 {
		return fail(2, "herder: usage: herder task %s <id>\n", verb)
	}
	task, err := store.GetTask(args[0])
	if err != nil {
		return fail(1, "herder: task %q not found\n", args[0])
	}
	allowed := false
	for _, s := range states {
		if task.Status == s {
			allowed = true
			break
		}
	}
	if !allowed {
		return fail(1, "herder: task %s in %s cannot %s (want %s)\n",
			task.ID, task.Status, verb, strings.Join(stateNames(states), " or "))
	}
	repo, ok := cfg.Repositories[task.Repository]
	if !ok {
		return fail(1, "herder: task repository %q not in config\n", task.Repository)
	}
	return task, repo, newProvider(ew), -1
}

// stateNames renders a state list for refusal messages.
func stateNames(states []tasks.State) []string {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	return names
}

// gateOrRoute runs the validation gate and routes the outcome: a pass
// returns ok with the task left in REVIEWING; a failure returns the exit
// code from routeValidationFailure; a transport error returns 1. Either
// way the caller's task copy reflects the new state.
func gateOrRoute(ctx context.Context, cfg *config.Config, store *storage.Store,
	provider *sandbox.DockerProvider, task *tasks.Task, repo config.RepositoryConfig,
	w, ew io.Writer,
) (code int, ok bool) {
	container, workspace, err := ensureSandbox(ctx, provider, task, dispatch.SandboxRoot(cfg.Database.Path))
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1, false
	}
	rep, ok := runGate(ctx, store, provider, task, repo, container, workspace, ew)
	if !ok {
		return 1, false
	}
	if !rep.Passed() {
		return routeValidationFailure(ctx, store, task, rep, w, ew), false
	}
	return 0, true
}

// taskValidate runs the repository's validation gate inside the task
// sandbox and routes the outcome: pass advances toward REVIEWING, failure
// returns to the live agent or to a human with the failure attached.
func taskValidate(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	task, repo, provider, code := prepareTask(cfg, store, args, "validate",
		[]tasks.State{tasks.Running, tasks.Validating, tasks.WaitingForHuman}, ew)
	if code >= 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	if code, passed := gateOrRoute(ctx, cfg, store, provider, &task, repo, w, ew); !passed {
		return code
	}
	fmt.Fprintf(w, "herder: task %s validation passed\n", task.ID)
	return 0
}

// taskDeliver runs the full ship path: validation gate, push the task
// branch under the controller's identity, open the PR, comment the issue,
// and advance its stage labels. Resumable: VALIDATING re-runs the gate,
// REVIEWING/DELIVERING pick up after it, PR_OPEN is already delivered.
func taskDeliver(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	task, repo, provider, code := prepareTask(cfg, store, args, "deliver",
		[]tasks.State{
			tasks.Running, tasks.Validating, tasks.WaitingForHuman,
			tasks.Reviewing, tasks.Delivering, tasks.PROpen,
		}, ew)
	if code >= 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	// The gate runs for RUNNING/VALIDATING tasks and for
	// WAITING_FOR_HUMAN ones whose worker disconnected before the gate
	// ever ran; REVIEWING and beyond already passed it (resume after a
	// crash mid-delivery). A branch that moved since the recorded pass
	// re-enters the gate instead of shipping unverified commits.
	if task.Status == tasks.Running || task.Status == tasks.Validating ||
		task.Status == tasks.WaitingForHuman {
		if code, passed := gateOrRoute(ctx, cfg, store, provider, &task, repo, w, ew); !passed {
			return code
		}
	} else if task.Status != tasks.PROpen && repo.Delivery.CreatePR {
		moved, err := branchMoved(ctx, cfg, provider, store, &task)
		if err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return 1
		}
		if moved {
			fmt.Fprintf(ew, "herder: task %s branch moved since validation; re-running the gate\n", task.ID)
			if code, passed := gateOrRoute(ctx, cfg, store, provider, &task, repo, w, ew); !passed {
				return code
			}
		}
	}
	if task.Status == tasks.PROpen {
		fmt.Fprintf(w, "herder: task %s already delivered (PR open)\n", task.ID)
		return 0
	}
	if !repo.Delivery.CreatePR {
		// A task resumed from DELIVERING rests back in REVIEWING; the
		// delivery.skipped event records why no remote work happened.
		if task.Status == tasks.Delivering {
			if err := transition(store, &task, tasks.Reviewing, ew); err != nil {
				fmt.Fprintf(ew, "herder: %v\n", err)
				return 1
			}
		}
		if err := emitEvent(store, task.ID, "delivery.skipped",
			map[string]string{"reason": "delivery.create_pr is false"}, ew); err != nil {
			return 1
		}
		// The review stage of the label path still applies: the work is
		// validated and awaiting human review even without a PR.
		if _, hasIssue := deliver.IssueNumber(task.SourceRef); hasIssue {
			d := newDispatcher(store, w, ew)
			if err := d.AdvanceIssueLabels(ctx, &task, repo,
				repo.Delivery.LabelSet().Review); err != nil {
				fmt.Fprintf(ew, "herder: %v\n", err)
				return 1
			}
		}
		fmt.Fprintf(w, "herder: task %s validated; delivery.create_pr is false, resting in %s\n", task.ID, task.Status)
		return 0
	}
	return deliverPR(ctx, cfg, store, provider, &task, repo, w, ew)
}

// taskWorkspace resolves the directory the task's container mounts at
// /workspace, falling back to the deterministic path when the container
// is already gone.
func taskWorkspace(ctx context.Context, cfg *config.Config, provider *sandbox.DockerProvider,
	task *tasks.Task,
) string {
	container := task.SandboxID
	if container == "" {
		container = sandbox.ContainerName(task.ID)
	}
	if sb, err := provider.Inspect(ctx, container); err == nil && sb.Workspace != "" {
		return sb.Workspace
	}
	return sandbox.WorkspacePath(dispatch.SandboxRoot(cfg.Database.Path), task.ID)
}

// validatedHead returns the HEAD SHA recorded by the most recent
// validation.passed event; empty means no recorded pass (permissive).
func validatedHead(store *storage.Store, taskID string) string {
	events, err := store.ListEvents(taskID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "validation.passed" {
			continue
		}
		var payload struct {
			Head string `json:"head"`
		}
		if json.Unmarshal([]byte(events[i].Payload), &payload) == nil {
			return payload.Head
		}
	}
	return ""
}

// branchMoved reports whether the task branch's tip differs from the
// commit the gate validated: a still-live agent can commit after the
// pass, and only the validated SHA may ship. No recorded pass counts as
// moved — a REVIEWING task without one re-runs the gate instead of
// pushing unverified commits.
func branchMoved(ctx context.Context, cfg *config.Config, provider *sandbox.DockerProvider,
	store *storage.Store, task *tasks.Task,
) (bool, error) {
	want := validatedHead(store, task.ID)
	if want == "" {
		return true, nil
	}
	workspace := taskWorkspace(ctx, cfg, provider, task)
	engine := &deliver.Engine{}
	sha, err := engine.BranchSHA(ctx, workspace, dispatch.BranchForTask(*task))
	if err != nil {
		return false, err
	}
	return sha != want, nil
}

// provisionedBase returns the upstream base SHA recorded by the FIRST
// sandbox.provisioned event — captured before the agent ran, so the
// gate's diff base cannot be forged by moving refs in the workspace.
// Later events are ignored: a re-provision after the agent ran would
// record an agent-influenced origin/HEAD. Empty means no record
// (hand-rolled workspace): the gate falls back to merge-base.
func provisionedBase(store *storage.Store, taskID string) string {
	events, err := store.ListEvents(taskID)
	if err != nil {
		return ""
	}
	for _, e := range events {
		if e.Type != "sandbox.provisioned" {
			continue
		}
		// First event wins — even when its base_sha is empty (a workspace
		// provisioned before this field existed). Skipping to a later
		// event would adopt a SHA recorded after the agent ran, which is
		// agent-influenced and forgeable.
		var payload struct {
			BaseSHA string `json:"base_sha"`
		}
		_ = json.Unmarshal([]byte(e.Payload), &payload)
		return payload.BaseSHA
	}
	return ""
}

// deliverPR performs the remote delivery steps: push, PR, comment,
// labels, then DELIVERING -> PR_OPEN. Failures leave the task in
// DELIVERING so a rerun resumes instead of duplicating work.
func deliverPR(ctx context.Context, cfg *config.Config, store *storage.Store,
	provider *sandbox.DockerProvider, task *tasks.Task, repo config.RepositoryConfig,
	w, ew io.Writer,
) int {
	// Push from the workspace the container actually mounts — the gate
	// validated that directory, so that directory ships. The deterministic
	// path is only a fallback when the container is already gone.
	workspace := taskWorkspace(ctx, cfg, provider, task)
	branch := dispatch.BranchForTask(*task)
	engine := &deliver.Engine{}
	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()

	// A task already in DELIVERING is resuming after a mid-delivery
	// failure; the state machine has no self-transition, so skip it.
	if task.Status != tasks.Delivering {
		if err := transition(store, task, tasks.Delivering, ew); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return 1
		}
	}
	if err := emitEvent(store, task.ID, "delivery.started",
		map[string]string{"branch": branch, "repository": task.Repository}, ew); err != nil {
		return 1
	}
	if err := engine.PushBranch(dctx, workspace, task.Repository, branch, validatedHead(store, task.ID)); err != nil {
		return deliverFailed(store, task, err, ew)
	}
	issue, _ := deliver.IssueNumber(task.SourceRef)
	title := prTitle(*task)
	body := prBody(*task, issue)
	url, err := engine.CreatePR(dctx, task.Repository, branch, title, body)
	if err != nil {
		return deliverFailed(store, task, err, ew)
	}
	if err := emitEvent(store, task.ID, "pull_request.created",
		map[string]string{"url": url, "branch": branch}, ew); err != nil {
		return 1
	}
	if issue > 0 {
		comment := fmt.Sprintf("Herder opened %s for this issue (task %s, branch `%s`).", url, task.ID, branch)
		if err := engine.CommentIssue(dctx, task.Repository, issue, comment, url); err != nil {
			return deliverFailed(store, task, err, ew)
		}
		if err := emitEvent(store, task.ID, "issue.commented",
			map[string]any{"issue": issue, "pr": url}, ew); err != nil {
			return 1
		}
		d := newDispatcher(store, w, ew)
		if err := d.AdvanceIssueLabels(dctx, task, repo,
			repo.Delivery.LabelSet().Review); err != nil {
			return deliverFailed(store, task, err, ew)
		}
	}
	if err := transition(store, task, tasks.PROpen, ew); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, "delivery.completed",
		map[string]string{"pr": url}, ew); err != nil {
		return 1
	}
	fmt.Fprintf(w, "herder: task %s delivered: %s\n", task.ID, url)
	return 0
}

// taskDone completes a shipped task: PR_OPEN always, REVIEWING only when
// delivery.create_pr is false. The completed label lands before DONE so
// a crash between them leaves a retryable task, not a silent skip.
func taskDone(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	task, repo, _, code := prepareTask(cfg, store, args, "done",
		[]tasks.State{tasks.PROpen, tasks.Reviewing}, ew)
	if code >= 0 {
		return code
	}
	// REVIEWING is the shipped state only when delivery does not open
	// PRs; with create_pr the task must go through task deliver first.
	if task.Status == tasks.Reviewing && repo.Delivery.CreatePR {
		fmt.Fprintf(ew, "herder: task %s in REVIEWING has no PR; run `herder task deliver %s` first\n",
			task.ID, task.ID)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	if _, ok := deliver.IssueNumber(task.SourceRef); ok {
		d := newDispatcher(store, w, ew)
		if err := d.AdvanceIssueLabels(ctx, &task, repo,
			repo.Delivery.LabelSet().Completed); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return 1
		}
	}
	if err := transition(store, &task, tasks.Done, ew); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, "task.completed",
		map[string]string{"source_ref": task.SourceRef}, ew); err != nil {
		return 1
	}
	fmt.Fprintf(w, "herder: task %s done\n", task.ID)
	return 0
}

// runGate transitions the task into VALIDATING when needed and runs the
// repository's validation sequence, streaming structured events into the
// task history. Returns the report; ok=false means a transport or state
// error already reported on ew (not a validation failure).
func runGate(ctx context.Context, store *storage.Store, provider *sandbox.DockerProvider,
	task *tasks.Task, repo config.RepositoryConfig, container, workspace string,
	ew io.Writer,
) (validation.Report, bool) {
	// RUNNING enters the gate normally; REVIEWING/DELIVERING re-enter it
	// when the branch moved since the recorded pass (drift re-check).
	if task.Status != tasks.Validating {
		if err := transition(store, task, tasks.Validating, ew); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return validation.Report{}, false
		}
	}
	// The emit hook captures the first append error: a lost
	// validation.passed event would drop the recorded head and silently
	// disable the push pin, so the run fails closed instead.
	var emitErr error
	gate := &validation.Gate{
		Exec: provider.Exec,
		Emit: func(eventType string, payload map[string]any) {
			if err := emitEvent(store, task.ID, eventType, payload, ew); err != nil && emitErr == nil {
				emitErr = err
			}
		},
	}
	rep, err := gate.Run(ctx, validation.Input{
		TaskID: task.ID, Container: container,
		Workspace: workspace, Branch: dispatch.BranchForTask(*task),
		BaseSHA: provisionedBase(store, task.ID), Config: repo.Validation,
	})
	if err != nil {
		fmt.Fprintf(ew, "herder: validation could not run: %v\n", err)
		return rep, false
	}
	if emitErr != nil {
		fmt.Fprintf(ew, "herder: record validation event: %v\n", emitErr)
		return rep, false
	}
	if rep.Passed() {
		if err := transition(store, task, tasks.Reviewing, ew); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return rep, false
		}
	}
	return rep, true
}

// routeValidationFailure returns a failed task to the agent when its
// session is still live — the failure text becomes the next prompt and
// the attempt counter moves — or to a human via WAITING_FOR_HUMAN. The
// sandbox and its work are preserved either way.
func routeValidationFailure(ctx context.Context, store *storage.Store, task *tasks.Task,
	rep validation.Report, w, ew io.Writer,
) int {
	summary := rep.Summary()
	launcher := &agent.Launcher{}
	if task.AgentSessionID != "" && launcher.IsLive(ctx, task.AgentSessionID) {
		bumped, err := store.IncrementAttempt(task.ID)
		if err != nil {
			fmt.Fprintf(ew, "herder: bump attempt: %v\n", err)
			return 1
		}
		// One atomic hop back to RUNNING: the agent never stopped, so a
		// RETRYING waypoint would only open a window where reconcile's
		// lease check requeues the task mid-transition and a second
		// dispatch launches a duplicate worker.
		if err := transition(store, task, tasks.Running, ew); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			return 1
		}
		prompt := fmt.Sprintf("Herder validation failed (attempt %d). Fix the issues below, commit your work, and report done again.\n\n%s",
			bumped.Attempt, summary)
		if err := launcher.SendPrompt(ctx, task.AgentSessionID, prompt); err != nil {
			// The agent is live but unreachable: a human must look.
			if err := transition(store, task, tasks.WaitingForHuman, ew); err != nil {
				fmt.Fprintf(ew, "herder: %v\n", err)
			}
			_ = emitEvent(store, task.ID, "validation.awaiting_human",
				map[string]string{"reason": "agent session unreachable: " + err.Error(), "summary": summary}, ew)
			fmt.Fprintf(ew, "herder: task %s validation failed; agent unreachable (%v)\n", task.ID, err)
			return 1
		}
		if err := emitEvent(store, task.ID, "validation.retry",
			map[string]any{"attempt": bumped.Attempt, "session": task.AgentSessionID, "summary": summary}, ew); err != nil {
			return 1
		}
		fmt.Fprintf(w, "herder: task %s validation failed; returned to agent (attempt %d)\n",
			task.ID, bumped.Attempt)
		return 1
	}
	if err := transition(store, task, tasks.WaitingForHuman, ew); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, "validation.awaiting_human",
		map[string]string{"reason": "no live agent session", "summary": summary}, ew); err != nil {
		return 1
	}
	fmt.Fprintf(w, "herder: task %s validation failed; waiting for human\n", task.ID)
	fmt.Fprintf(ew, "%s\n", summary)
	return 1
}

// deliverFailed records a delivery step failure and leaves the task in
// DELIVERING so a rerun resumes instead of duplicating remote work.
func deliverFailed(store *storage.Store, task *tasks.Task, cause error, ew io.Writer) int {
	_ = emitEvent(store, task.ID, "delivery.failed",
		map[string]string{"reason": cause.Error()}, ew)
	fmt.Fprintf(ew, "herder: %v\n", cause)
	return 1
}

// transition moves the task and keeps the caller's copy in sync.
func transition(store *storage.Store, task *tasks.Task, to tasks.State, ew io.Writer) error {
	if _, err := store.Transition(task.ID, to, "controller", "cli"); err != nil {
		return fmt.Errorf("transition %s -> %s: %w", task.Status, to, err)
	}
	task.Status = to
	return nil
}

// prTitle uses the goal's first line, falling back to the task id.
func prTitle(task tasks.Task) string {
	title, _, _ := strings.Cut(task.Goal, "\n")
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Herder task " + task.ID
	}
	return title
}

// prBody renders the PR body: the goal plus the durable link back to the
// issue and task so reviewers can trace the work.
func prBody(task tasks.Task, issue int) string {
	var b strings.Builder
	if g := strings.TrimSpace(task.Goal); g != "" {
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Opened by Herder for task `%s` (attempt %d).", task.ID, task.Attempt)
	if issue > 0 {
		fmt.Fprintf(&b, "\n\nCloses #%d", issue)
	}
	return b.String()
}
