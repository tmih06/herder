package deliver

import (
	"context"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/sandbox"
)

// fakeRunner scripts subprocess results and records every argv.
type fakeRunner struct {
	calls   [][]string
	respond func(name string, args []string) (sandbox.RunResult, error)
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.respond != nil {
		return f.respond(name, args)
	}
	return sandbox.RunResult{}, nil
}

// findCall returns the recorded argv containing all fragments, or nil.
func findCall(calls [][]string, fragments ...string) []string {
outer:
	for _, c := range calls {
		joined := strings.Join(c, " ")
		for _, frag := range fragments {
			if !strings.Contains(joined, frag) {
				continue outer
			}
		}
		return c
	}
	return nil
}

// The push must authenticate as the controller through gh's credential
// helper — never a token in argv or the sandbox (SPEC section 28).
// The push must run from a controller-owned staging repo: the workspace's
// .git is agent-writable, so pushing there would execute agent hooks and
// honor agent config (credential boundary, SPEC section 28). The fetched
// SHA must equal the validated head, and the push must target the
// explicit repository URL with only gh credentials.
func TestPushBranchStagingRepo(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "rev-parse FETCH_HEAD") {
			return sandbox.RunResult{Stdout: "deadbeef\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	if err := e.PushBranch(context.Background(), "/tmp/ws", "acme/web", "herder/7-fix", "deadbeef"); err != nil {
		t.Fatalf("PushBranch = %v", err)
	}
	var fetch, push []string
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "fetch"):
			fetch = c
		case strings.Contains(joined, "push"):
			push = c
		}
	}
	if fetch == nil || !strings.Contains(strings.Join(fetch, " "), "uploadpack.packObjectsHook=") {
		t.Errorf("fetch must neutralize source-side hooks, ran %v", f.calls)
	}
	if push == nil {
		t.Fatalf("missing push call, ran %v", f.calls)
	}
	joined := strings.Join(push, " ")
	if !strings.Contains(joined, "https://github.com/acme/web.git") {
		t.Errorf("push must target the explicit repo URL, ran %v", push)
	}
	if !strings.Contains(joined, "deadbeef:refs/heads/herder/7-fix") {
		t.Errorf("push must ship the validated SHA, ran %v", push)
	}
	if !strings.Contains(joined, "credential.helper=") ||
		!strings.Contains(joined, "!gh auth git-credential") {
		t.Errorf("push must route credentials through gh, ran %v", push)
	}
	if strings.Contains(joined, "x-access-token") || strings.Contains(joined, "oauth") {
		t.Errorf("push must never carry a token in argv, ran %v", push)
	}
}

// A branch that moved after validation must not ship: the fetched SHA
// differs from the validated head and the push never runs.
func TestPushBranchDrift(t *testing.T) {
	var pushed bool
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse FETCH_HEAD"):
			return sandbox.RunResult{Stdout: "cafef00d\n"}, nil
		case strings.Contains(joined, "push"):
			pushed = true
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	err := e.PushBranch(context.Background(), "/tmp/ws", "acme/web", "herder/7-fix", "deadbeef")
	if err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("drift must fail naming the move, got %v", err)
	}
	if pushed {
		t.Error("a moved branch must never push")
	}
}

func TestPushBranchFailure(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "push") {
			return sandbox.RunResult{ExitCode: 1, Stderr: "permission denied"}, nil
		}
		return sandbox.RunResult{Stdout: "deadbeef\n"}, nil
	}}
	e := &Engine{Runner: f.run}
	err := e.PushBranch(context.Background(), "/tmp/ws", "acme/web", "herder/7-fix", "deadbeef")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("push failure must name stderr, got %v", err)
	}
}

// A fresh PR returns its URL; an already-open PR for the branch is found
// instead of failing (idempotent redelivery, SPEC section 49).
func TestCreatePR(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		if strings.Contains(strings.Join(args, " "), "pr create") {
			return sandbox.RunResult{Stdout: "https://github.com/acme/web/pull/201\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	url, err := e.CreatePR(context.Background(), "acme/web", "herder/7-fix", "Fix it", "body")
	if err != nil {
		t.Fatalf("CreatePR = %v", err)
	}
	if url != "https://github.com/acme/web/pull/201" {
		t.Errorf("url = %q", url)
	}
	call := findCall(f.calls, "pr", "create", "--repo", "acme/web", "--head", "herder/7-fix")
	if call == nil {
		t.Errorf("missing pr create call, ran %v", f.calls)
	}
}

func TestCreatePRAlreadyExists(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "pr create"):
			return sandbox.RunResult{ExitCode: 1, Stderr: "a pull request for branch already exists"}, nil
		case strings.Contains(joined, "pr view"):
			return sandbox.RunResult{Stdout: "https://github.com/acme/web/pull/201\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	url, err := e.CreatePR(context.Background(), "acme/web", "herder/7-fix", "t", "b")
	if err != nil {
		t.Fatalf("CreatePR = %v", err)
	}
	if url != "https://github.com/acme/web/pull/201" {
		t.Errorf("existing PR url = %q", url)
	}
}

func TestCreatePRRealFailure(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		return sandbox.RunResult{ExitCode: 1, Stderr: "gh not authenticated"}, nil
	}}
	e := &Engine{Runner: f.run}
	if _, err := e.CreatePR(context.Background(), "acme/web", "b", "t", "b"); err == nil {
		t.Fatal("a failed create with no existing PR must error")
	}
}

