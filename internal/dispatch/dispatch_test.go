package dispatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/deliver"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
	"github.com/tmih06/herder/internal/testutil"
)

// testConfig builds one repo with a default codex profile, a docker
// sandbox, and trigger labels for the label tests.
func testConfig() *config.Config {
	return &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join("/tmp", "herder-test", "herder.db")},
		Repositories: map[string]config.RepositoryConfig{
			"acme/web": {
				Agent:   config.RepoAgentConfig{Default: "codex-default"},
				Trigger: config.TriggerConfig{Labels: []string{"agent-ready"}},
				Sandbox: config.SandboxConfig{Provider: "docker", Image: "golang:1.22"},
			},
		},
		Agents: map[string]config.AgentConfig{
			"codex-default": {
				Kind:      "codex",
				Resources: map[string]string{"cpu": "2", "memory": "4Gi"},
			},
		},
	}
}

// queueTask creates a task and walks it to QUEUED, returning the row.
func queueTask(t *testing.T, store *storage.Store) tasks.Task {
	t.Helper()
	task, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
		Goal: "Fix the thing",
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

// launchRespond scripts a ready sandbox plus a successful Herdr launch:
// full inspect reports running, workspace create answers with pane ids,
// agent get reports the detected kind, and every other call succeeds
// empty.
func launchRespond(name string, args []string) (sandbox.RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "docker" && strings.HasPrefix(argv, "inspect "):
		return sandbox.RunResult{Stdout: `[{"Id":"cid","Name":"/herder-x","Config":{"Image":"img"},"State":{"Status":"running"}}]`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "workspace create"):
		return sandbox.RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "agent get"):
		return sandbox.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "pane process-info"):
		return sandbox.RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
	case name == "gh" && strings.HasPrefix(argv, "issue view"):
		return sandbox.RunResult{Stdout: "agent-ready\n"}, nil
	}
	return sandbox.RunResult{}, nil
}

// provisionRespond scripts a fresh provision: no repo yet (clone), clean
// tree, no container (create + start).
func provisionRespond(name string, args []string) (sandbox.RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "git" && strings.Contains(argv, "rev-parse --git-dir"):
		return sandbox.RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
	case name == "docker" && strings.HasPrefix(argv, "inspect "):
		return sandbox.RunResult{ExitCode: 1, Stderr: "No such object: herder-x"}, nil
	}
	return sandbox.RunResult{}, nil
}

