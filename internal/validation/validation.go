// Package validation is the Herder gate between "agent says done" and
// "work may ship" (issue #6; SPEC section 33): configured commands run
// inside the task sandbox, then host-side git checks enforce the
// forbidden-path and clean-tree policy on the task workspace.
//
// Why: agent completion is a claim, not a fact. A PR must never open over
// unverified work, uncommitted leftovers, edits to protected files, or a
// ref that is not the task branch — delivery pushes the branch, so the
// gate must prove the workspace sits on it. The workspace's .git is
// agent-writable, so every host-side git call neutralizes repo-controlled
// config (hooks, fsmonitor) and the diff base comes from the SHA recorded
// at provisioning time, never from refs the agent can move.
// Approach: commands exec inside the sandbox through the injectable Exec
// seam (same shape as sandbox.Provider.Exec); the forbidden/dirty/branch
// checks run git on the host workspace through the Runner seam, so tests
// script subprocesses instead of needing Docker. Every step emits a
// structured event through the Emit hook so the durable history shows
// per-command start, output, and pass/fail.
// Inputs: Input (task identity, container, workspace, task branch, base
// SHA, repo validation config). Flow: validation.started -> per-command
// events -> branch, forbidden, and clean-tree checks -> validation.passed
// or validation.failed.
// Returns: a Report naming every failure; transport errors (sandbox or
// git unreachable) return as errors, not failed reports.
package validation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
)

// Event types emitted through Gate.Emit (SPEC section 58).
const (
	EventStarted          = "validation.started"
	EventCommandStarted   = "validation.command.started"
	EventCommandSucceeded = "validation.command.succeeded"
	EventCommandFailed    = "validation.command.failed"
	EventPassed           = "validation.passed"
	EventFailed           = "validation.failed"
)

// outputCap bounds command output stored in events and reports: enough to
// diagnose a failure, never an unbounded buffer.
const outputCap = 4096

// Executor runs one command inside the task sandbox; satisfied by
// *sandbox.DockerProvider.Exec.
type Executor func(ctx context.Context, id string, cmd []string) (*sandbox.Result, error)

// Runner runs one host-side subprocess (git probes on the workspace);
// satisfied by sandbox.DefaultRunner.
type Runner = sandbox.Runner

// Gate runs the repository's validation sequence for one task.
type Gate struct {
	// Exec runs commands inside the sandbox; required.
	Exec Executor
	// Runner runs host-side git probes; defaults to sandbox.DefaultRunner.
	Runner Runner
	// Emit receives structured events; nil discards them.
	Emit func(eventType string, payload map[string]any)
}

// Input describes one validation run.
type Input struct {
	TaskID    string
	Container string
	Workspace string
	// Branch is the task branch the workspace must be checked out on:
	// delivery pushes that ref, so the gate must prove it is what was
	// validated. Empty skips the branch check.
	Branch string
	// BaseSHA pins the diff base to the commit recorded at provisioning
	// time, before the agent ran: refs inside the workspace are
	// agent-writable, so merge-base against origin/HEAD is forgeable.
	// Empty falls back to merge-base (hand-rolled workspaces).
	BaseSHA string
	Config  config.ValidationConfig
}

// CommandResult is one finished validation command.
type CommandResult struct {
	Command  string
	ExitCode int
	Output   string
	Started  time.Time
	Duration time.Duration
}

// Report is the complete gate outcome: per-command results plus the
// policy violations (forbidden paths, dirty tree, wrong branch) that
// block delivery.
type Report struct {
	Commands  []CommandResult
	Forbidden []string
	Dirty     []string
	// WrongBranch is the checked-out branch when it is not Input.Branch
	// ("(detached HEAD)" when HEAD is detached); TaskBranch names the
	// expected branch so the retry prompt can say where work belongs.
	WrongBranch string
	TaskBranch  string
	// Head is the validated HEAD SHA: delivery pushes exactly this commit.
	Head string
}

