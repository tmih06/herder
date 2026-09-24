package dispatch

import (
	"context"
	"sort"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/deliver"
	"github.com/tmih06/herder/internal/tasks"
)

// AdvanceIssueLabels swaps the issue's stage labels for the next stage:
// every configured stage or trigger label currently on the issue leaves,
// the new one arrives. Label failures are delivery failures — the label
// path is part of the contract.
func (d *Dispatcher) AdvanceIssueLabels(ctx context.Context, task *tasks.Task,
	repo config.RepositoryConfig, next string,
) error {
	issue, ok := deliver.IssueNumber(task.SourceRef)
	if !ok || repo.IsLocal() {
		// No forge behind a local repository: there is no issue to label.
		return nil
	}
	current, err := d.engine().IssueLabels(ctx, task.Repository, issue)
	if err != nil {
		return err
	}
	remove := staleLabels(current, repo, next)
	if err := d.engine().SetLabels(ctx, task.Repository, issue, []string{next}, remove); err != nil {
		return err
	}
	return d.emitEvent(task.ID, "issue.labels_updated",
		map[string]any{"issue": issue, "added": next, "removed": remove})
}

// staleLabels computes which labels to remove when advancing to next:
// the trigger labels plus every stage label currently on the issue,
// except the one being applied. Sorted for stable events and argv.
func staleLabels(current []string, repo config.RepositoryConfig, next string) []string {
	stages := repo.Delivery.LabelSet()
	managed := map[string]bool{next: true}
	for _, l := range repo.Trigger.Labels {
		managed[l] = true
	}
	for _, l := range []string{stages.Running, stages.Review, stages.Completed} {
		managed[l] = true
	}
	var remove []string
	for _, l := range current {
		if managed[l] && l != next {
			remove = append(remove, l)
		}
	}
	sort.Strings(remove)
	return remove
}

// MarkIssueRunning applies the running stage label when a task's agent
// starts. Best-effort: a missing gh or unreachable GitHub warns via
// Warnf but never blocks a launch.
func (d *Dispatcher) MarkIssueRunning(task *tasks.Task, repo config.RepositoryConfig) {
	if _, ok := deliver.IssueNumber(task.SourceRef); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.AdvanceIssueLabels(ctx, task, repo,
		repo.Delivery.LabelSet().Running); err != nil {
		d.warnf("herder: warning: could not mark %s running: %v", task.SourceRef, err)
	}
}
