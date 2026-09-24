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

	"github.com/tmih06/herder/internal/machine"
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
// Docker CLI (SPEC section 24). StateDir roots the controller's SSH
// assets (<statedir>/ssh); when set, Provision also wires the container
// as a saved herdr SSH machine (issue #19). Machines is the injectable
// herdr-machine registry; nil defaults to the real CLI through Runner.
type DockerProvider struct {
	// Runner executes subprocesses; DefaultRunner in production.
	Runner Runner
	// Log receives no-op notes (stop/destroy of unknown sandboxes).
	// Defaults to discard.
	Log func(format string, args ...any)
	// StateDir roots the controller's SSH assets (<statedir>/ssh); empty
	// disables SSH/machine wiring for bare-provider use.
	StateDir string
	// Machines registers workers as saved herdr SSH machines; nil
	// defaults to a registry on Runner.
	Machines *machine.Registry
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
// create-or-start container, unpausing first when paused (docker start
// cannot wake one). Dirty work fails with *DirtyError before any
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
	if out, err := p.run(ctx, "git", GitArgs(workspace, "checkout", "-B", spec.Branch)...); err != nil {
		return nil, err
	} else if out.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: checkout %s: %s", spec.Branch, textutil.FirstLine(out.Stderr))
	}
	// Record the upstream base before the agent runs: the gate diffs
	// against this SHA because refs inside the workspace are
	// agent-writable and a later merge-base would be forgeable.
	baseSHA := ""
	if out, err := p.run(ctx, "git", GitArgs(workspace, "rev-parse", "origin/HEAD")...); err == nil &&
		out.ExitCode == 0 {
		baseSHA = strings.TrimSpace(out.Stdout)
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
	// docker start fails on a paused container, which would requeue a
	// QUEUED task forever; thaw before the start below.
	if state == "paused" {
		if err := p.Unpause(ctx, name); err != nil {
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
	// The entrypoint is `herdr server`: an image without herdr exits
	// immediately, and every later step (ssh injection, machine add)
	// would fail against a dead container with a misleading error.
	// Surface the real cause now — the logs name the missing binary.
	if p.StateDir != "" {
		if now, err := p.containerState(ctx, name); err != nil {
			return nil, err
		} else if now != "running" {
			logs := ""
			if out, err := p.run(ctx, "docker", "logs", "--tail", "5", name); err == nil {
				logs = textutil.FirstLine(out.Stdout + out.Stderr)
			}
			return nil, fmt.Errorf("sandbox: %s exited at start (%s); the worker image must ship herdr and openssh-server (see docs/setup.md)",
				name, orDefault(logs, "no logs"))
		}
	}
	sb := &Sandbox{
		ID: name, TaskID: spec.TaskID, Image: imageOf(spec),
		Status: "running", Branch: spec.Branch, Workspace: workspace,
		BaseSHA: baseSHA,
	}
	// The container-local herdr server is the worker's control surface:
	// wire SSH access and register the machine so `herdr --machine
	// <task>` drives it. Skipped only when no state dir is configured —
	// a bare provider used outside the controller.
	if p.StateDir != "" {
		if err := p.ensureSSH(ctx, spec.TaskID, name); err != nil {
			return nil, err
		}
		m, err := p.MachineRegistry().Ensure(ctx, spec.TaskID, name)
		if err != nil {
			return nil, err
		}
		sb.MachineID = m.ID
	}
	return sb, nil
}

// remoteReadyTimeout bounds the wait for the container-local herdr
// server to open its socket after the container starts.
const remoteReadyTimeout = 30 * time.Second

// MachineRegistry resolves the injectable registry default, adapting the
// provider's Runner seam to the machine package's result type.
func (p *DockerProvider) MachineRegistry() *machine.Registry {
	if p.Machines != nil {
		return p.Machines
	}
	return &machine.Registry{Runner: func(ctx context.Context, name string, args ...string) (machine.RunResult, error) {
		out, err := p.run(ctx, name, args...)
		return machine.RunResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}, err
	}}
}

// ensureSSH converges the worker's SSH access (issue #19): the
// controller keypair and per-task host key exist under <statedir>/ssh,
// the Host block is written and included from ~/.ssh/config, the
// container has a passwd entry for the worker uid (sshd needs a named
// login), and authorized_keys plus the host key are injected. Every step
// is idempotent so a re-provision converges instead of duplicating.
// The in-container herdr server is polled ready before returning so the
// caller's `machine add` never races a half-started server.
func (p *DockerProvider) ensureSSH(ctx context.Context, taskID, container string) error {
	a := SSHAssets{StateDir: p.StateDir, Container: container}
	if err := os.MkdirAll(a.SSHDir(), 0o700); err != nil {
		return fmt.Errorf("sandbox: create ssh dir: %w", err)
	}
	for _, key := range []string{a.IdentityFile(), a.HostKeyFile()} {
		if _, err := os.Stat(key); err == nil {
			continue
		}
		out, err := p.run(ctx, "ssh-keygen", KeygenArgv(key)...)
		if err != nil {
			return err
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("sandbox: ssh-keygen %s: %s", key, textutil.FirstLine(out.Stderr))
		}
	}
	if err := a.WriteConfig(); err != nil {
		return err
	}
	if err := a.EnsureSSHInclude(); err != nil {
		return err
	}
	// sshd authenticates a named login, so the container uid needs a
	// passwd entry. uid 0 appends it (root can write root-owned files
	// even with every capability dropped); a pre-existing entry with a
	// different uid would break the same-uid sshd -i path, so it is a
	// hard error, not a silent skip.
	uid, gid := os.Getuid(), os.Getgid()
	passwdScript := fmt.Sprintf(
		`grep -q '^%s:' /etc/passwd || echo '%s:*:%d:%d::%s:/bin/sh' >> /etc/passwd; grep '^%s:' /etc/passwd`,
		SSHUser, SSHUser, uid, gid, ContainerHome, SSHUser)
	out, err := p.run(ctx, "docker", "exec", "-u", "0", container, "sh", "-c", passwdScript)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("sandbox: provision ssh user in %s: %s", container, textutil.FirstLine(out.Stderr))
	}
	entry := strings.TrimSpace(out.Stdout)
	parts := strings.Split(entry, ":")
	if len(parts) < 4 || parts[2] != strconv.Itoa(uid) {
		return fmt.Errorf("sandbox: container %s already has a %s user with a different uid (%s); cannot run sshd as the worker uid",
			container, SSHUser, entry)
	}
	// Key injection: docker cp preserves the source file's owner and
	// mode, so the worker uid owns both files and the host key stays
	// 0600 — no chown needed (CAP_CHOWN is dropped).
	if out, err := p.run(ctx, "docker", "exec", container, "mkdir", "-p", ContainerSSHDir); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("sandbox: create %s in %s: %s", ContainerSSHDir, container, textutil.FirstLine(out.Stderr))
	}
	for _, copy := range [][2]string{
		{a.PublicKeyFile(), container + ":" + ContainerAuthorizedKeys},
		{a.HostKeyFile(), container + ":" + ContainerHostKeyPath},
	} {
		out, err := p.run(ctx, "docker", "cp", copy[0], copy[1])
		if err != nil {
			return err
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("sandbox: inject %s into %s: %s", copy[0], container, textutil.FirstLine(out.Stderr))
		}
	}
	return p.waitRemoteReady(ctx, container)
}

