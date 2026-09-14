package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBins installs fake `herdr` and `docker` CLIs on PATH for agent
// launch tests. The herdr fake records calls, tracks started sessions in
// STATE markers, and answers `agent get` by marker presence; dockerMode
// selects the inspect outcome ("ready" or "missing").
func writeFakeBins(t *testing.T, dockerMode string) string {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	t.Setenv("STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	herdr := `#!/bin/sh
echo "$@" >> "$STATE/calls"
case "$1 $2" in
"agent start")
  echo '{"result":{"agent":{"pane_id":"w9:p1","workspace_id":"w9"}}}'
  touch "$STATE/started-$3"
  ;;
"agent send")
  printf '%s' "$4" >> "$STATE/prompt-$3"
  ;;
"agent get")
  [ -f "$STATE/started-$3" ] || { echo "agent_not_found" >&2; exit 1; }
  ;;
*) echo "unexpected herdr: $@" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(herdr), 0o755); err != nil {
		t.Fatal(err)
	}
	var docker string
	if dockerMode == "ready" {
		docker = `#!/bin/sh
if [ "$1" = "inspect" ]; then
cat <<'EOF'
[{"Id":"abc123","Name":"/herder-task","Config":{"Image":"golang:1.22-bookworm"},"State":{"Status":"running"},"Mounts":[{"Source":"/tmp/w","Destination":"/workspace"}]}]
EOF
else echo "unexpected docker: $@" >&2; exit 1; fi
`
	} else {
		docker = `#!/bin/sh
echo "Error: No such object: $2" >&2
exit 1
`
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	return state
}

// queueTask creates a task and walks it to QUEUED, returning its id.
func queueTask(t *testing.T, cfg string) string {
	t.Helper()
	withConfig := func(argv ...string) []string {
		return append([]string{"--config", cfg}, argv...)
	}
	code, id, errOut := runCmd(t, withConfig("task", "create",
		"--repo", "acme/web", "--source-ref", "acme/web#7")...)
	if code != 0 {
		t.Fatalf("create exit = %d (%s)", code, errOut)
	}
	id = strings.TrimSpace(id)
	for _, state := range []string{"ELIGIBLE", "CLAIMED", "QUEUED"} {
		if code, _, errOut := runCmd(t, withConfig("task", "transition", id, state)...); code != 0 {
			t.Fatalf("transition %s exit = %d (%s)", state, code, errOut)
		}
	}
	return id
}

// TestTaskStartUnknownTask fails before touching Herdr.
func TestTaskStartUnknownTask(t *testing.T) {
	writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	code, _, errOut := runCmd(t, "--config", path, "task", "start", "task_missing")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "not found") {
		t.Errorf("stderr should name the missing task, got %q", errOut)
	}
}

// TestTaskStartNeedsLaunchableState refuses tasks that cannot run yet.
func TestTaskStartNeedsLaunchableState(t *testing.T) {
	writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	code, id, _ := runCmd(t, "--config", path, "task", "create",
		"--repo", "acme/web", "--source-ref", "acme/web#7")
	if code != 0 {
		t.Fatalf("create exit = %d, want 0", code)
	}
	id = strings.TrimSpace(id)
	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "DISCOVERED") {
		t.Errorf("stderr should name the blocking state, got %q", errOut)
	}
}

// TestTaskStartNeedsSandbox refuses to launch when no container is ready.
func TestTaskStartNeedsSandbox(t *testing.T) {
	writeFakeBins(t, "missing")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "sandbox provision") {
		t.Errorf("stderr should point at sandbox provision, got %q", errOut)
	}
}

