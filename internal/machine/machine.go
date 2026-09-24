// Package machine owns the controller's saved-SSH-machine registry
// (issue #19): each worker container is registered as a herdr machine so
// `herdr --machine <label>` forwards workspace/pane/agent commands to the
// container-local herdr server and the host sidebar shows every worker.
//
// Why: `herdr machine add` is not idempotent — it appends a profile even
// when the label already exists — so re-provisioning must converge by
// listing first, and teardown must remove by profile id (labels are not
// accepted by `machine remove`).
// Approach: the same Runner seam as sandbox/agent so tests script the
// herdr CLI; profile ids come from `machine list --json`, never derived.
// Inputs: SSH target (the container's Host alias) and the machine label
// (the task id). Flow: List -> Ensure (add when absent) -> Remove.
// Returns: Machine records carrying the durable profile id.
package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/textutil"
)

// runTimeout bounds one herdr machine invocation: catalog reads are
// local, but `machine add` opens an SSH connection and prepares the
// remote server, so it needs real time.
const runTimeout = 2 * time.Minute

// RunResult is one finished herdr invocation: split streams plus exit.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner runs one subprocess; the injectable seam so tests script herdr
// results instead of needing a live server.
type Runner func(ctx context.Context, name string, args ...string) (RunResult, error)

// DefaultRunner resolves the binary in PATH and captures both streams,
// translating ExitError into ExitCode so a failed herdr call is data.
func DefaultRunner(ctx context.Context, name string, args ...string) (RunResult, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return RunResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return RunResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exit.ExitCode()}, nil
		}
		return RunResult{}, err
	}
	return RunResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// Machine is one saved SSH machine profile: the durable id herdr minted,
// its sidebar label, and the SSH target it connects to.
type Machine struct {
	ID      string
	Label   string
	Target  string
	Enabled bool
}

// Registry talks to the herdr machine catalog through the CLI.
type Registry struct {
	// Runner executes subprocesses; nil means DefaultRunner.
	Runner Runner
}

// runner resolves the injectable Runner default.
func (r *Registry) runner() Runner {
	if r.Runner != nil {
		return r.Runner
	}
	return DefaultRunner
}

// List returns every saved machine profile.
func (r *Registry) List(ctx context.Context) ([]Machine, error) {
	out, err := r.runner()(ctx, "herdr", "machine", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("machine: list: %w", err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("machine: list: %s", textutil.FirstLine(out.Stderr))
	}
	var raw []struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Target  string `json:"target"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(out.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("machine: list: unreadable output")
	}
	machines := make([]Machine, 0, len(raw))
	for _, m := range raw {
		machines = append(machines, Machine{ID: m.ID, Label: m.Label, Target: m.Target, Enabled: m.Enabled})
	}
	return machines, nil
}

// Find returns the profile whose label or target matches, or nil. Label
// wins: it is the stable task identity; target matches catch profiles
// left by an earlier naming scheme.
func (r *Registry) Find(ctx context.Context, label, target string) (*Machine, error) {
	machines, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range machines {
		if machines[i].Label == label {
			return &machines[i], nil
		}
	}
	for i := range machines {
		if machines[i].Target == target {
			return &machines[i], nil
		}
	}
	return nil, nil
}

// Ensure converges one worker's machine profile: an existing profile for
// the label is returned unchanged (idempotent re-provision); otherwise
// `machine add` prepares the remote server and saves the profile, and a
// fresh list resolves the minted profile id. A failed add is a real
// error — the worker cannot be driven without it.
func (r *Registry) Ensure(ctx context.Context, label, target string) (*Machine, error) {
	if existing, err := r.Find(ctx, label, target); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	out, err := r.runner()(ctx, "herdr", "machine", "add", target, "--label", label)
	if err != nil {
		return nil, fmt.Errorf("machine: add %s: %w", label, err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("machine: add %s: %s", label, textutil.FirstLine(out.Stderr))
	}
	created, err := r.Find(ctx, label, target)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, fmt.Errorf("machine: add %s succeeded but no profile is saved", label)
	}
	return created, nil
}

// Remove deletes every profile matching the label or target, best-effort
// per profile: `machine remove` takes the profile id, not the label, and
// a destroyed worker must not accumulate dead sidebar machines. Returns
// the first removal error after attempting all matches; a missing
// profile is a no-op.
func (r *Registry) Remove(ctx context.Context, label, target string) error {
	machines, err := r.List(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, m := range machines {
		// An empty criterion never matches: a caller that knows only
		// the target must not sweep profiles with blank labels.
		if (label == "" || m.Label != label) && (target == "" || m.Target != target) {
			continue
		}
		out, err := r.runner()(ctx, "herdr", "machine", "remove", m.ID)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("machine: remove %s: %w", m.ID, err)
		} else if out.ExitCode != 0 && firstErr == nil {
			firstErr = fmt.Errorf("machine: remove %s: %s", m.ID, textutil.FirstLine(out.Stderr))
		}
	}
	return firstErr
}
