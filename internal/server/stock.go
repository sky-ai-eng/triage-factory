package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// stockTicket is the per-row payload for the carry-over list. Bucket +
// PrefilledAction let the frontend render two sections ("Your tickets" /
// "Available to claim") and seed the tri-selector with a sensible default
// based on the ticket's current Jira status.
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
	// Bucket is "assigned" (assigned to the user) or "available" (unassigned
	// in a Pickup-rule status). Frontend splits the list on this field.
	Bucket string `json:"bucket"`
	// PrefilledAction is "queue" | "claim" | "done" | "". Empty means the
	// user must choose — we couldn't infer a sensible default from the
	// current Jira status (e.g. an assigned ticket in a status that matches
	// none of the configured Pickup/InProgress/Done rules, or any ticket in
	// the available bucket).
	PrefilledAction string `json:"prefilled_action,omitempty"`
}

// handleJiraStockGet returns two carry-over buckets:
//
//   - assigned: non-terminal Jira tickets assigned to the user, with a
//     prefilled action derived from the current status (Pickup → queue,
//     InProgress → claim, Done → done; unmapped statuses → no prefill).
//   - available: unassigned tickets currently in a Pickup-rule status —
//     new work the user could grab.
//
// Tickets without snapshots yet, tickets with active tasks, and parents
// with open subtasks (SKY-173) are skipped. Returns {status: "polling"}
// while the Jira poller hasn't completed its first cycle since the last
// config change — snapshots are seeded on first poll.
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

	type scored struct {
		ticket    stockTicket
		createdAt string // ISO-8601 from snap.CreatedAt; empty for old snapshots
		fallback  string // entity.CreatedAt as RFC3339 — sort key when snap.CreatedAt is empty
	}

	var assigned, available []scored
	for _, e := range entities {
		if _, hasTask := taskedEntityIDs[e.ID]; hasTask {
			continue
		}
		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			log.Printf("[stock] skipping entity %s (%s): invalid snapshot: %v", e.ID, e.SourceID, err)
			continue
		}
		// Subtask gate (SKY-173) applies to both buckets — a parent ticket
		// with open subtasks is a container, not a work unit. Its subtasks
		// (if assigned or available) surface on their own; if the
		// decomposition later collapses, became_atomic routes the parent
		// through the normal path.
		if snap.OpenSubtaskCount > 0 {
			continue
		}

		var parentURL string
		if snap.ParentKey != "" && cfg.Jira.BaseURL != "" {
			parentURL = strings.TrimRight(cfg.Jira.BaseURL, "/") + "/browse/" + snap.ParentKey
		}

		baseTicket := stockTicket{
			IssueKey:  snap.Key,
			Summary:   snap.Summary,
			Status:    snap.Status,
			Project:   projectFromKey(snap.Key),
			IssueType: snap.IssueType,
			Priority:  snap.Priority,
			ParentKey: snap.ParentKey,
			ParentURL: parentURL,
			URL:       snap.URL,
		}

		isSelf := snap.Assignee == creds.JiraDisplayName
		isUnassigned := snap.Assignee == ""

		switch {
		case isSelf:
			baseTicket.Bucket = "assigned"
			baseTicket.PrefilledAction = prefillForAssigned(cfg.Jira, snap.Status)
			assigned = append(assigned, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		case isUnassigned && cfg.Jira.Pickup.Contains(snap.Status):
			baseTicket.Bucket = "available"
			baseTicket.PrefilledAction = "" // user decides
			available = append(available, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		default:
			// Assigned to someone else, or unassigned but not in Pickup
			// (in-progress orphan, stale Done) — no action in carry-over.
			continue
		}
	}

	// Newest-first within each bucket. Primary key is snap.CreatedAt (Jira's
	// own creation timestamp); when a snapshot predates this field (zero
	// value) we fall back to the entity's TF-side created_at so ordering
	// degrades gracefully instead of jumping to top/bottom.
	// Times are parsed to time.Time before comparison so timezone/offset
	// format differences (e.g. "+0000" vs "-07:00") don't corrupt ordering.
	byNewest := func(list []scored) {
		sort.SliceStable(list, func(i, j int) bool {
			iKey := list[i].createdAt
			if iKey == "" {
				iKey = list[i].fallback
			}
			jKey := list[j].createdAt
			if jKey == "" {
				jKey = list[j].fallback
			}
			it, iOK := parseTime(iKey)
			jt, jOK := parseTime(jKey)
			if iOK && jOK {
				return it.After(jt)
			}
			return iKey > jKey
		})
	}
	byNewest(assigned)
	byNewest(available)

	assignedOut := make([]stockTicket, len(assigned))
	for i, s := range assigned {
		assignedOut[i] = s.ticket
	}
	availableOut := make([]stockTicket, len(available))
	for i, s := range available {
		availableOut[i] = s.ticket
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ready",
		"assigned":  assignedOut,
		"available": availableOut,
	})
}

