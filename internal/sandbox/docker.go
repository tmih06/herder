package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/textutil"
)

// RunResult is one finished subprocess: split streams plus exit status.
// ExitCode is the process exit; err is non-nil only when the binary could
// not run at all (missing, signaled, context done).
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner runs one subprocess; the injectable seam behind DockerProvider.
// Production uses DefaultRunner (LookPath-resolved binaries, timeout);
// tests script canned results and assert argv instead of needing a daemon.
type Runner func(ctx context.Context, name string, args ...string) (RunResult, error)

// runTimeout bounds one docker/git invocation.
const runTimeout = 2 * time.Minute

// DefaultRunner resolves name in PATH and captures both streams,
// translating ExitError into ExitCode so "command failed" (a normal exec
// outcome) is data, not a transport error.
func DefaultRunner(ctx context.Context, name string, args ...string) (RunResult, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return RunResult{}, fmt.Errorf("sandbox: %s not in PATH: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	out := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		return out, nil
	}
	return out, fmt.Errorf("sandbox: run %s: %w", name, runErr)
}

// DockerProvider provisions least-privilege worker containers through the
// Docker CLI (SPEC section 24).
type DockerProvider struct {
	// Runner executes subprocesses; DefaultRunner in production.
	Runner Runner
	// Log receives no-op notes (stop/destroy of unknown sandboxes).
	// Defaults to discard.
	Log func(format string, args ...any)
}

// NewDockerProvider builds a DockerProvider on the real CLIs.
func NewDockerProvider() *DockerProvider {
	return &DockerProvider{Runner: DefaultRunner, Log: func(string, ...any) {}}
}

// Name reports the config provider key.
func (p *DockerProvider) Name() string { return "docker" }

// run executes one subprocess through the injectable Runner, falling back
// to DefaultRunner when none is set.
func (p *DockerProvider) run(ctx context.Context, name string, args ...string) (RunResult, error) {
	if p.Runner != nil {
		return p.Runner(ctx, name, args...)
	}
	return DefaultRunner(ctx, name, args...)
}

// logf reports no-op notes to the caller's log, discarding when unset.
func (p *DockerProvider) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// Provision ensures the workspace checkout and a running container for
// spec, never discarding uncommitted work.
// Why: re-provisioning the same task must converge, not destroy.
// Flow: validate -> ensure repo -> dirty guard -> checkout branch ->
// create-or-start container. Dirty work fails with *DirtyError before any
// container call; missing containers are created least-privilege.
func (p *DockerProvider) Provision(ctx context.Context, spec Spec) (*Sandbox, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	name := ContainerName(spec.TaskID)
	workspace := WorkspacePath(spec.WorkspaceRoot, spec.TaskID)
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		return nil, fmt.Errorf("sandbox: create workspace: %w", err)
	}
	if err := p.ensureRepo(ctx, spec, workspace); err != nil {
		return nil, err
	}
	if dirty, err := p.dirtyFiles(ctx, workspace); err != nil {
		return nil, err
	} else if len(dirty) > 0 {
		return nil, &DirtyError{TaskID: spec.TaskID, Branch: spec.Branch, Files: dirty}
	}
	if out, err := p.run(ctx, "git", "-C", workspace, "checkout", "-B", spec.Branch); err != nil {
		return nil, err
	} else if out.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: checkout %s: %s", spec.Branch, textutil.FirstLine(out.Stderr))
	}
	state, err := p.containerState(ctx, name)
	if err != nil {
		return nil, err
	}
	if state == "" {
		if err := p.create(ctx, spec, name, workspace); err != nil {
			return nil, err
		}
	}
	if state != "running" {
		if out, err := p.run(ctx, "docker", "start", name); err != nil {
			return nil, err
		} else if out.ExitCode != 0 {
			return nil, fmt.Errorf("sandbox: start %s: %s", name, textutil.FirstLine(out.Stderr))
		}
	}
	return &Sandbox{
		ID: name, TaskID: spec.TaskID, Image: imageOf(spec),
		Status: "running", Branch: spec.Branch, Workspace: workspace,
	}, nil
}

