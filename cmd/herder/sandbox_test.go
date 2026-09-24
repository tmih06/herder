package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/textutil"
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
		if got := dispatch.BranchForTask(task); got != want {
			t.Errorf("dispatch.BranchForTask(%+v) = %q, want %q", task, got, want)
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
	if got := textutil.Truncate("abc", 10); got != "abc" {
		t.Errorf("short input must pass through, got %q", got)
	}
	if got := textutil.Truncate("0123456789x", 10); got != "0123456789...(truncated)" {
		t.Errorf("long input must be cut and marked, got %q", got)
	}
}

// --- sandbox destroy target expansion -------------------------------------

// listFunc adapts a static sandbox list to the injectable List seam.
func listFunc(sandboxes []sandbox.Sandbox, err error) func(context.Context) ([]sandbox.Sandbox, error) {
	return func(context.Context) ([]sandbox.Sandbox, error) { return sandboxes, err }
}

// openTestStore returns a scratch store for resolveTarget's task lookup.
func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "herder.db"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Plain ids pass through resolveTarget untouched: a task id maps to its
// container name, an unknown string is treated as a container name, and
// the managed list is never consulted (no glob metacharacters).
func TestExpandSandboxTargetsPlainIDs(t *testing.T) {
	store := openTestStore(t)
	task, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "api", SourceRef: "acme/web#7", Repository: "acme/web",
		AgentProfile: "codex-default", Goal: "g",
	})
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	calls := 0
	list := func(context.Context) ([]sandbox.Sandbox, error) {
		calls++
		return nil, errors.New("list must not run for plain ids")
	}
	got, err := expandSandboxTargets(context.Background(), list, store,
		[]string{task.ID, "herder-orphan"})
	if err != nil {
		t.Fatalf("expand = %v", err)
	}
	want := []string{"herder-" + task.ID, "herder-orphan"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("targets = %v, want %v", got, want)
	}
	if calls != 0 {
		t.Errorf("List ran %d times for plain ids", calls)
	}
}

// A glob expands over the managed list, matching both the full container
// name and its herder- stripped short form; order follows List order.
func TestExpandSandboxTargetsGlob(t *testing.T) {
	store := openTestStore(t)
	managed := []sandbox.Sandbox{
		{ID: "herder-task_aaa"}, {ID: "herder-task_bbb"}, {ID: "herder-other"},
	}
	for name, tc := range map[string]struct {
		pattern string
		want    []string
	}{
		"short glob":   {"task_*", []string{"herder-task_aaa", "herder-task_bbb"}},
		"full glob":    {"herder-task_*", []string{"herder-task_aaa", "herder-task_bbb"}},
		"single char":  {"herder-task_aa?", []string{"herder-task_aaa"}},
		"char class":   {"herder-task_[ab]*", []string{"herder-task_aaa", "herder-task_bbb"}},
		"all":          {"*", []string{"herder-task_aaa", "herder-task_bbb", "herder-other"}},
		"other prefix": {"other", nil}, // no metacharacters: plain id path, not glob
	} {
		got, err := expandSandboxTargets(context.Background(), listFunc(managed, nil), store, []string{tc.pattern})
		if tc.want == nil {
			// "other" is a plain target: passes through unresolved.
			if err != nil || len(got) != 1 || got[0] != "other" {
				t.Errorf("%s: plain passthrough = %v, %v", name, got, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: expand = %v", name, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: targets = %v, want %v", name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: targets[%d] = %s, want %s", name, i, got[i], tc.want[i])
			}
		}
	}
}

// A pattern matching zero managed containers is an error: a typo must
// never look like a successful sweep.
func TestExpandSandboxTargetsGlobNoMatch(t *testing.T) {
	store := openTestStore(t)
	managed := []sandbox.Sandbox{{ID: "herder-task_aaa"}}
	_, err := expandSandboxTargets(context.Background(), listFunc(managed, nil), store, []string{"task_zzz*"})
	if err == nil || !strings.Contains(err.Error(), "task_zzz*") {
		t.Errorf("no-match pattern must error naming the pattern, got %v", err)
	}
	// Empty managed list: every glob misses.
	_, err = expandSandboxTargets(context.Background(), listFunc(nil, nil), store, []string{"*"})
	if err == nil {
		t.Errorf("glob over empty fleet must error")
	}
}

// Mixed args dedupe: a task id and a glob covering its container destroy
// once, and repeated globs collapse overlapping matches in first-seen order.
func TestExpandSandboxTargetsMixedDedupe(t *testing.T) {
	store := openTestStore(t)
	task, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "api", SourceRef: "acme/web#9", Repository: "acme/web",
		AgentProfile: "codex-default", Goal: "g",
	})
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	managed := []sandbox.Sandbox{
		{ID: "herder-" + task.ID}, {ID: "herder-task_bbb"},
	}
	got, err := expandSandboxTargets(context.Background(), listFunc(managed, nil), store,
		[]string{task.ID, "task_*", "herder-*"})
	if err != nil {
		t.Fatalf("expand = %v", err)
	}
	want := []string{"herder-" + task.ID, "herder-task_bbb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("targets = %v, want %v (dedupe, first-seen order)", got, want)
	}
}

// A List failure propagates instead of expanding against an empty fleet.
func TestExpandSandboxTargetsListError(t *testing.T) {
	store := openTestStore(t)
	_, err := expandSandboxTargets(context.Background(),
		listFunc(nil, errors.New("docker down")), store, []string{"task_*"})
	if err == nil || !strings.Contains(err.Error(), "docker down") {
		t.Errorf("list error must propagate, got %v", err)
	}
}
