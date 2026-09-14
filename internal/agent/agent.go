// Package agent launches coding agents through Herdr into task sandboxes
// (issue #4; SPEC sections 16-18, 20-21, 24, 26): one Herdr agent session
// per running task, seeded with the task goal plus repository instructions
// and completion requirements, bound durably as task <-> sandbox <->
// session so a human can attach to the real agent instead of a spinner.
//
// Why: black-box runners hide the worker; Herder stays glass-box by keeping
// the agent in a real Herdr pane behind a sandbox wrapper the host can
// still attribute via HERDR_AGENT (SPEC section 24).
// Approach: Stage 1 CLI orchestration (SPEC section 17) — `herdr agent
// start` with --env HERDR_AGENT=<kind> and a `docker exec -it` wrapper,
// then `herdr agent send` to seed the prompt. The Runner seam scripts
// subprocesses in tests; the socket API comes later.
// Inputs: task + repo/agent config + live sandbox (container + workspace).
// Flow: ResolveProfile -> BuildPrompt -> Launcher.Start records the session
// name; attach re-enters via AttachArgv. Unknown kinds fail before any
// subprocess so tasks fail with an event instead of hanging.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/tasks"
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
type PromptInput struct {
	Task             tasks.Task
	AgentKind        string
	ProfileName      string
	Repo             config.RepositoryConfig
	RepoInstructions string
	SandboxID        string
	Workspace        string
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
// Herdr CLI.
type Launcher struct {
	// Runner executes subprocesses; nil means DefaultRunner.
	Runner Runner
}

// StartInput describes one agent launch: the deterministic session name,
// the resolved kind, the live sandbox handles, and the seeded prompt.
type StartInput struct {
	Session   string
	AgentKind string
	Workspace string
	Container string
	Prompt    string
}

// StartResult carries the Herdr pane identity for the durable event: the
// session name is the stable link, pane/workspace ids are the live detail.
type StartResult struct {
	PaneID      string
	WorkspaceID string
}

// Start launches the agent wrapper in a Herdr pane and seeds its prompt.
// Why two calls: `agent start` owns the pane and the HERDR_AGENT
// attribution on the host-visible wrapper; `agent send` delivers the task
// contract as first input (Stage 1 CLI orchestration, SPEC section 17).
// Unknown kinds fail before any subprocess so the caller can fail the task
// with a clear event instead of hanging (acceptance criterion).
func (l *Launcher) Start(ctx context.Context, in StartInput) (StartResult, error) {
	cmd, ok := CommandForKind(in.AgentKind)
	if !ok {
		return StartResult{}, fmt.Errorf("agent: unknown agent kind %q (want codex, claude, opencode, gemini)", in.AgentKind)
	}
	run := l.runner()
	start := []string{
		"agent", "start", in.Session,
		"--cwd", in.Workspace, "--env", "HERDR_AGENT=" + in.AgentKind,
		"--no-focus", "--",
		"docker", "exec", "-it", in.Container, cmd,
	}
	out, err := run(ctx, "herdr", start...)
	if err != nil {
		return StartResult{}, fmt.Errorf("agent: start %s: %w", in.Session, err)
	}
	if out.ExitCode != 0 {
		return StartResult{}, fmt.Errorf("agent: start %s: %s", in.Session, firstLine(out.Stderr))
	}
	paneID, workspaceID := parseStartResult(out.Stdout)
	if err := l.SendPrompt(ctx, in.Session, in.Prompt); err != nil {
		return StartResult{}, err
	}
	return StartResult{PaneID: paneID, WorkspaceID: workspaceID}, nil
}

// SendPrompt delivers the seeded prompt to a live Herdr session via
// `agent send`. Purpose: keep the send contract — trailing-newline
// normalization and error wording — in one place so Start and any later
// re-seed caller share it. Inputs: session name and prompt text. Returns
// a wrapped transport error, or the first stderr line on a nonzero exit.
func (l *Launcher) SendPrompt(ctx context.Context, session, prompt string) error {
	if !strings.HasSuffix(prompt, "\n") {
		prompt += "\n"
	}
	out, err := l.runner()(ctx, "herdr", "agent", "send", session, prompt)
	if err != nil {
		return fmt.Errorf("agent: send prompt to %s: %w", session, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("agent: send prompt to %s: %s", session, firstLine(out.Stderr))
	}
	return nil
}

// IsLive reports whether a Herdr session still answers: exit 0 from
// `agent get` means the pane exists and Herdr tracks it. Transport errors
// read as not-live; the subsequent Start surfaces the real cause.
func (l *Launcher) IsLive(ctx context.Context, session string) bool {
	out, err := l.runner()(ctx, "herdr", "agent", "get", session)
	return err == nil && out.ExitCode == 0
}

// AttachArgv builds the glass-box attach entry: the human lands in the
// real running agent through Herdr, and detaching leaves it running.
// The caller must wire stdio through (needs a real TTY, not buffers).
func AttachArgv(session string) []string {
	return []string{"herdr", "agent", "attach", session}
}

// parseStartResult extracts pane/workspace ids from `agent start` JSON for
// the durable agent.started event. Unparseable output yields empty ids:
// the launch still succeeded, only the linkage detail is missing.
func parseStartResult(stdout string) (paneID, workspaceID string) {
	var parsed struct {
		Result struct {
			Agent struct {
				PaneID      string `json:"pane_id"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return "", ""
	}
	return parsed.Result.Agent.PaneID, parsed.Result.Agent.WorkspaceID
}

// runner resolves the injectable Runner default.
func (l *Launcher) runner() Runner {
	if l.Runner != nil {
		return l.Runner
	}
	return DefaultRunner
}

// firstLine keeps launch errors to one actionable line.
func firstLine(s string) string {
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(s)
}
