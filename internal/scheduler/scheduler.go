// Package scheduler is the daemon's dispatch loop (issue #7; SPEC
// sections 15, 48-50): FIFO-plus-priority dispatch under concurrency
// caps, expiring dispatch leases, restart reconciliation, and agent
// time/resource limit enforcement.
//
// Why: a queued task must start exactly once even across daemon crashes
// and competing dispatchers, while a worker that outlives its session,
// sandbox, or time budget must surface as a durable state change, not a
// silent orphan.
// Approach: one Tick per interval runs three passes — dead leases
// requeue, live workers reconcile against Herdr and the sandbox
// provider, then the queue dispatches under global, per-repository,
// and per-agent-kind caps. Every dispatch takes the task's store lease
// before any subprocess and heartbeats it through provisioning, so a
// dead scheduler's task requeues instead of stranding.
// Inputs: the store, the shared dispatcher, config, and the owner name
// the daemon stamps on leases (daemon-<pid>).
// Flow: Run -> Tick per configured interval -> expireLeases ->
// reconcile -> dispatch; dispatchOne runs per task in its own goroutine.
// Returns: nothing; failures log through Logf and the next tick retries.
package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// defaultMaxWorkers applies when the config carries no global cap; it
// mirrors the applyDefaults value so a hand-built config behaves like a
// loaded one.
const defaultMaxWorkers = 4

// defaultRetryDelay suppresses immediate re-dispatch after a failed
// attempt: long enough for a transient provision failure to clear,
// short enough that a real fix is not delayed.
const defaultRetryDelay = time.Minute

// workerStates hold a worker slot: every state between dispatch and the
// terminal/PR_OPEN states counts against the concurrency caps (SPEC
// section 15). QUEUED tasks hold no worker yet — they are the queue.
var workerStates = map[tasks.State]bool{
	tasks.Provisioning:    true,
	tasks.Running:         true,
	tasks.Validating:      true,
	tasks.Reviewing:       true,
	tasks.Delivering:      true,
	tasks.Blocked:         true,
	tasks.WaitingForHuman: true,
	tasks.Paused:          true,
}

// Scheduler runs the dispatch loop: one ticker, three passes per tick.
// Store and Cfg are required; Dispatcher carries the pipeline seams
// (nil fields inside it default like the CLI's). Owner names this
// scheduler in task leases, and Logf receives non-fatal errors.
type Scheduler struct {
	Store      *storage.Store
	Dispatcher *dispatch.Dispatcher
	Cfg        *config.Config
	// Owner names this scheduler in task leases and store attribution;
	// empty means Dispatcher.Owner, then "scheduler" (the daemon passes
	// "daemon-<pid>").
	Owner string
	// Logf receives non-fatal errors; nil discards them.
	Logf func(format string, args ...any)

	// mu guards inflight and backoff: a spawned dispatch occupies a
	// slot before its transition lands, and a failed attempt suppresses
	// retries until its stamp passes.
	mu       sync.Mutex
	inflight map[string]bool
	backoff  map[string]time.Time
	// wg tracks live dispatchOne goroutines so Run can drain them on
	// shutdown instead of abandoning a half-run launch.
	wg sync.WaitGroup
	// stampOnce stamps the dispatcher's lease identity exactly once:
	// dispatchOne goroutines read those fields through the dispatcher's
	// accessors, so a per-tick rewrite would race them.
	stampOnce sync.Once
}

