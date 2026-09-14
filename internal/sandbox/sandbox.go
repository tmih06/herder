// Package sandbox owns the Herder worker isolation boundary (issue #3;
// SPEC sections 23, 24, 27): one least-privilege Docker container per
// claimed task, on an independent checkout of a deterministic branch.
//
// Why: a coding worker must run where a developer can inspect and enter
// it, while controller secrets and host control surfaces never enter the
// worker and uncertain dirty state is preserved rather than silently
// destroyed.
// Approach: the Provider interface keeps orchestration independent of the
// runtime; DockerProvider shells to the Docker and git CLIs (no new
// dependencies, same probe style as internal/health) behind the Runner
// seam so tests script subprocesses instead of needing a daemon.
// Idempotence: container names derive from the task id, so re-provisioning
// converges; a dirty workspace fails with DirtyError instead of discarding
// work, and stop/destroy of an unknown sandbox is a logged no-op.
// Inputs: Spec (task identity, repository, branch, image, limits,
// workspace root). Flow: ensure workspace -> dirty guard -> checkout
// branch -> create-or-start container.
// Returns: Sandbox handles plus exec Results carrying output and exit
// status for later validation use.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DefaultImage is the worker image when the config names none: a Go
// toolchain image so the example validation (`go test ./...`) runs.
const DefaultImage = "golang:1.22-bookworm"

// DefaultPidsLimit caps fork bombs in the worker.
const DefaultPidsLimit = 256

// DefaultCPUs and DefaultMemory apply when the agent profile names no
// resources.
const (
	DefaultCPUs   = "2"
	DefaultMemory = "4g"
)

// ErrNotFound reports an unknown sandbox id.
var ErrNotFound = errors.New("sandbox: not found")

// DirtyError reports a re-provision that refused to touch uncommitted
// work. The workspace and container are left exactly as found.
type DirtyError struct {
	TaskID string
	Branch string
	Files  []string
}

// Error renders which task kept which dirty files.
func (e *DirtyError) Error() string {
	return fmt.Sprintf("sandbox: task %s has uncommitted work on %s (%s); preserved, not re-provisioned",
		e.TaskID, e.Branch, strings.Join(e.Files, ", "))
}

// Spec describes one worker sandbox.
type Spec struct {
	// TaskID is the owning task (e.g. task_abc123); it derives the
	// container name and workspace directory deterministically.
	TaskID string
	// Repository is owner/name; the workspace clones
	// https://github.com/<repository>.git when reachable.
	Repository string
	// Branch is the deterministic worker branch (herder/<issue>-<slug>).
	Branch string
	// Image is the worker image; empty means DefaultImage.
	Image string
	// CPUs and Memory translate to --cpus/--memory; empty means defaults.
	CPUs   string
	Memory string
	// PidsLimit translates to --pids-limit; <=0 means DefaultPidsLimit.
	PidsLimit int
	// WorkspaceRoot holds one directory per task id; empty means a
	// sandboxes/ sibling of the state database (resolved by the caller).
	WorkspaceRoot string
}

// Sandbox is a live worker handle.
type Sandbox struct {
	// ID is the container name (herder-<task>).
	ID string
	// TaskID is the owning task.
	TaskID string
	// ContainerID is the engine id when known.
	ContainerID string
	// Image is the worker image.
	Image string
	// Status is the engine status (running, exited, ...).
	Status string
	// Branch is the checked-out worker branch.
	Branch string
	// Workspace is the host checkout mounted at /workspace.
	Workspace string
}

