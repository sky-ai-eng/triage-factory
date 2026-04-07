package server

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/todo-tinder/internal/delegate"
	"github.com/sky-ai-eng/todo-tinder/internal/jira"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"
)

// OnCredentialsChanged is called after credentials are saved. The caller
// wires this to restart pollers and the delegation spawner.
type OnCredentialsChanged func()

// Server is the main HTTP server for Todo Tinder.
type Server struct {
	db                    *sql.DB
	mux                   *http.ServeMux
	static                fs.FS
	ws                    *websocket.Hub
	spawner               *delegate.Spawner
	jiraClient            *jira.Client
	jiraInProgressStatus  string
	onCredentialsChanged  OnCredentialsChanged
}

// New creates a new server with the given database and registers all routes.
func New(db *sql.DB) *Server {
	s := &Server{
		db:  db,
		mux: http.NewServeMux(),
		ws:  websocket.NewHub(),
	}
	s.routes()
	return s
}

// ListenAndServe starts the HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) routes() {
	// API routes
	s.mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("DELETE /api/auth", s.handleAuthDelete)

	s.mux.HandleFunc("GET /api/queue", s.handleQueue)
	s.mux.HandleFunc("GET /api/tasks", s.handleTasks)
	s.mux.HandleFunc("GET /api/tasks/{id}", s.handleTaskGet)
	s.mux.HandleFunc("POST /api/tasks/{id}/swipe", s.handleSwipe)
	s.mux.HandleFunc("POST /api/tasks/{id}/snooze", s.handleSnooze)
	s.mux.HandleFunc("POST /api/tasks/{id}/undo", s.handleUndo)

	s.mux.HandleFunc("GET /api/agent/runs/{runID}", s.handleAgentStatus)
	s.mux.HandleFunc("GET /api/agent/runs/{runID}/messages", s.handleAgentMessages)
	s.mux.HandleFunc("POST /api/agent/runs/{runID}/cancel", s.handleAgentCancel)
	s.mux.HandleFunc("GET /api/agent/runs", s.handleAgentRuns)

	// Websocket
	s.mux.HandleFunc("GET /api/ws", s.ws.HandleWS)

	s.mux.HandleFunc("GET /api/dashboard/stats", s.handleDashboardStats)
	s.mux.HandleFunc("GET /api/dashboard/prs", s.handleDashboardPRs)
	s.mux.HandleFunc("GET /api/dashboard/prs/{number}/status", s.handleDashboardPRStatus)
	s.mux.HandleFunc("POST /api/dashboard/prs/{number}/draft", s.handleDashboardPRDraft)

	s.mux.HandleFunc("GET /api/brief", s.handleBrief)
	s.mux.HandleFunc("GET /api/preferences", s.handlePreferences)

	s.mux.HandleFunc("GET /api/settings", s.handleSettingsGet)
	s.mux.HandleFunc("POST /api/settings", s.handleSettingsPost)
	s.mux.HandleFunc("GET /api/jira/statuses", s.handleJiraStatuses)

	s.mux.HandleFunc("GET /api/event-types", s.handleEventTypes)
	s.mux.HandleFunc("GET /api/bindings", s.handleAllBindings)
	s.mux.HandleFunc("POST /api/bindings", s.handleBindingCreate)
	s.mux.HandleFunc("DELETE /api/bindings", s.handleBindingDelete)
	s.mux.HandleFunc("POST /api/bindings/set-default", s.handleBindingSetDefault)
	s.mux.HandleFunc("GET /api/prompts", s.handlePromptsList)
	s.mux.HandleFunc("POST /api/prompts", s.handlePromptCreate)
	s.mux.HandleFunc("GET /api/prompts/{id}", s.handlePromptGet)
	s.mux.HandleFunc("PUT /api/prompts/{id}", s.handlePromptPut)
	s.mux.HandleFunc("DELETE /api/prompts/{id}", s.handlePromptDelete)
	s.mux.HandleFunc("GET /api/prompts/{id}/bindings", s.handlePromptBindingsGet)
	s.mux.HandleFunc("PUT /api/prompts/{id}/bindings", s.handlePromptBindingsSet)

	// Frontend: serve embedded SPA, with fallback to index.html for client-side routing
	s.mux.HandleFunc("/", s.handleFrontend)
}

// handleFrontend serves the embedded React SPA. Non-file requests fall back to index.html
// so that client-side routing works.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		http.Error(w, "frontend not built — run: cd frontend && npm run build", http.StatusNotFound)
		return
	}

	path := r.URL.Path
	if path == "/" {
		path = "index.html"
	} else {
		path = strings.TrimPrefix(path, "/")
	}

	// Try to serve the file directly
	if _, err := fs.Stat(s.static, path); err == nil {
		http.ServeFileFS(w, r, s.static, path)
		return
	}

	// Fallback to index.html for SPA client-side routing
	http.ServeFileFS(w, r, s.static, "index.html")
}

// SetStatic sets the embedded frontend filesystem.
func (s *Server) SetStatic(f fs.FS) {
	s.static = f
}

// SetSpawner sets the delegation spawner for agent runs.
func (s *Server) SetSpawner(sp *delegate.Spawner) {
	s.spawner = sp
}

// SetOnCredentialsChanged registers a callback for when credentials are updated.
func (s *Server) SetOnCredentialsChanged(fn OnCredentialsChanged) {
	s.onCredentialsChanged = fn
}

// SetJiraClient sets the Jira client and in-progress status for claim actions.
func (s *Server) SetJiraClient(client *jira.Client, inProgressStatus string) {
	s.jiraClient = client
	s.jiraInProgressStatus = inProgressStatus
}

// --- Stub handlers (to be implemented) ---

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

// Prompt handlers are in prompts_handler.go

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