// Passed reports whether every check succeeded.
func (r Report) Passed() bool {
	for _, c := range r.Commands {
		if c.ExitCode != 0 {
			return false
		}
	}
	return len(r.Forbidden) == 0 && len(r.Dirty) == 0 && r.WrongBranch == ""
}

// Summary renders the failure details for the retry prompt or a human:
// failed commands with output tails, then the offending paths and branch.
func (r Report) Summary() string {
	var b strings.Builder
	for _, c := range r.Commands {
		if c.ExitCode == 0 {
			continue
		}
		fmt.Fprintf(&b, "command %q failed (exit %d):\n%s\n", c.Command, c.ExitCode, tail(c.Output, 2000))
	}
	if len(r.Forbidden) > 0 {
		fmt.Fprintf(&b, "forbidden paths changed: %s\n", strings.Join(r.Forbidden, ", "))
	}
	if len(r.Dirty) > 0 {
		fmt.Fprintf(&b, "uncommitted or untracked files: %s\n", strings.Join(r.Dirty, ", "))
	}
	if r.WrongBranch != "" {
		fmt.Fprintf(&b, "workspace is on %s, want the task branch %s\n", r.WrongBranch, r.TaskBranch)
	}
	return strings.TrimSpace(b.String())
}

// Run executes the full gate: sandbox commands in configured order (all
// run even after a failure so the report is complete), then the branch,
// forbidden-path, and clean-tree checks against the workspace.
func (g *Gate) Run(ctx context.Context, in Input) (Report, error) {
	if g.Exec == nil {
		return Report{}, errors.New("validation: no sandbox executor configured")
	}
	rep := Report{}
	g.emit(EventStarted, map[string]any{
		"task": in.TaskID, "container": in.Container,
		"commands": in.Config.Commands, "forbidden": in.Config.ForbiddenChanges,
		"require_clean_git": in.Config.CleanTreeRequired(),
	})
	for _, command := range in.Config.Commands {
		res, err := g.runCommand(ctx, in, command)
		if err != nil {
			g.emit(EventFailed, map[string]any{"task": in.TaskID, "reason": err.Error()})
			return rep, err
		}
		rep.Commands = append(rep.Commands, res)
	}
	if err := g.checkWorkspace(ctx, in, &rep); err != nil {
		g.emit(EventFailed, map[string]any{"task": in.TaskID, "reason": err.Error()})
		return rep, err
	}
	if rep.Passed() {
		g.emit(EventPassed, map[string]any{
			"task": in.TaskID, "commands": len(rep.Commands), "head": rep.Head,
		})
	} else {
		g.emit(EventFailed, map[string]any{
			"task": in.TaskID, "summary": rep.Summary(),
			"forbidden": rep.Forbidden, "dirty": rep.Dirty,
			"wrong_branch": rep.WrongBranch,
		})
	}
	return rep, nil
}

// runCommand executes one configured command via sh -c inside the sandbox
// and emits its start/finish events with the captured output.
func (g *Gate) runCommand(ctx context.Context, in Input, command string) (CommandResult, error) {
	started := time.Now().UTC()
	g.emit(EventCommandStarted, map[string]any{"task": in.TaskID, "command": command})
	res, err := g.Exec(ctx, in.Container, []string{"sh", "-c", command})
	if err != nil {
		return CommandResult{}, fmt.Errorf("validation: exec %q: %w", command, err)
	}
	out := CommandResult{
		Command: command, ExitCode: res.ExitCode,
		Output:  truncate(res.Stdout+res.Stderr, outputCap),
		Started: started, Duration: time.Since(started),
	}
	payload := map[string]any{
		"task": in.TaskID, "command": command,
		"exit_code": out.ExitCode, "output": out.Output,
		"duration_ms": out.Duration.Milliseconds(),
	}
	if out.ExitCode == 0 {
		g.emit(EventCommandSucceeded, payload)
	} else {
		g.emit(EventCommandFailed, payload)
	}
	return out, nil
}

