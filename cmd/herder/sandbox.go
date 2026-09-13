package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// execOutputCap bounds sandbox.exec event payloads: full output goes to
// the terminal, the durable event keeps the head for later validation use.
const execOutputCap = 4096

// cmdSandbox implements `herder sandbox provision|exec|list|inspect|
// shell|stop|destroy` against the live Docker host and the durable store.
func cmdSandbox(path string, args []string, w, ew io.Writer) int {
	if len(args) == 0 {
		printSandboxUsage(ew)
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "provision":
		return sandboxProvision(path, rest, w, ew)
	case "exec":
		return sandboxExec(path, rest, w, ew)
	case "list":
		return sandboxList(path, w, ew)
	case "inspect":
		return sandboxInspect(path, rest, w, ew)
	case "shell":
		return sandboxShell(path, rest, w, ew)
	case "stop":
		return sandboxStop(path, rest, w, ew)
	case "destroy":
		return sandboxDestroy(path, rest, w, ew)
	default:
		fmt.Fprintf(ew, "herder: unknown sandbox command %q\n\n", verb)
		printSandboxUsage(ew)
		return 2
	}
}

// printSandboxUsage lists the sandbox verbs.
func printSandboxUsage(w io.Writer) {
	fmt.Fprint(w, `usage: herder sandbox <command> [args]

  provision <task-id>         create or reuse the task's container
  exec <id> -- <command...>   run a command inside the sandbox
  list                        show Herder-managed sandboxes
  inspect <id>                show one sandbox
  shell <id>                  open an interactive shell (needs a TTY)
  stop <id>                   halt the container (unknown: logged no-op)
  destroy <id>                remove the container (unknown: logged no-op)

<id> accepts a task id or a container name.
`)
}

// sandboxSetup loads config, opens state, and builds the Docker provider
// logging to ew. The caller owns the store and must close it.
func sandboxSetup(path string, ew io.Writer) (*config.Config, *storage.Store, *sandbox.DockerProvider) {
	cfg := loadConfig(path, ew)
	if cfg == nil {
		return nil, nil, nil
	}
	store := openStore(cfg, ew)
	if store == nil {
		return nil, nil, nil
	}
	provider := sandbox.NewDockerProvider()
	provider.Log = func(format string, args ...any) {
		fmt.Fprintf(ew, format+"\n", args...)
	}
	return cfg, store, provider
}

// specForTask builds the provision spec from the task plus its repo and
// agent config: image from the repo sandbox block, CPU/memory limits from
// the agent profile, workspace rooted beside the state database so it
// survives restarts without a config change.
func specForTask(cfg *config.Config, task tasks.Task) sandbox.Spec {
	repo := cfg.Repositories[task.Repository]
	agent := cfg.Agents[task.AgentProfile]
	return sandbox.Spec{
		TaskID: task.ID, Repository: task.Repository,
		Branch: branchForTask(task), Image: repo.Sandbox.Image,
		CPUs: agent.Resources["cpu"], Memory: agent.Resources["memory"],
		WorkspaceRoot: sandboxRoot(cfg.Database.Path),
	}
}

// branchForTask returns the deterministic worker branch: the claimed
// branch when set, else herder/<issue> parsed from the source ref, else
// herder/<task-id> so provisioning never blocks on an empty branch.
func branchForTask(task tasks.Task) string {
	if strings.TrimSpace(task.BranchName) != "" {
		return strings.TrimSpace(task.BranchName)
	}
	if _, num, ok := strings.Cut(task.SourceRef, "#"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(num)); err == nil && n > 0 {
			return fmt.Sprintf("herder/%d", n)
		}
	}
	return "herder/" + task.ID
}

// sandboxRoot resolves the workspace root beside the state database:
// <dbdir>/sandboxes, with ~/ expanded like the store does.
func sandboxRoot(dbPath string) string {
	if rest, ok := strings.CutPrefix(dbPath, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			dbPath = filepath.Join(home, rest)
		}
	}
	return filepath.Join(filepath.Dir(dbPath), "sandboxes")
}

// resolveTarget maps a task id to its container name, passing container
// names through. The task is non-nil when the target is a known task, so
// callers can link exec output back to durable history.
func resolveTarget(store *storage.Store, target string) (container string, task *tasks.Task) {
	if t, err := store.GetTask(target); err == nil {
		return sandbox.ContainerName(t.ID), &t
	}
	return target, nil
}

// sandboxProvision creates or reuses the task's container and walks the
// task toward RUNNING along legal transitions, recording the durable
// sandbox.provisioned event. Dirty work fails with a preservation note.
func sandboxProvision(path string, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox provision <task-id>\n")
		return 2
	}
	cfg, store, provider := sandboxSetup(path, ew)
	if cfg == nil {
		return 1
	}
	defer store.Close()
	task, err := store.GetTask(args[0])
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q: %v\n", args[0], err)
		return 1
	}
	if repo, ok := cfg.Repositories[task.Repository]; !ok {
		fmt.Fprintf(ew, "herder: task repository %q not in config\n", task.Repository)
		return 1
	} else if repo.Sandbox.Provider != "docker" {
		fmt.Fprintf(ew, "herder: provider %q unsupported here (want docker)\n", repo.Sandbox.Provider)
		return 1
	}
	spec := specForTask(cfg, task)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sb, err := provider.Provision(ctx, spec)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: sandbox %s running (branch %s)\n", sb.ID, sb.Branch)
	advanceToRunning(store, &task, ew)
	payload, _ := json.Marshal(map[string]string{
		"sandbox": sb.ID, "branch": sb.Branch,
		"workspace": sb.Workspace, "image": sb.Image,
	})
	if _, err := store.AppendEvent(task.ID, "sandbox.provisioned", "controller", "cli", string(payload)); err != nil {
		fmt.Fprintf(ew, "herder: record provision event: %v\n", err)
		return 1
	}
	return 0
}