// Run ticks immediately — a daemon restart reconciles right away — then
// once per configured interval until ctx is done. In-flight dispatches
// drain before Run returns so a graceful shutdown never abandons a
// half-run launch.
func (s *Scheduler) Run(ctx context.Context) {
	s.Tick(ctx)
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	defer s.wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs one scheduler pass in order: dead leases requeue, live
// workers reconcile against Herdr and the sandbox provider, then the
// queue dispatches under the concurrency caps. The dispatcher's lease
// identity is stamped once — before the first pass can spawn a
// dispatchOne — so the write never races a reader.
func (s *Scheduler) Tick(ctx context.Context) {
	if s.Dispatcher != nil {
		s.stampOnce.Do(s.stampDispatcher)
	}
	s.expireLeases(ctx)
	s.reconcile(ctx)
	s.dispatch(ctx)
}

// stampDispatcher writes the scheduler's lease identity onto the
// dispatcher once: Owner and ActorID name this scheduler, LeaseTTL
// bounds each dispatch attempt. Runs under stampOnce because
// dispatchOne goroutines read the same fields via d.owner(),
// d.leaseTTL(), and d.actorID().
func (s *Scheduler) stampDispatcher() {
	s.Dispatcher.Owner = s.owner()
	s.Dispatcher.LeaseTTL = s.leaseTTL()
	s.Dispatcher.ActorID = s.owner()
}

// expireLeases returns tasks whose dispatch lease lapsed to the queue:
// the owner is presumed dead, so a queued/provisioning/retrying task
// loses its possibly-orphaned pane and requeues with a lease.expired
// event. Tasks in any other state just lose the stale row — their
// worker already launched or finished, so no event, no transition.
func (s *Scheduler) expireLeases(ctx context.Context) {
	expired, err := s.Store.ExpireLeases(time.Now().UTC())
	if err != nil {
		s.logf("herder: scheduler: expire leases: %v", err)
		return
	}
	for taskID, owner := range expired {
		task, err := s.Store.GetTask(taskID)
		if err != nil {
			s.logf("herder: scheduler: expired lease: get %s: %v", taskID, err)
			continue
		}
		switch task.Status {
		case tasks.Queued, tasks.Provisioning, tasks.Retrying:
		default:
			continue
		}
		// The dead owner may have left a Herdr pane running the task;
		// stopping is best-effort because the pane is usually gone too.
		s.stopWorkerSession(ctx, &task)
		if task.Status != tasks.Queued {
			if _, err := s.Store.Transition(task.ID, tasks.Queued, "controller", s.owner()); err != nil {
				s.logf("herder: scheduler: expired lease: requeue %s: %v", task.ID, err)
			}
		}
		if _, err := s.Store.AppendEvent(task.ID, tasks.EventLeaseExpired, "controller", s.owner(),
			tasks.EventPayload(map[string]string{
				"owner": owner, "task_id": task.ID, "previous_status": string(task.Status),
			})); err != nil {
			s.logf("herder: scheduler: expired lease: event %s: %v", task.ID, err)
		}
	}
}

// reconcile folds runtime truth into durable state for every task
// holding a worker (SPEC section 48): a worker past its agent timeout
// stops and lands TIMED_OUT, a dead or OOM-killed sandbox disconnects
// or fails the task, and a bound session that vanished lands
// WAITING_FOR_HUMAN. The sandbox verdict runs before the session check
// so a kill that also took the session down still attributes the
// resource-limit reason instead of a generic disconnect. A task under
// a live dispatch lease is skipped entirely: the lease owner is
// responsible until the lease lapses.
// A PROVISIONING task with no live lease is an abandoned dispatch —
// the owner died between Provision and Launch, or a manual provision
// was left staged — so it requeues with a dispatch_failed event
// instead of holding a worker slot forever.
// Uncertain inspect answers only log — the next tick retries — because
// acting on a maybe-wrong reading could strand real work. No path ever
// destroys a sandbox or workspace: dirty work is preserved for a human.
// VALIDATING, REVIEWING, DELIVERING, PR_OPEN, WAITING_FOR_HUMAN, and
// PAUSED are deliberately not reconciled: they are parked or
// post-agent operator-gated stages with no live session to supervise,
// so a dead sandbox there surfaces as a command error, not a
// disconnect.
func (s *Scheduler) reconcile(ctx context.Context) {
	found, err := s.Store.ListTasks()
	if err != nil {
		s.logf("herder: scheduler: reconcile: list tasks: %v", err)
		return
	}
	for i := range found {
		task := &found[i]
		if task.Status == tasks.Provisioning {
			if s.leaseHeld(task.ID) {
				continue
			}
			if _, err := s.Store.Transition(task.ID, tasks.Queued, "controller", s.owner()); err != nil {
				s.logf("herder: scheduler: reconcile %s: requeue abandoned provision: %v", task.ID, err)
				continue
			}
			if _, err := s.Store.AppendEvent(task.ID, tasks.EventDispatchFailed,
				"controller", s.owner(), tasks.EventPayload(map[string]string{
					"reason": "provisioning abandoned: no dispatch lease", "stage": "provision",
				})); err != nil {
				s.logf("herder: scheduler: reconcile %s: event: %v", task.ID, err)
			}
			continue
		}
		if task.Status != tasks.Running && task.Status != tasks.Blocked {
			continue
		}
		if s.leaseHeld(task.ID) {
			continue
		}
		if s.enforceTimeout(ctx, task) {
			continue
		}
		if !s.checkSandbox(ctx, task) {
			continue
		}
		if task.AgentSessionID == "" {
			// The supervisor's agent.exited already cleared the binding;
			// a session-less worker on a healthy sandbox is a disconnected
			// one (SPEC 48).
			s.disconnect(task, "session gone", map[string]string{
				"session": "",
			})
		}
	}
}

// enforceTimeout stops a worker that exceeded its agent profile's
// timeout and lands the task TIMED_OUT. StartedAt stamps the latest
// RUNNING entry, so the budget measures this attempt's wall clock.
// Returns true when the task was handled — timed out or errored — and
// the caller should move on to the next task.
func (s *Scheduler) enforceTimeout(ctx context.Context, task *tasks.Task) bool {
	if task.StartedAt.IsZero() {
		return false
	}
	prof, ok := s.agentProfile(task.AgentProfile)
	if !ok {
		return false
	}
	timeout := prof.TimeoutDuration()
	if timeout <= 0 || time.Since(task.StartedAt) <= timeout {
		return false
	}
	if err := s.stopWorker(ctx, task); err != nil {
		s.logf("herder: scheduler: timeout %s: stop worker: %v", task.ID, err)
		return true
	}
	if _, err := s.Store.Transition(task.ID, tasks.TimedOut, "controller", s.owner()); err != nil {
		s.logf("herder: scheduler: timeout %s: transition: %v", task.ID, err)
		return true
	}
	if _, err := s.Store.AppendEvent(task.ID, tasks.EventTaskTimedOut, "controller", s.owner(),
		tasks.EventPayload(map[string]string{
			"timeout":       timeout.String(),
			"started_at":    task.StartedAt.UTC().Format(time.RFC3339Nano),
			"agent_profile": task.AgentProfile,
		})); err != nil {
		s.logf("herder: scheduler: timeout %s: event: %v", task.ID, err)
	}
	return true
}

// checkSandbox reconciles one worker against the sandbox provider: a
// missing container disconnects the worker, an OOM kill fails the task
// with the resource-limit reason, and any other non-running status
// waits for a human. The dispatcher's provider nil-defaults to the real
// Docker provider, so only a nil Dispatcher skips the check. An inspect
// error is uncertain — logged and skipped, never acted on — and
// nothing here ever destroys the sandbox or workspace (SPEC section
// 48). Returns true only when the sandbox is confirmed running, so the
// caller may still check the session binding; every other outcome
// already handled the task.
func (s *Scheduler) checkSandbox(ctx context.Context, task *tasks.Task) bool {
	if s.Dispatcher == nil {
		return true
	}
	container := sandbox.ContainerName(task.ID)
	sb, err := s.Dispatcher.SandboxProvider().Inspect(ctx, container)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			s.stopWorkerBestEffort(ctx, task)
			s.disconnect(task, "sandbox gone", map[string]string{
				"sandbox": container,
			})
			return false
		}
		s.logf("herder: scheduler: reconcile %s: inspect %s: %v", task.ID, container, err)
		return false
	}
	// OOMKilled is checked before the running status: the engine reports
	// the kill on the dead container, and a kill is a resource-limit
	// verdict regardless of the status word beside it.
	if sb.OOMKilled {
		s.stopWorkerBestEffort(ctx, task)
		if _, err := s.Store.Transition(task.ID, tasks.Failed, "controller", s.owner()); err != nil {
			s.logf("herder: scheduler: reconcile %s: fail OOM task: %v", task.ID, err)
			return false
		}
		s.appendDisconnectEvent(task, map[string]string{
			"reason": "resource limit: container OOM-killed", "sandbox": container,
		})
		return false
	}
	if sb.Status == "running" {
		return true
	}
	s.stopWorkerBestEffort(ctx, task)
	s.disconnect(task, "sandbox "+sb.Status, map[string]string{
		"sandbox": container,
	})
	return false
}

