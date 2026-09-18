package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestSetBindingRoundTrip proves the task-sandbox-session link from SPEC
// section 18 survives as durable columns, not just event payloads.
func TestSetBindingRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "herder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	task, err := store.CreateTask(CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
		Goal: "Fix the flaky login retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentSessionID != "" || task.SandboxID != "" {
		t.Fatalf("new task should carry no binding, got %+v", task)
	}
	if task.Goal != "Fix the flaky login retry" {
		t.Fatalf("new task should carry the issue goal, got %+v", task)
	}

	if err := store.SetBinding(task.ID, "herder-task_abc", "herder-task_abc"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "herder-task_abc" || got.AgentSessionID != "herder-task_abc" {
		t.Errorf("binding = sandbox %q session %q, want both herder-task_abc",
			got.SandboxID, got.AgentSessionID)
	}
	if got.Goal != "Fix the flaky login retry" {
		t.Errorf("goal = %q, want the issue goal to round-trip", got.Goal)
	}

	if err := store.SetBinding(task.ID, "", "herder-task_xyz"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "herder-task_abc" {
		t.Errorf("empty sandbox must leave the stored value, got %q", got.SandboxID)
	}
	if got.AgentSessionID != "herder-task_xyz" {
		t.Errorf("session = %q, want herder-task_xyz", got.AgentSessionID)
	}

	if err := store.SetBinding("task_missing", "sbx", "sess"); err == nil {
		t.Error("binding an unknown task should fail")
	}
}

// TestClearSessionBinding proves the failed-launch cleanup drops only the
// session half of the task link: the sandbox stays bound for recovery,
// clearing an unbound task is a no-op, and unknown ids report ErrNotFound.
func TestClearSessionBinding(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "herder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	task, err := store.CreateTask(CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(task.ID, "sbx-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearSessionBinding(task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSessionID != "" {
		t.Errorf("session = %q, want cleared", got.AgentSessionID)
	}
	if got.SandboxID != "sbx-1" {
		t.Errorf("sandbox = %q, want the clear to leave sbx-1", got.SandboxID)
	}

	if err := store.ClearSessionBinding(task.ID); err != nil {
		t.Errorf("clearing an already-empty session should be a no-op, got %v", err)
	}
	if err := store.ClearSessionBinding("task_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("clearing an unknown task = %v, want ErrNotFound", err)
	}
}

// TestBindingSurvivesReopen proves crash recovery (SPEC section 48) keeps
// the session link: reopening the same file returns the binding.
func TestBindingSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herder.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(task.ID, "sbx-1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sbx-1" || got.AgentSessionID != "sess-1" {
		t.Errorf("reopened binding = %+v, want sbx-1/sess-1", got)
	}
}

// TestLegacyDBMigration proves databases written before the binding and
// goal columns existed still open, read back with empty values, and
// accept new ones.
func TestLegacyDBMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herder.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const legacy = `CREATE TABLE tasks (
		id TEXT PRIMARY KEY, source_provider TEXT NOT NULL, source_ref TEXT NOT NULL,
		status TEXT NOT NULL, repository TEXT NOT NULL, agent_profile TEXT NOT NULL,
		branch_name TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
	CREATE TABLE task_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
		event_type TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT NOT NULL,
		payload_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL);
	CREATE TABLE webhook_deliveries (
		delivery_id TEXT PRIMARY KEY, source_provider TEXT NOT NULL DEFAULT '',
		source_ref TEXT NOT NULL DEFAULT '', repository TEXT NOT NULL DEFAULT '',
		decision TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
	INSERT INTO tasks (id, source_provider, source_ref, status, repository,
		agent_profile, branch_name, attempt, created_at, updated_at)
		VALUES ('task_legacy', 'github', 'acme/web#3', 'QUEUED', 'acme/web',
		'codex-default', 'herder/3', 1, '2026-09-13T00:00:00Z', '2026-09-13T00:00:00Z');`
	if _, err := raw.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("legacy db should migrate on open: %v", err)
	}
	defer store.Close()
	got, err := store.GetTask("task_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSessionID != "" || got.SandboxID != "" || got.Goal != "" {
		t.Errorf("legacy task bindings and goal should read empty, got %+v", got)
	}
	if got.Priority != 0 || !got.StartedAt.IsZero() {
		t.Errorf("legacy task scheduler fields should read zero, got %+v", got)
	}
	if err := store.SetBinding("task_legacy", "sbx-9", "sess-9"); err != nil {
		t.Fatalf("migrated db should accept bindings: %v", err)
	}
	created, err := store.CreateTask(CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#9",
		Repository: "acme/web", AgentProfile: "codex-default",
		Goal: "Ship the goal column",
	})
	if err != nil {
		t.Fatalf("migrated db should accept a goal: %v", err)
	}
	got, err = store.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "Ship the goal column" {
		t.Errorf("migrated db should return the goal, got %q", got.Goal)
	}
}
