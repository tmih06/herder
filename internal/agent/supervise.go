// Supervision loop (issue #5; SPEC sections 19-22, 61): the daemon polls
// Herdr for every live worker session and folds the reports into durable
// task state — normalized agent states with event history, BLOCKED tasks
// with human-visible notifications, and automatic unblock when the agent
// moves again.
//
// Why: a running agent is supervised, not fire-and-forget; the terminal
// is never the sole state database, so every observed change lands on the
// owning task where `task inspect` and the status view can surface it.
// Approach: Stage 1 CLI orchestration — `agent get` per bound session on
// a ticker (the socket event subscription is the Stage 2 upgrade). All
// mutations go through storage so poll results are transactional with
// their events.
// Inputs: open store, launcher, poll interval. Flow: Run -> PollOnce per
// tick -> per task Get -> RecordAgentState -> blocked/unblocked/done/
// exited reactions. Returns: nothing; failures are logged, never fatal.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// DefaultPollInterval spaces Herdr probes: `agent get` is a local socket
// call, so two seconds keeps blocked agents human-visible promptly
// without measurable load.
const DefaultPollInterval = 2 * time.Second

// blockedTailLines bounds the recent-output snippet captured as the
// blocked reason: enough for the agent's question, not its scrollback.
const blockedTailLines = 10

// blockedReasonCap bounds the reason text stored in events and shown in
// notifications.
const blockedReasonCap = 300

// Event types the supervisor mints on top of the tasks package's set.
const (
	// EventAgentBlocked records a blocked worker with its reason.
	EventAgentBlocked = "agent.blocked"
	// EventAgentExited records a bound session that stopped answering.
	EventAgentExited = "agent.exited"
)

// Supervisor watches bound agent sessions and reflects them onto tasks.
type Supervisor struct {
	// Store is the durable task repository the loop writes through.
	Store *storage.Store
	// Launcher talks to Herdr; nil Runner inside means the real CLI.
	Launcher *Launcher
	// Interval spaces polls; zero means DefaultPollInterval.
	Interval time.Duration
	// Logf receives non-fatal poll errors; nil discards them.
	Logf func(format string, args ...any)
}

// supervised reports whether a task status is worth polling: the states
// where a live session may exist and its reports still matter. Paused is
// excluded — a frozen container cannot report, and its stored agent state
// is the honest pre-pause one.
func supervised(status tasks.State) bool {
	switch status {
	case tasks.Provisioning, tasks.Running, tasks.Blocked, tasks.WaitingForHuman:
		return true
	}
	return false
}

// Run polls until ctx is done: one immediate pass so a daemon restart
// reconciles state right away, then one per Interval.
func (s *Supervisor) Run(ctx context.Context) {
	s.PollOnce(ctx)
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.PollOnce(ctx)
		}
	}
}

// PollOnce runs one supervision pass over every supervised task with a
// bound session. Each task is independent: one failure logs and moves on
// so a single wedged session cannot blind the whole fleet.
func (s *Supervisor) PollOnce(ctx context.Context) {
	found, err := s.Store.ListTasks()
	if err != nil {
		s.logf("herder: supervise: list tasks: %v", err)
		return
	}
	for i := range found {
		task := &found[i]
		if !supervised(task.Status) || task.AgentSessionID == "" {
			continue
		}
		s.pollTask(ctx, task)
	}
}

