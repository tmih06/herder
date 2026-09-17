package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
)

// writeGateBins installs fake docker, herdr, gh, and git CLIs for
// validation and delivery tests. docker inspect reports a running
// container (mount source from STATE/workspace when set); docker exec
// exits with STATE/execmode ("ok" or "fail"). gh records every call and
// answers `issue view --json labels` with STATE/labels, `api` with
// STATE/comments, and `pr create` with a fixed URL unless STATE/ghmode
// says "exists" (then pr view answers the URL). git execs the real
// binary except that pushes to GitHub HTTPS URLs are rewritten to the
// local bare remote in STATE/remote.
func writeGateBins(t *testing.T, execMode string) string {
	t.Helper()
	// Resolve real git before PATH gains the fake.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	state := t.TempDir()
	t.Setenv("STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(state, "execmode"), []byte(execMode), 0o600); err != nil {
		t.Fatal(err)
	}

	docker := `#!/bin/sh
echo "docker $@" >> "$STATE/calls"
case "$1" in
inspect)
ws=$(cat "$STATE/workspace" 2>/dev/null)
if [ -n "$ws" ]; then
  printf '[{"Id":"abc123","Name":"/herder-task","Config":{"Image":"img"},"State":{"Status":"running"},"Mounts":[{"Type":"bind","Source":"%s","Destination":"/workspace"}]}]\n' "$ws"
else
  echo '[{"Id":"abc123","Name":"/herder-task","Config":{"Image":"img"},"State":{"Status":"running"},"Mounts":[]}]'
fi
;;
exec)
  [ "$(cat "$STATE/execmode")" = "fail" ] && { echo "gate output: FAIL"; exit 1; }
  echo "gate output: ok"
  ;;
*) exit 0 ;;
esac
`
	herdr := `#!/bin/sh
echo "herdr $@" >> "$STATE/calls"
case "$1 $2" in
"agent get")
  [ -f "$STATE/started-$3" ] || { echo "agent_not_found" >&2; exit 1; }
  ;;
"agent send")
  printf '%s' "$4" >> "$STATE/prompt-$3"
  ;;
*) exit 0 ;;
esac
`
	gh := `#!/bin/sh
echo "gh $@" >> "$STATE/calls"
mode=$(cat "$STATE/ghmode" 2>/dev/null)
case "$1 $2" in
"pr create")
  [ "$mode" = "exists" ] && { echo "a pull request for branch already exists" >&2; exit 1; }
  echo "https://github.com/acme/web/pull/201"
  ;;
"pr view")
  [ "$mode" = "exists" ] && { echo "https://github.com/acme/web/pull/201"; exit 0; }
  echo "no pull requests found" >&2; exit 1
  ;;
"issue view")
  cat "$STATE/labels" 2>/dev/null || true
  ;;
"api")
  cat "$STATE/comments" 2>/dev/null || true
  ;;
*) exit 0 ;;
esac
`
	git := `#!/bin/bash
echo "git $@" >> "$STATE/calls"
args=()
for a in "$@"; do
  case "$a" in
  https://github.com/*) args+=("$(cat "$STATE/remote" 2>/dev/null || echo "$a")") ;;
  *) args+=("$a") ;;
  esac
done
exec ` + realGit + ` "${args[@]}"
`
	for name, script := range map[string]string{"docker": docker, "herdr": herdr, "gh": gh, "git": git} {
		//nolint:gosec // G306: temp-dir test fake needs the owner-exec bit.
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// runningTask stages a RUNNING task with a bound sandbox and session, and
// a real git workspace (init commit on origin/HEAD plus the task branch)
// so the host-side gate checks run against real git output.
func runningTask(t *testing.T, cfgPath, state string, live bool) (id, workspace string) {
	t.Helper()
	id = queueTask(t, cfgPath)
	for _, s := range []string{"PROVISIONING", "RUNNING"} {
		if code, _, errOut := runCmd(t, "--config", cfgPath, "task", "transition", id, s); code != 0 {
			t.Fatalf("transition %s exit = %d (%s)", s, code, errOut)
		}
	}
	bindSession(t, cfgPath, state, id, live)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	workspace = sandbox.WorkspacePath(sandboxRoot(cfg.Database.Path), id)
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-qm", "init", "--allow-empty")
	// A local bare remote stands in for origin: the push is real git, and
	// the credential helper is never invoked for a file path remote.
	remote := filepath.Join(t.TempDir(), "remote.git")
	initBare := exec.Command("git", "init", "-q", "--bare", remote)
	if out, err := initBare.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	git("remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(state, "remote"), []byte(remote), 0o600); err != nil {
		t.Fatal(err)
	}
	git("push", "-qu", "origin", "main")
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	git("checkout", "-qb", "herder/7")
	return id, workspace
}

// commitWork adds a committed change on the task branch.
func commitWork(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "work"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// taskStatus re-reads one task's status from the store.
func taskStatus(t *testing.T, cfgPath, id string) string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return string(task.Status)
}

// taskEvents re-reads one task's event types in order.
func taskEvents(t *testing.T, cfgPath, id string) []string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ListEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

func hasEvent(types []string, want string) bool {
	for _, got := range types {
		if got == want {
			return true
		}
	}
	return false
}

// ghCalls returns the recorded gh argv lines containing the fragment.
func ghCalls(t *testing.T, state, fragment string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "calls"))
	if err != nil {
		return nil
	}
	var hits []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "gh ") && strings.Contains(line, fragment) {
			hits = append(hits, line)
		}
	}
	return hits
}

