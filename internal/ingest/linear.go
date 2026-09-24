package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tmih06/herder/internal/config"
)

// LinearAdapter authenticates and translates Linear webhook deliveries
// (issue #21). Field names follow Linear's webhook contract: the
// Linear-Delivery header carries the delivery UUID, Linear-Signature is
// the hex HMAC-SHA256 of the raw body signed with the webhook's secret,
// and the body is {action, type, data} where Issue data carries
// identifier, title, description, labels[].name, and team.key.
// Signature verification runs when linear.webhook_secret is configured;
// an empty secret is documented dev mode.
type LinearAdapter struct {
	webhookSecret string
	teams         map[string]string
}

// NewLinearAdapter builds the adapter from the loaded config: teams maps
// Linear team keys (ENG) to configured repositories (owner/repo).
func NewLinearAdapter(cfg *config.Config) *LinearAdapter {
	return &LinearAdapter{webhookSecret: cfg.Linear.WebhookSecret, teams: cfg.Linear.Teams}
}

// Verify enforces Linear-Signature (hex HMAC-SHA256 over the raw body)
// when a webhook secret is configured.
func (a *LinearAdapter) Verify(r *http.Request, body []byte) error {
	return verifyHMACSHA256(r.Header.Get("Linear-Signature"), "", body, a.webhookSecret)
}

// Parse converts one verified Linear delivery into a TriggerEvent. Only
// Issue create/update actions are work: remove actions and non-Issue
// types return an ignore reason and record nothing. The issue's team key
// resolves through linear.teams; an unmapped team pre-denies the delivery
// so misrouted work stays inspectable instead of silent.
func (a *LinearAdapter) Parse(r *http.Request, body []byte) (TriggerEvent, string, error) {
	delivery := strings.TrimSpace(r.Header.Get("Linear-Delivery"))
	if delivery == "" {
		return TriggerEvent{}, "", errors.New("missing Linear-Delivery header")
	}
	var payload linearWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return TriggerEvent{}, "", fmt.Errorf("ingest: decode linear event: %w", err)
	}
	if payload.Type != "Issue" {
		return TriggerEvent{}, "only Issue events trigger work", nil
	}
	if payload.Action != "create" && payload.Action != "update" {
		return TriggerEvent{}, "only create and update actions trigger work", nil
	}
	if strings.TrimSpace(payload.Data.Identifier) == "" {
		return TriggerEvent{}, "", errors.New("ingest: linear event needs data.identifier")
	}
	labels := labelNames(payload.Data.Labels)
	ev := TriggerEvent{
		DeliveryID: delivery,
		Provider:   ProviderLinear,
		IssueRef:   payload.Data.Identifier,
		Title:      payload.Data.Title,
		Body:       payload.Data.Description,
		Labels:     labels,
	}
	team := payload.Data.Team.Key
	repo, ok := a.teams[team]
	if !ok {
		ev.DenyReason = fmt.Sprintf("linear team %q is not mapped to a repository (linear.teams)", team)
		return ev, "", nil
	}
	ev.Repository = repo
	return ev, "", nil
}

// linearWebhookPayload is the subset of Linear's data-change webhook the
// adapter reads: the action, the entity type, and the Issue fields that
// seed the task (identifier, title, description, labels, team key).
type linearWebhookPayload struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		Identifier  string      `json:"identifier"`
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Labels      []labelName `json:"labels"`
		Team        struct {
			Key string `json:"key"`
		} `json:"team"`
	} `json:"data"`
}
