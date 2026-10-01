package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/machine"
	"github.com/tmih06/herder/internal/tasks"
)

func testConfig() *config.Config {
	return &config.Config{
		Repositories: map[string]config.RepositoryConfig{
			"acme/web": {
				Agent: config.RepoAgentConfig{Default: "codex-default"},
				Validation: config.ValidationConfig{
					Commands: []string{"go test ./..."},
				},
				Delivery: config.DeliveryConfig{CreatePR: true},
			},
		},
		Agents: map[string]config.AgentConfig{
			"codex-default": {
				Kind: "codex", Timeout: "2h",
				Resources: map[string]string{"cpu": "2", "memory": "4Gi"},
			},
			"claude-review": {Kind: "claude"},
		},
	}
}

func testTask() tasks.Task {
	return tasks.Task{
		ID: "task_abc123", SourceProvider: "github", SourceRef: "acme/web#7",
		Status: tasks.Queued, Repository: "acme/web",
		AgentProfile: "codex-default", BranchName: "herder/7",
		Goal: "Fix OAuth refresh race\n\nRefresh tokens rotate mid-request.",
	}
}

// TestResolveProfile proves per-repo selection with override and fallback:
// explicit flag wins, then the task profile, then the repo default.
func TestResolveProfile(t *testing.T) {
	cfg := testConfig()
	name, prof, ok := ResolveProfile(cfg, "acme/web", "codex-default", "")
	if !ok || name != "codex-default" || prof.Kind != "codex" {
		t.Errorf("task profile should resolve, got %q %+v %v", name, prof, ok)
	}
	name, prof, ok = ResolveProfile(cfg, "acme/web", "codex-default", "claude-review")
	if !ok || name != "claude-review" || prof.Kind != "claude" {
		t.Errorf("override should win, got %q %+v %v", name, prof, ok)
	}
	name, prof, ok = ResolveProfile(cfg, "acme/web", "", "")
	if !ok || name != "codex-default" || prof.Kind != "codex" {
		t.Errorf("empty task profile should fall back to repo default, got %q %+v %v", name, prof, ok)
	}
	if name, _, ok := ResolveProfile(cfg, "acme/web", "missing", ""); ok {
		t.Errorf("unknown profile should not resolve, got %q", name)
	}
	if _, _, ok := ResolveProfile(cfg, "unknown/repo", "", ""); ok {
		t.Error("unknown repo should not resolve")
	}
}

// TestBuildPrompt proves the seeded prompt carries every acceptance item:
// goal, issue metadata, repo instructions, and the working rules.
func TestBuildPrompt(t *testing.T) {
	prompt := BuildPrompt(PromptInput{
		Task: testTask(), AgentKind: "codex", ProfileName: "codex-default",
		Repo:             testConfig().Repositories["acme/web"],
		RepoInstructions: "Always run gofmt.",
		SandboxID:        "herder-task_abc123",
		Workspace:        "/state/sandboxes/task_abc123",
	})
	for _, want := range []string{
		"acme/web#7", "herder/7", "task_abc123", "codex",
		"Fix OAuth refresh race", "Refresh tokens rotate mid-request.",
		"Always run gofmt.", "herder-task_abc123",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should contain %q, got:\n%s", want, prompt)
		}
	}
	lower := strings.ToLower(prompt)
	for _, want := range []string{"goal", "allowed operations", "completion requirements", "go test ./..."} {
		if !strings.Contains(lower, want) {
			t.Errorf("prompt should section %q, got:\n%s", want, prompt)
		}
	}
}

// TestBuildPromptWithoutInstructions still seeds a usable prompt: the agent
// must get the completion contract even when the repo has no AGENTS.md.
func TestBuildPromptWithoutInstructions(t *testing.T) {
	prompt := BuildPrompt(PromptInput{
		Task: testTask(), AgentKind: "codex", ProfileName: "codex-default",
		Repo: testConfig().Repositories["acme/web"],
	})
	for _, want := range []string{"clean working tree", "Report done only when"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should carry the completion contract %q, got:\n%s", want, prompt)
		}
	}
}

// TestSessionNameIsStable proves the Herdr session name derives
// deterministically from the task id for idempotent relaunch.
func TestSessionNameIsStable(t *testing.T) {
	if got := SessionName("task_abc123"); got != "herder-task_abc123" {
		t.Errorf("session = %q, want herder-task_abc123", got)
	}
}

// stripMachine drops the `--machine <label>` forwarding prefix so scripted
// runners can match the herdr subcommand; host-side calls (notification
// show) carry no prefix and pass through.
func stripMachine(args []string) []string {
	if len(args) >= 2 && args[0] == "--machine" {
		return args[2:]
	}
	return args
}

