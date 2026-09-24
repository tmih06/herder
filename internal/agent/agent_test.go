package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// goal, issue metadata, repo instructions, allowed ops, completion reqs.
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
		Repo: testConfig().Repositories["acme/web"],
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

// fakeLookPath resolves every binary to a path inside dir so Start's
// shim step never needs a real docker install.
func fakeLookPath(dir string) func(string) (string, error) {
	return func(name string) (string, error) { return filepath.Join(dir, name), nil }
}

// startRespond scripts the full herdr 0.9.x launch flow: workspace create
// answers with pane ids, pane run records the shim argv, agent get reports
// the detected kind, rename binds the session, and prompt records the seed.
func startRespond(calls *[][]string) Runner {
	return func(_ context.Context, name string, args ...string) (RunResult, error) {
		*calls = append(*calls, append([]string{name}, args...))
		argv := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(argv, "workspace create"):
			return RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
		case strings.HasPrefix(argv, "agent get"):
			return RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
		case strings.HasPrefix(argv, "pane process-info"):
			return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
		}
		return RunResult{Stdout: `{}`}, nil
	}
}

// TestStartArgv proves the launch flow stays attributable: the workspace
// carries the session label and HERDR_AGENT env, the pane runs the kind
// shim into docker exec on the task container, and the prompt seeds the
// renamed session (SPEC sections 24, 26).
func TestStartArgv(t *testing.T) {
	dir := t.TempDir()
	lookPath := fakeLookPath(dir)
	var calls [][]string
	l := &Launcher{Runner: startRespond(&calls), LookPath: lookPath}
	res, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_abc123", AgentKind: "codex",
		Workspace: "/state/sandboxes/task_abc123",
		Container: "herder-task_abc123", ShimDir: filepath.Join(dir, "shims"),
		Prompt: "Do the thing.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	var create, run, rename, prompt string
	for _, call := range calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "workspace create"):
			create = joined
		case strings.Contains(joined, "pane run"):
			run = joined
		case strings.Contains(joined, "agent rename"):
			rename = joined
		case strings.Contains(joined, "agent prompt"):
			prompt = joined
		}
	}
	for _, want := range []string{
		"workspace create", "--label herder-task_abc123",
		"--cwd /state/sandboxes/task_abc123", "HERDR_AGENT=codex",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("workspace create should contain %q, got %q", want, create)
		}
	}
	shim := filepath.Join(dir, "shims", "codex")
	for _, want := range []string{
		"pane run w5:p7", shim + " exec -it herder-task_abc123 codex",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("pane run should contain %q, got %q", want, run)
		}
	}
	if !strings.Contains(rename, "agent rename w5:p7 herder-task_abc123") {
		t.Errorf("rename should bind the session name, got %q", rename)
	}
	if !strings.Contains(prompt, "agent prompt herder-task_abc123") ||
		!strings.Contains(prompt, "Do the thing.") {
		t.Errorf("prompt should seed the session, got %q", prompt)
	}
	if res.PaneID != "w5:p7" || res.WorkspaceID != "w5" {
		t.Errorf("result = %+v, want pane w5:p7 workspace w5", res)
	}
}