// A passing gate walks RUNNING -> VALIDATING -> REVIEWING and records
// per-command events plus validation.passed (issue #6 criterion 1).
func TestTaskValidatePasses(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, _ := runningTask(t, path, state, false)

	code, out, errOut := runCmd(t, "--config", path, "task", "validate", id)
	if code != 0 {
		t.Fatalf("validate exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "passed") {
		t.Errorf("output should report the pass, got %q", out)
	}
	if got := taskStatus(t, path, id); got != "REVIEWING" {
		t.Errorf("status = %s, want REVIEWING", got)
	}
	events := taskEvents(t, path, id)
	for _, want := range []string{
		"validation.started", "validation.command.started",
		"validation.command.succeeded", "validation.passed",
	} {
		if !hasEvent(events, want) {
			t.Errorf("missing event %s, got %v", want, events)
		}
	}
}

// A failing command with a live agent session routes back to the agent:
// RETRYING -> RUNNING, attempt bumped, failure text sent to the session.
func TestTaskValidateFailsRoutesToAgent(t *testing.T) {
	state := writeGateBins(t, "fail")
	path := writeTestConfig(t)
	id, _ := runningTask(t, path, state, true)

	code, _, errOut := runCmd(t, "--config", path, "task", "validate", id)
	if code != 1 {
		t.Fatalf("validate exit = %d, want 1 (%s)", code, errOut)
	}
	if got := taskStatus(t, path, id); got != "RUNNING" {
		t.Errorf("status = %s, want RUNNING (returned to agent)", got)
	}
	session := agent.SessionName(id)
	prompt, err := os.ReadFile(filepath.Join(state, "prompt-"+session))
	if err != nil {
		t.Fatalf("agent must receive the failure, read prompt: %v", err)
	}
	if !strings.Contains(string(prompt), "gate output: FAIL") {
		t.Errorf("prompt must carry the failure output, got %q", prompt)
	}
	events := taskEvents(t, path, id)
	if !hasEvent(events, "validation.failed") || !hasEvent(events, "validation.retry") {
		t.Errorf("missing failure routing events, got %v", events)
	}
	cfg, _ := config.Load(path)
	store, _ := storage.Open(cfg.Database.Path)
	defer store.Close()
	task, _ := store.GetTask(id)
	if task.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 after retry routing", task.Attempt)
	}
}

// A failing gate with no live agent goes to WAITING_FOR_HUMAN with the
// failure attached, preserving the sandbox for the next attempt.
func TestTaskValidateFailsToHuman(t *testing.T) {
	state := writeGateBins(t, "fail")
	path := writeTestConfig(t)
	id, _ := runningTask(t, path, state, false)

	code, _, _ := runCmd(t, "--config", path, "task", "validate", id)
	if code != 1 {
		t.Fatalf("validate exit = %d, want 1", code)
	}
	if got := taskStatus(t, path, id); got != "WAITING_FOR_HUMAN" {
		t.Errorf("status = %s, want WAITING_FOR_HUMAN", got)
	}
	if !hasEvent(taskEvents(t, path, id), "validation.awaiting_human") {
		t.Errorf("missing awaiting_human event, got %v", taskEvents(t, path, id))
	}
}

// A committed change under a forbidden glob fails validation naming the
// path, and no PR is opened.
func TestTaskDeliverForbiddenBlocksPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, ".github/workflows/ci.yml", "on: push")

	code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 1 {
		t.Fatalf("deliver exit = %d, want 1 (%s)", code, errOut)
	}
	if !strings.Contains(errOut, ".github/workflows/ci.yml") {
		t.Errorf("stderr must name the offending path, got %q", errOut)
	}
	if calls := ghCalls(t, state, "pr create"); len(calls) != 0 {
		t.Errorf("no PR may open on failed validation, ran %v", calls)
	}
	if got := taskStatus(t, path, id); got == "PR_OPEN" || got == "DONE" {
		t.Errorf("status = %s, must not reach delivery", got)
	}
}