// TestStartClosesPaneOnFailure proves a failed `agent start` does not leak
// the workspace's pane: closing it also removes the workspace.
func TestStartClosesPaneOnFailure(t *testing.T) {
	var calls [][]string
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			calls = append(calls, append([]string{name}, args...))
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "workspace create"):
				return machine.RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
			case strings.HasPrefix(argv, "agent start"):
				// Detection never classifies the agent: the native wait
				// times out (herdr 0.9.1: code "timeout").
				return machine.RunResult{ExitCode: 1, Stderr: `{"error":{"code":"timeout","message":"timed out waiting for agent startup"}}`}, nil
			}
			return machine.RunResult{}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Machine: "task_x", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("startup timeout should fail the launch, got %v", err)
	}
	var closed bool
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), "pane close w5:p7") {
			closed = true
		}
	}
	if !closed {
		t.Errorf("failed launch must close the pane, calls: %v", calls)
	}
}

// TestStartBlockedKeepsPane proves agent_not_ready is not a failed launch:
// the agent is live, blocked at a startup screen, and already owns the
// session name — the pane must survive for a human to unblock it.
func TestStartBlockedKeepsPane(t *testing.T) {
	var calls [][]string
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			calls = append(calls, append([]string{name}, args...))
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "workspace create"):
				return machine.RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
			case strings.HasPrefix(argv, "agent start"):
				return machine.RunResult{ExitCode: 1, Stderr: `{"error":{"code":"agent_not_ready","message":"agent herder-task_x is blocked during startup and is not ready for prompts"}}`}, nil
			}
			return machine.RunResult{}, nil
		},
	}
	res, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Machine: "task_x", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "agent_not_ready") {
		t.Fatalf("blocked startup should surface agent_not_ready, got %v", err)
	}
	if res.PaneID != "w5:p7" {
		t.Errorf("blocked start should still return the pane id, got %+v", res)
	}
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), "pane close") {
			t.Errorf("blocked startup must not kill the live agent, calls: %v", calls)
		}
	}
}

// TestStartNameTakenClearsStaleRecord proves a session name still held by a
// stale agent record is released once — `agent rename <name> --clear` —
// and the start retried, so relaunch converges on the deterministic name.
func TestStartNameTakenClearsStaleRecord(t *testing.T) {
	var calls [][]string
	starts := 0
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			calls = append(calls, append([]string{name}, args...))
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "workspace create"):
				return machine.RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
			case strings.HasPrefix(argv, "agent start"):
				starts++
				if starts == 1 {
					return machine.RunResult{ExitCode: 1, Stderr: `{"error":{"code":"agent_name_taken","message":"agent name herder-task_x is already used"}}`}, nil
				}
				return machine.RunResult{Stdout: `{}`}, nil
			case strings.HasPrefix(argv, "agent get"):
				// The pre-start lookup checks the stale name holder: gone.
				// Once the retried start succeeded, the session is live.
				if starts < 2 {
					return machine.RunResult{ExitCode: 1, Stderr: "agent_not_found"}, nil
				}
				return machine.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return machine.RunResult{Stdout: `{"result":{"process_info":{"foreground_process_group_id":4242,"shell_pid":100}}}`}, nil
			}
			return machine.RunResult{Stdout: `{}`}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Machine: "task_x", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("stale name should clear and retry, got %v", err)
	}
	var cleared bool
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), "agent rename herder-task_x --clear") {
			cleared = true
		}
	}
	if !cleared || starts != 2 {
		t.Errorf("name should be cleared once and start retried, cleared=%v starts=%d", cleared, starts)
	}
}

// TestSendPromptRetriesNotReady proves a prompt that meets an agent still
// reporting not-ready retries instead of failing the launch or re-seed.
func TestSendPromptRetriesNotReady(t *testing.T) {
	n := 0
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return machine.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return machine.RunResult{Stdout: `{"result":{"process_info":{"foreground_process_group_id":4242,"shell_pid":100}}}`}, nil
			}
			n++
			if n < 3 {
				return machine.RunResult{ExitCode: 1, Stderr: "agent_not_ready"}, nil
			}
			return machine.RunResult{}, nil
		},
	}
	if err := l.SendPrompt(context.Background(), "task_x", "herder-task_x", "go"); err != nil {
		t.Fatalf("transient agent_not_ready should retry, got %v", err)
	}
	if n != 3 {
		t.Errorf("prompt calls = %d, want 3", n)
	}
}

