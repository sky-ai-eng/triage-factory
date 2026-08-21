package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// projectEntitiesHandler serves the per-project entities panel endpoint,
// holding the transactional store runner it reads through.
type projectEntitiesHandler struct {
	tx db.TxRunner
}

// projectEntity is the per-row payload returned by
// GET /api/projects/{id}/entities. Trimmed down from domain.Entity —
// snapshot_json + description aren't useful in a list view, and
// elision keeps the response small for projects with many entities.
type projectEntity struct {
	ID                      string     `json:"id"`
	Source                  string     `json:"source"`
	SourceID                string     `json:"source_id"`
	Kind                    string     `json:"kind"`
	Title                   string     `json:"title"`
	URL                     string     `json:"url"`
	State                   string     `json:"state"`
	ClassificationRationale string     `json:"classification_rationale,omitempty"`
	LastPolledAt            *time.Time `json:"last_polled_at"`
	CreatedAt               time.Time  `json:"created_at"`
}

// projectEntityListRequest is the body of
// POST /api/projects/{id}/entities/list. The project is the path id and the
// active-only rule is the resource's definition, so the body is paging alone.
type projectEntityListRequest struct {
	httpx.PageRequest
}

// handleProjectEntities returns the active entities assigned to this project,
// ordered most-recently-polled first.
//
// Active-only: terminal-state entities (closed PRs, completed Jiras)
// are filtered out at the DB layer. The panel surfaces work that's
// still in flight; historical context lives elsewhere (entity detail
// pages, future audit views) so the panel stays scannable.
//
// POST /api/projects/{id}/entities/list
func (pe *projectEntitiesHandler) handleProjectEntities(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID, ok := projectIDOr404(w, r)
	if !ok {
		return
	}

	var req projectEntityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		project  *domain.Project
		entities []domain.ProjectPanelEntity
		total    int
	)
	if err := pe.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil {
			return e
		}
		if project == nil {
			return nil
		}
		entities, total, e = tx.Entities.ListProjectPanel(r.Context(), orgID, projectID,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "entities", fmt.Errorf("load entities for project %s: %w", projectID, err))
		return
	}
	// The project gate stays a 404 rather than an empty page: the path id is
	// the resource being addressed, so a project the caller cannot see is a
	// missing address, not a filter that matched nothing.
	if project == nil {
		notFound(w, "project")
		return
	}

	out := make([]projectEntity, 0, len(entities))
	for _, e := range entities {
		out = append(out, projectEntity{
			ID:                      e.ID,
			Source:                  e.Source,
			SourceID:                e.SourceID,
			Kind:                    e.Kind,
			Title:                   e.Title,
			URL:                     e.URL,
			State:                   e.State,
			ClassificationRationale: e.ClassificationRationale,
			LastPolledAt:            e.LastPolledAt,
			CreatedAt:               e.CreatedAt,
		})
	}
	httpx.WriteList(w, page, out, total)
}
