package server

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
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
	// AlreadyDone is true when snap.Status already matches the user's configured
	// DoneStatus. Frontend pre-selects the "done" action so the user can close
	// orphan entities with one click — the POST handler's existing no-op guard
	// skips the Jira transition when the status is already correct.
	AlreadyDone bool `json:"already_done,omitempty"`
}

// handleJiraStockGet lists non-terminal Jira tickets assigned to the user
// that don't already have an active task. Returns {status: "polling"} if the
// Jira poller hasn't completed its first cycle since the last config change —
// carry-over reads snapshots, which are seeded only after a full poll runs.
func (s *Server) handleJiraStockGet(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load()
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}

	// Require full Jira configuration (PAT + URL + at least one project) plus
	// a stored display name so we can match the assignee field. Partial config
	// would silently stall on "polling" forever because the poller never has
	// anything to do.
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) || creds.JiraDisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	if !s.jiraPollReady() {
		writeJSON(w, http.StatusOK, map[string]any{"status": "polling"})
		return
	}

	entities, err := db.ListActiveEntities(s.db, "jira")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list entities: " + err.Error()})
		return
	}

	// Batch-fetch the set of Jira entity IDs that already have an active task
	// so we don't run N queries inside the loop. If this fails we can't tell
	// which entities are safe to show, so fail the request outright.
	taskedEntityIDs, err := db.EntityIDsWithActiveTasks(s.db, "jira")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check active tasks: " + err.Error()})
		return
	}

	tickets := make([]stockTicket, 0, len(entities))
	for _, e := range entities {
		if _, hasTask := taskedEntityIDs[e.ID]; hasTask {
			continue
		}

		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			// Corrupt snapshot — skip this row but log so the state doesn't
			// silently hide a stuck entity. The tracker will reseed on its
			// next poll via UpdateEntitySnapshot.
			log.Printf("[stock] skipping entity %s (%s): invalid snapshot: %v", e.ID, e.SourceID, err)
			continue
		}

		if snap.Assignee != creds.JiraDisplayName {
			continue
		}
		// Skip well-known terminal statuses (Done/Closed/Resolved) — these
		// get MarkEntityClosed at tracker-discovery time, so seeing them here
		// is usually impossible, but guard anyway for safety.
		if isJiraStatusTerminal(snap.Status) {
			continue
		}
		// Include tickets already in the user's configured DoneStatus with a
		// flag so the frontend can pre-select "done" — lets the user close
		// orphan entities without side-effects on Jira (the POST no-op guard
		// skips the transition when status already matches).
		alreadyDone := cfg.Jira.DoneStatus != "" && snap.Status == cfg.Jira.DoneStatus

		var parentURL string
		if snap.ParentKey != "" && cfg.Jira.BaseURL != "" {
			parentURL = strings.TrimRight(cfg.Jira.BaseURL, "/") + "/browse/" + snap.ParentKey
		}

		tickets = append(tickets, stockTicket{
			IssueKey:    snap.Key,
			Summary:     snap.Summary,
			Status:      snap.Status,
			Project:     projectFromKey(snap.Key),
			IssueType:   snap.IssueType,
			Priority:    snap.Priority,
			ParentKey:   snap.ParentKey,
			ParentURL:   parentURL,
			URL:         snap.URL,
			AlreadyDone: alreadyDone,
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
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) || creds.JiraDisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	// Batch-fetch the set of Jira entities that already have an active task so
	// eligibility checks run in O(1) per action. Fail the request if this
	// fails — otherwise we'd act on tickets without knowing whether they're
	// already being tracked.
	taskedEntityIDs, err := db.EntityIDsWithActiveTasks(s.db, "jira")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check active tasks: " + err.Error()})
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

		issueKey := strings.TrimSpace(a.IssueKey)
		if issueKey == "" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "missing issue_key"})
			continue
		}

		entity, err := db.GetEntityBySource(s.db, "jira", issueKey)
		if err != nil {
			failed = append(failed, stockFailure{issueKey, a.Action, "failed to load entity"})
			continue
		}
		if entity == nil {
			failed = append(failed, stockFailure{issueKey, a.Action, "entity not found"})
			continue
		}

		// Enforce the same eligibility rules as the GET list. Prevents acting
		// on tickets that shouldn't be in carry-over at all — stale frontend
		// state, tampered requests, or tickets that changed since GET.
		if entity.SnapshotJSON == "" || entity.SnapshotJSON == "{}" {
			failed = append(failed, stockFailure{issueKey, a.Action, "no snapshot yet"})
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "invalid snapshot"})
			continue
		}
		if snap.Assignee != creds.JiraDisplayName {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "not assigned to you"})
			continue
		}
		// Hard-terminal statuses (Done/Closed/Resolved) should never reach here
		// because the tracker marks those entities closed at discovery, but
		// guard anyway. Tickets already in the user's DoneStatus ARE allowed
		// through — the GET flags them as already_done so the user can close
		// the orphan entity, and the "done" branch below no-ops the Jira
		// transition when the status already matches.
		if isJiraStatusTerminal(snap.Status) {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is already terminal"})
			continue
		}
		if _, hasTask := taskedEntityIDs[entity.ID]; hasTask {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket already has an active task"})
			continue
		}

		switch a.Action {
		case "queue":
			eventID, err := recordCarryOverAssignedEvent(s.db, entity.ID, snap, creds.JiraDisplayName)
			if err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "record event: " + err.Error()})
				continue
			}
			if _, _, err := db.FindOrCreateTask(s.db, entity.ID, domain.EventJiraIssueAssigned, "", eventID, 0.5); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}

		case "claim":
			if cfg.Jira.InProgressStatus == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "in_progress_status not configured"})
				continue
			}

			// Do Jira mutations first: if Jira fails we bail before touching
			// the task table, so there's no claimed-task orphan pointing at a
			// Jira issue that never got assigned or transitioned. Claim-guard
			// pattern skips the API calls when state is already correct.
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

			// Refresh the snap with the known post-mutation state so the
			// synthesized event metadata matches the ticket's actual Jira
			// state at the moment of claim. Otherwise predicates that filter
			// on status would see stale pre-claim values.
			snap.Assignee = creds.JiraDisplayName
			snap.Status = cfg.Jira.InProgressStatus

			// Jira is now consistent — create the task and promote to claimed.
			// If either of these fails the ticket stays assigned + in-progress
			// in Jira but has no task on our side; the next carry-over run
			// would surface it again for retry.
			eventID, err := recordCarryOverAssignedEvent(s.db, entity.ID, snap, creds.JiraDisplayName)
			if err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "record event: " + err.Error()})
				continue
			}
			task, _, err := db.FindOrCreateTask(s.db, entity.ID, domain.EventJiraIssueAssigned, "", eventID, 0.5)
			if err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			if err := db.SetTaskStatus(s.db, task.ID, "claimed"); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
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

// recordCarryOverAssignedEvent writes a synthesized jira:issue:assigned event
// for a carry-over ticket and returns the event ID. Tasks require a non-null
// primary_event_id FK to events.id, but carry-over has no upstream event —
// the tracker seeded the snapshot silently on first poll per the "no events
// on initial load" rule. Semantically this matches what would have fired if
// the ticket had been assigned after we started watching. Uses RecordEvent
// (not bus.Publish) so downstream handlers don't double-create a task.
func recordCarryOverAssignedEvent(database *sql.DB, entityID string, snap domain.JiraSnapshot, displayName string) (string, error) {
	meta := events.JiraIssueAssignedMetadata{
		Assignee:       snap.Assignee,
		AssigneeIsSelf: snap.Assignee == displayName,
		IssueKey:       snap.Key,
		Project:        projectFromKey(snap.Key),
		IssueType:      snap.IssueType,
		Priority:       snap.Priority,
		Status:         snap.Status,
		Summary:        snap.Summary,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	eid := entityID
	return db.RecordEvent(database, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventJiraIssueAssigned,
		MetadataJSON: string(metaJSON),
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