// TestStartClosesPaneOnFailure proves a launch that dies after the pane
// exists cleans it up: a detection timeout must not leak the workspace.
func TestStartClosesPaneOnFailure(t *testing.T) {
	dir := t.TempDir()
	lookPath := fakeLookPath(dir)
	var calls [][]string
	l := &Launcher{
		LookPath:      lookPath,
		DetectTimeout: 50 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			calls = append(calls, append([]string{name}, args...))
			argv := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(argv, "workspace create"):
				return RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
			case strings.HasPrefix(argv, "agent get"):
				// Detection never reports the kind: the wait times out.
				return RunResult{ExitCode: 1, Stderr: "agent_not_found"}, nil
			}
			return RunResult{}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Workspace: "/w", Container: "c", ShimDir: filepath.Join(dir, "shims"), Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "not detected") {
		t.Fatalf("detection timeout should fail the launch, got %v", err)
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

// TestSendPromptRetriesNotReady proves the first prompt absorbs Herdr's
// detection-to-ready gap instead of failing the launch.
func TestSendPromptRetriesNotReady(t *testing.T) {
	n := 0
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			argv := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
			}
			n++
			if n < 3 {
				return RunResult{ExitCode: 1, Stderr: "agent_not_ready"}, nil
			}
			return RunResult{}, nil
		},
	}
	if err := l.SendPrompt(context.Background(), "herder-task_x", "go"); err != nil {
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
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			argv := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"fish"}]}}}`}, nil
			case strings.HasPrefix(argv, "agent prompt"):
				t.Error("prompt must never reach a dead agent's pane")
			}
			return RunResult{}, nil
		},
	}
	err := l.SendPrompt(context.Background(), "herder-task_x", "rm -rf /")
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("dead agent prompt = %v, want ErrSessionGone", err)
	}
}

// TestReadReturnsRawText proves agent read passes the pane text through:
// herdr 0.9.x prints the tail directly, no JSON envelope.
func TestReadReturnsRawText(t *testing.T) {
	l := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			return RunResult{Stdout: "line one\nline two\n"}, nil
		},
	}
	text, err := l.Read(context.Background(), "herder-task_x", 10)
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
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			t.Error("unknown kind must fail before any subprocess")
			return RunResult{}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "mystery",
		Workspace: "/w", Container: "c", ShimDir: "/s", Prompt: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Errorf("should name the unknown kind, got %v", err)
	}
}

// TestStartSurfacesHerdrFailure proves a failed workspace create is an
// error, so the caller fails the task instead of pretending it runs.
func TestStartSurfacesHerdrFailure(t *testing.T) {
	dir := t.TempDir()
	lookPath := fakeLookPath(dir)
	l := &Launcher{
		LookPath: lookPath,
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			return RunResult{ExitCode: 1, Stderr: "no such server"}, nil
		},
	}
	_, err := l.Start(context.Background(), StartInput{
		Session: "herder-task_x", AgentKind: "codex",
		Workspace: "/w", Container: "c", ShimDir: filepath.Join(dir, "shims"), Prompt: "p",
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

// TestEnsureShim proves the shim is a symlink to docker named after the
// kind, recreated when the link target drifts.
func TestEnsureShim(t *testing.T) {
	dir := t.TempDir()
	lookPath := fakeLookPath(dir)
	shim, err := EnsureShim(filepath.Join(dir, "shims"), "codex", lookPath)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(shim)
	if err != nil {
		t.Fatalf("shim should be a symlink: %v", err)
	}
	if target != filepath.Join(dir, "docker") {
		t.Errorf("shim target = %q, want the fake docker", target)
	}
	// A wrong link is replaced.
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", shim); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureShim(filepath.Join(dir, "shims"), "codex", lookPath); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(shim); target != filepath.Join(dir, "docker") {
		t.Errorf("stale shim not repaired, target = %q", target)
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

// TestIsLive maps `agent get` plus `pane process-info` to real liveness:
// a named record alone is not proof — the shim must still be foreground.
func TestIsLive(t *testing.T) {
	live := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			argv := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				return RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w9:p1"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
			}
			return RunResult{}, nil
		},
	}
	if !live.IsLive(context.Background(), "herder-task_x") {
		t.Error("running shim should mean live")
	}
	exited := &Launcher{
		Runner: func(_ context.Context, name string, args ...string) (RunResult, error) {
			argv := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(argv, "agent get"):
				// The stale record still answers after the agent exits.
				return RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w9:p1"}}}`}, nil
			case strings.HasPrefix(argv, "pane process-info"):
				return RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"fish"}]}}}`}, nil
			}
			return RunResult{}, nil
		},
	}
	if exited.IsLive(context.Background(), "herder-task_x") {
		t.Error("stale record with no shim should mean not live")
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
