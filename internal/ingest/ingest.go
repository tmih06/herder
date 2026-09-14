// Package ingest turns labeled-issue webhook deliveries into exactly one
// policy-approved task (issue #2; SPEC sections 11, 12, 36, 49).
//
// Why: GitHub redelivers webhooks and operators relabel issues, so every
// delivery must converge on one claimed task or one logged non-event
// instead of a second worker.
// Approach: static policy from config (repository known and enabled,
// trigger label present), then one atomic storage.Claim covering delivery
// dedup, source dedup, the concurrency cap, and the insert. A mutex
// serializes same-process deliveries; UNIQUE rows cover sibling processes.
// Inputs: IssueEvent deliveries plus the loaded config and open store.
// Flow: Handle validates -> static gate -> Claim or RecordDenied.
// Returns: Outcome with the durable decision and the new or surviving task.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// ProviderGitHub is the source provider recorded on claimed tasks.
const ProviderGitHub = "github"

// IssueEvent is one labeled-issue trigger delivery: the durable identity
// (delivery id), the issue coordinate, the labels at delivery time, and
// the title/body text that seeds the claimed task's agent goal.
type IssueEvent struct {
	DeliveryID  string
	Repository  string
	IssueNumber int
	Title       string
	Body        string
	Labels      []string
}

// Outcome is the durable decision for one delivery: accepted tasks carry
// the new task, duplicates point at the surviving task, and denials carry
// the policy reason with no task.
type Outcome struct {
	Decision string
	Reason   string
	TaskID   string
	Task     tasks.Task
}

// Handler gates deliveries against config policy and claims tasks.
// Purpose: one shared choke point for the webhook endpoint and the CLI so
// both report the same durable decision for the same delivery.
type Handler struct {
	cfg   *config.Config
	store *storage.Store
	mu    sync.Mutex
}

// New builds a Handler over the loaded config and open store.
func New(cfg *config.Config, store *storage.Store) *Handler {
	return &Handler{cfg: cfg, store: store}
}

