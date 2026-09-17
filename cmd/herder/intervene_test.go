package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// readTask opens the test store and returns the current task row.
func readTask(t *testing.T, cfgPath, id string) tasks.Task {
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
	return task
}

// startTask runs the full provision-free start path against the fakes:
// queue, then `task start` into the ready fake container.
func startTask(t *testing.T, cfg string) (id, session string) {
	t.Helper()
	id = queueTask(t, cfg)
	code, out, errOut := runCmd(t, "--config", cfg, "task", "start", id)
	if code != 0 {
		t.Fatalf("task start exit = %d (%s%s)", code, out, errOut)
	}
	return id, agent.SessionName(id)
}

// TestTaskTellReachesAgent proves the message lands on the live session
// promptly and durably: the fake herdr received it and agent.prompted
// sits in the task's event history.
func TestTaskTellReachesAgent(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, _, errOut := runCmd(t, "--config", cfg, "task", "tell", id,
		"Do not change the public API.")
	if code != 0 {
		t.Fatalf("task tell exit = %d (%s)", code, errOut)
	}
	prompt, err := os.ReadFile(filepath.Join(state, "prompt-"+session))
	if err != nil {
		t.Fatalf("agent never received the message: %v", err)
	}
	if !strings.Contains(string(prompt), "Do not change the public API.") {
		t.Errorf("message missing from agent input, got:\n%s", prompt)
	}
	_, inspect, _ := runCmd(t, "--config", cfg, "task", "inspect", id)
	if !strings.Contains(inspect, "agent.prompted") {
		t.Errorf("agent.prompted event missing, got:\n%s", inspect)
	}
}

// TestTaskTellUnblocksBlocked proves the human answer moves a BLOCKED
// task back toward working: the send lands and the task returns to
// RUNNING.
func TestTaskTellUnblocksBlocked(t *testing.T) {
	writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, _ := startTask(t, cfg)
	if code, _, errOut := runCmd(t, "--config", cfg, "task", "transition", id, "BLOCKED"); code != 0 {
		t.Fatalf("block task exit = %d (%s)", code, errOut)
	}
	code, _, errOut := runCmd(t, "--config", cfg, "task", "tell", id, "yes, use the cache")
	if code != 0 {
		t.Fatalf("task tell exit = %d (%s)", code, errOut)
	}
	if got := readTask(t, cfg, id).Status; got != tasks.Running {
		t.Errorf("task after answer = %s, want RUNNING", got)
	}
}

// TestTaskTellNeedsSession refuses to send into nothing.
func TestTaskTellNeedsSession(t *testing.T) {
	writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id := queueTask(t, cfg)
	code, _, errOut := runCmd(t, "--config", cfg, "task", "tell", id, "hello")
	if code == 0 || !strings.Contains(errOut, "no agent session") {
		t.Errorf("tell without session = %d (%s), want refusal", code, errOut)
	}
}

