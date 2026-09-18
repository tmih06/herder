package scheduler

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/deliver"
	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// testConfig builds two repos sharing one codex profile so per-repo and
// per-kind caps can be exercised independently.
func testConfig() *config.Config {
	return &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join("/tmp", "herder-sched-test", "herder.db")},
		Scheduler: config.SchedulerConfig{
			MaxWorkers: 2,
		},
		Repositories: map[string]config.RepositoryConfig{
			"acme/web": {
				Agent:   config.RepoAgentConfig{Default: "codex-default"},
				Sandbox: config.SandboxConfig{Provider: "docker"},
			},
			"acme/api": {
				Agent:   config.RepoAgentConfig{Default: "codex-default"},
				Sandbox: config.SandboxConfig{Provider: "docker"},
			},
		},
		Agents: map[string]config.AgentConfig{
			"codex-default": {Kind: "codex"},
		},
	}
}

// testStore opens a real store in a temp dir so lease, transition, and
// event behavior is exercised against SQLite, not a stub.
func testStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "herder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// queueTask creates a task and walks it to QUEUED, returning the row.
func queueTask(t *testing.T, store *storage.Store, repo, ref string, priority int) tasks.Task {
	t.Helper()
	task, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: ref,
		Repository: repo, AgentProfile: "codex-default",
		Goal: "Fix the thing", Priority: priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []tasks.State{tasks.Eligible, tasks.Claimed, tasks.Queued} {
		if _, err := store.Transition(task.ID, next, "controller", "test"); err != nil {
			t.Fatalf("transition %s = %v", next, err)
		}
	}
	task.Status = tasks.Queued
	return task
}

// recorder scripts subprocess results by argv and records every call;
// one script feeds the docker, herdr, and gh runners alike. The mutex
// makes it safe under concurrent dispatchOne goroutines.
type recorder struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(name string, args []string) (sandbox.RunResult, error)
}

func (r *recorder) run(_ context.Context, name string, args []string) (sandbox.RunResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.respond != nil {
		return r.respond(name, args)
	}
	return sandbox.RunResult{}, nil
}

