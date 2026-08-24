package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// promptsHandler serves the prompt and event-type endpoints. It holds only
// the deps these routes need — the DB handle (for the event-types catalog
// read) and the transactional store runner — and routes() registers its
// methods through the api()/apiMutating() middleware wrappers.
type promptsHandler struct {
	db *sql.DB
	tx db.TxRunner
	az *authz.Checker
}

// eventTypeResponse is the /api/event-types wire shape: the catalog row plus a
// derived supports_watch flag (TFAC-519). The flag tells the rule/trigger
// editors whether the applies_to_unowned ("watch") toggle is meaningful for this
// event — true only for owner-ladder events, where a non-owner team can opt into
// reaching the entity; inert (and so hidden) for pool / requested-party events.
// The embedded domain.EventType flattens its json fields, so the row shape is
// unchanged and only supports_watch is added.
type eventTypeResponse struct {
	domain.EventType
	SupportsWatch bool `json:"supports_watch"`
}

func (ph *promptsHandler) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	types, err := db.ListEventTypes(ph.db)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	// make(_, 0, len(types)) + append yields a non-nil empty slice when the
	// catalog read returns nil (or every row is gated off), so the response
	// serializes as [] (not null) — preserving the prior explicit nil guard's
	// contract while dropping gated-off rows (TFAC-524).
	out := make([]eventTypeResponse, 0, len(types))
	for _, t := range types {
		if !entitlements.EventTypeAllowed(orgID, t.ID) {
			continue
		}
		out = append(out, eventTypeResponse{EventType: t, SupportsWatch: routing.EventSupportsWatch(t.ID)})
	}
	writeJSON(w, http.StatusOK, out)
}

// promptListRequest is the body of POST /api/prompts/list. TeamID narrows to
// one team's prompts on the multi-team prompts page; empty returns everything
// visible (solo/local, or an unfiltered view).
type promptListRequest struct {
	TeamID string `json:"team_id"`

	httpx.PageRequest
}

type promptListFilterKey struct {
	TeamID string `json:"team_id"`
}

// POST /api/prompts/list
func (ph *promptsHandler) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req promptListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	teamIDFilterField(&v, req.TeamID)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(promptListFilterKey{TeamID: req.TeamID}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		list  []domain.Prompt
		total int
	)
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		list, total, e = tx.Prompts.List(r.Context(), orgID, req.TeamID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	httpx.WriteList(w, page, list, total)
}

func (ph *promptsHandler) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}
	var prompt *domain.Prompt
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}

	writeJSON(w, http.StatusOK, prompt)
}

