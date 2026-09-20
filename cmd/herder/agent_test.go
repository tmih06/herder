package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
)

// writeFakeBins installs fake `herdr` and `docker` CLIs on PATH for agent
// launch and intervention tests. The herdr fake records calls, tracks
// started sessions in STATE markers, answers `agent get` with JSON whose
// status comes from STATE/status-<session> (default "working"), serves
// `agent read` from STATE/read-<session>, closes panes via `pane close`,
// and logs `notification show`; it re-reads STATE/herdrmode on every call
// so setHerdrMode can arm "send-fails" or "start-fails" mid-test.
// dockerMode selects the inspect outcome ("ready", "stopped", or
// "missing"); the fake tracks STATE/docker-status so pause/unpause/start
// move the container between running, paused, and stopped.
func writeFakeBins(t *testing.T, dockerMode string) string {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	t.Setenv("STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	herdr := `#!/bin/sh
echo "$@" >> "$STATE/calls"
mode=$(cat "$STATE/herdrmode" 2>/dev/null)
case "$1 $2" in
"agent start")
  [ "$mode" = "start-fails" ] && { echo "start refused" >&2; exit 1; }
  echo "{\"result\":{\"agent\":{\"pane_id\":\"w9:p-$3\",\"workspace_id\":\"w9\"}}}"
  touch "$STATE/started-$3"
  ;;
"agent send")
  [ "$mode" = "send-fails" ] && { echo "send refused" >&2; exit 1; }
  printf '%s' "$4" >> "$STATE/prompt-$3"
  ;;
"agent get")
  [ -f "$STATE/started-$3" ] || { echo "agent_not_found" >&2; exit 1; }
  st=$(cat "$STATE/status-$3" 2>/dev/null || true); [ -z "$st" ] && st=working
  echo "{\"result\":{\"agent\":{\"agent\":\"codex\",\"agent_status\":\"$st\",\"pane_id\":\"w9:p-$3\",\"workspace_id\":\"w9\"}}}"
  ;;
"agent read")
  [ -f "$STATE/started-$3" ] || { echo "agent_not_found" >&2; exit 1; }
  text=$(cat "$STATE/read-$3" 2>/dev/null || true); [ -z "$text" ] && text="agent output tail"
  printf '%s\n' "{\"result\":{\"read\":{\"text\":\"$text\",\"pane_id\":\"w9:p-$3\"}}}"
  ;;
"pane close")
  sess=${3#w9:p-}
  rm -f "$STATE/started-$sess"
  ;;
"notification show")
  echo "$3 ${4:-} ${5:-}" >> "$STATE/notifications"
  ;;
*) echo "unexpected herdr: $@" >&2; exit 1 ;;
esac
`
	//nolint:gosec // G306: temp-dir test fake needs the owner-exec bit to run;
	// file and parent dir are owner-only, never a real install path.
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(herdr), 0o700); err != nil {
		t.Fatal(err)
	}
	var docker string
	switch dockerMode {
	case "ready", "stopped":
		initial := "running"
		if dockerMode == "stopped" {
			initial = "exited"
		}
		docker = `#!/bin/sh
st=$(cat "$STATE/docker-status" 2>/dev/null || true); [ -z "$st" ] && st=` + initial + `
case "$1" in
"inspect")
  if [ "$2" = "--format" ]; then
    [ "$st" = "missing" ] && { echo "Error: No such object: $4" >&2; exit 1; }
    echo "$st"; exit 0
  fi
  [ "$st" = "missing" ] && { echo "Error: No such object: $2" >&2; exit 1; }
  cat <<EOF
[{"Id":"abc123","Name":"/herder-task","Config":{"Image":"golang:1.22-bookworm"},"State":{"Status":"$st"},"Mounts":[{"Source":"/tmp/w","Destination":"/workspace"}]}]
EOF
  ;;
"pause")   [ "$st" = "missing" ] && { echo "Error: No such container" >&2; exit 1; }; echo paused > "$STATE/docker-status" ;;
"unpause") [ "$st" = "missing" ] && { echo "Error: No such container" >&2; exit 1; }; echo running > "$STATE/docker-status" ;;
"start")   [ "$st" = "missing" ] && { echo "Error: No such container" >&2; exit 1; }; echo running > "$STATE/docker-status" ;;
*) echo "unexpected docker: $@" >&2; exit 1 ;;
esac
`
	default:
		docker = `#!/bin/sh