// disconnect lands a workerless task WAITING_FOR_HUMAN and records why
// as the event's reason: the session or sandbox is gone, so a human
// decides whether the preserved work retries or stops (SPEC section
// 48).
func (s *Scheduler) disconnect(task *tasks.Task, why string, payload map[string]string) {
	if _, err := s.Store.Transition(task.ID, tasks.WaitingForHuman, "controller", s.owner()); err != nil {
		s.logf("herder: scheduler: reconcile %s: %s: %v", task.ID, why, err)
		return
	}
	payload["reason"] = why
	s.appendDisconnectEvent(task, payload)
}

// appendDisconnectEvent records worker.disconnected; a failed append
// only logs because the state change it annotates already committed.
func (s *Scheduler) appendDisconnectEvent(task *tasks.Task, payload map[string]string) {
	if _, err := s.Store.AppendEvent(task.ID, tasks.EventWorkerDisconnected,
		"controller", s.owner(), tasks.EventPayload(payload)); err != nil {
		s.logf("herder: scheduler: reconcile %s: event: %v", task.ID, err)
	}
}

// dispatch starts queued tasks under the concurrency caps, oldest first
// within each priority band (FIFO-plus-priority, SPEC section 15). The
// global cap stops the pass outright — nothing further can fit — while
// a full repository or agent-kind bucket only skips that task. A task
// already dispatching (inflight) or inside its retry backoff is skipped
// without consuming a slot.
func (s *Scheduler) dispatch(ctx context.Context) {
	if s.Dispatcher == nil || s.Cfg == nil {
		return
	}
	found, err := s.Store.ListTasks()
	if err != nil {
		s.logf("herder: scheduler: dispatch: list tasks: %v", err)
		return
	}
	byID := make(map[string]tasks.Task, len(found))
	var queued []tasks.Task
	workers, repos, kinds := 0, map[string]int{}, map[string]int{}
	for _, task := range found {
		byID[task.ID] = task
		if task.Status == tasks.Queued {
			queued = append(queued, task)
			continue
		}
		if workerStates[task.Status] {
			workers++
			repos[task.Repository]++
			kinds[s.kindOf(task)]++
		}
	}
	s.mu.Lock()
	s.maps()
	// Inflight dispatches hold a slot before their transition lands, so
	// they count against every bucket their task belongs to — but only
	// while the stored status is still QUEUED: once Provision lands
	// PROVISIONING the worker-states pass above already counts it, and
	// counting again would burn two slots on one worker.
	for id := range s.inflight {
		if task, ok := byID[id]; ok && task.Status == tasks.Queued {
			workers++
			repos[task.Repository]++
			kinds[s.kindOf(task)]++
		}
	}
	// Backoff entries die with the queue: a task that left QUEUED
	// retries on its next queue entry, not on a stale stamp.
	for id := range s.backoff {
		if task, ok := byID[id]; !ok || task.Status != tasks.Queued {
			delete(s.backoff, id)
		}
	}
	s.mu.Unlock()
	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].Priority != queued[j].Priority {
			return queued[i].Priority > queued[j].Priority
		}
		return queued[i].CreatedAt.Before(queued[j].CreatedAt)
	})
	maxWorkers := s.maxWorkers()
	now := time.Now()
	for _, task := range queued {
		if workers >= maxWorkers {
			break
		}
		if !s.dispatchable(task.ID, now) {
			continue
		}
		if limit, ok := s.Cfg.Scheduler.PerRepository[task.Repository]; ok && repos[task.Repository] >= limit {
			continue
		}
		kind := s.kindOf(task)
		if limit, ok := s.Cfg.Scheduler.PerAgent[kind]; ok && kinds[kind] >= limit {
			continue
		}
		s.mu.Lock()
		s.maps()
		s.inflight[task.ID] = true
		s.mu.Unlock()
		workers++
		repos[task.Repository]++
		kinds[kind]++
		s.wg.Add(1)
		go s.dispatchOne(ctx, task)
	}
}

