package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// stockTicket is the per-row payload for the carry-over list.
type stockTicket struct {
	IssueKey  string `json:"issue_key"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	Project   string `json:"project"`
	IssueType string `json:"issue_type"`
	Priority  string `json:"priority"`
	ParentKey string `json:"parent_key,omitempty"`
	ParentURL string `json:"parent_url,omitempty"`
	URL       string `json:"url"`
}

// handleJiraStockGet lists non-terminal Jira tickets assigned to the user
// that don't already have an active task. Returns {status: "polling"} if the
// Jira poller hasn't completed its first cycle since the last config change —
// carry-over reads snapshots, which are seeded only after a full poll runs.
func (s *Server) handleJiraStockGet(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load()
	if creds.JiraPAT == "" || creds.JiraDisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	if !s.jiraPollReady() {
		writeJSON(w, http.StatusOK, map[string]any{"status": "polling"})
		return
	}

	cfg, _ := config.Load()

	entities, err := db.ListActiveEntities(s.db, "jira")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list entities: " + err.Error()})
		return
	}

	tickets := make([]stockTicket, 0, len(entities))
	for _, e := range entities {
		active, _ := db.FindActiveTasksByEntity(s.db, e.ID)
		if len(active) > 0 {
			continue
		}

		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			continue
		}

		if snap.Assignee != creds.JiraDisplayName {
			continue
		}
		if isJiraStatusTerminal(snap.Status) {
			continue
		}

		var parentURL string
		if snap.ParentKey != "" && cfg.Jira.BaseURL != "" {
			parentURL = strings.TrimRight(cfg.Jira.BaseURL, "/") + "/browse/" + snap.ParentKey
		}

		tickets = append(tickets, stockTicket{
			IssueKey:  snap.Key,
			Summary:   snap.Summary,
			Status:    snap.Status,
			Project:   projectFromKey(snap.Key),
			IssueType: snap.IssueType,
			Priority:  snap.Priority,
			ParentKey: snap.ParentKey,
			ParentURL: parentURL,
			URL:       snap.URL,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ready",
		"tickets": tickets,
	})
}

type stockAction struct {
	IssueKey string `json:"issue_key"`
	Action   string `json:"action"` // "queue" | "claim" | "done"
}

type stockFailure struct {
	IssueKey string `json:"issue_key"`
	Action   string `json:"action"`
	Error    string `json:"error"`
}

// handleJiraStockPost applies carry-over actions. "queue" creates a queued
// task, "claim" creates a claimed task + assigns and transitions the Jira
// issue to in-progress, "done" transitions the Jira issue to done and closes
// the entity. Transition failures are surfaced per-row; other actions still
// apply.
func (s *Server) handleJiraStockPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Actions []stockAction `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load()
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	client := jira.NewClient(creds.JiraURL, creds.JiraPAT)

	applied := 0
	failed := make([]stockFailure, 0)

	for _, a := range req.Actions {
		if a.Action != "queue" && a.Action != "claim" && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "unknown action"})
			continue
		}

		entity, err := db.GetEntityBySource(s.db, "jira", a.IssueKey)
		if err != nil || entity == nil {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "entity not found"})
			continue
		}

		switch a.Action {
		case "queue":
			if _, _, err := db.FindOrCreateTask(s.db, entity.ID, domain.EventJiraIssueAssigned, "", "", 0.5); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}

		case "claim":
			if cfg.Jira.InProgressStatus == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "in_progress_status not configured"})
				continue
			}
			task, _, err := db.FindOrCreateTask(s.db, entity.ID, domain.EventJiraIssueAssigned, "", "", 0.5)
			if err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			if err := db.SetTaskStatus(s.db, task.ID, "claimed"); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}

			// Jira mutations — mirror the handleSwipe claim guard pattern so we
			// don't re-assign/re-transition when the state is already correct.
			state := client.GetClaimState(a.IssueKey)
			if state == nil || !state.AssignedToSelf {
				if err := client.AssignToSelf(a.IssueKey); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "assign: " + err.Error()})
					continue
				}
			}
			if state == nil || state.StatusName != cfg.Jira.InProgressStatus {
				if err := client.TransitionTo(a.IssueKey, cfg.Jira.InProgressStatus); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
			}

		case "done":
			if cfg.Jira.DoneStatus == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "done_status not configured"})
				continue
			}
			state := client.GetClaimState(a.IssueKey)
			if state == nil || state.StatusName != cfg.Jira.DoneStatus {
				if err := client.TransitionTo(a.IssueKey, cfg.Jira.DoneStatus); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
			}
			if err := db.MarkEntityClosed(s.db, entity.ID); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
		}

		applied++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"failed":  failed,
	})
}

// isJiraStatusTerminal mirrors the terminal-status set used by the tracker.
// Kept local to avoid exporting the tracker helper.
func isJiraStatusTerminal(status string) bool {
	return status == "Done" || status == "Closed" || status == "Resolved"
}

// projectFromKey pulls "SKY" out of "SKY-123". Mirrors tracker.extractProject.
func projectFromKey(key string) string {
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}
