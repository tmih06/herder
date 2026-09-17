package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// execAttach runs the interactive attach entry with the caller's stdio.
// Purpose: the human lands in the real running agent (glass-box); Herdr
// owns the session, so detaching leaves the agent running. A variable so
// tests capture argv without needing a TTY.
var execAttach = func(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "herder: attach: %v\n", err)
		return 1
	}
	return 0
}

// startTimeout bounds one task start: sandbox inspect plus the Herdr
// start/send calls are local and must fail fast, never hang a launch.
const startTimeout = 2 * time.Minute

// taskStart launches the task's agent through Herdr into its ready sandbox
// and records the durable task-sandbox-session link. Purpose: turn a queued
// task into a watchable session (issue #4 acceptance core). Flow: state
// gate -> launchTask. Unknown task profiles or agent kinds fail the task
// with agent.start_failed instead of hanging; an unknown --agent override
// is an operator typo and leaves the task alone.
func taskStart(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(ew)
	override := fs.String("agent", "", "agent profile (default: the task's claimed profile)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task start [--agent P] <id>\n")
		return 2
	}
	task, err := store.GetTask(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", fs.Arg(0))
		return 1
	}
	switch task.Status {
	case tasks.Queued, tasks.Provisioning, tasks.Running:
	default:
		fmt.Fprintf(ew, "herder: task %s in %s cannot start (want QUEUED, PROVISIONING, or RUNNING)\n",
			task.ID, task.Status)
		return 1
	}
	// The bounded ctx and launcher exist before the first failTaskStart so
	// every failure path can probe session liveness before clearing a link.
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	return launchTask(ctx, cfg, store, &task, *override, "", w, ew)
}

// launchTask runs the shared launch flow behind start, retry, and
// handoff: resolve profile -> sandbox inspect reports running -> build
// the seed prompt -> reuse a live session (re-seeded unless the task
// already runs) or launch -> bind -> RUNNING -> agent.started event.
// priorAgent names the previous worker on a handoff so the prompt tells
// the new agent it inherits existing work instead of starting cold.
func launchTask(ctx context.Context, cfg *config.Config, store *storage.Store, task *tasks.Task, override, priorAgent string, w, ew io.Writer) int {
	provider := sandbox.NewDockerProvider()
	provider.Log = func(format string, args ...any) {
		fmt.Fprintf(ew, format+"\n", args...)
	}
	launcher := &agent.Launcher{}
	repo, ok := cfg.Repositories[task.Repository]
	if !ok {
		return failTaskStart(ctx, launcher, store, task, fmt.Sprintf("repository %q not in config", task.Repository), ew)
	}
	// One policy owns profile selection (agent.ResolveProfile): explicit
	// override, then the task's claimed profile, then the repo default. An
	// unknown override is an operator typo and leaves the task alone; an
	// unresolvable task profile fails the task with a clear event.
	profileName, prof, ok := agent.ResolveProfile(cfg, task.Repository, task.AgentProfile, override)
	if !ok {
		if override != "" {
			fmt.Fprintf(ew, "herder: agent profile %q unknown\n", override)
			return 1
		}
		return failTaskStart(ctx, launcher, store, task,
			fmt.Sprintf("agent profile %q unknown and no usable default", task.AgentProfile), ew)
	}
	if _, ok := agent.CommandForKind(prof.Kind); !ok {
		return failTaskStart(ctx, launcher, store, task, fmt.Sprintf("unknown agent kind %q", prof.Kind), ew)
	}
	container := sandbox.ContainerName(task.ID)
	sb, err := provider.Inspect(ctx, container)
	if err != nil {
		fmt.Fprintf(ew, "herder: sandbox %s not ready: run `herder sandbox provision %s` first (%v)\n",
			container, task.ID, err)
		return 1
	}
	if sb.Status != "running" {
		fmt.Fprintf(ew, "herder: sandbox %s not ready: status %s (run `herder sandbox provision %s` first)\n",
			container, sb.Status, task.ID)
		return 1
	}
	workspace := sandbox.WorkspacePath(sandboxRoot(cfg.Database.Path), task.ID)
	session := agent.SessionName(task.ID)
	// The deterministic worker branch: claimed when set, else herder/<issue>.
	// The prompt is built before the reuse/launch decision because a reused
	// session on a not-yet-running task is re-seeded with the same contract.
	task.BranchName = branchForTask(*task)
	prompt := agent.BuildPrompt(agent.PromptInput{
		Task: *task, AgentKind: prof.Kind, ProfileName: profileName,
		Repo:             repo,
		RepoInstructions: agent.LoadRepoInstructions(workspace),
		SandboxID:        container, Workspace: workspace,
		PriorAgent: priorAgent,
	})
	if task.AgentSessionID != "" && launcher.IsLive(ctx, task.AgentSessionID) {
		return reuseSession(ctx, launcher, store, task, prompt, map[string]any{
			"session": task.AgentSessionID, "kind": prof.Kind, "profile": profileName,
			"sandbox": container, "reused": true,
		}, ew)
	}
	res, err := launcher.Start(ctx, agent.StartInput{
		Session: session, AgentKind: prof.Kind,
		Workspace: workspace, Container: container, Prompt: prompt,
	})
	if err != nil {
		// Record the binding optimistically so a session that did start
		// (send-side failure) stays attachable and retry re-seeds it;
		// failTaskStart clears it when the session never came up.
		if bindErr := store.SetBinding(task.ID, container, session); bindErr == nil {
			task.AgentSessionID = session
		}
		return failTaskStart(ctx, launcher, store, task, err.Error(), ew)
	}
	// Bind only after the pane exists: a failed Start leaves no session
	// name behind for attach to resolve. A bind failure still fails the
	// task, and the reason names the orphaned session for manual recovery.
	if err := store.SetBinding(task.ID, container, session); err != nil {
		return failTaskStart(ctx, launcher, store, task,
			fmt.Sprintf("session %s started but binding failed: %v", session, err), ew)
	}
	// "starting" is the one normalized state Herder mints itself: Herdr's
	// first detection report lands on the next supervisor poll.
	if _, err := store.RecordAgentState(task.ID, tasks.AgentStarting, "controller", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: record agent state: %v\n", err)
	}
	if err := advanceToRunning(store, task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, "agent.started", map[string]string{
		"session": session, "kind": prof.Kind, "profile": profileName,
		"sandbox": container, "pane": res.PaneID, "workspace_id": res.WorkspaceID,
		"branch": task.BranchName,
	}, ew); err != nil {
		return 1
	}
	fmt.Fprintf(w, "herder: agent %s started for %s in session %s (pane %s)\n",
		prof.Kind, task.ID, session, res.PaneID)
	return 0
}

