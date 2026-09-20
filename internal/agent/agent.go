// Package agent launches coding agents through Herdr into task sandboxes
// (issue #4; SPEC sections 16-18, 20-21, 24, 26): one Herdr agent session
// per running task, seeded with the task goal plus repository instructions
// and completion requirements, bound durably as task <-> sandbox <->
// session so a human can attach to the real agent instead of a spinner.
//
// Why: black-box runners hide the worker; Herder stays glass-box by keeping
// the agent in a real Herdr pane the host can watch, prompt, and attach.
// Approach: Stage 1 CLI orchestration (SPEC section 17) against the real
// Herdr 0.9.x surface — `workspace create` yields a shell pane, `pane run`
// starts a shim that enters the sandbox, Herdr detects the agent by the
// shim's process name, `agent rename` binds the deterministic session
// name, and `agent prompt` seeds the task contract. The shim is a symlink
// to the docker binary named after the agent kind: Herdr classifies the
// pane's foreground process by comm, so `<kind> exec -it <ctr> <cmd>`
// reads as the agent kind while actually running `docker exec` (SPEC
// section 26's shim-executable model). The Runner seam scripts
// subprocesses in tests; the socket API comes later.
// Inputs: task + repo/agent config + live sandbox (container + workspace).
// Flow: ResolveProfile -> BuildPrompt -> Launcher.Start records the session
// name; attach re-enters via AttachArgv. Unknown kinds fail before any
// subprocess so tasks fail with an event instead of hanging.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/textutil"
)

// instructionCap bounds repository instructions embedded in the prompt: the
// agent needs the working rules, not the entire handbook.
const instructionCap = 8000

// runTimeout bounds one Herdr CLI invocation: start/send/get are local
// socket calls and must fail fast instead of hanging a task launch.
const runTimeout = 2 * time.Minute

// agentCommands maps a configured agent kind to the binary invoked inside
// the sandbox wrapper. v0.1 covers the Herdr-supported CLI agents from the
// validated config allow-list; an unmapped kind is a caller-visible error.
var agentCommands = map[string]string{
	"codex":    "codex",
	"claude":   "claude",
	"opencode": "opencode",
	"gemini":   "gemini",
}

// CommandForKind reports the sandbox binary for an agent kind.
func CommandForKind(kind string) (string, bool) {
	cmd, ok := agentCommands[kind]
	return cmd, ok
}

// SessionName derives the deterministic Herdr session for a task.
// Purpose: relaunching the same task converges on one session instead of
// orphaning panes. Inputs: task id (already charset-safe). Returns the
// herder-<task> session name.
func SessionName(taskID string) string {
	return "herder-" + strings.ToLower(strings.TrimSpace(taskID))
}

// ResolveProfile selects the agent profile for a task: an explicit override
// wins, then the task's claimed profile, then the repository default.
// Purpose: per-repo policy with a fallback (acceptance criterion), resolved
// before any subprocess so unknown names fail the task, not the launcher.
// An explicitly named profile that no longer exists is an error, not a
// silent substitution: only an empty selection falls back to the repo
// default. Returns the profile name plus config, or ok=false when the repo
// or the winning profile is unknown.
func ResolveProfile(cfg *config.Config, repoName, taskProfile, override string) (string, config.AgentConfig, bool) {
	repo, ok := cfg.Repositories[repoName]
	if !ok {
		return "", config.AgentConfig{}, false
	}
	if strings.TrimSpace(override) != "" {
		prof, ok := cfg.Agents[override]
		if !ok {
			return "", config.AgentConfig{}, false
		}
		return override, prof, true
	}
	if strings.TrimSpace(taskProfile) != "" {
		prof, ok := cfg.Agents[taskProfile]
		if !ok {
			return "", config.AgentConfig{}, false
		}
		return taskProfile, prof, true
	}
	prof, ok := cfg.Agents[repo.Agent.Default]
	if !ok {
		return "", config.AgentConfig{}, false
	}
	return repo.Agent.Default, prof, true
}

