package server

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Server is the main HTTP server for Triage Factory.
type Server struct {
	db                   *sql.DB
	mux                  *http.ServeMux
	static               fs.FS
	ws                   *websocket.Hub
	spawner              *delegate.Spawner
	ghClient             *ghclient.Client
	jiraClient           *jira.Client
	jiraInProgressStatus string
	onGitHubChanged      func() // GitHub creds/repos changed — full restart + re-profile
	onJiraChanged        func() // Jira config changed — restart Jira poller only

	// Jira poll readiness — used by /api/jira/stock to decide whether the
	// poller has completed its first cycle after a restart. Carry-over reads
	// from the DB and needs snapshots to be populated before showing tickets.
	jiraPollMu      sync.RWMutex
	jiraRestartedAt time.Time
	jiraLastPollAt  time.Time
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
	s.mux.HandleFunc("DELETE /api/auth/jira", s.handleAuthDeleteJira)

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
	s.mux.HandleFunc("POST /api/skills/import", s.handleSkillsImport)
	s.mux.HandleFunc("GET /api/github/repos", s.handleGitHubRepos)
	s.mux.HandleFunc("GET /api/repos", s.handleRepoProfiles)
	s.mux.HandleFunc("POST /api/repos", s.handleReposSave)
	s.mux.HandleFunc("PATCH /api/repos/{owner}/{repo}", s.handleRepoUpdate)
	s.mux.HandleFunc("GET /api/repos/{owner}/{repo}/branches", s.handleRepoBranches)
	s.mux.HandleFunc("POST /api/jira/connect", s.handleJiraConnect)
	s.mux.HandleFunc("GET /api/jira/statuses", s.handleJiraStatuses)
	s.mux.HandleFunc("GET /api/jira/stock", s.handleJiraStockGet)
	s.mux.HandleFunc("POST /api/jira/stock", s.handleJiraStockPost)

	s.mux.HandleFunc("GET /api/reviews/{id}", s.handleReviewGet)
	s.mux.HandleFunc("PATCH /api/reviews/{id}", s.handleReviewUpdate)
	s.mux.HandleFunc("GET /api/reviews/{id}/diff", s.handleReviewDiff)
	s.mux.HandleFunc("POST /api/reviews/{id}/submit", s.handleReviewSubmit)
	s.mux.HandleFunc("PUT /api/reviews/{id}/comments/{commentId}", s.handleReviewCommentUpdate)
	s.mux.HandleFunc("DELETE /api/reviews/{id}/comments/{commentId}", s.handleReviewCommentDelete)
	s.mux.HandleFunc("GET /api/agent/runs/{runID}/review", s.handleRunReview)

	s.mux.HandleFunc("GET /api/event-types", s.handleEventTypes)
	s.mux.HandleFunc("GET /api/event-schemas", s.handleEventSchemasList)
	s.mux.HandleFunc("GET /api/event-schemas/{event_type}", s.handleEventSchemaGet)
	s.mux.HandleFunc("GET /api/triggers", s.handleTriggersList)
	s.mux.HandleFunc("POST /api/triggers", s.handleTriggerCreate)
	s.mux.HandleFunc("PUT /api/triggers/{id}", s.handleTriggerUpdate)
	s.mux.HandleFunc("DELETE /api/triggers/{id}", s.handleTriggerDelete)
	s.mux.HandleFunc("POST /api/triggers/{id}/toggle", s.handleTriggerToggle)

	s.mux.HandleFunc("GET /api/task-rules", s.handleTaskRulesList)
	s.mux.HandleFunc("POST /api/task-rules", s.handleTaskRuleCreate)
	s.mux.HandleFunc("PUT /api/task-rules/reorder", s.handleTaskRuleReorder)
	s.mux.HandleFunc("PATCH /api/task-rules/{id}", s.handleTaskRuleUpdate)
	s.mux.HandleFunc("DELETE /api/task-rules/{id}", s.handleTaskRuleDelete)
	s.mux.HandleFunc("GET /api/prompts", s.handlePromptsList)
	s.mux.HandleFunc("POST /api/prompts", s.handlePromptCreate)
	s.mux.HandleFunc("GET /api/prompts/{id}", s.handlePromptGet)
	s.mux.HandleFunc("PUT /api/prompts/{id}", s.handlePromptPut)
	s.mux.HandleFunc("DELETE /api/prompts/{id}", s.handlePromptDelete)
	s.mux.HandleFunc("GET /api/prompts/{id}/stats", s.handlePromptStats)

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

// SetOnGitHubChanged registers a callback for GitHub config changes (creds, URL, repos).
// This triggers a full restart: invalidate profiles → stop all pollers → re-profile → restart.
func (s *Server) SetOnGitHubChanged(fn func()) {
	s.onGitHubChanged = fn
}

// SetOnJiraChanged registers a callback for Jira config changes.
// This restarts only the Jira poller.
func (s *Server) SetOnJiraChanged(fn func()) {
	s.onJiraChanged = fn
}

// SetGitHubClient sets the GitHub client for review approval submissions.
func (s *Server) SetGitHubClient(client *ghclient.Client) {
	s.ghClient = client
}

// SetJiraClient sets the Jira client and in-progress status for claim actions.
func (s *Server) SetJiraClient(client *jira.Client, inProgressStatus string) {
	s.jiraClient = client
	s.jiraInProgressStatus = inProgressStatus
}

// MarkJiraRestarted records the moment the Jira poller was restarted. Clears
// the last-poll timestamp so jiraPollReady reports false until a completion
// event arrives. Call from the Jira-changed callback.
func (s *Server) MarkJiraRestarted() {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	s.jiraRestartedAt = time.Now()
	s.jiraLastPollAt = time.Time{}
}

// MarkJiraPollComplete records a successful Jira poll cycle. Call from the
// event-bus subscriber on system:poll:completed when source == "jira".
// startedAt is the wall-clock time the poll cycle started; completions from
// poll goroutines that started before the most recent MarkJiraRestarted are
// ignored so an in-flight pre-restart poll can't incorrectly flip readiness
// back to true.
func (s *Server) MarkJiraPollComplete(startedAt time.Time) {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	if startedAt.Before(s.jiraRestartedAt) {
		return
	}
	s.jiraLastPollAt = time.Now()
}

// jiraPollReady returns true when the poller has completed at least one cycle
// since the last restart. Used by /api/jira/stock to gate the list response.
func (s *Server) jiraPollReady() bool {
	s.jiraPollMu.RLock()
	defer s.jiraPollMu.RUnlock()
	return !s.jiraLastPollAt.IsZero() && s.jiraLastPollAt.After(s.jiraRestartedAt)
}

// --- Stub handlers (to be implemented) ---

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

// Prompt handlers are in prompts_handler.go
// Skill import handler is in skills_handler.go

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
