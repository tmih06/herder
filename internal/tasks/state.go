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
	"encoding/json"
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

// Normalized agent states (SPEC section 19): the Herdr-reported worker
// condition recorded on the owning task. AgentStarting is minted by Herder
// at launch, before Herdr's first detection report lands.
const (
	AgentStarting = "starting"
	AgentWorking  = "working"
	AgentIdle     = "idle"
	AgentBlocked  = "blocked"
	AgentDone     = "done"
	AgentUnknown  = "unknown"
)

// Event types minted by this package.
const (
	// EventCreated marks task creation.
	EventCreated = "task.created"
	// EventTransition marks an accepted state transition.
	EventTransition = "task.transition"
	// EventPolicyDecision records the ingest policy outcome on a task.
	EventPolicyDecision = "policy.decision"
	// EventWebhookDuplicate records a deduplicated redelivery on the
	// surviving task so repeats stay visible instead of silent.
	EventWebhookDuplicate = "webhook.duplicate"
	// EventAgentStateChanged records one normalized agent-state change on
	// the owning task (SPEC sections 19, 58: agent.state_changed).
	EventAgentStateChanged = "agent.state_changed"
	// EventRetry records a fresh attempt with the incremented counter.
	EventRetry = "task.retry"
	// EventHandoff records a worker change that preserves task history and
	// sandbox work (SPEC section 22: handoff).
	EventHandoff = "agent.handed_off"
	// EventAgentBlocked records a blocked worker with its reason.
	EventAgentBlocked = "agent.blocked"
	// EventAgentExited records a bound session that stopped answering.
	EventAgentExited = "agent.exited"
	// EventAgentPrompted records a human message sent to the live worker.
	EventAgentPrompted = "agent.prompted"
	// EventAgentStopped records a human-driven session close.
	EventAgentStopped = "agent.stopped"
	// EventLeaseAcquired records a dispatch lease granted to an owner.
	EventLeaseAcquired = "lease.acquired"
	// EventLeaseReleased records a dispatch lease returned on completion.
	EventLeaseReleased = "lease.released"
	// EventLeaseExpired records a dead owner's lease returning the task to
	// the queue (SPEC section 50: expired leases requeue, never strand).
	EventLeaseExpired = "lease.expired"
	// EventDispatchFailed records a dispatch attempt that failed before the
	// agent launched; the task returns to QUEUED for a later attempt.
	EventDispatchFailed = "task.dispatch_failed"
	// EventWorkerDisconnected marks a worker whose session or sandbox is
	// gone at reconcile time (SPEC section 48: recovery policy).
	EventWorkerDisconnected = "worker.disconnected"
	// EventTaskTimedOut records a worker stopped for exceeding its
	// configured agent timeout (SPEC section 15: time limits enforced).
	EventTaskTimedOut = "task.timed_out"
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
	Provisioning:    {Running, Queued, Retrying, Cancelled, Failed},
	Running:         {Validating, Blocked, WaitingForHuman, Paused, Retrying, TimedOut, Cancelled, Failed},
	Validating:      {Reviewing, Running, WaitingForHuman, Retrying, Cancelled, Failed},
	Reviewing:       {Delivering, Validating, Done, WaitingForHuman, Retrying, Cancelled, Failed},
	Delivering:      {PROpen, Validating, Reviewing, Retrying, Cancelled, Failed},
	PROpen:          {Done, Cancelled, Failed},
	Blocked:         {Running, WaitingForHuman, Retrying, TimedOut, Cancelled, Failed},
	WaitingForHuman: {Running, Validating, Reviewing, Retrying, Cancelled, Failed},
	Paused:          {Queued, Running, Retrying, Cancelled},
	Retrying:        {Queued, Provisioning, Running, Cancelled, Failed},
	Failed:          {Retrying, Cancelled},
	Done:            {},
	Cancelled:       {},
	TimedOut:        {Retrying, Cancelled},
}

// Task is the in-memory form of one row in tasks. Goal carries the issue
// goal text seeded into the agent prompt. AgentSessionID names the
// live Herdr agent session on the worker's machine (empty until the
// agent launches) and SandboxID names the worker container; MachineID
// is the saved herdr SSH profile driving that container, and
// RemoteWorkspaceID/RemotePaneID name the workspace and pane the agent
// occupies on the container-local herdr server — together they are the
// durable task ↔ sandbox ↔ machine ↔ session link (issue #19). AgentState
// is the normalized Herdr-reported worker condition (empty until the
// supervision loop observes the session).
type Task struct {
	ID                string
	SourceProvider    string
	SourceRef         string
	Goal              string
	Status            State
	Repository        string
	AgentProfile      string
	BranchName        string
	AgentSessionID    string
	SandboxID         string
	MachineID         string
	RemoteWorkspaceID string
	RemotePaneID      string
	AgentState        string
	// Priority orders the dispatch queue: higher runs first, ties break on
	// CreatedAt (FIFO-plus-priority, SPEC section 15).
	Priority int
	// StartedAt stamps the latest entry into RUNNING: the agent timeout
	// measures wall-clock work from here, not from task creation.
	StartedAt time.Time
	Attempt   int
	CreatedAt time.Time
	UpdatedAt time.Time
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

// NewInput carries the fields New needs to seed a task: source identity,
// repository, agent profile, and the issue goal text seeded into the
// agent prompt. Priority orders the dispatch queue (zero is normal).
type NewInput struct {
	SourceProvider string
	SourceRef      string
	Repository     string
	AgentProfile   string
	Goal           string
	Priority       int
}

// New builds a DISCOVERED task with fresh identity and timestamps.
// Inputs: a NewInput carrying source identity, repository, agent
// profile, and the issue goal text seeded into the agent prompt.
func New(in NewInput) Task {
	now := time.Now().UTC()
	return Task{
		ID:             "task_" + hexID(8),
		SourceProvider: in.SourceProvider,
		SourceRef:      in.SourceRef,
		Goal:           in.Goal,
		Status:         Discovered,
		Repository:     in.Repository,
		AgentProfile:   in.AgentProfile,
		Priority:       in.Priority,
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
		Payload:   EventPayload(map[string]string{"from": string(from), "to": string(to)}),
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
		Payload: EventPayload(map[string]string{
			"source_ref": task.SourceRef,
			"repository": task.Repository, "agent_profile": task.AgentProfile,
		}),
		CreatedAt: task.CreatedAt,
	}
}

// EventPayload renders an event payload as real JSON: fmt %q quoting is
// Go syntax, not JSON, and can store payloads that fail to parse.
// Returns "{}" only when the value itself cannot marshal.
func EventPayload(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
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
