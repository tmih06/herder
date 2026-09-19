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
// log that mirrors SPEC sections 46-47, the webhook_deliveries log that
// makes every trigger delivery an inspectable durable decision (SPEC
// sections 11, 49: dedup by delivery id, one task per issue), and the
// leases table that gives each dispatch an expiring owner (SPEC section
// 50: heartbeats keep a live lease, expiry returns the task to the queue).
const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	source_provider TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	goal TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	repository TEXT NOT NULL,
	agent_profile TEXT NOT NULL,
	branch_name TEXT NOT NULL DEFAULT '',
	agent_session_id TEXT NOT NULL DEFAULT '',
	sandbox_id TEXT NOT NULL DEFAULT '',
	agent_state TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	priority INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_source ON tasks(source_provider, source_ref);
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
CREATE TABLE IF NOT EXISTS webhook_deliveries (
	delivery_id TEXT PRIMARY KEY,
	source_provider TEXT NOT NULL DEFAULT '',
	source_ref TEXT NOT NULL DEFAULT '',
	repository TEXT NOT NULL DEFAULT '',
	decision TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS leases (
	task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
	owner TEXT NOT NULL,
	acquired_at TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
`

// CreateInput carries the fields needed to open a new task.
type CreateInput struct {
	SourceProvider string
	SourceRef      string
	Repository     string
	AgentProfile   string
	BranchName     string
	Goal           string
	// Priority orders the dispatch queue (zero is normal).
	Priority  int
	ActorType string
	ActorID   string
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
	// Single writer: SQLite takes database-level write locks, so a
	// pool of writers just meets SQLITE_BUSY. One connection serializes
	// claims in the driver, keeps every PRAGMA on the same handle, and
	// leaves the UNIQUE constraints as the cross-process safety net.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("storage: %s: %w", pragma, err)
		}
	}
	// journal_mode bypasses busy_timeout and fails instantly with
	// SQLITE_BUSY under a sibling's transient lock, so it gets its own
	// retry: the CLI shares one SQLite file with the daemon by design
	// (e.g. doctor racing a dying daemon), and a momentarily locked
	// file must be waited out, not fatal.
	if err := setWALMode(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate schema: %w", err)
	}
	if err := migrateColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: expanded}, nil
}

// migrateColumns adds columns introduced after the first databases were
// written: the task-sandbox-session link columns plus goal, which carries
// the issue goal text seeded into the agent prompt (issue #4), agent_state,
// the normalized Herdr-reported worker condition (issue #5), and the
// scheduler columns priority and started_at (issue #7). Fresh databases
// already carry the columns via schema; legacy files get one ALTER each,
// and the duplicate-column error on a partially migrated file is the
// success signal, not a failure.
func migrateColumns(db *sql.DB) error {
	for _, column := range []string{
		"agent_session_id TEXT NOT NULL DEFAULT ''",
		"sandbox_id TEXT NOT NULL DEFAULT ''",
		"goal TEXT NOT NULL DEFAULT ''",
		"agent_state TEXT NOT NULL DEFAULT ''",
		"priority INTEGER NOT NULL DEFAULT 0",
		"started_at TEXT NOT NULL DEFAULT ''",
	} {
		_, err := db.Exec("ALTER TABLE tasks ADD COLUMN " + column)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("storage: migrate columns: %w", err)
		}
	}
	return nil
}

// walRetryBudget bounds how long setWALMode waits out a sibling's lock.
const walRetryBudget = 5 * time.Second

// walRetryInterval spaces journal_mode attempts while locked.
const walRetryInterval = 50 * time.Millisecond

// setWALMode switches the database to WAL durability, retrying through a
// sibling process's transient lock. Purpose: PRAGMA journal_mode ignores
// busy_timeout, so without a retry any CLI racing the daemon's shutdown
// or checkpoint fails instantly with SQLITE_BUSY. The pragma is
// idempotent, making retries safe. Returns a wrapped error past budget.
func setWALMode(db *sql.DB) error {
	deadline := time.Now().Add(walRetryBudget)
	for {
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err == nil {
			return nil
		} else if !isBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("storage: PRAGMA journal_mode=WAL: %w", err)
		}
		time.Sleep(walRetryInterval)
	}
}

// isBusy reports transient SQLite lock errors worth waiting out.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
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
	task := tasks.New(tasks.NewInput{
		SourceProvider: in.SourceProvider,
		SourceRef:      in.SourceRef,
		Repository:     in.Repository,
		AgentProfile:   in.AgentProfile,
		Goal:           in.Goal,
		Priority:       in.Priority,
	})
	task.BranchName = in.BranchName
	actorType, actorID := orDefault(in.ActorType, "controller"), orDefault(in.ActorID, "cli")
	event := tasks.CreatedEvent(task, actorType, actorID)
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := insertTaskTx(tx, task, ""); err != nil {
		return tasks.Task{}, err
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
// task.transition event in one transaction. Entering RUNNING also stamps
// started_at so the agent timeout measures each attempt's wall clock from
// dispatch, not from task creation. Illegal jumps fail with the
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
	if to == tasks.Running {
		task.StartedAt = task.UpdatedAt
	}
	if _, err := tx.Exec("UPDATE tasks SET status = ?, started_at = ?, updated_at = ? WHERE id = ?",
		string(task.Status), formatStartedAt(task.StartedAt), formatTime(task.UpdatedAt), task.ID); err != nil {
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

// SetBinding records the durable task <-> sandbox <-> session link (SPEC
// section 18) without touching task status: sandbox provision stores the
// container id, agent start stores the Herdr session name. Empty values
// leave the stored column unchanged so callers update only what they know.
// Unknown ids fail with ErrNotFound; updated_at moves so crash recovery
// can tell a fresh link from a stale one.
func (s *Store) SetBinding(id, sandboxID, sessionID string) error {
	task, err := s.GetTask(id)
	if err != nil {
		return err
	}
	if sandboxID != "" {
		task.SandboxID = sandboxID
	}
	if sessionID != "" {
		task.AgentSessionID = sessionID
	}
	task.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(`UPDATE tasks SET sandbox_id = ?, agent_session_id = ?,
		updated_at = ? WHERE id = ?`,
		task.SandboxID, task.AgentSessionID, formatTime(task.UpdatedAt), id)
	if err != nil {
		return fmt.Errorf("storage: set binding: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearSessionBinding erases a stale agent session link after a failed
// launch so crash recovery does not resurrect a dead session name.
// SetBinding cannot express this: its empty-means-unchanged rule exists
// for partial updates, not clears. Unknown ids fail with ErrNotFound;
// updated_at moves so recovery can tell a fresh clear from a stale link.
func (s *Store) ClearSessionBinding(id string) error {
	if _, err := s.GetTask(id); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE tasks SET agent_session_id = '', updated_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("storage: clear session binding: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementAttempt bumps the task's attempt counter and returns the
// updated row. Purpose: a validation failure routed back to the agent is
// a new attempt on the same sandbox work (issue #6); the counter makes
// retries visible in inspect output and event history. Unknown ids fail
// with ErrNotFound; updated_at moves so recovery sees the touch.
func (s *Store) IncrementAttempt(id string) (tasks.Task, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET attempt = attempt + 1, updated_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), id)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: increment attempt %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return tasks.Task{}, ErrNotFound
	}
	return s.GetTask(id)
}

// RecordAgentState stores the normalized Herdr-reported agent state on the
// owning task and appends agent.state_changed when it moved (SPEC
// sections 19, 58). Same-state reports are a no-op so the supervision
// loop stays quiet between changes; the first observation always mints
// the event. Unknown ids fail with ErrNotFound.
func (s *Store) RecordAgentState(id, state, actorType, actorID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := getTaskTx(tx, id)
	if err != nil {
		return false, err
	}
	if task.AgentState == state {
		return false, nil
	}
	now := time.Now().UTC()
	if _, err := tx.Exec("UPDATE tasks SET agent_state = ?, updated_at = ? WHERE id = ?",
		state, formatTime(now), id); err != nil {
		return false, fmt.Errorf("storage: record agent state: %w", err)
	}
	if err := insertEvent(tx, tasks.Event{
		TaskID: id, Type: tasks.EventAgentStateChanged,
		ActorType: orDefault(actorType, "controller"), ActorID: orDefault(actorID, "supervisor"),
		Payload:   tasks.EventPayload(map[string]string{"from": task.AgentState, "to": state}),
		CreatedAt: now,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("storage: commit: %w", err)
	}
	return true, nil
}

// Retry begins a fresh attempt on one task: RETRYING (when the current
// state can retry), attempt counter incremented, dead session link and
// agent state cleared, and a task.retry event — all atomically. QUEUED
// and RETRYING tasks skip the transition but still count the attempt.
// Terminal or non-retryable states fail with the state-machine error and
// change nothing. Returns the updated task for the relaunch flow.
func (s *Store) Retry(id, actorType, actorID string) (tasks.Task, error) {
	return s.restartAttempt(id, "", tasks.EventRetry, actorType, actorID)
}

// Handoff moves one task to a different agent profile on a fresh attempt:
// same atomic RETRYING + attempt increment as Retry, plus the profile
// swap and an agent.handed_off event naming both profiles so the worker
// change stays traceable (SPEC section 22). An empty or unchanged profile
// still restarts the attempt; the event records what happened.
func (s *Store) Handoff(id, newProfile, actorType, actorID string) (tasks.Task, error) {
	return s.restartAttempt(id, newProfile, tasks.EventHandoff, actorType, actorID)
}

// restartAttempt runs the shared retry/handoff transaction: legal states
// move to RETRYING, the attempt counter increments, the dead session link
// and stale agent state clear (the caller stops the old session first),
// and the typed event lands in the same commit. Inputs: task id, the
// replacement profile (empty keeps the current one), the event type, and
// actor attribution. Returns the updated task.
func (s *Store) restartAttempt(id, newProfile, eventType, actorType, actorID string) (tasks.Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := getTaskTx(tx, id)
	if err != nil {
		return tasks.Task{}, err
	}
	now := time.Now().UTC()
	if task.Status != tasks.Queued && task.Status != tasks.Retrying {
		event, err := tasks.ApplyTransition(&task, tasks.Retrying, orDefault(actorType, "controller"), orDefault(actorID, "cli"))
		if err != nil {
			return tasks.Task{}, err
		}
		if err := insertEvent(tx, event); err != nil {
			return tasks.Task{}, err
		}
	}
	task.Attempt++
	task.AgentSessionID = ""
	task.AgentState = ""
	task.UpdatedAt = now
	payload := tasks.EventPayload(map[string]any{"attempt": task.Attempt})
	if newProfile != "" {
		payload = tasks.EventPayload(map[string]any{
			"attempt":      task.Attempt,
			"from_profile": task.AgentProfile, "to_profile": newProfile,
		})
		task.AgentProfile = newProfile
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, agent_profile = ?, agent_session_id = '',
		agent_state = '', attempt = ?, updated_at = ? WHERE id = ?`,
		string(task.Status), task.AgentProfile, task.Attempt, formatTime(task.UpdatedAt), id); err != nil {
		return tasks.Task{}, fmt.Errorf("storage: restart attempt: %w", err)
	}
	if err := insertEvent(tx, tasks.Event{
		TaskID: id, Type: eventType,
		ActorType: orDefault(actorType, "controller"), ActorID: orDefault(actorID, "cli"),
		Payload: payload, CreatedAt: now,
	}); err != nil {
		return tasks.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, fmt.Errorf("storage: commit: %w", err)
	}
	return task, nil
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

// Delivery decisions recorded in webhook_deliveries (SPEC sections 11, 49).
const (
	// DecisionAccepted means the delivery created the task.
	DecisionAccepted = "accepted"
	// DecisionDuplicate means the delivery repeated an already-recorded
	// delivery id or an already-claimed issue; no new task was made.
	DecisionDuplicate = "duplicate"
	// DecisionPolicyDenied means policy refused the work; no task was made.
	DecisionPolicyDenied = "policy_denied"
)

// ErrDeliveryNotFound is returned when a delivery id was never recorded.
var ErrDeliveryNotFound = errors.New("storage: delivery not found")

// Delivery is one webhook receipt and its durable decision: which task it
// created, or why it became a logged non-event instead of a second worker.
type Delivery struct {
	DeliveryID     string
	SourceProvider string
	SourceRef      string
	Repository     string
	Decision       string
	Reason         string
	TaskID         string
	CreatedAt      time.Time
}

// ClaimRequest carries everything needed to atomically deduplicate,
// policy-check, and claim one trigger delivery (issue #2).
type ClaimRequest struct {
	DeliveryID     string
	SourceProvider string
	SourceRef      string
	Repository     string
	AgentProfile   string
	BranchName     string
	// Goal carries the issue goal text seeded into the agent prompt.
	Goal string
	// Priority orders the dispatch queue (zero is normal).
	Priority int
	// PolicyPayload is stored on the policy.decision event of an
	// accepted task so inspect shows the policy outcome end to end.
	PolicyPayload string
	ActorType     string
	ActorID       string
}

// ClaimOutcome is the durable decision for one delivery.
type ClaimOutcome struct {
	Decision string
	Reason   string
	Task     tasks.Task
}

// Claim deduplicates one trigger delivery and, when it is new, creates
// exactly one task already walked to QUEUED with its full event timeline.
// Every path records the delivery row, so redeliveries and denials stay
// inspectable instead of silent. Concurrency caps live in the scheduler
// (issue #7): a burst queues, it is never denied here.
// Why: GitHub redelivers webhooks and operators relabel issues; without
// one atomic check-then-claim, concurrent duplicates open a double-claim
// window with two workers on one issue (SPEC sections 49, 66.8).
// Approach: one transaction per claim. UNIQUE(delivery_id) plus
// UNIQUE(source_provider, source_ref) turn a lost race into a re-read
// that returns duplicate instead of a second task; ON CONFLICT DO NOTHING
// keeps that re-read free of fragile error-string matching.
// Inputs: validated delivery identity, source, agent profile, branch.
// Flow: known delivery -> duplicate; known source -> duplicate + log on
// the surviving task; else insert task, created +
// DISCOVERED->ELIGIBLE->CLAIMED->QUEUED + policy.decision events, and the
// accepted delivery row, atomically.
// Returns: the durable decision with the new or surviving task.
func (s *Store) Claim(req ClaimRequest) (ClaimOutcome, error) {
	if strings.TrimSpace(req.DeliveryID) == "" || strings.TrimSpace(req.SourceProvider) == "" ||
		strings.TrimSpace(req.SourceRef) == "" || strings.TrimSpace(req.Repository) == "" ||
		strings.TrimSpace(req.AgentProfile) == "" {
		return ClaimOutcome{}, errors.New("storage: claim needs delivery, source, repository, and agent profile")
	}
	out, err := s.claimOnce(req)
	if err != nil && isConflict(err) {
		// A sibling process (CLI beside the daemon) won the race between
		// our check and our insert. Re-read under the new state and report
		// duplicate instead of a constraint error. Same-process claims
		// never reach here: MaxOpenConns(1) serializes them.
		return s.claimOnce(req)
	}
	return out, err
}

// claimOnce runs one check-then-claim transaction; see Claim.
func (s *Store) claimOnce(req ClaimRequest) (ClaimOutcome, error) {
	actorType, actorID := orDefault(req.ActorType, "controller"), orDefault(req.ActorID, "webhook")
	tx, err := s.db.Begin()
	if err != nil {
		return ClaimOutcome{}, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, err := getDeliveryTx(tx, req.DeliveryID); err == nil {
		reason := fmt.Sprintf("delivery %q already recorded as %s", req.DeliveryID, existing.Decision)
		outcome := ClaimOutcome{Decision: DecisionDuplicate, Reason: reason}
		if existing.TaskID != "" {
			if task, terr := getTaskTx(tx, existing.TaskID); terr == nil {
				outcome.Task = task
				if err := insertEvent(tx, tasks.Event{
					TaskID: task.ID, Type: tasks.EventWebhookDuplicate,
					ActorType: actorType, ActorID: actorID,
					Payload:   tasks.EventPayload(map[string]string{"delivery_id": req.DeliveryID, "reason": reason}),
					CreatedAt: time.Now().UTC(),
				}); err != nil {
					return ClaimOutcome{}, err
				}
				if err := tx.Commit(); err != nil {
					return ClaimOutcome{}, fmt.Errorf("storage: commit: %w", err)
				}
			}
		}
		return outcome, nil
	}
	if survivor, err := getTaskBySourceTx(tx, req.SourceProvider, req.SourceRef); err == nil {
		reason := fmt.Sprintf("issue %s:%s already claimed by %s", req.SourceProvider, req.SourceRef, survivor.ID)
		if err := recordDuplicateTx(tx, req, survivor, reason, actorType, actorID); err != nil {
			return ClaimOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return ClaimOutcome{}, fmt.Errorf("storage: commit: %w", err)
		}
		return ClaimOutcome{Decision: DecisionDuplicate, Reason: reason, Task: survivor}, nil
	}

	task := tasks.New(tasks.NewInput{
		SourceProvider: req.SourceProvider,
		SourceRef:      req.SourceRef,
		Repository:     req.Repository,
		AgentProfile:   req.AgentProfile,
		Goal:           req.Goal,
		Priority:       req.Priority,
	})
	task.BranchName = req.BranchName
	res, err := insertTaskTx(tx, task, ` ON CONFLICT(source_provider, source_ref) DO NOTHING`)
	if err != nil {
		return ClaimOutcome{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		survivor, serr := getTaskBySourceTx(tx, req.SourceProvider, req.SourceRef)
		if serr != nil {
			return ClaimOutcome{}, fmt.Errorf("storage: lost claim race with no winner: %w", serr)
		}
		reason := fmt.Sprintf("issue %s:%s claimed concurrently by %s", req.SourceProvider, req.SourceRef, survivor.ID)
		if err := recordDuplicateTx(tx, req, survivor, reason, actorType, actorID); err != nil {
			return ClaimOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return ClaimOutcome{}, fmt.Errorf("storage: commit: %w", err)
		}
		return ClaimOutcome{Decision: DecisionDuplicate, Reason: reason, Task: survivor}, nil
	}
	if err := insertEvent(tx, tasks.CreatedEvent(task, actorType, actorID)); err != nil {
		return ClaimOutcome{}, err
	}
	for _, to := range []tasks.State{tasks.Eligible, tasks.Claimed, tasks.Queued} {
		event, err := tasks.ApplyTransition(&task, to, actorType, actorID)
		if err != nil {
			return ClaimOutcome{}, err
		}
		if _, err := tx.Exec("UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?",
			string(task.Status), formatTime(task.UpdatedAt), task.ID); err != nil {
			return ClaimOutcome{}, fmt.Errorf("storage: update status: %w", err)
		}
		if err := insertEvent(tx, event); err != nil {
			return ClaimOutcome{}, err
		}
	}
	if err := insertEvent(tx, tasks.Event{
		TaskID: task.ID, Type: tasks.EventPolicyDecision,
		ActorType: actorType, ActorID: actorID,
		Payload:   orDefault(req.PolicyPayload, "{}"),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return ClaimOutcome{}, err
	}
	if err := insertDeliveryTx(tx, Delivery{
		DeliveryID: req.DeliveryID, SourceProvider: req.SourceProvider,
		SourceRef: req.SourceRef, Repository: req.Repository,
		Decision: DecisionAccepted, Reason: "policy accepted; task claimed and queued",
		TaskID: task.ID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return ClaimOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return ClaimOutcome{}, fmt.Errorf("storage: commit: %w", err)
	}
	return ClaimOutcome{Decision: DecisionAccepted, Reason: "policy accepted; task claimed and queued", Task: task}, nil
}

// isConflict reports SQLite uniqueness violations from a lost cross-process
// race: a UNIQUE delivery id or a UNIQUE source already taken by a sibling.
// Purpose: lets Claim retry once as a re-read instead of surfacing a
// constraint error for what is really a duplicate delivery.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY constraint") ||
		strings.Contains(msg, "constraint failed")
}

// RecordDenied durably logs a delivery refused before claiming: unknown or
// disabled repository, or a label outside the trigger set. It creates no
// task. Redelivering the same delivery id returns the first decision so
// the log stays stable and callers report duplicate.
// Inputs: delivery identity plus the human-readable policy reason.
// Returns: the recorded (or already-recorded) delivery plus whether this
// call created the row, so callers can tell a fresh denial from a
// redelivery. A lost cross-process race re-reads instead of failing.
func (s *Store) RecordDenied(deliveryID, provider, sourceRef, repository, reason string) (denied Delivery, created bool, err error) {
	if strings.TrimSpace(deliveryID) == "" || strings.TrimSpace(reason) == "" {
		return Delivery{}, false, errors.New("storage: denied delivery needs an id and a reason")
	}
	denied = Delivery{
		DeliveryID: deliveryID, SourceProvider: provider,
		SourceRef: sourceRef, Repository: repository,
		Decision: DecisionPolicyDenied, Reason: reason,
		CreatedAt: time.Now().UTC(),
	}
	// ON CONFLICT DO NOTHING plus the re-read inside recordDeniedOnce
	// already turn a lost race into the rival row; no retry needed.
	got, created, err := recordDeniedOnce(s, denied)
	return got, created, err
}

// recordDeniedOnce inserts one denial unless the delivery id is taken.
// Purpose: one attempt of RecordDenied. Returns created=false with a nil
// error when the row already exists, so the caller can report duplicate.
func recordDeniedOnce(s *Store, denied Delivery) (Delivery, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Delivery{}, false, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, err := getDeliveryTx(tx, denied.DeliveryID); err == nil {
		return existing, false, nil
	}
	const insert = `INSERT INTO webhook_deliveries
		(delivery_id, source_provider, source_ref, repository, decision, reason, task_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(delivery_id) DO NOTHING`
	res, err := tx.Exec(insert, denied.DeliveryID, denied.SourceProvider, denied.SourceRef,
		denied.Repository, denied.Decision, denied.Reason, denied.TaskID, formatTime(denied.CreatedAt))
	if err != nil {
		return Delivery{}, false, fmt.Errorf("storage: insert delivery: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		existing, rerr := getDeliveryTx(tx, denied.DeliveryID)
		if rerr != nil {
			return Delivery{}, false, fmt.Errorf("storage: re-read denied delivery: %w", rerr)
		}
		_ = tx.Rollback()
		return existing, false, nil
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, false, fmt.Errorf("storage: commit: %w", err)
	}
	return denied, true, nil
}

// GetDelivery returns one recorded delivery or ErrDeliveryNotFound.
func (s *Store) GetDelivery(id string) (Delivery, error) {
	var d Delivery
	var createdAt string
	err := s.db.QueryRow(deliveryColumns+` WHERE delivery_id = ?`, id).Scan(
		&d.DeliveryID, &d.SourceProvider, &d.SourceRef, &d.Repository,
		&d.Decision, &d.Reason, &d.TaskID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrDeliveryNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("storage: get delivery: %w", err)
	}
	var perr error
	if d.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return Delivery{}, fmt.Errorf("storage: parse delivery time: %w", perr)
	}
	return d, nil
}

// ListDeliveries returns every recorded delivery in receipt order.
// Purpose: the inspectable non-event log behind `herder ingest log` and
// GET /v1/deliveries. Returns oldest first, never nil.
func (s *Store) ListDeliveries() ([]Delivery, error) {
	rows, err := s.db.Query(deliveryColumns + ` ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("storage: list deliveries: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var createdAt string
		if err := rows.Scan(&d.DeliveryID, &d.SourceProvider, &d.SourceRef,
			&d.Repository, &d.Decision, &d.Reason, &d.TaskID, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan delivery: %w", err)
		}
		var perr error
		if d.CreatedAt, perr = parseTime(createdAt); perr != nil {
			return nil, fmt.Errorf("storage: parse delivery time: %w", perr)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list deliveries: %w", err)
	}
	return out, nil
}

// FindTaskBySource returns the task claimed for one provider issue, or
// ErrNotFound when the issue was never claimed.
func (s *Store) FindTaskBySource(provider, ref string) (tasks.Task, error) {
	task, err := scanTask(s.db.QueryRow(taskColumns+` WHERE source_provider = ? AND source_ref = ?`, provider, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return tasks.Task{}, ErrNotFound
	}
	if err != nil {
		return tasks.Task{}, fmt.Errorf("storage: find task by source: %w", err)
	}
	return task, nil
}

// ErrLeaseHeld is returned when a live lease belongs to another owner.
var ErrLeaseHeld = errors.New("storage: lease held")

// ErrLeaseLost is returned when a heartbeat names a lease the owner no
// longer holds: expired, released, or never acquired.
var ErrLeaseLost = errors.New("storage: lease lost")

// Lease is one task's dispatch lease: the owner holding it, when it was
// taken, the last heartbeat, and when it lapses (SPEC section 50).
type Lease struct {
	TaskID      string
	Owner       string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

// leaseColumns lists the leases columns in scanLease order.
const leaseColumns = `SELECT task_id, owner, acquired_at, heartbeat_at, expires_at FROM leases`

// AcquireLease takes or renews the dispatch lease on one task atomically:
// the upsert lands only when no lease exists, the stored lease has
// expired, or the caller already owns it (renew), and the lease.acquired
// event commits in the same transaction so the grant is always
// attributable. A live lease under another owner fails with ErrLeaseHeld
// and changes nothing; an unknown task fails with ErrNotFound.
// Inputs: task id, the scheduler owner identity, and the lease TTL.
// Returns wrapped errors only.
func (s *Store) AcquireLease(taskID, owner string, ttl time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := getTaskTx(tx, taskID); err != nil {
		return err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	res, err := tx.Exec(`INSERT INTO leases (task_id, owner, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET owner = excluded.owner,
			acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at,
			expires_at = excluded.expires_at
		WHERE leases.expires_at <= ? OR leases.owner = excluded.owner`,
		taskID, owner, formatTime(now), formatTime(now), formatTime(expires), formatTime(now))
	if err != nil {
		return fmt.Errorf("storage: acquire lease: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrLeaseHeld
	}
	if err := insertEvent(tx, tasks.Event{
		TaskID: taskID, Type: tasks.EventLeaseAcquired,
		ActorType: "controller", ActorID: owner,
		Payload: tasks.EventPayload(map[string]string{
			"owner": owner, "task_id": taskID, "expires_at": formatTime(expires),
		}),
		CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// HeartbeatLease refreshes one owned lease's heartbeat and expiry.
// Purpose: a live scheduler proves it still holds the dispatch so the
// lease never lapses mid-run (SPEC section 50). The UPDATE is guarded by
// owner and a still-valid expiry, so zero rows affected means the lease
// is gone or stolen and the caller must stop: ErrLeaseLost.
// Inputs: task id, the claiming owner, and the TTL to re-arm.
// Returns ErrLeaseLost on zero rows, wrapped errors otherwise.
func (s *Store) HeartbeatLease(taskID, owner string, ttl time.Duration) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE leases SET heartbeat_at = ?, expires_at = ?
		WHERE task_id = ? AND owner = ? AND expires_at > ?`,
		formatTime(now), formatTime(now.Add(ttl)), taskID, owner, formatTime(now))
	if err != nil {
		return fmt.Errorf("storage: heartbeat lease: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ReleaseLease drops one owned lease and mints lease.released in the same
// transaction when a row was actually deleted. Releasing a lease that is
// absent (or held by someone else) is a no-op so completion paths stay
// idempotent after a crash.
// Inputs: task id and the releasing owner. Returns wrapped errors only.
func (s *Store) ReleaseLease(taskID, owner string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM leases WHERE task_id = ? AND owner = ?`, taskID, owner)
	if err != nil {
		return fmt.Errorf("storage: release lease: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		if err := insertEvent(tx, tasks.Event{
			TaskID: taskID, Type: tasks.EventLeaseReleased,
			ActorType: "controller", ActorID: owner,
			Payload: tasks.EventPayload(map[string]string{
				"owner": owner, "task_id": taskID,
			}),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// ExpireLeases deletes every lease whose expiry has passed and returns
// the evicted task_id -> owner map. No events are minted here: the
// scheduler inspects each task's state and decides whether it requeues,
// so the event belongs to the caller's recovery path.
// Inputs: the cutoff time. Returns the evicted owners by task id.
func (s *Store) ExpireLeases(now time.Time) (map[string]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT task_id, owner FROM leases WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("storage: list expired leases: %w", err)
	}
	expired := map[string]string{}
	for rows.Next() {
		var taskID, owner string
		if err := rows.Scan(&taskID, &owner); err != nil {
			rows.Close()
			return nil, fmt.Errorf("storage: scan expired lease: %w", err)
		}
		expired[taskID] = owner
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("storage: list expired leases: %w", err)
	}
	rows.Close()
	if _, err := tx.Exec(`DELETE FROM leases WHERE expires_at <= ?`, formatTime(now)); err != nil {
		return nil, fmt.Errorf("storage: expire leases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("storage: commit: %w", err)
	}
	return expired, nil
}

// GetLease returns one task's lease or ErrNotFound when none is held.
func (s *Store) GetLease(taskID string) (Lease, error) {
	lease, err := scanLease(s.db.QueryRow(leaseColumns+` WHERE task_id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, fmt.Errorf("storage: get lease: %w", err)
	}
	return lease, nil
}

// scanLease scans one leaseColumns row and parses its timestamps.
// Purpose: one schema change touches one place. Inputs: a row in
// leaseColumns order. Returns the lease, or sql.ErrNoRows unwrapped so
// callers map it to ErrNotFound.
func scanLease(row rowScanner) (Lease, error) {
	var lease Lease
	var acquiredAt, heartbeatAt, expiresAt string
	if err := row.Scan(&lease.TaskID, &lease.Owner, &acquiredAt, &heartbeatAt, &expiresAt); err != nil {
		return Lease{}, err
	}
	var perr error
	if lease.AcquiredAt, perr = parseTime(acquiredAt); perr != nil {
		return Lease{}, fmt.Errorf("storage: parse lease acquired_at: %w", perr)
	}
	if lease.HeartbeatAt, perr = parseTime(heartbeatAt); perr != nil {
		return Lease{}, fmt.Errorf("storage: parse lease heartbeat_at: %w", perr)
	}
	if lease.ExpiresAt, perr = parseTime(expiresAt); perr != nil {
		return Lease{}, fmt.Errorf("storage: parse lease expires_at: %w", perr)
	}
	return lease, nil
}

// deliveryColumns lists webhook_deliveries columns in scan order.
const deliveryColumns = `SELECT delivery_id, source_provider, source_ref, repository,
	decision, reason, task_id, created_at FROM webhook_deliveries`

// getDeliveryTx loads a delivery inside a transaction.
// Purpose: dedup reads share the claim transaction so the decision and
// the task insert commit or roll back together.
// Returns sql.ErrNoRows (unmapped) when the delivery is new.
func getDeliveryTx(tx *sql.Tx, id string) (Delivery, error) {
	var d Delivery
	var createdAt string
	if err := tx.QueryRow(deliveryColumns+` WHERE delivery_id = ?`, id).Scan(
		&d.DeliveryID, &d.SourceProvider, &d.SourceRef, &d.Repository,
		&d.Decision, &d.Reason, &d.TaskID, &createdAt); err != nil {
		return Delivery{}, err
	}
	var perr error
	if d.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return Delivery{}, fmt.Errorf("storage: parse delivery time: %w", perr)
	}
	return d, nil
}

// getTaskBySourceTx loads a task by provider issue inside a transaction.
// Purpose: the already-claimed check inside the claim transaction.
// Returns sql.ErrNoRows (unmapped) when the issue is unclaimed.
func getTaskBySourceTx(tx *sql.Tx, provider, ref string) (tasks.Task, error) {
	task, err := scanTask(tx.QueryRow(taskColumns+` WHERE source_provider = ? AND source_ref = ?`, provider, ref))
	if err != nil {
		return tasks.Task{}, err
	}
	return task, nil
}

// insertTaskTx inserts one task row inside the caller's transaction.
// Purpose: CreateTask and claimOnce share one column list and argument
// order so a schema change touches one place; conflictSuffix carries
// claimOnce's ON CONFLICT clause (empty for CreateTask).
// Inputs: open tx, the task to persist, and a leading-space conflict
// suffix appended to the INSERT. Returns the Exec result so callers can
// inspect RowsAffected, plus wrapped errors only.
func insertTaskTx(tx *sql.Tx, task tasks.Task, conflictSuffix string) (sql.Result, error) {
	const insert = `INSERT INTO tasks
		(id, source_provider, source_ref, goal, status, repository, agent_profile, branch_name, agent_session_id, sandbox_id, priority, started_at, attempt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?)`
	res, err := tx.Exec(insert+conflictSuffix, task.ID, task.SourceProvider, task.SourceRef,
		task.Goal, string(task.Status), task.Repository, task.AgentProfile, task.BranchName,
		task.Priority, formatStartedAt(task.StartedAt), task.Attempt,
		formatTime(task.CreatedAt), formatTime(task.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("storage: insert task: %w", err)
	}
	return res, nil
}

// insertDeliveryTx records one delivery row inside the caller's transaction.
// Purpose: single choke point so claim, duplicate, and denial paths log
// the same way. Returns wrapped errors only.
func insertDeliveryTx(tx *sql.Tx, d Delivery) error {
	const insert = `INSERT INTO webhook_deliveries
		(delivery_id, source_provider, source_ref, repository, decision, reason, task_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insert, d.DeliveryID, d.SourceProvider, d.SourceRef,
		d.Repository, d.Decision, d.Reason, d.TaskID, formatTime(d.CreatedAt)); err != nil {
		return fmt.Errorf("storage: insert delivery: %w", err)
	}
	return nil
}

// recordDuplicateTx logs one deduplicated delivery inside the claim
// transaction: the duplicate delivery row plus a webhook.duplicate event
// on the surviving task, so repeats stay visible instead of silent.
// Inputs: open claim tx, the losing request, the surviving task, the
// human-readable reason, actor attribution. Returns wrapped errors only.
func recordDuplicateTx(tx *sql.Tx, req ClaimRequest, survivor tasks.Task, reason, actorType, actorID string) error {
	if err := insertDeliveryTx(tx, Delivery{
		DeliveryID: req.DeliveryID, SourceProvider: req.SourceProvider,
		SourceRef: req.SourceRef, Repository: req.Repository,
		Decision: DecisionDuplicate, Reason: reason, TaskID: survivor.ID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	return insertEvent(tx, tasks.Event{
		TaskID: survivor.ID, Type: tasks.EventWebhookDuplicate,
		ActorType: actorType, ActorID: actorID,
		Payload:   tasks.EventPayload(map[string]string{"delivery_id": req.DeliveryID, "reason": reason}),
		CreatedAt: time.Now().UTC(),
	})
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
const taskColumns = `SELECT id, source_provider, source_ref, goal, status, repository,
	agent_profile, branch_name, agent_session_id, sandbox_id, agent_state, priority, started_at, attempt, created_at, updated_at FROM tasks`

// rowScanner abstracts *sql.Row, *sql.Rows, and *sql.Tx row results.
type rowScanner interface{ Scan(dest ...any) error }

// scanTask scans one taskColumns row and parses its timestamps.
// Purpose: one schema change touches one place instead of three.
// Inputs: a row in taskColumns order. Returns the task, or sql.ErrNoRows
// unwrapped so callers map it to ErrNotFound.
func scanTask(row rowScanner) (tasks.Task, error) {
	var task tasks.Task
	var status, startedAt, createdAt, updatedAt string
	if err := row.Scan(&task.ID, &task.SourceProvider, &task.SourceRef,
		&task.Goal, &status, &task.Repository, &task.AgentProfile, &task.BranchName,
		&task.AgentSessionID, &task.SandboxID, &task.AgentState,
		&task.Priority, &startedAt, &task.Attempt, &createdAt, &updatedAt); err != nil {
		return tasks.Task{}, err
	}
	task.Status = tasks.State(status)
	var perr error
	if task.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return tasks.Task{}, fmt.Errorf("storage: parse created_at: %w", perr)
	}
	if startedAt != "" {
		if task.StartedAt, perr = parseTime(startedAt); perr != nil {
			return tasks.Task{}, fmt.Errorf("storage: parse started_at: %w", perr)
		}
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

// formatStartedAt stores the RUNNING-entry stamp, keeping the empty
// string for a task that has never run so the column default round-trips.
func formatStartedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// parseTime reads back formatTime output.
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// orDefault returns s unless blank, for actor/payload attribution.
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
