package main

import (
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/tasks"
)

// TestSandboxNeedsSubcommand bare `sandbox` exits 2 with usage.
func TestSandboxNeedsSubcommand(t *testing.T) {
	code, _, errOut := runCmd(t, "--config", writeTestConfig(t), "sandbox")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "provision") {
		t.Errorf("stderr should list sandbox verbs, got %q", errOut)
	}
}

// TestSandboxUnknownVerb names the verb and exits 2.
func TestSandboxUnknownVerb(t *testing.T) {
	code, _, errOut := runCmd(t, "--config", writeTestConfig(t), "sandbox", "frobnicate")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "frobnicate") {
		t.Errorf("stderr should name the verb, got %q", errOut)
	}
}

// TestSandboxProvisionUnknownTask fails before touching Docker.
func TestSandboxProvisionUnknownTask(t *testing.T) {
	code, _, errOut := runCmd(t, "--config", writeTestConfig(t), "sandbox", "provision", "task_missing")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "task_missing") {
		t.Errorf("stderr should name the task, got %q", errOut)
	}
}

// TestSandboxExecUsage requires the -- separator with a command.
func TestSandboxExecUsage(t *testing.T) {
	path := writeTestConfig(t)
	for _, argv := range [][]string{
		{"sandbox", "exec"},
		{"sandbox", "exec", "some-id"},
		{"sandbox", "exec", "some-id", "--"},
		{"sandbox", "exec", "--", "echo"},
	} {
		full := append([]string{"--config", path}, argv...)
		if code, _, _ := runCmd(t, full...); code != 2 {
			t.Errorf("%v exit = %d, want 2", argv, code)
		}
	}
}

// TestSplitExecArgs cuts id -- command at the separator.
func TestSplitExecArgs(t *testing.T) {
	target, cmd, ok := splitExecArgs([]string{"task_1", "--", "go", "test", "./..."})
	if !ok || target != "task_1" || len(cmd) != 3 {
		t.Errorf("split = %q %v %v, want task_1 [go test ./...] true", target, cmd, ok)
	}
	if _, _, ok := splitExecArgs([]string{"task_1", "go"}); ok {
		t.Error("missing separator must fail")
	}
}

// TestBranchForTaskFallback derives the deterministic branch: claimed
// branch first, then herder/<issue> from the source ref.
func TestBranchForTaskFallback(t *testing.T) {
	for task, want := range map[tasks.Task]string{
		{BranchName: "herder/7-fix"}:                 "herder/7-fix",
		{SourceRef: "acme/web#42"}:                   "herder/42",
		{ID: "task_abc", SourceRef: "acme/web#nope"}: "herder/task_abc",
	} {
		if got := branchForTask(task); got != want {
			t.Errorf("branchForTask(%+v) = %q, want %q", task, got, want)
		}
	}
}

// TestSandboxStopInspectUsage require exactly one id.
func TestSandboxStopInspectUsage(t *testing.T) {
	path := writeTestConfig(t)
	for _, argv := range [][]string{
		{"sandbox", "stop"},
		{"sandbox", "destroy"},
		{"sandbox", "inspect"},
		{"sandbox", "shell"},
		{"sandbox", "provision"},
	} {
		full := append([]string{"--config", path}, argv...)
		if code, _, _ := runCmd(t, full...); code != 2 {
			t.Errorf("%v exit = %d, want 2", argv, code)
		}
	}
}

// TestTruncateBoundsEventPayloads and marks the cut.
func TestTruncateBoundsEventPayloads(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short input must pass through, got %q", got)
	}
	if got := truncate("0123456789x", 10); got != "0123456789...(truncated)" {
		t.Errorf("long input must be cut and marked, got %q", got)
	}
}
