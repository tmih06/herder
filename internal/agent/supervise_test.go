package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// scriptedHerdr fakes the Herdr CLI for supervision tests: `agent get`
// answers from the statuses map (gone sessions exit 1), `agent read`
// serves a fixed tail, and `pane close`/`notification show` record calls.
type scriptedHerdr struct {
	statuses map[string]string
	gone     map[string]bool
	readText string
	calls    []string
}

func (s *scriptedHerdr) run(ctx context.Context, name string, args ...string) (RunResult, error) {
	s.calls = append(s.calls, strings.Join(args, " "))
	switch strings.Join(args[:2], " ") {
	case "agent get":
		session := args[2]
		if s.gone[session] {
			return RunResult{ExitCode: 1, Stderr: "agent_not_found"}, nil
		}
		status := s.statuses[session]
		if status == "" {
			status = "working"
		}
		return RunResult{Stdout: fmt.Sprintf(
			`{"result":{"agent":{"agent":"codex","agent_status":%q,"pane_id":"w9:p-%s","workspace_id":"w9"}}}`,
			status, session)}, nil
	case "agent read":
		// herdr 0.9.x prints the pane tail directly, no JSON envelope.
		return RunResult{Stdout: s.readText + "\n"}, nil
	case "pane process-info":
		// The shim stays foreground while the session lives; a gone
		// session's pane fell back to its shell.
		pane := args[3]
		sess := strings.TrimPrefix(pane, "w9:p-")
		if s.gone[sess] {
			return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"fish"}]}}}`}, nil
		}
		return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
	}
	return RunResult{}, nil
}

// openSupervisedStore opens a temp store and returns it with a scripted
// supervisor wired to it.
func openSupervisedStore(t *testing.T) (*storage.Store, *scriptedHerdr, *Supervisor) {
	t.Helper()
	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake := &scriptedHerdr{statuses: map[string]string{}, gone: map[string]bool{}}
	sup := &Supervisor{Store: store, Launcher: &Launcher{Runner: fake.run}}
	return store, fake, sup
}

// runningTask creates a task walked to RUNNING with a bound session.
func runningTask(t *testing.T, store *storage.Store, ref string) tasks.Task {
	t.Helper()
	task, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: ref, Repository: "acme/web",
		AgentProfile: "codex-default", ActorType: "controller", ActorID: "test",
	})
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	for _, next := range []tasks.State{tasks.Eligible, tasks.Claimed, tasks.Queued, tasks.Provisioning, tasks.Running} {
		if _, err := store.Transition(task.ID, next, "controller", "test"); err != nil {
			t.Fatalf("Transition %s = %v", next, err)
		}
	}
	session := SessionName(task.ID)
	if err := store.SetBinding(task.ID, "herder-"+task.ID, session); err != nil {
		t.Fatalf("SetBinding = %v", err)
	}
	task.AgentSessionID = session
	return task
}

// eventTypes returns the task's event type sequence for assertions.
func eventTypes(t *testing.T, store *storage.Store, taskID string) []string {
	t.Helper()
	events, err := store.ListEvents(taskID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

func hasEvent(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

// TestPollRecordsNormalizedState proves the acceptance core: a live Herdr
// report lands on the owning task as a normalized agent state with an
// agent.state_changed event.
func TestPollRecordsNormalizedState(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#1")
	fake.statuses[task.AgentSessionID] = "working"

	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentState != tasks.AgentWorking {
		t.Errorf("agent state = %q, want working", got.AgentState)
	}
	if !hasEvent(eventTypes(t, store, task.ID), tasks.EventAgentStateChanged) {
		t.Error("agent.state_changed event missing")
	}
}

// TestPollBlockedNotifies proves a blocked agent moves the task to
// BLOCKED, records the reason, and raises the human-visible notification.
func TestPollBlockedNotifies(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#2")
	fake.statuses[task.AgentSessionID] = "blocked"
	fake.readText = "editing files\nMay I delete the old migration?"

	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Blocked {
		t.Errorf("task status = %s, want BLOCKED", got.Status)
	}
	if got.AgentState != tasks.AgentBlocked {
		t.Errorf("agent state = %q, want blocked", got.AgentState)
	}
	types := eventTypes(t, store, task.ID)
	if !hasEvent(types, tasks.EventAgentBlocked) {
		t.Error("agent.blocked event missing")
	}
	var notified bool
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "notification show") && strings.Contains(call, "blocked") {
			notified = true
		}
	}
	if !notified {
		t.Errorf("blocked agent must raise a notification, calls: %v", fake.calls)
	}
}

// TestPollUnblocksOnRecovery proves a BLOCKED task returns to RUNNING
// once the agent reports anything else — the human answer worked.
func TestPollUnblocksOnRecovery(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#3")
	fake.statuses[task.AgentSessionID] = "blocked"
	sup.PollOnce(context.Background())

	fake.statuses[task.AgentSessionID] = "working"
	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Running {
		t.Errorf("task status = %s, want RUNNING after unblock", got.Status)
	}
	if got.AgentState != tasks.AgentWorking {
		t.Errorf("agent state = %q, want working", got.AgentState)
	}
}

// TestPollDoneNotifiesWithoutBlocking proves a finished agent surfaces
// for attention while the task keeps its lifecycle state for the
// validation slice to pick up.
func TestPollDoneNotifiesWithoutBlocking(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#4")
	fake.statuses[task.AgentSessionID] = "done"

	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Running {
		t.Errorf("done agent must not move the task, got %s", got.Status)
	}
	if got.AgentState != tasks.AgentDone {
		t.Errorf("agent state = %q, want done", got.AgentState)
	}
	var notified bool
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "notification show") && strings.Contains(call, "done") {
			notified = true
		}
	}
	if !notified {
		t.Errorf("done agent must raise a notification, calls: %v", fake.calls)
	}
}

// TestPollReblocksAfterAnswer proves the unblock loop is honest: a human
// answer moves the task to RUNNING, but an agent that still reports
// blocked re-blocks it on the next poll instead of silently staying up.
func TestPollReblocksAfterAnswer(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#8")
	fake.statuses[task.AgentSessionID] = "blocked"
	sup.PollOnce(context.Background())

	// The human answers: tell-style unblock back to RUNNING while the
	// agent's report has not changed yet.
	if _, err := store.Transition(task.ID, tasks.Running, "human", "test"); err != nil {
		t.Fatal(err)
	}
	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Blocked {
		t.Errorf("still-blocked agent must re-block the task, got %s", got.Status)
	}
}

// TestPollDoneOnBlockedUnblocksAndNotifies proves a done report on a
// BLOCKED task both returns it to RUNNING and raises the notification —
// the unblock must not swallow the done signal.
func TestPollDoneOnBlockedUnblocksAndNotifies(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#9")
	fake.statuses[task.AgentSessionID] = "blocked"
	sup.PollOnce(context.Background())

	fake.statuses[task.AgentSessionID] = "done"
	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Running {
		t.Errorf("done on blocked task = %s, want RUNNING", got.Status)
	}
	var notified bool
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "notification show") && strings.Contains(call, "done") {
			notified = true
		}
	}
	if !notified {
		t.Errorf("done on a blocked task must still notify, calls: %v", fake.calls)
	}
}

// TestPollExitedFiresOnce proves a dead session notifies once, not every
// tick: the cleared binding removes the task from the poll set.
func TestPollExitedFiresOnce(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#10")
	fake.gone[task.AgentSessionID] = true

	sup.PollOnce(context.Background())
	sup.PollOnce(context.Background())

	var exited, notified int
	for _, e := range eventTypes(t, store, task.ID) {
		if e == tasks.EventAgentExited {
			exited++
		}
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "notification show") {
			notified++
		}
	}
	if exited != 1 || notified != 1 {
		t.Errorf("dead session must fire once: exited=%d notifications=%d", exited, notified)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSessionID != "" {
		t.Errorf("dead binding should clear, got %q", got.AgentSessionID)
	}
}

// TestGetHerdrOutageIsNotGone proves a Herdr failure that is not
// agent_not_found stays a plain error: an unreachable daemon must not
// masquerade as a dead session.
func TestGetHerdrOutageIsNotGone(t *testing.T) {
	l := &Launcher{Runner: func(ctx context.Context, name string, args ...string) (RunResult, error) {
		return RunResult{ExitCode: 1, Stderr: "dial unix /run/herdr.sock: connect: no such file"}, nil
	}}
	_, err := l.Get(context.Background(), "herder-task_x")
	if err == nil || errors.Is(err, ErrSessionGone) {
		t.Fatalf("Herdr outage = %v, want a plain error not ErrSessionGone", err)
	}
}

// TestPollExitedSession proves a dead pane records unknown plus
// agent.exited and notifies, without failing the task outright.
func TestPollExitedSession(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	task := runningTask(t, store, "acme/web#5")
	fake.gone[task.AgentSessionID] = true

	sup.PollOnce(context.Background())

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentState != tasks.AgentUnknown {
		t.Errorf("agent state = %q, want unknown", got.AgentState)
	}
	if got.Status != tasks.Running {
		t.Errorf("exited session must not move the task, got %s", got.Status)
	}
	if !hasEvent(eventTypes(t, store, task.ID), tasks.EventAgentExited) {
		t.Error("agent.exited event missing")
	}
}

// TestPollSkipsPausedAndUnbound proves frozen tasks and tasks without a
// session are never polled: no Herdr calls, no state churn.
func TestPollSkipsPausedAndUnbound(t *testing.T) {
	store, fake, sup := openSupervisedStore(t)
	paused := runningTask(t, store, "acme/web#6")
	if _, err := store.Transition(paused.ID, tasks.Paused, "human", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7", Repository: "acme/web",
		AgentProfile: "codex-default", ActorType: "controller", ActorID: "test",
	}); err != nil {
		t.Fatal(err)
	}

	sup.PollOnce(context.Background())

	for _, call := range fake.calls {
		if strings.HasPrefix(call, "agent get") {
			t.Errorf("paused/unbound tasks must not be polled, ran %q", call)
		}
	}
}

// TestGetParsesAgentInfo proves `agent get` JSON lands as AgentInfo with
// the pane identity needed to stop the worker.
func TestGetParsesAgentInfo(t *testing.T) {
	fake := &scriptedHerdr{statuses: map[string]string{"herder-task_x": "blocked"}, gone: map[string]bool{}}
	l := &Launcher{Runner: fake.run}
	info, err := l.Get(context.Background(), "herder-task_x")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if info.Status != "blocked" || info.PaneID != "w9:p-herder-task_x" {
		t.Errorf("Get = %+v, want blocked status with pane id", info)
	}
}

// TestGetGoneSession proves a missing session reports ErrSessionGone so
// callers can tell "dead" from "Herdr unreachable".
func TestGetGoneSession(t *testing.T) {
	fake := &scriptedHerdr{gone: map[string]bool{"herder-task_x": true}}
	l := &Launcher{Runner: fake.run}
	_, err := l.Get(context.Background(), "herder-task_x")
	if err == nil || !errors.Is(err, ErrSessionGone) {
		t.Fatalf("Get on dead session = %v, want ErrSessionGone", err)
	}
}

// TestReadReturnsTail proves `agent read` raw text lands as plain text.
func TestReadReturnsTail(t *testing.T) {
	fake := &scriptedHerdr{readText: "line one\nline two"}
	l := &Launcher{Runner: fake.run}
	text, err := l.Read(context.Background(), "herder-task_x", 50)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if text != "line one\nline two" {
		t.Errorf("Read = %q", text)
	}
}

// TestStopClosesPane proves stop resolves the session's pane and closes
// it — the agent dies with the pane while the sandbox survives.
func TestStopClosesPane(t *testing.T) {
	fake := &scriptedHerdr{gone: map[string]bool{}}
	l := &Launcher{Runner: fake.run}
	if err := l.Stop(context.Background(), "herder-task_x", "herder-task_x"); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	var closed bool
	for _, call := range fake.calls {
		if call == "pane close w9:p-herder-task_x" {
			closed = true
		}
	}
	if !closed {
		t.Errorf("stop must close the session's pane, calls: %v", fake.calls)
	}
}

// TestStopKillsContainerAgent proves stop kills the docker exec'd agent
// inside the container: `pane close` only drops the host exec client and
// the agent would keep running deaf inside (verified against herdr
// 0.9.1). The kill is host-side via docker top + kill.
func TestStopKillsContainerAgent(t *testing.T) {
	fake := &scriptedHerdr{gone: map[string]bool{}}
	var calls []string
	l := &Launcher{Runner: func(ctx context.Context, name string, args ...string) (RunResult, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "docker" && len(args) > 0 && args[0] == "top" {
			return RunResult{Stdout: "PID   COMMAND   ARGS\n1     sleep     sleep infinity\n4242  sh        /bin/sh /usr/local/bin/codex\n"}, nil
		}
		return fake.run(ctx, name, args...)
	}}
	if err := l.Stop(context.Background(), "herder-task_x", "herder-task_x"); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	var killed bool
	for _, call := range calls {
		if call == "kill -9 4242" {
			killed = true
		}
	}
	if !killed {
		t.Errorf("stop must kill the in-container agent pid, calls: %v", calls)
	}
}

// TestStopGoneSessionIsNoop proves stopping a dead session is a no-op:
// the desired end state already holds.
func TestStopGoneSessionIsNoop(t *testing.T) {
	fake := &scriptedHerdr{gone: map[string]bool{"herder-task_x": true}}
	l := &Launcher{Runner: fake.run}
	if err := l.Stop(context.Background(), "herder-task_x", "herder-task_x"); err != nil {
		t.Fatalf("Stop on dead session = %v, want nil", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "pane close") {
			t.Errorf("dead session must not reach pane close, ran %q", call)
		}
	}
}

// TestNormalizeState maps Herdr's vocabulary onto Herder's and folds
// anything foreign into unknown.
func TestNormalizeState(t *testing.T) {
	for reported, want := range map[string]string{
		"starting": tasks.AgentStarting,
		"working":  tasks.AgentWorking,
		"idle":     tasks.AgentIdle,
		"blocked":  tasks.AgentBlocked,
		"done":     tasks.AgentDone,
		"unknown":  tasks.AgentUnknown,
		"Working":  tasks.AgentWorking,
		"":         tasks.AgentUnknown,
		"bored":    tasks.AgentUnknown,
	} {
		if got := NormalizeState(reported); got != want {
			t.Errorf("NormalizeState(%q) = %q, want %q", reported, got, want)
		}
	}
}