// TestTaskLogsPrintsAgentOutput proves logs expose the live pane's recent
// output through Herdr, not a replayed event history.
func TestTaskLogsPrintsAgentOutput(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)
	if err := os.WriteFile(filepath.Join(state, "read-"+session),
		[]byte("compiling main.go\\ntests green"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmd(t, "--config", cfg, "task", "logs", id)
	if code != 0 {
		t.Fatalf("task logs exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "compiling main.go") {
		t.Errorf("logs should carry the agent's recent output, got:\n%s", out)
	}
}

// TestTaskPauseResume proves pause freezes the sandbox without killing
// the session and resume thaws both container and task state.
func TestTaskPauseResume(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, out, errOut := runCmd(t, "--config", cfg, "task", "pause", id)
	if code != 0 {
		t.Fatalf("task pause exit = %d (%s%s)", code, out, errOut)
	}
	if got := readTask(t, cfg, id).Status; got != tasks.Paused {
		t.Fatalf("task after pause = %s, want PAUSED", got)
	}
	if st, _ := os.ReadFile(filepath.Join(state, "docker-status")); string(st) != "paused\n" {
		t.Errorf("container not frozen, status = %q", st)
	}
	// The session survives the freeze: attach must still resolve it.
	if _, err := os.Stat(filepath.Join(state, "started-"+session)); err != nil {
		t.Errorf("pause killed the session: %v", err)
	}

	code, _, errOut = runCmd(t, "--config", cfg, "task", "resume", id)
	if code != 0 {
		t.Fatalf("task resume exit = %d (%s)", code, errOut)
	}
	if got := readTask(t, cfg, id).Status; got != tasks.Running {
		t.Errorf("task after resume = %s, want RUNNING", got)
	}
	if st, _ := os.ReadFile(filepath.Join(state, "docker-status")); string(st) != "running\n" {
		t.Errorf("container not thawed, status = %q", st)
	}
}

// TestTaskPauseNeedsRunning refuses to freeze a task with nothing live.
func TestTaskPauseNeedsRunning(t *testing.T) {
	writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id := queueTask(t, cfg)
	code, _, errOut := runCmd(t, "--config", cfg, "task", "pause", id)
	if code == 0 || !strings.Contains(errOut, "cannot pause") {
		t.Errorf("pause on QUEUED = %d (%s), want refusal", code, errOut)
	}
}

// TestTaskStopEndsSession proves stop closes the pane, clears the dead
// binding, and cancels the task while the sandbox stays for inspection.
func TestTaskStopEndsSession(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, _, errOut := runCmd(t, "--config", cfg, "task", "stop", id)
	if code != 0 {
		t.Fatalf("task stop exit = %d (%s)", code, errOut)
	}
	task := readTask(t, cfg, id)
	if task.Status != tasks.Cancelled {
		t.Errorf("task after stop = %s, want CANCELLED", task.Status)
	}
	if task.AgentSessionID != "" {
		t.Errorf("dead session binding should clear, got %q", task.AgentSessionID)
	}
	if _, err := os.Stat(filepath.Join(state, "started-"+session)); !os.IsNotExist(err) {
		t.Errorf("session pane should be closed, stat err = %v", err)
	}
	_, inspect, _ := runCmd(t, "--config", cfg, "task", "inspect", id)
	if !strings.Contains(inspect, "agent.stopped") {
		t.Errorf("agent.stopped event missing, got:\n%s", inspect)
	}
}

// TestTaskRetryFreshAttempt proves retry closes the old session, bumps
// the attempt counter, and relaunches on the same sandbox with the
// attempt number in the re-seeded prompt.
func TestTaskRetryFreshAttempt(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, out, errOut := runCmd(t, "--config", cfg, "task", "retry", id)
	if code != 0 {
		t.Fatalf("task retry exit = %d (%s%s)", code, out, errOut)
	}
	task := readTask(t, cfg, id)
	if task.Attempt != 2 {
		t.Errorf("attempt after retry = %d, want 2", task.Attempt)
	}
	if task.Status != tasks.Running {
		t.Errorf("task after retry = %s, want RUNNING", task.Status)
	}
	prompt, err := os.ReadFile(filepath.Join(state, "prompt-"+session))
	if err != nil || !strings.Contains(string(prompt), "attempt 2") {
		t.Errorf("relaunched prompt should carry attempt 2, got: %s (%v)", prompt, err)
	}
	_, inspect, _ := runCmd(t, "--config", cfg, "task", "inspect", id)
	if !strings.Contains(inspect, "task.retry") || !strings.Contains(inspect, "RETRYING") {
		t.Errorf("retry should leave task.retry plus the RETRYING transition, got:\n%s", inspect)
	}
}

// TestTaskHandoffPreservesWork proves the worker change keeps the same
// task, sandbox, and history: profile swap, attempt bump, handed_off
// event, and a prompt telling the new agent to continue existing work.
func TestTaskHandoffPreservesWork(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, out, errOut := runCmd(t, "--config", cfg, "task", "handoff", "--agent", "claude-reviewer", id)
	if code != 0 {
		t.Fatalf("task handoff exit = %d (%s%s)", code, out, errOut)
	}
	task := readTask(t, cfg, id)
	if task.AgentProfile != "claude-reviewer" {
		t.Errorf("profile after handoff = %s, want claude-reviewer", task.AgentProfile)
	}
	if task.Attempt != 2 || task.Status != tasks.Running {
		t.Errorf("handoff should bump attempt and relaunch, got attempt=%d status=%s",
			task.Attempt, task.Status)
	}
	if task.SandboxID == "" {
		t.Error("handoff must keep the sandbox binding")
	}
	prompt, err := os.ReadFile(filepath.Join(state, "prompt-"+session))
	if err != nil || !strings.Contains(string(prompt), "handed off from agent profile codex-default") {
		t.Errorf("new agent's prompt should name the handoff, got: %s (%v)", prompt, err)
	}
	_, inspect, _ := runCmd(t, "--config", cfg, "task", "inspect", id)
	if !strings.Contains(inspect, "agent.handed_off") {
		t.Errorf("agent.handed_off event missing, got:\n%s", inspect)
	}
}

// TestTaskHandoffUnknownProfileKeepsWorker proves a typo'd target never
// touches the live session: the old agent keeps running and the task
// stays RUNNING.
func TestTaskHandoffUnknownProfileKeepsWorker(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)

	code, _, errOut := runCmd(t, "--config", cfg, "task", "handoff", "--agent", "bogus", id)
	if code == 0 || !strings.Contains(errOut, "unknown") {
		t.Fatalf("handoff to bogus profile = %d (%s), want refusal", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(state, "started-"+session)); err != nil {
		t.Errorf("refused handoff killed the live session: %v", err)
	}
	if got := readTask(t, cfg, id); got.Status != tasks.Running || got.AgentProfile != "codex-default" {
		t.Errorf("task should be untouched, got %s/%s", got.Status, got.AgentProfile)
	}
}

// TestTaskRetryTerminalKeepsWorker proves a refused retry changes
// nothing: the live session survives and the task keeps its state.
func TestTaskRetryTerminalKeepsWorker(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, session := startTask(t, cfg)
	for _, s := range []string{"VALIDATING", "REVIEWING", "DELIVERING", "PR_OPEN", "DONE"} {
		if code, _, errOut := runCmd(t, "--config", cfg, "task", "transition", id, s); code != 0 {
			t.Fatalf("transition %s exit = %d (%s)", s, code, errOut)
		}
	}
	code, _, errOut := runCmd(t, "--config", cfg, "task", "retry", id)
	if code == 0 {
		t.Fatal("retry on DONE must fail")
	}
	if !strings.Contains(errOut, "illegal transition") {
		t.Errorf("refusal should name the illegal transition, got %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(state, "started-"+session)); err != nil {
		t.Errorf("refused retry killed the live session: %v", err)
	}
	if got := readTask(t, cfg, id); got.Status != tasks.Done || got.Attempt != 1 {
		t.Errorf("refused retry must change nothing, got %s attempt %d", got.Status, got.Attempt)
	}
}

// TestTaskPauseMissingContainerRefuses proves pause cannot claim a freeze
// on a sandbox that does not exist: the task stays RUNNING.
func TestTaskPauseMissingContainerRefuses(t *testing.T) {
	state := writeFakeBins(t, "ready")
	cfg := writeTestConfig(t)
	id, _ := startTask(t, cfg)
	if err := os.WriteFile(filepath.Join(state, "docker-status"), []byte("missing"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCmd(t, "--config", cfg, "task", "pause", id)
	if code == 0 {
		t.Fatal("pause on a missing container must fail")
	}
	if !strings.Contains(errOut, "cannot pause") {
		t.Errorf("refusal should explain the missing sandbox, got %q", errOut)
	}
	if got := readTask(t, cfg, id).Status; got != tasks.Running {
		t.Errorf("task after refused pause = %s, want RUNNING", got)
	}
}
