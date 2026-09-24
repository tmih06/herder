// Package api serves the daemon's local HTTP surface (SPEC section 57):
// a human-readable status view plus the JSON endpoints the CLI queries.
//
// Why: the daemon is the fleet's glass box; even with zero workers an
// operator must see "zero tasks" instead of a refused connection, and
// every trigger delivery must land in one place with one durable decision.
// Approach: status and task reads over the durable store (operator
// mutations go through the CLI onto the same SQLite file) plus the GitHub
// webhook receiver that gates and claims through internal/ingest;
// html/template escapes all task content. Health delegates to
// internal/health so CLI and HTTP agree.
// Inputs: validated config, open store. Flow: New -> Handler/Serve.
// Returns: http.Handler for tests, graceful Shutdown for signals.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/health"
	"github.com/tmih06/herder/internal/ingest"
	"github.com/tmih06/herder/internal/storage"
)

// Server is the daemon HTTP server.
type Server struct {
	cfg      *config.Config
	cfgPath  string
	store    *storage.Store
	ingest   *ingest.Handler
	adapters map[string]ingest.SourceAdapter
	api      ingest.SourceAdapter
	http     *http.Server
	mux      *http.ServeMux
}

// New builds a Server that serves cfg/store on the configured listen addr.
// cfgPath is the config file location reported by /v1/health.
// Every inbound route runs the same flow: the provider's SourceAdapter
// verifies the request and translates the payload, then ingest.Handler
// applies the one policy gate. POST /v1/tasks answers 404 unless
// api.secret is configured — the daemon never runs an unauthenticated
// queue.
func New(cfg *config.Config, store *storage.Store, cfgPath string) *Server {
	s := &Server{
		cfg: cfg, cfgPath: cfgPath, store: store, ingest: ingest.New(cfg, store),
		adapters: map[string]ingest.SourceAdapter{
			ingest.ProviderGitHub: ingest.NewGitHubAdapter(cfg),
			ingest.ProviderLinear: ingest.NewLinearAdapter(cfg),
		},
		api: ingest.NewAPIAdapter(cfg),
		mux: http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /", s.handleStatusView)
	s.mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.handleInspectTask)
	s.mux.HandleFunc("GET /v1/deliveries", s.handleListDeliveries)
	s.mux.HandleFunc("GET /v1/deliveries/{id}", s.handleInspectDelivery)
	s.mux.HandleFunc("POST /v1/webhooks/{provider}", s.handleWebhook)
	s.mux.HandleFunc("POST /v1/tasks", s.handleSubmitTask)
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.http = &http.Server{Addr: cfg.Server.Listen, Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Handler exposes the routes for tests and embedding.
// Purpose: httptest access without binding a port. Returns the mux.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe blocks serving HTTP until Shutdown or a fatal error.
// Purpose: daemon's blocking serve loop. Returns http.ErrServerClosed on
// graceful shutdown, any other error on bind/serve failure.
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown drains the server gracefully.
// Purpose: let in-flight status/JSON reads finish on SIGINT/SIGTERM.
// Inputs: context bounding the drain. Returns the shutdown error, if any.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// statusTemplate is the local status view: task count plus one row per task.
var statusTemplate = template.Must(template.New("status").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Herder</title></head>
<body>
<h1>Herder</h1>
<p>{{len .Tasks}} tasks</p>
<table border="1">
<tr><th>ID</th><th>Source</th><th>Status</th><th>Agent state</th><th>Repository</th><th>Agent</th></tr>
{{range .Tasks}}<tr><td>{{.ID}}</td><td>{{.SourceRef}}</td><td>{{.Status}}</td><td>{{.AgentState}}</td><td>{{.Repository}}</td><td>{{.AgentProfile}}</td></tr>
{{end}}</table>
</body></html>
`))

// handleStatusView renders the human-readable fleet overview.
// A fresh daemon shows "0 tasks" instead of an error.
func (s *Server) handleStatusView(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	found, err := s.store.ListTasks()
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = statusTemplate.Execute(w, map[string]any{"Tasks": found})
}

// handleListTasks returns every task as JSON for `herder status`.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	found, err := s.store.ListTasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": found})
}

// handleInspectTask returns one task plus its full event history.
func (s *Server) handleInspectTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.store.GetTask(id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("task %q not found", id))
		return
	}
	events, err := s.store.ListEvents(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "events": events})
}

// maxInboundBody caps one inbound delivery body: trigger payloads are
// small JSON, and the daemon must not buffer unbounded uploads. It
// applies to every inbound route — both webhooks and /v1/tasks.
const maxInboundBody = 1 << 20

// decisionEnvelope is the JSON decision envelope every inbound route
// answers: accepted tasks carry the new task id, duplicates point at the
// survivor, and denials carry the policy reason.
type decisionEnvelope struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	TaskID   string `json:"task_id,omitempty"`
}

// handleWebhook receives one source-provider delivery on
// POST /v1/webhooks/{provider}: the matching adapter verifies the request
// and translates the payload, then ingest.Handler reports the durable
// decision. Unknown providers answer 404. Accepted claims answer 201,
// duplicate and policy_denied deliveries answer 200 (both recorded),
// ignored event types answer 202, malformed deliveries answer 400 with
// nothing recorded, and failed authentication answers 401 with nothing
// recorded.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	adapter, ok := s.adapters[r.PathValue("provider")]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown source provider %q", r.PathValue("provider")))
		return
	}
	s.handleDelivery(w, r, adapter)
}

// handleSubmitTask queues one direct task submission on POST /v1/tasks.
// The endpoint answers the mux's own 404 when api.secret is unset — an
// unauthenticated queue is indistinguishable from a route that does not
// exist.
func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	if s.cfg.API.Secret == "" {
		http.NotFound(w, r)
		return
	}
	s.handleDelivery(w, r, s.api)
}

// handleDelivery runs the shared inbound flow behind every trigger
// route: read the bounded body, authenticate through the adapter,
// translate the payload, then gate and claim through ingest.Handler.
func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request, adapter ingest.SourceAdapter) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read delivery body: %v", err))
		return
	}
	if err := adapter.Verify(r, body); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	event, ignored, err := adapter.Parse(r, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ignored != "" {
		writeJSON(w, http.StatusAccepted, decisionEnvelope{Decision: "ignored", Reason: ignored})
		return
	}
	out, err := s.ingest.Handle(event)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	code := http.StatusOK
	if out.Decision == storage.DecisionAccepted {
		code = http.StatusCreated
	}
	writeJSON(w, code, decisionEnvelope{Decision: out.Decision, Reason: out.Reason, TaskID: out.TaskID})
}

// handleListDeliveries returns every recorded delivery oldest-first: the
// inspectable log of accepted tasks and policy-denied or duplicate
// non-events behind `herder ingest log`.
func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	found, err := s.store.ListDeliveries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": found})
}

// handleInspectDelivery returns one recorded delivery by its provider
// delivery id (GitHub/Linear delivery UUID or API idempotency key).
func (s *Server) handleInspectDelivery(w http.ResponseWriter, r *http.Request) {
	delivery, err := s.store.GetDelivery(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("delivery %q not found", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": delivery})
}

// handleHealth reports controller/storage/Herdr/Docker/SSH distinctly so
// one glance separates "daemon broken" from "sandbox host down".
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	report := health.Build(s.cfg, s.cfgPath, nil, s.store)
	writeJSON(w, http.StatusOK, report)
}

// writeJSON encodes v with indentation for curl readability.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeError renders a JSON error envelope.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": strings.TrimSpace(msg)})
}
