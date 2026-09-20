package storage

import (
	"fmt"
	"sync"
	"testing"

	"github.com/tmih06/herder/internal/tasks"
)

// claimInput builds a ClaimRequest for one distinct delivery/issue pair.
func claimInput(delivery, ref string) ClaimRequest {
	return ClaimRequest{
		DeliveryID:     delivery,
		SourceProvider: "github",
		SourceRef:      ref,
		Repository:     "acme/web",
		AgentProfile:   "codex-default",
		Priority:       5,
		BranchName:     "herder/7",
		PolicyPayload:  `{"decision":"accepted"}`,
		ActorType:      "controller",
		ActorID:        "webhook",
	}
}

// An eligible delivery must create exactly one QUEUED task with source
// identity, branch, agent profile, and a full event timeline.
func TestClaimAcceptsAndQueues(t *testing.T) {
	store, _ := openTestStore(t)

	out, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	if out.Decision != DecisionAccepted {
		t.Fatalf("decision = %q, want accepted", out.Decision)
	}
	got := out.Task
	if got.Status != tasks.Queued {
		t.Errorf("status = %s, want QUEUED", got.Status)
	}
	if got.SourceProvider != "github" || got.SourceRef != "acme/web#7" {
		t.Errorf("source identity lost: %+v", got)
	}
	if got.BranchName != "herder/7" || got.AgentProfile != "codex-default" {
		t.Errorf("branch/agent not recorded: %+v", got)
	}
	if got.Priority != 5 {
		t.Errorf("priority = %d, want 5", got.Priority)
	}

	events, err := store.ListEvents(got.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	wantTypes := []string{
		tasks.EventCreated,
		tasks.EventTransition, tasks.EventTransition, tasks.EventTransition,
		tasks.EventPolicyDecision,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("want %d events, got %d (%v)", len(wantTypes), len(events), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d = %q, want %q", i, events[i].Type, want)
		}
	}

	del, err := store.GetDelivery("del-1")
	if err != nil {
		t.Fatalf("GetDelivery = %v", err)
	}
	if del.Decision != DecisionAccepted || del.TaskID != got.ID {
		t.Errorf("delivery not linked: %+v", del)
	}
}

// Redelivering the same delivery id must not create a second task and
// must log the deduplication decision on the surviving task.
func TestClaimRedeliveryIsDuplicate(t *testing.T) {
	store, _ := openTestStore(t)

	first, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	second, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("re-Claim = %v", err)
	}
	if second.Decision != DecisionDuplicate {
		t.Fatalf("decision = %q, want duplicate", second.Decision)
	}
	if second.Task.ID != first.Task.ID {
		t.Errorf("duplicate must point at surviving task %s, got %s", first.Task.ID, second.Task.ID)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("redelivery created %d tasks, want 1", len(found))
	}

	events, err := store.ListEvents(first.Task.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	last := events[len(events)-1]
	if last.Type != tasks.EventWebhookDuplicate {
		t.Errorf("last event = %q, want %q", last.Type, tasks.EventWebhookDuplicate)
	}
}

// A new delivery for an already-claimed issue must not create a task.
func TestClaimSameIssueNewDeliveryIsDuplicate(t *testing.T) {
	store, _ := openTestStore(t)

	first, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	second, err := store.Claim(claimInput("del-2", "acme/web#7"))
	if err != nil {
		t.Fatalf("re-Claim = %v", err)
	}
	if second.Decision != DecisionDuplicate {
		t.Fatalf("decision = %q, want duplicate", second.Decision)
	}
	if second.Task.ID != first.Task.ID {
		t.Errorf("duplicate must point at surviving task %s, got %s", first.Task.ID, second.Task.ID)
	}
}

// Concurrent duplicate deliveries must converge on one task with no
// double-claim window.
func TestClaimConcurrentDuplicates(t *testing.T) {
	store, _ := openTestStore(t)

	const racers = 16
	var wg sync.WaitGroup
	outcomes := make([]ClaimOutcome, racers)
	errs := make([]error, racers)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = store.Claim(claimInput("del-race", "acme/web#7"))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: Claim = %v", i, err)
		}
	}
	accepted := 0
	for _, out := range outcomes {
		if out.Decision == DecisionAccepted {
			accepted++
		} else if out.Decision != DecisionDuplicate {
			t.Fatalf("racer decision = %q, want accepted or duplicate", out.Decision)
		}
	}
	if accepted != 1 {
		t.Fatalf("want exactly 1 accepted claim, got %d", accepted)
	}
	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("concurrent duplicates created %d tasks, want 1", len(found))
	}
}