// ensureRepo leaves an independent checkout in workspace: existing repos
// are kept, otherwise the repository is cloned. A failed clone fails
// provisioning outright: an empty fallback would masquerade as a checkout
// and hide auth, network, or naming failures from the controller.
func (p *DockerProvider) ensureRepo(ctx context.Context, spec Spec, workspace string) error {
	if out, err := p.run(ctx, "git", "-C", workspace, "rev-parse", "--git-dir"); err != nil {
		return err
	} else if out.ExitCode == 0 {
		return nil
	}
	remote := fmt.Sprintf("https://github.com/%s.git", spec.Repository)
	if out, err := p.run(ctx, "git", "clone", remote, workspace); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("sandbox: clone %s: %s", remote, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// dirtyFiles lists uncommitted paths, capped for the error message.
func (p *DockerProvider) dirtyFiles(ctx context.Context, workspace string) ([]string, error) {
	out, err := p.run(ctx, "git", "-C", workspace, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: git status: %s", textutil.FirstLine(out.Stderr))
	}
	var files []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		files = append(files, strings.TrimSpace(line))
		if len(files) >= 5 {
			break
		}
	}
	return files, nil
}

// containerState reports the engine status, or "" when absent.
func (p *DockerProvider) containerState(ctx context.Context, name string) (string, error) {
	out, err := p.run(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			return "", nil
		}
		return "", fmt.Errorf("sandbox: inspect %s: %s", name, textutil.FirstLine(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

// create builds the least-privilege container (SPEC section 24): an
// unprivileged uid matching the workspace owner (cap-drop removes even
// root's DAC override, so the worker must own its files), dropped
// capabilities, no-new-privileges, CPU/memory/process caps, bridge
// networking, and exactly one host mount (the task workspace). Host
// namespaces, the Docker and Herdr sockets, and host home directories are
// never mounted or shared: every mount below names only the workspace.
func (p *DockerProvider) create(ctx context.Context, spec Spec, name, workspace string) error {
	args := []string{
		"create",
		"--name", name,
		"--hostname", name,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cpus", cpusOf(spec),
		"--memory", memoryOf(spec),
		"--pids-limit", strconv.Itoa(pidsOf(spec)),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--network", "bridge",
		"--workdir", "/workspace",
		"--volume", workspace + ":/workspace:rw",
		"--env", "HERDER_TASK=" + spec.TaskID,
		"--label", "herder-task=" + spec.TaskID,
		"--label", "herder-managed=true",
		imageOf(spec),
		"sleep", "infinity",
	}
	out, err := p.run(ctx, "docker", args...)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("sandbox: create %s: %s", name, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// Exec runs cmd inside the sandbox and returns output plus exit status.
// A nonzero command exit is data (Result.ExitCode), not an error; only
// transport failures and unknown containers error. Note: docker exec
// takes no -- separator; the container id already delimits flags.
func (p *DockerProvider) Exec(ctx context.Context, id string, cmd []string) (*Result, error) {
	if strings.TrimSpace(id) == "" || len(cmd) == 0 {
		return nil, errors.New("sandbox: exec needs a sandbox id and a command")
	}
	args := append([]string{"exec", id}, cmd...)
	out, err := p.run(ctx, "docker", args...)
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 && isNoSuch(out.Stderr) {
		return nil, fmt.Errorf("sandbox: exec %s: %w", id, ErrNotFound)
	}
	return &Result{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}, nil
}

// ShellArgv builds the interactive shell entry: docker exec -it wiring
// the caller's stdio to /bin/sh. Kept here so the exact entry is tested.
func ShellArgv(id string) []string {
	return []string{"exec", "-it", id, "/bin/sh"}
}

// inspectJSON mirrors the docker inspect fields Herder reads.
type inspectJSON struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// Inspect reports one sandbox with its current status (running or
// stopped) or ErrNotFound; callers gate on Sandbox.Status.
func (p *DockerProvider) Inspect(ctx context.Context, id string) (*Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sandbox: inspect needs a sandbox id")
	}
	out, err := p.run(ctx, "docker", "inspect", id)
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			return nil, fmt.Errorf("sandbox: inspect %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("sandbox: inspect %s: %s", id, textutil.FirstLine(out.Stderr))
	}
	var parsed []inspectJSON
	if err := json.Unmarshal([]byte(out.Stdout), &parsed); err != nil || len(parsed) == 0 {
		return nil, fmt.Errorf("sandbox: inspect %s: unreadable output", id)
	}
	info := parsed[0]
	sb := &Sandbox{
		ID: strings.TrimPrefix(info.Name, "/"), ContainerID: info.ID,
		Image: info.Config.Image, Status: info.State.Status,
	}
	for _, m := range info.Mounts {
		if m.Destination == "/workspace" {
			sb.Workspace = m.Source
		}
	}
	return sb, nil
}

// List reports every Herder-managed container, oldest first.
func (p *DockerProvider) List(ctx context.Context) ([]Sandbox, error) {
	out, err := p.run(ctx, "docker", "ps", "-a",
		"--filter", "label=herder-managed=true",
		"--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: list: %s", textutil.FirstLine(out.Stderr))
	}
	var sandboxes []Sandbox
	for _, line := range strings.Split(out.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		sandboxes = append(sandboxes, Sandbox{
			ContainerID: fields[0], ID: fields[1], Image: fields[2], Status: fields[3],
		})
	}
	if sandboxes == nil {
		sandboxes = []Sandbox{}
	}
	return sandboxes, nil
}

// Stop halts the container; an unknown or already-removed sandbox is a
// logged no-op rather than an error cascade.
func (p *DockerProvider) Stop(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sandbox: stop needs a sandbox id")
	}
	out, err := p.run(ctx, "docker", "stop", id)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			p.logf("herder: sandbox %s already removed (stop no-op)", id)
			return nil
		}
		return fmt.Errorf("sandbox: stop %s: %s", id, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// Pause freezes every process in the container (docker pause, cgroup
// freezer): task progress halts while the Herdr session and the agent
// stay alive, which is exactly what `herder task pause` needs (SPEC
// section 22). Unlike Stop, a missing container is ErrNotFound — claiming
// a freeze on nothing would lie about the task's state.
func (p *DockerProvider) Pause(ctx context.Context, id string) error {
	return p.freeze(ctx, id, "pause")
}

// Unpause thaws a paused container so the agent resumes mid-session.
// A missing container is ErrNotFound for the same reason as Pause.
func (p *DockerProvider) Unpause(ctx context.Context, id string) error {
	return p.freeze(ctx, id, "unpause")
}

// freeze runs docker pause/unpause behind one implementation: both are
// single-word subcommands with identical error shape.
func (p *DockerProvider) freeze(ctx context.Context, id, verb string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("sandbox: %s needs a sandbox id", verb)
	}
	out, err := p.run(ctx, "docker", verb, id)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			return fmt.Errorf("sandbox: %s %s: %w", verb, id, ErrNotFound)
		}
		return fmt.Errorf("sandbox: %s %s: %s", verb, id, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// EnsureRunning converges a sandbox toward running without touching the
// workspace: paused containers unpause, stopped ones start, running ones
// are a no-op. Unlike Provision it never inspects or resets the checkout,
// so retry/handoff can revive a worker's container without tripping the
// dirty-state guard on the previous attempt's uncommitted work.
// ErrNotFound means the container is gone and the caller must provision.
func (p *DockerProvider) EnsureRunning(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sandbox: ensure-running needs a sandbox id")
	}
	state, err := p.containerState(ctx, id)
	if err != nil {
		return err
	}
	switch state {
	case "running":
		return nil
	case "paused":
		return p.Unpause(ctx, id)
	case "":
		return fmt.Errorf("sandbox: ensure-running %s: %w", id, ErrNotFound)
	}
	out, err := p.run(ctx, "docker", "start", id)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			return fmt.Errorf("sandbox: ensure-running %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("sandbox: start %s: %s", id, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// Destroy removes the container; an unknown or already-removed sandbox
// is a logged no-op rather than an error cascade.
func (p *DockerProvider) Destroy(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sandbox: destroy needs a sandbox id")
	}
	// docker rm -f is exit-0 idempotent: absence never surfaces in its
	// status, so check first to keep the no-op explicit and logged. A
	// container lost to a race still removes cleanly below.
	state, err := p.containerState(ctx, id)
	if err != nil {
		return err
	}
	if state == "" {
		p.logf("herder: sandbox %s already removed (destroy no-op)", id)
		return nil
	}
	out, err := p.run(ctx, "docker", "rm", "-f", id)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		if isNoSuch(out.Stderr) {
			p.logf("herder: sandbox %s already removed (destroy no-op)", id)
			return nil
		}
		return fmt.Errorf("sandbox: destroy %s: %s", id, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// isNoSuch reports Docker's unknown-container stderr.
func isNoSuch(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no such container") ||
		strings.Contains(lower, "no such object")
}
