package server

import (
	"net/http"

	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
	"github.com/sky-ai-eng/todo-triage/pkg/websocket"
)

func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	run, err := db.GetAgentRun(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	messages, err := db.MessagesForRun(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if messages == nil {
		messages = []domain.AgentMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := s.spawner.Cancel(runID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id query parameter required"})
		return
	}
	runs, err := db.AgentRunsForTask(s.db, taskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if runs == nil {
		runs = []domain.AgentRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