// A dirty tree fails the clean-tree check naming the offending paths.
func TestTaskValidateDirtyTree(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	if err := os.WriteFile(filepath.Join(ws, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCmd(t, "--config", path, "task", "validate", id)
	if code != 1 {
		t.Fatalf("validate exit = %d, want 1 (%s)", code, errOut)
	}
	if !strings.Contains(errOut, "scratch.txt") {
		t.Errorf("stderr must name the dirty path, got %q", errOut)
	}
}

// Passing validation pushes the branch, opens the PR, comments the issue,
// and lands on PR_OPEN with the delivery events recorded.
func TestTaskDeliverOpensPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	// The issue still carries the trigger and running labels; delivery
	// must swap them for the review label and leave unrelated labels.
	if err := os.WriteFile(filepath.Join(state, "labels"),
		[]byte("agent-ready\nagent-running\nunrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitWork(t, ws, "internal/app.go", "package app")

	code, out, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "https://github.com/acme/web/pull/201") {
		t.Errorf("output should name the PR, got %q", out)
	}
	if got := taskStatus(t, path, id); got != "PR_OPEN" {
		t.Errorf("status = %s, want PR_OPEN", got)
	}
	for _, frag := range []string{"pr create", "issue comment", "issue edit"} {
		if len(ghCalls(t, state, frag)) == 0 {
			t.Errorf("missing gh call %q, calls %v", frag, ghCalls(t, state, ""))
		}
	}
	// The review label arrives and the earlier stage labels leave; the
	// unrelated label is never touched.
	edits := ghCalls(t, state, "issue edit")
	if !strings.Contains(edits[0], "--add-label agent-review") ||
		!strings.Contains(edits[0], "--remove-label agent-ready") ||
		!strings.Contains(edits[0], "--remove-label agent-running") ||
		strings.Contains(edits[0], "unrelated") {
		t.Errorf("label swap wrong, ran %v", edits)
	}
	// The push is real git against the local bare remote: the branch must
	// exist there, and no gh call may carry it.
	remote, err := os.ReadFile(filepath.Join(state, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	lsRemote := exec.Command("git", "ls-remote", strings.TrimSpace(string(remote)), "refs/heads/herder/7")
	if out, err := lsRemote.CombinedOutput(); err != nil || !strings.Contains(string(out), "herder/7") {
		t.Errorf("push must land the branch on origin: %v\n%s", err, out)
	}
	events := taskEvents(t, path, id)
	for _, want := range []string{"delivery.started", "pull_request.created", "issue.commented", "issue.labels_updated", "delivery.completed"} {
		if !hasEvent(events, want) {
			t.Errorf("missing event %s, got %v", want, events)
		}
	}
}

// A retried delivery finds the existing PR instead of failing or
// duplicating it (SPEC section 49 idempotency).
func TestTaskDeliverIdempotentPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	if err := os.WriteFile(filepath.Join(state, "ghmode"), []byte("exists"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "pull/201") {
		t.Errorf("existing PR url must surface, got %q", out)
	}
	if got := taskStatus(t, path, id); got != "PR_OPEN" {
		t.Errorf("status = %s, want PR_OPEN", got)
	}
}

// create_pr: false delivers nothing remote: validation still runs, then
// the task rests in REVIEWING for a human.
func TestTaskDeliverNoPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	// Flip the example config's create_pr to false.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw),
		"create_pr: true", "create_pr: false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")

	code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}
	if got := taskStatus(t, path, id); got != "REVIEWING" {
		t.Errorf("status = %s, want REVIEWING", got)
	}
	if calls := ghCalls(t, state, "pr create"); len(calls) != 0 {
		t.Errorf("create_pr: false must not open a PR, ran %v", calls)
	}
}

// task done completes the PR_OPEN task and applies the completed label.
func TestTaskDone(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	if code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id); code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}

	code, _, errOut := runCmd(t, "--config", path, "task", "done", id)
	if code != 0 {
		t.Fatalf("done exit = %d (%s)", code, errOut)
	}
	if got := taskStatus(t, path, id); got != "DONE" {
		t.Errorf("status = %s, want DONE", got)
	}
	if calls := ghCalls(t, state, "--add-label completed"); len(calls) == 0 {
		t.Errorf("completed label must be applied, calls %v", ghCalls(t, state, "issue edit"))
	}
	if !hasEvent(taskEvents(t, path, id), "task.completed") {
		t.Errorf("missing task.completed event, got %v", taskEvents(t, path, id))
	}
}

