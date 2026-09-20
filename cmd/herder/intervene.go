// Human intervention commands (issue #5; SPEC sections 21-22): tell,
// logs, pause, resume, stop, retry, and handoff keep a running agent
// interruptible without destroying the job.
//
// Why: autonomy stays interruptible from first prompt through completion
// — a human can talk to, redirect, freeze, end, restart, or re-staff the
// worker while the durable task and its sandbox survive every verb.
// Approach: each command resolves the task's durable session/sandbox
// link, drives Herdr or Docker through the same seams launch uses, then
// records the outcome as task state plus structured events.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/textutil"
)

// interveneTimeout bounds one intervention: Herdr and Docker calls are
// local and must fail fast instead of hanging the operator's terminal.
const interveneTimeout = 2 * time.Minute

// logLines is the default `task logs` tail: recent agent output, not the
// whole scrollback.
const logLines = 200

// promptCap bounds the message text stored in agent.prompted events: the
// full text reaches the agent, the durable event keeps the head.
const promptCap = 4096

// taskTell delivers a human message to the live agent behind a task
// (SPEC section 22: `herder task tell`). The send is a local Herdr call,
// so the message lands promptly. A BLOCKED task returns to RUNNING on a
// successful send: the human's answer is the unblock, and the supervisor
// re-blocks if the agent still reports blocked.
func taskTell(store *storage.Store, args []string, w, ew io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(ew, "herder: usage: herder task tell <id> <message...>\n")
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
	message := strings.Join(args[1:], " ")
	if message == "" {
		fmt.Fprintf(ew, "herder: task tell needs a message\n")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), interveneTimeout)
	defer cancel()
	launcher := &agent.Launcher{}
	if err := launcher.SendPrompt(ctx, task.AgentSessionID, message); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if err := emitEvent(store, task.ID, tasks.EventAgentPrompted, map[string]string{
		"session": task.AgentSessionID, "message": textutil.Truncate(message, promptCap),
	}, ew); err != nil {
		return 1
	}
	if task.Status == tasks.Blocked {
		if _, err := store.Transition(task.ID, tasks.Running, "human", "cli"); err != nil {
			fmt.Fprintf(ew, "herder: unblock task %s: %v\n", task.ID, err)
			return 1
		}
	}
	fmt.Fprintf(w, "herder: sent to %s (session %s)\n", task.ID, task.AgentSessionID)
	return 0
}

// taskLogs prints the agent's recent terminal output through Herdr
// (SPEC section 22: `herder task logs`). The durable event history stays
// on `task inspect`; logs are the live pane's tail.
func taskLogs(store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(ew)
	lines := fs.Int("lines", logLines, "recent lines to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task logs [--lines N] <id>\n")
		return 2
	}
	task, err := store.GetTask(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", fs.Arg(0))
		return 1
	}
	if task.AgentSessionID == "" {
		fmt.Fprintf(ew, "herder: task %s has no agent session; `herder task inspect %s` shows event history\n",
			task.ID, task.ID)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), interveneTimeout)
	defer cancel()
	text, err := (&agent.Launcher{}).Read(ctx, task.AgentSessionID, *lines)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprint(w, text)
	if text != "" && text[len(text)-1] != '\n' {
		fmt.Fprintln(w)
	}
	return 0
}

// taskPause freezes task progress without killing the session (SPEC
// section 22): the container's processes stop on the cgroup freezer while
// the Herdr pane and the agent stay alive for resume. Only RUNNING tasks
// pause — QUEUED has nothing to freeze and BLOCKED is already halted.
// The container must actually exist: claiming PAUSED on a vanished
// sandbox would lie about the freeze. The task lands PAUSED before the
// freeze so a reconcile tick between the two cannot strand the
// transition; a failed freeze rolls the task back to RUNNING so the
// still-running container never escapes supervision while the task
// claims PAUSED.
func taskPause(store *storage.Store, args []string, w, ew io.Writer) int {
	task, code := oneTask(store, args, "pause", ew)
	if code != 0 {
		return code
	}
	if task.Status != tasks.Running {
		fmt.Fprintf(ew, "herder: task %s in %s cannot pause (want RUNNING)\n", task.ID, task.Status)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), interveneTimeout)
	defer cancel()
	provider := newProvider(ew)
	container := sandbox.ContainerName(task.ID)
	if _, err := provider.Inspect(ctx, container); err != nil {
		fmt.Fprintf(ew, "herder: cannot pause %s: %v\n", task.ID, err)
		return 1
	}
	if _, err := store.Transition(task.ID, tasks.Paused, "human", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: pause task %s: %v\n", task.ID, err)
		return 1
	}
	if err := provider.Pause(ctx, container); err != nil {
		// The task landed PAUSED but the container kept running; roll
		// back to RUNNING so the worker stays supervised instead of
		// escaping the timeout behind a PAUSED label.
		if _, rerr := store.Transition(task.ID, tasks.Running, "human", "cli"); rerr != nil {
			fmt.Fprintf(ew, "herder: revert pause on task %s: %v\n", task.ID, rerr)
		}
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: task %s paused (session %s kept alive)\n", task.ID, task.AgentSessionID)
	return 0
}

