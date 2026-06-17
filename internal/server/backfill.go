package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// backfillHandler serves the project backfill endpoints (candidate listing
// and the backfill action). It holds the transactional store runner and the
// websocket hub for progress broadcasts; routes() registers its methods
// through the api()/apiMutating() wrappers.
type backfillHandler struct {
	tx db.TxRunner
	ws *websocket.Hub
}

// backfillCandidate is the per-row payload returned by
// GET /api/projects/{id}/backfill-candidates. The caller renders these
// as checkboxes in the create-flow popup; current_project_id +
// current_project_name surface so the user knows when they're
// reclaiming an entity from another project.
type backfillCandidate struct {
	ID                 string `json:"id"`
	Source             string `json:"source"`
	SourceID           string `json:"source_id"`
	Kind               string `json:"kind"`
	Title              string `json:"title"`
	URL                string `json:"url"`
	State              string `json:"state"`
	CurrentProjectID   string `json:"current_project_id"`
	CurrentProjectName string `json:"current_project_name"`
}

// handleBackfillCandidates returns the list of non-terminal entities
// that the create-flow popup should show for the given project.
//
// Scope rules:
//   - pinned_repos non-empty → GitHub entities scoped to those repos.
//   - pinned_repos empty → ALL GitHub entities (no filter on that source).
//   - jira_project_key non-empty → Jira entities matching that key.
//   - jira_project_key empty → ALL Jira entities (no filter on that source).
//   - Both empty → every non-terminal entity across sources.
//
// Empty filter == "no filter" rather than "exclude this source." A
// project that scopes only one tracker still wants to see candidates
// from the other source so the user can claim them manually.
//
// Entities already assigned to this project are excluded — there's
// nothing to backfill for them.
func (bf *backfillHandler) handleBackfillCandidates(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID := r.PathValue("id")
	var project *domain.Project
	var out []backfillCandidate
	if err := bf.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil {
			return e
		}
		if project == nil {
			return nil
		}

		var collected []domain.Entity

		github, e := tx.Entities.ListActive(r.Context(), orgID, "github")
		if e != nil {
			return e
		}
		for _, ent := range github {
			if !entityInProjectScope(&ent, project) {
				continue
			}
			collected = append(collected, ent)
		}

		jira, e := tx.Entities.ListActive(r.Context(), orgID, "jira")
		if e != nil {
			return e
		}
		for _, ent := range jira {
			if !entityInProjectScope(&ent, project) {
				continue
			}
			collected = append(collected, ent)
		}

		// Resolve current_project_name once per distinct project_id rather
		// than per row — the same other-project may sponsor many candidates.
		nameCache := map[string]string{}
		out = make([]backfillCandidate, 0, len(collected))
		for _, ent := range collected {
			// Already in this project — no work to do; skip from candidates.
			if ent.ProjectID != nil && *ent.ProjectID == projectID {
				continue
			}
			c := backfillCandidate{
				ID:       ent.ID,
				Source:   ent.Source,
				SourceID: ent.SourceID,
				Kind:     ent.Kind,
				Title:    ent.Title,
				URL:      ent.URL,
				State:    ent.State,
			}
			if ent.ProjectID != nil && *ent.ProjectID != "" {
				c.CurrentProjectID = *ent.ProjectID
				name, ok := nameCache[*ent.ProjectID]
				if !ok {
					if p, perr := tx.Projects.Get(r.Context(), orgID, *ent.ProjectID); perr == nil && p != nil {
						name = p.Name
					}
					nameCache[*ent.ProjectID] = name
				}
				c.CurrentProjectName = name
			}
			out = append(out, c)
		}
		return nil
	}); err != nil {
		backfillLog.Error("load candidates failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load backfill candidates"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

// manualAssignmentMessage is the rationale text stamped on entities
// reclaimed via the project-creation backfill popup. The entities
// panel renders it as-is so the row reads "Manually assigned by
// user." instead of the empty-rationale fallback.
const manualAssignmentMessage = "Manually assigned by user."

type backfillRequest struct {
	EntityIDs []string `json:"entity_ids"`
}

type backfillFailure struct {
	EntityID string `json:"entity_id"`
	Error    string `json:"error"`
}

// handleBackfill bulk-assigns the named entities to the project. Reuses
// EntityStore.AssignProject so each row gets its classified_at stamped —
// popup-claimed entities stay sticky against the auto-classifier.
//
// Partial-success result shape mirrors handleJiraStockPost
// (internal/server/stock.go): per-entity failures are collected into
// `failed: [{entity_id, error}]` and the call returns 200 with the
// applied count rather than failing the whole batch on a single row.
func (bf *backfillHandler) handleBackfill(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID := r.PathValue("id")
	var project *domain.Project
	if err := bf.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	var req backfillRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if len(req.EntityIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"applied": 0, "failed": []backfillFailure{}})
		return
	}

	applied := 0
	var failures []backfillFailure
	var assigned []string
	seen := make(map[string]struct{}, len(req.EntityIDs))
	for _, eid := range req.EntityIDs {
		eid = strings.TrimSpace(eid)
		if eid == "" {
			continue
		}
		if _, ok := seen[eid]; ok {
			continue
		}
		seen[eid] = struct{}{}
		// Re-validate every id server-side. The client built this list
		// from /backfill-candidates, which already filtered, but a
		// stale tab, a tampered request, or a race against entity
		// closure could submit ids that are now ineligible — closed
		// entities, entities outside the project's tracker scope, etc.
		// Without this gate, a malicious client could reassign any
		// entity row by id, and a stale UI could quietly stamp
		// classified_at on closed work.
		var failure *backfillFailure
		txErr := bf.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			entity, lookupErr := tx.Entities.Get(r.Context(), orgID, eid)
			if lookupErr != nil {
				failure = &backfillFailure{EntityID: eid, Error: "lookup failed: " + lookupErr.Error()}
				return nil
			}
			if entity == nil {
				failure = &backfillFailure{EntityID: eid, Error: "entity not found"}
				return nil
			}
			if entity.State != "active" {
				failure = &backfillFailure{EntityID: eid, Error: "entity is not active"}
				return nil
			}
			if !entityInProjectScope(entity, project) {
				failure = &backfillFailure{EntityID: eid, Error: "entity is outside this project's scope"}
				return nil
			}
			// Stamp manual-assignment display copy so the entities-panel
			// UI (SKY-238) renders "Manually assigned by user." instead
			// of the empty-rationale fallback. Overwrites any prior
			// model-driven rationale on reclaim — the human's pick
			// supersedes the classifier's vote, and showing the stale
			// model rationale next to a human-claimed assignment would
			// be misleading.
			if assignErr := tx.Entities.AssignProject(r.Context(), orgID, eid, &projectID, manualAssignmentMessage); assignErr != nil {
				if errors.Is(assignErr, sql.ErrNoRows) {
					failure = &backfillFailure{EntityID: eid, Error: "entity not found"}
					return nil
				}
				return assignErr
			}
			return nil
		})
		if txErr != nil {
			failures = append(failures, backfillFailure{EntityID: eid, Error: txErr.Error()})
			continue
		}
		if failure != nil {
			failures = append(failures, *failure)
			continue
		}
		applied++
		assigned = append(assigned, eid)
	}

	if len(assigned) > 0 && bf.ws != nil {
		bf.ws.Broadcast(websocket.Event{
			Type:      "entities_assigned_to_project",
			OrgID:     orgID,
			ProjectID: projectID,
			Data:      map[string]any{"entity_ids": assigned},
		})
	}

	if failures == nil {
		failures = []backfillFailure{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied, "failed": failures})
}

