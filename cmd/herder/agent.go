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
// gate -> dispatch Launch. Unknown task profiles or agent kinds fail the
// task with agent.start_failed instead of hanging; an unknown --agent
// override is an operator typo and leaves the task alone.
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
	case tasks.Queued, tasks.Provisioning, tasks.Running, tasks.Retrying, tasks.WaitingForHuman:
	default:
		fmt.Fprintf(ew, "herder: task %s in %s cannot start (want QUEUED, PROVISIONING, RUNNING, RETRYING, or WAITING_FOR_HUMAN)\n",
			task.ID, task.Status)
		return 1
	}
	// The bounded ctx covers the whole launch: sandbox inspect plus the
	// Herdr start/send calls are local and must fail fast, never hang.
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	if err := newDispatcher(store, w, ew).Launch(ctx, cfg, &task, *override, ""); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
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
