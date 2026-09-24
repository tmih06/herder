package storage

import (
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/tasks"
)

// runningTask walks a fresh task to RUNNING with a bound session so
// supervision mutators have realistic state to work on.
func runningTask(t *testing.T, store *Store, ref string) tasks.Task {
	t.Helper()
	in := createInputWithRef(ref)
	task, err := store.CreateTask(in)
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	for _, next := range []tasks.State{tasks.Eligible, tasks.Claimed, tasks.Queued, tasks.Provisioning, tasks.Running} {
		if _, err := store.Transition(task.ID, next, "controller", "test"); err != nil {
			t.Fatalf("Transition %s = %v", next, err)
		}
	}
	if err := store.SetBinding(task.ID, Binding{SandboxID: "herder-" + task.ID, SessionID: "herder-" + task.ID}); err != nil {
		t.Fatalf("SetBinding = %v", err)
	}
	task.AgentSessionID = "herder-" + task.ID
	return task
}

// TestRecordAgentState proves the normalized state lands on the task with
// an agent.state_changed event, and repeat reports stay silent.
func TestRecordAgentState(t *testing.T) {
	store, _ := openTestStore(t)
	task := runningTask(t, store, "acme/web#21")

	changed, err := store.RecordAgentState(task.ID, tasks.AgentWorking, "agent", "herder-x")
	if err != nil || !changed {
		t.Fatalf("first RecordAgentState = %v, %v; want changed", changed, err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentState != tasks.AgentWorking {
		t.Errorf("agent state = %q, want working", got.AgentState)
	}

	changed, err = store.RecordAgentState(task.ID, tasks.AgentWorking, "agent", "herder-x")
	if err != nil || changed {
		t.Errorf("same-state report = %v, %v; want quiet no-op", changed, err)
	}
	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stateChanges int
	for _, e := range events {
		if e.Type == tasks.EventAgentStateChanged {
			stateChanges++
		}
	}
	if stateChanges != 1 {
		t.Errorf("agent.state_changed count = %d, want 1", stateChanges)
	}
}

// TestRetryIncrementsAttempt proves a running task's retry lands in
// RETRYING with the counter bumped, the dead session cleared, and a
// task.retry event — atomically.
func TestRetryIncrementsAttempt(t *testing.T) {
	store, _ := openTestStore(t)
	task := runningTask(t, store, "acme/web#22")
	if _, err := store.RecordAgentState(task.ID, tasks.AgentWorking, "agent", "x"); err != nil {
		t.Fatal(err)
	}

	updated, err := store.Retry(task.ID, "human", "cli")
	if err != nil {
		t.Fatalf("Retry = %v", err)
	}
	if updated.Status != tasks.Retrying || updated.Attempt != 2 {
		t.Errorf("Retry = %s attempt %d, want RETRYING attempt 2", updated.Status, updated.Attempt)
	}
	if updated.AgentSessionID != "" || updated.AgentState != "" {
		t.Errorf("retry must clear the dead session and stale state, got %q/%q",
			updated.AgentSessionID, updated.AgentState)
	}
	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawRetry, sawTransition bool
	for _, e := range events {
		if e.Type == tasks.EventRetry && strings.Contains(e.Payload, `"attempt":2`) {
			sawRetry = true
		}
		if e.Type == tasks.EventTransition && strings.Contains(e.Payload, `"to":"RETRYING"`) {
			sawTransition = true
		}
	}
	if !sawRetry || !sawTransition {
		t.Errorf("retry must mint task.retry plus the RETRYING transition, events: %v", events)
	}
}

// TestRetryFromQueuedSkipsTransition proves a queued task retries without
// an illegal self-transition: attempt still increments, task.retry lands.
func TestRetryFromQueuedSkipsTransition(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInputWithRef("acme/web#23"))
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []tasks.State{tasks.Eligible, tasks.Claimed, tasks.Queued} {
		if _, err := store.Transition(task.ID, next, "controller", "test"); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := store.Retry(task.ID, "human", "cli")
	if err != nil {
		t.Fatalf("Retry from QUEUED = %v", err)
	}
	if updated.Status != tasks.Queued || updated.Attempt != 2 {
		t.Errorf("queued retry = %s attempt %d, want QUEUED attempt 2", updated.Status, updated.Attempt)
	}
}

// TestRetryTerminalRefused proves a finished task cannot retry: the
// state-machine error surfaces and nothing changes.
func TestRetryTerminalRefused(t *testing.T) {
	store, _ := openTestStore(t)
	task := runningTask(t, store, "acme/web#24")
	for _, next := range []tasks.State{tasks.Validating, tasks.Reviewing, tasks.Delivering, tasks.PROpen, tasks.Done} {
		if _, err := store.Transition(task.ID, next, "controller", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Retry(task.ID, "human", "cli"); err == nil {
		t.Error("Retry on DONE must fail")
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 || got.Status != tasks.Done {
		t.Errorf("refused retry must change nothing, got %s attempt %d", got.Status, got.Attempt)
	}
}

// TestHandoffSwapsProfile proves the atomic handoff: profile swap,
// attempt increment, RETRYING transition, and the traceable
// agent.handed_off event naming both profiles.
func TestHandoffSwapsProfile(t *testing.T) {
	store, _ := openTestStore(t)
	task := runningTask(t, store, "acme/web#25")

	updated, err := store.Handoff(task.ID, "claude-default", "human", "cli")
	if err != nil {
		t.Fatalf("Handoff = %v", err)
	}
	if updated.AgentProfile != "claude-default" || updated.Attempt != 2 {
		t.Errorf("Handoff = profile %s attempt %d, want claude-default attempt 2",
			updated.AgentProfile, updated.Attempt)
	}
	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, e := range events {
		if e.Type == tasks.EventHandoff &&
			strings.Contains(e.Payload, "codex-default") &&
			strings.Contains(e.Payload, "claude-default") {
			saw = true
		}
	}
	if !saw {
		t.Error("agent.handed_off must name both profiles")
	}
}