// TestTaskStartLaunchesAgent proves the acceptance core: a queued task with
// a ready sandbox launches through Herdr, reaches RUNNING, and records the
// session binding plus a prompt carrying goal, metadata, and validation.
func TestTaskStartLaunchesAgent(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)

	code, out, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 0 {
		t.Fatalf("start exit = %d (%s)", code, errOut)
	}
	session := "herder-" + id
	if !strings.Contains(out, session) {
		t.Errorf("stdout should name the session %s, got %q", session, out)
	}

	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"RUNNING", session, "agent.started"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}

	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if !strings.Contains(string(calls), "HERDR_AGENT=codex") {
		t.Errorf("herdr start should carry HERDR_AGENT=codex, got %q", calls)
	}
	prompt, _ := os.ReadFile(filepath.Join(state, "prompt-"+session))
	for _, want := range []string{"acme/web#7", "herder/7", "go test ./..."} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt should contain %q, got:\n%s", want, prompt)
		}
	}
}

// TestTaskStartReusesLiveSession proves relaunch converges: a second start
// reuses the live session instead of orphaning a pane.
func TestTaskStartReusesLiveSession(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)

	if code, _, errOut := runCmd(t, "--config", path, "task", "start", id); code != 0 {
		t.Fatalf("first start exit = %d (%s)", code, errOut)
	}
	if code, _, errOut := runCmd(t, "--config", path, "task", "start", id); code != 0 {
		t.Fatalf("second start exit = %d (%s)", code, errOut)
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if n := strings.Count(string(calls), "agent start"); n != 1 {
		t.Errorf("relaunch should reuse the session (1 start), got %d in %q", n, calls)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 || !strings.Contains(inspect, "reused") {
		t.Errorf("inspect should record the reuse, got:\n%s", inspect)
	}
}

// TestTaskStartUnknownProfileFailsTask proves an unresolvable agent profile
// fails the task with a clear event instead of hanging.
func TestTaskStartUnknownProfileFailsTask(t *testing.T) {
	writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)

	code, _, errOut := runCmd(t, "--config", path, "task", "start", "--agent", "nope", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("stderr should name the unknown profile, got %q", errOut)
	}
	// An override typo leaves the task untouched for a corrected retry.
	if code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id); code != 0 ||
		!strings.Contains(inspect, "QUEUED") {
		t.Errorf("override typo should leave the task QUEUED, got:\n%s", inspect)
	}
}

// TestTaskStartStaleProfileFailsTask proves a task whose claimed profile no
// longer exists fails with a clear event instead of hanging: the operator
// cannot retry into a launch, so FAILED plus agent.start_failed is the
// honest state.
func TestTaskStartStaleProfileFailsTask(t *testing.T) {
	writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.ReplaceAll(string(raw), "codex-default", "claude-default")
	stale = strings.ReplaceAll(stale, "kind: codex", "kind: claude")
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "codex-default") {
		t.Errorf("stderr should name the stale profile, got %q", errOut)
	}
	if code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id); code != 0 ||
		!strings.Contains(inspect, "FAILED") || !strings.Contains(inspect, "agent.start_failed") {
		t.Errorf("stale profile should fail the task with an event, got:\n%s", inspect)
	}
}

// TestTaskAttachNeedsSession points at task start when nothing runs.
func TestTaskAttachNeedsSession(t *testing.T) {
	path := writeTestConfig(t)
	id := queueTask(t, path)
	code, _, errOut := runCmd(t, "--config", path, "task", "attach", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "task start") {
		t.Errorf("stderr should point at task start, got %q", errOut)
	}
}

// TestTaskAttachTargetsSession proves attach resolves the task to its live
// Herdr session instead of replaying logs.
func TestTaskAttachTargetsSession(t *testing.T) {
	writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	if code, _, errOut := runCmd(t, "--config", path, "task", "start", id); code != 0 {
		t.Fatalf("start exit = %d (%s)", code, errOut)
	}
	var got []string
	old := execAttach
	execAttach = func(argv []string) int { got = argv; return 0 }
	defer func() { execAttach = old }()
	if code, _, errOut := runCmd(t, "--config", path, "task", "attach", id); code != 0 {
		t.Fatalf("attach exit = %d (%s)", code, errOut)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "agent attach herder-"+id) {
		t.Errorf("attach should target the live session, got %q", joined)
	}
}
