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
// log that mirrors SPEC sections 46-47, plus the webhook_deliveries log
// that makes every trigger delivery an inspectable durable decision
// (SPEC sections 11, 49: dedup by delivery id, one task per issue).
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
	// Single writer: SQLite takes database-level write locks, so a
	// pool of writers just meets SQLITE_BUSY. One connection serializes
	// claims in the driver, keeps every PRAGMA on the same handle, and
	// leaves the UNIQUE constraints as the cross-process safety net.
	db.SetMaxOpenConns(1)
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
	// MaxActive caps tasks in non-terminal states; the count and the
	// insert share one transaction so the cap cannot be raced.
	MaxActive int
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

// terminalStatusClause lists the states that free worker capacity, as a
// SQL IN-list fragment for the active-task count.
const terminalStatusClause = `('DONE','CANCELLED','FAILED')`

// Claim deduplicates one trigger delivery and, when it is new and policy
// allows, creates exactly one task already walked to QUEUED with its full
// event timeline. Every path records the delivery row, so redeliveries and
// denials stay inspectable instead of silent.
// Why: GitHub redelivers webhooks and operators relabel issues; without
// one atomic check-then-claim, concurrent duplicates open a double-claim
// window with two workers on one issue (SPEC sections 49, 66.8).
// Approach: one transaction per claim. UNIQUE(delivery_id) plus
// UNIQUE(source_provider, source_ref) turn a lost race into a re-read
// that returns duplicate instead of a second task; ON CONFLICT DO NOTHING
// keeps that re-read free of fragile error-string matching. The active
// count and the insert share the transaction so the cap holds.
// Inputs: validated delivery identity, source, agent profile, branch, cap.
// Flow: known delivery -> duplicate; known source -> duplicate + log on
// the surviving task; cap breached -> policy_denied; else insert task,
// created + DISCOVERED->ELIGIBLE->CLAIMED->QUEUED + policy.decision
// events, and the accepted delivery row, atomically.
// Returns: the durable decision with the new or surviving task.
func (s *Store) Claim(req ClaimRequest) (ClaimOutcome, error) {
	if strings.TrimSpace(req.DeliveryID) == "" || strings.TrimSpace(req.SourceProvider) == "" ||
		strings.TrimSpace(req.SourceRef) == "" || strings.TrimSpace(req.Repository) == "" ||
		strings.TrimSpace(req.AgentProfile) == "" {
		return ClaimOutcome{}, fmt.Errorf("storage: claim needs delivery, source, repository, and agent profile")
	}
	if req.MaxActive < 1 {
		return ClaimOutcome{}, fmt.Errorf("storage: claim needs MaxActive >= 1, got %d", req.MaxActive)
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
					Payload:   fmt.Sprintf(`{"delivery_id":%q,"reason":%q}`, req.DeliveryID, reason),
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
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status NOT IN ` + terminalStatusClause).Scan(&active); err != nil {
		return ClaimOutcome{}, fmt.Errorf("storage: count active: %w", err)
	}
	if active >= req.MaxActive {
		reason := fmt.Sprintf("concurrency cap reached: %d active tasks of %d allowed", active, req.MaxActive)
		if err := insertDeliveryTx(tx, Delivery{
			DeliveryID: req.DeliveryID, SourceProvider: req.SourceProvider,
			SourceRef: req.SourceRef, Repository: req.Repository,
			Decision: DecisionPolicyDenied, Reason: reason,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return ClaimOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return ClaimOutcome{}, fmt.Errorf("storage: commit: %w", err)
		}
		return ClaimOutcome{Decision: DecisionPolicyDenied, Reason: reason}, nil
	}
	task := tasks.New(req.SourceProvider, req.SourceRef, req.Repository, req.AgentProfile)
	task.BranchName = req.BranchName
	const insertTask = `INSERT INTO tasks
		(id, source_provider, source_ref, status, repository, agent_profile, branch_name, attempt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_provider, source_ref) DO NOTHING`
	res, err := tx.Exec(insertTask, task.ID, task.SourceProvider, task.SourceRef,
		string(task.Status), task.Repository, task.AgentProfile, task.BranchName,
		task.Attempt, formatTime(task.CreatedAt), formatTime(task.UpdatedAt))
	if err != nil {
		return ClaimOutcome{}, fmt.Errorf("storage: insert task: %w", err)
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
		return Delivery{}, false, fmt.Errorf("storage: denied delivery needs an id and a reason")
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

// CountActive reports tasks in non-terminal states holding worker capacity.
// Purpose: policy input for the concurrency cap and a diagnostic signal.
// Returns the count of tasks outside DONE/CANCELLED/FAILED.
func (s *Store) CountActive() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status NOT IN ` + terminalStatusClause).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count active: %w", err)
	}
	return n, nil
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
		Payload:   fmt.Sprintf(`{"delivery_id":%q,"reason":%q}`, req.DeliveryID, reason),
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