// create builds the least-privilege container (SPEC section 24): an
// unprivileged uid matching the workspace owner (cap-drop removes even
// root's DAC override, so the worker must own its files), dropped
// capabilities, no-new-privileges, CPU/memory/process caps, bridge
// networking, and exactly one host mount (the task workspace). Host
// namespaces, the Docker and host Herdr sockets, and host home
// directories are never mounted or shared: every mount below names only
// the workspace. No port is published — SSH reaches the container
// through `docker exec` (issue #19).
// The entrypoint is the container-local herdr server itself: PID 1 is a
// session leader, which is exactly what herdr's saved-machine check
// requires (detached_server_daemon), and a dead server exits the
// container so reconcile sees a dead worker instead of a live shell.
// HOME points at the container-local home the provisioner creates.
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
		"--workdir", ContainerWorkspace,
		"--volume", workspace + ":" + ContainerWorkspace + ":rw",
		"--env", "HERDER_TASK=" + spec.TaskID,
		"--env", "HOME=" + ContainerHome,
		"--label", "herder-task=" + spec.TaskID,
		"--label", "herder-managed=true",
		imageOf(spec),
		"sh", "-c", "mkdir -p \"$HOME/.ssh\" && exec herdr server",
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

// waitRemoteReady polls the container-local herdr server until its
// socket answers: the entrypoint starts it at container start, and
// `machine add` must not race a server that has not opened its socket
// yet (the add would spawn a duplicate daemon). Bounded by
// remoteReadyTimeout; the last probe error is returned on expiry.
func (p *DockerProvider) waitRemoteReady(ctx context.Context, container string) error {
	ctx, cancel := context.WithTimeout(ctx, remoteReadyTimeout)
	defer cancel()
	probe := []string{"exec", "-e", "HOME=" + ContainerHome, container, "herdr", "status", "server"}
	var last string
	for {
		out, err := p.run(ctx, "docker", probe...)
		if err == nil && out.ExitCode == 0 && strings.Contains(out.Stdout, "status: running") {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = textutil.FirstLine(out.Stderr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox: herdr server in %s not ready: %s (the worker image must ship herdr and openssh-server — see docs/setup.md)", container, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ensureRepo leaves an independent checkout in workspace: existing repos
// are kept, otherwise the repository is cloned. A failed clone fails
// provisioning outright: an empty fallback would masquerade as a checkout
// and hide auth, network, or naming failures from the controller.
func (p *DockerProvider) ensureRepo(ctx context.Context, spec Spec, workspace string) error {
	if out, err := p.run(ctx, "git", GitArgs(workspace, "rev-parse", "--git-dir")...); err != nil {
		return err
	} else if out.ExitCode == 0 {
		return nil
	}
	remote := spec.RemoteURL
	if remote == "" {
		remote = fmt.Sprintf("https://github.com/%s.git", spec.Repository)
	}
	// The clone runs on the controller host under the controller's own
	// GitHub identity: the gh credential helper authenticates private
	// repositories without a token ever entering the sandbox (same
	// pattern as deliver.PushBranch). It attaches only to https remotes —
	// a local path must never shell out to gh at all.
	args := []string{"-c", "credential.helper="}
	if strings.HasPrefix(remote, "https://") {
		args = append(args, "-c", "credential.https://github.com.helper=!gh auth git-credential")
	}
	args = append(args, "clone", remote, workspace)
	if out, err := p.run(ctx, "git", args...); err != nil {
		return err
	} else if out.ExitCode != 0 {
		return fmt.Errorf("sandbox: clone %s: %s", remote, textutil.FirstLine(out.Stderr))
	}
	return nil
}

// dirtyFiles lists uncommitted paths, capped for the error message.
func (p *DockerProvider) dirtyFiles(ctx context.Context, workspace string) ([]string, error) {
	out, err := p.run(ctx, "git", GitArgs(workspace, "status", "--porcelain")...)
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
		Status    string `json:"Status"`
		OOMKilled bool   `json:"OOMKilled"`
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
		OOMKilled: info.State.OOMKilled,
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

// Destroy removes the container and its control-plane residue: the saved
// herdr machine profile (a destroyed worker must not accumulate dead
// sidebar machines) and the per-task SSH config/known_hosts/host key.
// An unknown or already-removed sandbox is a logged no-op rather than
// an error cascade; machine and file cleanup are best-effort so a dead
// herdr never blocks container removal.
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
	} else {
		out, err := p.run(ctx, "docker", "rm", "-f", id)
		if err != nil {
			return err
		}
		if out.ExitCode != 0 {
			if isNoSuch(out.Stderr) {
				p.logf("herder: sandbox %s already removed (destroy no-op)", id)
			} else {
				return fmt.Errorf("sandbox: destroy %s: %s", id, textutil.FirstLine(out.Stderr))
			}
		}
	}
	// The machine profile keys on the SSH target, which is the container
	// name — removal works even when the task id is unknown here. Gated
	// on StateDir like Provision: a bare provider never registered a
	// machine, and an ungated sweep could match profiles it did not
	// create.
	if p.StateDir != "" {
		if err := p.MachineRegistry().Remove(ctx, "", id); err != nil {
			p.logf("herder: remove machine profile for %s: %v", id, err)
		}
		a := SSHAssets{StateDir: p.StateDir, Container: id}
		if err := a.RemoveConfig(); err != nil {
			p.logf("herder: remove ssh config for %s: %v", id, err)
		}
	}
	return nil
}

// isNoSuch reports Docker's unknown-container stderr.
func isNoSuch(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no such container") ||
		strings.Contains(lower, "no such object")
}

// GitArgs prefixes every workspace git call with overrides that
// neutralize repo-controlled config: once the agent has run, .git is
// agent-writable, so hooks and fsmonitor must never execute on the
// controller host.
func GitArgs(workspace string, args ...string) []string {
	return append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-C", workspace,
	}, args...)
}
