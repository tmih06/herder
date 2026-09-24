package ingest_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/ingest"
	"github.com/tmih06/herder/internal/storage"
)

// hmacHex computes the hex HMAC-SHA256 a webhook sender would attach.
func hmacHex(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// githubReq builds one signed-or-unsigned issues delivery request.
func githubReq(body, delivery, signature string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", delivery)
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	return req
}

// GitHub signature verification must accept valid HMACs, reject missing
// or wrong signatures, and skip entirely when no secret is configured.
func TestGitHubAdapterVerify(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.Github.WebhookSecret = "gh-secret"
	adapter := ingest.NewGitHubAdapter(cfg)
	body := `{"action":"labeled"}`

	if err := adapter.Verify(githubReq(body, "d", "sha256="+hmacHex("gh-secret", body)), []byte(body)); err != nil {
		t.Errorf("valid signature must verify: %v", err)
	}
	for name, sig := range map[string]string{
		"missing":      "",
		"bad hex":      "sha256=zzzz",
		"wrong secret": "sha256=" + hmacHex("other", body),
		"wrong body":   "sha256=" + hmacHex("gh-secret", "{}"),
		"bare hex":     hmacHex("gh-secret", body),
		"truncated":    "sha256=" + hmacHex("gh-secret", body)[:10],
	} {
		if err := adapter.Verify(githubReq(body, "d", sig), []byte(body)); err == nil {
			t.Errorf("%s: Verify must reject", name)
		}
	}

	cfg.Github.WebhookSecret = ""
	open := ingest.NewGitHubAdapter(cfg)
	if err := open.Verify(githubReq(body, "d", ""), []byte(body)); err != nil {
		t.Errorf("unset secret must skip verification: %v", err)
	}
}

// linearBody builds a minimal Linear Issue webhook body.
func linearBody(action, teamKey string, labels ...string) string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, `{"name":"`+l+`"}`)
	}
	team := "null"
	if teamKey != "" {
		team = `{"key":"` + teamKey + `"}`
	}
	return `{"action":"` + action + `","type":"Issue","data":{` +
		`"identifier":"ENG-123","title":"Fix OAuth refresh race",` +
		`"description":"Refresh tokens race on expiry",` +
		`"labels":[` + strings.Join(names, ",") + `],"team":` + team + `}}`
}

// linearReq builds one Linear delivery request.
func linearReq(body, delivery, signature string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/linear", strings.NewReader(body))
	req.Header.Set("Linear-Delivery", delivery)
	req.Header.Set("Linear-Event", "Issue")
	if signature != "" {
		req.Header.Set("Linear-Signature", signature)
	}
	return req
}

// linearSetup loads the example config with a Linear team mapping.
func linearSetup(t *testing.T) *ingest.LinearAdapter {
	t.Helper()
	cfg, _, _ := testSetup(t)
	cfg.Linear.Teams = map[string]string{"ENG": "owner/repo"}
	return ingest.NewLinearAdapter(cfg)
}

// Linear Issue create/update deliveries must translate into a
// linear-provider event on the mapped repository.
func TestLinearAdapterParse(t *testing.T) {
	adapter := linearSetup(t)

	for _, action := range []string{"create", "update"} {
		body := linearBody(action, "ENG", "bug", "agent-ready", "priority:3")
		ev, ignored, err := adapter.Parse(linearReq(body, "lin-del-1", ""), []byte(body))
		if err != nil || ignored != "" {
			t.Fatalf("%s: Parse = %+v, %q, %v; want event", action, ev, ignored, err)
		}
		if ev.Provider != ingest.ProviderLinear || ev.IssueRef != "ENG-123" ||
			ev.Repository != "owner/repo" || ev.DeliveryID != "lin-del-1" {
			t.Errorf("%s: event mistranslated: %+v", action, ev)
		}
		if ev.Title != "Fix OAuth refresh race" || ev.Body != "Refresh tokens race on expiry" {
			t.Errorf("%s: title/body lost: %+v", action, ev)
		}
		if len(ev.Labels) != 3 {
			t.Errorf("%s: labels = %v, want 3", action, ev.Labels)
		}
	}
}

// Non-Issue types and remove actions are legitimate chatter: ignored
// with a reason, never an error, never recorded.
func TestLinearAdapterIgnores(t *testing.T) {
	adapter := linearSetup(t)

	comment := `{"action":"create","type":"Comment","data":{"id":"c1"}}`
	if _, ignored, err := adapter.Parse(linearReq(comment, "d1", ""), []byte(comment)); err != nil || ignored == "" {
		t.Errorf("Comment event must be ignored, got %q, %v", ignored, err)
	}
	remove := linearBody("remove", "ENG", "agent-ready")
	if _, ignored, err := adapter.Parse(linearReq(remove, "d2", ""), []byte(remove)); err != nil || ignored == "" {
		t.Errorf("remove action must be ignored, got %q, %v", ignored, err)
	}
}

