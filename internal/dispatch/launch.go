package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/tasks"
)

// Launch runs the shared launch flow behind start, retry, handoff, and
// the scheduler: resolve profile -> sandbox inspect reports running
// (an OOM-killed container fails the task with the resource-limit
// reason instead) -> build the seed prompt -> reuse a live session
// (re-seeded unless the task already runs) or launch -> bind ->
// RUNNING -> agent.started event. priorAgent names the previous
// worker on a handoff so the
// prompt tells the new agent it inherits existing work instead of
// starting cold.
// A QUEUED, PROVISIONING, or RETRYING task takes the dispatch lease
// before any subprocess so two dispatchers never start it twice; the
// lease heartbeats until the call returns and releases on exit, unless
// an outer dispatch already owns it. All work runs on the lease-held
// context bounded by dispatchTimeout: a lost heartbeat cancels it so a
// stale CLI stops instead of finishing a dispatch the store requeued,
// and a hung subprocess dies at the bound instead of beating forever.
// Returns the first failure carrying the operator-facing message; the
// success line goes to Logf.
// An advance failure after a bound start — the task left the
// launchable set mid-dispatch, e.g. CANCELLED — stops the fresh pane
// and clears the binding so no unsupervised session outlives it.
func (d *Dispatcher) Launch(ctx context.Context, cfg *config.Config, task *tasks.Task, override, priorAgent string) error {
	// The dispatch lease serializes QUEUED, PROVISIONING, and RETRYING
	// launches (SPEC section 50): a second dispatcher sees ErrLeaseHeld
	// instead of double-starting. A live same-owner lease means an outer
	// dispatch owns the lifecycle — including an intervene holding the
	// lease across a retry's requeue-and-launch.
	hctx, release, err := d.HoldLease(ctx, task)
	if err != nil {
		return err
	}
	if release != nil {
		defer release()
	}
	ctx, cancel := context.WithTimeout(hctx, dispatchTimeout)
	defer cancel()
	repo, ok := cfg.Repositories[task.Repository]
	if !ok {
		return d.failTaskStart(ctx, task, fmt.Sprintf("repository %q not in config", task.Repository))
	}
	// One policy owns profile selection (agent.ResolveProfile): explicit
	// override, then the task's claimed profile, then the repo default. An
	// unknown override is an operator typo and leaves the task alone; an
	// unresolvable task profile fails the task with a clear event.
	profileName, prof, ok := agent.ResolveProfile(cfg, task.Repository, task.AgentProfile, override)
	if !ok {
		if override != "" {
			return fmt.Errorf("agent profile %q unknown", override)
		}
		return d.failTaskStart(ctx, task,
			fmt.Sprintf("agent profile %q unknown and no usable default", task.AgentProfile))
	}
	if _, ok := agent.CommandForKind(prof.Kind); !ok {
		return d.failTaskStart(ctx, task, fmt.Sprintf("unknown agent kind %q", prof.Kind))
	}
	container := sandbox.ContainerName(task.ID)
	sb, err := d.provider().Inspect(ctx, container)
	if err != nil {
		return fmt.Errorf("sandbox %s not ready: run `herder sandbox provision %s` first (%w)",
			container, task.ID, err)
	}
	// OOMKilled is checked before the status word: the engine reports
	// the kill on the dead container, and a kill is a resource-limit
	// verdict the task carries as FAILED, not a "not ready" hint.
	if sb.OOMKilled {
		return d.failTaskStart(ctx, task, "resource limit: container OOM-killed")
	}
	if sb.Status != "running" {
		return fmt.Errorf("sandbox %s not ready: status %s (run `herder sandbox provision %s` first)",
			container, sb.Status, task.ID)
	}
	workspace := sandbox.WorkspacePath(SandboxRoot(cfg.Database.Path), task.ID)
	session := agent.SessionName(task.ID)
	// The deterministic worker branch: claimed when set, else herder/<issue>.
	// The prompt is built before the reuse/launch decision because a reused
	// session on a not-yet-running task is re-seeded with the same contract.
	task.BranchName = BranchForTask(*task)
	prompt := agent.BuildPrompt(agent.PromptInput{
		Task: *task, AgentKind: prof.Kind, ProfileName: profileName,
		Repo:             repo,
		RepoInstructions: agent.LoadRepoInstructions(workspace),
		SandboxID:        container, Workspace: workspace,
		PriorAgent: priorAgent,
	})
	if task.AgentSessionID != "" && d.launcher().IsLive(ctx, task.AgentSessionID) {
		return d.reuseSession(ctx, task, repo, prompt, map[string]any{
			"session": task.AgentSessionID, "kind": prof.Kind, "profile": profileName,
			"sandbox": container, "reused": true,
		})
	}
	res, err := d.launcher().Start(ctx, agent.StartInput{
		Session: session, AgentKind: prof.Kind,
		Workspace: workspace, Container: container, Prompt: prompt,
	})
	if err != nil {
		// Record the binding optimistically so a session that did start
		// (send-side failure) stays attachable and retry re-seeds it;
		// failTaskStart clears it when the session never came up.
		if bindErr := d.Store.SetBinding(task.ID, container, session); bindErr == nil {
			task.AgentSessionID = session
		}
		return d.failTaskStart(ctx, task, err.Error())
	}
	// Bind only after the pane exists: a failed Start leaves no session
	// name behind for attach to resolve. A bind failure still fails the
	// task, and the reason names the orphaned session for manual recovery.
	if err := d.Store.SetBinding(task.ID, container, session); err != nil {
		return d.failTaskStart(ctx, task,
			fmt.Sprintf("session %s started but binding failed: %v", session, err))
	}
	// "starting" is the one normalized state Herder mints itself: Herdr's
	// first detection report lands on the next supervisor poll.
	if _, err := d.Store.RecordAgentState(task.ID, tasks.AgentStarting,
		d.actorType(), d.actorID()); err != nil {
		d.warnf("herder: record agent state: %v", err)
	}
	if err := d.advanceToRunning(ctx, task, true); err != nil {
		// The task left the launchable set mid-dispatch (CANCELLED has
		// no outgoing edges), so nothing supervises it: stop the
		// just-bound pane on a fresh bounded ctx — a cancelled parent
		// must not skip cleanup — and drop the binding so attach
		// reports no session instead of landing on an orphaned pane.
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stopCancel()
		if stopErr := d.launcher().Stop(stopCtx, session); stopErr != nil {
			d.warnf("herder: stop orphaned session %s: %v", session, stopErr)
		}
		if clearErr := d.Store.ClearSessionBinding(task.ID); clearErr != nil {
			d.warnf("herder: clear orphaned session binding: %v", clearErr)
		}
		return err
	}
	if err := d.emitEvent(task.ID, "agent.started", map[string]string{
		"session": session, "kind": prof.Kind, "profile": profileName,
		"sandbox": container, "pane": res.PaneID, "workspace_id": res.WorkspaceID,
		"branch": task.BranchName,
	}); err != nil {
		return err
	}
	// The issue's stage label moves to running once the agent is live;
	// best-effort so a missing gh never blocks a launch.
	d.MarkIssueRunning(task, repo)
	d.logf("herder: agent %s started for %s in session %s (pane %s)",
		prof.Kind, task.ID, session, res.PaneID)
	return nil
}

