// Package storage persists tasks and their event history in SQLite
// behind a small repository interface (SPEC sections 44-48).
//
// Why: Herder must survive a daemon kill and restart without forgetting
// tasks or history, while staying hidden behind an interface so
// PostgreSQL can replace SQLite later. Every mutation runs in one
// transaction: the state-machine check, the status update, and the event
// append commit or roll back together.
// Inputs: filesystem path for the database file (parent dirs created).
// Flow: Open (mkdir, pragmas, migrate) -> CreateTask / Transition /
// AppendEvent in transactions -> Close. Reads observe committed state.
// Returns: tasks.Task / tasks.Event values, or ErrNotFound for unknown ids.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/tasks"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a task id does not exist.
var ErrNotFound = errors.New("storage: task not found")

// schema creates the v0.1 tables: tasks plus the append-only task_events
// log that mirrors SPEC sections 46-47.
const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	source_provider TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	status TEXT NOT NULL,
	repository TEXT NOT NULL,
	agent_profile TEXT NOT NULL,
	branch_name TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	event_type TEXT NOT NULL,
	actor_type TEXT NOT NULL,
	actor_id TEXT NOT NULL,
	payload_json TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_events_task_id ON task_events(task_id, id);
`

// CreateInput carries the fields needed to open a new task.
type CreateInput struct {
	SourceProvider string
	SourceRef      string
	Repository     string
	AgentProfile   string
	BranchName     string
	ActorType      string
	ActorID        string
}

// Store is the SQLite-backed task repository.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates parent directories, opens (or creates) the SQLite file,
// enables WAL durability with foreign keys, and migrates the schema.
func Open(path string) (*Store, error) {
	expanded := expandPath(path)
	if dir := filepath.Dir(expanded); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("storage: create state dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", expanded)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", expanded, err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate schema: %w", err)
	}
	return &Store{db: db, path: expanded}, nil
}

// Path returns the database file in use.
// Purpose: surface the state location in logs, status, and diagnostics.
// Returns the expanded path passed to Open.
func (s *Store) Path() string { return s.path }

// Close releases the database handle.
// Purpose: flush WAL and free the handle on shutdown or test cleanup.
// Returns the close error, if any.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database answers.
// Purpose: storage half of the doctor check. Flow: pool ping plus a
// SELECT 1 probe. Returns a wrapped error when unreachable.
func (s *Store) Ping() error {
	if err := s.db.Ping(); err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	var one int
	if err := s.db.QueryRow("SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("storage: probe query: %w", err)
	}
	return nil
}

// CreateTask inserts a DISCOVERED task and its task.created event atomically.
func (s *Store) CreateTask(in CreateInput) (tasks.Task, error) {
	task := tasks.New(in.SourceProvider, in.SourceRef, in.Repository, in.AgentProfile)
	task.BranchName = in.BranchName
	actorType, actorID := orDefault(in.ActorType, "controller"), orDefault(in.ActorID, "cli")
	event := tasks.CreatedEvent(task, actorType, actorID)
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const insertTask = `INSERT INTO tasks
		(id, source_provider, source_ref, status, repository, agent_profile, branch_name, attempt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insertTask, task.ID, task.SourceProvider, task.SourceRef,
		string(task.Status), task.Repository, task.AgentProfile, task.BranchName,
		task.Attempt, formatTime(task.CreatedAt), formatTime(task.UpdatedAt)); err != nil {
		return tasks.Task{}, fmt.Errorf("storage: insert task: %w", err)
	}
	if err := insertEvent(tx, event); err != nil {
		return tasks.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, fmt.Errorf("storage: commit: %w", err)
	}
	return task, nil
}

// GetTask returns one task by id or ErrNotFound.
func (s *Store) GetTask(id string) (tasks.Task, error) {
	task, err := scanTask(s.db.QueryRow(taskColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return tasks.Task{}, ErrNotFound
	}
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: get task: %w", err)
	}
	return task, nil
}