// An unmapped or absent team must pre-deny the delivery so the misroute
// is inspectable, not a request error and not silent.
func TestLinearAdapterUnmappedTeam(t *testing.T) {
	adapter := linearSetup(t)

	for name, body := range map[string]string{
		"unmapped team": linearBody("create", "OPS", "agent-ready"),
		"missing team":  linearBody("create", "", "agent-ready"),
	} {
		ev, ignored, err := adapter.Parse(linearReq(body, "d-"+name, ""), []byte(body))
		if err != nil || ignored != "" {
			t.Fatalf("%s: Parse = %+v, %q, %v; want denied event", name, ev, ignored, err)
		}
		if ev.DenyReason == "" || ev.Repository != "" {
			t.Errorf("%s: want DenyReason with no repository, got %+v", name, ev)
		}
	}
}

// Linear signature verification mirrors GitHub's: bare hex HMAC-SHA256,
// enforced only when a secret is configured.
func TestLinearAdapterVerify(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.Linear.WebhookSecret = "lin-secret"
	adapter := ingest.NewLinearAdapter(cfg)
	body := linearBody("create", "ENG", "agent-ready")

	if err := adapter.Verify(linearReq(body, "d", hmacHex("lin-secret", body)), []byte(body)); err != nil {
		t.Errorf("valid signature must verify: %v", err)
	}
	for name, sig := range map[string]string{
		"missing":      "",
		"bad hex":      "zzzz",
		"wrong secret": hmacHex("other", body),
	} {
		if err := adapter.Verify(linearReq(body, "d", sig), []byte(body)); err == nil {
			t.Errorf("%s: Verify must reject", name)
		}
	}
}

// A Linear delivery must claim a task with the bare identifier as source
// ref, a lowercased herder/eng-123-* branch, and the mapped repository.
func TestLinearEventClaimsEndToEnd(t *testing.T) {
	cfg, _, h := testSetup(t)
	cfg.Linear.Teams = map[string]string{"ENG": "owner/repo"}
	adapter := ingest.NewLinearAdapter(cfg)

	body := linearBody("create", "ENG", "agent-ready", "priority:2")
	ev, ignored, err := adapter.Parse(linearReq(body, "lin-del-9", ""), []byte(body))
	if err != nil || ignored != "" {
		t.Fatalf("Parse = %+v, %q, %v", ev, ignored, err)
	}
	out, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionAccepted {
		t.Fatalf("decision = %q, want accepted", out.Decision)
	}
	if out.Task.SourceProvider != "linear" || out.Task.SourceRef != "ENG-123" {
		t.Errorf("source identity = %s:%s, want linear:ENG-123", out.Task.SourceProvider, out.Task.SourceRef)
	}
	if out.Task.Repository != "owner/repo" {
		t.Errorf("repository = %q, want mapped owner/repo", out.Task.Repository)
	}
	if !strings.HasPrefix(out.Task.BranchName, "herder/eng-123-") {
		t.Errorf("branch = %q, want herder/eng-123-*", out.Task.BranchName)
	}
	if out.Task.Priority != 2 {
		t.Errorf("priority = %d, want 2 from priority:2 label", out.Task.Priority)
	}
}

// An unmapped Linear team must record a policy_denied delivery with no
// task, and the redelivery must report duplicate.
func TestLinearUnmappedTeamDenies(t *testing.T) {
	cfg, store, h := testSetup(t)
	cfg.Linear.Teams = map[string]string{"ENG": "owner/repo"}
	adapter := ingest.NewLinearAdapter(cfg)

	body := linearBody("create", "OPS", "agent-ready")
	ev, _, err := adapter.Parse(linearReq(body, "lin-del-ops", ""), []byte(body))
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	out, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionPolicyDenied || !strings.Contains(out.Reason, "OPS") {
		t.Fatalf("unmapped team must deny naming the team, got %+v", out)
	}
	again, err := h.Handle(ev)
	if err != nil || again.Decision != storage.DecisionDuplicate {
		t.Errorf("redelivery = %+v, %v; want duplicate", again, err)
	}
	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("unmapped team created %d tasks, want 0", len(found))
	}
}

// apiReq builds one POST /v1/tasks request.
func apiReq(body, token, idempotencyKey string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req
}

