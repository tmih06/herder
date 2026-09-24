package machine

import (
	"context"
	"strings"
	"testing"
)

// scriptRunner records every herdr invocation and answers from a canned
// script keyed on the joined argv; unmatched calls succeed empty.
type scriptRunner struct {
	calls   [][]string
	respond func(args []string) (RunResult, error)
}

func (s *scriptRunner) run(ctx context.Context, name string, args ...string) (RunResult, error) {
	s.calls = append(s.calls, append([]string{name}, args...))
	if s.respond != nil {
		return s.respond(args)
	}
	return RunResult{}, nil
}

func (s *scriptRunner) count(verb string) int {
	n := 0
	for _, c := range s.calls {
		if len(c) >= 3 && c[0] == "herdr" && c[1] == "machine" && c[2] == verb {
			n++
		}
	}
	return n
}

// TestEnsureSkipsAddWhenProfileExists proves re-provisioning is
// idempotent: `machine add` appends duplicates, so an existing profile
// for the label must be returned from the first list alone.
func TestEnsureSkipsAddWhenProfileExists(t *testing.T) {
	s := &scriptRunner{respond: func(args []string) (RunResult, error) {
		if strings.Join(args, " ") == "machine list --json" {
			return RunResult{Stdout: `[{"id":"m1","label":"task_abc123","target":"herder-task_abc123","enabled":true}]`}, nil
		}
		return RunResult{}, nil
	}}
	r := &Registry{Runner: s.run}
	m, err := r.Ensure(context.Background(), "task_abc123", "herder-task_abc123")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if m == nil || m.ID != "m1" {
		t.Errorf("Ensure = %+v, want the existing profile m1", m)
	}
	if s.count("add") != 0 {
		t.Errorf("existing profile must skip machine add, ran %v", s.calls)
	}
	if s.count("list") != 1 {
		t.Errorf("existing profile needs exactly one list, ran %v", s.calls)
	}
}

// TestEnsureAddsThenRelists proves the absent path: `machine add
// <target> --label <label>` runs, then a fresh list resolves the id
// herdr minted — ids are never derived client-side.
func TestEnsureAddsThenRelists(t *testing.T) {
	var added bool
	s := &scriptRunner{respond: func(args []string) (RunResult, error) {
		switch strings.Join(args, " ") {
		case "machine list --json":
			if !added {
				return RunResult{Stdout: "[]"}, nil
			}
			return RunResult{Stdout: `[{"id":"m9","label":"task_abc123","target":"herder-task_abc123","enabled":true}]`}, nil
		case "machine add herder-task_abc123 --label task_abc123":
			added = true
			return RunResult{}, nil
		}
		return RunResult{}, nil
	}}
	r := &Registry{Runner: s.run}
	m, err := r.Ensure(context.Background(), "task_abc123", "herder-task_abc123")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !added {
		t.Fatalf("absent profile must run machine add, ran %v", s.calls)
	}
	if m == nil || m.ID != "m9" {
		t.Errorf("Ensure = %+v, want the minted profile m9", m)
	}
	if s.count("list") != 2 {
		t.Errorf("add path must list before and after, ran %v", s.calls)
	}
}

// TestEnsureAddFailureIsReal proves a failed add is not swallowed: the
// worker cannot be driven without its profile.
func TestEnsureAddFailureIsReal(t *testing.T) {
	s := &scriptRunner{respond: func(args []string) (RunResult, error) {
		switch strings.Join(args, " ") {
		case "machine list --json":
			return RunResult{Stdout: "[]"}, nil
		case "machine add herder-task_abc123 --label task_abc123":
			return RunResult{ExitCode: 1, Stderr: "ssh: connect failed"}, nil
		}
		return RunResult{}, nil
	}}
	r := &Registry{Runner: s.run}
	if _, err := r.Ensure(context.Background(), "task_abc123", "herder-task_abc123"); err == nil {
		t.Error("failed machine add must propagate an error")
	}
}

// TestRemoveDeletesMatchingProfiles proves teardown removes every
// profile matching the label OR the target — by profile id, since
// `machine remove` does not accept labels — and leaves others alone.
func TestRemoveDeletesMatchingProfiles(t *testing.T) {
	s := &scriptRunner{respond: func(args []string) (RunResult, error) {
		if strings.Join(args, " ") == "machine list --json" {
			return RunResult{Stdout: `[{"id":"m1","label":"task_abc123","target":"other-target","enabled":true},` +
				`{"id":"m2","label":"stale","target":"herder-task_abc123","enabled":true},` +
				`{"id":"m3","label":"task_other","target":"herder-task_other","enabled":true}]`}, nil
		}
		return RunResult{}, nil
	}}
	r := &Registry{Runner: s.run}
	if err := r.Remove(context.Background(), "task_abc123", "herder-task_abc123"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var removed []string
	for _, c := range s.calls {
		if len(c) == 4 && c[0] == "herdr" && c[1] == "machine" && c[2] == "remove" {
			removed = append(removed, c[3])
		}
	}
	if strings.Join(removed, ",") != "m1,m2" {
		t.Errorf("removed ids = %v, want [m1 m2]: label and target matches only", removed)
	}
}

// TestRemoveToleratesMissing proves a destroyed worker with no saved
// profile is a no-op, not an error.
func TestRemoveToleratesMissing(t *testing.T) {
	s := &scriptRunner{respond: func(args []string) (RunResult, error) {
		return RunResult{Stdout: "[]"}, nil
	}}
	r := &Registry{Runner: s.run}
	if err := r.Remove(context.Background(), "task_gone", "herder-task_gone"); err != nil {
		t.Errorf("Remove with no matching profile must be a no-op, got %v", err)
	}
	if s.count("remove") != 0 {
		t.Errorf("no match means no machine remove, ran %v", s.calls)
	}
}
