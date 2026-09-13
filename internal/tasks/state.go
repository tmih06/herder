// Package tasks owns the Herder task state machine and the structured
// events every accepted transition mints (SPEC sections 14, 37, 47).
//
// Why: SQLite is the source of truth but the terminal is not, so illegal
// jumps must be rejected before persistence and every accepted jump must
// leave a timestamped, attributable trace for audit and crash recovery.
// Approach: a pure transition table (no I/O) plus small pure helpers;
// storage applies them transactionally. Task and Event mirror the
// tasks/task_events tables in internal/storage.
// Inputs: states from the SPEC machine, actor type/id for attribution.
// Flow: ValidateTransition -> ApplyTransition mints EventTransition.
// Returns: updated task + event, or an error naming both states and the
// legal targets.
package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Primary lifecycle states (SPEC section 14).
const (
	Discovered   State = "DISCOVERED"
	Eligible     State = "ELIGIBLE"
	Claimed      State = "CLAIMED"
	Queued       State = "QUEUED"
	Provisioning State = "PROVISIONING"
	Running      State = "RUNNING"
	Validating   State = "VALIDATING"
	Reviewing    State = "REVIEWING"
	Delivering   State = "DELIVERING"
	PROpen       State = "PR_OPEN"
	Done         State = "DONE"
)

// Alternative states (SPEC section 14).
const (
	Blocked         State = "BLOCKED"
	WaitingForHuman State = "WAITING_FOR_HUMAN"
	Failed          State = "FAILED"
	Cancelled       State = "CANCELLED"
	TimedOut        State = "TIMED_OUT"
	Retrying        State = "RETRYING"
	Paused          State = "PAUSED"
)

// Event types minted by this package.
const (
	// EventCreated marks task creation.
	EventCreated = "task.created"
	// EventTransition marks an accepted state transition.
	EventTransition = "task.transition"
)

// State is a task lifecycle state.
type State string

// allowed maps each state to its legal successors. The primary chain
// walks forward; alternative states model failure, human intervention,
// and retry without ever skipping the audit trail.
var allowed = map[State][]State{
	Discovered:      {Eligible, Cancelled, Failed},
	Eligible:        {Claimed, Cancelled, Failed},
	Claimed:         {Queued, Cancelled, Failed},
	Queued:          {Provisioning, Paused, Cancelled, Failed},
	Provisioning:    {Running, Retrying, Cancelled, Failed},
	Running:         {Validating, Blocked, WaitingForHuman, Paused, TimedOut, Cancelled, Failed},
	Validating:      {Reviewing, Retrying, Cancelled, Failed},
	Reviewing:       {Delivering, WaitingForHuman, Retrying, Cancelled, Failed},
	Delivering:      {PROpen, Retrying, Cancelled, Failed},
	PROpen:          {Done, Cancelled, Failed},
	Blocked:         {Running, Cancelled, Failed},
	WaitingForHuman: {Running, Reviewing, Cancelled, Failed},
	Paused:          {Queued, Running, Cancelled},
	Retrying:        {Queued, Provisioning, Running, Cancelled, Failed},
	Failed:          {Retrying, Cancelled},
	Done:            {},
	Cancelled:       {},
	TimedOut:        {Retrying, Cancelled},
}

// Task is the in-memory form of one row in tasks.
type Task struct {
	ID             string
	SourceProvider string
	SourceRef      string
	Status         State
	Repository     string
	AgentProfile   string
	BranchName     string
	Attempt        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Event is the in-memory form of one row in task_events.
type Event struct {
	ID        int64
	TaskID    string
	Type      string
	ActorType string
	ActorID   string
	Payload   string
	CreatedAt time.Time
}

// New builds a DISCOVERED task with fresh identity and timestamps.
func New(sourceProvider, sourceRef, repository, agentProfile string) Task {
	now := time.Now().UTC()
	return Task{
		ID:             "task_" + hexID(8),
		SourceProvider: sourceProvider,
		SourceRef:      sourceRef,
		Status:         Discovered,
		Repository:     repository,
		AgentProfile:   agentProfile,
		Attempt:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// ValidateTransition reports whether from -> to is legal. The error names
// both states and the legal successors so operators can fix the caller.
func ValidateTransition(from, to State) error {
	targets, ok := allowed[from]
	if !ok {
		return fmt.Errorf("tasks: unknown state %q", string(from))
	}
	for _, next := range targets {
		if next == to {
			return nil
		}
	}
	if _, ok := allowed[to]; !ok {
		return fmt.Errorf("tasks: unknown state %q", string(to))
	}
	names := make([]string, 0, len(targets))
	for _, next := range targets {
		names = append(names, string(next))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("tasks: illegal transition %s -> %s: %s is terminal",
			from, to, from)
	}
	return fmt.Errorf("tasks: illegal transition %s -> %s: %s allows [%s]",
		from, to, from, strings.Join(names, ", "))
}

// ApplyTransition moves task to the next state and mints the matching
// structured event. On illegal jumps it returns the error and leaves the
// task untouched so callers can safely retry.
func ApplyTransition(task *Task, to State, actorType, actorID string) (Event, error) {
	from := task.Status
	if err := ValidateTransition(from, to); err != nil {
		return Event{}, err
	}
	now := time.Now().UTC()
	task.Status = to
	task.UpdatedAt = now
	return Event{
		TaskID:    task.ID,
		Type:      EventTransition,
		ActorType: actorType,
		ActorID:   actorID,
		Payload:   fmt.Sprintf(`{"from":%q,"to":%q}`, string(from), string(to)),
		CreatedAt: now,
	}, nil
}

// CreatedEvent mints the task.created event for a fresh task.
// Purpose: every task starts life with an auditable creation record.
// Inputs: the new task (carries ID, source, timestamp), actor attribution.
// Returns an Event stamped at the task's creation time.
func CreatedEvent(task Task, actorType, actorID string) Event {
	return Event{
		TaskID:    task.ID,
		Type:      EventCreated,
		ActorType: actorType,
		ActorID:   actorID,
		Payload: fmt.Sprintf(`{"source_ref":%q,"repository":%q,"agent_profile":%q}`,
			task.SourceRef, task.Repository, task.AgentProfile),
		CreatedAt: task.CreatedAt,
	}
}

// hexID returns n random bytes as hex for task identity.
// Purpose: unpredictable task IDs without a central allocator.
// Inputs: byte count (capped by the 16-byte buffer). Flow: crypto/rand,
// falling back to nanotime on (practically impossible) failure.
func hexID(n int) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:n]); err != nil {
		return strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	}
	return hex.EncodeToString(buf[:n])
}
