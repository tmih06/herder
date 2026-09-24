// Package dispatch owns the shared worker pipeline behind `herder task
// start|retry|handoff`, `herder sandbox provision`, and the scheduler
// (issue #7; SPEC sections 15, 17-18, 50): one implementation, one
// message set, one event stream, so a task started by hand and a task
// started by the loop are indistinguishable.
//
// Why: duplicating the launch flow across the CLI and the scheduler
// would let wording, events, and failure handling drift; a single
// Dispatcher keeps the contract in one place.
// Approach: the Dispatcher bundles the store, the Herdr launcher, the
// Docker provider, and the delivery engine behind the same Runner seams
// the leaf packages use, so tests script subprocesses and the scheduler
// injects its owner identity. QUEUED, PROVISIONING, and RETRYING tasks
// take a store lease — heartbeated for the life of the call — before
// any subprocess so two dispatchers never start the same task.
// Inputs: config, the task row, an optional profile override, and the
// prior agent's name on handoffs.
// Flow: lease -> resolve profile -> sandbox inspect -> build prompt ->
// reuse-or-start session -> bind -> RUNNING -> agent.started event ->
// running label.
// Returns: errors carrying the operator-facing message (the CLI prints
// them once); non-fatal notes go to Warnf, success lines to Logf.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/deliver"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// HeartbeatDivisor spaces lease heartbeats inside the TTL: three beats
// per lease keeps a live dispatch renewed with slack for a slow store.
// Exported for the scheduler's own dispatch heartbeat.
const HeartbeatDivisor = 3

// dispatchTimeout bounds one Provision or Launch call: a hung provider
// or herdr call must not heartbeat forever and burn a worker slot.
const dispatchTimeout = 10 * time.Minute

// ErrPermanent marks terminal misconfiguration — the task's repository
// is absent from config or names an unsupported provider — so callers
// (the scheduler's dispatchOne) fail the task instead of requeueing a
// dispatch that can never succeed. Wrap with fmt.Errorf %w; test with
// errors.Is.
var ErrPermanent = errors.New("dispatch: permanent misconfiguration")

// Dispatcher runs the shared worker pipeline: provision, launch, stop,
// and issue-label bookkeeping for one task at a time.
// Store is required; Launcher, Provider, and Engine lazily default to
// zero-value instances (the same pattern as Launcher.runner), Owner and
// LeaseTTL bound the QUEUED-task dispatch lease, ActorType/ActorID stamp
// every store write, and Logf/Warnf carry operator-facing output.
type Dispatcher struct {
	// Store is the durable task repository; required.
	Store *storage.Store
	// Launcher drives Herdr sessions; nil defaults to a zero-value
	// launcher on the real CLI.
	Launcher *agent.Launcher
	// Provider drives the task's Docker sandbox; nil defaults to a
	// zero-value provider whose notes log through Warnf.
	Provider *sandbox.DockerProvider
	// Engine performs controller-side delivery ops (issue labels); nil
	// defaults to a zero-value engine.
	Engine *deliver.Engine
	// Owner names this dispatcher in the task lease; empty means
	// "cli-<pid>-<nonce>" — per-process with a random suffix so a
	// recycled PID cannot adopt a dead process's lease through the
	// same-owner fast path.
	Owner string
	// LeaseTTL bounds one dispatch attempt; zero means
	// config.DefaultLeaseTTL.
	LeaseTTL time.Duration
	// ActorType and ActorID stamp transitions, agent-state records, and
	// events; empty means "controller"/"cli". The scheduler sets ActorID
	// to its owner name.
	ActorType string
	ActorID   string
	// Logf carries success/info lines; nil discards.
	Logf func(format string, args ...any)
	// Warnf carries non-fatal warnings; nil discards.
	Warnf func(format string, args ...any)
}

// launcher resolves the injectable Launcher default.
func (d *Dispatcher) launcher() *agent.Launcher {
	if d.Launcher != nil {
		return d.Launcher
	}
	return &agent.Launcher{}
}

