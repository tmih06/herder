package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