// dispatchable reports whether a queued task may start now: not already
// dispatching and not inside its post-failure backoff window.
func (s *Scheduler) dispatchable(taskID string, now time.Time) bool {
	s.mu.Lock()
	s.maps()
	defer s.mu.Unlock()
	if s.inflight[taskID] {
		return false
	}
	until, ok := s.backoff[taskID]
	return !ok || !now.Before(until)
}

// dispatchOne runs one task's dispatch under its store lease: acquire,
// heartbeat through provision and launch, release. The lease — not the
// queue scan — serializes competing dispatchers: ErrLeaseHeld means
// someone else owns the task and this attempt simply ends. A dirty
// workspace fails the task (the work is preserved, never re-provisioned
// over); any other provision failure requeues with a dispatch_failed
// event and a backoff stamp so the next tick does not hammer a broken
// task. Launch failures need no state work here: the launch path
// already failed the task itself.
func (s *Scheduler) dispatchOne(ctx context.Context, task tasks.Task) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, task.ID)
		s.mu.Unlock()
	}()
	owner := s.owner()
	if err := s.Store.AcquireLease(task.ID, owner, s.leaseTTL()); err != nil {
		if errors.Is(err, storage.ErrLeaseHeld) {
			s.logf("herder: scheduler: dispatch %s: lease held by another owner", task.ID)
		} else {
			s.logf("herder: scheduler: dispatch %s: acquire lease: %v", task.ID, err)
		}
		return
	}
	defer func() {
		if err := s.Store.ReleaseLease(task.ID, owner); err != nil {
			s.logf("herder: scheduler: dispatch %s: release lease: %v", task.ID, err)
		}
	}()
	// The queue scan raced real state: re-read before spending a
	// provision on a task that left QUEUED (cancelled, retried by hand).
	if cur, err := s.Store.GetTask(task.ID); err != nil {
		s.logf("herder: scheduler: dispatch %s: re-read: %v", task.ID, err)
		return
	} else if cur.Status != tasks.Queued {
		return
	}
	// The heartbeat cancels pctx on lease loss so a mid-flight provision
	// aborts promptly instead of racing the next owner's dispatch for
	// the full provision timeout.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost, stopHeartbeat := s.heartbeat(pctx, task.ID, owner, cancel)
	defer stopHeartbeat()
	if err := s.Dispatcher.Provision(pctx, s.Cfg, &task); err != nil {
		var dirty *sandbox.DirtyError
		switch {
		case errors.As(err, &dirty):
			s.failDispatch(task, err.Error(), true)
		case s.leaseLost(lost) || pctx.Err() != nil:
			// The lease lapsed mid-provision (or the daemon is
			// stopping): expireLeases already requeued the task, so
			// this attempt just ends — no event, no backoff.
		default:
			s.requeueDispatch(task, err.Error())
		}
		return
	}
	if s.leaseLost(lost) {
		return
	}
	if err := s.Dispatcher.Launch(pctx, s.Cfg, &task, "", ""); err != nil {
		// ErrLeaseHeld means a competing dispatcher owns the task;
		// every other failure already landed on the task via
		// failTaskStart, so both paths only log.
		if errors.Is(err, storage.ErrLeaseHeld) {
			s.logf("herder: scheduler: dispatch %s: launch: lease held by another owner", task.ID)
		} else {
			s.logf("herder: scheduler: dispatch %s: launch: %v", task.ID, err)
		}
		return
	}
	s.mu.Lock()
	delete(s.backoff, task.ID)
	s.mu.Unlock()
}

