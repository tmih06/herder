package ingest_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/ingest"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// testSetup loads the example config over a throwaway store: repository
// tmih06/meltiply with trigger agent-ready, agent codex-default, cap 4.
func testSetup(t *testing.T) (*config.Config, *storage.Store, *ingest.Handler) {
	t.Helper()
	cfg, err := config.Load("../../examples/herder.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return cfg, store, ingest.New(cfg, store)
}

func eligibleEvent(delivery string, issue int) ingest.IssueEvent {
	return ingest.IssueEvent{
		DeliveryID:  delivery,
		Repository:  "tmih06/meltiply",
		IssueNumber: issue,
		Title:       "Fix OAuth refresh race",
		Body:        "Refresh tokens race on expiry",
		Labels:      []string{"bug", "agent-ready"},
	}
}

// An eligible delivery must create one QUEUED task with source identity,
// branch, agent profile, and a policy outcome in its timeline.
func TestHandleAccepts(t *testing.T) {
	_, store, h := testSetup(t)

	out, err := h.Handle(eligibleEvent("del-1", 182))
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionAccepted {
		t.Fatalf("decision = %q, want accepted", out.Decision)
	}
	if out.Task.Status != tasks.Queued {
		t.Errorf("status = %s, want QUEUED", out.Task.Status)
	}
	if out.Task.SourceRef != "tmih06/meltiply#182" || out.Task.SourceProvider != "github" {
		t.Errorf("source identity lost: %+v", out.Task)
	}
	if out.Task.BranchName != "herder/182-fix-oauth-refresh-race" {
		t.Errorf("branch = %q, want slug branch", out.Task.BranchName)
	}
	if out.Task.AgentProfile != "codex-default" {
		t.Errorf("agent = %q, want repo default", out.Task.AgentProfile)
	}
	if !strings.Contains(out.Task.Goal, "Fix OAuth refresh race") ||
		!strings.Contains(out.Task.Goal, "Refresh tokens race on expiry") {
		t.Errorf("goal = %q, want title and body", out.Task.Goal)
	}

	events, err := store.ListEvents(out.TaskID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	var sawPolicy bool
	for _, e := range events {
		if e.Type == tasks.EventPolicyDecision {
			sawPolicy = true
			var payload map[string]any
			if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
				t.Fatalf("policy payload is not JSON: %v", err)
			}
			if payload["decision"] != storage.DecisionAccepted || payload["delivery_id"] != "del-1" {
				t.Errorf("policy payload missing outcome: %s", e.Payload)
			}
		}
	}
	if !sawPolicy {
		t.Errorf("timeline has no policy.decision event: %v", events)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("want 1 task, got %d", len(found))
	}
}

// Redeliveries and already-claimed issues converge on the surviving task.
func TestHandleDuplicates(t *testing.T) {
	_, store, h := testSetup(t)

	first, err := h.Handle(eligibleEvent("del-1", 182))
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	for name, ev := range map[string]ingest.IssueEvent{
		"same delivery": eligibleEvent("del-1", 182),
		"same issue":    eligibleEvent("del-2", 182),
	} {
		out, err := h.Handle(ev)
		if err != nil {
			t.Fatalf("Handle(%s) = %v", name, err)
		}
		if out.Decision != storage.DecisionDuplicate {
			t.Errorf("Handle(%s) decision = %q, want duplicate", name, out.Decision)
		}
		if out.TaskID != first.TaskID {
			t.Errorf("Handle(%s) task = %s, want survivor %s", name, out.TaskID, first.TaskID)
		}
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("duplicates created %d tasks, want 1", len(found))
	}
	events, err := store.ListEvents(first.TaskID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	dups := 0
	for _, e := range events {
		if e.Type == tasks.EventWebhookDuplicate {
			dups++
		}
	}
	if dups != 2 {
		t.Errorf("want 2 webhook.duplicate log entries, got %d", dups)
	}
}

// Unknown repos, disabled repos, and missing trigger labels deny without
// a task and record the reason.
func TestHandleDenies(t *testing.T) {
	_, store, h := testSetup(t)

	unknown := eligibleEvent("del-x", 1)
	unknown.Repository = "evil/repo"
	missingLabel := eligibleEvent("del-y", 2)
	missingLabel.Labels = []string{"bug"}

	cases := []struct {
		name   string
		event  ingest.IssueEvent
		reason string
	}{
		{"unknown repository", unknown, `"evil/repo" is not configured`},
		{"missing trigger label", missingLabel, "no trigger label"},
	}
	for _, tc := range cases {
		out, err := h.Handle(tc.event)
		if err != nil {
			t.Fatalf("Handle(%s) = %v", tc.name, err)
		}
		if out.Decision != storage.DecisionPolicyDenied {
			t.Errorf("Handle(%s) decision = %q, want policy_denied", tc.name, out.Decision)
		}
		if !strings.Contains(out.Reason, tc.reason) {
			t.Errorf("Handle(%s) reason %q must mention %q", tc.name, out.Reason, tc.reason)
		}
		if out.TaskID != "" {
			t.Errorf("Handle(%s) must not create a task", tc.name)
		}
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("denials created %d tasks, want 0", len(found))
	}
	deliveries, err := store.ListDeliveries()
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("want 2 denial rows, got %d", len(deliveries))
	}
}

// A disabled repository denies even with the trigger label present.
func TestHandleDisabledRepo(t *testing.T) {
	cfg, _, _ := testSetup(t)
	repo := cfg.Repositories["tmih06/meltiply"]
	repo.Enabled = false
	cfg.Repositories["tmih06/meltiply"] = repo

	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := ingest.New(cfg, store)

	out, err := h.Handle(eligibleEvent("del-1", 182))
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionPolicyDenied || !strings.Contains(out.Reason, "disabled") {
		t.Errorf("disabled repo must deny, got %+v", out)
	}
}

// A breached concurrency cap denies without a task.
func TestHandleCapDenied(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.Scheduler.MaxWorkers = 1

	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := ingest.New(cfg, store)

	if _, err := h.Handle(eligibleEvent("del-1", 182)); err != nil {
		t.Fatalf("Handle = %v", err)
	}
	out, err := h.Handle(eligibleEvent("del-2", 183))
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionPolicyDenied || !strings.Contains(out.Reason, "concurrency cap") {
		t.Errorf("cap breach must deny, got %+v", out)
	}
	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("cap denial created a task: %d tasks", len(found))
	}
}