// pollTask folds one session's Herdr report into the task: record the
// normalized state, then react — blocked raises the notification and
// moves the task to BLOCKED, recovery from blocked returns it to RUNNING,
// done and exited notify without touching the task's lifecycle state.
func (s *Supervisor) pollTask(ctx context.Context, task *tasks.Task) {
	session := task.AgentSessionID
	info, err := s.Launcher.Get(ctx, session)
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			s.onExited(ctx, task)
			return
		}
		s.logf("herder: supervise: %s: %v", task.ID, err)
		return
	}
	state := NormalizeState(info.Status)
	changed, err := s.Store.RecordAgentState(task.ID, state, "agent", session)
	if err != nil {
		s.logf("herder: supervise: %s: record agent state: %v", task.ID, err)
		return
	}
	if !changed {
		return
	}
	switch {
	case state == tasks.AgentBlocked && task.Status == tasks.Running:
		s.onBlocked(ctx, task, session)
	case state != tasks.AgentBlocked && task.Status == tasks.Blocked:
		s.onUnblocked(task)
	case state == tasks.AgentDone:
		s.notify(ctx, "Herder: agent done",
			fmt.Sprintf("task %s (%s) reports done; validation is the next slice", task.ID, task.SourceRef))
	}
}

// onBlocked moves a RUNNING task to BLOCKED and raises the human-visible
// notification carrying the task and the agent's own reason — the last
// non-empty recent output, or a plain marker when the pane won't read.
func (s *Supervisor) onBlocked(ctx context.Context, task *tasks.Task, session string) {
	reason := "agent reports blocked"
	if tail, err := s.Launcher.Read(ctx, session, blockedTailLines); err == nil {
		if line := lastLine(tail); line != "" {
			reason = truncate(line, blockedReasonCap)
		}
	}
	if _, err := s.Store.Transition(task.ID, tasks.Blocked, "agent", session); err != nil {
		s.logf("herder: supervise: %s: block task: %v", task.ID, err)
		return
	}
	s.appendEvent(task.ID, EventAgentBlocked, session,
		fmt.Sprintf(`{"session":%q,"reason":%q}`, session, reason))
	s.notify(ctx, "Herder: agent blocked",
		fmt.Sprintf("task %s (%s): %s", task.ID, task.SourceRef, reason))
}

// onUnblocked returns a BLOCKED task to RUNNING once the agent reports
// anything else: the human answer (or the agent's own recovery) worked.
func (s *Supervisor) onUnblocked(task *tasks.Task) {
	if _, err := s.Store.Transition(task.ID, tasks.Running, "agent", task.AgentSessionID); err != nil {
		s.logf("herder: supervise: %s: unblock task: %v", task.ID, err)
	}
}

// onExited records a bound session that no longer answers: agent state
// unknown plus agent.exited, and a notification so the human can retry or
// stop the task. The task keeps its lifecycle state — a dead pane is a
// fact about the worker, not a verdict on the work.
func (s *Supervisor) onExited(ctx context.Context, task *tasks.Task) {
	session := task.AgentSessionID
	if _, err := s.Store.RecordAgentState(task.ID, tasks.AgentUnknown, "agent", session); err != nil {
		s.logf("herder: supervise: %s: record agent state: %v", task.ID, err)
	}
	s.appendEvent(task.ID, EventAgentExited, session,
		fmt.Sprintf(`{"session":%q}`, session))
	s.notify(ctx, "Herder: agent session ended",
		fmt.Sprintf("task %s (%s): session %s no longer exists", task.ID, task.SourceRef, session))
}

// appendEvent records one supervisor-minted event; failures only log
// because the state change they annotate already committed.
func (s *Supervisor) appendEvent(taskID, eventType, session, payload string) {
	if _, err := s.Store.AppendEvent(taskID, eventType, "agent", session, payload); err != nil {
		s.logf("herder: supervise: %s: record %s: %v", taskID, eventType, err)
	}
}

// notify raises the human-visible notification; a failed notification
// never gates the state change it announces.
func (s *Supervisor) notify(ctx context.Context, title, body string) {
	if err := s.Launcher.Notify(ctx, title, body); err != nil {
		s.logf("herder: supervise: notify: %v", err)
	}
}

// logf reports through the configured logger, discarding when unset.
func (s *Supervisor) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// lastLine returns the last non-empty line of a terminal tail.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// truncate caps s at n bytes with an ellipsis marker.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
