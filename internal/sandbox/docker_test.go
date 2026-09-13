package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeRunner scripts subprocess results by argv and records every call.
type fakeRunner struct {
	calls [][]string
	// respond maps the joined argv to a result; unmatched calls succeed empty.
	respond func(name string, args []string) (RunResult, error)
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) (RunResult, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.respond != nil {
		return f.respond(name, args)
	}
	return RunResult{}, nil
}

// okGit responds to the git half of Provision: fresh workspace (rev-parse
// fails), successful clone, clean status.
func okGit(name string, args []string) (RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "git" && strings.Contains(argv, "rev-parse"):
		return RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
	case name == "git" && strings.HasPrefix(argv, "clone "):
		return RunResult{}, nil
	case name == "git" && strings.Contains(argv, "status"):
		return RunResult{Stdout: ""}, nil
	}
	return RunResult{}, nil
}

func TestProvisionCloneFailureAborts(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		argv := strings.Join(args, " ")
		if name == "git" && strings.Contains(argv, "rev-parse") {
			return RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
		}
		if name == "git" && strings.HasPrefix(argv, "clone ") {
			return RunResult{ExitCode: 128, Stderr: "repository not found"}, nil
		}
		return RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	_, err := p.Provision(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("failed clone must abort with a clone error, got %v", err)
	}
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "docker" {
			t.Errorf("failed clone must never reach docker, ran %v", c)
		}
	}
}

// argvOf returns the recorded call starting with prefix, or nil.
func argvOf(calls [][]string, binary, verb string) []string {
	for _, c := range calls {
		if len(c) >= 3 && c[0] == binary && c[1] == verb {
			return c
		}
	}
	return nil
}

func testSpec() Spec {
	return Spec{
		TaskID: "task_abc123", Repository: "acme/web",
		Branch: "herder/7-fix-refresh", WorkspaceRoot: "/tmp/herder-test-sb",
	}
}

func TestContainerNameDeterministic(t *testing.T) {
	if got := ContainerName("task_abc123"); got != "herder-task_abc123" {
		t.Errorf("name = %q, want herder-task_abc123", got)
	}
	first, second := ContainerName("task_abc123"), ContainerName("task_abc123")
	if first != second {
		t.Error("same task must map to the same container")
	}
	if first == ContainerName("task_xyz999") {
		t.Error("distinct tasks must map to distinct containers")
	}
	for _, c := range ContainerName("TASK A/B#C") {
		allowed := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !allowed {
			t.Errorf("name %q carries unsafe character %q", ContainerName("TASK A/B#C"), c)
		}
	}
}

func TestProvisionCreatesLeastPrivilegeContainer(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		if out, err := okGit(name, args); name == "git" {
			return out, err
		}
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect --format") {
			return RunResult{ExitCode: 1, Stderr: "No such container"}, nil
		}
		return RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	sb, err := p.Provision(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID != "herder-task_abc123" || sb.Status != "running" || sb.Branch != "herder/7-fix-refresh" {
		t.Errorf("sandbox = %+v, want deterministic id/running/branch", sb)
	}
	create := argvOf(f.calls, "docker", "create")
	if create == nil {
		t.Fatal("provision must docker create the missing container")
	}
	joined := strings.Join(create, " ")
	for _, want := range []string{
		"--cap-drop ALL", "no-new-privileges", "--pids-limit 256",
		"--cpus 2", "--memory 4g", "--network bridge", "--user",
		"/tmp/herder-test-sb/task_abc123:/workspace:rw",
		"herder-managed=true", "sleep infinity",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("create argv %q should contain %q", joined, want)
		}
	}
	wantUser := fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())
	if !strings.Contains(joined, wantUser) {
		t.Errorf("create argv %q should run the worker as %q", joined, wantUser)
	}
	for _, forbidden := range []string{
		"--privileged", "--network host", "--pid host", "--ipc host",
		"--uts host", "docker.sock", "herdr", ":/root", ":/home/",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("create argv %q must never contain %q", joined, forbidden)
		}
	}
}

func TestProvisionPreservesDirtyWorkspace(t *testing.T) {
	var created bool
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		argv := strings.Join(args, " ")
		if name == "git" && strings.Contains(argv, "rev-parse") {
			return RunResult{}, nil // existing checkout
		}
		if name == "git" && strings.Contains(argv, "status") {
			return RunResult{Stdout: " M internal/api/server.go\n?? scratch.txt\n"}, nil
		}
		if name == "docker" && args[0] == "create" {
			created = true
		}
		return RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	_, err := p.Provision(context.Background(), testSpec())
	var dirty *DirtyError
	if !errors.As(err, &dirty) {
		t.Fatalf("dirty re-provision must fail with DirtyError, got %v", err)
	}
	if created {
		t.Error("dirty re-provision must never create a container")
	}
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "docker" {
			t.Errorf("dirty re-provision must not touch docker, ran %v", c)
		}
	}
}