// Result is one finished exec: output plus exit status for later
// validation use.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Provider isolates one worker runtime behind task orchestration (SPEC
// section 23: alternative runtimes without changing orchestration).
// Provision fuses create+start into the one converging call issue #3
// needs; Snapshot/Attach land with the agent-launch slice that needs them.
type Provider interface {
	// Name reports the provider key from config (docker).
	Name() string
	// Provision ensures the workspace checkout and a running container,
	// preserving dirty state via DirtyError.
	Provision(ctx context.Context, spec Spec) (*Sandbox, error)
	// Exec runs a command inside the sandbox, reporting output and exit.
	Exec(ctx context.Context, id string, cmd []string) (*Result, error)
	// Inspect reports one sandbox with its current status (running or
	// stopped) or ErrNotFound; callers gate on Sandbox.Status.
	Inspect(ctx context.Context, id string) (*Sandbox, error)
	// List reports every Herder-managed sandbox.
	List(ctx context.Context) ([]Sandbox, error)
	// Stop halts the container; unknown ids are a logged no-op.
	Stop(ctx context.Context, id string) error
	// Pause freezes the container's processes without killing them;
	// unknown ids are a logged no-op.
	Pause(ctx context.Context, id string) error
	// Unpause thaws a paused container; unknown ids are a logged no-op.
	Unpause(ctx context.Context, id string) error
	// EnsureRunning converges a sandbox toward running (unpause or start)
	// without touching the workspace; ErrNotFound when it is gone.
	EnsureRunning(ctx context.Context, id string) error
	// Destroy removes the container; unknown ids are a logged no-op.
	Destroy(ctx context.Context, id string) error
}

var nameSafe = regexp.MustCompile(`[^a-z0-9_-]`)

// ContainerName derives the deterministic container name for a task.
// Purpose: re-provisioning the same task converges on one container.
// Inputs: task id. Returns herder-<sanitized task id>.
func ContainerName(taskID string) string {
	s := strings.ToLower(strings.TrimSpace(taskID))
	s = nameSafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "task"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return "herder-" + s
}

// WorkspacePath returns the host checkout for a task under root.
// Purpose: one independent directory per task, stable across restarts.
func WorkspacePath(root, taskID string) string {
	return strings.TrimRight(root, "/") + "/" + strings.ToLower(strings.TrimSpace(taskID))
}

// imageOf applies the default image.
func imageOf(spec Spec) string {
	if strings.TrimSpace(spec.Image) != "" {
		return strings.TrimSpace(spec.Image)
	}
	return DefaultImage
}

// cpusOf applies the default CPU limit.
func cpusOf(spec Spec) string {
	if strings.TrimSpace(spec.CPUs) != "" {
		return strings.TrimSpace(spec.CPUs)
	}
	return DefaultCPUs
}

// memoryOf normalizes the memory limit to Docker units (4Gi -> 4g).
func memoryOf(spec Spec) string {
	return normalizeMemory(orDefault(spec.Memory, DefaultMemory))
}

// pidsOf applies the default process limit.
func pidsOf(spec Spec) int {
	if spec.PidsLimit > 0 {
		return spec.PidsLimit
	}
	return DefaultPidsLimit
}

var memPattern = regexp.MustCompile(`(?i)^([0-9]+)\s*([kmg])(i)?(b)?$`)

// normalizeMemory maps Go-style sizes (4Gi, 512Mi) to Docker --memory
// units (4g, 512m); anything unrecognized passes through untouched.
func normalizeMemory(s string) string {
	m := memPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return strings.TrimSpace(s)
	}
	return m[1] + strings.ToLower(m[2])
}

// validateSpec rejects provisioning with no task, repo, or branch.
func validateSpec(spec Spec) error {
	if strings.TrimSpace(spec.TaskID) == "" {
		return errors.New("sandbox: provision needs a task id")
	}
	if strings.TrimSpace(spec.Repository) == "" {
		return errors.New("sandbox: provision needs a repository (owner/name)")
	}
	if strings.TrimSpace(spec.Branch) == "" {
		return errors.New("sandbox: provision needs a branch")
	}
	if strings.TrimSpace(spec.WorkspaceRoot) == "" {
		return errors.New("sandbox: provision needs a workspace root")
	}
	return nil
}

// orDefault returns s trimmed, or fallback when blank.
func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return fallback
}