// TestSendPromptRefusesDeadAgent proves the prompt never reaches a pane
// whose agent exited: herdr would type the text into the pane's shell
// and execute it as host commands.
func TestSendPromptRefusesDeadAgent(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return machine.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				// Foreground group IS the shell: the agent exited.
				return machine.RunResult{Stdout: `{"result":{"process_info":{"foreground_process_group_id":100,"shell_pid":100}}}`}, nil
			case strings.HasPrefix(argv, "agent prompt"):
				t.Error("prompt must never reach a dead agent's pane")
			}
			return machine.RunResult{}, nil
		},
	}
	err := l.SendPrompt(context.Background(), "task_x", "herder-task_x", "rm -rf /")
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("dead agent prompt = %v, want ErrSessionGone", err)
	}
}

// TestReadReturnsRawText proves agent read passes the pane text through:
// herdr 0.9.x prints the tail directly, no JSON envelope.
func TestReadReturnsRawText(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			return machine.RunResult{Stdout: "line one\nline two\n"}, nil
		},
	}
	text, err := l.Read(context.Background(), "task_x", "herder-task_x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if text != "line one\nline two" {
		t.Errorf("read = %q, want raw tail", text)
	}
}

// TestStartRefusesUnknownKind proves an unknown agent kind fails fast with
// a clear error instead of launching something unaccounted.
func TestStartRefusesUnknownKind(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			t.Error("unknown kind must fail before any subprocess")
			return machine.RunResult{}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "mystery",
		Machine: "task_x", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Errorf("should name the unknown kind, got %v", err)
	}
}

// TestStartSurfacesHerdrFailure proves a failed workspace create is an
// error, so the caller fails the task instead of pretending it runs.
func TestStartSurfacesHerdrFailure(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			return machine.RunResult{ExitCode: 1, Stderr: "no such server"}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Machine: "task_x", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "no such server") {
		t.Errorf("should surface herdr stderr, got %v", err)
	}
}

// TestParseWorkspaceCreate extracts pane/workspace ids for the durable event.
func TestParseWorkspaceCreate(t *testing.T) {
	pane, workspace := parseWorkspaceCreate(`{"id":"cli:workspace:create","result":{
		"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`)
	if pane != "w5:p7" || workspace != "w5" {
		t.Errorf("got pane %q workspace %q, want w5:p7/w5", pane, workspace)
	}
	if pane, _ := parseWorkspaceCreate("not json"); pane != "" {
		t.Errorf("unparseable output should yield empty ids, got %q", pane)
	}
}

// TestIsLive maps `agent get` plus `pane process-info` to real liveness:
// a named record alone is not proof — the pane's foreground group must
// still differ from its shell pid.
func TestIsLive(t *testing.T) {
	live := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return machine.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w9:p1"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return machine.RunResult{Stdout: `{"result":{"process_info":{"foreground_process_group_id":4242,"shell_pid":100}}}`}, nil
			}
			return machine.RunResult{}, nil
		},
	}
	if !live.IsLive(context.Background(), "task_x", "herder-task_x") {
		t.Error("foreground agent should mean live")
	}
	exited := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			argv := strings.Join(stripMachine(args), " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				// The stale record still answers after the agent exits.
				return machine.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w9:p1"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				// The pane fell back to its shell: foreground == shell pid.
				return machine.RunResult{Stdout: `{"result":{"process_info":{"foreground_process_group_id":100,"shell_pid":100}}}`}, nil
			}
			return machine.RunResult{}, nil
		},
	}
	if exited.IsLive(context.Background(), "task_x", "herder-task_x") {
		t.Error("stale record with shell foreground should mean not live")
	}
	dead := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (machine.RunResult, error) {
			return machine.RunResult{ExitCode: 1, Stderr: "agent_not_found"}, nil
		},
	}
	if dead.IsLive(context.Background(), "task_x", "herder-task_x") {
		t.Error("missing agent should mean not live")
	}
}

// TestLoadRepoInstructions prefers AGENTS.md, falls back to CLAUDE.md, and
// stays silent when the checkout carries no instructions.
func TestLoadRepoInstructions(t *testing.T) {
	dir := t.TempDir()
	if got := LoadRepoInstructions(dir); got != "" {
		t.Errorf("empty workspace should load nothing, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Use tabs."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Run gofmt."), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadRepoInstructions(dir); got != "Run gofmt." {
		t.Errorf("AGENTS.md should win, got %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if got := LoadRepoInstructions(dir); got != "Use tabs." {
		t.Errorf("CLAUDE.md should back up AGENTS.md, got %q", got)
	}
}
