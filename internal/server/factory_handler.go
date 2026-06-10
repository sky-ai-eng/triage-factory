package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// factoryHandler serves the factory snapshot endpoint, holding the
// transactional store runner it reads through.
type factoryHandler struct {
	tx db.TxRunner
}

// factoryEntityLimit caps how many active entities we ship per snapshot.
// The factory view renders each entity as an item on the belt network;
// at some point the canvas gets visually swamped, but 500 comfortably
// covers an enterprise Jira stock plus tracked GitHub repos without
// churning the displayed set when pollers from different sources
// alternate their last_polled_at updates.
const factoryEntityLimit = 500

// factoryStationJSON is the per-event-type payload for the factory view.
// The frontend keys StationDetailOverlay data off event_type; any event
// type with zero activity in the window is omitted.
type factoryStationJSON struct {
	EventType    string `json:"event_type"`
	Items24h     int    `json:"items_24h"`
	Triggered24h int    `json:"triggered_24h"`
	ActiveRuns   int    `json:"active_runs"`
	// ItemsLifetime is the from-catalog-start distinct-entity count for
	// this event_type — "PRs that ever reached this station," not
	// "events fired here." Drives the always-on numeric readout on the
	// station's front-face screen.
	//
	// The lifetime merge can introduce stations into the snapshot that
	// have no 24h activity and no active runs (a Merged station weeks
	// after the last release still has a lifetime count). That's
	// intentional: those stations need to render their counter even
	// when otherwise quiet. The frontend ignores any event_type it
	// doesn't have a station for, so the wider keyset is harmless;
	// system events stay out because the aggregate filters on
	// entity_id IS NOT NULL.
	ItemsLifetime int              `json:"items_lifetime"`
	Runs          []factoryRunJSON `json:"runs"`
}

type factoryRunJSON struct {
	Run  factoryRunSummaryJSON `json:"run"`
	Task taskJSON              `json:"task"`
	Mine bool                  `json:"mine"`
}

// factoryRunSummaryJSON mirrors the AgentCard-expected shape the frontend
// already consumes (see frontend/src/types.ts AgentRun). Field names are
// capitalized to match the struct-tag-free JSON the existing /api/agent/
// runs handler emits for Status/StartedAt/etc.
type factoryRunSummaryJSON struct {
	ID            string     `json:"ID"`
	TaskID        string     `json:"TaskID"`
	PromptID      string     `json:"PromptID"`
	Status        string     `json:"Status"`
	Model         string     `json:"Model"`
	StartedAt     time.Time  `json:"StartedAt"`
	CompletedAt   *time.Time `json:"CompletedAt"`
	TotalCostUSD  *float64   `json:"TotalCostUSD"`
	DurationMs    *int       `json:"DurationMs"`
	NumTurns      *int       `json:"NumTurns"`
	StopReason    string     `json:"StopReason"`
	ResultSummary string     `json:"ResultSummary"`
	SessionID     string     `json:"SessionID"`
	MemoryMissing bool       `json:"MemoryMissing"`
	TriggerType   string     `json:"TriggerType"`
	TriggerID     string     `json:"TriggerID"`
}

func toFactoryRunSummary(r domain.AgentRun) factoryRunSummaryJSON {
	return factoryRunSummaryJSON{
		ID:            r.ID,
		TaskID:        r.TaskID,
		PromptID:      r.PromptID,
		Status:        r.Status,
		Model:         r.Model,
		StartedAt:     r.StartedAt,
		CompletedAt:   r.CompletedAt,
		TotalCostUSD:  r.TotalCostUSD,
		DurationMs:    r.DurationMs,
		NumTurns:      r.NumTurns,
		StopReason:    r.StopReason,
		ResultSummary: r.ResultSummary,
		SessionID:     r.SessionID,
		MemoryMissing: r.MemoryMissing,
		TriggerType:   r.TriggerType,
		TriggerID:     r.TriggerID,
	}
}