// A burst beyond the worker cap must still queue: concurrency caps live
// in the scheduler (issue #7), so Claim accepts every distinct issue and
// leaves dispatch order to the queue.
func TestClaimBurstBeyondCapStillQueues(t *testing.T) {
	store, _ := openTestStore(t)

	const burst = 6
	for i := range burst {
		ref := fmt.Sprintf("acme/web#%d", i+1)
		out, err := store.Claim(claimInput(fmt.Sprintf("del-%d", i+1), ref))
		if err != nil {
			t.Fatalf("Claim %d = %v", i, err)
		}
		if out.Decision != DecisionAccepted {
			t.Fatalf("claim %d decision = %q, want accepted", i, out.Decision)
		}
		if out.Task.Status != tasks.Queued {
			t.Errorf("claim %d status = %s, want QUEUED", i, out.Task.Status)
		}
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != burst {
		t.Fatalf("burst created %d tasks, want %d", len(found), burst)
	}
	for _, task := range found {
		if task.Status != tasks.Queued {
			t.Errorf("task %s status = %s, want QUEUED", task.ID, task.Status)
		}
	}
}

// Static policy denials (bad repo, wrong label) record a stable delivery
// and stay stable on redelivery.
func TestRecordDeniedAndRedelivery(t *testing.T) {
	store, _ := openTestStore(t)

	denied, created, err := store.RecordDenied("del-x", "github", "evil/repo#1", "evil/repo", `repository "evil/repo" is not configured`)
	if err != nil {
		t.Fatalf("RecordDenied = %v", err)
	}
	if !created {
		t.Fatalf("RecordDenied created = false, want true")
	}
	if denied.Decision != DecisionPolicyDenied {
		t.Fatalf("decision = %q, want policy_denied", denied.Decision)
	}
	again, created, err := store.RecordDenied("del-x", "github", "evil/repo#1", "evil/repo", "other reason")
	if err != nil {
		t.Fatalf("re-RecordDenied = %v", err)
	}
	if created {
		t.Errorf("redelivery must not create a second row")
	}
	if again.Reason != denied.Reason {
		t.Errorf("redelivery must keep first decision %q, got %q", denied.Reason, again.Reason)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("denial created %d tasks, want 0", len(found))
	}
	if _, err := store.GetDelivery("missing"); err == nil {
		t.Fatalf("GetDelivery(missing) must fail")
	}
}

// Claims and denials must survive close/reopen with identical decisions.
func TestClaimDurabilityAcrossReopen(t *testing.T) {
	store, path := openTestStore(t)

	claimed, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	if _, _, err := store.RecordDenied("del-2", "github", "evil/repo#1", "evil/repo", "not configured"); err != nil {
		t.Fatalf("RecordDenied = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after close = %v", err)
	}
	defer reopened.Close()

	got, err := reopened.GetTask(claimed.Task.ID)
	if err != nil {
		t.Fatalf("GetTask after reopen = %v", err)
	}
	if got.Status != tasks.Queued {
		t.Errorf("status after reopen = %s, want QUEUED", got.Status)
	}
	deliveries, err := reopened.ListDeliveries()
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("want 2 deliveries after reopen, got %d", len(deliveries))
	}
	events, err := reopened.ListEvents(claimed.Task.ID)
	if err != nil {
		t.Fatalf("ListEvents after reopen = %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("want 5 events after reopen, got %d", len(events))
	}
}

// Entering RUNNING must stamp started_at so the agent timeout measures
// each attempt's wall clock from dispatch; a second run resets it.
func TestTransitionRunningStampsStartedAt(t *testing.T) {
	store, _ := openTestStore(t)

	claimed, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	if !claimed.Task.StartedAt.IsZero() {
		t.Fatalf("queued task must not carry started_at, got %v", claimed.Task.StartedAt)
	}
	if _, err := store.Transition(claimed.Task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatalf("Transition to PROVISIONING = %v", err)
	}
	if _, err := store.Transition(claimed.Task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatalf("Transition to RUNNING = %v", err)
	}
	running, err := store.GetTask(claimed.Task.ID)
	if err != nil {
		t.Fatalf("GetTask = %v", err)
	}
	if running.StartedAt.IsZero() {
		t.Fatalf("RUNNING task must carry started_at")
	}
	if !running.StartedAt.Equal(running.UpdatedAt) {
		t.Errorf("started_at %v must equal updated_at %v on the RUNNING transition",
			running.StartedAt, running.UpdatedAt)
	}

	// A retry re-enters RUNNING on a fresh attempt: the clock must reset.
	if _, err := store.Transition(claimed.Task.ID, tasks.Retrying, "controller", "test"); err != nil {
		t.Fatalf("Transition to RETRYING = %v", err)
	}
	if _, err := store.Transition(claimed.Task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatalf("Transition to PROVISIONING = %v", err)
	}
	if _, err := store.Transition(claimed.Task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatalf("second Transition to RUNNING = %v", err)
	}
	retried, err := store.GetTask(claimed.Task.ID)
	if err != nil {
		t.Fatalf("GetTask = %v", err)
	}
	if !retried.StartedAt.Equal(retried.UpdatedAt) {
		t.Errorf("second attempt must reset started_at to updated_at: %v vs %v",
			retried.StartedAt, retried.UpdatedAt)
	}
	if retried.StartedAt.Before(running.StartedAt) {
		t.Errorf("second attempt moved started_at backwards: %v -> %v",
			running.StartedAt, retried.StartedAt)
	}
}

// Resuming from PAUSED or BLOCKED continues the same attempt, so
// started_at must survive — otherwise a periodically pausing worker never
// reaches its timeout. A RETRYING re-entry is a fresh attempt and still
// resets the clock.
func TestTransitionResumePreservesStartedAt(t *testing.T) {
	store, _ := openTestStore(t)

	claimed, err := store.Claim(claimInput("del-1", "acme/web#7"))
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	transition := func(to tasks.State) {
		t.Helper()
		if _, err := store.Transition(claimed.Task.ID, to, "controller", "test"); err != nil {
			t.Fatalf("Transition to %s = %v", to, err)
		}
	}
	get := func() tasks.Task {
		t.Helper()
		task, err := store.GetTask(claimed.Task.ID)
		if err != nil {
			t.Fatalf("GetTask = %v", err)
		}
		return task
	}

	transition(tasks.Provisioning)
	transition(tasks.Running)
	started := get().StartedAt
	if started.IsZero() {
		t.Fatalf("RUNNING task must carry started_at")
	}

	// PAUSED -> RUNNING resumes the same attempt: keep the stamp.
	transition(tasks.Paused)
	transition(tasks.Running)
	resumed := get()
	if !resumed.StartedAt.Equal(started) {
		t.Errorf("resume from PAUSED restamped started_at: %v -> %v",
			started, resumed.StartedAt)
	}
	if resumed.StartedAt.Equal(resumed.UpdatedAt) {
		t.Errorf("resume from PAUSED must not restamp started_at to updated_at %v",
			resumed.UpdatedAt)
	}

	// BLOCKED -> RUNNING resumes the same attempt: keep the stamp.
	transition(tasks.Blocked)
	transition(tasks.Running)
	unblocked := get()
	if !unblocked.StartedAt.Equal(started) {
		t.Errorf("resume from BLOCKED restamped started_at: %v -> %v",
			started, unblocked.StartedAt)
	}
	if unblocked.StartedAt.Equal(unblocked.UpdatedAt) {
		t.Errorf("resume from BLOCKED must not restamp started_at to updated_at %v",
			unblocked.UpdatedAt)
	}

	// RETRYING -> RUNNING is a fresh attempt: the clock must reset.
	transition(tasks.Retrying)
	transition(tasks.Provisioning)
	transition(tasks.Running)
	retried := get()
	if !retried.StartedAt.Equal(retried.UpdatedAt) {
		t.Errorf("retry must reset started_at to updated_at: %v vs %v",
			retried.StartedAt, retried.UpdatedAt)
	}
	if retried.StartedAt.Before(started) {
		t.Errorf("retry moved started_at backwards: %v -> %v",
			started, retried.StartedAt)
	}
}
