// Package deliver turns validated task work into its external result
// (issue #6; SPEC sections 28, 35): push the task branch, open the pull
// request, comment the original issue, and advance its stage labels.
//
// Why: the credential boundary is a security invariant — the worker
// commits locally inside its sandbox, but every remote write runs on the
// controller host under the controller's own GitHub identity. The agent
// never sees a token: push authenticates through `gh auth
// git-credential` as a per-command git credential helper, and PR/issue
// operations go through the `gh` CLI, so no secret enters argv, the
// environment, or the sandbox.
// Approach: the Runner seam scripts subprocesses in tests (same style as
// internal/sandbox); every method is idempotent so a crashed delivery can
// be re-run — an existing open PR for the branch is found, not
// duplicated, and a comment carrying the PR URL is not posted twice.
// Inputs: workspace path, branch, repository, issue number, labels.
// Flow (driven by the caller): PushBranch -> CreatePR -> CommentIssue ->
// SetLabels. Returns: errors naming the failed step and its stderr.
package deliver

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/textutil"
)

// Runner runs one host-side subprocess; satisfied by
// sandbox.DefaultRunner.
type Runner = sandbox.Runner

// Engine performs controller-side delivery operations.
type Engine struct {
	// Runner executes git/gh on the controller host; defaults to
	// sandbox.DefaultRunner.
	Runner Runner
}

