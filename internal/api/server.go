// Package api serves the daemon's local HTTP surface (SPEC section 57):
// a human-readable status view plus the JSON endpoints the CLI queries.
//
// Why: the daemon is the fleet's glass box; even with zero workers an
// operator must see "zero tasks" instead of a refused connection.
// Approach: read-only handlers over the durable store (mutations go
// through the CLI onto the same SQLite file); html/template escapes all
// task content. Health delegates to internal/health so CLI and HTTP agree.
// Inputs: validated config, open store. Flow: New -> Handler/Serve.
// Returns: http.Handler for tests, graceful Shutdown for signals.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/health"
	"github.com/tmih06/herder/internal/storage"
)

// Server is the daemon HTTP server.
type Server struct {
	cfg     *config.Config
	cfgPath string
	store   *storage.Store
	http    *http.Server
	mux     *http.ServeMux
}

// New builds a Server that serves cfg/store on the configured listen addr.
// cfgPath is the config file location reported by /v1/health.
func New(cfg *config.Config, store *storage.Store, cfgPath string) *Server {
	s := &Server{cfg: cfg, cfgPath: cfgPath, store: store, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /", s.handleStatusView)
	s.mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.handleInspectTask)
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.http = &http.Server{Addr: cfg.Server.Listen, Handler: s.mux}
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
<tr><th>ID</th><th>Source</th><th>Status</th><th>Repository</th><th>Agent</th></tr>
{{range .Tasks}}<tr><td>{{.ID}}</td><td>{{.SourceRef}}</td><td>{{.Status}}</td><td>{{.Repository}}</td><td>{{.AgentProfile}}</td></tr>
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

// handleHealth reports controller/storage/Herdr/Docker distinctly so one
// glance separates "daemon broken" from "sandbox host down".
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