// prefillForAssigned returns the carry-over action that matches the ticket's
// current Jira status, or "" if none of the configured status rules apply.
// Done.Contains takes precedence over InProgress (a ticket in a Done-rule
// status should always be offered for closure, even if the user has
// overlapping rule membership). Pickup is checked last so the "new work"
// case is the default for simply-assigned-to-you tickets.
func prefillForAssigned(cfg config.JiraConfig, status string) string {
	switch {
	case cfg.Done.Contains(status):
		return "done"
	case cfg.InProgress.Contains(status):
		return "claim"
	case cfg.Pickup.Contains(status):
		return "queue"
	default:
		return ""
	}
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

// handleJiraStockPost applies carry-over actions. Eligibility varies by
// bucket:
//
//   - Assigned (snap.Assignee == self): queue/claim/done are all valid.
//     queue emits jira:issue:assigned (no Jira mutation). claim emits
//     jira:issue:assigned + assigns-to-self + transitions to InProgress.
//     done transitions to Done + closes the entity; a no-op guard skips
//     the transition when already in a Done-member status.
//
//   - Available (unassigned, Pickup status): queue emits jira:issue:available
//     (no Jira mutation — user is parking it in the queue to decide later).
//     claim behaves like the assigned-claim path (assign + transition +
//     claimed task). done is rejected — closing an unassigned ticket from
//     here is not a supported cleanup action.
//
// Transition failures are surfaced per-row; other actions still apply.
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
	queued := 0 // number of queue actions applied — gates the scorer trigger
	claimed := 0
	closed := 0
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

		isSelf := snap.Assignee == creds.JiraDisplayName
		isUnassigned := snap.Assignee == ""
		isAvailable := isUnassigned && cfg.Jira.Pickup.Contains(snap.Status)

		if !isSelf && !isAvailable {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is not assigned to you and not in the available pickup queue"})
			continue
		}

		// Defensive subtask gate (SKY-173 principle): queue/claim on a parent
		// with open subtasks would create the exact non-atomic task the main
		// flow works hard to suppress. The GET handler already filters these
		// out so legitimate UI flows never submit them, but subtasks could be
		// added between GET and POST, or the request could come from a stale
		// frontend. "done" is still allowed on the assigned branch — closing
		// a parent with dangling subtasks is a valid cleanup action.
		if snap.OpenSubtaskCount > 0 && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket has open subtasks — delegate those atomic subtasks directly rather than the parent"})
			continue
		}

		// Available-bucket branches never make sense for "done" — closing an
		// unassigned ticket from carry-over isn't a supported cleanup (the
		// "done" flow is for orphan cleanup on your own assigned tickets that
		// are already in a Done-rule status).
		if isAvailable && a.Action == "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is not assigned to you; done is only for cleaning up your own already-complete tickets"})
			continue
		}

		// Assigned-bucket: tickets in Done.Members are allowed through for
		// the "done" action (no-op guard skips the Jira transition when the
		// status is already a Done member); queue/claim on an already-done
		// ticket is pointless, so reject those outright.
		if isSelf && cfg.Jira.Done.Contains(snap.Status) && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is already in a done status — only the done action is valid"})
			continue
		}

		if _, hasTask := taskedEntityIDs[entity.ID]; hasTask {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket already has an active task"})
			continue
		}

		switch a.Action {
		case "queue":
			// Available tickets synthesize jira:issue:available (they're
			// unassigned — a synthesized jira:issue:assigned would be a
			// lie). Assigned tickets use jira:issue:assigned as before.
			var eventType string
			var eventID string
			if isAvailable {
				eventType = domain.EventJiraIssueAvailable
				eventID, err = recordCarryOverAvailableEvent(s.db, entity.ID, snap, creds.JiraDisplayName)
			} else {
				eventType = domain.EventJiraIssueAssigned
				eventID, err = recordCarryOverAssignedEvent(s.db, entity.ID, snap, creds.JiraDisplayName)
			}
			if err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "record event: " + err.Error()})
				continue
			}
			if _, _, err := db.FindOrCreateTask(s.db, entity.ID, eventType, "", eventID, 0.5); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			queued++

		case "claim":
			if cfg.Jira.InProgress.Canonical == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "in_progress canonical status not configured"})
				continue
			}

			// Do Jira mutations first: if Jira fails we bail before touching
			// the task table, so there's no claimed-task orphan pointing at a
			// Jira issue that never got assigned or transitioned. Claim-guard
			// pattern skips the API calls when state is already correct —
			// containment against InProgress.Members so a ticket in any
			// in-progress variant isn't transitioned back to canonical. For
			// available tickets the state check is a no-op (they're
			// unassigned by definition), but GetClaimState is cheap and keeps
			// one code path for both branches.
			state := client.GetClaimState(a.IssueKey)
			if state == nil || !state.AssignedToSelf {
				if err := client.AssignToSelf(a.IssueKey); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "assign: " + err.Error()})
					continue
				}
			}
			if state == nil || !cfg.Jira.InProgress.Contains(state.StatusName) {
				if err := client.TransitionTo(a.IssueKey, cfg.Jira.InProgress.Canonical); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
				snap.Status = cfg.Jira.InProgress.Canonical
			} else {
				snap.Status = state.StatusName
			}

			// Refresh the snap with the known post-mutation state so the
			// synthesized event metadata matches the ticket's actual Jira
			// state at the moment of claim. The assignee flips to self
			// regardless of where we started.
			snap.Assignee = creds.JiraDisplayName

			// Both assigned and available claim paths end with a
			// jira:issue:assigned event — after the AssignToSelf call, the
			// user is the assignee in Jira too, so the event metadata is
			// accurate for either starting state.
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
			claimed++

		case "done":
			if cfg.Jira.Done.Canonical == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "done canonical status not configured"})
				continue
			}
			// Skip the transition when the ticket is already in any Done
			// member (not just the canonical) — a ticket in "Verified" when
			// Done.Members=[Resolved,Verified] is already done from TF's
			// perspective; transitioning to Resolved would be a no-op at best
			// and a workflow violation at worst.
			state := client.GetClaimState(a.IssueKey)
			if state == nil || !cfg.Jira.Done.Contains(state.StatusName) {
				if err := client.TransitionTo(a.IssueKey, cfg.Jira.Done.Canonical); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
			}
			if err := db.MarkEntityClosed(s.db, entity.ID); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			closed++
		}

		applied++
	}

	// Carry-over creates tasks without going through the poller, so no
	// system:poll:completed fires to wake the scorer via its event-bus
	// subscription. Poke it directly, but only when we actually produced
	// queued tasks — claim promotes straight to 'claimed' (skipped by the
	// UnscoredTasks query) and done doesn't create a task at all, so those
	// two branches have nothing for the scorer to pick up.
	if queued > 0 && s.scorerTrigger != nil {
		s.scorerTrigger()
	}

	// Success toast with the per-action breakdown when at least one ticket
	// applied cleanly. The frontend also shows a partial-failure warning toast
	// if there are any failures; this one only fires on at-least-one-success.
	if applied > 0 {
		toast.Success(s.ws, fmt.Sprintf(
			"Carry-over applied: %d queued, %d claimed, %d closed", queued, claimed, closed,
		))
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

// recordCarryOverAvailableEvent is the available-bucket analogue of
// recordCarryOverAssignedEvent: synthesizes a jira:issue:available event so
// the carry-over "queue" action on an unassigned ticket has a real event
// row to hang the task off. Mirrors the tracker's own emission path for
// first-discovered available tickets (diff.go).
func recordCarryOverAvailableEvent(database *sql.DB, entityID string, snap domain.JiraSnapshot, _ string) (string, error) {
	meta := events.JiraIssueAvailableMetadata{
		IssueKey:  snap.Key,
		Project:   projectFromKey(snap.Key),
		IssueType: snap.IssueType,
		Priority:  snap.Priority,
		Status:    snap.Status,
		Summary:   snap.Summary,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	eid := entityID
	return db.RecordEvent(database, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventJiraIssueAvailable,
		MetadataJSON: string(metaJSON),
	})
}

// projectFromKey pulls "SKY" out of "SKY-123". Mirrors tracker.extractProject.
func projectFromKey(key string) string {
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}

// parseTime parses a timestamp string produced by Jira or TF's own
// entity.CreatedAt. Jira's format omits the colon in the UTC offset
// (e.g. "+0000"), which RFC3339 rejects; we try that layout before the
// standard ones so the common case succeeds on the first attempt.
func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