// reuseSession records a converged relaunch: the stored session still
// answers, so no new pane is opened. A not-yet-running task is re-seeded
// via SendPrompt so the reused pane carries the current goal (crash
// recovery); a RUNNING task's mid-work agent is left alone. A failed send
// fails the task like a failed launch. Then the task just ensures RUNNING.
func (d *Dispatcher) reuseSession(ctx context.Context, task *tasks.Task,
	repo config.RepositoryConfig, prompt string, payload map[string]any,
) error {
	if err := d.Store.SetBinding(task.ID, sandbox.ContainerName(task.ID), ""); err != nil {
		return fmt.Errorf("record sandbox binding: %w", err)
	}
	if task.Status != tasks.Running {
		if err := d.launcher().SendPrompt(ctx, task.AgentSessionID, prompt); err != nil {
			return d.failTaskStart(ctx, task, err.Error())
		}
	}
	if err := d.advanceToRunning(ctx, task, true); err != nil {
		return err
	}
	if err := d.emitEvent(task.ID, "agent.started", payload); err != nil {
		return err
	}
	d.MarkIssueRunning(task, repo)
	return nil
}

// emitEvent appends one agent lifecycle event with a JSON payload.
// Purpose: launch, reuse, and failure report their event types from one
// place so the marshal and error naming cannot drift between paths.
// Returns the append error named for the caller to print once.
func (d *Dispatcher) emitEvent(taskID, eventType string, payload any) error {
	raw, _ := json.Marshal(payload)
	if _, err := d.Store.AppendEvent(taskID, eventType,
		d.actorType(), d.actorID(), string(raw)); err != nil {
		return fmt.Errorf("record agent event: %w", err)
	}
	return nil
}

// failTaskStart moves the task to FAILED and records agent.start_failed
// with the reason, so an unlaunchable task is a clear event, not a hang.
// A cancelled ctx means the lease was lost or dispatchTimeout hit: the
// scheduler's expireLeases/reconcile owns the requeue, so the FAILED
// stamp is skipped and the reason returned with the ctx error instead
// of overwriting it. A dead session's stale binding is cleared first so
// task attach reports no session instead of execing a dead pane; a live
// session keeps its binding so attach can still land on it. The liveness
// probe runs on a fresh bounded context detached from ctx so a cancelled
// parent cannot false-negative and orphan a live session. Returns an
// error carrying the same "task %s failed: %s" line the CLI printed.
func (d *Dispatcher) failTaskStart(ctx context.Context, task *tasks.Task, reason string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", reason, ctx.Err())
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if task.AgentSessionID != "" && !d.launcher().IsLive(probeCtx, task.AgentSessionID) {
		if err := d.Store.ClearSessionBinding(task.ID); err != nil {
			d.warnf("herder: clear stale session binding: %v", err)
		}
		if _, err := d.Store.RecordAgentState(task.ID, tasks.AgentUnknown,
			d.actorType(), d.actorID()); err != nil {
			d.warnf("herder: record agent state: %v", err)
		}
	}
	if _, err := d.Store.Transition(task.ID, tasks.Failed, d.actorType(), d.actorID()); err != nil {
		d.warnf("herder: fail task %s: %v", task.ID, err)
	}
	if err := d.emitEvent(task.ID, "agent.start_failed",
		map[string]string{"reason": reason}); err != nil {
		return err
	}
	return fmt.Errorf("task %s failed: %s", task.ID, reason)
}