// Handle deduplicates, policy-gates, and claims one delivery.
// Why: the single place where "eligible issue becomes a queued task and
// ineligible or duplicate delivery becomes a logged non-event" holds.
// Flow: validate identity -> unknown/disabled repository denied ->
// missing trigger label denied -> atomic Claim (same-delivery and
// same-issue redeliveries return duplicate, a breached cap is denied).
// Malformed events (no delivery id, repository, or issue number) are
// caller errors, not denials: nothing durable can reference them.
func (h *Handler) Handle(ev IssueEvent) (Outcome, error) {
	if strings.TrimSpace(ev.DeliveryID) == "" {
		return Outcome{}, errors.New("ingest: delivery needs an id")
	}
	if strings.TrimSpace(ev.Repository) == "" {
		return Outcome{}, errors.New("ingest: delivery needs a repository")
	}
	if ev.IssueNumber < 1 {
		return Outcome{}, fmt.Errorf("ingest: delivery needs an issue number, got %d", ev.IssueNumber)
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	repo, ok := h.cfg.Repositories[ev.Repository]
	if !ok {
		return h.deny(ev, fmt.Sprintf("repository %q is not configured (defined: %s)",
			ev.Repository, strings.Join(config.RepositoryNames(h.cfg), ", ")))
	}
	if !repo.Enabled {
		return h.deny(ev, fmt.Sprintf("repository %q is disabled", ev.Repository))
	}
	trigger := matchTrigger(ev.Labels, repo.Trigger.Labels)
	if trigger == "" {
		return h.deny(ev, fmt.Sprintf("issue %s labels [%s] include no trigger label (want one of [%s])",
			SourceRef(ev.Repository, ev.IssueNumber),
			strings.Join(sortedLabels(ev.Labels), ", "),
			strings.Join(repo.Trigger.Labels, ", ")))
	}
	// The goal is the issue's title plus body, whichever is present.
	goal := strings.TrimSpace(ev.Title + "\n\n" + ev.Body)
	branch := BranchName(ev.IssueNumber, ev.Title)
	payload, err := json.Marshal(map[string]any{
		"decision":       storage.DecisionAccepted,
		"delivery_id":    ev.DeliveryID,
		"repository":     ev.Repository,
		"issue":          ev.IssueNumber,
		"labels":         sortedLabels(ev.Labels),
		"trigger_labels": repo.Trigger.Labels,
		"agent_profile":  repo.Agent.Default,
		"branch":         branch,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("ingest: encode policy payload: %w", err)
	}
	claimed, err := h.store.Claim(storage.ClaimRequest{
		DeliveryID:     ev.DeliveryID,
		SourceProvider: ProviderGitHub,
		SourceRef:      SourceRef(ev.Repository, ev.IssueNumber),
		Repository:     ev.Repository,
		AgentProfile:   repo.Agent.Default,
		BranchName:     branch,
		Goal:           goal,
		MaxActive:      h.cfg.Scheduler.MaxWorkers,
		PolicyPayload:  string(payload),
		ActorType:      "controller",
		ActorID:        "webhook",
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{
		Decision: claimed.Decision,
		Reason:   claimed.Reason,
		TaskID:   claimed.Task.ID,
		Task:     claimed.Task,
	}, nil
}

// deny records a static policy refusal and maps a redelivered denial to
// duplicate so repeats stay stable and never create work.
func (h *Handler) deny(ev IssueEvent, reason string) (Outcome, error) {
	denied, created, err := h.store.RecordDenied(
		ev.DeliveryID, ProviderGitHub, SourceRef(ev.Repository, ev.IssueNumber), ev.Repository, reason)
	if err != nil {
		return Outcome{}, err
	}
	if !created {
		return Outcome{
			Decision: storage.DecisionDuplicate,
			Reason:   fmt.Sprintf("delivery %q already recorded as %s", ev.DeliveryID, denied.Decision),
			TaskID:   denied.TaskID,
		}, nil
	}
	return Outcome{Decision: denied.Decision, Reason: denied.Reason}, nil
}

// SourceRef renders the durable issue coordinate, e.g. "acme/web#7".
func SourceRef(repository string, issue int) string {
	return fmt.Sprintf("%s#%d", repository, issue)
}

// BranchName renders the worker branch per SPEC section 27:
// herder/<issue>-<slug>, falling back to herder/<issue> when the title
// carries no slug material.
func BranchName(issue int, title string) string {
	if slug := Slug(title); slug != "" {
		return fmt.Sprintf("herder/%d-%s", issue, slug)
	}
	return fmt.Sprintf("herder/%d", issue)
}

// Slug lowercases the issue title into a branch-safe slug of up to 40
// characters: alphanumerics kept, every other run becomes one dash.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return slug
}

// matchTrigger returns the first trigger label present on the issue, or
// "" when the delivery carries no configured trigger.
func matchTrigger(have, triggers []string) string {
	set := make(map[string]bool, len(have))
	for _, l := range have {
		set[l] = true
	}
	for _, want := range triggers {
		if set[want] {
			return want
		}
	}
	return ""
}

// sortedLabels copies labels sorted for stable denial reasons and payloads.
func sortedLabels(labels []string) []string {
	out := append([]string(nil), labels...)
	sort.Strings(out)
	return out
}

// githubIssuesPayload is the subset of GitHub's issues event v0.1 reads:
// the action, the triggering label, the issue, and the repository.
type githubIssuesPayload struct {
	Action string `json:"action"`
	Label  struct {
		Name string `json:"name"`
	} `json:"label"`
	Issue struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ParseGitHubIssuesEvent converts one GitHub issues webhook body into an
// IssueEvent. Non-labeled actions (opened, unlabeled, ...) are not work:
// they return ignored=true with no error and must not be recorded.
// The delivery id travels in the X-GitHub-Delivery header, so it arrives
// as a parameter rather than from the body.
func ParseGitHubIssuesEvent(deliveryID string, body []byte) (ev IssueEvent, ignored bool, err error) {
	var payload githubIssuesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return IssueEvent{}, false, fmt.Errorf("ingest: decode issues event: %w", err)
	}
	if payload.Action != "labeled" {
		return IssueEvent{}, true, nil
	}
	seen := make(map[string]bool)
	var labels []string
	for _, l := range payload.Issue.Labels {
		if l.Name != "" && !seen[l.Name] {
			seen[l.Name] = true
			labels = append(labels, l.Name)
		}
	}
	if payload.Label.Name != "" && !seen[payload.Label.Name] {
		labels = append(labels, payload.Label.Name)
	}
	ev = IssueEvent{
		DeliveryID:  deliveryID,
		Repository:  payload.Repository.FullName,
		IssueNumber: payload.Issue.Number,
		Title:       payload.Issue.Title,
		Body:        payload.Issue.Body,
		Labels:      labels,
	}
	if strings.TrimSpace(ev.Repository) == "" || ev.IssueNumber < 1 {
		return IssueEvent{}, false, errors.New("ingest: issues event needs repository.full_name and issue.number")
	}
	return ev, false, nil
}