// With create_pr: true a REVIEWING task has not shipped: done must refuse
// so unshipped work cannot be marked complete.
func TestTaskDoneRequiresPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	for _, s := range []string{"VALIDATING", "REVIEWING"} {
		if code, _, errOut := runCmd(t, "--config", path, "task", "transition", id, s); code != 0 {
			t.Fatalf("transition %s exit = %d (%s)", s, code, errOut)
		}
	}

	code, _, errOut := runCmd(t, "--config", path, "task", "done", id)
	if code != 1 {
		t.Fatalf("done exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "deliver") {
		t.Errorf("stderr should point at task deliver, got %q", errOut)
	}
	if got := taskStatus(t, path, id); got != "REVIEWING" {
		t.Errorf("status = %s, want REVIEWING", got)
	}
}

// With create_pr: false the REVIEWING rest state is the shipped state, so
// done completes the task without a PR.
func TestTaskDoneNoPR(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw),
		"create_pr: true", "create_pr: false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	if code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id); code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}

	code, _, errOut := runCmd(t, "--config", path, "task", "done", id)
	if code != 0 {
		t.Fatalf("done exit = %d (%s)", code, errOut)
	}
	if got := taskStatus(t, path, id); got != "DONE" {
		t.Errorf("status = %s, want DONE", got)
	}
}

// The push must stage from the workspace the container actually mounts,
// not a re-derived path: a divergent mount source is the directory that
// was validated, so it is the directory that ships. A non-git mount fails
// the drift check's rev-parse, proving the mount path was used.
func TestDeliverPRUsesInspectedMount(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	if code, _, errOut := runCmd(t, "--config", path, "task", "validate", id); code != 0 {
		t.Fatalf("validate exit = %d (%s)", code, errOut)
	}
	// The inspected mount points at a directory that is not a git repo:
	// if the drift check ran in the deterministic workspace it would
	// resolve the branch and proceed to push.
	bogus := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "workspace"), []byte(bogus), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 1 {
		t.Fatalf("deliver exit = %d, want 1 (drift check must run in the mount)", code)
	}
	if !strings.Contains(errOut, "resolve") {
		t.Errorf("stderr should name the rev-parse failure, got %q", errOut)
	}
	if got := taskStatus(t, path, id); got != "REVIEWING" {
		t.Errorf("status = %s, want REVIEWING", got)
	}
}

// A branch that moved after the gate passed must re-run validation, not
// ship the drifted commits: the second deliver run re-enters VALIDATING
// and only then pushes.
func TestTaskDeliverRevalidatesMovedBranch(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	if code, _, errOut := runCmd(t, "--config", path, "task", "validate", id); code != 0 {
		t.Fatalf("validate exit = %d (%s)", code, errOut)
	}
	// The still-live agent commits again after the gate passed.
	commitWork(t, ws, "internal/more.go", "package more")

	code, _, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 0 {
		t.Fatalf("deliver exit = %d (%s)", code, errOut)
	}
	if got := taskStatus(t, path, id); got != "PR_OPEN" {
		t.Errorf("status = %s, want PR_OPEN", got)
	}
	events := taskEvents(t, path, id)
	var passes int
	for _, e := range events {
		if e == "validation.passed" {
			passes++
		}
	}
	if passes != 2 {
		t.Errorf("a moved branch must re-run the gate, got %v", events)
	}
}

// Validation only runs on tasks the agent finished: a QUEUED task is
// refused before any sandbox or git call.
func TestTaskValidateNeedsRunningState(t *testing.T) {
	writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id := queueTask(t, path)

	code, _, errOut := runCmd(t, "--config", path, "task", "validate", id)
	if code != 1 {
		t.Fatalf("validate exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "QUEUED") {
		t.Errorf("stderr should name the blocking state, got %q", errOut)
	}
}

// A task stranded in DELIVERING by a mid-delivery failure must resume:
// the rerun pushes, opens the PR, and lands in PR_OPEN instead of dying
// on an illegal DELIVERING -> DELIVERING transition.
func TestTaskDeliverResumesDelivering(t *testing.T) {
	state := writeGateBins(t, "ok")
	path := writeTestConfig(t)
	id, ws := runningTask(t, path, state, false)
	commitWork(t, ws, "internal/app.go", "package app")
	for _, s := range []string{"VALIDATING", "REVIEWING", "DELIVERING"} {
		if code, _, errOut := runCmd(t, "--config", path, "task", "transition", id, s); code != 0 {
			t.Fatalf("transition %s exit = %d (%s)", s, code, errOut)
		}
	}

	code, out, errOut := runCmd(t, "--config", path, "task", "deliver", id)
	if code != 0 {
		t.Fatalf("resumed deliver exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "pull/201") {
		t.Errorf("resumed delivery must report the PR, got %q", out)
	}
	if got := taskStatus(t, path, id); got != "PR_OPEN" {
		t.Errorf("status = %s, want PR_OPEN", got)
	}
}