// advanceToRunning walks QUEUED -> PROVISIONING -> RUNNING so the durable
// state reflects the live container. Any other state is left untouched:
// re-provisioning an active task is normal, and the sandbox.provisioned
// event records the run regardless.
func advanceToRunning(store *storage.Store, task *tasks.Task, ew io.Writer) {
	var path []tasks.State
	switch task.Status {
	case tasks.Queued:
		path = []tasks.State{tasks.Provisioning, tasks.Running}
	case tasks.Provisioning:
		path = []tasks.State{tasks.Running}
	default:
		return
	}
	for _, next := range path {
		if _, err := store.Transition(task.ID, next, "controller", "cli"); err != nil {
			fmt.Fprintf(ew, "herder: leaving task in %s (%v)\n", task.Status, err)
			return
		}
		task.Status = next
	}
}

// sandboxExec runs a command inside the sandbox, streams output, records
// a sandbox.exec event on the linked task, and exits with the remote
// status so validation callers can gate on it.
func sandboxExec(path string, args []string, w, ew io.Writer) int {
	target, cmd, ok := splitExecArgs(args)
	if !ok {
		fmt.Fprintf(ew, "herder: usage: herder sandbox exec <task-or-container> -- <command...>\n")
		return 2
	}
	cfg, store, provider := sandboxSetup(path, ew)
	if cfg == nil {
		return 1
	}
	defer store.Close()
	container, task := resolveTarget(store, target)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := provider.Exec(ctx, container, cmd)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprint(w, res.Stdout)
	fmt.Fprint(ew, res.Stderr)
	if task != nil {
		payload, _ := json.Marshal(map[string]any{
			"command": cmd, "exit_code": res.ExitCode,
			"output": truncate(res.Stdout+res.Stderr, execOutputCap),
		})
		if _, err := store.AppendEvent(task.ID, "sandbox.exec", "controller", "cli", string(payload)); err != nil {
			fmt.Fprintf(ew, "herder: record exec event: %v\n", err)
			return 1
		}
	}
	return res.ExitCode
}

// splitExecArgs cuts <id> -- <command...> at the separator.
func splitExecArgs(args []string) (target string, cmd []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			if i == 1 && i+1 < len(args) {
				return args[0], args[i+1:], true
			}
			return "", nil, false
		}
	}
	return "", nil, false
}

// sandboxList prints every Herder-managed sandbox.
func sandboxList(path string, w, ew io.Writer) int {
	_, store, provider := sandboxSetup(path, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sandboxes, err := provider.List(ctx)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "%d sandboxes\n", len(sandboxes))
	for _, sb := range sandboxes {
		fmt.Fprintf(w, "- %s %s %s\n", sb.ID, sb.Status, sb.Image)
	}
	return 0
}

// sandboxInspect prints one sandbox, linking its task and branch when the
// id names a known task.
func sandboxInspect(path string, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox inspect <task-or-container>\n")
		return 2
	}
	_, store, provider := sandboxSetup(path, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	container, task := resolveTarget(store, args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb, err := provider.Inspect(ctx, container)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "sandbox:   %s\nstatus:    %s\nimage:     %s\n", sb.ID, sb.Status, sb.Image)
	if sb.ContainerID != "" {
		fmt.Fprintf(w, "container: %s\n", sb.ContainerID)
	}
	if sb.Workspace != "" {
		fmt.Fprintf(w, "workspace: %s\n", sb.Workspace)
	}
	if task != nil {
		fmt.Fprintf(w, "task:      %s (%s, branch %s)\n", task.ID, task.Status, branchForTask(*task))
	}
	return 0
}

// sandboxShell enters the live container interactively; stdio passes
// straight through, so this needs a real TTY, not test buffers.
func sandboxShell(path string, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox shell <task-or-container>\n")
		return 2
	}
	_, store, _ := sandboxSetup(path, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	container, _ := resolveTarget(store, args[0])
	docker, err := exec.LookPath("docker")
	if err != nil {
		fmt.Fprintf(ew, "herder: docker not in PATH (install Docker to enter sandboxes)\n")
		return 1
	}
	cmd := exec.Command(docker, sandbox.ShellArgv(container)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(ew, "herder: shell %s: %v\n", container, err)
		return 1
	}
	return 0
}

// sandboxStop halts the container; unknown ids log a no-op via the
// provider and still exit 0.
func sandboxStop(path string, args []string, w, ew io.Writer) int {
	return sandboxOneID(path, args, w, ew, "stop", "stopped",
		func(ctx context.Context, p *sandbox.DockerProvider, id string) error {
			return p.Stop(ctx, id)
		})
}

// sandboxDestroy removes the container; unknown ids log a no-op via the
// provider and still exit 0.
func sandboxDestroy(path string, args []string, w, ew io.Writer) int {
	return sandboxOneID(path, args, w, ew, "destroy", "destroyed",
		func(ctx context.Context, p *sandbox.DockerProvider, id string) error {
			return p.Destroy(ctx, id)
		})
}

// sandboxOneID runs one stop/destroy-style action against a task id or
// container name. Unknown ids are a logged no-op with exit 0.
func sandboxOneID(path string, args []string, w, ew io.Writer, verb, done string,
	action func(ctx context.Context, p *sandbox.DockerProvider, id string) error) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox %s <task-or-container>\n", verb)
		return 2
	}
	_, store, provider := sandboxSetup(path, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	container, _ := resolveTarget(store, args[0])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := action(ctx, provider, container); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: sandbox %s %s\n", container, done)
	return 0
}

// truncate keeps event payloads bounded while marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
