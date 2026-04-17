package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id"`
	Source              string   `json:"source"`      // from entity
	SourceID            string   `json:"source_id"`   // from entity
	SourceURL           string   `json:"source_url"`  // from entity
	Title               string   `json:"title"`       // from entity
	EntityKind          string   `json:"entity_kind"` // "pr" | "issue"
	EventType           string   `json:"event_type"`
	DedupKey            string   `json:"dedup_key,omitempty"`
	Severity            string   `json:"severity,omitempty"`
	RelevanceReason     string   `json:"relevance_reason,omitempty"`
	ScoringStatus       string   `json:"scoring_status"`
	CreatedAt           string   `json:"created_at"`
	Status              string   `json:"status"`
	PriorityScore       *float64 `json:"priority_score"`
	AutonomySuitability *float64 `json:"autonomy_suitability"`
	AISummary           string   `json:"ai_summary,omitempty"`
	PriorityReasoning   string   `json:"priority_reasoning,omitempty"`
	CloseReason         string   `json:"close_reason,omitempty"`
}

func taskToJSON(t domain.Task) taskJSON {
	return taskJSON{
		ID:                  t.ID,
		Source:              t.EntitySource,
		SourceID:            t.EntitySourceID,
		SourceURL:           t.SourceURL,
		Title:               t.Title,
		EntityKind:          t.EntityKind,
		EventType:           t.EventType,
		DedupKey:            t.DedupKey,
		Severity:            t.Severity,
		RelevanceReason:     t.RelevanceReason,
		ScoringStatus:       t.ScoringStatus,
		CreatedAt:           t.CreatedAt.Format(time.RFC3339),
		Status:              t.Status,
		PriorityScore:       t.PriorityScore,
		AutonomySuitability: t.AutonomySuitability,
		AISummary:           t.AISummary,
		PriorityReasoning:   t.PriorityReasoning,
		CloseReason:         t.CloseReason,
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
	PromptID     string `json:"prompt_id,omitempty"`
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
		if err == nil && task != nil && task.EntitySource == "jira" {
			go func(issueKey, targetStatus string) {
				if err := s.jiraClient.AssignToSelf(issueKey); err != nil {
					log.Printf("[jira] failed to assign %s: %v", issueKey, err)
					return
				}
				if err := s.jiraClient.TransitionTo(issueKey, targetStatus); err != nil {
					log.Printf("[jira] failed to transition %s to %q: %v", issueKey, targetStatus, err)
				}
			}(task.EntitySourceID, s.jiraInProgressStatus)
		}
	}

	// Trigger delegation on swipe-up
	if req.Action == "delegate" && s.spawner != nil {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil {
			runID, err := s.spawner.Delegate(*task, req.PromptID, "manual", "")
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
	Until        string `json:"until"`
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

	task, _ := db.GetTask(s.db, id)

	if err := db.UndoLastSwipe(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Revert Jira ticket: unassign and transition back to original status
	if task != nil && task.EntitySource == "jira" && task.SourceStatus != "" && s.jiraClient != nil {
		go func(issueKey, originalStatus string) {
			if err := s.jiraClient.Unassign(issueKey); err != nil {
				log.Printf("[jira] failed to unassign %s on undo: %v", issueKey, err)
			}
			if err := s.jiraClient.TransitionTo(issueKey, originalStatus); err != nil {
				log.Printf("[jira] failed to transition %s back to %q on undo: %v", issueKey, originalStatus, err)
			}
		}(task.EntitySourceID, task.SourceStatus)
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
		return time.Parse(time.RFC3339, s)
	}
}
