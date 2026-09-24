package tasks

import (
	"strings"
	"testing"
)

// The happy path from SPEC section 14 must be walkable end to end.
func TestHappyPathTransitions(t *testing.T) {
	path := []State{
		Discovered, Eligible, Claimed, Queued, Provisioning,
		Running, Validating, Reviewing, Delivering, PROpen, Done,
	}
	for i := 0; i+1 < len(path); i++ {
		if err := ValidateTransition(path[i], path[i+1]); err != nil {
			t.Errorf("transition %s -> %s rejected: %v", path[i], path[i+1], err)
		}
	}
}

// Illegal jumps must fail with the offending states and legal targets.
func TestIllegalJumpRejected(t *testing.T) {
	err := ValidateTransition(Discovered, Running)
	if err == nil {
		t.Fatal("expected DISCOVERED -> RUNNING to be rejected, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "DISCOVERED") || !strings.Contains(msg, "RUNNING") {
		t.Errorf("error should name both states, got: %v", err)
	}
	if !strings.Contains(msg, "ELIGIBLE") {
		t.Errorf("error should list legal targets, got: %v", err)
	}
}

// Terminal states must have no outgoing transitions.
func TestTerminalStatesHaveNoOutgoing(t *testing.T) {
	for _, s := range []State{Done, Cancelled, TimedOut} {
		for _, to := range []State{Discovered, Queued, Running, Done} {
			if to == s {
				continue
			}
			if ValidateTransition(s, to) == nil {
				t.Errorf("terminal state %s must reject transition to %s", s, to)
			}
		}
	}
}

// Human-intervention loop: RUNNING can block, wait, pause, then resume.
func TestInterventionLoop(t *testing.T) {
	pairs := [][2]State{
		{Running, Blocked},
		{Blocked, Running},
		{Running, WaitingForHuman},
		{WaitingForHuman, Running},
		{Running, Paused},
		{Paused, Running},
		{Failed, Retrying},
		{Retrying, Queued},
		// issue #5: a human can retry a live, blocked, waiting, or frozen
		// task — retry is not reserved for failures.
		{Running, Retrying},
		{Blocked, Retrying},
		{WaitingForHuman, Retrying},
		{Paused, Retrying},
	}
	for _, p := range pairs {
		if err := ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("transition %s -> %s rejected: %v", p[0], p[1], err)
		}
	}
}

// Validation failure must route back to the agent (RETRYING -> RUNNING)
// or to a human (WAITING_FOR_HUMAN), and a no-PR delivery must be able to
// finish from REVIEWING (issue #6 acceptance criteria).
func TestValidationOutcomeRouting(t *testing.T) {
	pairs := [][2]State{
		{Validating, Retrying},
		{Retrying, Running},
		{Validating, WaitingForHuman},
		{WaitingForHuman, Running},
		{Reviewing, Done},
		// A branch that moved after the gate passed re-enters validation
		// instead of shipping unverified commits.
		{Reviewing, Validating},
		{Delivering, Validating},
		// create_pr: false rests a delivered task back in REVIEWING.
		{Delivering, Reviewing},
	}
	for _, p := range pairs {
		if err := ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("transition %s -> %s rejected: %v", p[0], p[1], err)
		}
	}
}

// ApplyTransition must move the task and mint a structured, timestamped,
// attributable event describing exactly that jump.
func TestApplyTransitionEmitsEvent(t *testing.T) {
	task := New(NewInput{SourceProvider: "github", SourceRef: "acme/web#7", Repository: "acme/web", AgentProfile: "codex-default"})
	event, err := ApplyTransition(&task, Eligible, "controller", "daemon")
	if err != nil {
		t.Fatalf("ApplyTransition = %v", err)
	}
	if task.Status != Eligible {
		t.Errorf("task status = %s, want ELIGIBLE", task.Status)
	}
	if event.TaskID != task.ID {
		t.Errorf("event task = %q, want %q", event.TaskID, task.ID)
	}
	if event.Type != EventTransition {
		t.Errorf("event type = %q, want %q", event.Type, EventTransition)
	}
	if event.ActorType != "controller" || event.ActorID != "daemon" {
		t.Errorf("event actor = %q/%q, want controller/daemon", event.ActorType, event.ActorID)
	}
	if event.CreatedAt.IsZero() {
		t.Error("event must carry a timestamp")
	}
	if !strings.Contains(event.Payload, `"from":"DISCOVERED"`) ||
		!strings.Contains(event.Payload, `"to":"ELIGIBLE"`) {
		t.Errorf("event payload must record from/to, got %s", event.Payload)
	}
}

// A rejected jump must leave the task untouched and mint no event.
func TestApplyTransitionRejectsIllegalJump(t *testing.T) {
	task := New(NewInput{SourceProvider: "github", SourceRef: "acme/web#7", Repository: "acme/web", AgentProfile: "codex-default"})
	before := task
	if _, err := ApplyTransition(&task, Running, "controller", "daemon"); err == nil {
		t.Fatal("expected illegal jump to fail, got nil")
	}
	if task != before {
		t.Error("rejected transition must not mutate the task")
	}
}

// Unknown state names must fail, not silently pass.
func TestUnknownStatesRejected(t *testing.T) {
	if err := ValidateTransition("FROBNICATING", Running); err == nil {
		t.Error("unknown source state must be rejected")
	}
	if err := ValidateTransition(Running, "FROBNICATING"); err == nil {
		t.Error("unknown target state must be rejected")
	}
}

// SanitizeDisplayName normalizes operator input into Herdr's name class:
// lowercase, unsafe runs become dashes, edges trimmed, 31-char cap, and
// a leading digit gains a t- prefix.
func TestSanitizeDisplayName(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want string
	}{
		"plain":         {"web-issue-7", "web-issue-7"},
		"spaces+case":   {"  Web Issue 7 ", "web-issue-7"},
		"unsafe chars":  {"fix: oauth/race!", "fix-oauth-race"},
		"leading digit": {"7-fix", "t-7-fix"},
		"underscores":   {"my_task__x", "my_task__x"},
		"long":          {"abcdefghijklmnopqrstuvwxyz0123456789", "abcdefghijklmnopqrstuvwxyz01234"},
		"empty":         {"", ""},
		"all unsafe":    {"!!!", ""},
		"unicode":       {"café-üñí", "caf"}, // non-ascii is outside the name class
	} {
		if got := SanitizeDisplayName(tc.in); got != tc.want {
			t.Errorf("%s: SanitizeDisplayName(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