// heartbeat keeps the dispatch lease alive while Provision and Launch
// run: a slow provision must not let the lease lapse mid-dispatch, or
// expireLeases would requeue a task whose launch is still live. The
// goroutine ticks every TTL/3; ErrLeaseLost means the lease is gone —
// expired under a stalled beat or released — so it closes lost and
// cancels the dispatch context, aborting the in-flight call instead of
// racing the next owner for the rest of the provision timeout.
// Transient store errors only log: one missed beat never kills a
// healthy dispatch. Returns the loss signal and a stop func for the
// owning dispatchOne.
func (s *Scheduler) heartbeat(ctx context.Context, taskID, owner string, cancel context.CancelFunc) (<-chan struct{}, func()) {
	lost := make(chan struct{})
	done := make(chan struct{})
	interval := s.leaseTTL() / dispatch.HeartbeatDivisor
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
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := s.Store.HeartbeatLease(taskID, owner, s.leaseTTL())
				switch {
				case err == nil:
				case errors.Is(err, storage.ErrLeaseLost):
					s.logf("herder: scheduler: dispatch %s: lease lost", taskID)
					close(lost)
					cancel()
					return
				default:
					s.logf("herder: scheduler: dispatch %s: heartbeat: %v", taskID, err)
				}
			}
		}
	}()
	return lost, func() { close(done) }
}