func TestProvisionReusesRunningContainer(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		if out, err := okGit(name, args); name == "git" {
			return out, err
		}
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect --format") {
			return RunResult{Stdout: "running\n"}, nil
		}
		return RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	if _, err := p.Provision(context.Background(), testSpec()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if argvOf(f.calls, "docker", "create") != nil {
		t.Error("running container must be reused, not recreated")
	}
}

func TestExecReturnsOutputAndExit(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		return RunResult{Stdout: "ok\n", Stderr: "warn\n", ExitCode: 3}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	res, err := p.Exec(context.Background(), "herder-task_abc123", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != "ok\n" || res.Stderr != "warn\n" || res.ExitCode != 3 {
		t.Errorf("result = %+v, want split streams with exit 3", res)
	}
	if len(f.calls) != 1 {
		t.Fatalf("exec must run one docker call, ran %v", f.calls)
	}
	want := []string{"docker", "exec", "herder-task_abc123", "go", "test", "./..."}
	if strings.Join(f.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("exec argv = %v, want %v (docker exec takes no --)", f.calls[0], want)
	}
}

func TestExecUnknownSandboxIsNotFound(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		return RunResult{ExitCode: 1, Stderr: "Error: No such container: herder-gone"}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	_, err := p.Exec(context.Background(), "herder-gone", []string{"true"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("exec on unknown sandbox must wrap ErrNotFound, got %v", err)
	}
}

func TestDestroyUnknownIsLoggedNoOp(t *testing.T) {
	var logged []string
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		// docker rm -f masks absence with exit 0, so Destroy pre-checks.
		if name == "docker" && args[0] == "inspect" {
			return RunResult{ExitCode: 1, Stderr: "Error: No such container"}, nil
		}
		return RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(f string, a ...any) { logged = append(logged, f) }}
	if err := p.Destroy(context.Background(), "herder-gone"); err != nil {
		t.Errorf("destroy of unknown sandbox must be a no-op, got %v", err)
	}
	if len(logged) != 1 {
		t.Errorf("destroy no-op must log once, logged %v", logged)
	}
	if argvOf(f.calls, "docker", "rm") != nil {
		t.Errorf("destroy no-op must not remove anything, ran %v", f.calls)
	}
}

func TestShellArgvEntersContainerInteractively(t *testing.T) {
	got := ShellArgv("herder-task_abc123")
	want := []string{"exec", "-it", "herder-task_abc123", "/bin/sh"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("shell argv = %v, want %v", got, want)
	}
}

func TestStopUnknownIsLoggedNoOp(t *testing.T) {
	var logged int
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		return RunResult{ExitCode: 1, Stderr: "Error: No such object"}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) { logged++ }}
	if err := p.Stop(context.Background(), "herder-gone"); err != nil {
		t.Errorf("stop of unknown sandbox must be a no-op, got %v", err)
	}
	if logged != 1 {
		t.Errorf("stop no-op must log once, logged %d", logged)
	}
}

func TestInspectParsesLiveSandbox(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		return RunResult{Stdout: `[{"Id":"abc","Name":"/herder-task_abc123",` +
			`"Config":{"Image":"golang:1.22-bookworm"},` +
			`"State":{"Status":"running"},` +
			`"Mounts":[{"Source":"/state/task_abc123","Destination":"/workspace"}]}]`}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	sb, err := p.Inspect(context.Background(), "herder-task_abc123")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if sb.Status != "running" || sb.Workspace != "/state/task_abc123" || sb.ContainerID != "abc" {
		t.Errorf("sandbox = %+v, want status/workspace/id", sb)
	}
	if _, err := p.Inspect(context.Background(), ""); err == nil {
		t.Error("empty id must fail")
	}
}

func TestListShowsManagedSandboxes(t *testing.T) {
	f := &fakeRunner{}
	f.respond = func(name string, args []string) (RunResult, error) {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "label=herder-managed=true") {
			t.Errorf("list must filter managed containers, ran %v", args)
		}
		return RunResult{Stdout: "abc\therder-task_a\timg\tUp 3 minutes\n"}, nil
	}
	p := &DockerProvider{Runner: f.run, Log: func(string, ...any) {}}
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "herder-task_a" {
		t.Errorf("list = %+v, want one managed sandbox", got)
	}
}

func TestNormalizeMemory(t *testing.T) {
	for in, want := range map[string]string{
		"4Gi": "4g", "512Mi": "512m", "1G": "1g", "2048m": "2048m", "2": "2",
	} {
		if got := normalizeMemory(in); got != want {
			t.Errorf("normalizeMemory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProvisionRejectsEmptySpec(t *testing.T) {
	p := &DockerProvider{Runner: (&fakeRunner{}).run, Log: func(string, ...any) {}}
	if _, err := p.Provision(context.Background(), Spec{}); err == nil {
		t.Error("empty spec must fail")
	}
}