// checkWorkspace enforces the host-side policy checks: the workspace must
// sit on the task branch (delivery pushes that ref), then forbidden paths
// over the changed-file set, then the clean-tree rule. The validated HEAD
// SHA is recorded on the report so delivery can pin the push to it.
func (g *Gate) checkWorkspace(ctx context.Context, in Input, rep *Report) error {
	if in.Branch != "" {
		branch, err := g.currentBranch(ctx, in.Workspace)
		if err != nil {
			return err
		}
		if branch != in.Branch {
			rep.WrongBranch = branch
			if rep.WrongBranch == "" {
				rep.WrongBranch = "(detached HEAD)"
			}
			rep.TaskBranch = in.Branch
		}
	}
	head, err := g.headSHA(ctx, in.Workspace)
	if err != nil {
		return err
	}
	rep.Head = head
	changed, status, err := g.changedPaths(ctx, in)
	if err != nil {
		return err
	}
	for _, path := range changed {
		for _, pattern := range in.Config.ForbiddenChanges {
			if matchGlob(pattern, path) {
				rep.Forbidden = append(rep.Forbidden, path)
				break
			}
		}
	}
	if in.Config.CleanTreeRequired() {
		rep.Dirty = status
	}
	return nil
}

// currentBranch returns the workspace's checked-out branch, or "" when
// HEAD is detached (symbolic-ref exits non-zero).
func (g *Gate) currentBranch(ctx context.Context, workspace string) (string, error) {
	out, err := g.run(ctx, "git", sandbox.GitArgs(workspace, "symbolic-ref", "--short", "HEAD")...)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(out.Stdout), nil
}