// The API adapter must require the configured bearer token in constant
// time and reject everything else.
func TestAPIAdapterVerify(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.API.Secret = "s3cret"
	adapter := ingest.NewAPIAdapter(cfg)
	body := `{}`

	if err := adapter.Verify(apiReq(body, "s3cret", ""), []byte(body)); err != nil {
		t.Errorf("correct token must verify: %v", err)
	}
	for name, token := range map[string]string{
		"missing": "",
		"wrong":   "nope",
		"prefix":  "s3cret extra",
	} {
		if err := adapter.Verify(apiReq(body, token, ""), []byte(body)); err == nil {
			t.Errorf("%s: Verify must reject", name)
		}
	}
	// A bare secret without the Bearer scheme must reject.
	raw := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
	raw.Header.Set("Authorization", "s3cret")
	if err := adapter.Verify(raw, []byte(body)); err == nil {
		t.Errorf("scheme-less secret: Verify must reject")
	}
}

// API submissions must translate into self-triggered api-provider events:
// the idempotency key becomes the delivery id, a missing key gets a
// generated one, and explicit priority rides the event.
func TestAPIAdapterParse(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.API.Secret = "s3cret"
	adapter := ingest.NewAPIAdapter(cfg)

	body := `{"repository":"owner/repo","issue":"7","title":"Fix it","body":"details","labels":["bug"],"priority":4}`
	ev, ignored, err := adapter.Parse(apiReq(body, "s3cret", "ci-run-42"), []byte(body))
	if err != nil || ignored != "" {
		t.Fatalf("Parse = %+v, %q, %v; want event", ev, ignored, err)
	}
	if ev.Provider != ingest.ProviderAPI || ev.DeliveryID != "ci-run-42" ||
		ev.Repository != "owner/repo" || ev.IssueRef != "7" {
		t.Errorf("event mistranslated: %+v", ev)
	}
	if !ev.Triggered {
		t.Errorf("API submissions must be self-triggered")
	}
	if ev.Priority == nil || *ev.Priority != 4 {
		t.Errorf("priority = %v, want explicit 4", ev.Priority)
	}

	// Numeric issue and generated delivery id.
	numeric := `{"repository":"owner/repo","issue":9}`
	ev, _, err = adapter.Parse(apiReq(numeric, "s3cret", ""), []byte(numeric))
	if err != nil || ev.IssueRef != "9" || !strings.HasPrefix(ev.DeliveryID, "api-") {
		t.Errorf("numeric issue/generated id mistranslated: %+v, %v", ev, err)
	}

	for name, body := range map[string]string{
		"broken json":       `{oops`,
		"missing repo":      `{"issue":"7"}`,
		"missing issue":     `{"repository":"owner/repo"}`,
		"negative priority": `{"repository":"owner/repo","issue":"7","priority":-1}`,
		"object issue":      `{"repository":"owner/repo","issue":{"n":1}}`,
	} {
		if _, _, err := adapter.Parse(apiReq(body, "s3cret", ""), []byte(body)); err == nil {
			t.Errorf("%s: Parse must fail", name)
		}
	}
}

// An authorized API submission must claim without any trigger label, and
// explicit priority must win over priority:N labels.
func TestAPIEventClaimsEndToEnd(t *testing.T) {
	cfg, store, h := testSetup(t)
	cfg.API.Secret = "s3cret"
	adapter := ingest.NewAPIAdapter(cfg)

	body := `{"repository":"owner/repo","issue":"7","title":"Fix it","labels":["priority:9"],"priority":4}`
	ev, _, err := adapter.Parse(apiReq(body, "s3cret", "ci-1"), []byte(body))
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	out, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if out.Decision != storage.DecisionAccepted {
		t.Fatalf("decision = %q, want accepted (no trigger label needed)", out.Decision)
	}
	if out.Task.SourceProvider != "api" || out.Task.SourceRef != "owner/repo#7" {
		t.Errorf("source identity = %s:%s, want api:owner/repo#7", out.Task.SourceProvider, out.Task.SourceRef)
	}
	if out.Task.Priority != 4 {
		t.Errorf("priority = %d, want explicit 4 over label 9", out.Task.Priority)
	}
	events, err := store.ListEvents(out.TaskID)
	if err != nil {
		t.Fatalf("ListEvents = %v", err)
	}
	if len(events) == 0 || events[0].ActorID != "api" {
		t.Errorf("task events must record actor api, got %+v", events[0])
	}
}

