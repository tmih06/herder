package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tmih06/herder/internal/config"
)

// GitHubAdapter authenticates and translates GitHub issues webhook
// deliveries. Signature verification runs when github.webhook_secret is
// configured; an empty secret is documented dev mode.
type GitHubAdapter struct {
	webhookSecret string
}

// NewGitHubAdapter builds the adapter from the loaded config.
func NewGitHubAdapter(cfg *config.Config) *GitHubAdapter {
	return &GitHubAdapter{webhookSecret: cfg.Github.WebhookSecret}
}

// Verify enforces X-Hub-Signature-256 ("sha256=<hex>" HMAC-SHA256 over
// the raw body) when a webhook secret is configured.
func (a *GitHubAdapter) Verify(r *http.Request, body []byte) error {
	return verifyHMACSHA256(r.Header.Get("X-Hub-Signature-256"), "sha256=", body, a.webhookSecret)
}

// Parse converts one verified issues delivery into a TriggerEvent.
// Non-issues event types and non-labeled actions are legitimate but not
// work: they return an ignore reason and must not be recorded. The
// delivery id travels in the X-GitHub-Delivery header.
func (a *GitHubAdapter) Parse(r *http.Request, body []byte) (TriggerEvent, string, error) {
	if event := r.Header.Get("X-GitHub-Event"); event != "" && event != "issues" {
		return TriggerEvent{}, "only issues events trigger work", nil
	}
	delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if delivery == "" {
		return TriggerEvent{}, "", errors.New("missing X-GitHub-Delivery header")
	}
	var payload githubIssuesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return TriggerEvent{}, "", fmt.Errorf("ingest: decode issues event: %w", err)
	}
	if payload.Action != "labeled" {
		return TriggerEvent{}, "only labeled actions trigger work", nil
	}
	labels := labelNames(append(payload.Issue.Labels, payload.Label))
	if strings.TrimSpace(payload.Repository.FullName) == "" || payload.Issue.Number < 1 {
		return TriggerEvent{}, "", errors.New("ingest: issues event needs repository.full_name and issue.number")
	}
	return TriggerEvent{
		DeliveryID: delivery,
		Provider:   ProviderGitHub,
		Repository: payload.Repository.FullName,
		IssueRef:   strconv.Itoa(payload.Issue.Number),
		Title:      payload.Issue.Title,
		Body:       payload.Issue.Body,
		Labels:     labels,
	}, "", nil
}

// githubIssuesPayload is the subset of GitHub's issues event the adapter
// reads: the action, the triggering label, the issue, and the repository.
type githubIssuesPayload struct {
	Action string    `json:"action"`
	Label  labelName `json:"label"`
	Issue  struct {
		Number int         `json:"number"`
		Title  string      `json:"title"`
		Body   string      `json:"body"`
		Labels []labelName `json:"labels"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}
