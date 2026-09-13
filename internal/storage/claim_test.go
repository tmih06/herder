package storage

import (
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
		BranchName:     "herder/7",
		MaxActive:      4,
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

// A breached concurrency cap must deny without creating a task.
func TestClaimCapDenied(t *testing.T) {
	store, _ := openTestStore(t)

	in := claimInput("del-1", "acme/web#7")
	in.MaxActive = 1
	if _, err := store.Claim(in); err != nil {
		t.Fatalf("Claim = %v", err)
	}
	deniedIn := claimInput("del-2", "acme/web#8")
	deniedIn.MaxActive = 1
	out, err := store.Claim(deniedIn)
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	if out.Decision != DecisionPolicyDenied {
		t.Fatalf("decision = %q, want policy_denied", out.Decision)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("denied claim created a task: %d tasks", len(found))
	}
	del, err := store.GetDelivery("del-2")
	if err != nil {
		t.Fatalf("GetDelivery = %v", err)
	}
	if del.Decision != DecisionPolicyDenied || del.Reason == "" {
		t.Errorf("denial must record decision and reason: %+v", del)
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
	if n, err := reopened.CountActive(); err != nil || n != 1 {
		t.Fatalf("CountActive after reopen = %d, %v; want 1", n, err)
	}
}

// Terminal tasks must free concurrency capacity.
func TestCountActiveIgnoresTerminal(t *testing.T) {
	store, _ := openTestStore(t)

	in := claimInput("del-1", "acme/web#7")
	in.MaxActive = 1
	claimed, err := store.Claim(in)
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	for _, to := range []tasks.State{tasks.Provisioning, tasks.Running, tasks.Validating, tasks.Reviewing, tasks.Delivering, tasks.PROpen, tasks.Done} {
		if _, err := store.Transition(claimed.Task.ID, to, "controller", "test"); err != nil {
			t.Fatalf("Transition to %s = %v", to, err)
		}
	}
	next := claimInput("del-2", "acme/web#8")
	next.MaxActive = 1
	out, err := store.Claim(next)
	if err != nil {
		t.Fatalf("Claim = %v", err)
	}
	if out.Decision != DecisionAccepted {
		t.Fatalf("decision after terminal = %q, want accepted", out.Decision)
	}
}
