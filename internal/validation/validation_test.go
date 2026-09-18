package validation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
)

// fakeExec scripts sandbox exec results by command text and records calls.
type fakeExec struct {
	calls   [][]string
	respond func(cmd string) (*sandbox.Result, error)
}

func (f *fakeExec) run(ctx context.Context, id string, cmd []string) (*sandbox.Result, error) {
	f.calls = append(f.calls, cmd)
	if f.respond != nil {
		return f.respond(strings.Join(cmd, " "))
	}
	return &sandbox.Result{}, nil
}

// fakeGit scripts host-side git results and records argv. A respond func
// returning the zero RunResult falls through to the defaults:
// symbolic-ref answers the task branch, everything else an empty success.
type fakeGit struct {
	calls   [][]string
	respond func(args []string) (sandbox.RunResult, error)
}

func (f *fakeGit) run(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.respond != nil {
		if res, err := f.respond(args); err != nil || res != (sandbox.RunResult{}) {
			return res, err
		}
	}
	if strings.Contains(strings.Join(args, " "), "symbolic-ref") {
		return sandbox.RunResult{Stdout: "herder/7\n"}, nil
	}
	return sandbox.RunResult{}, nil
}

// cleanGit answers the host-side probes with an empty diff and clean tree.
func cleanGit(args []string) (sandbox.RunResult, error) {
	return sandbox.RunResult{}, nil
}

// collectEvents captures the gate's emitted events in order.
type eventLog struct {
	types    []string
	payloads []map[string]any
}

func (l *eventLog) emit(t string, p map[string]any) {
	l.types = append(l.types, t)
	l.payloads = append(l.payloads, p)
}

func (l *eventLog) has(t string) bool {
	for _, got := range l.types {
		if got == t {
			return true
		}
	}
	return false
}

func testInput(cfg config.ValidationConfig) Input {
	return Input{
		TaskID: "task_abc", Container: "herder-task_abc",
		Workspace: "/tmp/ws", Branch: "herder/7", Config: cfg,
	}
}

// A fully passing run must record per-command start/finish events and a
// final validation.passed (issue #6 acceptance criterion).
func TestGateAllCommandsPass(t *testing.T) {
	exec := &fakeExec{}
	git := &fakeGit{respond: cleanGit}
	events := &eventLog{}
	gate := &Gate{Exec: exec.run, Runner: git.run, Emit: events.emit}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{
		Commands: []string{"gofmt -l .", "go test ./..."},
	}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !rep.Passed() {
		t.Fatalf("report must pass, got %+v", rep)
	}
	if len(rep.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(rep.Commands))
	}
	if len(exec.calls) != 2 || exec.calls[0][0] != "sh" || exec.calls[0][1] != "-c" {
		t.Errorf("commands must run via sh -c inside the sandbox, ran %v", exec.calls)
	}
	for _, want := range []string{
		"validation.started", "validation.command.started",
		"validation.command.succeeded", "validation.passed",
	} {
		if !events.has(want) {
			t.Errorf("missing event %s, got %v", want, events.types)
		}
	}
}

// A failing command must fail the gate with its output attached, and the
// remaining commands still run so the report is complete.
func TestGateCommandFailure(t *testing.T) {
	exec := &fakeExec{respond: func(cmd string) (*sandbox.Result, error) {
		if strings.Contains(cmd, "go test") {
			return &sandbox.Result{ExitCode: 1, Stdout: "FAIL: TestX"}, nil
		}
		return &sandbox.Result{}, nil
	}}
	git := &fakeGit{respond: cleanGit}
	events := &eventLog{}
	gate := &Gate{Exec: exec.run, Runner: git.run, Emit: events.emit}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{
		Commands: []string{"go test ./...", "gofmt -l ."},
	}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rep.Passed() {
		t.Fatal("a failed command must fail the gate")
	}
	if len(exec.calls) != 2 {
		t.Errorf("all commands must run for a complete report, ran %d", len(exec.calls))
	}
	if !events.has("validation.command.failed") || !events.has("validation.failed") {
		t.Errorf("missing failure events, got %v", events.types)
	}
	summary := rep.Summary()
	if !strings.Contains(summary, "go test ./...") || !strings.Contains(summary, "FAIL: TestX") {
		t.Errorf("summary must name the command and its output, got %q", summary)
	}
}

