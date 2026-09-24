// Package ingest turns source-provider trigger deliveries into exactly
// one policy-approved task (issues #2, #21; SPEC sections 11, 12, 36, 49).
//
// Why: GitHub and Linear redeliver webhooks and operators relabel issues
// or retry API submissions, so every delivery must converge on one
// claimed task or one logged non-event instead of a second worker.
// Approach: one SourceAdapter per provider authenticates the raw request
// and translates its payload into a normalized TriggerEvent; this Handler
// is the single choke point every adapter feeds — static policy from
// config (repository known and enabled, trigger label present unless the
// caller is itself the trigger), then one atomic storage.Claim covering
// delivery dedup, source dedup, and the insert. Concurrency caps belong
// to the scheduler (issue #7), so bursts queue instead of being denied.
// A mutex serializes same-process deliveries; UNIQUE rows cover sibling
// processes.
// Inputs: TriggerEvent deliveries plus the loaded config and open store.
// Flow: adapter Verify+Parse -> Handle validates -> static gate -> Claim
// or RecordDenied. Returns: Outcome with the durable decision and the new
// or surviving task.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// Source providers recorded on claimed tasks and delivery rows. The
// provider namespaces source refs: github:owner/repo#7, linear:ENG-123,
// and api:owner/repo#7 are three distinct tasks.
const (
	ProviderGitHub = "github"
	ProviderLinear = "linear"
	ProviderAPI    = "api"
)