// taskResume continues a paused task: the container thaws and the task
// returns to RUNNING so the supervisor picks the session back up.
// EnsureRunning converges instead of a bare unpause so a PAUSED task on
// an already-running container — a failed pause rollback or external
// docker drift — still resumes. The thaw happens before the transition
// so a failed converge leaves the task honestly PAUSED.
func taskResume(store *storage.Store, args []string, w, ew io.Writer) int {
	task, code := oneTask(store, args, "resume", ew)
	if code != 0 {
		return code
	}
	if task.Status != tasks.Paused {
		fmt.Fprintf(ew, "herder: task %s in %s cannot resume (want PAUSED)\n", task.ID, task.Status)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), interveneTimeout)
	defer cancel()
	provider := newProvider(ew)
	if err := provider.EnsureRunning(ctx, sandbox.ContainerName(task.ID)); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if _, err := store.Transition(task.ID, tasks.Running, "human", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: resume task %s: %v\n", task.ID, err)
		return 1
	}
	fmt.Fprintf(w, "herder: task %s resumed\n", task.ID)
	return 0
}

// taskStop ends the agent session and cancels the task (SPEC section 22):
// the task lands in CANCELLED first so a reconcile tick mid-teardown
// cannot strand the transition, then the Herdr pane closes and the
// binding clears so attach cannot resolve a dead session. A paused
// container thaws first so the agent process actually dies with the pane
// instead of staying frozen inside the sandbox — StopWorker reads the
// pre-transition task.Status for that, so it stays Paused here. A task
// already CANCELLED with a live binding means an earlier stop died
// mid-teardown, so the verb retries the teardown instead of
// early-returning. The sandbox and its workspace stay for inspection —
// `sandbox destroy` owns their removal.
func taskStop(store *storage.Store, args []string, w, ew io.Writer) int {
	task, code := oneTask(store, args, "stop", ew)
	if code != 0 {
		return code
	}
	switch task.Status {
	case tasks.Done, tasks.Failed:
		fmt.Fprintf(ew, "herder: task %s already %s\n", task.ID, task.Status)
		return 1
	case tasks.Cancelled:
		// CANCELLED has no outgoing edges; only a leftover binding
		// justifies continuing into the teardown below.
		if task.AgentSessionID == "" && task.SandboxID == "" {
			fmt.Fprintf(ew, "herder: task %s already %s\n", task.ID, task.Status)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), interveneTimeout)
	defer cancel()
	if task.Status != tasks.Cancelled {
		if _, err := store.Transition(task.ID, tasks.Cancelled, "human", "cli"); err != nil {
			fmt.Fprintf(ew, "herder: stop task %s: %v\n", task.ID, err)
			return 1
		}
	} else if task.SandboxID != "" {
		// The earlier stop may have died before thawing a frozen
		// container; EnsureRunning is a no-op on a running one, and a
		// missing container already killed the agent inside, so a thaw
		// failure only warns — the pane close below still converges.
		if err := newProvider(ew).EnsureRunning(ctx, sandbox.ContainerName(task.ID)); err != nil {
			fmt.Fprintf(ew, "herder: thaw sandbox: %v\n", err)
		}
	}
	if err := newDispatcher(store, w, ew).StopWorker(ctx, &task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if task.AgentSessionID != "" {
		if err := emitEvent(store, task.ID, tasks.EventAgentStopped, map[string]string{
			"session": task.AgentSessionID,
		}, ew); err != nil {
			return 1
		}
		if err := store.ClearSessionBinding(task.ID); err != nil {
			fmt.Fprintf(ew, "herder: clear session binding: %v\n", err)
		}
		if _, err := store.RecordAgentState(task.ID, tasks.AgentUnknown, "controller", "cli"); err != nil {
			fmt.Fprintf(ew, "herder: record agent state: %v\n", err)
		}
	}
	fmt.Fprintf(w, "herder: task %s stopped\n", task.ID)
	return 0
}

// taskRetry starts a fresh attempt (SPEC section 22): the live session
// closes, the container thaws if paused, the attempt counter increments
// through RETRYING -> QUEUED, and the launch flow runs again on the same
// sandbox and workspace. The dispatch lease is held across the requeue
// so a scheduler tick in the gap cannot claim the task and double-
// dispatch it; Launch's own HoldLease sees the same-owner lease and
// proceeds. A missing container leaves the task QUEUED with a provision
// hint instead of failing it.
func taskRetry(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	task, code := oneTask(store, args, "retry", ew)
	if code != 0 {
		return code
	}
	if code := guardRestart(&task, "retry", ew); code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	d := newDispatcher(store, w, ew)
	if err := d.StopWorker(ctx, &task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	updated, err := store.Retry(task.ID, "human", "cli")
	if err != nil {
		fmt.Fprintf(ew, "herder: retry task %s: %v\n", task.ID, err)
		return 1
	}
	lctx, release, err := d.HoldLease(ctx, &updated)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if release != nil {
		defer release()
	}
	if code := requeue(store, &updated, ew); code != 0 {
		return code
	}
	fmt.Fprintf(w, "herder: task %s retrying (attempt %d)\n", updated.ID, updated.Attempt)
	if err := d.Launch(lctx, cfg, &updated, "", ""); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	return 0
}

// taskHandoff moves the task to a different agent kind on a fresh attempt
// (SPEC section 22): the old session closes, the profile swap and attempt
// increment commit atomically with an agent.handed_off event, and the new
// agent launches into the same sandbox and workspace — history and work
// preserved, never an untraceable new job. The dispatch lease is held
// across the requeue like taskRetry's so a scheduler tick cannot claim
// the fresh attempt mid-handoff.
func taskHandoff(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	fs.SetOutput(ew)
	profile := fs.String("agent", "", "target agent profile (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *profile == "" || fs.NArg() != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task handoff --agent P <id>\n")
		return 2
	}
	// Validate the target before touching the live worker: an unknown
	// profile is an operator typo, not a reason to kill the session.
	prof, ok := cfg.Agents[*profile]
	if !ok {
		fmt.Fprintf(ew, "herder: agent profile %q unknown (defined: %s)\n",
			*profile, strings.Join(config.AgentNames(cfg), ", "))
		return 1
	}
	if _, ok := agent.CommandForKind(prof.Kind); !ok {
		fmt.Fprintf(ew, "herder: agent profile %q has unknown kind %q\n", *profile, prof.Kind)
		return 1
	}
	task, err := store.GetTask(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", fs.Arg(0))
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	if code := guardRestart(&task, "handoff", ew); code != 0 {
		return code
	}
	d := newDispatcher(store, w, ew)
	if err := d.StopWorker(ctx, &task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	prior := task.AgentProfile
	updated, err := store.Handoff(task.ID, *profile, "human", "cli")
	if err != nil {
		fmt.Fprintf(ew, "herder: handoff task %s: %v\n", task.ID, err)
		return 1
	}
	lctx, release, err := d.HoldLease(ctx, &updated)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	if release != nil {
		defer release()
	}
	if code := requeue(store, &updated, ew); code != 0 {
		return code
	}
	fmt.Fprintf(w, "herder: task %s handed off %s -> %s (attempt %d)\n",
		updated.ID, prior, *profile, updated.Attempt)
	if err := d.Launch(lctx, cfg, &updated, *profile, prior); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	return 0
}

// oneTask resolves the single <id> argument shared by the intervention
// verbs, printing usage or not-found itself. Returns the task and the
// exit code to propagate (0 means proceed).
func oneTask(store *storage.Store, args []string, verb string, ew io.Writer) (tasks.Task, int) {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task %s <id>\n", verb)
		return tasks.Task{}, 2
	}
	task, err := store.GetTask(args[0])
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", args[0])
		return tasks.Task{}, 1
	}
	return task, 0
}

// newProvider builds the Docker provider with operator-facing logging,
// shared by every intervention verb that touches a container.
func newProvider(ew io.Writer) *sandbox.DockerProvider {
	provider := sandbox.NewDockerProvider()
	provider.Log = func(format string, args ...any) { fmt.Fprintf(ew, format+"\n", args...) }
	return provider
}

// guardRestart validates the RETRYING transition shared by retry and
// handoff before either touches the live worker: a refused command must
// leave the running agent alone, not orphan it mid-flight. QUEUED and
// RETRYING tasks pass without validating — they have no live worker to
// protect and the store's Retry/Handoff accepts them as-is.
// Returns the exit code to propagate (0 means proceed).
func guardRestart(task *tasks.Task, verb string, ew io.Writer) int {
	if task.Status == tasks.Queued || task.Status == tasks.Retrying {
		return 0
	}
	if err := tasks.ValidateTransition(task.Status, tasks.Retrying); err != nil {
		fmt.Fprintf(ew, "herder: %s task %s: %v\n", verb, task.ID, err)
		return 1
	}
	return 0
}

// requeue lands a RETRYING task back into QUEUED after Retry or Handoff
// minted the fresh attempt, so the launch flow runs again on the same
// sandbox and workspace. Returns the exit code to propagate.
func requeue(store *storage.Store, task *tasks.Task, ew io.Writer) int {
	if task.Status != tasks.Retrying {
		return 0
	}
	if _, err := store.Transition(task.ID, tasks.Queued, "controller", "cli"); err != nil {
		fmt.Fprintf(ew, "herder: requeue task %s: %v\n", task.ID, err)
		return 1
	}
	task.Status = tasks.Queued
	return 0
}