// leaseLost reports whether the heartbeat declared the lease gone.
func (s *Scheduler) leaseLost(lost <-chan struct{}) bool {
	select {
	case <-lost:
		return true
	default:
		return false
	}
}

// leaseHeld reports whether a live dispatch lease covers the task: a
// mid-dispatch task sits PROVISIONING until Launch binds the session,
// so reconcile must leave it to the lease owner. ErrNotFound means no
// lease — proceed; any other error is uncertain, so it logs and counts
// as held (never act on uncertainty).
func (s *Scheduler) leaseHeld(taskID string) bool {
	lease, err := s.Store.GetLease(taskID)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			s.logf("herder: scheduler: reconcile %s: get lease: %v", taskID, err)
			return true
		}
		return false
	}
	return lease.ExpiresAt.After(time.Now().UTC())
}

// failDispatch lands a task FAILED after a provision refusal that must
// not retry: a dirty workspace means re-provisioning would destroy
// uncommitted work, so the task fails with the preservation recorded.
func (s *Scheduler) failDispatch(task tasks.Task, reason string, preserved bool) {
	if _, err := s.Store.Transition(task.ID, tasks.Failed, "controller", s.owner()); err != nil {
		s.logf("herder: scheduler: dispatch %s: fail task: %v", task.ID, err)
	}
	payload := map[string]any{"reason": reason, "stage": "provision"}
	if preserved {
		payload["workspace_preserved"] = true
	}
	if _, err := s.Store.AppendEvent(task.ID, tasks.EventDispatchFailed,
		"controller", s.owner(), tasks.EventPayload(payload)); err != nil {
		s.logf("herder: scheduler: dispatch %s: event: %v", task.ID, err)
	}
}

// requeueDispatch returns a task to QUEUED after a failed provision and
// stamps its backoff so the next tick does not retry immediately. The
// transition is skipped when the task already sits in QUEUED — the
// store rejects same-state jumps — and logged when it races elsewhere.
func (s *Scheduler) requeueDispatch(task tasks.Task, reason string) {
	if cur, err := s.Store.GetTask(task.ID); err != nil {
		s.logf("herder: scheduler: dispatch %s: re-read: %v", task.ID, err)
	} else if cur.Status != tasks.Queued {
		if _, err := s.Store.Transition(task.ID, tasks.Queued, "controller", s.owner()); err != nil {
			s.logf("herder: scheduler: dispatch %s: requeue: %v", task.ID, err)
		}
	}
	if _, err := s.Store.AppendEvent(task.ID, tasks.EventDispatchFailed,
		"controller", s.owner(), tasks.EventPayload(map[string]any{
			"reason": reason, "stage": "provision",
		})); err != nil {
		s.logf("herder: scheduler: dispatch %s: event: %v", task.ID, err)
	}
	s.mu.Lock()
	s.maps()
	s.backoff[task.ID] = time.Now().Add(defaultRetryDelay)
	s.mu.Unlock()
}