// entityInProjectScope reports whether the entity falls under the
// project's tracker scope. Per-source rules:
//   - github: source_id's "owner/repo" prefix must be in pinned_repos
//     (an empty pinned_repos = no filter on github).
//   - jira: source_id's project-key prefix must equal jira_project_key
//     (an empty jira_project_key = no filter on jira).
//   - other sources are rejected — we only know how to scope these
//     two, and nothing outside them should be claimable.
//
// Used by both /backfill-candidates (to filter the list shown to the
// user) and /backfill (to revalidate every submitted id, so a stale
// tab can't reassign out-of-scope entities).
func entityInProjectScope(entity *domain.Entity, project *domain.Project) bool {
	switch entity.Source {
	case "github":
		if len(project.PinnedRepos) == 0 {
			return true
		}
		repo := githubRepoFromSourceID(entity.SourceID)
		for _, pin := range project.PinnedRepos {
			if pin == repo {
				return true
			}
		}
		return false
	case "jira":
		if project.JiraProjectKey == "" {
			return true
		}
		return jiraKeyFromSourceID(entity.SourceID) == project.JiraProjectKey
	default:
		return false
	}
}

// githubRepoFromSourceID extracts "owner/repo" from a GitHub entity's
// source_id, which is shaped "owner/repo#NNN".
func githubRepoFromSourceID(sourceID string) string {
	if idx := strings.LastIndex(sourceID, "#"); idx >= 0 {
		return sourceID[:idx]
	}
	return sourceID
}

// jiraKeyFromSourceID extracts the project key from a Jira entity's
// source_id, which is shaped "PROJ-123".
func jiraKeyFromSourceID(sourceID string) string {
	if idx := strings.Index(sourceID, "-"); idx >= 0 {
		return sourceID[:idx]
	}
	return sourceID
}
