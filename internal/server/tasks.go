package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id"`
	EntityID            string   `json:"entity_id"`   // FK to entities.id — lets callers correlate tasks back to their entity
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
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal (SKY-173). Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
}

func taskToJSON(t domain.Task) taskJSON {
	return taskJSON{
		ID:                  t.ID,
		EntityID:            t.EntityID,
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
		OpenSubtaskCount:    t.OpenSubtaskCount,
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

	// Dismiss is a terminal state — if the user swipes away a task mid-run
	// (rare, but possible on a delegated card via the Board gesture rather
	// than the AgentCard cancel button), the run must stop. Mirrors the
	// inline-close and entity-close cascades: task state is authoritative;
	// runs follow.
	if req.Action == "dismiss" && s.spawner != nil {
		ids, err := db.ActiveRunIDsForTask(s.db, id)
		if err != nil {
			log.Printf("[swipe] active-run lookup for task %s failed: %v", id, err)
		} else {
			for _, runID := range ids {
				if err := s.spawner.Cancel(runID); err != nil {
					log.Printf("[swipe] cancel run %s on dismiss of task %s: %v", runID, id, err)
				}
			}
		}
	}

	response := map[string]any{"status": newStatus}

	// On claim: if Jira task, assign to self and transition to in-progress.
	// Claim guard: with multiple tasks per entity, a second claim on the same
	// Jira issue would re-assign + re-transition redundantly (and probably
	// error). Skip the transition when the ticket is already in ANY member of
	// the in-progress rule — if the user (or an earlier claim) moved it to
	// "In Review" while canonical is "In Progress", transitioning back to the
	// canonical would be a spurious status change that would confuse watchers.
	rule := s.jiraInProgressRule
	if req.Action == "claim" && s.jiraClient != nil && rule.Canonical != "" {
		task, err := db.GetTask(s.db, id)
		if err == nil && task != nil && task.EntitySource == "jira" {
			go func(issueKey string, rule config.JiraStatusRule) {
				state := s.jiraClient.GetClaimState(issueKey)

				needAssign := state == nil || !state.AssignedToSelf
				needTransition := state == nil || !rule.Contains(state.StatusName)

				if !needAssign && !needTransition {
					log.Printf("[jira] claim guard: %s already assigned to self and already in in-progress (%q), skipping", issueKey, state.StatusName)
					return
				}

				if needAssign {
					if err := s.jiraClient.AssignToSelf(issueKey); err != nil {
						log.Printf("[jira] failed to assign %s: %v", issueKey, err)
						return
					}
				}
				if needTransition {
					if err := s.jiraClient.TransitionTo(issueKey, rule.Canonical); err != nil {
						log.Printf("[jira] failed to transition %s to %q: %v", issueKey, rule.Canonical, err)
					}
				}
			}(task.EntitySourceID, rule)
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

	// Revert Jira ticket: unassign and transition back to original status.
	// Undo guard: if someone else reassigned the issue or the status diverged
	// from what we stored, don't step on their manual changes.
	if task != nil && task.EntitySource == "jira" && task.SourceStatus != "" && s.jiraClient != nil {
		go func(issueKey, originalStatus string) {
			state := s.jiraClient.GetClaimState(issueKey)

			// Three assignee cases:
			//   - assigned to someone else -> skip undo entirely (manual reassignment)
			//   - unassigned -> skip Unassign (already unassigned), still transition
			//   - assigned to self -> proceed normally (unassign + transition)
			if state != nil && !state.AssignedToSelf && !state.Unassigned {
				log.Printf("[jira] undo guard: %s reassigned to someone else, skipping undo", issueKey)
				return
			}
			// Skip undo if the ticket has moved out of the in-progress rule
			// entirely — that means someone progressed it (to done, back to
			// pickup, etc.) and we shouldn't yank it back. Membership rather
			// than strict-canonical match, because a user moving Claim →
			// "In Review" is still "working on it on my plate" and undo should
			// still unwind to the original status.
			if state != nil && len(s.jiraInProgressRule.Members) > 0 && !s.jiraInProgressRule.Contains(state.StatusName) {
				log.Printf("[jira] undo guard: %s status is %q (not in in-progress members %v), skipping undo", issueKey, state.StatusName, s.jiraInProgressRule.Members)
				return
			}

			if state == nil || state.AssignedToSelf {
				if err := s.jiraClient.Unassign(issueKey); err != nil {
					log.Printf("[jira] failed to unassign %s on undo: %v", issueKey, err)
				}
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
