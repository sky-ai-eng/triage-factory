package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// taskJSON is the API representation of a task.
type taskJSON struct {
	ID              string   `json:"id"`
	Source          string   `json:"source"`
	SourceID        string   `json:"source_id"`
	SourceURL       string   `json:"source_url"`
	Title           string   `json:"title"`
	Description     string   `json:"description,omitempty"`
	Repo            string   `json:"repo,omitempty"`
	Author          string   `json:"author,omitempty"`
	Labels          []string `json:"labels"`
	Severity        string   `json:"severity,omitempty"`
	DiffAdditions   int      `json:"diff_additions,omitempty"`
	DiffDeletions   int      `json:"diff_deletions,omitempty"`
	FilesChanged    int      `json:"files_changed,omitempty"`
	CIStatus        string   `json:"ci_status,omitempty"`
	RelevanceReason string   `json:"relevance_reason,omitempty"`
	EventType       string   `json:"event_type,omitempty"`
	ScoringStatus   string   `json:"scoring_status"`
	CreatedAt       string   `json:"created_at"`
	Status          string   `json:"status"`
	PriorityScore   *float64 `json:"priority_score"`
	AISummary         string   `json:"ai_summary,omitempty"`
	PriorityReasoning string   `json:"priority_reasoning,omitempty"`
	AgentConfidence   *float64 `json:"agent_confidence"`
}

func taskToJSON(t domain.Task) taskJSON {
	labels := t.Labels
	if labels == nil {
		labels = []string{}
	}
	return taskJSON{
		ID:              t.ID,
		Source:          t.Source,
		SourceID:        t.SourceID,
		SourceURL:       t.SourceURL,
		Title:           t.Title,
		Description:     t.Description,
		Repo:            t.Repo,
		Author:          t.Author,
		Labels:          labels,
		Severity:        t.Severity,
		DiffAdditions:   t.DiffAdditions,
		DiffDeletions:   t.DiffDeletions,
		FilesChanged:    t.FilesChanged,
		CIStatus:        t.CIStatus,
		RelevanceReason: t.RelevanceReason,
		EventType:       t.EventType,
		ScoringStatus:   t.ScoringStatus,
		CreatedAt:       t.CreatedAt.Format(time.RFC3339),
		Status:          t.Status,
		PriorityScore:   t.PriorityScore,
		AISummary:         t.AISummary,
		PriorityReasoning: t.PriorityReasoning,
		AgentConfidence:   t.AgentConfidence,
	}
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	tasks, err := db.QueuedTasks(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var tasks []domain.Task
	var err error
	if status != "" {
		tasks, err = db.TasksByStatus(s.db, status)
	} else {
		tasks, err = db.QueuedTasks(s.db)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	result := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		result[i] = taskToJSON(t)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := db.GetTask(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	writeJSON(w, http.StatusOK, taskToJSON(*task))
}

type swipeRequest struct {
	Action       string `json:"action"`
	HesitationMs int    `json:"hesitation_ms"`
	PromptID     string `json:"prompt_id,omitempty"` // optional: explicit prompt for delegation
}

func (s *Server) handleSwipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req swipeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	switch req.Action {
	case "claim", "dismiss", "snooze", "delegate":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action: must be claim, dismiss, snooze, or delegate"})
		return
	}

	newStatus, err := db.RecordSwipe(s.db, id, req.Action, req.HesitationMs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	response := map[string]any{"status": newStatus}

	// On claim: if Jira task, assign to self and transition to in-progress
	if req.Action == "claim" && s.jiraClient != nil && s.jiraInProgressStatus != "" {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil && task.Source == "jira" {
			go func(issueKey, targetStatus string) {
				if err := s.jiraClient.AssignToSelf(issueKey); err != nil {
					log.Printf("[jira] failed to assign %s: %v", issueKey, err)
					return
				}
				if err := s.jiraClient.TransitionTo(issueKey, targetStatus); err != nil {
					log.Printf("[jira] failed to transition %s to %q: %v", issueKey, targetStatus, err)
				}
			}(task.SourceID, s.jiraInProgressStatus)
		}
	}

	// Trigger delegation on swipe-up
	if req.Action == "delegate" && s.spawner != nil {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil {
			runID, err := s.spawner.Delegate(*task, req.PromptID)
			if err != nil {
				response["delegate_error"] = err.Error()
			} else {
				response["run_id"] = runID
			}
		}
	}

	writeJSON(w, http.StatusOK, response)
}

type snoozeRequest struct {
	Until        string `json:"until"` // RFC3339 or duration like "1h", "4h", "tomorrow"
	HesitationMs int    `json:"hesitation_ms"`
}

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snoozeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	until, err := parseSnoozeUntil(req.Until)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snooze duration: " + err.Error()})
		return
	}

	if err := db.SnoozeTask(s.db, id, until, req.HesitationMs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "snoozed", "until": until.Format(time.RFC3339)})
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Read task before undo so we know the source status to revert to
	task, _ := db.GetTask(s.db, id)

	if err := db.UndoLastSwipe(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Revert Jira ticket: unassign and transition back to original status
	if task != nil && task.Source == "jira" && task.SourceStatus != "" && s.jiraClient != nil {
		go func(issueKey, originalStatus string) {
			if err := s.jiraClient.Unassign(issueKey); err != nil {
				log.Printf("[jira] failed to unassign %s on undo: %v", issueKey, err)
			}
			if err := s.jiraClient.TransitionTo(issueKey, originalStatus); err != nil {
				log.Printf("[jira] failed to transition %s back to %q on undo: %v", issueKey, originalStatus, err)
			}
		}(task.SourceID, task.SourceStatus)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func parseSnoozeUntil(s string) (time.Time, error) {
	now := time.Now()
	switch s {
	case "1h":
		return now.Add(1 * time.Hour), nil
	case "2h":
		return now.Add(2 * time.Hour), nil
	case "4h":
		return now.Add(4 * time.Hour), nil
	case "tomorrow":
		tomorrow := now.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, tomorrow.Location()), nil
	default:
		// Try parsing as RFC3339
		return time.Parse(time.RFC3339, s)
	}
}