// PushBranch ships the validated head of the task branch to origin under
// the controller's GitHub identity.
//
// The workspace's .git is agent-writable, so pushing there would execute
// agent-controlled hooks (pre-push, core.hooksPath) and honor agent
// config (url.insteadOf redirection, core.fsmonitor) — a credential
// boundary breach (SPEC section 28). Instead the branch is fetched into a
// controller-owned bare staging repo (the -c overrides propagate to
// upload-pack via GIT_CONFIG_PARAMETERS), the fetched SHA is verified
// against wantSHA — the head the gate validated — and only that SHA is
// pushed to the explicit repository URL. A moved branch fails instead of
// shipping unverified commits; empty wantSHA skips the pin for tasks
// that reached REVIEWING without a recorded validation.
func (e *Engine) PushBranch(ctx context.Context, workspace, repo, branch, wantSHA string) error {
	staging, err := os.MkdirTemp("", "herder-deliver-*")
	if err != nil {
		return fmt.Errorf("deliver: staging repo: %w", err)
	}
	defer os.RemoveAll(staging)

	// cleanGit pins the knobs an agent-controlled repo could abuse:
	// hooks, fsmonitor, and upload-pack's pack-objects hook (the -c
	// overrides propagate to upload-pack via GIT_CONFIG_PARAMETERS).
	cleanGit := func(args ...string) []string {
		return sandbox.GitArgs(staging,
			append([]string{"-c", "uploadpack.packObjectsHook="}, args...)...)
	}
	if out, err := e.run(ctx, "git", cleanGit("init", "--bare")...); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("deliver: init staging repo: %s", textutil.FirstLine(out.Stderr))
	}
	// refs/heads/ qualifies the refspec: an unqualified name resolves a
	// same-named tag first, and the workspace's refs are agent-writable.
	if out, err := e.run(ctx, "git",
		cleanGit("fetch", "--no-tags", workspace, "refs/heads/"+branch)...); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("deliver: fetch %s: %s", branch, textutil.FirstLine(out.Stderr))
	}
	out, err := e.run(ctx, "git", "-C", staging, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("deliver: resolve fetched head: %s", textutil.FirstLine(out.Stderr))
	}
	sha := strings.TrimSpace(out.Stdout)
	if wantSHA != "" && sha != wantSHA {
		return fmt.Errorf("deliver: branch %s moved since validation (%s, want %s)", branch, sha, wantSHA)
	}
	remote := fmt.Sprintf("https://github.com/%s.git", repo)
	if out, err := e.run(ctx, "git", cleanGit(
		"-c", "credential.helper=",
		"-c", "credential.helper=!gh auth git-credential",
		"push", remote, sha+":refs/heads/"+branch)...); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("deliver: push %s: %s", branch, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// BranchSHA resolves the current tip of branch in the workspace so the
// caller can detect drift since validation. refs/heads/ qualifies the
// name so a same-named tag cannot shadow the branch; repo-controlled
// hooks and fsmonitor are neutralized.
func (e *Engine) BranchSHA(ctx context.Context, workspace, branch string) (string, error) {
	out, err := e.run(ctx, "git",
		sandbox.GitArgs(workspace, "rev-parse", "refs/heads/"+branch)...)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("deliver: resolve %s: %s", branch, textutil.FirstLine(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

// CreatePR opens the pull request for branch and returns its URL. When
// the branch already has an open PR (a retried delivery), the existing
// PR's URL is returned instead of failing.
func (e *Engine) CreatePR(ctx context.Context, repo, branch, title, body string) (string, error) {
	out, err := e.run(ctx, "gh", "pr", "create",
		"--repo", repo, "--head", branch, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	if out.ExitCode == 0 {
		if url := strings.TrimSpace(out.Stdout); url != "" {
			return url, nil
		}
	}
	// create failed or printed nothing: the branch may already have a PR.
	if url, found := e.findPR(ctx, repo, branch); found {
		return url, nil
	}
	return "", fmt.Errorf("deliver: create PR for %s: %s", branch, textutil.FirstLine(out.Stderr))
}

// findPR returns the OPEN PR URL for branch, or found=false. A closed or
// merged PR on the same branch is not a delivery: the jq filter drops it
// so a retried delivery fails instead of reporting a dead PR.
func (e *Engine) findPR(ctx context.Context, repo, branch string) (string, bool) {
	out, err := e.run(ctx, "gh", "pr", "view", branch,
		"--repo", repo, "--json", "url,state",
		"--jq", `select(.state == "OPEN") | .url`)
	if err != nil || out.ExitCode != 0 {
		return "", false
	}
	url := strings.TrimSpace(out.Stdout)
	return url, url != ""
}

// CommentIssue posts body on the task's source issue, skipping the post
// when a comment already carries marker (the PR URL) so a retried
// delivery never double-posts.
func (e *Engine) CommentIssue(ctx context.Context, repo string, issue int, body, marker string) error {
	if issue < 1 {
		return fmt.Errorf("deliver: comment needs an issue number, got %d", issue)
	}
	if marker != "" {
		// gh api --paginate walks every comment page: issue view caps at
		// the last 100, which would silently lose the marker on a busy
		// issue and double-post on retry.
		out, err := e.run(ctx, "gh", "api", "--paginate",
			fmt.Sprintf("repos/%s/issues/%d/comments", repo, issue),
			"--jq", ".[].body")
		if err != nil {
			return err
		}
		// A failed probe must not degrade to an unconditional post: the
		// caller leaves the task in DELIVERING and a rerun resumes instead
		// of double-commenting.
		if out.ExitCode != 0 {
			return fmt.Errorf("deliver: read comments on #%d: %s", issue, textutil.FirstLine(out.Stderr))
		}
		if strings.Contains(out.Stdout, marker) {
			return nil
		}
	}
	out, err := e.run(ctx, "gh", "issue", "comment", strconv.Itoa(issue),
		"--repo", repo, "--body", body)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("deliver: comment issue #%d: %s", issue, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// SetLabels advances the issue's stage labels: remove the previous
// stage's labels, add the next. Both lists may be empty; a fully empty
// change is a no-op that never calls gh.
func (e *Engine) SetLabels(ctx context.Context, repo string, issue int, add, remove []string) error {
	if issue < 1 {
		return fmt.Errorf("deliver: labels need an issue number, got %d", issue)
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	args := []string{"issue", "edit", strconv.Itoa(issue), "--repo", repo}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	out, err := e.run(ctx, "gh", args...)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("deliver: label issue #%d: %s", issue, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// IssueLabels returns the issue's current label names so callers can
// compute which stage labels to remove without guessing.
func (e *Engine) IssueLabels(ctx context.Context, repo string, issue int) ([]string, error) {
	if issue < 1 {
		return nil, fmt.Errorf("deliver: labels need an issue number, got %d", issue)
	}
	out, err := e.run(ctx, "gh", "issue", "view", strconv.Itoa(issue),
		"--repo", repo, "--json", "labels", "--jq", ".labels[].name")
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("deliver: read labels on #%d: %s", issue, textutil.FirstLine(out.Stderr))
	}
	var labels []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			labels = append(labels, l)
		}
	}
	return labels, nil
}

// IssueNumber parses the issue number out of a source ref like
// "acme/web#7"; ok is false when the ref carries no usable number.
func IssueNumber(sourceRef string) (int, bool) {
	_, num, ok := strings.Cut(sourceRef, "#")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// run executes one subprocess through the injectable Runner.
func (e *Engine) run(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	r := e.Runner
	if r == nil {
		r = sandbox.DefaultRunner
	}
	return r(ctx, name, args...)
}
