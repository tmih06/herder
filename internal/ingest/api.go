package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tmih06/herder/internal/config"
)

// APIAdapter authenticates and translates direct task submissions on
// POST /v1/tasks (issue #21): the operator/CI path that queues work
// without a platform webhook. The bearer token is the api.secret config
// value; the route is only registered when a secret is configured, so an
// empty secret here is a configuration bug, not dev mode.
type APIAdapter struct {
	secret string
}

// NewAPIAdapter builds the adapter from the loaded config.
func NewAPIAdapter(cfg *config.Config) *APIAdapter {
	return &APIAdapter{secret: cfg.API.Secret}
}

// Verify requires Authorization: Bearer <api.secret>, compared in
// constant time. Missing or wrong tokens reject before anything is
// recorded.
func (a *APIAdapter) Verify(r *http.Request, _ []byte) error {
	return verifyBearer(r, a.secret)
}

// Parse converts one verified submission into a self-triggered
// TriggerEvent: the authorized caller is itself the trigger, so the
// trigger-label gate is skipped while repository policy still applies.
// The Idempotency-Key header becomes the delivery id so retried
// submissions dedup; absent keys get a generated api-<uuid> id.
// The issue field accepts a JSON string ("7") or number (7).
func (a *APIAdapter) Parse(r *http.Request, body []byte) (TriggerEvent, string, error) {
	var req apiTaskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return TriggerEvent{}, "", fmt.Errorf("ingest: decode task submission: %w", err)
	}
	issue, err := issueRef(req.Issue)
	if err != nil {
		return TriggerEvent{}, "", err
	}
	if strings.TrimSpace(req.Repository) == "" || issue == "" {
		return TriggerEvent{}, "", errors.New("ingest: task submission needs repository and issue")
	}
	if req.Priority != nil && *req.Priority < 0 {
		return TriggerEvent{}, "", fmt.Errorf("ingest: priority must be >= 0, got %d", *req.Priority)
	}
	delivery := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if delivery == "" {
		delivery = "api-" + uuid.NewString()
	}
	return TriggerEvent{
		DeliveryID: delivery,
		Provider:   ProviderAPI,
		Repository: strings.TrimSpace(req.Repository),
		IssueRef:   issue,
		Title:      req.Title,
		Body:       req.Body,
		Labels:     req.Labels,
		Triggered:  true,
		Priority:   req.Priority,
	}, "", nil
}

// apiTaskRequest is the POST /v1/tasks body: repository, issue ref, the
// title/body that seed the agent goal, labels, and an optional explicit
// queue priority that wins over "priority:N" label parsing.
type apiTaskRequest struct {
	Repository string          `json:"repository"`
	Issue      json.RawMessage `json:"issue"`
	Title      string          `json:"title"`
	Body       string          `json:"body"`
	Labels     []string        `json:"labels"`
	Priority   *int            `json:"priority"`
}

// issueRef normalizes the issue field: a JSON string is used verbatim, a
// JSON number is rendered without quotes. Anything else is malformed.
func issueRef(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s), nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("ingest: issue must be a string or number, got %s", trimmed)
}