// provider resolves the injectable Provider default, wiring its no-op
// notes through Warnf like the CLI's provider did. StateDir roots the
// SSH assets and machine wiring beside the state database so a
// defaulted provider provisions the same worker shape as the CLI's.
func (d *Dispatcher) provider() *sandbox.DockerProvider {
	if d.Provider != nil {
		return d.Provider
	}
	p := &sandbox.DockerProvider{Log: d.warnf}
	if d.Store != nil {
		p.StateDir = filepath.Dir(d.Store.Path())
	}
	return p
}

// engine resolves the injectable Engine default.
func (d *Dispatcher) engine() *deliver.Engine {
	if d.Engine != nil {
		return d.Engine
	}
	return &deliver.Engine{}
}

// SandboxProvider exposes the nil-defaulted provider seam for the
// scheduler's reconcile path.
func (d *Dispatcher) SandboxProvider() *sandbox.DockerProvider {
	return d.provider()
}

// SessionLauncher exposes the nil-defaulted launcher seam for the
// scheduler's reconcile path.
func (d *Dispatcher) SessionLauncher() *agent.Launcher {
	return d.launcher()
}

// logf reports a success/info line, discarding when unset.
func (d *Dispatcher) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// warnf reports a non-fatal warning, discarding when unset.
func (d *Dispatcher) warnf(format string, args ...any) {
	if d.Warnf != nil {
		d.Warnf(format, args...)
	}
}

// owner names this dispatcher in the task lease. The CLI default is
// per-process with a random nonce: a recycled PID must not pass the
// same-owner check on a dead process's lease — that fast path skips
// acquisition and heartbeat, so the adopted lease would lapse
// mid-dispatch into a duplicate worker.
func (d *Dispatcher) owner() string {
	if d.Owner != "" {
		return d.Owner
	}
	return cliOwner()
}

// cliOwner is the process-lifetime default lease owner: "cli-<pid>-"
// plus 4 hex chars from crypto/rand so a recycled PID cannot
// impersonate a dead CLI. Falls back to nanotime on rand failure.
var cliOwner = sync.OnceValue(func() string {
	var nonce [2]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Sprintf("cli-%d-%d", os.Getpid(), time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("cli-%d-%s", os.Getpid(), hex.EncodeToString(nonce[:]))
})

// leaseTTL bounds one dispatch attempt with its default.
func (d *Dispatcher) leaseTTL() time.Duration {
	if d.LeaseTTL > 0 {
		return d.LeaseTTL
	}
	return config.DefaultLeaseTTL
}

// actorType stamps store writes with the configured attribution.
func (d *Dispatcher) actorType() string {
	if d.ActorType != "" {
		return d.ActorType
	}
	return "controller"
}

// actorID stamps store writes with the configured attribution.
func (d *Dispatcher) actorID() string {
	if d.ActorID != "" {
		return d.ActorID
	}
	return "cli"
}

// SpecForTask builds the provision spec from the task plus its repo and
// agent config: image from the repo sandbox block, CPU/memory limits from
// the agent profile, workspace rooted beside the state database so it
// survives restarts without a config change.
func SpecForTask(cfg *config.Config, task tasks.Task) sandbox.Spec {
	repo := cfg.Repositories[task.Repository]
	agent := cfg.Agents[task.AgentProfile]
	return sandbox.Spec{
		TaskID: task.ID, Repository: task.Repository,
		RemoteURL: repo.Remote(task.Repository),
		Branch:    BranchForTask(task), Image: repo.Sandbox.Image,
		CPUs: agent.Resources["cpu"], Memory: agent.Resources["memory"],
		WorkspaceRoot: SandboxRoot(cfg.Database.Path),
	}
}

// BranchForTask returns the deterministic worker branch: the claimed
// branch when set, else herder/<issue> parsed from the source ref, else
// herder/<task-id> so provisioning never blocks on an empty branch.
func BranchForTask(task tasks.Task) string {
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

// SandboxRoot resolves the workspace root beside the state database:
// <dbdir>/sandboxes, with ~/ expanded like the store does.
func SandboxRoot(dbPath string) string {
	if rest, ok := strings.CutPrefix(dbPath, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			dbPath = filepath.Join(home, rest)
		}
	}
	return filepath.Join(filepath.Dir(dbPath), "sandboxes")
}

// Provision creates or reuses the task's container and walks a QUEUED
// task to PROVISIONING, recording the durable sandbox.provisioned
// event. RUNNING is Launch's job once a session is bound: stamping it
// here would mark the task RUNNING before any agent exists, and
// reconcile would read the session-less worker as dead. Dirty work
// fails with a preservation note.
// A QUEUED, PROVISIONING, or RETRYING task takes the dispatch lease
// first — the same mutual exclusion Launch applies — so a manual
// provision never races the scheduler's dispatch of the same task
// (SPEC section 50); the lease heartbeats for the life of the call so
// a slow provision never lapses into a duplicate dispatch. All work
// runs on the lease-held context bounded by dispatchTimeout: a lost
// heartbeat cancels it, aborting the provider call instead of
// finishing a dispatch the store requeued.
// Inputs: bounded ctx, config, and the task row. Returns the first
// failure — terminal misconfiguration wraps ErrPermanent so the
// scheduler fails instead of requeueing; the success line goes to Logf.
func (d *Dispatcher) Provision(ctx context.Context, cfg *config.Config, task *tasks.Task) error {
	if repo, ok := cfg.Repositories[task.Repository]; !ok {
		return fmt.Errorf("task repository %q not in config: %w", task.Repository, ErrPermanent)
	} else if repo.Sandbox.Provider != "docker" {
		return fmt.Errorf("provider %q unsupported here (want docker): %w", repo.Sandbox.Provider, ErrPermanent)
	}
	hctx, release, err := d.HoldLease(ctx, task)
	if err != nil {
		return err
	}
	if release != nil {
		defer release()
	}
	spec := SpecForTask(cfg, *task)
	ctx, cancel := context.WithTimeout(hctx, dispatchTimeout)
	defer cancel()
	sb, err := d.provider().Provision(ctx, spec)
	if err != nil {
		return err
	}
	d.logf("herder: sandbox %s running (branch %s)", sb.ID, sb.Branch)
	if err := d.advanceToProvisioning(ctx, task); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"sandbox": sb.ID, "branch": sb.Branch,
		"workspace": sb.Workspace, "image": sb.Image,
		"base_sha": sb.BaseSHA,
	})
	if _, err := d.Store.AppendEvent(task.ID, "sandbox.provisioned",
		d.actorType(), d.actorID(), string(payload)); err != nil {
		return fmt.Errorf("record provision event: %w", err)
	}
	return nil
}