func (ph *promptsHandler) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	var req prompts.CreateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name",
		})
		return
	}
	// A prompt is the step content unit — it always carries a body (the
	// mission). Ordering prompts into a multi-step composition is the
	// blueprint's job, not a prompt-kind discriminator.
	if req.Body == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "body is required", Field: "body",
		})
		return
	}
	if universe := deploymentUniverse(); !prompts.ValidModel(universe, req.Model) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: prompts.InvalidModelError(universe), Field: "model",
		})
		return
	}

	id := uuid.New().String()
	prompt := domain.Prompt{
		ID:     id,
		Name:   req.Name,
		Body:   req.Body,
		Source: "user",
		Model:  req.Model,
	}

	userID := ClaimsFrom(r.Context()).Subject

	// Resolve the acting team read-only and reject viewers up front (TFAC-447):
	// a viewer can't author prompts. Gating here yields a clean 403 instead of
	// the prompts_insert RLS WITH CHECK surfacing as a 500. The main tx below
	// re-resolves (and stamps last-acting) only for callers that pass.
	var actingTeam string
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "prompts", err)
		return
	}
	if !ph.az.RequireTeamWrite(w, r, orgID, userID, actingTeam) {
		return
	}

	var created domain.Prompt
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		created, e = tx.Prompts.Create(r.Context(), orgID, teamID, prompt)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (ph *promptsHandler) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}

	var req prompts.UpdateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name",
		})
		return
	}
	if req.Body == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "body is required", Field: "body",
		})
		return
	}
	if universe := deploymentUniverse(); !prompts.ValidModel(universe, req.Model) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: prompts.InvalidModelError(universe), Field: "model",
		})
		return
	}

	var existing *domain.Prompt
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if existing == nil {
		notFound(w, "prompt")
		return
	}
	// A viewer of the prompt's team can read it but not rewrite it (TFAC-447).
	// Gate after the 404 so a missing prompt still reads as not-found.
	if !ph.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	var updated domain.Prompt
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Prompts.Update(r.Context(), orgID, id, req.Name, req.Body, req.Model)
		return e
	}); err != nil {
		// The Get above resolved the row, so a miss here is the row going away
		// between the two transactions rather than a caller naming nothing —
		// still a 404, because the resource the response would describe is gone.
		if errors.Is(err, db.ErrNoSuchPrompt) {
			notFound(w, "prompt")
			return
		}
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (ph *promptsHandler) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}

	// Pre-load to resolve the prompt's team for the viewer gate (TFAC-447): a
	// viewer can read a prompt but not delete it. A missing prompt falls through
	// to the 404 the mutation tx below already renders (RequireTeamWrite isn't
	// reached). The extra read is cheap and human-paced (canvas delete gesture).
	var existing *domain.Prompt
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	// The existing != nil guard is semantically load-bearing, not just defensive:
	// a nil preload (prompt missing, or not visible to this viewer under
	// prompts_select) must fall through to the delete tx below, which re-Gets nil
	// under the same RLS context and renders the 404 — so a viewer probing a
	// prompt they can't see learns "not found", never "view-only". RequireTeamWrite
	// needs a real team_id and must not be reached for an absent row.
	if existing != nil && !ph.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	var prompt *domain.Prompt
	var status string
	// orphaned is set when fragmenting a multi-step blueprint leaves the steps
	// after the deleted prompt as a new, trigger-less blueprint (head / mid
	// delete) — the canvas surfaces a toast so the now-untriggered downstream
	// doesn't go unnoticed.
	var orphaned bool
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if prompt == nil {
			return nil
		}

		// Delete-pairing under copy-only prompts. Every new prompt is auto-wrapped
		// as a 1-step blueprint, so a bare prompt-delete would hit the
		// blueprint_steps RESTRICT FK. Resolve the prompt's owning blueprint:
		//   - owner is a 1-step blueprint this prompt solely constitutes (the
		//     auto-wrap case) → soft-delete that blueprint alongside the prompt so
		//     the wrapper doesn't linger.
		//   - owner is a ≥2-step blueprint (a real composition) → fragment the
		//     chain per the split rule (DeleteStep): the component still headed by
		//     the original entry keeps the trigger + id; every other component
		//     becomes a new trigger-less blueprint; a head delete additionally
		//     detaches the trigger (its target entry prompt is the one going away).
		//   - no owner (prompt removed from all blueprints already) → just delete
		//     the prompt.
		ownerID, owned, e := tx.Blueprints.StepPromptOwner(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if owned {
			steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, ownerID)
			if e != nil {
				return e
			}
			if len(steps) == 1 {
				// Sole-owner pair-delete (auto-wrap case): retire the wrapper.
				if e := tx.Blueprints.Delete(r.Context(), orgID, ownerID); e != nil {
					return e
				}
			} else {
				stepIndex := indexOfStep(steps, id)
				if stepIndex < 0 {
					// StepPromptOwner said this blueprint owns the prompt, so the
					// step must be present — a miss is a store inconsistency.
					return fmt.Errorf("prompt %s not found among steps of owning blueprint %s", id, ownerID)
				}
				// Head delete: the trigger fired into the entry prompt that's going
				// away, so detach it (the downstream wasn't authored as the trigger's
				// target) via EventHandlers.Delete, matching handleEventHandlerDelete
				// — a system_slug (shipped) trigger soft-deletes so the
				// shipped-content sync never resurrects it, a user-created trigger
				// hard-deletes.
				if stepIndex == 0 {
					triggers, te := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, ownerID)
					if te != nil {
						return te
					}
					for _, t := range triggers {
						if de := tx.EventHandlers.Delete(r.Context(), orgID, t.ID); de != nil {
							return de
						}
					}
				}
				// The downstream blueprint (head / mid) is named after its new
				// step-0 prompt — the step just past the deleted one — consistent
				// with SplitAt / auto-wrap. Tail delete mints no downstream.
				downstreamName := ""
				if stepIndex < len(steps)-1 {
					if p, ge := tx.Prompts.Get(r.Context(), orgID, steps[stepIndex+1].StepPromptID); ge != nil {
						return ge
					} else if p != nil {
						downstreamName = p.Name
					}
					if downstreamName == "" {
						downstreamName = "Untitled blueprint"
					}
				}
				downstreamID, de := tx.Blueprints.DeleteStep(r.Context(), orgID, ownerID, stepIndex, downstreamName)
				if de != nil {
					return de
				}
				orphaned = downstreamID != ""
			}
		}

		// System and imported prompts are soft-deleted via Hide; user prompts via
		// Delete (also soft — conversations.prompt_id is RESTRICT). Both leave
		// the row + the conversations referencing it so historical timelines
		// still resolve the prompt.
		var de error
		status, de = prompts.SoftDeleteBySource(r.Context(), tx, orgID, prompt)
		return de
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "orphaned_blueprint": orphaned})
}

// indexOfStep returns the position of the step whose prompt is promptID, or -1.
func indexOfStep(steps []domain.BlueprintStep, promptID string) int {
	for i := range steps {
		if steps[i].StepPromptID == promptID {
			return i
		}
	}
	return -1
}

func (ph *promptsHandler) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}
	var (
		stats  *domain.PromptStats
		prompt *domain.Prompt
	)
	if err := ph.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Read the prompt first: Stats aggregates over runs, so a prompt that
		// doesn't exist (or isn't visible) produced an all-zero row that read
		// as a real, quiet prompt — while the sibling GET 404s the same id.
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		if e != nil || prompt == nil {
			return e
		}
		stats, e = tx.Prompts.Stats(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