// ListTasks returns all tasks in creation order.
func (s *Store) ListTasks() ([]tasks.Task, error) {
	rows, err := s.db.Query(taskColumns + ` ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("storage: list tasks: %w", err)
	}
	defer rows.Close()
	out := []tasks.Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan task: %w", err)
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list tasks: %w", err)
	}
	return out, nil
}

// Transition validates the jump, updates the status, and appends the
// task.transition event in one transaction. Illegal jumps fail with the
// state-machine error and change nothing.
func (s *Store) Transition(id string, to tasks.State, actorType, actorID string) (tasks.Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Event{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := getTaskTx(tx, id)
	if err != nil {
		return tasks.Event{}, err
	}
	event, err := tasks.ApplyTransition(&task, to, orDefault(actorType, "controller"), orDefault(actorID, "cli"))
	if err != nil {
		return tasks.Event{}, err
	}
	if _, err := tx.Exec("UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?",
		string(task.Status), formatTime(task.UpdatedAt), task.ID); err != nil {
		return tasks.Event{}, fmt.Errorf("storage: update status: %w", err)
	}
	if err := insertEvent(tx, event); err != nil {
		return tasks.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Event{}, fmt.Errorf("storage: commit: %w", err)
	}
	return event, nil
}

// AppendEvent records a custom event (agent output, validation results,
// delivery notes) against an existing task.
func (s *Store) AppendEvent(taskID, eventType, actorType, actorID, payload string) (tasks.Event, error) {
	event := tasks.Event{
		TaskID:    taskID,
		Type:      eventType,
		ActorType: orDefault(actorType, "controller"),
		ActorID:   orDefault(actorID, "cli"),
		Payload:   orDefault(payload, "{}"),
		CreatedAt: time.Now().UTC(),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Event{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := getTaskTx(tx, taskID); err != nil {
		return tasks.Event{}, err
	}
	if err := insertEvent(tx, event); err != nil {
		return tasks.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Event{}, fmt.Errorf("storage: commit: %w", err)
	}
	return event, nil
}

// ListEvents returns a task's full history in append order.
func (s *Store) ListEvents(taskID string) ([]tasks.Event, error) {
	const query = `SELECT id, task_id, event_type, actor_type, actor_id, payload_json, created_at
		FROM task_events WHERE task_id = ? ORDER BY id`
	rows, err := s.db.Query(query, taskID)
	if err != nil {
		return nil, fmt.Errorf("storage: list events: %w", err)
	}
	defer rows.Close()
	var out []tasks.Event
	for rows.Next() {
		var event tasks.Event
		var createdAt string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Type,
			&event.ActorType, &event.ActorID, &event.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		var perr error
		if event.CreatedAt, perr = parseTime(createdAt); perr != nil {
			return nil, fmt.Errorf("storage: parse event time: %w", perr)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list events: %w", err)
	}
	return out, nil
}

// getTaskTx loads a task inside a transaction or returns ErrNotFound.
func getTaskTx(tx *sql.Tx, id string) (tasks.Task, error) {
	task, err := scanTask(tx.QueryRow(taskColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return tasks.Task{}, ErrNotFound
	}
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: get task: %w", err)
	}
	return task, nil
}

// taskColumns lists the tasks columns in scanTask order.
const taskColumns = `SELECT id, source_provider, source_ref, status, repository,
	agent_profile, branch_name, attempt, created_at, updated_at FROM tasks`

// rowScanner abstracts *sql.Row, *sql.Rows, and *sql.Tx row results.
type rowScanner interface{ Scan(dest ...any) error }

// scanTask scans one taskColumns row and parses its timestamps.
// Purpose: one schema change touches one place instead of three.
// Inputs: a row in taskColumns order. Returns the task, or sql.ErrNoRows
// unwrapped so callers map it to ErrNotFound.
func scanTask(row rowScanner) (tasks.Task, error) {
	var task tasks.Task
	var status, createdAt, updatedAt string
	if err := row.Scan(&task.ID, &task.SourceProvider, &task.SourceRef,
		&status, &task.Repository, &task.AgentProfile, &task.BranchName,
		&task.Attempt, &createdAt, &updatedAt); err != nil {
		return tasks.Task{}, err
	}
	task.Status = tasks.State(status)
	var perr error
	if task.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return tasks.Task{}, fmt.Errorf("storage: parse created_at: %w", perr)
	}
	if task.UpdatedAt, perr = parseTime(updatedAt); perr != nil {
		return tasks.Task{}, fmt.Errorf("storage: parse updated_at: %w", perr)
	}
	return task, nil
}

// insertEvent appends one event row inside the caller's transaction.
// Purpose: single choke point so every mutation path records history the
// same way. Inputs: open tx, fully populated event. Flow: INSERT, map
// foreign-key violations to ErrNotFound. Returns wrapped errors only.
func insertEvent(tx *sql.Tx, event tasks.Event) error {
	const insert = `INSERT INTO task_events
		(task_id, event_type, actor_type, actor_id, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insert, event.TaskID, event.Type, event.ActorType,
		event.ActorID, event.Payload, formatTime(event.CreatedAt)); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return ErrNotFound
		}
		return fmt.Errorf("storage: insert event: %w", err)
	}
	return nil
}

// expandPath resolves a leading ~/ against $HOME.
func expandPath(path string) string {
	if after, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, after)
		}
	}
	return path
}

// formatTime stores timestamps in UTC RFC3339Nano for stable round-trips.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime reads back formatTime output.
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// orDefault returns s unless blank, for actor/payload attribution.
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