// Concurrent duplicate deliveries converge on one task.
func TestHandleConcurrentDuplicates(t *testing.T) {
	_, store, h := testSetup(t)

	const racers = 16
	var wg sync.WaitGroup
	outs := make([]ingest.Outcome, racers)
	errs := make([]error, racers)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = h.Handle(eligibleEvent("del-race", 182))
		}(i)
	}
	wg.Wait()
	accepted := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: Handle = %v", i, err)
		}
		if outs[i].Decision == storage.DecisionAccepted {
			accepted++
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

// Malformed deliveries are caller errors, never denials.
func TestHandleValidation(t *testing.T) {
	_, _, h := testSetup(t)

	for name, ev := range map[string]ingest.IssueEvent{
		"no delivery id": {Repository: "tmih06/meltiply", IssueNumber: 1, Labels: []string{"agent-ready"}},
		"no repository":  {DeliveryID: "d", IssueNumber: 1, Labels: []string{"agent-ready"}},
		"no issue":       {DeliveryID: "d", Repository: "tmih06/meltiply", Labels: []string{"agent-ready"}},
	} {
		if _, err := h.Handle(ev); err == nil {
			t.Errorf("Handle(%s) must fail", name)
		}
	}
}

// Only labeled actions are work; other actions are ignored, and broken
// bodies are rejected before anything durable happens.
func TestParseGitHubIssuesEvent(t *testing.T) {
	body := `{"action":"labeled",
		"label":{"name":"agent-ready"},
		"issue":{"number":182,"title":"Fix OAuth refresh race",
			"body":"Refresh tokens race on expiry",
			"labels":[{"name":"bug"},{"name":"agent-ready"}]},
		"repository":{"full_name":"tmih06/meltiply"}}`
	ev, ignored, err := ingest.ParseGitHubIssuesEvent("del-1", []byte(body))
	if err != nil || ignored {
		t.Fatalf("Parse = %+v, %v, %v; want event", ev, ignored, err)
	}
	if ev.Repository != "tmih06/meltiply" || ev.IssueNumber != 182 || ev.DeliveryID != "del-1" {
		t.Errorf("event mistranslated: %+v", ev)
	}
	if ev.Body != "Refresh tokens race on expiry" {
		t.Errorf("body = %q, want issue body", ev.Body)
	}
	for _, want := range []string{"bug", "agent-ready"} {
		found := false
		for _, l := range ev.Labels {
			found = found || l == want
		}
		if !found {
			t.Errorf("labels %v missing %q", ev.Labels, want)
		}
	}

	opened := `{"action":"opened","issue":{"number":1,"title":"x","labels":[]},
		"repository":{"full_name":"tmih06/meltiply"}}`
	if _, ignored, err := ingest.ParseGitHubIssuesEvent("d", []byte(opened)); err != nil || !ignored {
		t.Errorf("opened action must be ignored, got %v, %v", ignored, err)
	}
	if _, _, err := ingest.ParseGitHubIssuesEvent("d", []byte(`{oops`)); err == nil {
		t.Errorf("broken JSON must fail")
	}
	bare := `{"action":"labeled","issue":{"number":0,"title":"x","labels":[]},
		"repository":{"full_name":""}}`
	if _, _, err := ingest.ParseGitHubIssuesEvent("d", []byte(bare)); err == nil {
		t.Errorf("event without repo/issue must fail")
	}
}

// Branch names follow herder/<issue>-<slug> with an empty-title fallback.
func TestBranchName(t *testing.T) {
	cases := map[string]struct {
		issue int
		title string
		want  string
	}{
		"slug":          {182, "Fix OAuth refresh race", "herder/182-fix-oauth-refresh-race"},
		"punctuation":   {7, "API: export / import (v2)!", "herder/7-api-export-import-v2"},
		"empty title":   {9, "", "herder/9"},
		"blank title":   {9, "  !!!  ", "herder/9"},
		"long title":    {3, "aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk", "herder/3-aaaa-bbbb-cccc-dddd-eeee-ffff-gggg-hhhh"},
		"unicode title": {4, "Überprüfung fehlgeschlagen", "herder/4-berpr-fung-fehlgeschlagen"},
	}
	for name, tc := range cases {
		if got := ingest.BranchName(tc.issue, tc.title); got != tc.want {
			t.Errorf("%s: BranchName = %q, want %q", name, got, tc.want)
		}
	}
}

// Denials stay stable on redelivery: the first reason wins and no task
// ever appears.
func TestDeniedRedeliveryIsDuplicate(t *testing.T) {
	_, store, h := testSetup(t)

	ev := eligibleEvent("del-x", 1)
	ev.Repository = "evil/repo"
	first, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	second, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("re-Handle = %v", err)
	}
	if second.Decision != storage.DecisionDuplicate {
		t.Fatalf("redelivered denial = %q, want duplicate", second.Decision)
	}
	if first.Reason == "" || !strings.Contains(second.Reason, "policy_denied") {
		t.Errorf("denial reasons must reference the first decision: %+v %+v", first, second)
	}
	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("denials created %d tasks", len(found))
	}
}