// provisionThenLaunchRespond scripts a fresh provision followed by a
// ready sandbox: the first inspect (the provision container check)
// reports nothing, the full inspect Launch runs reports running, and the
// Herdr launch flow answers with pane ids and a detected kind.
func provisionThenLaunchRespond(name string, args []string) (sandbox.RunResult, error) {
	argv := strings.Join(args, " ")
	switch {
	case name == "git" && strings.Contains(argv, "rev-parse --git-dir"):
		return sandbox.RunResult{ExitCode: 1, Stderr: "not a git repo"}, nil
	case name == "docker" && strings.HasPrefix(argv, "inspect --format"):
		return sandbox.RunResult{ExitCode: 1, Stderr: "No such object: herder-x"}, nil
	case name == "docker" && strings.HasPrefix(argv, "inspect "):
		return sandbox.RunResult{Stdout: `[{"Id":"cid","Name":"/herder-x","Config":{"Image":"img"},"State":{"Status":"running"}}]`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "workspace create"):
		return sandbox.RunResult{Stdout: `{"result":{"root_pane":{"pane_id":"w5:p7"},"workspace":{"workspace_id":"w5"}}}`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "agent get"):
		return sandbox.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"idle","pane_id":"w5:p7"}}}`}, nil
	case name == "herdr" && strings.HasPrefix(argv, "pane process-info"):
		return sandbox.RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
	case name == "gh" && strings.HasPrefix(argv, "issue view"):
		return sandbox.RunResult{Stdout: "agent-ready\n"}, nil
	}
	return sandbox.RunResult{}, nil
}

// TestBranchForTaskFallback derives the deterministic branch: claimed
// branch first, then herder/<issue> from the source ref, then the id.
func TestBranchForTaskFallback(t *testing.T) {
	for task, want := range map[tasks.Task]string{
		{BranchName: "herder/7-fix"}:                 "herder/7-fix",
		{BranchName: "  herder/9  "}:                 "herder/9",
		{SourceRef: "acme/web#42"}:                   "herder/42",
		{ID: "task_abc", SourceRef: "acme/web#nope"}: "herder/task_abc",
		{ID: "task_xyz"}:                             "herder/task_xyz",
	} {
		if got := BranchForTask(task); got != want {
			t.Errorf("BranchForTask(%+v) = %q, want %q", task, got, want)
		}
	}
}

// TestSpecForTask proves the provision spec carries the repo image, the
// agent profile's resource limits, the deterministic branch, and a
// workspace rooted beside the database.
func TestSpecForTask(t *testing.T) {
	cfg := testConfig()
	task := tasks.Task{
		ID: "task_abc", Repository: "acme/web",
		AgentProfile: "codex-default", SourceRef: "acme/web#7",
	}
	spec := SpecForTask(cfg, task)
	if spec.Image != "golang:1.22" || spec.CPUs != "2" || spec.Memory != "4Gi" {
		t.Errorf("spec resources = %+v, want image/cpu/memory from config", spec)
	}
	if spec.Branch != "herder/7" || spec.TaskID != "task_abc" || spec.Repository != "acme/web" {
		t.Errorf("spec identity = %+v", spec)
	}
	if want := SandboxRoot(cfg.Database.Path); spec.WorkspaceRoot != want {
		t.Errorf("workspace root = %q, want %q", spec.WorkspaceRoot, want)
	}
}

// TestLaunchQueuedAcquiresAndReleasesLease proves a QUEUED launch takes
// the dispatch lease before any subprocess, releases it on success, and
// lands the task RUNNING with the agent.started event.
func TestLaunchQueuedAcquiresAndReleasesLease(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	rec := &testutil.Recorder{Respond: launchRespond}
	var logs []string
	d := &Dispatcher{
		Store:    store,
		Launcher: &agent.Launcher{Runner: rec.HerdrRun, LookPath: testutil.FakeLookPath},
		Provider: &sandbox.DockerProvider{Runner: rec.DockerRun},
		Engine:   &deliver.Engine{Runner: rec.DockerRun},
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	if err := d.Launch(context.Background(), testConfig(), &task, "", ""); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if task.Status != tasks.Running {
		t.Errorf("in-memory status = %s, want RUNNING", task.Status)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Running {
		t.Errorf("stored status = %s, want RUNNING", got.Status)
	}
	if got.AgentSessionID != agent.SessionName(task.ID) {
		t.Errorf("session binding = %q, want %q", got.AgentSessionID, agent.SessionName(task.ID))
	}
	// The lease is gone once dispatch ends: acquire ran (its event is
	// durable) and release deleted the row.
	if _, err := store.GetLease(task.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetLease after launch = %v, want ErrNotFound", err)
	}
	types := testutil.EventTypes(t, store, task.ID)
	for _, want := range []string{tasks.EventLeaseAcquired, tasks.EventLeaseReleased, "agent.started"} {
		if !testutil.HasEvent(types, want) {
			t.Errorf("events %v missing %q", types, want)
		}
	}
	var started bool
	for _, line := range logs {
		if strings.Contains(line, "agent codex started for "+task.ID) {
			started = true
		}
	}
	if !started {
		t.Errorf("Logf should carry the started line, got %v", logs)
	}
}

// TestLaunchQueuedLeaseHeld proves a foreign-held lease refuses the
// launch before any subprocess runs: no Herdr call, no state change,
// and the error names the holding owner.
func TestLaunchQueuedLeaseHeld(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	if err := store.AcquireLease(task.ID, "sched-1", config.DefaultLeaseTTL); err != nil {
		t.Fatal(err)
	}
	rec := &testutil.Recorder{Respond: launchRespond}
	d := &Dispatcher{
		Store:    store,
		Launcher: &agent.Launcher{Runner: rec.HerdrRun, LookPath: testutil.FakeLookPath},
		Provider: &sandbox.DockerProvider{Runner: rec.DockerRun},
	}
	err := d.Launch(context.Background(), testConfig(), &task, "", "")
	if err == nil {
		t.Fatal("launch under a foreign lease should fail, got nil")
	}
	if !strings.Contains(err.Error(), task.ID) || !strings.Contains(err.Error(), "sched-1") {
		t.Errorf("error should name the task and the lease owner, got %v", err)
	}
	if len(rec.Calls()) != 0 {
		t.Errorf("no subprocess may run under a foreign lease, ran %v", rec.Calls())
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Queued {
		t.Errorf("status = %s, want QUEUED (launch refused, not failed)", got.Status)
	}
	// The foreign lease is untouched: the refused dispatcher must not
	// release another owner's claim.
	lease, err := store.GetLease(task.ID)
	if err != nil || lease.Owner != "sched-1" {
		t.Errorf("lease = %+v, %v; want sched-1 still holding", lease, err)
	}
}

// TestProvisionEmitsEventAndAdvances proves provisioning walks QUEUED ->
// PROVISIONING and stops there — RUNNING is Launch's job once a session
// is bound — and records sandbox.provisioned.
func TestProvisionEmitsEventAndAdvances(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	rec := &testutil.Recorder{Respond: provisionRespond}
	var logs []string
	d := &Dispatcher{
		Store:    store,
		Provider: &sandbox.DockerProvider{Runner: rec.DockerRun},
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	if err := d.Provision(context.Background(), testConfig(), &task); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if task.Status != tasks.Provisioning {
		t.Errorf("in-memory status = %s, want PROVISIONING", task.Status)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Provisioning {
		t.Errorf("stored status = %s, want PROVISIONING", got.Status)
	}
	if !testutil.HasEvent(testutil.EventTypes(t, store, task.ID), "sandbox.provisioned") {
		t.Error("sandbox.provisioned event missing")
	}
	var running bool
	for _, line := range logs {
		if strings.Contains(line, "running (branch herder/7)") {
			running = true
		}
	}
	if !running {
		t.Errorf("Logf should carry the sandbox-running line, got %v", logs)
	}
}

// TestLaunchAdvancesProvisioningToRunning proves the dispatch pipeline's
// second half: a task Provision left PROVISIONING walks to RUNNING when
// Launch binds the session — the started_at stamp lands with the agent,
// not with the container.
func TestLaunchAdvancesProvisioningToRunning(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	rec := &testutil.Recorder{Respond: provisionThenLaunchRespond}
	d := &Dispatcher{
		Store:    store,
		Launcher: &agent.Launcher{Runner: rec.HerdrRun, LookPath: testutil.FakeLookPath},
		Provider: &sandbox.DockerProvider{Runner: rec.DockerRun},
		Engine:   &deliver.Engine{Runner: rec.DockerRun},
	}
	if err := d.Provision(context.Background(), testConfig(), &task); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if task.Status != tasks.Provisioning {
		t.Fatalf("after Provision status = %s, want PROVISIONING", task.Status)
	}
	if err := d.Launch(context.Background(), testConfig(), &task, "", ""); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if task.Status != tasks.Running {
		t.Errorf("in-memory status = %s, want RUNNING", task.Status)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.Running {
		t.Errorf("stored status = %s, want RUNNING", got.Status)
	}
	if got.AgentSessionID != agent.SessionName(task.ID) {
		t.Errorf("session binding = %q, want %q", got.AgentSessionID, agent.SessionName(task.ID))
	}
	types := testutil.EventTypes(t, store, task.ID)
	for _, want := range []string{"sandbox.provisioned", "agent.started"} {
		if !testutil.HasEvent(types, want) {
			t.Errorf("events %v missing %q", types, want)
		}
	}
}

// TestProvisionRejectsUnconfiguredRepo proves the repo gate fails before
// any subprocess with the original message.
func TestProvisionRejectsUnconfiguredRepo(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	task.Repository = "acme/unknown"
	rec := &testutil.Recorder{Respond: provisionRespond}
	d := &Dispatcher{Store: store, Provider: &sandbox.DockerProvider{Runner: rec.DockerRun}}
	err := d.Provision(context.Background(), testConfig(), &task)
	if err == nil || !strings.Contains(err.Error(), `task repository "acme/unknown" not in config`) {
		t.Fatalf("Provision = %v, want the not-in-config error", err)
	}
	if len(rec.Calls()) != 0 {
		t.Errorf("repo gate must fail before any subprocess, ran %v", rec.Calls())
	}
}

// TestAdvanceToRunningRejectsCancelled proves the mid-launch race is
// reported, not swallowed: a task cancelled after the dispatcher read it
// fails the QUEUED -> PROVISIONING transition and the caller sees the
// error instead of emitting agent.started for a cancelled task.
func TestAdvanceToRunningRejectsCancelled(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	// The operator cancels after the dispatcher fetched the row: the
	// in-memory copy still reads QUEUED while the store moved on.
	if _, err := store.Transition(task.ID, tasks.Cancelled, "controller", "test"); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Store: store}
	if err := d.advanceToRunning(context.Background(), &task, true); err == nil {
		t.Fatal("advance on a cancelled task should fail, got nil")
	}
	if task.Status != tasks.Queued {
		t.Errorf("in-memory status = %s, want QUEUED (no partial advance)", task.Status)
	}
}

// TestStaleLabels proves the removal set carries trigger and stage
// labels currently on the issue, minus the one being applied, sorted.
func TestStaleLabels(t *testing.T) {
	repo := testConfig().Repositories["acme/web"]
	got := staleLabels(
		[]string{"bug", "agent-review", "agent-ready", "agent-running"},
		repo, "agent-review")
	want := []string{"agent-ready", "agent-running"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("StaleLabels = %v, want %v", got, want)
	}
}

// TestMarkIssueRunningWarnsNeverFails proves a broken gh warns through
// Warnf without failing the launch path.
func TestMarkIssueRunningWarnsNeverFails(t *testing.T) {
	store := testutil.OpenStore(t)
	task := queueTask(t, store)
	rec := &testutil.Recorder{Respond: func(name string, args []string) (sandbox.RunResult, error) {
		if name == "gh" {
			return sandbox.RunResult{ExitCode: 1, Stderr: "gh: command not found"}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	var warns []string
	d := &Dispatcher{
		Store:  store,
		Engine: &deliver.Engine{Runner: rec.DockerRun},
		Warnf:  func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) },
	}
	d.MarkIssueRunning(&task, testConfig().Repositories["acme/web"])
	if len(warns) != 1 || !strings.Contains(warns[0], "could not mark acme/web#7 running") {
		t.Errorf("Warnf should carry the label warning once, got %v", warns)
	}
}

// TestStopWorkerThawsBeforeStopping proves a paused container unpauses
// before the session's pane closes: a frozen agent would survive the
// close and wake as an orphan.
func TestStopWorkerThawsBeforeStopping(t *testing.T) {
	rec := &testutil.Recorder{Respond: func(name string, args []string) (sandbox.RunResult, error) {
		argv := strings.Join(args, " ")
		switch {
		case name == "docker" && strings.HasPrefix(argv, "inspect "):
			return sandbox.RunResult{Stdout: "paused"}, nil
		case name == "herdr" && strings.HasPrefix(argv, "agent get"):
			return sandbox.RunResult{Stdout: `{"result":{"agent":{"agent":"codex","agent_status":"working","pane_id":"w1:p1"}}}`}, nil
		case name == "herdr" && strings.HasPrefix(argv, "pane process-info"):
			return sandbox.RunResult{Stdout: `{"result":{"process_info":{"foreground_processes":[{"name":"codex"}]}}}`}, nil
		}
		return sandbox.RunResult{}, nil
	}}
	d := &Dispatcher{
		Launcher: &agent.Launcher{Runner: rec.HerdrRun, LookPath: testutil.FakeLookPath},
		Provider: &sandbox.DockerProvider{Runner: rec.DockerRun},
	}
	task := &tasks.Task{ID: "task_abc", Status: tasks.Paused, AgentSessionID: "herder-task_abc"}
	if err := d.StopWorker(context.Background(), task); err != nil {
		t.Fatalf("StopWorker = %v", err)
	}
	unpause, paneClose := -1, -1
	for i, c := range rec.Calls() {
		argv := strings.Join(c, " ")
		if strings.Contains(argv, "docker unpause") {
			unpause = i
		}
		if strings.Contains(argv, "pane close") {
			paneClose = i
		}
	}
	if unpause < 0 || paneClose < 0 {
		t.Fatalf("want docker unpause and pane close, ran %v", rec.Calls())
	}
	if unpause > paneClose {
		t.Errorf("unpause (call %d) must precede pane close (call %d)", unpause, paneClose)
	}
}