// advanceToRunning walks QUEUED -> PROVISIONING -> RUNNING so the durable
// state reflects the live container. relaunch additionally admits
// RETRYING and WAITING_FOR_HUMAN — the start path legitimately re-enters
// RUNNING from them, while a bare re-provision must not silently un-block
// a task waiting on a human. Any other state is left untouched:
// re-provisioning an active task is normal. Returns the first rejected
// transition so callers can stop instead of reporting a started agent
// for a task the store no longer considers launchable.
func (d *Dispatcher) advanceToRunning(ctx context.Context, task *tasks.Task, relaunch bool) error {
	var path []tasks.State
	switch task.Status {
	case tasks.Queued:
		path = []tasks.State{tasks.Provisioning, tasks.Running}
	case tasks.Provisioning:
		path = []tasks.State{tasks.Running}
	case tasks.Retrying, tasks.WaitingForHuman:
		if !relaunch {
			return nil
		}
		path = []tasks.State{tasks.Running}
	default:
		return nil
	}
	return d.walkPath(ctx, task, path)
}

// walkPath applies one transition chain, stamping each step and
// advancing task.Status so the in-memory row tracks the store. ctx is
// re-checked before every step: a lease-lost dispatch must not mark a
// requeued task RUNNING. Returns the first rejected transition wrapped
// with the from/to states.
func (d *Dispatcher) walkPath(ctx context.Context, task *tasks.Task, path []tasks.State) error {
	for _, next := range path {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("herder: advance %s -> %s: %w", task.Status, next, err)
		}
		if _, err := d.Store.Transition(task.ID, next, d.actorType(), d.actorID()); err != nil {
			return fmt.Errorf("herder: advance %s -> %s: %w", task.Status, next, err)
		}
		task.Status = next
	}
	return nil
}