// PromptInput bundles everything BuildPrompt needs:
// the resolved agent, repo policy, and the live sandbox handles.
// PriorAgent names the previous worker profile on a handoff so the new
// agent knows it inherits existing work instead of starting cold.
type PromptInput struct {
	Task             tasks.Task
	AgentKind        string
	ProfileName      string
	Repo             config.RepositoryConfig
	RepoInstructions string
	SandboxID        string
	Workspace        string
	PriorAgent       string
}

// BuildPrompt renders the seed prompt for one worker (SPEC section 36):
// task goal, issue metadata, repository instructions, allowed operations,
// and completion requirements. Purpose: the agent starts with the same
// contract Herder will later validate and deliver, so "done" means the
// same thing on both sides.
func BuildPrompt(in PromptInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Herder task %s\n\n", in.Task.ID)
	fmt.Fprintf(&b, "You are a coding agent working a Herder task inside an isolated sandbox.\n\n")
	b.WriteString("## Goal\n\n")
	if goal := strings.TrimSpace(in.Task.Goal); goal != "" {
		fmt.Fprintf(&b, "%s\n\n", goal)
	}
	fmt.Fprintf(&b, "Implement the work for issue %s (%s) in repository %s.\n",
		in.Task.SourceRef, in.Task.SourceProvider, in.Task.Repository)
	if in.PriorAgent != "" {
		fmt.Fprintf(&b, "This task was handed off from agent profile %s: its work and history are already in this workspace — continue it, do not restart.\n", in.PriorAgent)
	} else if in.Task.Attempt > 1 {
		fmt.Fprintf(&b, "This is attempt %d: earlier attempts may have left work in this workspace — continue it, do not restart.\n", in.Task.Attempt)
	}
	fmt.Fprintf(&b, "Work on branch %s. Keep the change focused on the issue.\n\n", in.Task.BranchName)
	b.WriteString("## Issue metadata\n\n")
	fmt.Fprintf(&b, "- Task: %s (attempt %d)\n", in.Task.ID, in.Task.Attempt)
	fmt.Fprintf(&b, "- Source: %s:%s\n", in.Task.SourceProvider, in.Task.SourceRef)
	fmt.Fprintf(&b, "- Repository: %s\n", in.Task.Repository)
	fmt.Fprintf(&b, "- Agent profile: %s (kind %s)\n", in.ProfileName, in.AgentKind)
	if in.SandboxID != "" {
		fmt.Fprintf(&b, "- Sandbox: %s\n", in.SandboxID)
	}
	if in.Workspace != "" {
		fmt.Fprintf(&b, "- Workspace: %s\n", in.Workspace)
	}
	b.WriteString("\n## Repository instructions\n\n")
	if strings.TrimSpace(in.RepoInstructions) != "" {
		b.WriteString(strings.TrimSpace(in.RepoInstructions))
	} else {
		b.WriteString("(No AGENTS.md or CLAUDE.md found in the checkout; follow the repository's standard conventions.)")
	}
	b.WriteString("\n\n## Allowed operations\n\n")
	b.WriteString("- Read, modify, and create files inside the workspace.\n")
	b.WriteString("- Run tests, linters, and build commands inside the sandbox.\n")
	b.WriteString("- Commit locally on the task branch as you reach checkpoints.\n")
	b.WriteString("- Do not push, open pull requests, or publish anything: Herder validates and delivers.\n")
	b.WriteString("- Do not read or exfiltrate credentials, tokens, or files outside the workspace.\n")
	b.WriteString("- Do not touch infrastructure: no Herdr control socket, no container runtime socket, no host files.\n")
	b.WriteString("\n## Completion requirements\n\n")
	if len(in.Repo.Validation.Commands) > 0 {
		b.WriteString("Before reporting done, every validation command must pass:\n")
		for _, cmd := range in.Repo.Validation.Commands {
			fmt.Fprintf(&b, "- %s\n", cmd)
		}
	} else {
		b.WriteString("Before reporting done, run the repository's standard test suite until green.\n")
	}
	b.WriteString("- Leave the work committed on the task branch with a clean working tree.\n")
	if in.Repo.Delivery.CreatePR {
		b.WriteString("- Do not create the pull request yourself; Herder opens it after validation.\n")
	}
	b.WriteString("- Report done only when the issue goal is met and validation passes; describe what changed and how it was verified.\n")
	return b.String()
}