// reuseSession records a converged relaunch: the stored session still
// answers, so no new pane is opened. A not-yet-running task is re-seeded
// via SendPrompt so the reused pane carries the current goal (crash
// recovery); a RUNNING task's mid-work agent is left alone. A failed send
// fails the task like a failed launch. Then the task just ensures RUNNING.
func reuseSession(ctx context.Context, launcher *agent.Launcher, store *storage.Store, task *tasks.Task, prompt string, payload map[string]any, ew io.Writer) int {
	if err := store.SetBinding(task.ID, sandbox.ContainerName(task.ID), ""); err != nil {
		fmt.Fprintf(ew, "herder: record sandbox binding: %v\n", err)
		return 1
	}
	if task.Status != tasks.Running {
		if err := launcher.SendPrompt(ctx, task.AgentSessionID, prompt); err != nil {
			return failTaskStart(ctx, launcher, store, task, err.Error(), ew)
		}
	}
	if err := advanceToRunning(store, task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, "agent.started", payload, ew); err != nil {
		return 1
	}
	return 0
}

// emitEvent appends one agent lifecycle event with a JSON payload.
// Purpose: launch, reuse, and failure report their event types from one
// place so the marshal and error naming cannot drift between paths.
// Returns the append error after naming it on ew.
func emitEvent(store *storage.Store, taskID, eventType string, payload any, ew io.Writer) error {
	raw, _ := json.Marshal(payload)
	if _, err := store.AppendEvent(taskID, eventType, "controller", "cli", string(raw)); err != nil {
		fmt.Fprintf(ew, "herder: record agent event: %v\n", err)
		return err
	}
	return nil
}

// failTaskStart moves the task to FAILED and records agent.start_failed
// with the reason, so an unlaunchable task is a clear event, not a hang.
// A dead session's stale binding is cleared first so task attach reports
// no session instead of execing a dead pane; a live session keeps its
// binding so attach can still land on it.
func failTaskStart(ctx context.Context, launcher *agent.Launcher, store *storage.Store, task *tasks.Task, reason string, ew io.Writer) int {
	if task.AgentSessionID != "" && !launcher.IsLive(ctx, task.AgentSessionID) {
		if err := store.ClearSessionBinding(task.ID); err != nil {
			fmt.Fprintf(ew, "herder: clear stale session binding: %v\n", err)
		}
		if _, err := store.RecordAgentState(task.ID, tasks.AgentUnknown, "controller", "cli"); err != nil {
			fmt.Fprintf(ew, "herder: record agent state: %v\n", err)
		}
	}
	if _, err := store.Transition(task.ID, tasks.Failed, "controller", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: fail task %s: %v\n", task.ID, err)
		return 1
	}
	if err := emitEvent(store, task.ID, "agent.start_failed", map[string]string{"reason": reason}, ew); err != nil {
		return 1
	}
	fmt.Fprintf(ew, "herder: task %s failed: %s\n", task.ID, reason)
	return 1
}

// taskAttach drops the human into the real running agent behind the task.
// Purpose: glass-box attach (SPEC section 21) — resolve the durable session
// link and hand stdio to Herdr; detaching leaves the agent running.
func taskAttach(store *storage.Store, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task attach <id>\n")
		return 2
	}
	task, err := store.GetTask(args[0])
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", args[0])
		return 1
	}
	if task.AgentSessionID == "" {
		fmt.Fprintf(ew, "herder: task %s has no agent session (run `herder task start %s` first)\n",
			task.ID, task.ID)
		return 1
	}
	return execAttach(agent.AttachArgv(task.AgentSessionID))
}
