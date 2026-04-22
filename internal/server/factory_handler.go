package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// factoryEntityLimit caps how many active entities we ship per snapshot.
// The factory view renders each entity as an item on the belt network;
// hundreds of items would swamp the canvas visually and the belt pathing
// hops would start to stutter. 100 is chosen to match the "floor feels
// populated" target discussed during the factory design session.
const factoryEntityLimit = 100

// factoryStationJSON is the per-event-type payload for the factory view.
// The frontend keys StationDetailOverlay data off event_type; any event
// type with zero activity in the window is omitted.
type factoryStationJSON struct {
	EventType    string           `json:"event_type"`
	Items24h     int              `json:"items_24h"`
	Triggered24h int              `json:"triggered_24h"`
	ActiveRuns   int              `json:"active_runs"`
	Runs         []factoryRunJSON `json:"runs"`
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
}

type factorySnapshotJSON struct {
	Stations map[string]factoryStationJSON `json:"stations"`
	Entities []factoryEntityJSON           `json:"entities"`
}

// handleFactorySnapshot bundles station throughput, active runs, and
// active entities into a single payload for the /factory view. All data
// derived from existing persistence — no new event stream, no state
// projection — so repeated calls are cheap and idempotent.
func (s *Server) handleFactorySnapshot(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)

	// Session user's GitHub login drives the "mine" flag. Missing creds
	// (fresh install, no github configured) degrade to everyone-is-other
	// rather than failing the whole endpoint — the factory should still
	// render for a user who's only set up Jira.
	ghUsername := ""
	if creds, err := auth.Load(); err == nil {
		ghUsername = creds.GitHubUsername
	}

	// --- Throughput counters ------------------------------------------------
	eventCounts, err := db.EventCountsByTypeSince(s.db, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	taskCounts, err := db.TaskCountsByEventTypeSince(s.db, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// --- Active runs --------------------------------------------------------
	activeRuns, err := db.ListFactoryActiveRuns(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	stations := map[string]factoryStationJSON{}
	// Seed stations with counters first so event types with activity but no
	// active run still show up in the throughput strip.
	for eventType, count := range eventCounts {
		st := stations[eventType]
		st.EventType = eventType
		st.Items24h = count
		stations[eventType] = st
	}
	for eventType, count := range taskCounts {
		st := stations[eventType]
		st.EventType = eventType
		st.Triggered24h = count
		stations[eventType] = st
	}

	// Join active runs onto stations. Each active run also needs to know the
	// entity's author so "mine" tint is accurate — pre-fetch those entities.
	runAuthors := map[string]string{}
	for _, ar := range activeRuns {
		if _, seen := runAuthors[ar.Task.EntityID]; seen {
			continue
		}
		ent, err := db.GetEntity(s.db, ar.Task.EntityID)
		if err != nil || ent == nil {
			runAuthors[ar.Task.EntityID] = ""
			continue
		}
		runAuthors[ar.Task.EntityID] = extractEntityAuthor(ent)
	}

	for _, ar := range activeRuns {
		st := stations[ar.Task.EventType]
		st.EventType = ar.Task.EventType
		st.ActiveRuns++
		st.Runs = append(st.Runs, factoryRunJSON{
			Run:  toFactoryRunSummary(ar.Run),
			Task: taskToJSON(ar.Task),
			Mine: ghUsername != "" && runAuthors[ar.Task.EntityID] == ghUsername,
		})
		stations[ar.Task.EventType] = st
	}

	// --- Active entities ----------------------------------------------------
	entityRows, err := db.ListFactoryEntities(s.db, factoryEntityLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
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
			// Jira "mine" = assigned to the session user. We don't store the
			// Jira display name next to github username, but the keychain's
			// JiraDisplayName is available via auth.Load() — keep this empty
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
