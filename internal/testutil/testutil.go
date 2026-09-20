// Package testutil holds the test scaffolding shared across internal
// packages: a Recorder that scripts subprocess results by argv for the
// docker, herdr, and gh runner seams, a temp-dir store opener, and
// event-listing helpers.
package testutil

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tmih06/herder/internal/agent"
	"github.com/tmih06/herder/internal/sandbox"
	"github.com/tmih06/herder/internal/storage"
)

// Recorder scripts subprocess results by argv and records every call;
// one script feeds the docker, herdr, and gh runners alike. The mutex
// makes it safe under concurrent dispatchOne goroutines.
type Recorder struct {
	mu      sync.Mutex
	calls   [][]string
	Respond func(name string, args []string) (sandbox.RunResult, error)
}

// Run records the call and answers from Respond; unmatched calls
// succeed empty.
func (r *Recorder) Run(_ context.Context, name string, args ...string) (sandbox.RunResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.Respond != nil {
		return r.Respond(name, args)
	}
	return sandbox.RunResult{}, nil
}

// DockerRun adapts the script to the sandbox.Runner seam.
func (r *Recorder) DockerRun(ctx context.Context, name string, args ...string) (sandbox.RunResult, error) {
	return r.Run(ctx, name, args...)
}

// HerdrRun adapts the script to the agent.Runner seam.
func (r *Recorder) HerdrRun(ctx context.Context, name string, args ...string) (agent.RunResult, error) {
	out, err := r.Run(ctx, name, args...)
	return agent.RunResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, err
}

// Calls returns a copy of the recorded argv log so tests can assert
// call order without racing a running dispatch.
func (r *Recorder) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// Called reports whether any recorded argv contains every fragment.
func (r *Recorder) Called(fragments ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if matchAll(call, fragments) {
			return true
		}
	}
	return false
}

// CountCalls reports how many recorded argv contain every fragment.
func (r *Recorder) CountCalls(fragments ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if matchAll(call, fragments) {
			n++
		}
	}
	return n
}

// matchAll reports whether the joined argv contains every fragment.
func matchAll(call []string, fragments []string) bool {
	joined := strings.Join(call, " ")
	for _, f := range fragments {
		if !strings.Contains(joined, f) {
			return false
		}
	}
	return true
}

// OpenStore opens a real store in a temp dir so lease, transition, and
// event behavior is exercised against SQLite, not a stub.
func OpenStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "herder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// EventTypes lists a task's recorded event types in order.
func EventTypes(t *testing.T, store *storage.Store, taskID string) []string {
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

// HasEvent reports whether the type list contains want.
func HasEvent(types []string, want string) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}
