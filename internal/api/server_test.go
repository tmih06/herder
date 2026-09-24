package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/api"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// testServer builds a Server over a temp store with one transition applied.
func testServer(t *testing.T) (*api.Server, *storage.Store) {
	t.Helper()
	cfg, err := config.Load("../../examples/herder.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	store, err := storage.Open(t.TempDir() + "/herder.db")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return api.New(cfg, store, "../../examples/herder.yaml"), store
}

// A fresh store must serve a status view showing zero tasks.
func TestStatusViewShowsZeroTasks(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "0 tasks") {
		t.Errorf("status view should show zero tasks, got:\n%s", body)
	}
}

// GET /v1/tasks must list tasks as JSON once they exist.
func TestListTasksJSON(t *testing.T) {
	srv, store := testServer(t)
	created, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
	})
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks = %d, want 200", rec.Code)
	}
	var payload struct {
		Tasks []tasks.Task `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].ID != created.ID {
		t.Errorf("want the created task, got %+v", payload.Tasks)
	}
}

// GET /v1/tasks/:id must return the task with its event history.
func TestInspectTaskJSON(t *testing.T) {
	srv, store := testServer(t)
	created, err := store.CreateTask(storage.CreateInput{
		SourceProvider: "github", SourceRef: "acme/web#7",
		Repository: "acme/web", AgentProfile: "codex-default",
	})
	if err != nil {
		t.Fatalf("CreateTask = %v", err)
	}
	if _, err := store.Transition(created.ID, tasks.Eligible, "controller", "daemon"); err != nil {
		t.Fatalf("Transition = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+created.ID, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/:id = %d, want 200", rec.Code)
	}
	var payload struct {
		Task   tasks.Task    `json:"task"`
		Events []tasks.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if payload.Task.Status != tasks.Eligible {
		t.Errorf("status = %s, want ELIGIBLE", payload.Task.Status)
	}
	if len(payload.Events) != 2 {
		t.Errorf("want created+transition events, got %d", len(payload.Events))
	}
}

// Unknown task ids must 404 with a JSON error, not an empty 200.
func TestInspectUnknownTask404(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task_missing", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown task = %d, want 404", rec.Code)
	}
}

// GET /v1/health must report controller and storage distinctly.
func TestHealthEndpoint(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/health = %d, want 200", rec.Code)
	}
	var payload map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	for _, section := range []string{"controller", "storage", "herdr", "docker"} {
		if _, ok := payload[section]; !ok {
			t.Errorf("health must report %q distinctly, got keys %v", section, payload)
		}
	}
	if !strings.Contains(payload["controller"]["detail"], "examples/herder.yaml") {
		t.Errorf("controller detail should name the config, got %q", payload["controller"]["detail"])
	}
}

// labeledBody builds a minimal GitHub issues/labeled webhook body: the
// first label is the triggering label, mirroring real deliveries where
// the new label already appears in the issue label list.
func labeledBody(repo string, issue int, labels ...string) string {
	trigger := ""
	if len(labels) > 0 {
		trigger = labels[0]
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, `{"name":`+strconv.Quote(l)+`}`)
	}
	return `{"action":"labeled","label":{"name":` + strconv.Quote(trigger) + `},` +
		`"issue":{"number":` + strconv.Itoa(issue) + `,"title":"Fix OAuth refresh race",` +
		`"labels":[` + strings.Join(names, ",") + `]},` +
		`"repository":{"full_name":` + strconv.Quote(repo) + `}}`
}

// postWebhook delivers one issues webhook to the test server.
func postWebhook(t *testing.T, srv *api.Server, event, delivery, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github", strings.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// An eligible labeled delivery must claim a QUEUED task end to end:
// webhook in, task list and task inspect show it with its timeline.
func TestGitHubWebhookAcceptsEndToEnd(t *testing.T) {
	srv, store := testServer(t)

	rec := postWebhook(t, srv, "issues", "del-1", labeledBody("owner/repo", 182, "bug", "agent-ready"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST webhook = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var outcome map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome["decision"] != "accepted" || outcome["task_id"] == "" {
		t.Fatalf("outcome must accept with a task id, got %v", outcome)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 || found[0].Status != "QUEUED" {
		t.Fatalf("want 1 QUEUED task, got %+v", found)
	}
	if found[0].SourceRef != "owner/repo#182" || found[0].BranchName == "" || found[0].AgentProfile != "codex-default" {
		t.Errorf("task must record source, branch, and agent: %+v", found[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+found[0].ID, nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task = %d, want 200", rec.Code)
	}
	var inspected struct {
		Task   map[string]any   `json:"task"`
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inspected); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	var sawPolicy bool
	for _, e := range inspected.Events {
		if e["Type"] == "policy.decision" {
			sawPolicy = true
		}
	}
	if !sawPolicy {
		t.Errorf("inspect must show the policy outcome in the timeline: %v", inspected.Events)
	}
}

// Redelivering a delivery id, or a new delivery for a claimed issue,
// must not create a second task.
func TestGitHubWebhookDuplicates(t *testing.T) {
	srv, store := testServer(t)
	body := labeledBody("owner/repo", 182, "bug", "agent-ready")

	if rec := postWebhook(t, srv, "issues", "del-1", body); rec.Code != http.StatusCreated {
		t.Fatalf("first POST = %d, want 201", rec.Code)
	}
	for name, tc := range map[string]struct {
		delivery string
		want     int
	}{
		"same delivery": {"del-1", http.StatusOK},
		"same issue":    {"del-2", http.StatusOK},
	} {
		rec := postWebhook(t, srv, "issues", tc.delivery, body)
		if rec.Code != tc.want {
			t.Errorf("%s: POST = %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
		var outcome map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &outcome); err != nil {
			t.Fatalf("%s: decode outcome: %v", name, err)
		}
		if outcome["decision"] != "duplicate" {
			t.Errorf("%s: decision = %q, want duplicate", name, outcome["decision"])
		}
	}
	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("duplicates created %d tasks, want 1", len(found))
	}
}

// Disallowed repositories, missing trigger labels, and breached caps deny
// without a task and stay visible in the deliveries log.
func TestGitHubWebhookDenies(t *testing.T) {
	srv, store := testServer(t)

	rec := postWebhook(t, srv, "issues", "del-evil", labeledBody("evil/repo", 1, "agent-ready"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown repo POST = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var outcome map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome["decision"] != "policy_denied" || !strings.Contains(outcome["reason"], "not configured") {
		t.Fatalf("unknown repo must deny with reason, got %v", outcome)
	}

	rec = postWebhook(t, srv, "issues", "del-nolabel", labeledBody("owner/repo", 2, "bug"))
	if err := json.Unmarshal(rec.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome["decision"] != "policy_denied" || !strings.Contains(outcome["reason"], "trigger label") {
		t.Fatalf("missing label must deny with reason, got %v", outcome)
	}

	found, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks = %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("denials created %d tasks, want 0", len(found))
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/deliveries", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deliveries = %d, want 200", rec.Code)
	}
	var listed struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode deliveries: %v", err)
	}
	if len(listed.Deliveries) != 2 {
		t.Fatalf("want 2 denial rows, got %v", listed.Deliveries)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/deliveries/del-evil", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET delivery = %d, want 200", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/deliveries/missing", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown delivery = %d, want 404", rec.Code)
	}
}

// Non-issues events and non-labeled actions are ignored without recording.
func TestGitHubWebhookIgnored(t *testing.T) {
	srv, store := testServer(t)

	rec := postWebhook(t, srv, "ping", "del-ping", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("ping event POST = %d, want 202", rec.Code)
	}
	opened := `{"action":"opened","issue":{"number":1,"title":"x","labels":[]},
		"repository":{"full_name":"owner/repo"}}`
	rec = postWebhook(t, srv, "issues", "del-opened", opened)
	if rec.Code != http.StatusAccepted {
		t.Errorf("opened action POST = %d, want 202", rec.Code)
	}
	deliveries, err := store.ListDeliveries()
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("ignored events must not record deliveries, got %v", deliveries)
	}
}

// Malformed deliveries fail with nothing recorded.
func TestGitHubWebhookBadRequests(t *testing.T) {
	srv, store := testServer(t)

	rec := postWebhook(t, srv, "issues", "", labeledBody("owner/repo", 1, "agent-ready"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing delivery id POST = %d, want 400", rec.Code)
	}
	rec = postWebhook(t, srv, "issues", "del-bad", `{oops`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("broken JSON POST = %d, want 400", rec.Code)
	}
	deliveries, err := store.ListDeliveries()
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("bad requests must not record deliveries, got %v", deliveries)
	}
}
