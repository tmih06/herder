package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/textutil"
)

// execOutputCap bounds sandbox.exec event payloads: full output goes to
// the terminal, the durable event keeps the head for later validation use.
const execOutputCap = 4096

// containerNamePattern restricts interactive shell targets to Docker's
// own name charset (leading alnum, no spaces or flags), so raw container
// names cannot smuggle options into the docker exec argv.
var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

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
  destroy <id|glob>...        remove containers (unknown: logged no-op)

<id> accepts a task id or a container name; a glob like 'task_*' sweeps
every matching managed container.
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
	return cfg, store, newProvider(ew, store)
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

// sandboxProvision creates or reuses the task's container and walks a
// QUEUED task to PROVISIONING, recording the durable sandbox.provisioned
// event. Dirty work fails with a preservation note.
// The dispatcher owns the repo/provider checks and the state walk; the
// CLI keeps only the usage gate and the task lookup.
func sandboxProvision(path string, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox provision <task-id>\n")
		return 2
	}
	cfg, store, _ := sandboxSetup(path, ew)
	if cfg == nil {
		return 1
	}
	defer store.Close()
	task, err := store.GetTask(args[0])
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q: %v\n", args[0], err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := newDispatcher(store, w, ew).Provision(ctx, cfg, &task); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	return 0
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
			"output": textutil.Truncate(res.Stdout+res.Stderr, execOutputCap),
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
		fmt.Fprintf(w, "task:      %s (%s, branch %s)\n", task.ID, task.Status, dispatch.BranchForTask(*task))
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
	container, _ := resolveTarget(store, args[0])
	if !containerNamePattern.MatchString(container) {
		fmt.Fprintf(ew, "herder: sandbox name %q is not a valid container name\n", container)
		return 2
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		fmt.Fprintf(ew, "herder: docker not in PATH (install Docker to enter sandboxes)\n")
		return 1
	}
	//nolint:gosec // argv-form exec (no shell) with a LookPath binary and a
	// charset-validated name: nothing for a hostile name to inject through.
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

// sandboxDestroy removes containers; unknown ids log a no-op via the
// provider and still exit 0. Accepts several targets, and a target
// containing glob metacharacters (* ? [) matches against the managed
// container list — `herder sandbox destroy 'task_*'` sweeps a fleet.
// One failing target does not stop the rest; the exit code reports the
// first failure after all attempts.
func sandboxDestroy(path string, args []string, w, ew io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintf(ew, "herder: usage: herder sandbox destroy <task-or-container|glob>...\n")
		return 2
	}
	_, store, provider := sandboxSetup(path, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	targets, err := expandSandboxTargets(ctx, provider.List, store, args)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	code := 0
	for _, container := range targets {
		if err := provider.Destroy(ctx, container); err != nil {
			fmt.Fprintf(ew, "herder: %v\n", err)
			code = 1
			continue
		}
		fmt.Fprintf(w, "herder: sandbox %s destroyed\n", container)
	}
	return code
}

// expandSandboxTargets resolves each arg to container names: a glob
// pattern expands over the managed sandbox list (matched against both
// the container name and its herder-<task> suffix), anything else maps
// through resolveTarget. A pattern matching nothing is an error so a
// typo never silently destroys zero containers while looking like a
// sweep. Duplicates collapse so `task_x herder-task_x` destroys once.
// listManaged is the provider's List seam, injectable for tests.
func expandSandboxTargets(ctx context.Context, listManaged func(context.Context) ([]sandbox.Sandbox, error),
	store *storage.Store, args []string,
) ([]string, error) {
	var managed []sandbox.Sandbox
	needList := false
	for _, a := range args {
		if strings.ContainsAny(a, "*?[") {
			needList = true
		}
	}
	if needList {
		var err error
		managed, err = listManaged(ctx)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range args {
		if strings.ContainsAny(a, "*?[") {
			matched := 0
			for _, sb := range managed {
				short := strings.TrimPrefix(sb.ID, "herder-")
				ok, _ := gomatch(a, sb.ID)
				okShort, _ := gomatch(a, short)
				if ok || okShort {
					matched++
					if !seen[sb.ID] {
						seen[sb.ID] = true
						out = append(out, sb.ID)
					}
				}
			}
			if matched == 0 {
				return nil, fmt.Errorf("sandbox: pattern %q matched no managed containers", a)
			}
			continue
		}
		container, _ := resolveTarget(store, a)
		if !seen[container] {
			seen[container] = true
			out = append(out, container)
		}
	}
	return out, nil
}

// gomatch wraps path.Match so the pattern type stays local.
func gomatch(pattern, name string) (bool, error) {
	return path.Match(pattern, name)
}

// sandboxOneID runs one stop/destroy-style action against a task id or
// container name. Unknown ids are a logged no-op with exit 0.
func sandboxOneID(path string, args []string, w, ew io.Writer, verb, done string,
	action func(ctx context.Context, p *sandbox.DockerProvider, id string) error,
) int {
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