// headSHA resolves the workspace HEAD so the report can pin the validated
// commit for delivery.
func (g *Gate) headSHA(ctx context.Context, workspace string) (string, error) {
	out, err := g.run(ctx, "git", sandbox.GitArgs(workspace, "rev-parse", "HEAD")...)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("validation: git rev-parse HEAD: %s", sandbox.FirstLine(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

// changedPaths returns the files the task touched — the diff against the
// recorded base SHA plus untracked files — and the raw dirty list from
// git status. The diff uses -z so C-quoted paths cannot evade the glob,
// and status pins untracked-file reporting so repo-local config cannot
// hide work from the scan.
func (g *Gate) changedPaths(ctx context.Context, in Input) (changed, dirty []string, err error) {
	base, err := g.baseRef(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	out, err := g.run(ctx, "git", sandbox.GitArgs(in.Workspace, "diff", "--name-only", "--no-renames", "-z", base)...)
	if err != nil {
		return nil, nil, err
	}
	if out.ExitCode != 0 {
		return nil, nil, fmt.Errorf("validation: git diff %s: %s", base, sandbox.FirstLine(out.Stderr))
	}
	for _, p := range strings.Split(out.Stdout, "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	out, err = g.run(ctx, "git", sandbox.GitArgs(in.Workspace,
		"-c", "status.showUntrackedFiles=all",
		"status", "--porcelain", "-uall")...)
	if err != nil {
		return nil, nil, err
	}
	if out.ExitCode != 0 {
		return nil, nil, fmt.Errorf("validation: git status: %s", sandbox.FirstLine(out.Stderr))
	}
	for _, line := range strings.Split(out.Stdout, "\n") {
		path, ok := porcelainPath(line)
		if !ok {
			continue
		}
		dirty = append(dirty, path)
		if strings.HasPrefix(line, "??") {
			changed = append(changed, path)
		}
	}
	return changed, dirty, nil
}

// baseRef picks the diff base. The recorded provisioning-time SHA wins:
// refs inside the workspace are agent-writable, so merge-base against
// origin/HEAD is forgeable. Without a record (hand-rolled workspaces) it
// falls back to merge-base, then origin/HEAD, then HEAD.
func (g *Gate) baseRef(ctx context.Context, in Input) (string, error) {
	if in.BaseSHA != "" {
		return in.BaseSHA, nil
	}
	out, err := g.run(ctx, "git", sandbox.GitArgs(in.Workspace, "merge-base", "origin/HEAD", "HEAD")...)
	if err != nil {
		return "", err
	}
	if out.ExitCode == 0 && strings.TrimSpace(out.Stdout) != "" {
		return strings.TrimSpace(out.Stdout), nil
	}
	out, err = g.run(ctx, "git", sandbox.GitArgs(in.Workspace, "rev-parse", "--verify", "--quiet", "origin/HEAD")...)
	if err != nil {
		return "", err
	}
	if out.ExitCode == 0 && strings.TrimSpace(out.Stdout) != "" {
		return "origin/HEAD", nil
	}
	return "HEAD", nil
}

// run executes one host-side subprocess through the injectable Runner.
func (g *Gate) run(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	r := g.Runner
	if r == nil {
		r = sandbox.DefaultRunner
	}
	return r(ctx, name, args...)
}

// emit reports one event through the hook, discarding when unset.
func (g *Gate) emit(eventType string, payload map[string]any) {
	if g.Emit != nil {
		g.Emit(eventType, payload)
	}
}

// porcelainPath extracts the path from one git status --porcelain line:
// the two status columns plus space, then the path. Only R/C lines carry
// the "orig -> new" form, so the rename-target split is gated on the
// status code; C-style quoted names are unquoted.
func porcelainPath(line string) (string, bool) {
	if len(line) < 4 {
		return "", false
	}
	path := line[3:]
	if line[0] == 'R' || line[0] == 'C' || line[1] == 'R' || line[1] == 'C' {
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
	}
	if unquoted, err := strconv.Unquote(path); err == nil {
		path = unquoted
	}
	path = strings.TrimSpace(path)
	return path, path != ""
}

// matchGlob reports whether a repo-relative path matches a forbidden
// pattern: * and ? never cross '/', ** crosses separators, and the whole
// pattern must match the whole path.
func matchGlob(pattern, path string) bool {
	// A trailing slash means "everything under this directory": without
	// the normalization "secrets/" would trim to "secrets" and never
	// match the files it was written to protect.
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	pat := strings.Split(strings.Trim(pattern, "/"), "/")
	seg := strings.Split(strings.Trim(path, "/"), "/")
	return matchSegments(pat, seg)
}

// matchSegments is the recursive matcher: "**" consumes zero or more path
// segments, every other segment must match exactly one.
func matchSegments(pat, seg []string) bool {
	if len(pat) == 0 {
		return len(seg) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(seg); i++ {
			if matchSegments(pat[1:], seg[i:]) {
				return true
			}
		}
		return false
	}
	if len(seg) == 0 || !matchSegment(pat[0], seg[0]) {
		return false
	}
	return matchSegments(pat[1:], seg[1:])
}

// matchSegment matches one path segment against one pattern segment with
// * (any run) and ? (one rune), backtracking on mismatch.
func matchSegment(pattern, s string) bool {
	p, q := []rune(pattern), []rune(s)
	px, sx, star, mark := 0, 0, -1, 0
	for sx < len(q) {
		switch {
		case px < len(p) && (p[px] == '?' || p[px] == q[sx]):
			px++
			sx++
		case px < len(p) && p[px] == '*':
			star, mark, px = px, sx, px+1
		case star >= 0:
			px, sx = star+1, mark+1
			mark++
		default:
			return false
		}
	}
	for px < len(p) && p[px] == '*' {
		px++
	}
	return px == len(p)
}

// truncate keeps event payloads bounded while marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

// tail keeps the end of long output: failures matter at the bottom.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…[truncated]\n" + s[len(s)-n:]
}