// A change under a forbidden glob must fail validation naming the path,
// even when every command passes.
func TestGateForbiddenPath(t *testing.T) {
	exec := &fakeExec{}
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "merge-base"):
			return sandbox.RunResult{Stdout: "abc123\n"}, nil
		case strings.Contains(joined, "diff"):
			return sandbox.RunResult{Stdout: "internal/app.go\x00.github/workflows/ci.yml\x00"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: exec.run, Runner: git.run}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{
		Commands:         []string{"go test ./..."},
		ForbiddenChanges: []string{".github/workflows/**"},
	}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rep.Passed() {
		t.Fatal("a forbidden change must fail the gate")
	}
	if len(rep.Forbidden) != 1 || rep.Forbidden[0] != ".github/workflows/ci.yml" {
		t.Errorf("forbidden = %v, want the workflow path", rep.Forbidden)
	}
	if !strings.Contains(rep.Summary(), ".github/workflows/ci.yml") {
		t.Errorf("summary must name the offending path, got %q", rep.Summary())
	}
}

// Uncommitted or untracked work must fail the clean-tree check naming the
// offending paths; require_clean_git: false lifts the check.
func TestGateDirtyTree(t *testing.T) {
	dirty := func(args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "status") {
			return sandbox.RunResult{Stdout: " M internal/app.go\n?? notes.txt\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}
	exec := &fakeExec{}
	git := &fakeGit{respond: dirty}
	gate := &Gate{Exec: exec.run, Runner: git.run}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rep.Passed() {
		t.Fatal("a dirty tree must fail the gate by default")
	}
	if len(rep.Dirty) != 2 {
		t.Errorf("dirty = %v, want both offending paths", rep.Dirty)
	}
	if !strings.Contains(rep.Summary(), "internal/app.go") {
		t.Errorf("summary must name dirty paths, got %q", rep.Summary())
	}

	off := false
	rep, err = gate.Run(context.Background(), testInput(config.ValidationConfig{
		RequireCleanGit: &off,
	}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !rep.Passed() {
		t.Errorf("require_clean_git: false must lift the dirty check, got %+v", rep)
	}
}

// An exec transport failure is an error, not a failed report: the agent
// did not fail validation, the sandbox did not answer.
func TestGateExecTransportError(t *testing.T) {
	exec := &fakeExec{respond: func(cmd string) (*sandbox.Result, error) {
		return nil, errors.New("container not running")
	}}
	gate := &Gate{Exec: exec.run, Runner: (&fakeGit{respond: cleanGit}).run}
	if _, err := gate.Run(context.Background(), testInput(config.ValidationConfig{
		Commands: []string{"go test ./..."},
	})); err == nil {
		t.Fatal("transport failure must return an error")
	}
}

// Without origin/HEAD the diff base falls back to HEAD so hand-rolled
// workspaces still get a forbidden-path scan.
func TestGateBaseFallback(t *testing.T) {
	exec := &fakeExec{}
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "merge-base"):
			return sandbox.RunResult{ExitCode: 128, Stderr: "no origin"}, nil
		case strings.Contains(joined, "origin/HEAD"):
			return sandbox.RunResult{ExitCode: 128, Stderr: "no origin"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: exec.run, Runner: git.run}
	if _, err := gate.Run(context.Background(), testInput(config.ValidationConfig{})); err != nil {
		t.Fatalf("Run = %v", err)
	}
	var diff []string
	for _, c := range git.calls {
		if strings.Contains(strings.Join(c, " "), "diff --name-only") {
			diff = c
		}
	}
	if diff == nil || diff[len(diff)-1] != "HEAD" {
		t.Errorf("diff base must fall back to HEAD, calls %v", git.calls)
	}
}

// The recorded provisioning-time SHA pins the diff base: refs inside the
// workspace are agent-writable, so merge-base against origin/HEAD would
// be forgeable. With BaseSHA set, no ref lookup runs at all.
func TestGateBaseSHAPinned(t *testing.T) {
	git := &fakeGit{respond: cleanGit}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	in := testInput(config.ValidationConfig{})
	in.BaseSHA = "abc123"
	if _, err := gate.Run(context.Background(), in); err != nil {
		t.Fatalf("Run = %v", err)
	}
	var diff []string
	for _, c := range git.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "merge-base") {
			t.Errorf("a pinned base must not consult refs, ran %v", c)
		}
		if strings.Contains(joined, "diff --name-only") {
			diff = c
		}
	}
	if diff == nil || diff[len(diff)-1] != "abc123" {
		t.Errorf("diff must run against the recorded SHA, calls %v", git.calls)
	}
}

// The gate must refuse work committed on the wrong branch or a detached
// HEAD: validation proves the task branch's tree and delivery pushes that
// branch, so a mismatch would ship an unverified ref.
func TestGateWrongBranch(t *testing.T) {
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "symbolic-ref") {
			return sandbox.RunResult{Stdout: "agent-scratch\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rep.Passed() {
		t.Fatal("a wrong branch must fail the gate")
	}
	if !strings.Contains(rep.Summary(), "agent-scratch") || !strings.Contains(rep.Summary(), "herder/7") {
		t.Errorf("summary must name both branches, got %q", rep.Summary())
	}
}

// A detached HEAD is a wrong branch too: symbolic-ref exits non-zero and
// the gate must fail rather than ship an unnamed ref.
func TestGateDetachedHead(t *testing.T) {
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "symbolic-ref") {
			return sandbox.RunResult{ExitCode: 128, Stderr: "fatal: ref HEAD is not a symbolic ref"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rep.Passed() {
		t.Fatal("a detached HEAD must fail the gate")
	}
	if !strings.Contains(rep.Summary(), "detached") {
		t.Errorf("summary must name the detached HEAD, got %q", rep.Summary())
	}
}

// The changed-file scan must diff against the merge-base, not the moving
// origin/HEAD tip: upstream commits after the branch point are not the
// task's changes and must not trip the forbidden-path check.
func TestGateDiffsMergeBase(t *testing.T) {
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "merge-base") {
			return sandbox.RunResult{Stdout: "abc123\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	if _, err := gate.Run(context.Background(), testInput(config.ValidationConfig{})); err != nil {
		t.Fatalf("Run = %v", err)
	}
	var diff []string
	for _, c := range git.calls {
		if strings.Contains(strings.Join(c, " "), "diff --name-only") {
			diff = c
		}
	}
	if diff == nil || diff[len(diff)-1] != "abc123" {
		t.Errorf("diff must run against the merge-base, calls %v", git.calls)
	}
}

// The clean-tree scan must not be configurable away by the agent's repo:
// -uall lists every untracked file and the -c override pins
// status.showUntrackedFiles so .git/config cannot hide work.
func TestGateStatusNotConfigurable(t *testing.T) {
	git := &fakeGit{respond: cleanGit}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	if _, err := gate.Run(context.Background(), testInput(config.ValidationConfig{})); err != nil {
		t.Fatalf("Run = %v", err)
	}
	var status []string
	for _, c := range git.calls {
		if strings.Contains(strings.Join(c, " "), "status") {
			status = c
		}
	}
	joined := strings.Join(status, " ")
	if !strings.Contains(joined, "-uall") || !strings.Contains(joined, "status.showUntrackedFiles=all") {
		t.Errorf("status must pin untracked-file reporting, ran %v", status)
	}
}

// git diff -z emits raw paths separated by NUL: a C-quoted name would
// evade the forbidden glob, so the scan must read the -z format.
func TestGateDiffNULSeparated(t *testing.T) {
	git := &fakeGit{respond: func(args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "diff --name-only") {
			return sandbox.RunResult{Stdout: ".github/workflows/évil.yml\x00internal/app.go\x00"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	gate := &Gate{Exec: (&fakeExec{}).run, Runner: git.run}

	rep, err := gate.Run(context.Background(), testInput(config.ValidationConfig{
		ForbiddenChanges: []string{".github/workflows/**"},
	}))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(rep.Forbidden) != 1 || rep.Forbidden[0] != ".github/workflows/évil.yml" {
		t.Errorf("forbidden = %v, want the raw UTF-8 path", rep.Forbidden)
	}
	var diff []string
	for _, c := range git.calls {
		if strings.Contains(strings.Join(c, " "), "diff --name-only") {
			diff = c
		}
	}
	if diff == nil || !strings.Contains(strings.Join(diff, " "), "-z") {
		t.Errorf("diff must use -z so quoted paths cannot evade the glob, ran %v", diff)
	}
}

// Only R/C status lines carry "orig -> new"; a plain untracked file named
// "a -> b" must keep its full name in the dirty and forbidden scans.
func TestPorcelainPathRenameOnly(t *testing.T) {
	if got, ok := porcelainPath("?? .github/workflows/a -> b.yml"); !ok || got != ".github/workflows/a -> b.yml" {
		t.Errorf("untracked path with -> must not split, got %q", got)
	}
	if got, ok := porcelainPath("R  old.yml -> .github/workflows/ci.yml"); !ok || got != ".github/workflows/ci.yml" {
		t.Errorf("rename must report the new path, got %q", got)
	}
}

// The forbidden-path matcher: * and ? stay inside one segment, ** crosses
// separators, and patterns anchor to the repo root.
func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/a/b.yml", true},
		{".github/workflows/**", ".github/workflows", true},
		{".github/workflows/**", ".github/other/ci.yml", false},
		{"*.pem", "keys/cert.pem", false},
		{"*.pem", "cert.pem", true},
		{"**/secrets/**", "a/secrets/b.txt", true},
		{"**/secrets/**", "secrets/b.txt", true},
		{"docs/*.md", "docs/readme.md", true},
		{"docs/*.md", "docs/a/readme.md", false},
		{"docs/?.md", "docs/a.md", true},
		{"docs/?.md", "docs/ab.md", false},
		{"exact.go", "exact.go", true},
		{"exact.go", "exact.go.bak", false},
	} {
		if got := matchGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
