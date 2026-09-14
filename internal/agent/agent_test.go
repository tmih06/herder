package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/config"
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
			"codex-default": {Kind: "codex", Timeout: "2h",
				Resources: map[string]string{"cpu": "2", "memory": "4Gi"}},
			"claude-review": {Kind: "claude"},
		},
	}
}

func testTask() tasks.Task {
	return tasks.Task{
		ID: "task_abc123", SourceProvider: "github", SourceRef: "acme/web#7",
		Status: tasks.Queued, Repository: "acme/web",
		AgentProfile: "codex-default", BranchName: "herder/7",
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
// goal, issue metadata, repo instructions, allowed ops, completion reqs.
func TestBuildPrompt(t *testing.T) {
	prompt := BuildPrompt(PromptInput{
		Task: testTask(), AgentKind: "codex", ProfileName: "codex-default",
		Repository:       "acme/web",
		Repo:             testConfig().Repositories["acme/web"],
		RepoInstructions: "Always run gofmt.",
		SandboxID:        "herder-task_abc123",
		Workspace:        "/state/sandboxes/task_abc123",
	})
	for _, want := range []string{
		"acme/web#7", "herder/7", "task_abc123", "codex",
		"Always run gofmt.", "go test ./...",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should contain %q, got:\n%s", want, prompt)
		}
	}
	lower := strings.ToLower(prompt)
	for _, want := range []string{"goal", "allowed", "complet"} {
		if !strings.Contains(lower, want) {
			t.Errorf("prompt should section %q, got:\n%s", want, prompt)
		}
	}
}

// TestBuildPromptWithoutInstructions still seeds a usable prompt: the agent
// must get completion requirements even when the repo has no AGENTS.md.
func TestBuildPromptWithoutInstructions(t *testing.T) {
	prompt := BuildPrompt(PromptInput{
		Task: testTask(), AgentKind: "codex", ProfileName: "codex-default",
		Repository: "acme/web", Repo: testConfig().Repositories["acme/web"],
	})
	if !strings.Contains(prompt, "go test ./...") {
		t.Errorf("prompt should carry validation commands, got:\n%s", prompt)
	}
}

// TestSessionNameIsStable proves the Herdr session name derives
// deterministically from the task id for idempotent relaunch.
func TestSessionNameIsStable(t *testing.T) {
	if got := SessionName("task_abc123"); got != "herder-task_abc123" {
		t.Errorf("session = %q, want herder-task_abc123", got)
	}
	if SessionName("task_abc123") != SessionName("task_abc123") {
		t.Error("session name must be deterministic")
	}
}

// TestStartArgv proves the host-visible wrapper stays attributable: the
// HERDR_AGENT env rides on the Herdr start call and the wrapper names the
// exact sandbox container and agent binary (SPEC section 24).
func TestStartArgv(t *testing.T) {
	var calls [][]string
	l := &Launcher{
		HerdrBin: "herdr",
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(args) > 1 && args[1] == "start" {
				return RunResult{ExitCode: 0, Stdout: `{"result":{"agent":{
					"pane_id":"w5:p7","workspace_id":"w5"}}}`}, nil
			}
			return RunResult{ExitCode: 0, Stdout: `{}`}, nil
		},
	}
	res, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_abc123", AgentKind: "codex",
		Workspace: "/state/sandboxes/task_abc123",
		Container: "herder-task_abc123", Prompt: "Do the thing.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("start should run agent start + agent send, got %v", calls)
	}
	start := strings.Join(calls[0], " ")
	for _, want := range []string{
		"agent start herder-task_abc123",
		"HERDR_AGENT=codex",
		"docker exec -it herder-task_abc123 codex",
	} {
		if !strings.Contains(start, want) {
			t.Errorf("start argv should contain %q, got %q", want, start)
		}
	}
	send := strings.Join(calls[1], " ")
	if !strings.Contains(send, "agent send herder-task_abc123") {
		t.Errorf("second call should seed the prompt, got %q", send)
	}
	if !strings.Contains(send, "Do the thing.") {
		t.Errorf("send should carry the prompt, got %q", send)
	}
	if res.PaneID != "w5:p7" || res.WorkspaceID != "w5" {
		t.Errorf("result = %+v, want pane w5:p7 workspace w5", res)
	}
}

// TestStartRefusesUnknownKind proves an unknown agent kind fails fast with
// a clear error instead of launching something unaccounted.
func TestStartRefusesUnknownKind(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			t.Error("unknown kind must fail before any subprocess")
			return RunResult{}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "mystery",
		Workspace: "/w", Container: "c", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Errorf("should name the unknown kind, got %v", err)
	}
}

// TestStartSurfacesHerdrFailure proves a failed Herdr launch is an error,
// so the caller fails the task instead of pretending it runs.
func TestStartSurfacesHerdrFailure(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			return RunResult{ExitCode: 1, Stderr: "no such server"}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Workspace: "/w", Container: "c", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "no such server") {
		t.Errorf("should surface herdr stderr, got %v", err)
	}
}

// TestParseStartResult extracts pane/workspace ids for the durable event.
func TestParseStartResult(t *testing.T) {
	pane, workspace := ParseStartResult(`{"id":"cli:agent:start","result":{"agent":{
		"pane_id":"w5:p7","workspace_id":"w5","name":"herder-task_x"}}}`)
	if pane != "w5:p7" || workspace != "w5" {
		t.Errorf("got pane %q workspace %q, want w5:p7/w5", pane, workspace)
	}
	if pane, _ := ParseStartResult("not json"); pane != "" {
		t.Errorf("unparseable output should yield empty ids, got %q", pane)
	}
}

// TestAttachArgv proves attach targets the live session through the real
// agent (glass-box), not a log replay.
func TestAttachArgv(t *testing.T) {
	argv := AttachArgv("herder-task_abc123")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "agent attach herder-task_abc123") {
		t.Errorf("attach should target the session, got %q", joined)
	}
}

// TestIsLive maps `agent get` outcomes to liveness without parsing JSON.
func TestIsLive(t *testing.T) {
	live := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			return RunResult{ExitCode: 0, Stdout: `{}`}, nil
		},
	}
	if !live.IsLive(context.Background(), "herder-task_x") {
		t.Error("exit 0 should mean live")
	}
	dead := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			return RunResult{ExitCode: 1, Stderr: "agent_not_found"}, nil
		},
	}
	if dead.IsLive(context.Background(), "herder-task_x") {
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