func TestCommentIssue(t *testing.T) {
	f := &fakeRunner{}
	e := &Engine{Runner: f.run}
	if err := e.CommentIssue(context.Background(), "acme/web", 7, "PR opened", ""); err != nil {
		t.Fatalf("CommentIssue = %v", err)
	}
	if findCall(f.calls, "issue", "comment", "7", "--repo", "acme/web") == nil {
		t.Errorf("missing issue comment call, ran %v", f.calls)
	}
	if err := e.CommentIssue(context.Background(), "acme/web", 0, "x", ""); err == nil {
		t.Error("issue 0 must fail")
	}
}

// Label advancement removes the previous stage labels and adds the next
// in one gh call; nothing to change means no call at all.
func TestSetLabels(t *testing.T) {
	f := &fakeRunner{}
	e := &Engine{Runner: f.run}
	err := e.SetLabels(context.Background(), "acme/web", 7,
		[]string{"agent-review"}, []string{"agent-ready", "agent-running"})
	if err != nil {
		t.Fatalf("SetLabels = %v", err)
	}
	call := findCall(f.calls, "issue", "edit", "7")
	if call == nil {
		t.Fatalf("missing issue edit call, ran %v", f.calls)
	}
	joined := strings.Join(call, " ")
	for _, want := range []string{"--add-label agent-review", "--remove-label agent-ready", "--remove-label agent-running"} {
		if !strings.Contains(joined, want) {
			t.Errorf("label call missing %q, ran %v", want, call)
		}
	}

	f.calls = nil
	if err := e.SetLabels(context.Background(), "acme/web", 7, nil, nil); err != nil {
		t.Fatalf("empty SetLabels = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("empty label change must not call gh, ran %v", f.calls)
	}
}

func TestIssueNumber(t *testing.T) {
	for in, want := range map[string]int{
		"acme/web#7":   7,
		"acme/web#182": 182,
		"no-number":    0,
		"acme/web#abc": 0,
		"acme/web#0":   0,
		"":             0,
		"acme/web#7x":  0,
		"acme/web# 12": 12,
	} {
		if got, ok := IssueNumber(in); got != want || (want == 0) == ok {
			t.Errorf("IssueNumber(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
}

// A closed or merged PR on the branch is not a successful delivery:
// findPR must only match open PRs, so a retried delivery after a closed
// attempt fails instead of reporting a dead PR.
func TestCreatePRClosedExisting(t *testing.T) {
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "pr create"):
			return sandbox.RunResult{ExitCode: 1, Stderr: "a pull request for branch already exists"}, nil
		case strings.Contains(joined, "pr view"):
			// gh applies --jq client-side: select(.state == "OPEN") drops
			// the merged PR, so stdout is empty.
			return sandbox.RunResult{Stdout: ""}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	if _, err := e.CreatePR(context.Background(), "acme/web", "herder/7-fix", "t", "b"); err == nil {
		t.Fatal("a closed/merged PR must not satisfy delivery")
	}
	view := findCall(f.calls, "pr", "view")
	if view == nil || !strings.Contains(strings.Join(view, " "), `select(.state == "OPEN")`) {
		t.Errorf("findPR must filter to open PRs, ran %v", view)
	}
}

// CommentIssue must not double-post on a retried delivery: when a comment
// carrying the marker already exists, no new comment is created. The
// probe paginates so a marker older than the last 100 comments is found.
func TestCommentIssueIdempotent(t *testing.T) {
	var comments int
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "issue comment"):
			comments++
			return sandbox.RunResult{}, nil
		case name == "gh" && strings.HasPrefix(joined, "api"):
			if !strings.Contains(joined, "--paginate") {
				t.Errorf("comment probe must paginate past the 100-comment cap, ran %v", args)
			}
			return sandbox.RunResult{Stdout: "Herder opened https://github.com/acme/web/pull/201 for this issue\n"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	err := e.CommentIssue(context.Background(), "acme/web", 7,
		"Herder opened https://github.com/acme/web/pull/201 for this issue",
		"https://github.com/acme/web/pull/201")
	if err != nil {
		t.Fatalf("CommentIssue = %v", err)
	}
	if comments != 0 {
		t.Errorf("existing marker must suppress a duplicate comment, posted %d", comments)
	}
}

// A failed dedup probe must not degrade to an unconditional post: the
// error leaves the task in DELIVERING so a rerun resumes cleanly instead
// of double-commenting.
func TestCommentIssueProbeFailure(t *testing.T) {
	var comments int
	f := &fakeRunner{respond: func(name string, args []string) (sandbox.RunResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "issue comment"):
			comments++
			return sandbox.RunResult{}, nil
		case name == "gh" && strings.HasPrefix(joined, "api"):
			return sandbox.RunResult{ExitCode: 1, Stderr: "HTTP 502"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	e := &Engine{Runner: f.run}
	err := e.CommentIssue(context.Background(), "acme/web", 7, "body", "marker")
	if err == nil {
		t.Fatal("a failed dedup probe must return an error")
	}
	if comments != 0 {
		t.Errorf("no comment may post when the probe failed, posted %d", comments)
	}
}
