package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmih06/herder/internal/tasks"
)

// TestAcquireLeaseMintsEvent proves a first acquire writes the row and a
// lease.acquired event naming owner, task, and expiry in one commit.
func TestAcquireLeaseMintsEvent(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}

	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	lease, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}
	if lease.Owner != "sched-1" || lease.TaskID != task.ID {
		t.Errorf("lease identity wrong: %+v", lease)
	}
	if lease.AcquiredAt.IsZero() || lease.HeartbeatAt.IsZero() {
		t.Errorf("lease timestamps missing: %+v", lease)
	}
	if !lease.ExpiresAt.After(lease.AcquiredAt) {
		t.Errorf("expires_at %v must follow acquired_at %v", lease.ExpiresAt, lease.AcquiredAt)
	}

	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	last := events[len(events)-1]
	if last.Type != tasks.EventLeaseAcquired {
		t.Fatalf("last event = %q, want %q", last.Type, tasks.EventLeaseAcquired)
	}
	for _, key := range []string{"owner", "task_id", "expires_at"} {
		if !strings.Contains(last.Payload, `"`+key+`"`) {
			t.Errorf("lease.acquired payload missing %q: %s", key, last.Payload)
		}
	}
}

// TestAcquireLeaseHeldByOther proves a live lease blocks a second owner
// with ErrLeaseHeld and leaves the row untouched.
func TestAcquireLeaseHeldByOther(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}

	err = store.AcquireLease(task.ID, "sched-2", time.Minute)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second owner acquire = %v, want ErrLeaseHeld", err)
	}
	lease, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}
	if lease.Owner != "sched-1" {
		t.Errorf("owner changed under ErrLeaseHeld: %+v", lease)
	}
}

// TestAcquireLeaseRenewsForSameOwner proves the holder can re-acquire:
// the row updates and a fresh lease.acquired event lands.
func TestAcquireLeaseRenewsForSameOwner(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", 2*time.Minute); err != nil {
		t.Fatalf("renew = %v", err)
	}
	lease, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}
	if lease.Owner != "sched-1" {
		t.Errorf("owner = %q, want sched-1", lease.Owner)
	}
	if !lease.ExpiresAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("renew did not extend expiry: %v", lease.ExpiresAt)
	}
	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	acquired := 0
	for _, e := range events {
		if e.Type == tasks.EventLeaseAcquired {
			acquired++
		}
	}
	if acquired != 2 {
		t.Errorf("want 2 lease.acquired events, got %d", acquired)
	}
}

// TestAcquireLeaseExpiredIsTakeable proves an expired lease passes to a
// new owner instead of stranding the task (SPEC section 50).
func TestAcquireLeaseExpiredIsTakeable(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	// A non-positive TTL lands already expired.
	if err := store.AcquireLease(task.ID, "sched-1", -time.Second); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-2", time.Minute); err != nil {
		t.Fatalf("acquire over expired lease = %v", err)
	}
	lease, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}
	if lease.Owner != "sched-2" {
		t.Errorf("owner = %q, want sched-2", lease.Owner)
	}
}

// TestAcquireLeaseUnknownTask proves leases cannot attach to nothing.
func TestAcquireLeaseUnknownTask(t *testing.T) {
	store, _ := openTestStore(t)
	if err := store.AcquireLease("task_missing", "sched-1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AcquireLease(unknown) = %v, want ErrNotFound", err)
	}
}

// TestHeartbeatLease proves the holder extends expiry while a wrong
// owner, an expired lease, and a missing lease all report ErrLeaseLost.
func TestHeartbeatLease(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	before, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}

	if err := store.HeartbeatLease(task.ID, "sched-1", 2*time.Minute); err != nil {
		t.Fatalf("HeartbeatLease = %v", err)
	}
	after, err := store.GetLease(task.ID)
	if err != nil {
		t.Fatalf("GetLease = %v", err)
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("heartbeat did not extend expiry: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}
	if !after.HeartbeatAt.After(before.HeartbeatAt) && !after.HeartbeatAt.Equal(before.HeartbeatAt) {
		t.Errorf("heartbeat_at moved backwards: %v -> %v", before.HeartbeatAt, after.HeartbeatAt)
	}

	if err := store.HeartbeatLease(task.ID, "sched-2", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("foreign heartbeat = %v, want ErrLeaseLost", err)
	}
	if err := store.HeartbeatLease("task_missing", "sched-1", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("missing heartbeat = %v, want ErrLeaseLost", err)
	}
}

// TestHeartbeatLeaseExpired proves a lapsed lease cannot be revived by
// its own owner: expiry means the scheduler must re-acquire.
func TestHeartbeatLeaseExpired(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", -time.Second); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	if err := store.HeartbeatLease(task.ID, "sched-1", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired heartbeat = %v, want ErrLeaseLost", err)
	}
}