// advanceToProvisioning walks QUEUED -> PROVISIONING so the durable
// state reflects that a dispatch attempt is underway. Every other
// state is left untouched: re-provisioning an active task is normal.
// Returns the rejected transition so callers can stop instead of
// reporting a provisioned sandbox for a task the store no longer
// considers launchable.
func (d *Dispatcher) advanceToProvisioning(ctx context.Context, task *tasks.Task) error {
	if task.Status != tasks.Queued {
		return nil
	}
	return d.walkPath(ctx, task, []tasks.State{tasks.Provisioning})
}

// StopWorker ends the live session and thaws a paused container so a
// stop, retry, or handoff never leaves a frozen agent behind. The thaw
// happens before the pane close: a frozen agent survives `pane close`
// and would wake on unpause as an orphan running the same task.
// Inputs: bounded ctx and the task. Returns the first failure; a dead
// session is already the goal.
func (d *Dispatcher) StopWorker(ctx context.Context, task *tasks.Task) error {
	if task.Status == tasks.Paused {
		if err := d.provider().EnsureRunning(ctx, sandbox.ContainerName(task.ID)); err != nil {
			return fmt.Errorf("thaw sandbox: %w", err)
		}
	}
	if task.AgentSessionID != "" {
		if err := d.launcher().Stop(ctx, task.ID, task.AgentSessionID); err != nil {
			return err
		}
	}
	return nil
}

// leaseError renders a held-lease refusal naming the task and, when the
// store can still read it, the owner currently holding the lease.
func (d *Dispatcher) leaseError(taskID string, err error) error {
	owner := "another dispatcher"
	if lease, getErr := d.Store.GetLease(taskID); getErr == nil && lease.Owner != "" {
		owner = lease.Owner
	}
	return fmt.Errorf("task %s: dispatch already in progress (lease held by %s): %w",
		taskID, owner, err)
}

// HoldLease takes the task's dispatch lease for a QUEUED,
// PROVISIONING, or RETRYING task unless the caller already holds it: a
// live same-owner lease means an outer dispatch (the scheduler's
// dispatchOne, or an intervene holding the lease across a retry's
// requeue-and-launch) owns the lifecycle, so this call neither renews
// nor releases it. A lease this call acquires gets a heartbeat
// goroutine ticking every TTL/HeartbeatDivisor on hctx — a provision
// longer than the TTL must not let the lease lapse into a duplicate
// dispatch. Only ErrLeaseLost cancels hctx: the lease is gone, so a
// stale CLI must stop instead of finishing a dispatch the store
// requeued. Transient store errors warn and keep beating, matching
// the scheduler's heartbeat — one missed beat never kills a healthy
// dispatch.
// Returns the lease-held context and the release func for the caller
// to defer — ctx unchanged and nil release when the task is not
// QUEUED/PROVISIONING/RETRYING or the lease was already ours — or a
// leaseError when a foreign owner holds it. Release is idempotent: it
// stops the beat, cancels hctx, then drops the lease.
func (d *Dispatcher) HoldLease(ctx context.Context, task *tasks.Task) (context.Context, func(), error) {
	if task.Status != tasks.Queued && task.Status != tasks.Provisioning && task.Status != tasks.Retrying {
		return ctx, nil, nil
	}
	owner := d.owner()
	if lease, err := d.Store.GetLease(task.ID); err == nil &&
		lease.Owner == owner && lease.ExpiresAt.After(time.Now().UTC()) {
		return ctx, nil, nil
	}
	if err := d.Store.AcquireLease(task.ID, owner, d.leaseTTL()); err != nil {
		if errors.Is(err, storage.ErrLeaseHeld) {
			return nil, nil, d.leaseError(task.ID, err)
		}
		return nil, nil, err
	}
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	interval := d.leaseTTL() / HeartbeatDivisor
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-hctx.Done():
				return
			case <-ticker.C:
				err := d.Store.HeartbeatLease(task.ID, owner, d.leaseTTL())
				switch {
				case err == nil:
				case errors.Is(err, storage.ErrLeaseLost):
					d.warnf("herder: dispatch %s: lease lost", task.ID)
					cancel()
					return
				default:
					d.warnf("herder: dispatch %s: lease heartbeat: %v", task.ID, err)
				}
			}
		}
	}()
	var once sync.Once
	return hctx, func() {
		once.Do(func() {
			close(done)
			cancel()
			_ = d.Store.ReleaseLease(task.ID, owner)
		})
	}, nil
}