// TriggerEvent is one normalized trigger delivery from any source
// provider: the durable identity (delivery id, provider, source
// coordinate), the labels at delivery time, and the title/body text that
// seeds the claimed task's agent goal.
type TriggerEvent struct {
	// DeliveryID deduplicates the delivery row (webhook delivery id or
	// API idempotency key).
	DeliveryID string
	// Provider is one of the Provider* constants.
	Provider string
	// Repository is the resolved config repository (owner/repo). Linear
	// adapters resolve it from the issue's team; it may be empty when
	// DenyReason is set.
	Repository string
	// IssueRef is the provider's issue coordinate: "182" for GitHub,
	// "ENG-123" for Linear, the submitted ref for API submissions.
	IssueRef string
	Title    string
	Body     string
	Labels   []string
	// Triggered marks the caller as the trigger itself: authorized API
	// submissions skip the trigger-label gate while repository policy
	// still applies.
	Triggered bool
	// DenyReason pre-denies the delivery before repository validation:
	// adapters that cannot resolve a repository (e.g. an unmapped Linear
	// team) record a policy_denied row instead of failing the request.
	DenyReason string
	// Priority, when non-nil, is the explicit queue priority and wins
	// over "priority:N" label parsing.
	Priority *int
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
// Purpose: one shared choke point for every source adapter and the CLI so
// all report the same durable decision for the same delivery.
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
// Flow: validate identity -> adapter-supplied denial (unmapped Linear
// team) -> unknown/disabled repository denied -> missing trigger label
// denied unless the event is self-triggered (API) -> atomic Claim
// (same-delivery and same-issue redeliveries return duplicate; bursts
// queue for the scheduler's caps rather than being denied here).
// A "priority:N" label on the issue seeds the task's queue priority
// unless the event carries an explicit Priority (advisory only; malformed
// label values are ignored, never denied).
// Malformed events (no delivery id, provider, issue ref, or repository)
// are caller errors, not denials: nothing durable can reference them.
func (h *Handler) Handle(ev TriggerEvent) (Outcome, error) {
	if strings.TrimSpace(ev.DeliveryID) == "" {
		return Outcome{}, errors.New("ingest: delivery needs an id")
	}
	if strings.TrimSpace(ev.Provider) == "" {
		return Outcome{}, errors.New("ingest: delivery needs a source provider")
	}
	if strings.TrimSpace(ev.IssueRef) == "" {
		return Outcome{}, errors.New("ingest: delivery needs an issue ref")
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	sourceRef := SourceRef(ev.Provider, ev.Repository, ev.IssueRef)
	if ev.DenyReason != "" {
		return h.deny(ev, sourceRef, ev.DenyReason)
	}
	if strings.TrimSpace(ev.Repository) == "" {
		return Outcome{}, errors.New("ingest: delivery needs a repository")
	}
	repo, ok := h.cfg.Repositories[ev.Repository]
	if !ok {
		return h.deny(ev, sourceRef, fmt.Sprintf("repository %q is not configured (defined: %s)",
			ev.Repository, strings.Join(config.RepositoryNames(h.cfg), ", ")))
	}
	if !repo.Enabled {
		return h.deny(ev, sourceRef, fmt.Sprintf("repository %q is disabled", ev.Repository))
	}
	trigger := matchTrigger(ev.Labels, repo.Trigger.Labels)
	if trigger == "" && !ev.Triggered {
		return h.deny(ev, sourceRef, fmt.Sprintf("issue %s labels [%s] include no trigger label (want one of [%s])",
			sourceRef,
			strings.Join(sortedLabels(ev.Labels), ", "),
			strings.Join(repo.Trigger.Labels, ", ")))
	}
	// The goal is the issue's title plus body, whichever is present.
	goal := strings.TrimSpace(ev.Title + "\n\n" + ev.Body)
	branch := BranchName(ev.IssueRef, ev.Title)
	payload, err := json.Marshal(map[string]any{
		"decision":       storage.DecisionAccepted,
		"delivery_id":    ev.DeliveryID,
		"provider":       ev.Provider,
		"repository":     ev.Repository,
		"issue":          ev.IssueRef,
		"labels":         sortedLabels(ev.Labels),
		"trigger_labels": repo.Trigger.Labels,
		"agent_profile":  repo.Agent.Default,
		"branch":         branch,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("ingest: encode policy payload: %w", err)
	}
	priority := labelPriority(ev.Labels)
	if ev.Priority != nil {
		priority = *ev.Priority
	}
	claimed, err := h.store.Claim(storage.ClaimRequest{
		DeliveryID:     ev.DeliveryID,
		SourceProvider: ev.Provider,
		SourceRef:      sourceRef,
		Repository:     ev.Repository,
		AgentProfile:   repo.Agent.Default,
		BranchName:     branch,
		Goal:           goal,
		Priority:       priority,
		PolicyPayload:  string(payload),
		ActorType:      "controller",
		ActorID:        actorID(ev.Provider),
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
func (h *Handler) deny(ev TriggerEvent, sourceRef, reason string) (Outcome, error) {
	denied, created, err := h.store.RecordDenied(
		ev.DeliveryID, ev.Provider, sourceRef, ev.Repository, reason)
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

// actorID names the durable actor for one provider's deliveries: webhook
// receipts are the platform's webhook, API submissions are the api caller.
func actorID(provider string) string {
	if provider == ProviderAPI {
		return "api"
	}
	return "webhook"
}

// SourceRef renders the durable issue coordinate per provider: GitHub and
// API refs keep "owner/repo#7" while Linear uses the bare identifier
// "ENG-123" (the repository lives on the task row, not in the ref).
func SourceRef(provider, repository, issueRef string) string {
	if provider == ProviderLinear {
		return issueRef
	}
	return fmt.Sprintf("%s#%s", repository, issueRef)
}

// BranchName renders the worker branch per SPEC section 27:
// herder/<issue>-<slug> with the ref lowercased (herder/eng-123-fix),
// falling back to herder/<issue> when the title carries no slug material.
func BranchName(issueRef, title string) string {
	ref := strings.ToLower(issueRef)
	if slug := Slug(title); slug != "" {
		return fmt.Sprintf("herder/%s-%s", ref, slug)
	}
	return fmt.Sprintf("herder/%s", ref)
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

// labelPriority reads the queue priority convention "priority:N" from the
// issue labels: the highest non-negative integer wins so multiple labels
// are order-independent, malformed or negative values are skipped, and
// absent means 0 (normal).
func labelPriority(labels []string) int {
	best := 0
	for _, l := range labels {
		n, ok := strings.CutPrefix(l, "priority:")
		if !ok {
			continue
		}
		if v, err := strconv.Atoi(n); err == nil && v > best {
			best = v
		}
	}
	return best
}

// sortedLabels copies labels sorted for stable denial reasons and payloads.
func sortedLabels(labels []string) []string {
	out := append([]string(nil), labels...)
	sort.Strings(out)
	return out
}
