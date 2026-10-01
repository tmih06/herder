package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/machine"
	"github.com/tmih06/herder/internal/testutil"
)

// okGit responds to the git half of Provision: fresh workspace (rev-parse
// fails), successful clone, clean status.
func okGit(name string, args []string) (machine.RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "git" && strings.Contains(argv, "rev-parse"):
		return machine.RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
	case name == "git" && strings.Contains(argv, " clone "):
		return machine.RunResult{}, nil
	case name == "git" && strings.Contains(argv, "status"):
		return machine.RunResult{Stdout: ""}, nil
	}
	return machine.RunResult{}, nil
}

func TestProvisionCloneFailureAborts(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		argv := strings.Join(args, " ")
		if name == "git" && strings.Contains(argv, "rev-parse") {
			return machine.RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
		}
		if name == "git" && strings.Contains(argv, " clone ") {
			return machine.RunResult{ExitCode: 128, Stderr: "repository not found"}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	_, err := p.Provision(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("failed clone must abort with a clone error, got %v", err)
	}
	for _, c := range f.Calls() {
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
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		if out, err := okGit(name, args); name == "git" {
			return out, err
		}
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect --format") {
			return machine.RunResult{ExitCode: 1, Stderr: "No such container"}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	sb, err := p.Provision(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID != "herder-task_abc123" || sb.Status != "running" || sb.Branch != "herder/7-fix-refresh" {
		t.Errorf("sandbox = %+v, want deterministic id/running/branch", sb)
	}
	create := argvOf(f.Calls(), "docker", "create")
	if create == nil {
		t.Fatal("provision must docker create the missing container")
	}
	joined := strings.Join(create, " ")
	for _, want := range []string{
		"--cap-drop ALL", "no-new-privileges", "--pids-limit 1024",
		"--cpus 2", "--memory 4g", "--network bridge", "--user",
		"--workdir /workspace", "--env HOME=/tmp/herder-home",
		"/tmp/herder-test-sb/task_abc123:/workspace:rw",
		"herder-managed=true",
		`sh -c mkdir -p "$HOME/.ssh" && exec herdr server`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("create argv %q should contain %q", joined, want)
		}
	}
	wantUser := fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())
	if !strings.Contains(joined, wantUser) {
		t.Errorf("create argv %q should run the worker as %q", joined, wantUser)
	}
	// The entrypoint runs `herdr server` inside the container, so the
	// forbidden surface is the host's sockets and mounts, not the word.
	for _, forbidden := range []string{
		"--privileged", "--network host", "--pid host", "--ipc host",
		"--uts host", "docker.sock", "herdr.sock", ":/root", ":/home/",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("create argv %q must never contain %q", joined, forbidden)
		}
	}
}