// LoadRepoInstructions reads the working rules from a task workspace:
// AGENTS.md first, CLAUDE.md as backup, empty when neither exists.
// Purpose: the prompt carries repo instructions without shelling out.
// Inputs: workspace root. Returns trimmed content capped at instructionCap
// with a truncation marker, or "" when no instruction file exists.
func LoadRepoInstructions(workspace string) string {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		raw, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(raw))
		if text == "" {
			continue
		}
		if len(text) > instructionCap {
			text = text[:instructionCap] + "\n\n[... truncated: full instructions in " + name + " ...]"
		}
		return text
	}
	return ""
}

// RunResult is one finished Herdr invocation: split streams plus exit.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Runner runs one subprocess; the injectable seam behind Launcher so tests
// script Herdr results instead of needing a live server.
type Runner func(ctx context.Context, name string, args ...string) (RunResult, error)

// DefaultRunner resolves the binary in PATH and captures both streams,
// translating ExitError into ExitCode so a failed Herdr call is data the
// launcher can turn into a task event, not a transport mystery.
func DefaultRunner(ctx context.Context, name string, args ...string) (RunResult, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: resolve %s: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return RunResult{ExitCode: exit.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		return RunResult{}, fmt.Errorf("agent: run %s: %w", name, err)
	}
	return RunResult{ExitCode: 0, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// Launcher starts agents, seeds prompts, and probes sessions through the
// Herdr CLI. DetectTimeout bounds the post-launch wait for Herdr to
// classify the shim as the agent kind (default detectTimeout); PollInterval
// spaces those probes (default pollInterval). Both exist for tests.
type Launcher struct {
	// Runner executes subprocesses; nil means DefaultRunner.
	Runner Runner
	// DetectTimeout bounds the agent-detection wait after pane run.
	DetectTimeout time.Duration
	// PollInterval spaces detection probes.
	PollInterval time.Duration
}

// StartInput describes one agent launch: the deterministic session name,
// the resolved kind, the live sandbox handles, the shim directory, and
// the seeded prompt.
type StartInput struct {
	Session   string
	AgentKind string
	Workspace string
	Container string
	ShimDir   string
	Prompt    string
}

// StartResult carries the Herdr pane identity for the durable event: the
// session name is the stable link, pane/workspace ids are the live detail.
type StartResult struct {
	PaneID      string
	WorkspaceID string
}

// detectTimeout is the default bound on Herdr classifying the shim as the
// agent kind: detection polls the pane's foreground process, so a few
// seconds covers scheduling jitter without hanging a launch.
const detectTimeout = 15 * time.Second

// pollInterval spaces detection probes inside DetectTimeout.
const pollInterval = 250 * time.Millisecond

// promptReadyTimeout bounds the agent_not_ready retry window on the first
// prompt: Herdr may detect the shim before it reports the pane ready for
// input, so a short retry absorbs the gap.
const promptReadyTimeout = 10 * time.Second

// ShimRoot returns the directory holding per-kind docker shims, beside the
// state database like SandboxRoot: <dbdir>/shims.
func ShimRoot(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "shims")
}

// lookPath resolves binaries; a variable so tests point EnsureShim at a
// fake docker instead of requiring a real install.
var lookPath = exec.LookPath

// EnsureShim links the agent-kind shim into dir and returns its path.
// Why a symlink to docker: Herdr classifies a pane's foreground process by
// comm — the invoked basename — so `<kind> exec -it <ctr> <cmd>` is
// detected as the agent kind while really running `docker exec`. A symlink
// keeps comm at the link name (verified against herdr 0.9.1), costs
// nothing, and tracks docker upgrades automatically. A stale or wrong
// link is recreated; a real file at the path is left alone.
func EnsureShim(dir, kind string) (string, error) {
	dockerPath, err := lookPath("docker")
	if err != nil {
		return "", fmt.Errorf("agent: docker CLI not found (needed for the %s shim): %w", kind, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("agent: create shim dir: %w", err)
	}
	shim := filepath.Join(dir, kind)
	if target, err := os.Readlink(shim); err == nil && target == dockerPath {
		return shim, nil
	}
	if err := os.Symlink(dockerPath, shim); err != nil {
		if !os.IsExist(err) {
			return "", fmt.Errorf("agent: link %s shim: %w", kind, err)
		}
		// Exists but is not our link (or a dangling one): replace it.
		if rmErr := os.Remove(shim); rmErr != nil {
			return "", fmt.Errorf("agent: replace %s shim: %w", kind, rmErr)
		}
		if err := os.Symlink(dockerPath, shim); err != nil {
			return "", fmt.Errorf("agent: link %s shim: %w", kind, err)
		}
	}
	return shim, nil
}

// Start launches the agent wrapper in a Herdr pane and seeds its prompt.
// Flow (herdr 0.9.x, verified live): create a one-pane workspace labelled
// with the session name, `pane run` the kind shim into `docker exec -it`
// on the task container, poll `agent get <pane>` until Herdr classifies
// the shim's comm as the agent kind, `agent rename` the detected agent to
// the deterministic session name, then `agent prompt` the task contract.
// Unknown kinds fail before any subprocess so the caller can fail the task
// with a clear event instead of hanging (acceptance criterion). Any
// failure after the pane exists closes it best-effort — closing the
// workspace's last pane removes the workspace, so no layout leaks.
func (l *Launcher) Start(ctx context.Context, in StartInput) (StartResult, error) {
	cmd, ok := CommandForKind(in.AgentKind)
	if !ok {
		return StartResult{}, fmt.Errorf("agent: unknown agent kind %q (want codex, claude, opencode, gemini)", in.AgentKind)
	}
	if in.ShimDir == "" {
		return StartResult{}, fmt.Errorf("agent: no shim directory configured")
	}
	shim, err := EnsureShim(in.ShimDir, in.AgentKind)
	if err != nil {
		return StartResult{}, err
	}
	run := l.runner()
	out, err := run(ctx, "herdr", "workspace", "create",
		"--label", in.Session, "--cwd", in.Workspace,
		"--env", "HERDR_AGENT="+in.AgentKind, "--no-focus")
	if err != nil {
		return StartResult{}, fmt.Errorf("agent: workspace create %s: %w", in.Session, err)
	}
	if out.ExitCode != 0 {
		return StartResult{}, fmt.Errorf("agent: workspace create %s: %s", in.Session, textutil.FirstLine(out.Stderr))
	}
	paneID, workspaceID := parseWorkspaceCreate(out.Stdout)
	if paneID == "" {
		return StartResult{}, fmt.Errorf("agent: workspace create %s: no pane id in output", in.Session)
	}
	res := StartResult{PaneID: paneID, WorkspaceID: workspaceID}
	// From here on, failure must not leak the pane/workspace.
	fail := func(err error) (StartResult, error) {
		l.closePaneBestEffort(paneID)
		return res, err
	}
	if out, err := run(ctx, "herdr", "pane", "run", paneID,
		shim, "exec", "-it", in.Container, cmd); err != nil {
		return fail(fmt.Errorf("agent: pane run %s: %w", in.Session, err))
	} else if out.ExitCode != 0 {
		return fail(fmt.Errorf("agent: pane run %s: %s", in.Session, textutil.FirstLine(out.Stderr)))
	}
	if err := l.waitDetected(ctx, paneID, in.AgentKind); err != nil {
		return fail(err)
	}
	if out, err := run(ctx, "herdr", "agent", "rename", paneID, in.Session); err != nil {
		return fail(fmt.Errorf("agent: rename %s: %w", in.Session, err))
	} else if out.ExitCode != 0 {
		// A stale pane can still hold the session name after its agent
		// died (Herdr keeps the record until the pane closes): clear the
		// dead claimant and retry once before failing the launch.
		if !strings.Contains(out.Stderr, "agent_name_taken") ||
			l.clearSessionName(ctx, in.Session) != nil {
			return fail(fmt.Errorf("agent: rename %s: %s", in.Session, textutil.FirstLine(out.Stderr)))
		}
		if out, err := run(ctx, "herdr", "agent", "rename", paneID, in.Session); err != nil {
			return fail(fmt.Errorf("agent: rename %s: %w", in.Session, err))
		} else if out.ExitCode != 0 {
			return fail(fmt.Errorf("agent: rename %s: %s", in.Session, textutil.FirstLine(out.Stderr)))
		}
	}

	if err := l.SendPrompt(ctx, in.Session, in.Prompt); err != nil {
		// The named agent is live: keep the pane so the caller can bind
		// the session and a retry re-seeds it instead of orphaning work.
		return res, err
	}
	return res, nil
}

// clearSessionName frees a session name held by a stale pane: `agent
// rename <name> --clear` targets the holder by name and drops it so the
// fresh pane can take it. The holder's pane stays open for post-mortem
// reads; only the name is released.
func (l *Launcher) clearSessionName(ctx context.Context, session string) error {
	out, err := l.runner()(ctx, "herdr", "agent", "rename", session, "--clear")
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("%s", textutil.FirstLine(out.Stderr))
	}
	return nil
}

// waitDetected polls `agent get <pane>` until Herdr reports the pane's
// agent as the expected kind: detection keys on the shim's comm, so the
// poll proves the wrapper is up, not that the in-container agent finished
// initializing (real agents buffer stdin; the prompt retry covers the
// rest). Returns a named error on timeout or a transport failure.
func (l *Launcher) waitDetected(ctx context.Context, paneID, kind string) error {
	deadline := l.DetectTimeout
	if deadline <= 0 {
		deadline = detectTimeout
	}
	interval := l.PollInterval
	if interval <= 0 {
		interval = pollInterval
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		info, err := l.Get(ctx, paneID)
		if err == nil && info.Kind == kind {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent: %s not detected in pane %s within %s", kind, paneID, deadline)
		case <-time.After(interval):
		}
	}
}

// closePaneBestEffort closes a pane after a failed launch: the pane's
// process tree dies with it, and closing the workspace's last pane removes
// the workspace. Errors are swallowed — the launch error is authoritative.
func (l *Launcher) closePaneBestEffort(paneID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = l.runner()(ctx, "herdr", "pane", "close", paneID)
}

// SendPrompt delivers the seeded prompt to a live Herdr session via
// `agent prompt`. Purpose: keep the send contract — trailing-newline
// normalization, agent_not_ready retry, and error wording — in one place
// so Start and any later re-seed caller share it. Inputs: session name
// (or pane id) and prompt text. Returns a wrapped transport error, or the
// first stderr line on a nonzero exit. `agent prompt` rejects a blocked
// agent with agent_blocked — that refusal is the honest answer, so it is
// returned rather than bypassed with raw pane input.
func (l *Launcher) SendPrompt(ctx context.Context, session, prompt string) error {
	// `agent prompt` types the text into the session's pane: when the
	// agent is dead the pane sits at its shell and the prompt executes
	// as host commands. Refuse unless the shim is still foreground —
	// a stale named record is not a live agent.
	info, err := l.Get(ctx, session)
	if err != nil {
		return fmt.Errorf("agent: prompt %s: %w", session, err)
	}
	if !info.Running {
		return fmt.Errorf("agent: prompt %s: %w", session, ErrSessionGone)
	}
	text := strings.TrimRight(prompt, "\n") + "\n"
	deadline := time.Now().Add(promptReadyTimeout)
	for {
		out, err := l.runner()(ctx, "herdr", "agent", "prompt", session, text)
		if err != nil {
			return fmt.Errorf("agent: prompt %s: %w", session, err)
		}
		if out.ExitCode == 0 {
			return nil
		}
		if strings.Contains(out.Stderr, "agent_not_ready") && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("agent: prompt %s: %w", session, ctx.Err())
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		return fmt.Errorf("agent: prompt %s: %s", session, textutil.FirstLine(out.Stderr))
	}
}

// AgentInfo is the parsed `agent get` report for one live session: the
// detected kind, the normalized status Herdr reports, the pane identity
// needed to stop or attach the worker, and whether the agent process is
// actually still running. Herdr keeps a named agent's record after the
// process exits (the pane falls back to its shell while `agent get`
// still answers), so Running comes from `pane process-info`: the shim
// must still be the pane's foreground process.
type AgentInfo struct {
	Kind    string
	Status  string
	PaneID  string
	Running bool
}

// Get resolves one live Herdr session: exit 0 from `agent get` parses the
// agent record; an agent_not_found answer means the session is gone or
// never existed (ErrSessionGone), while any other failure — including a
// Herdr outage — is a plain error so callers can tell "dead" from
// "Herdr unreachable".
func (l *Launcher) Get(ctx context.Context, session string) (AgentInfo, error) {
	out, err := l.runner()(ctx, "herdr", "agent", "get", session)
	if err != nil {
		return AgentInfo{}, fmt.Errorf("agent: get %s: %w", session, err)
	}
	if out.ExitCode != 0 {
		if strings.Contains(out.Stderr, "agent_not_found") ||
			strings.Contains(out.Stdout, "agent_not_found") {
			return AgentInfo{}, fmt.Errorf("agent: get %s: %w", session, ErrSessionGone)
		}
		return AgentInfo{}, fmt.Errorf("agent: get %s: %s", session, textutil.FirstLine(out.Stderr))
	}
	var parsed struct {
		Result struct {
			Agent struct {
				Kind   string `json:"agent"`
				Status string `json:"agent_status"`
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &parsed); err != nil {
		return AgentInfo{}, fmt.Errorf("agent: get %s: unreadable output", session)
	}
	info := AgentInfo{
		Kind:   parsed.Result.Agent.Kind,
		Status: parsed.Result.Agent.Status,
		PaneID: parsed.Result.Agent.PaneID,
	}
	running, err := l.shimRunning(ctx, info)
	if err != nil {
		return AgentInfo{}, err
	}
	info.Running = running
	return info, nil
}

// shimRunning reports whether the pane's foreground process is still the
// agent-kind shim: `pane process-info` lists the foreground group, and a
// dead agent leaves the pane's shell there instead. A missing pane id or
// an unreadable process list is uncertain — the caller must not act on a
// maybe-dead reading, so it returns an error rather than a guess.
func (l *Launcher) shimRunning(ctx context.Context, info AgentInfo) (bool, error) {
	if info.PaneID == "" || info.Kind == "" {
		return false, nil
	}
	out, err := l.runner()(ctx, "herdr", "pane", "process-info", "--pane", info.PaneID)
	if err != nil {
		return false, fmt.Errorf("agent: process-info %s: %w", info.PaneID, err)
	}
	if out.ExitCode != 0 {
		return false, fmt.Errorf("agent: process-info %s: %s", info.PaneID, textutil.FirstLine(out.Stderr))
	}
	var parsed struct {
		Result struct {
			ProcessInfo struct {
				Foreground []struct {
					Name string `json:"name"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &parsed); err != nil {
		return false, fmt.Errorf("agent: process-info %s: unreadable output", info.PaneID)
	}
	for _, proc := range parsed.Result.ProcessInfo.Foreground {
		if proc.Name == info.Kind {
			return true, nil
		}
	}
	return false, nil
}

// ErrSessionGone reports a Herdr session that no longer answers: the pane
// exited or Herdr never tracked it. Callers clear the binding instead of
// retrying a dead name.
var ErrSessionGone = errors.New("session gone")

// IsLive reports whether a Herdr session's agent is still running: the
// named record must answer AND the pane's foreground must still be the
// shim — Herdr keeps the record after the process exits, so a bare
// successful Get is not proof of life. Transport errors read as
// not-live; the subsequent Start surfaces the real cause.
func (l *Launcher) IsLive(ctx context.Context, session string) bool {
	info, err := l.Get(ctx, session)
	return err == nil && info.Running
}

// Read returns the agent's recent terminal output via `agent read`:
// the human-facing tail behind `herder task logs` and the blocked-reason
// snippet behind supervisor notifications. `agent read` prints the pane
// text directly (no JSON envelope), so the trimmed stdout is the answer.
// Inputs: session name and the number of recent lines to keep. Returns
// the text, or a wrapped error when the session is unreadable.
func (l *Launcher) Read(ctx context.Context, session string, lines int) (string, error) {
	out, err := l.runner()(ctx, "herdr", "agent", "read", session,
		"--lines", strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("agent: read %s: %w", session, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("agent: read %s: %s", session, textutil.FirstLine(out.Stderr))
	}
	return strings.TrimRight(out.Stdout, "\n"), nil
}

// Stop ends one live Herdr session by closing its pane: the agent process
// dies with the pane while the sandbox and workspace stay intact for
// inspection or a fresh attempt. A session that is already gone is a
// no-op — the desired end state holds either way.
func (l *Launcher) Stop(ctx context.Context, session string) error {
	info, err := l.Get(ctx, session)
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			return nil
		}
		return err
	}
	if info.PaneID == "" {
		return fmt.Errorf("agent: stop %s: session has no pane id", session)
	}
	out, err := l.runner()(ctx, "herdr", "pane", "close", info.PaneID)
	if err != nil {
		return fmt.Errorf("agent: stop %s: %w", session, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("agent: stop %s: %s", session, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// Notify raises a human-visible Herdr notification (SPEC section 61):
// the supervision loop's channel for blocked, finished, and exited
// workers. A notification failure is returned so the caller can log it,
// but it must never gate the state change it announces.
func (l *Launcher) Notify(ctx context.Context, title, body string) error {
	args := []string{"notification", "show", title}
	if strings.TrimSpace(body) != "" {
		args = append(args, "--body", body)
	}
	out, err := l.runner()(ctx, "herdr", args...)
	if err != nil {
		return fmt.Errorf("agent: notify %q: %w", title, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("agent: notify %q: %s", title, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// NormalizeState maps a Herdr-reported agent_status onto Herder's
// normalized vocabulary (SPEC section 19). Herdr already reports the same
// words; anything unrecognized — including the empty string — normalizes
// to unknown instead of leaking a foreign token into task state.
func NormalizeState(reported string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case tasks.AgentStarting, tasks.AgentWorking, tasks.AgentIdle,
		tasks.AgentBlocked, tasks.AgentDone:
		return strings.ToLower(strings.TrimSpace(reported))
	default:
		return tasks.AgentUnknown
	}
}

// AttachArgv builds the glass-box attach entry: the human lands in the
// real running agent through Herdr, and detaching leaves it running.
// The caller must wire stdio through (needs a real TTY, not buffers).
func AttachArgv(session string) []string {
	return []string{"herdr", "agent", "attach", session}
}

// parseWorkspaceCreate extracts pane/workspace ids from `workspace create`
// JSON for the durable agent.started event. Unparseable output yields
// empty ids: the pane may still exist, so callers treat an empty pane id
// as a launch failure rather than leaking it.
func parseWorkspaceCreate(stdout string) (paneID, workspaceID string) {
	var parsed struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Workspace struct {
				WorkspaceID string `json:"workspace_id"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return "", ""
	}
	return parsed.Result.RootPane.PaneID, parsed.Result.Workspace.WorkspaceID
}

// runner resolves the injectable Runner default.
func (l *Launcher) runner() Runner {
	if l.Runner != nil {
		return l.Runner
	}
	return DefaultRunner
}