echo "Error: No such object: $2" >&2
exit 1
`
	}
	//nolint:gosec // G306: temp-dir test fake needs the owner-exec bit to run;
	// file and parent dir are owner-only, never a real install path.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o700); err != nil {
		t.Fatal(err)
	}
	return state
}

// setHerdrMode arms a fake-herdr failure mode for the next call: the
// script re-reads STATE/herdrmode every invocation, so a test can flip
// "send-fails" or "start-fails" on after a successful launch.
func setHerdrMode(t *testing.T, state, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "herdrmode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
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

// bindSession records a task-sandbox-session link directly in the store so
// a test can stage crash-recovery state (a bound session without a prior
// `task start`). When live is true it also writes the fake-herdr started
// marker so `agent get` answers live for that session.
func bindSession(t *testing.T, cfgPath, state, id string, live bool) {
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
	session := agent.SessionName(id)
	if err := store.SetBinding(id, sandbox.ContainerName(id), session); err != nil {
		t.Fatal(err)
	}
	if live {
		if err := os.WriteFile(filepath.Join(state, "started-"+session), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
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

// TestTaskStartStoppedSandbox refuses to launch into a container that
// exists but is not running: inspect succeeds, yet the task stays QUEUED
// with no session binding and no agent.started event.
func TestTaskStartStoppedSandbox(t *testing.T) {
	writeFakeBins(t, "stopped")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "not ready") || !strings.Contains(errOut, "exited") {
		t.Errorf("stderr should report the sandbox status, got %q", errOut)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, unwanted := range []string{"RUNNING", "agent.started"} {
		if strings.Contains(inspect, unwanted) {
			t.Errorf("inspect should not show %q, got:\n%s", unwanted, inspect)
		}
	}
	// The refused launch binds nothing, so attach must find no session.
	if !strings.Contains(inspect, "session: \n") {
		t.Errorf("inspect should show an empty session binding, got:\n%s", inspect)
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
// on the RUNNING task reuses the live session instead of orphaning a pane,
// and does NOT re-seed — injecting the contract into a mid-work agent
// would corrupt it.
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
	// The RUNNING task's live session is reused but not re-seeded: the only
	// send is the seed inside the first start.
	if n := strings.Count(string(calls), "agent send"); n != 1 {
		t.Errorf("running task should not be re-seeded (1 send), got %d in %q", n, calls)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"RUNNING", "reused"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}
}

// TestTaskStartQueuedReseedsLiveSession proves crash recovery: a QUEUED
// task whose bound session still answers is re-seeded with the task
// contract instead of launching a second pane.
func TestTaskStartQueuedReseedsLiveSession(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	bindSession(t, path, state, id, true)

	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 0 {
		t.Fatalf("start exit = %d (%s)", code, errOut)
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if n := strings.Count(string(calls), "agent start"); n != 0 {
		t.Errorf("live session should be reused (0 starts), got %d in %q", n, calls)
	}
	if n := strings.Count(string(calls), "agent send"); n != 1 {
		t.Errorf("queued task should be re-seeded (1 send), got %d in %q", n, calls)
	}
	prompt, err := os.ReadFile(filepath.Join(state, "prompt-herder-"+id))
	if err != nil {
		t.Fatalf("re-seeded session should have a prompt file: %v", err)
	}
	for _, want := range []string{"acme/web#7", "herder/7", "go test ./..."} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("re-seeded prompt should contain %q, got:\n%s", want, prompt)
		}
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 || !strings.Contains(inspect, "RUNNING") || !strings.Contains(inspect, "reused") {
		t.Errorf("inspect should show RUNNING plus the reuse event, got:\n%s", inspect)
	}
}

// TestTaskStartFailedLaunchLeavesNoBinding proves a Herdr-side refusal is
// an honest FAILED task: agent.start_failed names the reason and no
// session binding survives for attach to resolve.
func TestTaskStartFailedLaunchLeavesNoBinding(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	setHerdrMode(t, state, "start-fails")

	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "start refused") {
		t.Errorf("stderr should carry the Herdr refusal, got %q", errOut)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"FAILED", "agent.start_failed", "reason", "start refused"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}
	if !strings.Contains(inspect, "session: \n") {
		t.Errorf("failed launch should leave an empty session binding, got:\n%s", inspect)
	}
	// A refused start should never reach the prompt send.
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Contains(string(calls), "agent send") {
		t.Errorf("failed start should not send a prompt, got %q", calls)
	}
}

// TestTaskStartFailedReseedKeepsLiveBinding proves the reuse path fails
// honestly without orphaning the pane: the live session still answers but
// the re-seed send is refused, so the task goes FAILED while the session
// binding survives for `task attach` to land on the live pane.
func TestTaskStartFailedReseedKeepsLiveBinding(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	bindSession(t, path, state, id, true)
	setHerdrMode(t, state, "send-fails")

	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("start exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "send refused") {
		t.Errorf("stderr should carry the Herdr refusal, got %q", errOut)
	}
	// The still-live session was reused, not relaunched: no start at all.
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Contains(string(calls), "agent start") {
		t.Errorf("failed re-seed should not relaunch the live session, got %q", calls)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"FAILED", "agent.start_failed", "reason", "send refused"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}
	// The key assertion: the live session's binding survives so attach can
	// still resolve it, and the sandbox half of the link stays too.
	if !strings.Contains(inspect, "session: herder-"+id) {
		t.Errorf("failed re-seed should keep the live session binding, got:\n%s", inspect)
	}
	if !strings.Contains(inspect, "sandbox: herder-"+id) {
		t.Errorf("sandbox binding should survive the failed re-seed, got:\n%s", inspect)
	}
}

// TestTaskStartSendFailureKeepsLiveBinding proves a send-side failure
// after a successful launch is not an orphan: the pane exists, so the
// optimistic binding survives the launch failure's liveness probe and the
// task fails with the link intact for attach and a later re-seed.
func TestTaskStartSendFailureKeepsLiveBinding(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	setHerdrMode(t, state, "send-fails")

	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("start exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "send refused") {
		t.Errorf("stderr should carry the Herdr refusal, got %q", errOut)
	}
	// The pane was created before the send failed: one start, one send.
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if n := strings.Count(string(calls), "agent start"); n != 1 {
		t.Errorf("send failure follows a successful start (1 start), got %d in %q", n, calls)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"FAILED", "agent.start_failed", "send refused"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}
	if !strings.Contains(inspect, "session: herder-"+id) {
		t.Errorf("live pane's binding should survive the send failure, got:\n%s", inspect)
	}
}

// TestTaskStartFailedLaunchClearsDeadBinding proves a stale binding to a
// dead session is cleaned up: `agent get` finds no live session so start
// falls through to launch, the launch is refused, and the dead session's
// binding is cleared while the sandbox link stays for recovery.
func TestTaskStartFailedLaunchClearsDeadBinding(t *testing.T) {
	state := writeFakeBins(t, "ready")
	path := writeTestConfig(t)
	id := queueTask(t, path)
	bindSession(t, path, state, id, false)
	setHerdrMode(t, state, "start-fails")

	code, _, errOut := runCmd(t, "--config", path, "task", "start", id)
	if code != 1 {
		t.Errorf("start exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "start refused") {
		t.Errorf("stderr should carry the Herdr refusal, got %q", errOut)
	}
	code, inspect, _ := runCmd(t, "--config", path, "task", "inspect", id)
	if code != 0 {
		t.Fatalf("inspect exit = %d, want 0", code)
	}
	for _, want := range []string{"FAILED", "agent.start_failed", "reason", "start refused"} {
		if !strings.Contains(inspect, want) {
			t.Errorf("inspect should show %q, got:\n%s", want, inspect)
		}
	}
	// The dead session's stale binding is cleared so attach cannot land on
	// a dead pane; the sandbox half of the link stays for recovery.
	if !strings.Contains(inspect, "session: \n") {
		t.Errorf("failed launch should clear the dead session binding, got:\n%s", inspect)
	}
	if !strings.Contains(inspect, "sandbox: herder-"+id) {
		t.Errorf("sandbox binding should survive the session clear, got:\n%s", inspect)
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