// TestReleaseLease proves release deletes the row, mints lease.released,
// and repeats are silent no-ops.
func TestReleaseLease(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}

	// A different owner cannot release the lease.
	if err := store.ReleaseLease(task.ID, "sched-2"); err != nil {
		t.Fatalf("foreign ReleaseLease = %v", err)
	}
	if _, err := store.GetLease(task.ID); err != nil {
		t.Fatalf("foreign release deleted the lease: %v", err)
	}

	if err := store.ReleaseLease(task.ID, "sched-1"); err != nil {
		t.Fatalf("ReleaseLease = %v", err)
	}
	if _, err := store.GetLease(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLease after release = %v, want ErrNotFound", err)
	}
	events, err := store.ListEvents(task.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	last := events[len(events)-1]
	if last.Type != tasks.EventLeaseReleased {
		t.Fatalf("last event = %q, want %q", last.Type, tasks.EventLeaseReleased)
	}

	// Releasing an absent lease is a no-op: no error, no second event.
	if err := store.ReleaseLease(task.ID, "sched-1"); err != nil {
		t.Fatalf("re-ReleaseLease = %v", err)
	}
	events, err = store.ListEvents(task.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	released := 0
	for _, e := range events {
		if e.Type == tasks.EventLeaseReleased {
			released++
		}
	}
	if released != 1 {
		t.Errorf("want 1 lease.released event, got %d", released)
	}
}

// TestExpireLeases proves only lapsed leases are deleted and the evicted
// task_id -> owner map comes back without minting events.
func TestExpireLeases(t *testing.T) {
	store, _ := openTestStore(t)
	live, err := store.CreateTask(createInputWithRef("acme/web#1"))
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	dead, err := store.CreateTask(createInputWithRef("acme/web#2"))
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(live.ID, "sched-live", time.Minute); err != nil {
		t.Fatalf("AcquireLease live = %v", err)
	}
	if err := store.AcquireLease(dead.ID, "sched-dead", -time.Second); err != nil {
		t.Fatalf("AcquireLease dead = %v", err)
	}

	expired, err := store.ExpireLeases(time.Now().UTC())
	if err != nil {
		t.Fatalf("ExpireLeases = %v", err)
	}
	if len(expired) != 1 || expired[dead.ID] != "sched-dead" {
		t.Fatalf("expired map = %v, want {%s: sched-dead}", expired, dead.ID)
	}
	if _, err := store.GetLease(dead.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired lease still present: %v", err)
	}
	lease, err := store.GetLease(live.ID)
	if err != nil {
		t.Fatalf("live lease deleted: %v", err)
	}
	if lease.Owner != "sched-live" {
		t.Errorf("live owner = %q, want sched-live", lease.Owner)
	}

	// Expiry mints no events: the scheduler decides per task state.
	events, err := store.ListEvents(dead.ID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	for _, e := range events {
		if e.Type == tasks.EventLeaseExpired {
			t.Errorf("ExpireLeases must not mint events, found %q", e.Type)
		}
	}
}

// TestLeaseCascadeOnTaskDelete proves the FK keeps leases from
// outliving their task: deleting the task row drops the lease.
func TestLeaseCascadeOnTaskDelete(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if err := store.AcquireLease(task.ID, "sched-1", time.Minute); err != nil {
		t.Fatalf("AcquireLease = %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("delete task = %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE task_id = ?`, task.ID).Scan(&n); err != nil {
		t.Fatalf("count leases = %v", err)
	}
	if n != 0 {
		t.Errorf("lease survived its task: %d rows", n)
	}
}

// TestGetLeaseMissing proves an unleased task reports ErrNotFound.
func TestGetLeaseMissing(t *testing.T) {
	store, _ := openTestStore(t)
	task, err := store.CreateTask(createInput())
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if _, err := store.GetLease(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLease(unleased) = %v, want ErrNotFound", err)
	}
}