// Local repositories have no issue tracker: the issue field is optional
// and the delivery id becomes the source coordinate so distinct
// submissions stay distinct tasks. Non-local repos still require it.
func TestAPIAdapterParseLocalIssueOptional(t *testing.T) {
	cfg, _, _ := testSetup(t)
	cfg.API.Secret = "s3cret"
	repo := cfg.Repositories["owner/repo"]
	repo.Local = "/srv/git/owner-repo.git"
	cfg.Repositories["owner/repo"] = repo
	adapter := ingest.NewAPIAdapter(cfg)

	// Idempotency-Key doubles as the issue ref for local submissions.
	body := `{"repository":"owner/repo","title":"Fix it"}`
	ev, _, err := adapter.Parse(apiReq(body, "s3cret", "ci-9"), []byte(body))
	if err != nil {
		t.Fatalf("local submission without issue must parse: %v", err)
	}
	if ev.IssueRef != "ci-9" || ev.DeliveryID != "ci-9" {
		t.Errorf("issue ref must fall back to the idempotency key: %+v", ev)
	}
	// No key at all: a generated ref keeps submissions distinct.
	ev, _, err = adapter.Parse(apiReq(body, "s3cret", ""), []byte(body))
	if err != nil || !strings.HasPrefix(ev.IssueRef, "api-") {
		t.Errorf("missing key must generate a unique ref: %+v, %v", ev, err)
	}
	// An explicit issue still wins for local repos.
	withIssue := `{"repository":"owner/repo","issue":"work-1"}`
	ev, _, err = adapter.Parse(apiReq(withIssue, "s3cret", "k"), []byte(withIssue))
	if err != nil || ev.IssueRef != "work-1" {
		t.Errorf("explicit issue must win: %+v, %v", ev, err)
	}
	// Non-local and unknown repositories still require the issue.
	for name, repoName := range map[string]string{
		"github repo":  "other/repo",
		"unknown repo": "nobody/nothing",
	} {
		cfg2, _, _ := testSetup(t)
		cfg2.API.Secret = "s3cret"
		if repo, ok := cfg2.Repositories["owner/repo"]; ok && repoName == "other/repo" {
			cfg2.Repositories["other/repo"] = repo // github-backed, not local
		}
		a := ingest.NewAPIAdapter(cfg2)
		b := `{"repository":"` + repoName + `"}`
		if _, _, err := a.Parse(apiReq(b, "s3cret", ""), []byte(b)); err == nil {
			t.Errorf("%s: missing issue must fail for non-local repo", name)
		}
	}
}

// The panel name flows from the submission through the claim: an explicit
// name wins (sanitized), and an absent name derives <repo>-issue-<ref>.
func TestAPIAdapterNameAndDerivation(t *testing.T) {
	cfg, store, h := testSetup(t)
	cfg.API.Secret = "s3cret"
	adapter := ingest.NewAPIAdapter(cfg)

	// Explicit name, sanitized into Herdr's class.
	body := `{"repository":"owner/repo","issue":"7","name":"My Panel: OAuth Fix"}`
	ev, _, err := adapter.Parse(apiReq(body, "s3cret", "n1"), []byte(body))
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	out, err := h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	task, err := store.GetTask(out.TaskID)
	if err != nil {
		t.Fatalf("GetTask = %v", err)
	}
	if task.DisplayName != "my-panel-oauth-fix" {
		t.Errorf("DisplayName = %q, want sanitized explicit name", task.DisplayName)
	}

	// Derived default: <repo-name>-issue-<ref>.
	body = `{"repository":"owner/repo","issue":"42"}`
	ev, _, err = adapter.Parse(apiReq(body, "s3cret", "n2"), []byte(body))
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	out, err = h.Handle(ev)
	if err != nil {
		t.Fatalf("Handle = %v", err)
	}
	task, err = store.GetTask(out.TaskID)
	if err != nil {
		t.Fatalf("GetTask = %v", err)
	}
	if task.DisplayName != "repo-issue-42" {
		t.Errorf("DisplayName = %q, want derived repo-issue-42", task.DisplayName)
	}
}

// DisplayName derivation covers every provider shape.
func TestDisplayNameDerivation(t *testing.T) {
	for name, tc := range map[string]struct {
		ev   ingest.TriggerEvent
		want string
	}{
		"github":    {ingest.TriggerEvent{Provider: ingest.ProviderGitHub, Repository: "acme/web", IssueRef: "7"}, "web-issue-7"},
		"api":       {ingest.TriggerEvent{Provider: ingest.ProviderAPI, Repository: "acme/web", IssueRef: "ci-9"}, "web-issue-ci-9"},
		"linear":    {ingest.TriggerEvent{Provider: ingest.ProviderLinear, Repository: "acme/web", IssueRef: "ENG-123"}, "web-eng-123"},
		"explicit":  {ingest.TriggerEvent{Provider: ingest.ProviderAPI, Repository: "acme/web", IssueRef: "7", Name: "Custom Name!"}, "custom-name"},
		"long repo": {ingest.TriggerEvent{Provider: ingest.ProviderGitHub, Repository: "org/a-very-long-repository-name-here", IssueRef: "12345"}, "a-very-long-reposit-issue-12345"},
	} {
		if got := ingest.DisplayName(tc.ev); got != tc.want {
			t.Errorf("%s: DisplayName = %q, want %q", name, got, tc.want)
		}
	}
}
