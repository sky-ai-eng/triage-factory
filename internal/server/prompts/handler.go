package prompts

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// Handler serves the prompt and event-type endpoints. It holds only the deps
// these routes need — the DB handle (for the event-types catalog read), the
// transactional store runner and the authorization checker — and Register
// mounts its methods through httpx.Mounts.
type Handler struct {
	db *sql.DB
	tx db.TxRunner
	az *authz.Checker
}

// New builds the prompts handler. database is the handle the event-types
// catalog read uses.
func New(database *sql.DB, tx db.TxRunner, az *authz.Checker) *Handler {
	return &Handler{db: database, tx: tx, az: az}
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
	SupportsWatch bool `json:"supports_watch" doc:"Whether an event handler for this event type may set applies_to_unowned, which reaches entities no member of the handler's team owns."`
}

var opEventTypeList = httpx.NewJSONOp[httpx.NoBody, []eventTypeResponse](httpx.OpSpec{
	Pattern:     "GET /api/event-types",
	ID:          "list_event_types",
	Summary:     "List event types",
	Description: "Returns every event type the organization is licensed for, as an array with no pagination. The set is fixed by the deployment's build.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *Handler) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	types, err := db.ListEventTypes(h.db)
	if err != nil {
		httpx.InternalError(w, "prompts", err)
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
	opEventTypeList.Write(w, out)
}

// promptListRequest is the body of POST /api/prompts/list. TeamID narrows to
// one team's prompts on the multi-team prompts page; empty returns everything
// visible (solo/local, or an unfiltered view). Search narrows to prompts whose
// name carries it, case-insensitively and literally (httpx.Search).
type promptListRequest struct {
	TeamID string `json:"team_id" doc:"Narrows to the prompts of this team; absent is every team the caller can see."`
	Search string `json:"search" doc:"Narrows to prompts whose name contains this text, compared case-insensitively and literally; trimmed, at most 200 characters, and empty is absent."`

	httpx.PageRequest
}

var opPromptList = httpx.NewListOp[promptListRequest, domain.Prompt](httpx.OpSpec{
	Pattern:     "POST /api/prompts/list",
	ID:          "list_prompts",
	Summary:     "List prompts",
	Description: "Returns one page of the prompts the caller can see, narrowed by the body's filters and most recently updated first.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

type promptListFilterKey struct {
	TeamID string `json:"team_id"`
	Search string `json:"search"`
}

// POST /api/prompts/list
func (h *Handler) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opPromptList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	httpx.TeamIDFilterField(&v, req.TeamID)
	filter := db.PromptListFilter{TeamID: req.TeamID, Search: httpx.Search(&v, req.Search)}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(promptListFilterKey{
		TeamID: filter.TeamID, Search: httpx.SearchKey(filter.Search),
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		list  []domain.Prompt
		total int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		list, total, e = tx.Prompts.List(r.Context(), orgID, filter, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		httpx.InternalError(w, "prompts", err)
		return
	}
	opPromptList.Write(w, page, list, total)
}

var opPromptGet = httpx.NewJSONOp[httpx.NoBody, domain.Prompt](httpx.OpSpec{
	Pattern:     "GET /api/prompts/{id}",
	ID:          "get_prompt",
	Summary:     "Get a prompt",
	Description: "Returns one prompt, in the list's row shape.",
	PathParams:  map[string]string{"id": "The prompt's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *Handler) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}
	var prompt *domain.Prompt
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		httpx.NotFound(w, "prompt")
		return
	}

	opPromptGet.Write(w, *prompt)
}

var opPromptCreate = httpx.NewJSONOp[CreateRequest, domain.Prompt](httpx.OpSpec{
	Pattern:     "POST /api/prompts",
	ID:          "create_prompt",
	Summary:     "Create a prompt",
	Description: "Creates a prompt on the caller's team and returns it. The prompt is part of no blueprint until a blueprint's steps name it.",
	Status:      http.StatusCreated,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

func (h *Handler) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	req, ok := opPromptCreate.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	ValidateFields(&v, "", req.Name, req.Body, req.Model)
	if v.Flush(w, http.StatusBadRequest) {
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

	userID := httpx.ClaimsFrom(r.Context()).Subject

	// Resolve the acting team read-only and reject viewers up front (TFAC-447):
	// a viewer can't author prompts. Gating here yields a clean 403 instead of
	// the prompts_insert RLS WITH CHECK surfacing as a 500. The main tx below
	// re-resolves (and stamps last-acting) only for callers that pass.
	var actingTeam string
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		httpx.InternalError(w, "prompts", err)
		return
	}
	if !h.az.RequireTeamWrite(w, r, orgID, userID, actingTeam) {
		return
	}

	var created domain.Prompt
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		httpx.InternalError(w, "prompts", err)
		return
	}
	opPromptCreate.Write(w, created)
}

var opPromptReplace = httpx.NewJSONOp[UpdateRequest, domain.Prompt](httpx.OpSpec{
	Pattern:     "PUT /api/prompts/{id}",
	ID:          "replace_prompt",
	Summary:     "Update a prompt",
	Description: "Sets a prompt's name, body, and model, all three of which the body must carry, and returns the prompt.",
	PathParams:  map[string]string{"id": "The prompt's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *Handler) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}

	req, ok := opPromptReplace.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	ValidateFields(&v, "", req.Name, req.Body, req.Model)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var existing *domain.Prompt
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "prompts", err)
		return
	}
	if existing == nil {
		httpx.NotFound(w, "prompt")
		return
	}
	// A viewer of the prompt's team can read it but not rewrite it (TFAC-447).
	// Gate after the 404 so a missing prompt still reads as not-found.
	if !h.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	var updated domain.Prompt
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Prompts.Update(r.Context(), orgID, id, req.Name, req.Body, req.Model)
		return e
	}); err != nil {
		// The Get above resolved the row, so a miss here is the row going away
		// between the two transactions rather than a caller naming nothing —
		// still a 404, because the resource the response would describe is gone.
		if errors.Is(err, db.ErrNoSuchPrompt) {
			httpx.NotFound(w, "prompt")
			return
		}
		httpx.InternalError(w, "prompts", err)
		return
	}
	opPromptReplace.Write(w, updated)
}

// promptDeleteResponse is the answer to DELETE /api/prompts/{id}.
type promptDeleteResponse struct {
	Status            string `json:"status" doc:"hidden for a shipped or imported prompt, which is hidden from the library, or deleted for any other."`
	OrphanedBlueprint bool   `json:"orphaned_blueprint" doc:"Whether the delete left the steps after the prompt in a new blueprint with no event handler."`
}

var opPromptDelete = httpx.NewJSONOp[httpx.NoBody, promptDeleteResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/prompts/{id}",
	ID:          "delete_prompt",
	Summary:     "Delete a prompt",
	Description: "Deletes a prompt and takes it out of its blueprint. A one-step blueprint is deleted with it. In a longer blueprint, the steps after the prompt move to a new blueprint with no event handler, and when the prompt was the first step the blueprint is deleted; a deleted blueprint's event handlers lose their blueprint, and one in automate moves to watch.",
	PathParams:  map[string]string{"id": "The prompt's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *Handler) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}

	// Pre-load to resolve the prompt's team for the viewer gate (TFAC-447): a
	// viewer can read a prompt but not delete it. A missing prompt falls through
	// to the 404 the mutation tx below already renders (RequireTeamWrite isn't
	// reached). The extra read is cheap and human-paced (canvas delete gesture).
	var existing *domain.Prompt
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "prompts", err)
		return
	}
	// The existing != nil guard is semantically load-bearing, not just defensive:
	// a nil preload (prompt missing, or not visible to this viewer under
	// prompts_select) must fall through to the delete tx below, which re-Gets nil
	// under the same RLS context and renders the 404 — so a viewer probing a
	// prompt they can't see learns "not found", never "view-only". RequireTeamWrite
	// needs a real team_id and must not be reached for an absent row.
	if existing != nil && !h.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	var prompt *domain.Prompt
	var status string
	// orphaned is set when fragmenting a multi-step blueprint leaves the steps
	// after the deleted prompt as a new, trigger-less blueprint (head / mid
	// delete) — the canvas surfaces a toast so the now-untriggered downstream
	// doesn't go unnoticed.
	var orphaned bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		//     the wrapper doesn't linger, detaching its handlers as a blueprint
		//     delete does.
		//   - owner is a ≥2-step blueprint (a real composition) → fragment the
		//     chain per the split rule (DeleteStep): the component still headed by
		//     the original entry keeps its handlers + id; every other component
		//     becomes a new blueprint no handler references; a head delete
		//     additionally detaches the handlers (their entry prompt is the one
		//     going away).
		//   - no owner (prompt removed from all blueprints already) → just delete
		//     the prompt.
		ownerID, owned, e := tx.Blueprints.StepPromptOwner(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if owned {
			// Held until commit, before the chain is read, so whether this
			// retires the owner and which handlers it detaches are decided
			// from a chain nothing else can change before the delete lands
			// (BlueprintStore.GetForUpdate). An owner retired, or a prompt
			// moved off it, while this waited means another write changed
			// the chain the delete was asked against, so it answers 409
			// rather than delete from a chain the caller never saw.
			owner, e := tx.Blueprints.GetForUpdate(r.Context(), orgID, ownerID)
			if e != nil {
				return e
			}
			if owner == nil {
				return errOwnerChanged
			}
			steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, ownerID)
			if e != nil {
				return e
			}
			stepIndex := indexOfStep(steps, id)
			if stepIndex < 0 {
				return errOwnerChanged
			}
			if len(steps) == 1 {
				// Sole-owner pair-delete (auto-wrap case): retire the wrapper.
				if e := tx.EventHandlers.DetachBlueprint(r.Context(), orgID, ownerID); e != nil {
					return e
				}
				if e := tx.Blueprints.Delete(r.Context(), orgID, ownerID); e != nil {
					return e
				}
			} else {
				// Head delete: the handlers ran into the entry prompt that's going
				// away, so detach them (the downstream wasn't authored as their
				// target): each stays live without a blueprint, and one that
				// automated moves to watch.
				if stepIndex == 0 {
					if de := tx.EventHandlers.DetachBlueprint(r.Context(), orgID, ownerID); de != nil {
						return de
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
		status, de = SoftDeleteBySource(r.Context(), tx, orgID, prompt)
		return de
	}); err != nil {
		if errors.Is(err, errOwnerChanged) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonConflict,
				Message: "the blueprint this prompt belongs to was deleted, merged or split while this delete was made: read the prompt again and retry",
			})
			return
		}
		httpx.InternalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		httpx.NotFound(w, "prompt")
		return
	}
	opPromptDelete.Write(w, promptDeleteResponse{Status: status, OrphanedBlueprint: orphaned})
}

// errOwnerChanged is a prompt delete whose owning blueprint was retired, or
// no longer holds the prompt, once the delete held it: another write changed
// the chain while the delete waited.
var errOwnerChanged = errors.New("the prompt's blueprint changed during the delete")

// indexOfStep returns the position of the step whose prompt is promptID, or -1.
func indexOfStep(steps []domain.BlueprintStep, promptID string) int {
	for i := range steps {
		if steps[i].StepPromptID == promptID {
			return i
		}
	}
	return -1
}

var opPromptStats = httpx.NewJSONOp[httpx.NoBody, domain.PromptStats](httpx.OpSpec{
	Pattern:     "GET /api/prompts/{id}/stats",
	ID:          "get_prompt_stats",
	Summary:     "Get a prompt's run statistics",
	Description: "Returns counts, cost, and duration over every agent conversation that ran the prompt, and a daily count of those conversations for the last 30 days.",
	PathParams:  map[string]string{"id": "The prompt's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *Handler) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "prompt")
	if !ok {
		return
	}
	var (
		stats  *domain.PromptStats
		prompt *domain.Prompt
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		httpx.InternalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		httpx.NotFound(w, "prompt")
		return
	}
	opPromptStats.Write(w, *stats)
}

// Register mounts the prompt and event-type routes.
func (h *Handler) Register(m httpx.Mounts) {
	// **Declared exceptions to the paginated-list rule.** These two are not
	// lists of rows a tenant owns; they are the build's own vocabulary —
	// domain.AllEventTypes() and the schemas derived from it, fixed at compile
	// time and identical for every caller. There is nothing to filter by,
	// nothing that grows with usage, and a page token would address a set that
	// only changes when the binary does. A client reads them once and caches.
	// Their shape stays a bare list on purpose: paginating a constant is
	// ceremony that makes the surface look mutable.
	m.Op(opEventTypeList, h.handleEventTypes)
	m.Op(opPromptList, h.handlePromptsList)
	m.Op(opPromptCreate, h.handlePromptCreate)
	m.Op(opPromptGet, h.handlePromptGet)
	m.Op(opPromptReplace, h.handlePromptPut)
	m.Op(opPromptDelete, h.handlePromptDelete)
	m.Op(opPromptStats, h.handlePromptStats)
}
