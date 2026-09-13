package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tmih06/herder/internal/tasks"
)

// openTestStore opens a throwaway store backed by a temp file (WAL + file
// semantics, unlike :memory:, so reopen really replays the disk image).
func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herder.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func createInput() CreateInput {
	return createInputWithRef("acme/web#7")
}

// createInputWithRef builds a CreateTask input for one distinct issue.
// Purpose: the UNIQUE(source_provider, source_ref) invariant means each
// task in a store needs its own issue reference.
func createInputWithRef(ref string) CreateInput {
	return CreateInput{
		SourceProvider: "github",
		SourceRef:      ref,
		Repository:     "acme/web",
		AgentProfile:   "codex-default",
		ActorType:      "controller",
		ActorID:        "test",
	}
}

// Create -> transition -> custom event must survive close/reopen with the
// same observed state and full history (daemon kill/restart simulation).
func TestDurabilityAcrossReopen(t *testing.T) {
	store, path := openTestStore(t)

	created, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if _, err := store.Transition(created.ID, tasks.Eligible, "controller", "daemon"); err != nil {
		t.Fatalf("Transition = %v", err)
	}
	if _, err := store.AppendEvent(created.ID, "agent.working", "agent", "codex",
		`{"pane":"w3:p2"}`); err != nil {
		t.Fatalf("AppendEvent = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after close = %v", err)
	}
	defer reopened.Close()

	got, err := reopened.GetTask(created.ID)
	if err != nil {
		t.Fatalf("GetTask after reopen = %v", err)
	}
	if got.Status != tasks.Eligible {
		t.Errorf("status after reopen = %s, want ELIGIBLE", got.Status)
	}
	if got.SourceRef != "acme/web#7" || got.AgentProfile != "codex-default" {
		t.Errorf("task fields did not survive reopen: %+v", got)
	}

	events, err := reopened.ListEvents(created.ID)
	if err != nil {
		t.Fatalf("ListEvents after reopen = %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events after reopen, got %d", len(events))
	}
	wantTypes := []string{tasks.EventCreated, tasks.EventTransition, "agent.working"}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
		if events[i].CreatedAt.IsZero() {
			t.Errorf("event[%d] must carry a timestamp", i)
		}
		if events[i].ActorType == "" || events[i].ActorID == "" {
			t.Errorf("event[%d] must carry attribution", i)
		}
	}
}

// An illegal jump must fail and leave neither a new status nor an event.
func TestIllegalTransitionLeavesNoTrace(t *testing.T) {
	store, _ := openTestStore(t)
	created, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if _, err := store.Transition(created.ID, tasks.Running, "controller", "daemon"); err == nil {
		t.Fatal("expected illegal transition to fail, got nil")
	}
	got, err := store.GetTask(created.ID)
	if err != nil {
		t.Fatalf("GetTask = %v", err)
	}
	if got.Status != tasks.Discovered {
		t.Errorf("status = %s, want DISCOVERED after rejected jump", got.Status)
	}
	events, err := store.ListEvents(created.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	if len(events) != 1 || events[0].Type != tasks.EventCreated {
		t.Errorf("rejected jump must append no event, got %+v", events)
	}
}

// TestGetUnknownTask Unknown ids return ErrNotFound, not an empty task.
func TestGetUnknownTask(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.GetTask("task_missing"); err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAppendEventUnknownTask Events against unknown tasks are rejected.
func TestAppendEventUnknownTask(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.AppendEvent("task_missing", "agent.working", "agent", "codex", "{}"); err == nil {
		t.Fatal("expected error for unknown task, got nil")
	}
}

// TestListTasksEmptyAndOrdered Fresh stores list zero tasks; creates list in insertion order.
func TestListTasksEmptyAndOrdered(t *testing.T) {
	store, _ := openTestStore(t)
	got, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh store must list zero tasks, got %d", len(got))
	}
	first, err := store.CreateTask(createInputWithRef("acme/web#7"))
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	second, err := store.CreateTask(createInputWithRef("acme/web#8"))
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	got, err = store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Errorf("tasks must list in creation order, got %v", got)
	}
}

// TestOpenRetriesThroughTransientJournalLock Open shares one SQLite file
// between the daemon and the CLI, and PRAGMA journal_mode bypasses
// busy_timeout with instant SQLITE_BUSY under a sibling's lock. Open must
// therefore retry the mode switch: the holder keeps an uncommitted write
// for 400ms on a not-yet-WAL database, and Open must succeed with WAL
// active once the holder rolls back.
func TestOpenRetriesThroughTransientJournalLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herder.db")
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open holder = %v", err)
	}
	defer holder.Close()
	if _, err := holder.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE = %v", err)
	}
	if _, err := holder.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("holder write = %v", err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = holder.Exec("ROLLBACK")
	}()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open through a transient lock = %v", err)
	}
	defer store.Close()
	var mode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode = %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}
