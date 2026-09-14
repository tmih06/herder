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
// gate -> resolve profile -> sandbox ready -> reuse live session or launch
// -> bind -> RUNNING -> agent.started event. Unknown task profiles or agent
// kinds fail the task with agent.start_failed instead of hanging; an
// unknown --agent override is an operator typo and leaves the task alone.
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
	provider := sandbox.NewDockerProvider()
	provider.Log = func(format string, args ...any) {
		fmt.Fprintf(ew, format+"\n", args...)
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
	repo, ok := cfg.Repositories[task.Repository]
	if !ok {
		return failTaskStart(store, &task, fmt.Sprintf("repository %q not in config", task.Repository), ew)
	}
	// One policy owns profile selection (agent.ResolveProfile): explicit
	// override, then the task's claimed profile, then the repo default. An
	// unknown override is an operator typo and leaves the task alone; an
	// unresolvable task profile fails the task with a clear event.
	profileName, prof, ok := agent.ResolveProfile(cfg, task.Repository, task.AgentProfile, *override)
	if !ok {
		if *override != "" {
			fmt.Fprintf(ew, "herder: agent profile %q unknown\n", *override)
			return 1
		}
		return failTaskStart(store, &task,
			fmt.Sprintf("agent profile %q unknown and no usable default", task.AgentProfile), ew)
	}
	if _, ok := agent.CommandForKind(prof.Kind); !ok {
		return failTaskStart(store, &task, fmt.Sprintf("unknown agent kind %q", prof.Kind), ew)
	}
	container := sandbox.ContainerName(task.ID)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	if _, err := provider.Inspect(ctx, container); err != nil {
		fmt.Fprintf(ew, "herder: sandbox %s not ready: run `herder sandbox provision %s` first (%v)\n",
			container, task.ID, err)
		return 1
	}
	workspace := sandbox.WorkspacePath(sandboxRoot(cfg.Database.Path), task.ID)
	session := agent.SessionName(task.ID)
	launcher := &agent.Launcher{}
	if task.AgentSessionID != "" && launcher.IsLive(ctx, task.AgentSessionID) {
		return reuseSession(store, &task, container, session, prof.Kind, profileName, ew)
	}
	if err := store.SetBinding(task.ID, container, session); err != nil {
		fmt.Fprintf(ew, "herder: record session binding: %v\n", err)
		return 1
	}
	// The deterministic worker branch: claimed when set, else herder/<issue>.
	task.BranchName = branchForTask(task)
	prompt := agent.BuildPrompt(agent.PromptInput{
		Task: task, AgentKind: prof.Kind, ProfileName: profileName,
		Repository: task.Repository, Repo: repo,
		RepoInstructions: agent.LoadRepoInstructions(workspace),
		SandboxID:        container, Workspace: workspace,
	})
	res, err := launcher.Start(ctx, agent.StartInput{
		Session: session, AgentKind: prof.Kind,
		Workspace: workspace, Container: container, Prompt: prompt,
	})
	if err != nil {
		return failTaskStart(store, &task, err.Error(), ew)
	}
	advanceToRunning(store, &task, ew)
	if err := emitStarted(store, task.ID, map[string]string{
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
// answers, so no new pane is opened and the task just ensures RUNNING.
func reuseSession(store *storage.Store, task *tasks.Task, container, session, kind, profile string, ew io.Writer) int {
	if err := store.SetBinding(task.ID, container, ""); err != nil {
		fmt.Fprintf(ew, "herder: record sandbox binding: %v\n", err)
		return 1
	}
	advanceToRunning(store, task, ew)
	if err := emitStarted(store, task.ID, map[string]any{
		"session": session, "kind": kind, "profile": profile,
		"sandbox": container, "reused": true,
	}, ew); err != nil {
		return 1
	}
	return 0
}

// emitStarted appends one agent.started event with a JSON payload.
// Purpose: launch and reuse report the same event type from one place so
// the payload shape cannot drift between the two paths. Returns the append
// error after naming it on ew.
func emitStarted(store *storage.Store, taskID string, payload any, ew io.Writer) error {
	raw, _ := json.Marshal(payload)
	if _, err := store.AppendEvent(taskID, "agent.started", "controller", "cli", string(raw)); err != nil {
		fmt.Fprintf(ew, "herder: record agent event: %v\n", err)
		return err
	}
	return nil
}

// failTaskStart moves the task to FAILED and records agent.start_failed
// with the reason, so an unlaunchable task is a clear event, not a hang.
func failTaskStart(store *storage.Store, task *tasks.Task, reason string, ew io.Writer) int {
	if _, err := store.Transition(task.ID, tasks.Failed, "controller", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: fail task %s: %v\n", task.ID, err)
		return 1
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	if _, err := store.AppendEvent(task.ID, "agent.start_failed", "controller", "cli", string(payload)); err != nil {
		fmt.Fprintf(ew, "herder: record agent event: %v\n", err)
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