// factoryEntityJSON is the per-entity payload. PR-specific fields (number,
// repo, author, additions, deletions) are populated from snapshot_json for
// github entities; jira entities get status/priority/assignee instead. The
// frontend decides how to render each kind.
type factoryEntityJSON struct {
	ID               string `json:"id"`
	Source           string `json:"source"`
	SourceID         string `json:"source_id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Mine             bool   `json:"mine"`
	CurrentEventType string `json:"current_event_type,omitempty"`
	LastEventAt      string `json:"last_event_at,omitempty"`
	// RecentEvents is the tail of this entity's event history, ordered
	// oldest first. The factory reconciler walks it as an animation chain
	// so a poll that fires two events for the same entity (new_commits →
	// ci_passed) shows both transitions rather than teleporting to the
	// latest. Bounded per-entity by factoryRecentEventsPerEntity.
	RecentEvents []factoryRecentEventJSON `json:"recent_events,omitempty"`

	// GitHub PR fields.
	Number    int    `json:"number,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Author    string `json:"author,omitempty"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`

	// Jira fields.
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`

	// PendingTasks groups active tasks for this entity by event_type.
	// Drives the station drawer's drag-to-delegate flow: the frontend
	// reads the dropped station's first entry and forwards its
	// dedup_key (with entity_id + event_type) to /api/factory/delegate,
	// which find-or-creates via the unique index on (entity_id,
	// event_type, dedup_key). If pending_tasks has no entry for the
	// dropped station's event_type yet, that means there is no existing
	// task for that event_type; the request still carries event_type and
	// the handler creates the task. For a dedup-discriminated event type
	// (label_added, status_changed), the inner slice can have multiple
	// entries; v1 frontend uses the first.
	PendingTasks map[string][]pendingTaskRef `json:"pending_tasks,omitempty"`

	// HasOpenRun is true if any run on this entity is in the `open` state
	// (a turn ended without a conclusion). The runs-tray chip paints an
	// idle badge when this is set so a user scanning the factory can spot
	// open runs at a glance.
	HasOpenRun bool `json:"has_open_run,omitempty"`
}

// pendingTaskRef is the minimal task reference shipped per queued
// entity for the drag-to-delegate flow. dedup_key is what the request
// to /api/factory/delegate carries (the handler keys find-or-create
// on entity_id + event_type + dedup_key). task_id is informational —
// not consumed by the request today — and is kept available for
// future UI hints like "this chip already has a task here."
type pendingTaskRef struct {
	TaskID   string `json:"task_id"`
	DedupKey string `json:"dedup_key"`
}

type factoryRecentEventJSON struct {
	EventType string `json:"event_type"`
	// At is the event's source-time when known (commit committed_at,
	// check-run completed_at, review submitted_at), falling back to
	// detection time. Used for chain ORDER.
	At string `json:"at"`
	// DetectedAt is the row's insert time — when the poller actually
	// recorded the event. Used by the factory animation reconciler
	// for chain CLUSTERING: events from one poll insert within
	// milliseconds of each other, so a small gap test on this field
	// cleanly separates one poll's burst from another regardless of
	// how the upstream timestamps line up.
	DetectedAt string `json:"detected_at"`
}

// factoryRecentEventsPerEntity caps how many events we ship per entity for
// the chain animation. 10 comfortably covers a busy poll cycle's worth
// (PR opened + ready_for_review + several CI checks + a review) without
// bloating the snapshot.
const factoryRecentEventsPerEntity = 10

type factorySnapshotJSON struct {
	Stations map[string]factoryStationJSON `json:"stations"`
	Entities []factoryEntityJSON           `json:"entities"`
}

// handleFactorySnapshot bundles station throughput, active runs, and
// active entities into a single payload for the /factory view. All data
// derived from existing persistence — no new event stream, no state
// projection — so repeated calls are cheap and idempotent.
func (fh *factoryHandler) handleFactorySnapshot(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	since := time.Now().Add(-24 * time.Hour)
	// Optional per-page team filter. Empty = the union of the
	// viewer's teams; a team id narrows the entity belt to that team.
	// Only the belt narrows here — the throughput counters stay at the
	// viewer-union (a deliberate scope line; the belt is what "hides
	// cross-team rows" refers to on the factory).
	teamFilter := teamscope.FilterParam(r)

	// Session user's GitHub login drives the "mine" flag. Identity lives in
	// user_github_identities, host-scoped (SKY-396) — resolve the org's
	// GitHub host from org_settings, then look up (user, host). Missing
	// identity (fresh install, no GitHub configured) degrades to
	// everyone-is-other rather than failing the whole endpoint — the
	// factory should still render for a user who's only set up Jira.
	var ghUsername string
	var eventCounts, taskCounts, lifetimeCounts map[string]int
	var activeRuns []domain.FactoryActiveRun
	var entityRows []domain.FactoryEntityRow
	var recentByEntity map[string][]domain.FactoryRecentEvent
	var pendingTasks []domain.PendingTaskRef
	var openRunsByEntity map[string]struct{}
	runAuthors := map[string]string{}
	if err := fh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, _ := tx.Orgs.GetSettings(r.Context(), orgID)
		ghUsername, _ = tx.Users.GetGitHubLogin(r.Context(), userID, orgSet.GitHubBaseURL)

		// --- Throughput counters --------------------------------------
		var e error
		eventCounts, e = tx.Factory.EventCountsSince(r.Context(), orgID, since)
		if e != nil {
			return e
		}
		taskCounts, e = tx.Factory.TaskCountsSince(r.Context(), orgID, since)
		if e != nil {
			return e
		}
		lifetimeCounts, e = tx.Factory.LifetimeDistinctByEventType(r.Context(), orgID)
		if e != nil {
			return e
		}

		// --- Active runs ----------------------------------------------
		activeRuns, e = tx.Factory.ActiveRuns(r.Context(), orgID)
		if e != nil {
			return e
		}

		// Join active runs onto stations. Each active run also needs to
		// know the entity's author so "mine" tint is accurate — pre-fetch
		// those entities.
		for _, ar := range activeRuns {
			if _, seen := runAuthors[ar.Task.EntityID]; seen {
				continue
			}
			ent, getErr := tx.Entities.Get(r.Context(), orgID, ar.Task.EntityID)
			if getErr != nil || ent == nil {
				runAuthors[ar.Task.EntityID] = ""
				continue
			}
			runAuthors[ar.Task.EntityID] = extractEntityAuthor(ent)
		}

		// --- Active entities ------------------------------------------
		entityRows, e = tx.Factory.Entities(r.Context(), orgID, factoryEntityLimit, teamFilter)
		if e != nil {
			return e
		}

		entityIDs := make([]string, 0, len(entityRows))
		for _, row := range entityRows {
			entityIDs = append(entityIDs, row.Entity.ID)
		}
		recentByEntity, e = tx.Factory.RecentEventsByEntity(r.Context(), orgID, entityIDs, factoryRecentEventsPerEntity)
		if e != nil {
			return e
		}

		pendingTasks, e = tx.Tasks.ListActiveRefsForEntities(r.Context(), orgID, entityIDs, teamFilter)
		if e != nil {
			return e
		}

		openRunsByEntity, e = tx.AgentRuns.EntitiesWithOpenRuns(r.Context(), orgID, entityIDs)
		return e
	}); err != nil {
		internalError(w, "factory", err)
		return
	}

	stations := map[string]factoryStationJSON{}
	// Seed stations with counters first so event types with activity but no
	// active run still show up in the throughput strip. Initialize Runs as
	// an empty slice (not nil) so json.Marshal emits `[]` instead of `null`;
	// the frontend maps over this without a defensive fallback.
	ensureStation := func(eventType string) factoryStationJSON {
		if st, ok := stations[eventType]; ok {
			return st
		}
		return factoryStationJSON{EventType: eventType, Runs: []factoryRunJSON{}}
	}
	for eventType, count := range eventCounts {
		st := ensureStation(eventType)
		st.Items24h = count
		stations[eventType] = st
	}
	for eventType, count := range taskCounts {
		st := ensureStation(eventType)
		st.Triggered24h = count
		stations[eventType] = st
	}
	for eventType, count := range lifetimeCounts {
		st := ensureStation(eventType)
		st.ItemsLifetime = count
		stations[eventType] = st
	}

	for _, ar := range activeRuns {
		st := ensureStation(ar.Task.EventType)
		st.ActiveRuns++
		st.Runs = append(st.Runs, factoryRunJSON{
			Run:  toFactoryRunSummary(ar.Run),
			Task: taskToJSON(ar.Task),
			Mine: ghUsername != "" && runAuthors[ar.Task.EntityID] == ghUsername,
		})
		stations[ar.Task.EventType] = st
	}

	// --- Active entities ----------------------------------------------------
	// Pending tasks per entity, grouped by event_type. Drives the
	// drawer's drag-to-delegate flow. Uses the minimal
	// ListActiveTaskRefsForEntities projection (id + entity_id +
	// event_type + dedup_key) rather than the full Task struct so
	// /api/factory/snapshot doesn't pay for the entity JOIN and the
	// snapshot_json json_extract on every poll.
	pendingByEntity := map[string]map[string][]pendingTaskRef{}
	for _, t := range pendingTasks {
		byType, ok := pendingByEntity[t.EntityID]
		if !ok {
			byType = map[string][]pendingTaskRef{}
			pendingByEntity[t.EntityID] = byType
		}
		byType[t.EventType] = append(byType[t.EventType], pendingTaskRef{
			TaskID:   t.ID,
			DedupKey: t.DedupKey,
		})
	}

	entities := make([]factoryEntityJSON, 0, len(entityRows))
	for _, row := range entityRows {
		ej := factoryEntityJSON{
			ID:               row.Entity.ID,
			Source:           row.Entity.Source,
			SourceID:         row.Entity.SourceID,
			Kind:             row.Entity.Kind,
			Title:            row.Entity.Title,
			URL:              row.Entity.URL,
			CurrentEventType: row.LatestEventType,
		}
		if row.LatestEventAt != nil {
			ej.LastEventAt = row.LatestEventAt.Format(time.RFC3339)
		}
		if recent, ok := recentByEntity[row.Entity.ID]; ok {
			ej.RecentEvents = make([]factoryRecentEventJSON, len(recent))
			for i, r := range recent {
				ej.RecentEvents[i] = factoryRecentEventJSON{
					EventType:  r.EventType,
					At:         r.CreatedAt.Format(time.RFC3339),
					DetectedAt: r.DetectedAt.Format(time.RFC3339),
				}
			}
		}
		if pending, ok := pendingByEntity[row.Entity.ID]; ok {
			ej.PendingTasks = pending
		}
		if _, ok := openRunsByEntity[row.Entity.ID]; ok {
			ej.HasOpenRun = true
		}
		switch row.Entity.Source {
		case "github":
			var snap domain.PRSnapshot
			if row.Entity.SnapshotJSON != "" {
				if err := json.Unmarshal([]byte(row.Entity.SnapshotJSON), &snap); err == nil {
					ej.Number = snap.Number
					ej.Repo = snap.Repo
					ej.Author = snap.Author
					ej.Additions = snap.Additions
					ej.Deletions = snap.Deletions
				} else {
					log.Printf("[factory] entity %s has malformed snapshot_json: %v", row.Entity.ID, err)
				}
			}
			ej.Mine = ghUsername != "" && ej.Author == ghUsername
		case "jira":
			var snap domain.JiraSnapshot
			if row.Entity.SnapshotJSON != "" {
				if err := json.Unmarshal([]byte(row.Entity.SnapshotJSON), &snap); err == nil {
					ej.Status = snap.Status
					ej.Priority = snap.Priority
					ej.Assignee = snap.Assignee
				}
			}
			// Jira "mine" = assigned to the session user. The session user's
			// Jira display name lives in user_jira_identities (read via
			// UsersStore.GetJiraIdentity for the org's host); keep this empty
			// for v1 and let the UI fall back to the other tint.
		}
		entities = append(entities, ej)
	}

	writeJSON(w, http.StatusOK, factorySnapshotJSON{
		Stations: stations,
		Entities: entities,
	})
}

// extractEntityAuthor returns the GitHub author login for PR entities.
// Jira entities return "" — we don't have a reliable author-to-self mapping
// for Jira tickets at this layer (the user's Jira display name lives in a
// different keychain slot). Factory overlay tint for jira runs just
// defaults to "other" until that plumbing is added.
func extractEntityAuthor(e *domain.Entity) string {
	if e.Source != "github" || e.SnapshotJSON == "" {
		return ""
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
		return ""
	}
	return snap.Author
}