// called reports whether any recorded argv contains every fragment.
func (r *recorder) called(fragments ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		joined := strings.Join(call, " ")
		ok := true
		for _, f := range fragments {
			if !strings.Contains(joined, f) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// countCalls reports how many recorded argv contain every fragment.
func (r *recorder) countCalls(fragments ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		joined := strings.Join(call, " ")
		ok := true
		for _, f := range fragments {
			if !strings.Contains(joined, f) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// dockerRun adapts the script to the sandbox.Runner seam.
func (r *recorder) dockerRun(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	return r.run(ctx, name, args)
}

// herdrRun adapts the script to the agent.Runner seam.
func (r *recorder) herdrRun(ctx context.Context, name string, args ...string) (agent.RunResult, error) {
	out, err := r.run(ctx, name, args)
	return agent.RunResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, err
}

// happyRespond scripts a healthy fleet: existing clean repo, running
// container, live agent session, successful start, and readable labels.
func happyRespond(name string, args []string) (sandbox.RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "docker" && strings.HasPrefix(argv, "inspect --format"):
		return sandbox.RunResult{Stdout: "running\n"}, nil
	case name == "docker" && strings.HasPrefix(argv, "inspect "):
		return sandbox.RunResult{Stdout: inspectJSON("running", false)}, nil
	case name == "herdr" && strings.HasPrefix(argv, "agent get"):
		return sandbox.RunResult{Stdout: `{"result":{"agent":{"agent_status":"working","pane_id":"w5:p7"}}}`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "agent start"):
		return sandbox.RunResult{Stdout: `{"result":{"agent":{"pane_id":"w5:p7","workspace_id":"w5"}}}`}, nil
	case name == "gh" && strings.HasPrefix(argv, "issue view"):
		return sandbox.RunResult{Stdout: "agent-ready\n"}, nil
	}
	return sandbox.RunResult{}, nil
}

// inspectJSON renders the docker inspect document the provider parses.
func inspectJSON(status string, oom bool) string {
	return `[{"Id":"cid","Name":"/herder-x","Config":{"Image":"img"},` +
		`"State":{"Status":"` + status + `","OOMKilled":` + map[bool]string{true: "true", false: "false"}[oom] + `}}]`
}

// newScheduler wires a scheduler over scripted runners: the same
// recorder feeds the Docker provider, the Herdr launcher, and the gh
// engine so one script drives the whole dispatch pipeline.
func newScheduler(store *storage.Store, cfg *config.Config, rec *recorder) *Scheduler {
	return &Scheduler{
		Store: store,
		Cfg:   cfg,
		Dispatcher: &dispatch.Dispatcher{
			Store:    store,
			Launcher: &agent.Launcher{Runner: rec.herdrRun},
			Provider: &sandbox.DockerProvider{Runner: rec.dockerRun},
			Engine:   &deliver.Engine{Runner: rec.dockerRun},
		},
	}
}

// waitDispatch blocks until every spawned dispatchOne has returned so
// assertions observe the settled state, not a mid-flight write.
func waitDispatch(s *Scheduler) {
	s.wg.Wait()
}

// eventTypes lists a task's recorded event types in order.
func eventTypes(t *testing.T, store *storage.Store, taskID string) []string {
	t.Helper()
	events, err := store.ListEvents(taskID)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

// hasEvent reports whether the type list contains want.
func hasEvent(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

// taskStatus reloads one task's status.
func taskStatus(t *testing.T, store *storage.Store, taskID string) tasks.State {
	t.Helper()
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task.Status
}

// TestDispatchHonorsCapsAndPriority proves three queued tasks under
// max_workers=2 dispatch exactly two, priority first with FIFO breaking
// the tie: the high-priority task and the oldest normal task run while
// the newest normal task stays queued.
func TestDispatchHonorsCapsAndPriority(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	oldest := queueTask(t, store, "acme/web", "acme/web#1", 0)
	urgent := queueTask(t, store, "acme/web", "acme/web#2", 5)
	newest := queueTask(t, store, "acme/web", "acme/web#3", 0)

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, urgent.ID); got != tasks.Running {
		t.Errorf("urgent task = %s, want RUNNING", got)
	}
	if got := taskStatus(t, store, oldest.ID); got != tasks.Running {
		t.Errorf("oldest task = %s, want RUNNING (FIFO tie-break)", got)
	}
	if got := taskStatus(t, store, newest.ID); got != tasks.Queued {
		t.Errorf("newest task = %s, want QUEUED (cap reached)", got)
	}
	if n := rec.countCalls("herdr", "agent", "start"); n != 2 {
		t.Errorf("agent starts = %d, want exactly 2", n)
	}
}

// TestDispatchPerRepositoryCap proves a full repository bucket skips
// only that repo's tasks: the third acme/web task stays queued while
// acme/api's task dispatches into the remaining global slot.
func TestDispatchPerRepositoryCap(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.Scheduler.MaxWorkers = 3
	cfg.Scheduler.PerRepository = map[string]int{"acme/web": 1}
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	web1 := queueTask(t, store, "acme/web", "acme/web#1", 0)
	web2 := queueTask(t, store, "acme/web", "acme/web#2", 0)
	api := queueTask(t, store, "acme/api", "acme/api#1", 0)

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, web1.ID); got != tasks.Running {
		t.Errorf("first web task = %s, want RUNNING", got)
	}
	if got := taskStatus(t, store, web2.ID); got != tasks.Queued {
		t.Errorf("second web task = %s, want QUEUED (repo cap)", got)
	}
	if got := taskStatus(t, store, api.ID); got != tasks.Running {
		t.Errorf("api task = %s, want RUNNING (other repo unaffected)", got)
	}
}

// TestDispatchPerAgentCap proves the per-agent bucket keys on the
// resolved kind: two codex tasks under per_agent.codex=1 dispatch one.
func TestDispatchPerAgentCap(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.Scheduler.MaxWorkers = 3
	cfg.Scheduler.PerAgent = map[string]int{"codex": 1}
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	first := queueTask(t, store, "acme/web", "acme/web#1", 0)
	second := queueTask(t, store, "acme/api", "acme/api#1", 0)

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, first.ID); got != tasks.Running {
		t.Errorf("first codex task = %s, want RUNNING", got)
	}
	if got := taskStatus(t, store, second.ID); got != tasks.Queued {
		t.Errorf("second codex task = %s, want QUEUED (kind cap)", got)
	}
}

// TestExpiredLeaseRequeues proves a dead owner's lease on a
// PROVISIONING task returns it to QUEUED with a lease.expired event —
// and the requeued task does not instantly re-dispatch because the
// worker slot is still occupied.
func TestExpiredLeaseRequeues(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.Scheduler.MaxWorkers = 1
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	// A healthy RUNNING task occupies the one worker slot.
	occupant := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(occupant.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(occupant.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(occupant.ID, sandbox.ContainerName(occupant.ID), "herder-"+occupant.ID); err != nil {
		t.Fatal(err)
	}

	// The victim sits PROVISIONING under a lease that lapsed with its
	// dead owner (a negative TTL lands already expired).
	victim := queueTask(t, store, "acme/web", "acme/web#2", 0)
	if _, err := store.Transition(victim.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireLease(victim.ID, "dead-owner", -time.Minute); err != nil {
		t.Fatal(err)
	}

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, victim.ID); got != tasks.Queued {
		t.Errorf("expired-lease task = %s, want QUEUED", got)
	}
	types := eventTypes(t, store, victim.ID)
	if !hasEvent(types, tasks.EventLeaseExpired) {
		t.Errorf("want lease.expired event, got %v", types)
	}
	if got := taskStatus(t, store, occupant.ID); got != tasks.Running {
		t.Errorf("healthy occupant = %s, want RUNNING (untouched)", got)
	}
}

// TestAbandonedProvisioningRequeues proves a PROVISIONING task with no
// live dispatch lease — a dispatch that died between Provision and
// Launch, or a staged manual provision — returns to QUEUED with a
// task.dispatch_failed event instead of holding a worker slot forever,
// while a PROVISIONING task under a live lease is left to its owner.
func TestAbandonedProvisioningRequeues(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.Scheduler.MaxWorkers = 1
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	// A mid-dispatch task under a live lease: reconcile must skip it.
	leased := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(leased.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireLease(leased.ID, "daemon-live", time.Hour); err != nil {
		t.Fatal(err)
	}

	// The abandoned task: PROVISIONING with no lease at all. It fills
	// the one worker slot until reconcile frees it, so the requeue is
	// observable before the next dispatch pass could pick it up.
	abandoned := queueTask(t, store, "acme/web", "acme/web#2", 0)
	if _, err := store.Transition(abandoned.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, abandoned.ID); got != tasks.Queued {
		t.Errorf("abandoned task = %s, want QUEUED", got)
	}
	events, err := store.ListEvents(abandoned.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawFailed bool
	for _, e := range events {
		if e.Type == tasks.EventDispatchFailed &&
			strings.Contains(e.Payload, "provisioning abandoned: no dispatch lease") {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("want task.dispatch_failed with the abandoned reason, got %+v", events)
	}
	if got := taskStatus(t, store, leased.ID); got != tasks.Provisioning {
		t.Errorf("leased task = %s, want PROVISIONING (mid-dispatch, untouched)", got)
	}
	if types := eventTypes(t, store, leased.ID); hasEvent(types, tasks.EventDispatchFailed) {
		t.Errorf("leased task must not record dispatch_failed, got %v", types)
	}
}

// TestInflightProvisionedCountsOnce proves an inflight dispatch whose
// Provision already landed PROVISIONING occupies exactly one worker
// slot: the worker-states pass counts it, so the inflight pass must
// not count it again — a second queued task still fits the remaining
// slot while a third stays queued.
func TestInflightProvisionedCountsOnce(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	// Gate the Herdr start so first's dispatchOne stays inflight after
	// Provision lands PROVISIONING — the exact window where a second
	// count would double-book the slot.
	release := make(chan struct{})
	rec := &recorder{respond: func(name string, args []string) (sandbox.RunResult, error) {
		if name == "herdr" && strings.HasPrefix(strings.Join(args, " "), "agent start") {
			<-release
		}
		return happyRespond(name, args)
	}}
	s := newScheduler(store, cfg, rec)

	first := queueTask(t, store, "acme/web", "acme/web#1", 0)
	second := queueTask(t, store, "acme/web", "acme/web#2", 0)
	third := queueTask(t, store, "acme/web", "acme/web#3", 0)

	s.Tick(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for taskStatus(t, store, first.ID) != tasks.Provisioning {
		if time.Now().After(deadline) {
			t.Fatal("first task never reached PROVISIONING")
		}
		time.Sleep(time.Millisecond)
	}

	// Tick 2 sees first PROVISIONING and still inflight: counted once,
	// one slot remains for second; third hits the cap.
	s.Tick(context.Background())
	close(release)
	waitDispatch(s)

	if got := taskStatus(t, store, first.ID); got != tasks.Running {
		t.Errorf("first task = %s, want RUNNING", got)
	}
	if got := taskStatus(t, store, second.ID); got != tasks.Running {
		t.Errorf("second task = %s, want RUNNING (inflight task counted once)", got)
	}
	if got := taskStatus(t, store, third.ID); got != tasks.Queued {
		t.Errorf("third task = %s, want QUEUED (cap reached)", got)
	}
}

// TestMissingSessionDisconnects proves a RUNNING task whose session
// binding is gone lands WAITING_FOR_HUMAN with worker.disconnected —
// the recovery policy after the supervisor clears a dead binding.
func TestMissingSessionDisconnects(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	// No SetBinding: the supervisor's agent.exited already cleared it.

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.WaitingForHuman {
		t.Errorf("session-less task = %s, want WAITING_FOR_HUMAN", got)
	}
	types := eventTypes(t, store, task.ID)
	if !hasEvent(types, tasks.EventWorkerDisconnected) {
		t.Errorf("want worker.disconnected event, got %v", types)
	}
}

// TestAgentTimeoutStopsWorker proves a RUNNING task past its agent
// profile's timeout stops the live pane and lands TIMED_OUT with the
// task.timed_out event (SPEC section 15: time limits enforced).
func TestAgentTimeoutStopsWorker(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.Agents["codex-default"] = config.AgentConfig{Kind: "codex", Timeout: "1ms"}
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(task.ID, sandbox.ContainerName(task.ID), "herder-"+task.ID); err != nil {
		t.Fatal(err)
	}
	// The RUNNING transition stamped started_at; the 1ms budget is
	// already blown by the time the tick runs.
	time.Sleep(5 * time.Millisecond)

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.TimedOut {
		t.Errorf("timed-out task = %s, want TIMED_OUT", got)
	}
	types := eventTypes(t, store, task.ID)
	if !hasEvent(types, tasks.EventTaskTimedOut) {
		t.Errorf("want task.timed_out event, got %v", types)
	}
	if !rec.called("herdr", "pane", "close") {
		t.Error("timed-out worker's pane should be closed")
	}
}

// TestOOMKilledSandboxFails proves a RUNNING task whose container the
// engine OOM-killed lands FAILED with the resource-limit reason — the
// kill is a verdict on the work, not a transient disconnect.
func TestOOMKilledSandboxFails(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: func(name string, args []string) (sandbox.RunResult, error) {
		argv := strings.Join(args, " ")
		if name == "docker" && strings.HasPrefix(argv, "inspect ") {
			return sandbox.RunResult{Stdout: inspectJSON("exited", true)}, nil
		}
		return happyRespond(name, args)
	}}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(task.ID, sandbox.ContainerName(task.ID), "herder-"+task.ID); err != nil {
		t.Fatal(err)
	}

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.Failed {
		t.Errorf("OOM-killed task = %s, want FAILED", got)
	}
	types := eventTypes(t, store, task.ID)
	if !hasEvent(types, tasks.EventWorkerDisconnected) {
		t.Errorf("want worker.disconnected event, got %v", types)
	}
}

// TestProvisionFailureBacksOff proves a failed provision requeues the
// task with task.dispatch_failed and the retry backoff suppresses an
// immediate second attempt: a tick inside RetryDelay runs no new
// provision.
func TestProvisionFailureBacksOff(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: func(name string, args []string) (sandbox.RunResult, error) {
		argv := strings.Join(args, " ")
		switch {
		case name == "docker" && strings.HasPrefix(argv, "inspect --format"):
			return sandbox.RunResult{ExitCode: 1, Stderr: "No such object: herder-x"}, nil
		case name == "docker" && strings.HasPrefix(argv, "create "):
			return sandbox.RunResult{ExitCode: 1, Stderr: "docker daemon exploded"}, nil
		}
		return happyRespond(name, args)
	}}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.Queued {
		t.Errorf("failed provision task = %s, want QUEUED", got)
	}
	types := eventTypes(t, store, task.ID)
	if !hasEvent(types, tasks.EventDispatchFailed) {
		t.Errorf("want task.dispatch_failed event, got %v", types)
	}
	creates := rec.countCalls("docker", "create")

	// A second tick inside the backoff window must not retry.
	s.Tick(context.Background())
	waitDispatch(s)

	if got := rec.countCalls("docker", "create"); got != creates {
		t.Errorf("backoff should suppress re-dispatch: creates %d -> %d", creates, got)
	}
	if got := taskStatus(t, store, task.ID); got != tasks.Queued {
		t.Errorf("task = %s, want QUEUED", got)
	}
}

// TestForeignLeaseBlocksDispatch proves a second scheduler cannot steal
// a live lease: its dispatch attempt ends at AcquireLease and no second
// session ever starts (SPEC section 50).
func TestForeignLeaseBlocksDispatch(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)
	// A competing scheduler holds the live lease.
	if err := store.AcquireLease(task.ID, "daemon-other", time.Hour); err != nil {
		t.Fatal(err)
	}

	s.Tick(context.Background())
	waitDispatch(s)

	if rec.called("herdr", "agent", "start") {
		t.Error("foreign-held lease must block the launch: no agent start")
	}
	if got := taskStatus(t, store, task.ID); got != tasks.Queued {
		t.Errorf("task = %s, want QUEUED (still owned elsewhere)", got)
	}
	lease, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "daemon-other" {
		t.Errorf("lease owner = %q, want daemon-other (not stolen)", lease.Owner)
	}
}

// TestLiveLeaseSkipsReconcile proves a RUNNING task under a live
// dispatch lease is left alone: a mid-dispatch task can sit RUNNING
// before its session binding lands, so the missing-session check must
// not read a healthy mid-dispatch task as a dead worker. Once the
// lease lapses the same task disconnects normally.
func TestLiveLeaseSkipsReconcile(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	rec := &recorder{respond: happyRespond}
	s := newScheduler(store, cfg, rec)

	task := queueTask(t, store, "acme/web", "acme/web#1", 0)
	if _, err := store.Transition(task.ID, tasks.Provisioning, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(task.ID, tasks.Running, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	// No session bound yet — the mid-dispatch window — but the owner's
	// lease is live.
	if err := store.AcquireLease(task.ID, "daemon-mid-dispatch", time.Hour); err != nil {
		t.Fatal(err)
	}

	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.Running {
		t.Errorf("leased task = %s, want RUNNING (mid-dispatch, untouched)", got)
	}
	if types := eventTypes(t, store, task.ID); hasEvent(types, tasks.EventWorkerDisconnected) {
		t.Errorf("live lease must suppress worker.disconnected, got %v", types)
	}

	// The lease lapses: expireLeases deletes the row, reconcile sees no
	// lease, and the session-less worker disconnects as usual.
	if err := store.AcquireLease(task.ID, "daemon-mid-dispatch", -time.Minute); err != nil {
		t.Fatal(err)
	}
	s.Tick(context.Background())
	waitDispatch(s)

	if got := taskStatus(t, store, task.ID); got != tasks.WaitingForHuman {
		t.Errorf("expired-lease task = %s, want WAITING_FOR_HUMAN", got)
	}
	if types := eventTypes(t, store, task.ID); !hasEvent(types, tasks.EventWorkerDisconnected) {
		t.Errorf("want worker.disconnected after lease lapse, got %v", types)
	}
}