func TestProvisionPreservesDirtyWorkspace(t *testing.T) {
	var created bool
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		argv := strings.Join(args, " ")
		if name == "git" && strings.Contains(argv, "rev-parse") {
			return machine.RunResult{}, nil // existing checkout
		}
		if name == "git" && strings.Contains(argv, "status") {
			return machine.RunResult{Stdout: " M internal/api/server.go\n?? scratch.txt\n"}, nil
		}
		if name == "docker" && args[0] == "create" {
			created = true
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	_, err := p.Provision(context.Background(), testSpec())
	var dirty *DirtyError
	if !errors.As(err, &dirty) {
		t.Fatalf("dirty re-provision must fail with DirtyError, got %v", err)
	}
	if created {
		t.Error("dirty re-provision must never create a container")
	}
	for _, c := range f.Calls() {
		if len(c) > 0 && c[0] == "docker" {
			t.Errorf("dirty re-provision must not touch docker, ran %v", c)
		}
	}
}

func TestProvisionReusesRunningContainer(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		if out, err := okGit(name, args); name == "git" {
			return out, err
		}
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect --format") {
			return machine.RunResult{Stdout: "running\n"}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	if _, err := p.Provision(context.Background(), testSpec()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if argvOf(f.Calls(), "docker", "create") != nil {
		t.Error("running container must be reused, not recreated")
	}
}

// TestProvisionUnpausesPausedContainer proves a QUEUED task on a paused
// sandbox thaws before docker start: start alone cannot wake a paused
// container, so unpause must precede it in the call log.
func TestProvisionUnpausesPausedContainer(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		if out, err := okGit(name, args); name == "git" {
			return out, err
		}
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect --format") {
			return machine.RunResult{Stdout: "paused\n"}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	if _, err := p.Provision(context.Background(), testSpec()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	unpause, start := -1, -1
	for i, c := range f.Calls() {
		if len(c) >= 2 && c[0] == "docker" {
			switch c[1] {
			case "unpause":
				unpause = i
			case "start":
				start = i
			}
		}
	}
	if unpause < 0 || start < 0 || unpause > start {
		t.Errorf("paused container must unpause before start, calls %v", f.Calls())
	}
	if argvOf(f.Calls(), "docker", "create") != nil {
		t.Error("paused container must be thawed, not recreated")
	}
}

func TestExecReturnsOutputAndExit(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		return machine.RunResult{Stdout: "ok\n", Stderr: "warn\n", ExitCode: 3}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	res, err := p.Exec(context.Background(), "herder-task_abc123", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != "ok\n" || res.Stderr != "warn\n" || res.ExitCode != 3 {
		t.Errorf("result = %+v, want split streams with exit 3", res)
	}
	if len(f.Calls()) != 1 {
		t.Fatalf("exec must run one docker call, ran %v", f.Calls())
	}
	want := []string{"docker", "exec", "herder-task_abc123", "go", "test", "./..."}
	if strings.Join(f.Calls()[0], " ") != strings.Join(want, " ") {
		t.Errorf("exec argv = %v, want %v (docker exec takes no --)", f.Calls()[0], want)
	}
}

func TestExecUnknownSandboxIsNotFound(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		return machine.RunResult{ExitCode: 1, Stderr: "Error: No such container: herder-gone"}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	_, err := p.Exec(context.Background(), "herder-gone", []string{"true"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("exec on unknown sandbox must wrap ErrNotFound, got %v", err)
	}
}

func TestDestroyUnknownIsLoggedNoOp(t *testing.T) {
	var logged []string
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) { // docker rm -f masks absence with exit 0, so Destroy pre-checks.
		if name == "docker" && args[0] == "inspect" {
			return machine.RunResult{ExitCode: 1, Stderr: "Error: No such container"}, nil
		}
		// Destroy always sweeps the machine catalog; an empty list means
		// no profile to remove.
		if name == "herdr" && strings.Join(args, " ") == "machine list --json" {
			return machine.RunResult{Stdout: "[]"}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(f string, a ...any) { logged = append(logged, f) }}
	if err := p.Destroy(context.Background(), "herder-gone"); err != nil {
		t.Errorf("destroy of unknown sandbox must be a no-op, got %v", err)
	}
	if len(logged) != 1 {
		t.Errorf("destroy no-op must log once, logged %v", logged)
	}
	if argvOf(f.Calls(), "docker", "rm") != nil {
		t.Errorf("destroy no-op must not remove anything, ran %v", f.Calls())
	}
	if argvOf(f.Calls(), "herdr", "machine") != nil &&
		strings.Join(argvOf(f.Calls(), "herdr", "machine"), " ") != "herdr machine list --json" {
		t.Errorf("destroy no-op must not remove a machine profile, ran %v", f.Calls())
	}
}

// TestDestroyRemovesMachineProfileAndSSHFiles proves teardown cleans the
// control plane too: every saved machine profile matching the container
// name is removed by id, and the per-task SSH config/known_hosts/host
// key under StateDir are deleted.
func TestDestroyRemovesMachineProfileAndSSHFiles(t *testing.T) {
	stateDir := t.TempDir()
	a := SSHAssets{StateDir: stateDir, Container: "herder-task_abc123"}
	if err := a.WriteConfig(); err != nil {
		t.Fatalf("seed ssh config: %v", err)
	}
	for _, path := range []string{a.KnownHostsFile(), a.HostKeyFile()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		if name == "docker" && args[0] == "inspect" {
			return machine.RunResult{Stdout: "running\n"}, nil
		}
		if name == "herdr" && strings.Join(args, " ") == "machine list --json" {
			return machine.RunResult{Stdout: `[{"id":"m1","label":"task_abc123","target":"herder-task_abc123","enabled":true},` +
				`{"id":"m2","label":"other","target":"herder-task_abc123","enabled":true},` +
				`{"id":"m3","label":"task_other","target":"herder-task_other","enabled":true}]`}, nil
		}
		return machine.RunResult{}, nil
	}
	p := &DockerProvider{Runner: f.Run, StateDir: stateDir, Log: func(string, ...any) {}}
	if err := p.Destroy(context.Background(), "herder-task_abc123"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if argvOf(f.Calls(), "docker", "rm") == nil {
		t.Errorf("destroy must docker rm the live container, ran %v", f.Calls())
	}
	var removed []string
	for _, c := range f.Calls() {
		if len(c) == 4 && c[0] == "herdr" && c[1] == "machine" && c[2] == "remove" {
			removed = append(removed, c[3])
		}
	}
	if strings.Join(removed, ",") != "m1,m2" {
		t.Errorf("machine remove ids = %v, want [m1 m2] (label or target match only)", removed)
	}
	for _, path := range []string{a.ConfigFile(), a.KnownHostsFile(), a.HostKeyFile()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("destroy must remove %s, stat err = %v", path, err)
		}
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
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		return machine.RunResult{ExitCode: 1, Stderr: "Error: No such object"}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) { logged++ }}
	if err := p.Stop(context.Background(), "herder-gone"); err != nil {
		t.Errorf("stop of unknown sandbox must be a no-op, got %v", err)
	}
	if logged != 1 {
		t.Errorf("stop no-op must log once, logged %d", logged)
	}
}

func TestInspectParsesLiveSandbox(t *testing.T) {
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		return machine.RunResult{Stdout: `[{"Id":"abc","Name":"/herder-task_abc123",` +
			`"Config":{"Image":"golang:1.22-bookworm"},` +
			`"State":{"Status":"running"},` +
			`"Mounts":[{"Source":"/state/task_abc123","Destination":"/workspace"}]}]`}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
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
	f := &testutil.Recorder{}
	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "label=herder-managed=true") {
			t.Errorf("list must filter managed containers, ran %v", args)
		}
		return machine.RunResult{Stdout: "abc\therder-task_a\timg\tUp 3 minutes\n"}, nil
	}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
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
	p := &DockerProvider{Runner: (&testutil.Recorder{}).Run, Log: func(string, ...any) {}}
	if _, err := p.Provision(context.Background(), Spec{}); err == nil {
		t.Error("empty spec must fail")
	}
}

// statefulDocker fakes a container whose status moves with pause,
// unpause, and start so EnsureRunning can be tested end to end.
func statefulDocker(status *string) func(string, []string) (machine.RunResult, error) {
	return func(name string, args []string) (machine.RunResult, error) {
		if name != "docker" {
			return machine.RunResult{}, nil
		}
		switch args[0] {
		case "inspect":
			return machine.RunResult{Stdout: *status + "\n"}, nil
		case "pause":
			*status = "paused"
		case "unpause", "start":
			*status = "running"
		}
		return machine.RunResult{}, nil
	}
}

// TestPauseFreezeThaw proves pause and unpause drive docker's freezer
// verbs against the container without touching the workspace.
func TestPauseFreezeThaw(t *testing.T) {
	status := "running"
	f := &testutil.Recorder{Respond: statefulDocker(&status)}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	if err := p.Pause(context.Background(), "herder-task_x"); err != nil {
		t.Fatalf("Pause = %v", err)
	}
	if status != "paused" {
		t.Errorf("container status = %s, want paused", status)
	}
	if err := p.Unpause(context.Background(), "herder-task_x"); err != nil {
		t.Fatalf("Unpause = %v", err)
	}
	if status != "running" {
		t.Errorf("container status = %s, want running", status)
	}
	if argvOf(f.Calls(), "docker", "pause") == nil || argvOf(f.Calls(), "docker", "unpause") == nil {
		t.Errorf("expected docker pause and unpause calls, got %v", f.Calls())
	}
}

// TestEnsureRunningConverges proves the retry/handoff revive path:
// running is a no-op, paused unpauses, stopped starts, missing reports
// ErrNotFound so the caller knows to provision.
func TestEnsureRunningConverges(t *testing.T) {
	status := "running"
	f := &testutil.Recorder{Respond: statefulDocker(&status)}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}

	if err := p.EnsureRunning(context.Background(), "herder-task_x"); err != nil {
		t.Fatalf("EnsureRunning on running = %v", err)
	}
	if len(f.Calls()) != 1 {
		t.Errorf("running container needs only the inspect, ran %v", f.Calls())
	}

	status = "paused"
	if err := p.EnsureRunning(context.Background(), "herder-task_x"); err != nil {
		t.Fatalf("EnsureRunning on paused = %v", err)
	}
	if status != "running" {
		t.Errorf("paused container should thaw, status = %s", status)
	}

	status = "exited"
	if err := p.EnsureRunning(context.Background(), "herder-task_x"); err != nil {
		t.Fatalf("EnsureRunning on exited = %v", err)
	}
	if status != "running" || argvOf(f.Calls(), "docker", "start") == nil {
		t.Errorf("exited container should start, status = %s calls %v", status, f.Calls())
	}

	f.Respond = func(name string, args []string) (machine.RunResult, error) {
		return machine.RunResult{ExitCode: 1, Stderr: "Error: No such container: herder-task_x"}, nil
	}
	if err := p.EnsureRunning(context.Background(), "herder-task_x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing container = %v, want ErrNotFound", err)
	}
}

// A local-path remote clones the filesystem path verbatim: the gh
// credential helper stays URL-scoped and never applies to it.
func TestProvisionClonesLocalRemote(t *testing.T) {
	f := &testutil.Recorder{Respond: okGit}
	p := &DockerProvider{Runner: f.Run, Log: func(string, ...any) {}}
	spec := testSpec()
	spec.RemoteURL = "/srv/git/acme-web.git"
	if _, err := p.Provision(context.Background(), spec); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	var clone []string
	for _, c := range f.Calls() {
		if c[0] == "git" && strings.Contains(strings.Join(c, " "), " clone ") {
			clone = c
		}
	}
	if clone == nil {
		t.Fatal("provision must clone the workspace")
	}
	joined := strings.Join(clone, " ")
	if !strings.Contains(joined, "clone /srv/git/acme-web.git") {
		t.Errorf("clone must use the local path, ran %q", joined)
	}
	if strings.Contains(joined, "github.com") {
		t.Errorf("local clone must never name github.com, ran %q", joined)
	}
}