// stopWorker ends the task's live session (and thaws a paused sandbox)
// through the dispatcher; a nil dispatcher reports success so the
// reconcile state change still lands.
func (s *Scheduler) stopWorker(ctx context.Context, task *tasks.Task) error {
	if s.Dispatcher == nil {
		return nil
	}
	return s.Dispatcher.StopWorker(ctx, task)
}

// stopWorkerBestEffort stops the worker when possible, logging
// failures: the disconnect it precedes must not be gated by a dead
// pane.
func (s *Scheduler) stopWorkerBestEffort(ctx context.Context, task *tasks.Task) {
	if err := s.stopWorker(ctx, task); err != nil {
		s.logf("herder: scheduler: reconcile %s: stop worker: %v", task.ID, err)
	}
}

// stopWorkerSession closes the task's deterministic Herdr pane,
// best-effort: the pane is usually already gone with its dead owner.
func (s *Scheduler) stopWorkerSession(ctx context.Context, task *tasks.Task) {
	if s.Dispatcher == nil {
		return
	}
	if err := s.Dispatcher.SessionLauncher().Stop(ctx, agent.SessionName(task.ID)); err != nil {
		s.logf("herder: scheduler: stop session for %s: %v", task.ID, err)
	}
}

// agentProfile resolves a task's agent profile, nil-safe on Cfg so a
// half-built scheduler degrades to no timeouts instead of panicking.
func (s *Scheduler) agentProfile(name string) (config.AgentConfig, bool) {
	if s.Cfg == nil {
		return config.AgentConfig{}, false
	}
	prof, ok := s.Cfg.Agents[name]
	return prof, ok
}

// kindOf resolves the task's agent kind for the per-agent cap: the
// configured profile's kind, or a "profile:"-prefixed key when the
// profile is unresolvable so unknown profiles still group under one
// bucket instead of bypassing the cap.
func (s *Scheduler) kindOf(task tasks.Task) string {
	if prof, ok := s.agentProfile(task.AgentProfile); ok {
		return prof.Kind
	}
	return "profile:" + task.AgentProfile
}

// owner names this scheduler in leases and store attribution: the
// configured Owner, then the dispatcher's, then "scheduler".
func (s *Scheduler) owner() string {
	if s.Owner != "" {
		return s.Owner
	}
	if s.Dispatcher != nil && s.Dispatcher.Owner != "" {
		return s.Dispatcher.Owner
	}
	return "scheduler"
}

// interval spaces ticks with the configured default.
func (s *Scheduler) interval() time.Duration {
	if s.Cfg != nil {
		return s.Cfg.Scheduler.DispatchIntervalDuration()
	}
	return config.DefaultDispatchInterval
}

// leaseTTL bounds one dispatch lease with the configured default.
func (s *Scheduler) leaseTTL() time.Duration {
	if s.Cfg != nil {
		return s.Cfg.Scheduler.LeaseTTLDuration()
	}
	return config.DefaultLeaseTTL
}

// maxWorkers reads the global cap with the applyDefaults fallback so a
// hand-built config behaves like a loaded one.
func (s *Scheduler) maxWorkers() int {
	if s.Cfg != nil && s.Cfg.Scheduler.MaxWorkers > 0 {
		return s.Cfg.Scheduler.MaxWorkers
	}
	return defaultMaxWorkers
}

// maps lazily allocates the bookkeeping maps; callers hold s.mu. The
// Scheduler is a plain struct (no constructor), so the zero value must
// be usable.
func (s *Scheduler) maps() {
	if s.inflight == nil {
		s.inflight = map[string]bool{}
	}
	if s.backoff == nil {
		s.backoff = map[string]time.Time{}
	}
}

// logf reports through the configured logger, discarding when unset.
func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
