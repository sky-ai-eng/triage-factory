// Package blueprints serves the blueprints API: blueprint CRUD, the step list
// and its restructuring verbs (merge, split, reconnect, duplicate), and the
// blueprint-run reads.
package blueprints

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// Handler serves the blueprint + blueprint-run endpoints.
type Handler struct {
	tx db.TxRunner
	az *authz.Checker
}

// New builds the blueprints handler.
func New(tx db.TxRunner, az *authz.Checker) *Handler {
	return &Handler{tx: tx, az: az}
}

const maxBlueprintSteps = 50

// gateBlueprintWrite rejects a viewer before a write against an existing
// blueprint (TFAC-447), pre-loading the row to resolve its team. It returns
// false (the caller should return — a response was already written) when the
// caller is a viewer (403 "view-only access") or the load failed (500). A
// missing blueprint passes through (returns true) so the handler's own nil-check
// renders the 404 — RequireTeamWrite is never reached for an absent row, the
// same shape the prompt-delete gate uses. The same-team guards inside the
// restructure handlers (merge/reconnect refuse cross-team) mean gating on the
// host/path blueprint's team is sufficient for those.
func (h *Handler) gateBlueprintWrite(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var bp *domain.Blueprint
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		bp, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return false
	}
	if bp == nil {
		return true // let the handler's own nil-check render the 404
	}
	return h.az.RequireTeamWrite(w, r, orgID, userID, bp.TeamID)
}

// --- Blueprint header CRUD -----------------------------------------------

// blueprintListRequest is the body of POST /api/blueprints/list. TeamID
// narrows to one team's blueprints on the multi-team page; empty returns
// everything visible. Search narrows to blueprints whose name carries it,
// case-insensitively and literally (httpx.Search).
type blueprintListRequest struct {
	TeamID string `json:"team_id" doc:"Narrows to the blueprints of this team; absent is every team the caller can see."`
	Search string `json:"search" doc:"Narrows to blueprints whose name contains this text, compared case-insensitively and literally; trimmed, at most 200 characters, and empty is absent."`

	httpx.PageRequest
}

var opBlueprintList = httpx.NewListOp[blueprintListRequest, domain.Blueprint](httpx.OpSpec{
	Pattern:     "POST /api/blueprints/list",
	ID:          "list_blueprints",
	Summary:     "List blueprints",
	Description: "Returns one page of the blueprints the caller can see, narrowed by the body's filters and most recently updated first. A blueprint whose every event handler is for an event type the organization is not licensed for is left out.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

type blueprintListFilterKey struct {
	TeamID string `json:"team_id"`
	Search string `json:"search"`
}

// POST /api/blueprints/list
func (h *Handler) handleBlueprintsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opBlueprintList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	httpx.TeamIDFilterField(&v, req.TeamID)
	search := httpx.Search(&v, req.Search)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(blueprintListFilterKey{
		TeamID: req.TeamID, Search: httpx.SearchKey(search),
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The entitlement gate (TFAC-524) hides a blueprint whose every attached
	// trigger fires on an event type this org isn't licensed for. It rides
	// into the query as a filter rather than trimming the page afterwards:
	// a post-window trim makes the page short and total_count a lie about
	// rows the caller cannot see.
	filter := db.BlueprintListFilter{TeamID: req.TeamID, GatedEventTypes: entitlements.GatedEventTypes(orgID), Search: search}

	var (
		blueprints []domain.Blueprint
		total      int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprints, total, e = tx.Blueprints.List(r.Context(), orgID, filter, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	opBlueprintList.Write(w, page, blueprints, total)
}

var opBlueprintGet = httpx.NewJSONOp[httpx.NoBody, domain.Blueprint](httpx.OpSpec{
	Pattern:     "GET /api/blueprints/{id}",
	ID:          "get_blueprint",
	Summary:     "Get a blueprint",
	Description: "Returns one blueprint, in the list's row shape, including one the list leaves out because of the organization's licence.",
	PathParams:  map[string]string{"id": "The blueprint's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintGet is the canonical single read: one blueprint, in the
// list's row shape. It is deliberately NOT entitlement-gated the way the list
// is — the gate is a visibility rule for a browse surface, and a caller
// holding a blueprint's id (from a run, a trigger, a link) must be able to
// resolve its name rather than meet a 404 that reads as deletion.
//
// GET /api/blueprints/{id}
func (h *Handler) handleBlueprintGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var blueprint *domain.Blueprint
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if blueprint == nil {
		httpx.NotFound(w, "blueprint")
		return
	}
	opBlueprintGet.Write(w, *blueprint)
}

// firstPromptInput is the optional inline prompt the canvas's "New Prompt"
// gesture posts to the blueprint-create endpoint. Present ⇒ the handler creates
// prompt + 1-step blueprint + step atomically (no orphan prompts — an
// un-wrapped prompt can't be a canvas node or a trigger target). Absent ⇒ a
// bare blueprint, as before.
type firstPromptInput struct {
	Name  string `json:"name" doc:"The prompt's name; required."`
	Body  string `json:"body" doc:"The prompt's text, what the step's agent is asked to do; required."`
	Model string `json:"model" doc:"The model key the step runs on, one the deployment offers, or empty to use the team's default model."`
}

type createBlueprintRequest struct {
	Name string `json:"name" doc:"The blueprint's name; required without first_prompt, and with it, absent or empty is the prompt's name."`
	// TeamID is the acting team the write picker supplied (see teamscope.ResolveActing).
	TeamID string `json:"team_id" doc:"The id of the team to create the blueprint under, required when the caller writes to more than one team; absent is the caller's one team."`
	// FirstPrompt, when set, auto-wraps a new prompt as this blueprint's sole
	// step in one transaction. Optional — bare-blueprint callers omit it.
	FirstPrompt *firstPromptInput `json:"first_prompt" doc:"A prompt to create with the blueprint as its one step; absent or null creates a blueprint with no steps."`
}

// blueprintCreateResponse returns the created blueprint plus the id of the
// auto-wrapped first prompt (empty when no first_prompt was supplied).
type blueprintCreateResponse struct {
	*domain.Blueprint
	FirstPromptID string `json:"first_prompt_id,omitempty" doc:"The id of the prompt created from first_prompt, absent when the body had none."`
}

var opBlueprintCreate = httpx.NewJSONOp[createBlueprintRequest, blueprintCreateResponse](httpx.OpSpec{
	Pattern:     "POST /api/blueprints",
	ID:          "create_blueprint",
	Summary:     "Create a blueprint",
	Description: "Creates a blueprint on the caller's team and returns it. With first_prompt, it also creates that prompt and makes it the blueprint's one step, and the prompt's name and body are required in place of the blueprint's name.",
	Status:      http.StatusCreated,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

func (h *Handler) handleBlueprintCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	req, ok := opBlueprintCreate.Decode(w, r)
	if !ok {
		return
	}

	// With first_prompt the blueprint name defaults to the prompt's name; the
	// prompt itself must carry a name + body (it's the step's mission).
	// Name defaulting happens here (before teamscope.ResolveActing inside the tx)
	// because it is a pure string assignment independent of team resolution.
	var v httpx.Validation
	if req.FirstPrompt != nil {
		prompts.ValidateFields(&v, "first_prompt.", req.FirstPrompt.Name, req.FirstPrompt.Body, req.FirstPrompt.Model)
		if req.Name == "" {
			req.Name = req.FirstPrompt.Name
		}
	} else if req.Name == "" {
		v.Missing("name")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	id := uuid.New().String()
	userID := httpx.ClaimsFrom(r.Context()).Subject

	// Reject viewers before authoring a blueprint (TFAC-447): resolve the acting
	// team read-only (no last-acting stamp — we may 403) and gate. The main tx
	// re-resolves (and stamps) only for callers that pass.
	if !h.az.RequireActingTeamWrite(w, r, orgID, userID, req.TeamID, "blueprints") {
		return
	}

	var created *domain.Blueprint
	var firstPromptID string
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		// Auto-wrap: create the prompt first, then the blueprint, then bind the
		// step — all in this tx so a mid-stream failure leaves no orphan prompt.
		if req.FirstPrompt != nil {
			firstPromptID = uuid.New().String()
			if _, e := tx.Prompts.Create(r.Context(), orgID, teamID, domain.Prompt{
				ID:     firstPromptID,
				Name:   req.FirstPrompt.Name,
				Body:   req.FirstPrompt.Body,
				Source: "user",
				Model:  req.FirstPrompt.Model,
			}); e != nil {
				return e
			}
		}
		row, e := tx.Blueprints.Create(r.Context(), orgID, teamID, domain.Blueprint{
			ID:     id,
			Name:   req.Name,
			Source: "user",
		})
		if e != nil {
			return e
		}
		// Seeding a first step re-stamps the header (user_modified,
		// updated_at), so the row the create returned is superseded by the one
		// the step write returns — not by a re-read.
		if firstPromptID != "" {
			if row, e = tx.Blueprints.ReplaceSteps(r.Context(), orgID, id, []string{firstPromptID}, nil); e != nil {
				return e
			}
		}
		created = &row
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		httpx.InternalError(w, "blueprints", err)
		return
	}
	opBlueprintCreate.Write(w, blueprintCreateResponse{Blueprint: created, FirstPromptID: firstPromptID})
}

type renameBlueprintRequest struct {
	Name string `json:"name" doc:"The blueprint's new name, trimmed, which may not be empty."`
}

var opBlueprintRename = httpx.NewJSONOp[renameBlueprintRequest, domain.Blueprint](httpx.OpSpec{
	Pattern:     "PUT /api/blueprints/{id}",
	ID:          "replace_blueprint",
	Summary:     "Rename a blueprint",
	Description: "Sets a blueprint's name and returns the blueprint. Its steps and its prompts' names are left as they are.",
	PathParams:  map[string]string{"id": "The blueprint's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintUpdate renames a blueprint header. A blueprint's name is
// independent of its entry prompt's (auto-wrap defaults them equal, but the box
// chrome lets a user rename the blueprint without touching the prompt).
func (h *Handler) handleBlueprintUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	req, ok := opBlueprintRename.Decode(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name"})
		return
	}

	// Viewers can't rename a blueprint (TFAC-447).
	if !h.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	var updated domain.Blueprint
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Blueprints.Rename(r.Context(), orgID, id, name)
		return e
	}); err != nil {
		// The rename itself reports a missing or soft-deleted row now, so the
		// probe read this handler used to bracket the write with is gone.
		if errors.Is(err, db.ErrNoSuchBlueprint) {
			httpx.NotFound(w, "blueprint")
			return
		}
		httpx.InternalError(w, "blueprints", err)
		return
	}
	opBlueprintRename.Write(w, updated)
}

// blueprintDeleteResponse is the answer to DELETE /api/blueprints/{id}.
type blueprintDeleteResponse struct {
	Status string `json:"status" doc:"Always deleted."`
}

var opBlueprintDelete = httpx.NewJSONOp[httpx.NoBody, blueprintDeleteResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/blueprints/{id}",
	ID:          "delete_blueprint",
	Summary:     "Delete a blueprint",
	Description: "Deletes a blueprint and the prompts of its steps. Each event handler that ran the blueprint loses it and stays in place, and one in automate moves to watch.",
	PathParams:  map[string]string{"id": "The blueprint's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintDelete soft-deletes a whole blueprint and cascades, in one
// tx (team-scoped, mutating, RLS under the acting team). The cascade is
// richer than a plain delete because team blueprints carry copy-only step
// prompts and the event handlers that reference it:
//
//   - The header is soft-deleted (Blueprints.Delete stamps deleted_at). The row
//     and its blueprint_steps stay as durable audit so the ...System reads keep
//     resolving the name for in-flight runs (which execute their frozen plan,
//     not the live steps) and past-run timelines; request-facing List/Get filter
//     deleted_at IS NULL.
//   - Its step prompts are soft-deleted too. Prompts are copy-only (1:1 with
//     their blueprint), so leaving them behind orphans rows with no canvas
//     presence. We resolve the steps and soft-delete each via the shared
//     source-dispatch — the other half of the prompt-delete sole-owner pairing.
//   - Its event handlers are detached (EventHandlers.DetachBlueprint): each
//     loses its blueprint and stays live, and one that automated moves to
//     watch and keeps making tasks. The prompt head-delete path detaches the
//     same way.
//
// 404 by re-read (GetForUpdate filters soft-deleted) when the blueprint is
// missing or already deleted.
func (h *Handler) handleBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	// Viewers can't delete a blueprint (TFAC-447).
	if !h.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	var found bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Held until commit, before the detach below reads which handlers
		// reference the blueprint, so that set cannot gain a handler before
		// the delete lands (BlueprintStore.GetForUpdate).
		existing, e := tx.Blueprints.GetForUpdate(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		found = true

		// Soft-delete each step prompt while the blueprint is still live (ListSteps
		// keys off blueprint_steps, which persist regardless). A copy-only prompt
		// belongs to this blueprint alone, so its retirement takes the prompt with
		// it. A nil read means the prompt is already deleted_at-stamped or not
		// visible under RLS — nothing to pair. (Get filters deleted_at IS NULL but
		// not hidden, so a Hide-d system/imported prompt is still returned here and
		// re-hidden idempotently below.)
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		for _, st := range steps {
			p, ge := tx.Prompts.Get(r.Context(), orgID, st.StepPromptID)
			if ge != nil {
				return ge
			}
			if p == nil {
				continue
			}
			if _, de := prompts.SoftDeleteBySource(r.Context(), tx, orgID, p); de != nil {
				return de
			}
		}

		if e := tx.EventHandlers.DetachBlueprint(r.Context(), orgID, id); e != nil {
			return e
		}

		// Retire the header last (audit row + steps stay; deleted_at stamped).
		return tx.Blueprints.Delete(r.Context(), orgID, id)
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if !found {
		httpx.NotFound(w, "blueprint")
		return
	}
	opBlueprintDelete.Write(w, blueprintDeleteResponse{Status: "deleted"})
}

// --- Blueprint steps -----------------------------------------------------

// blueprintStepListRequest is the body of POST /api/blueprint-steps/list.
//
// It is ONE route for what used to be two GETs — the canvas's org-wide bulk
// fetch and the per-blueprint read. They differed only in a filter, and a
// filter is what a list body is for; keeping them apart meant two shapes, two
// envelopes, and two places to remember the ordering contract.
type blueprintStepListRequest struct {
	// TeamID narrows to one team's blueprints, like the blueprint list's.
	TeamID string `json:"team_id" doc:"Narrows to the steps of this team's blueprints; absent is every team the caller can see."`
	// BlueprintIDs narrows to specific blueprints. Empty is the canvas's
	// bulk read; a single id is the old per-blueprint read.
	BlueprintIDs []string `json:"blueprint_ids" doc:"Narrows to the steps of these blueprints, at most 200 ids; absent or empty is every blueprint."`
	// Search narrows to steps whose name carries it, case-insensitively and
	// literally (httpx.Search). A step is named by the prompt it runs.
	Search string `json:"search" doc:"Narrows to steps whose prompt's name contains this text, compared case-insensitively and literally; trimmed, at most 200 characters, and empty is absent."`

	httpx.PageRequest
}

var opBlueprintStepList = httpx.NewListOp[blueprintStepListRequest, domain.BlueprintStep](httpx.OpSpec{
	Pattern:     "POST /api/blueprint-steps/list",
	ID:          "list_blueprint_steps",
	Summary:     "List blueprint steps",
	Description: "Returns one page of the steps of the blueprints the caller can see, narrowed by the body's filters and ordered by blueprint id and then step index. Each step carries the version of its blueprint at the time of the read, which a steps replace names.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

type blueprintStepListFilterKey struct {
	TeamID       string   `json:"team_id"`
	BlueprintIDs []string `json:"blueprint_ids"`
	Search       string   `json:"search"`
}

// maxBlueprintStepIDs bounds the id set one request may name. A canvas asks
// for every blueprint in scope by sending none; a caller naming more ids than
// a team could plausibly hold is hand-rolled, and the bound keeps the IN-list
// from growing without limit.
const maxBlueprintStepIDs = 200

// handleBlueprintStepsAll returns the steps of the scope's blueprints — the
// binding canvas's bulk fetch (which avoids an N+1 over the blueprint list)
// and, with blueprint_ids, the per-blueprint read.
//
// POST /api/blueprint-steps/list
func (h *Handler) handleBlueprintStepsAll(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opBlueprintStepList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	httpx.TeamIDFilterField(&v, req.TeamID)
	blueprintIDs := httpx.CanonicalStrings(req.BlueprintIDs)
	if len(blueprintIDs) > maxBlueprintStepIDs {
		v.OutOfRange("blueprint_ids", fmt.Sprintf("at most %d blueprint ids per request", maxBlueprintStepIDs))
	}
	for _, id := range blueprintIDs {
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("blueprint_ids", fmt.Sprintf("blueprint id %q is not a valid blueprint id", id))
		}
	}
	search := httpx.Search(&v, req.Search)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(blueprintStepListFilterKey{
		TeamID: req.TeamID, BlueprintIDs: blueprintIDs, Search: httpx.SearchKey(search),
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		steps []domain.BlueprintStep
		total int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		steps, total, e = tx.Blueprints.ListAllSteps(r.Context(), orgID,
			db.BlueprintStepListFilter{TeamID: req.TeamID, BlueprintIDs: blueprintIDs, Search: search},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	opBlueprintStepList.Write(w, page, steps, total)
}

// blueprintRunListRequest is the body of POST /api/blueprint-runs/list. Before
// this route the collection was unenumerable — a run could only be read by an
// id the caller already held.
type blueprintRunListRequest struct {
	BlueprintID string   `json:"blueprint_id" doc:"Narrows to the runs of this blueprint; absent is every blueprint."`
	Statuses    []string `json:"statuses" doc:"Narrows to runs in one of these statuses, running, completed, aborted, failed, or cancelled; absent or empty is every status."`

	httpx.PageRequest
}

var opBlueprintRunList = httpx.NewListOp[blueprintRunListRequest, domain.BlueprintRun](httpx.OpSpec{
	Pattern:     "POST /api/blueprint-runs/list",
	ID:          "list_blueprint_runs",
	Summary:     "List blueprint runs",
	Description: "Returns one page of the blueprint runs the caller can see, narrowed by the body's filters and most recently started first. Each item is the run itself; GET /api/blueprint-runs/{id} adds its steps.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

type blueprintRunListFilterKey struct {
	BlueprintID string   `json:"blueprint_id"`
	Statuses    []string `json:"statuses"`
}

// POST /api/blueprint-runs/list
func (h *Handler) handleBlueprintRunsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opBlueprintRunList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	if req.BlueprintID != "" {
		if _, err := uuid.Parse(req.BlueprintID); err != nil {
			v.Invalid("blueprint_id", fmt.Sprintf("blueprint_id %q is not a valid blueprint id", req.BlueprintID))
		}
	}
	statuses := httpx.CanonicalStrings(req.Statuses)
	for _, st := range statuses {
		if !domain.ValidBlueprintRunStatus(st) {
			v.Invalid("statuses", fmt.Sprintf("unknown status %q; must be one of: %s",
				st, strings.Join(domain.BlueprintRunStatuses, ", ")))
		}
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(blueprintRunListFilterKey{
		BlueprintID: req.BlueprintID, Statuses: statuses,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		runs  []domain.BlueprintRun
		total int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		runs, total, e = tx.Blueprints.ListRuns(r.Context(), orgID,
			db.BlueprintRunListFilter{BlueprintID: req.BlueprintID, Statuses: statuses},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	// The rows are the run objects themselves — the same shape the single
	// read carries under its "blueprint_run" key. The single read adds the
	// per-step views, which are a composite of the run and its conversations
	// and belong to opening one run, not to enumerating many.
	opBlueprintRunList.Write(w, page, runs, total)
}

type blueprintStepInput struct {
	StepPromptID string `json:"step_prompt_id" doc:"The id of the prompt the step runs, a prompt of the blueprint's team that belongs to no other blueprint; required."`
	Brief        string `json:"brief" doc:"Instructions for this step that are added to its prompt, or empty for none."`
}

type blueprintStepsPutRequest struct {
	Steps []blueprintStepInput `json:"steps" doc:"The blueprint's steps in their new order, at most 50, naming every step the blueprint holds and each prompt once; a step whose prompt the blueprint does not hold yet is added."`
	// Version is the blueprint version the list was decided against: the
	// blueprint_version the caller's read of the steps returned, or the
	// blueprint's own version when it has no steps to carry one. Reading the
	// two apart is safe there, because the only write that moves an empty
	// blueprint's steps adds some, and a list that leaves them out is refused
	// whatever version it names. Not part of the list: it cannot be cleared,
	// and it says which steps the list replaces.
	Version *int64 `json:"version" doc:"The blueprint_version of the steps the list was decided against, or the blueprint's version when it has no steps; required, and refused when the blueprint has changed since."`
}

var opBlueprintStepsReplace = httpx.NewJSONOp[blueprintStepsPutRequest, blueprintWithSteps](httpx.OpSpec{
	Pattern:     "PUT /api/blueprints/{id}/steps",
	ID:          "replace_blueprint_steps",
	Summary:     "Replace a blueprint's steps",
	Description: "Sets the order of a blueprint's steps and their briefs, and adds the steps the list names that the blueprint does not hold yet. Returns the blueprint and its steps. The list must name every step the blueprint holds, so removing a step is a prompt delete, and a version the blueprint has moved past is refused.",
	PathParams:  map[string]string{"id": "The blueprint's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintStepsPut replaces the blueprint's step list: the order of its
// steps, their briefs, and the prompts it adds. It answers the blueprint and
// the steps it wrote, as merge and split do, so the caller holds the version
// its next PUT names.
//
// Two rules keep it from losing a write it never saw. The body names the
// version of the steps it was decided against, and a blueprint any write has
// moved past that version since is refused (409 VERSION_CONFLICT): the
// caller's list was built from steps the blueprint no longer has, so the only
// honest answer is to read them again. And the list must name every step the
// blueprint holds (409 CONFLICT), so a PUT never leaves a prompt in no
// blueprint, where no screen shows it. Removing a step is a prompt delete,
// and moving one is a split and a merge.
//
// Validates that the blueprint exists and that every step references a
// prompt the blueprint's own team owns (same-team guard).
func (h *Handler) handleBlueprintStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var blueprint *domain.Blueprint
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if blueprint == nil {
		httpx.NotFound(w, "blueprint")
		return
	}
	// Viewers can't rewrite a blueprint's steps (TFAC-447). Gate on the
	// already-loaded blueprint's team — no extra round-trip.
	if !h.az.RequireTeamWrite(w, r, orgID, userID, blueprint.TeamID) {
		return
	}

	req, ok := opBlueprintStepsReplace.Decode(w, r)
	if !ok {
		return
	}

	stepIDs := make([]string, 0, len(req.Steps))
	briefs := make([]string, 0, len(req.Steps))
	for _, step := range req.Steps {
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}
	var shape httpx.Validation
	switch {
	case req.Version == nil:
		shape.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "version is required: send the blueprint_version your read of the steps returned, or the blueprint's version if it has no steps",
			Field:   "version",
		})
	case *req.Version < 1:
		// Versions start at 1 and only rise, so anything below names no read.
		shape.Invalid("version", "version must be the blueprint_version your read of the steps returned, or the blueprint's version if it has no steps: a positive integer")
	}
	if slices.Contains(stepIDs, "") {
		shape.Add(httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "step_prompt_id is required for every step", Field: "steps"})
	}
	// A prompt is one step of one blueprint, so a list naming it twice could
	// only fail at the insert.
	for i, sid := range stepIDs {
		if sid != "" && slices.Index(stepIDs, sid) < i {
			shape.Invalid("steps", "step "+strconv.Itoa(i)+" names the same prompt as step "+strconv.Itoa(slices.Index(stepIDs, sid))+": a prompt is one step")
			break
		}
	}
	if shape.Flush(w, http.StatusBadRequest) {
		return
	}
	if len(req.Steps) > maxBlueprintSteps {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "blueprint may not exceed " + strconv.Itoa(maxBlueprintSteps) + " steps", Field: "steps"})
		return
	}

	// Validate each step's prompt exists + is same-team, then replace in one
	// tx so all lookups and the final write share claims.
	var (
		fault   blueprintFault
		gone    bool
		written blueprintWithSteps
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Held until commit, before the checks below read the blueprint's
		// version, its steps, and which blueprint owns each prompt, so they
		// read what a merge, split, reconnect, prompt delete or steps PUT
		// that held this blueprint first committed
		// (BlueprintStore.GetAllForUpdate). Every write that raises the
		// version holds the row too, so the version cannot move between this
		// check and the replace. A blueprint retired meanwhile is answered
		// 404.
		held, e := tx.Blueprints.GetForUpdate(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if held == nil {
			gone = true
			return nil
		}
		// First, because every check after it would judge a list decided
		// against steps the blueprint no longer has.
		if held.Version != *req.Version {
			fault.raise(http.StatusConflict, httpx.ReasonVersionConflict,
				"this blueprint changed since you read its steps; read them again and decide again", "version")
			return nil
		}
		// A step the list leaves out would lose its row, and its prompt would
		// be in no blueprint.
		current, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		var missing []string
		for _, st := range current {
			if !slices.Contains(stepIDs, st.StepPromptID) {
				missing = append(missing, st.StepPromptID)
			}
		}
		if len(missing) > 0 {
			fault.raise(http.StatusConflict, httpx.ReasonConflict,
				"the list leaves out steps the blueprint holds (step prompts "+strings.Join(missing, ", ")+
					"): a steps PUT may reorder, re-brief, and add steps, but must name every one. To remove a step, delete its prompt.", "steps")
			return nil
		}
		for i, sid := range stepIDs {
			stepPrompt, e := tx.Prompts.Get(r.Context(), orgID, sid)
			if e != nil {
				return e
			}
			if stepPrompt == nil {
				fault.raise(http.StatusUnprocessableEntity, httpx.ReasonNotFound,
					"step "+strconv.Itoa(i)+" references a non-existent prompt", "steps")
				return nil
			}
			// Same-team guard: a blueprint may only step through prompts its own
			// team owns. The DB enforces this via the (step_prompt_id, team_id)
			// composite FK on ReplaceSteps; pre-check for a clean 422.
			if blueprint.TeamID != "" && stepPrompt.TeamID != "" && stepPrompt.TeamID != blueprint.TeamID {
				fault.raise(http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef,
					"step "+strconv.Itoa(i)+" references a prompt owned by another team", "steps")
				return nil
			}
			// Copy-only guard: a prompt belongs to at most one blueprint. If this
			// prompt is already a step in a *different* blueprint, refuse with a
			// clean 422 instead of letting the unique index 500. (Owner == this
			// blueprint is fine — re-saving its own step list.)
			// TODO(TFAC-1540): only this PUT's blueprint is held, so two PUTs on
			// different blueprints that add one prompt both pass, and the
			// second answers 500 from the unique index.
			ownerID, owned, e := tx.Blueprints.StepPromptOwner(r.Context(), orgID, sid)
			if e != nil {
				return e
			}
			if owned && ownerID != id {
				fault.raise(http.StatusUnprocessableEntity, httpx.ReasonConflict,
					"step "+strconv.Itoa(i)+" references a prompt that already belongs to another blueprint — copy it to reuse.", "steps")
				return nil
			}
		}
		if fault.status != 0 {
			return nil
		}
		stamped, e := tx.Blueprints.ReplaceSteps(r.Context(), orgID, id, stepIDs, briefs)
		if e != nil {
			return e
		}
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		written = withSteps(&stamped, steps)
		return nil
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if gone {
		httpx.NotFound(w, "blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	opBlueprintStepsReplace.Write(w, written)
}

// blueprintFault is a validation refusal raised inside a WithTx closure and
// written after it unwinds — the response can't be written mid-tx, so the fault
// travels back as data. It carries its own reason and field because these
// handlers refuse for several distinct causes: a prompt that doesn't exist, one
// another team owns, one another blueprint already claims, a size bound, an
// attached trigger. A single reason stamped at the write site would answer
// "something about teams" to all of them, which is true of exactly one.
type blueprintFault struct {
	status int
	item   httpx.ErrorItem
}

// raise records a refusal. Every caller returns from the closure immediately
// after, so the first one is the only one; a later raise is a bug rather than a
// second fault to accumulate, and is dropped rather than overwriting.
func (f *blueprintFault) raise(status int, reason, message, field string) {
	if f.status != 0 {
		return
	}
	f.status = status
	f.item = httpx.ErrorItem{Reason: reason, Message: message, Field: field}
}

// write emits the refusal if one was raised, reporting whether it did so the
// caller can return without falling through to the success body.
func (f *blueprintFault) write(w http.ResponseWriter) bool {
	if f.status == 0 {
		return false
	}
	httpx.WriteErrors(w, f.status, f.item)
	return true
}

// --- Blueprint composition (merge / split) -------------------------------
//
// The two canvas gestures that move steps across blueprint rows, each a single
// transactional endpoint. Done as a client sequence of PUT .../steps + delete
// calls these would be multi-call and non-atomic — a mid-sequence failure
// orphans prompts and leaves an empty blueprint. One tx per gesture closes
// that, and gives third parties the operations as real primitives.

// blueprintWithSteps bundles a blueprint header with its full ordered step
// list — the shape both gestures return so the canvas can refresh in one read.
type blueprintWithSteps struct {
	Blueprint *domain.Blueprint      `json:"blueprint" doc:"The blueprint."`
	Steps     []domain.BlueprintStep `json:"steps" doc:"The blueprint's steps in order, each carrying the blueprint's version."`
}

// withSteps builds a blueprintWithSteps whose steps carry the blueprint's
// version, as the step list's do, so a caller can name the version of the
// steps it holds from either read. Steps are a list, never null.
func withSteps(b *domain.Blueprint, steps []domain.BlueprintStep) blueprintWithSteps {
	out := make([]domain.BlueprintStep, len(steps))
	for i, st := range steps {
		if b != nil {
			st.BlueprintVersion = b.Version
		}
		out[i] = st
	}
	return blueprintWithSteps{Blueprint: b, Steps: out}
}

type mergeBlueprintRequest struct {
	// SourceBlueprintID (B) is absorbed at {id}'s (the host's) tail and retired.
	SourceBlueprintID string `json:"source_blueprint_id" doc:"The id of the blueprint whose steps are appended to the path's blueprint and which is then deleted; it must be of the same team, have no event handler, and leave the result at most 50 steps."`
}

var opBlueprintMerge = httpx.NewJSONOp[mergeBlueprintRequest, blueprintWithSteps](httpx.OpSpec{
	Pattern:     "POST /api/blueprints/{id}/merge",
	ID:          "merge_blueprint",
	Summary:     "Merge a blueprint into another",
	Description: "Appends the source blueprint's steps to the path's blueprint and deletes the source, in one write. The path's blueprint keeps its id, name, and event handlers, and is returned with its steps.",
	PathParams:  map[string]string{"id": "The id of the blueprint that receives the source's steps."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintMerge absorbs a trigger-less source blueprint onto the tail of
// the host ({id}) and retires the source, atomically. The host keeps its
// identity, name, and trigger.
func (h *Handler) handleBlueprintMerge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	hostID := r.PathValue("id")

	req, ok := opBlueprintMerge.Decode(w, r)
	if !ok {
		return
	}
	if req.SourceBlueprintID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "source_blueprint_id is required", Field: "source_blueprint_id"})
		return
	}
	if req.SourceBlueprintID == hostID {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "cannot merge a blueprint into itself", Field: "source_blueprint_id"})
		return
	}

	// Viewers can't restructure blueprints (TFAC-447). Gate on the host; the
	// same-team guard below refuses a cross-team source, so the host's team is
	// the write scope.
	if !h.gateBlueprintWrite(w, r, orgID, userID, hostID) {
		return
	}

	var (
		host, source *domain.Blueprint
		merged       *domain.Blueprint
		steps        []domain.BlueprintStep
		fault        blueprintFault
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if host, e = tx.Blueprints.Get(r.Context(), orgID, hostID); e != nil {
			return e
		}
		if source, e = tx.Blueprints.Get(r.Context(), orgID, req.SourceBlueprintID); e != nil {
			return e
		}
		if host == nil || source == nil {
			return nil // resolved to 404 below
		}
		// Same-team guard: both blueprints must belong to one team (MergeInto
		// leaves team_id on the reparented steps unchanged).
		if host.TeamID != "" && source.TeamID != "" && host.TeamID != source.TeamID {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef,
				"host and source blueprints belong to different teams", "source_blueprint_id")
			return nil
		}
		// Held until commit, before the checks below read either blueprint's
		// steps or handlers, so what they read is what the merge moves
		// (BlueprintStore.GetAllForUpdate). Either one retired while this
		// waited, as a merge absorbing the host would, is answered 404. The
		// hold comes after the same-team guard because its row-level security
		// is the UPDATE policy, which hides a source of a team the caller may
		// only read, and that source is answered with the guard's fault.
		held, e := tx.Blueprints.GetAllForUpdate(r.Context(), orgID, []string{hostID, req.SourceBlueprintID})
		if e != nil {
			return e
		}
		if host, source = held[hostID], held[req.SourceBlueprintID]; host == nil || source == nil {
			return nil // resolved to 404 below
		}
		// Cap: the merged blueprint must stay editable via the normal steps-PUT,
		// which rejects lists longer than maxBlueprintSteps. Reject up front
		// rather than minting an un-editable blueprint.
		hostSteps, e := tx.Blueprints.ListSteps(r.Context(), orgID, hostID)
		if e != nil {
			return e
		}
		sourceSteps, e := tx.Blueprints.ListSteps(r.Context(), orgID, req.SourceBlueprintID)
		if e != nil {
			return e
		}
		if len(hostSteps)+len(sourceSteps) > maxBlueprintSteps {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonOutOfRange,
				"merged blueprint would exceed "+strconv.Itoa(maxBlueprintSteps)+" steps", "source_blueprint_id")
			return nil
		}
		// No event handler may reference the source, in any mode: the merge
		// retires it, and a handler on it would be left referencing a retired
		// blueprint. Unreachable from the canvas (you can only connect a tail
		// into an entry no handler references), but the endpoint is
		// third-party-callable, so it refuses here. A handler write holding
		// the source (GetForShare) either committed before the hold above,
		// and its handler is listed here, or waits for this transaction and
		// then reads the source as absent.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.SourceBlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonConflict,
				"an event handler references the absorbed blueprint; detach it first", "source_blueprint_id")
			return nil
		}
		if e := tx.Blueprints.MergeInto(r.Context(), orgID, hostID, req.SourceBlueprintID); e != nil {
			return e
		}
		if merged, e = tx.Blueprints.Get(r.Context(), orgID, hostID); e != nil {
			return e
		}
		steps, e = tx.Blueprints.ListSteps(r.Context(), orgID, hostID)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if host == nil {
		httpx.NotFound(w, "blueprint")
		return
	}
	if source == nil {
		httpx.NotFound(w, "source blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	opBlueprintMerge.Write(w, withSteps(merged, steps))
}

type splitBlueprintRequest struct {
	// AtStepIndex k splits the boundary before step k: steps [0,k) stay on
	// {id}, steps [k,N) move to a new trigger-less blueprint. Pointer so a
	// missing field 400s ("required") instead of silently defaulting to 0.
	AtStepIndex *int `json:"at_step_index" doc:"The 0-based index of the first step to move to the new blueprint, which must leave at least one step on each side; required."`
}

// blueprintSplitResponse returns both halves of a split: the upstream blueprint
// (trigger retained) and the new trigger-less downstream blueprint.
type blueprintSplitResponse struct {
	Upstream   blueprintWithSteps `json:"upstream" doc:"The path's blueprint with the steps before at_step_index."`
	Downstream blueprintWithSteps `json:"downstream" doc:"The new blueprint with the steps from at_step_index on, named after its first step's prompt."`
}

var opBlueprintSplit = httpx.NewJSONOp[splitBlueprintRequest, blueprintSplitResponse](httpx.OpSpec{
	Pattern:     "POST /api/blueprints/{id}/split",
	ID:          "split_blueprint",
	Summary:     "Split a blueprint in two",
	Description: "Moves the steps from at_step_index on to a new blueprint with no event handler, in one write, and returns both blueprints with their steps. The path's blueprint keeps its id, name, and event handlers.",
	PathParams:  map[string]string{"id": "The id of the blueprint to split."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintSplit partitions a blueprint at an index into the upstream
// (trigger-retained) half and a new trigger-less downstream blueprint,
// atomically.
func (h *Handler) handleBlueprintSplit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	req, ok := opBlueprintSplit.Decode(w, r)
	if !ok {
		return
	}
	// *int so a missing field is distinguishable from an explicit 0 (which is a
	// real index whose 422 message is about non-empty halves, not absence).
	if req.AtStepIndex == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "at_step_index is required", Field: "at_step_index"})
		return
	}
	atIndex := *req.AtStepIndex

	// Viewers can't restructure blueprints (TFAC-447).
	if !h.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	newID := uuid.New().String()
	var (
		bp                   *domain.Blueprint
		upstream, downstream *domain.Blueprint
		upSteps, downSteps   []domain.BlueprintStep
		fault                blueprintFault
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// Held until commit, before the steps are read, so the index is
		// checked against the steps the split moves
		// (BlueprintStore.GetAllForUpdate).
		if bp, e = tx.Blueprints.GetForUpdate(r.Context(), orgID, id); e != nil || bp == nil {
			return e
		}
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		// TODO(TFAC-1539): the index is checked against the steps held now,
		// not the ones the caller read, so a split sent after another write
		// moved them cuts next to a step the caller never chose.
		// 0 < k < N: a split that keeps one side empty is a no-op.
		if atIndex <= 0 || atIndex >= len(steps) {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonOutOfRange,
				"at_step_index must split the blueprint into two non-empty halves", "at_step_index")
			return nil
		}
		// The downstream name defaults to its new step-0 prompt's name (the
		// prompt at the original boundary index), consistent with auto-wrap.
		// Fall back to a placeholder on the theoretical missing-prompt path so
		// the new blueprint is never anonymous (prompts.name is non-empty in
		// practice — create + auto-wrap both require it).
		newName := ""
		if p, e := tx.Prompts.Get(r.Context(), orgID, steps[atIndex].StepPromptID); e != nil {
			return e
		} else if p != nil {
			newName = p.Name
		}
		if newName == "" {
			newName = "Untitled blueprint"
		}
		if _, e := tx.Blueprints.SplitAt(r.Context(), orgID, id, atIndex, newID, newName); e != nil {
			return e
		}
		if upstream, e = tx.Blueprints.Get(r.Context(), orgID, id); e != nil {
			return e
		}
		if downstream, e = tx.Blueprints.Get(r.Context(), orgID, newID); e != nil {
			return e
		}
		if upSteps, e = tx.Blueprints.ListSteps(r.Context(), orgID, id); e != nil {
			return e
		}
		downSteps, e = tx.Blueprints.ListSteps(r.Context(), orgID, newID)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if bp == nil {
		httpx.NotFound(w, "blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	opBlueprintSplit.Write(w, blueprintSplitResponse{
		Upstream:   withSteps(upstream, upSteps),
		Downstream: withSteps(downstream, downSteps),
	})
}

type reconnectBlueprintRequest struct {
	// AtStepIndex k re-points the boundary before step k: steps [0,k) stay on
	// {id} and continue into the target's chain; steps [k,N) peel off as a new
	// orphaned trigger-less blueprint. Pointer so a missing field 400s
	// ("required") instead of silently defaulting to 0.
	AtStepIndex *int `json:"at_step_index" doc:"The 0-based index of the first step to move off the path's blueprint to a new one, which must leave at least one step on each side; required."`
	// TargetBlueprintID (C) is the trigger-less blueprint whose entry the
	// upstream half now connects to; its steps are absorbed onto {id}'s tail.
	TargetBlueprintID string `json:"target_blueprint_id" doc:"The id of the blueprint whose steps follow the steps the path's blueprint keeps, and which is then deleted; it must be of the same team, have no event handler, and leave the result at most 50 steps. Required."`
}

// blueprintReconnectResponse returns the updated host (the upstream half with
// the target's steps absorbed onto its tail) plus the id of the peeled-off
// orphan downstream blueprint, so the caller can refresh + surface a toast.
type blueprintReconnectResponse struct {
	Host              blueprintWithSteps `json:"host" doc:"The path's blueprint, holding its steps before at_step_index followed by the target's steps."`
	OrphanBlueprintID string             `json:"orphan_blueprint_id" doc:"The id of the new blueprint, with no event handler, that holds the path's steps from at_step_index on."`
}

var opBlueprintReconnect = httpx.NewJSONOp[reconnectBlueprintRequest, blueprintReconnectResponse](httpx.OpSpec{
	Pattern:     "POST /api/blueprints/{id}/reconnect",
	ID:          "reconnect_blueprint",
	Summary:     "Reconnect a blueprint to another's steps",
	Description: "Moves the path's steps from at_step_index on to a new blueprint, then appends the target blueprint's steps to the path's blueprint and deletes the target, in one write. The path's blueprint keeps its id, name, and event handlers, and is returned with its steps and the new blueprint's id.",
	PathParams:  map[string]string{"id": "The id of the blueprint whose steps are reconnected."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintReconnect re-points a sequence edge's head onto another
// blueprint's entry — the canvas "drag a step arrow's head to a new prompt"
// gesture (full re-target). It splits {id} at at_step_index (steps [k,N) peel
// off as a new orphaned trigger-less blueprint) and merges target_blueprint_id
// onto the surviving upstream half, atomically: SplitAt + MergeInto run in one
// tx (inTx reuses the WithTx tx), so a mid-sequence failure can't leave a
// half-split blueprint behind. The host keeps its id + trigger.
func (h *Handler) handleBlueprintReconnect(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	req, ok := opBlueprintReconnect.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	if req.AtStepIndex == nil {
		v.Missing("at_step_index")
	}
	if req.TargetBlueprintID == "" {
		v.Missing("target_blueprint_id")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Viewers can't restructure blueprints (TFAC-447). Gate on the host; the
	// same-team guard below refuses a cross-team target.
	if !h.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	atIndex := *req.AtStepIndex
	orphanID := uuid.New().String()

	var (
		host, target *domain.Blueprint
		hostOut      *domain.Blueprint
		hostSteps    []domain.BlueprintStep
		fault        blueprintFault
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if host, e = tx.Blueprints.Get(r.Context(), orgID, id); e != nil || host == nil {
			return e
		}
		if target, e = tx.Blueprints.Get(r.Context(), orgID, req.TargetBlueprintID); e != nil || target == nil {
			return e
		}
		if req.TargetBlueprintID == id {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonInvalidField,
				"cannot reconnect a blueprint into itself", "target_blueprint_id")
			return nil
		}
		// Same-team guard (MergeInto leaves team_id on the reparented steps
		// unchanged).
		if host.TeamID != "" && target.TeamID != "" && host.TeamID != target.TeamID {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef,
				"host and target blueprints belong to different teams", "target_blueprint_id")
			return nil
		}
		// Held until commit, as a merge holds its host and source, and after
		// the same-team guard for the merge's reason.
		held, e := tx.Blueprints.GetAllForUpdate(r.Context(), orgID, []string{id, req.TargetBlueprintID})
		if e != nil {
			return e
		}
		if host, target = held[id], held[req.TargetBlueprintID]; host == nil || target == nil {
			return nil // resolved to 404 below
		}
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		// TODO(TFAC-1539): the index is checked against the steps held now,
		// not the ones the caller read, so a reconnect sent after another
		// write moved them cuts next to a step the caller never chose.
		// 0 < k < N: both the surviving upstream and the orphaned downstream must
		// be non-empty (a sequence edge always sits at such a boundary).
		if atIndex <= 0 || atIndex >= len(steps) {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonOutOfRange,
				"at_step_index must split the blueprint into two non-empty halves", "at_step_index")
			return nil
		}
		// Cap: the surviving upstream (atIndex steps) + the absorbed target must
		// stay editable via the steps-PUT, which rejects lists over the max.
		targetSteps, e := tx.Blueprints.ListSteps(r.Context(), orgID, req.TargetBlueprintID)
		if e != nil {
			return e
		}
		if atIndex+len(targetSteps) > maxBlueprintSteps {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonOutOfRange,
				"reconnected blueprint would exceed "+strconv.Itoa(maxBlueprintSteps)+" steps", "target_blueprint_id")
			return nil
		}
		// No event handler may reference the target, as for a merge's source:
		// the absorb retires it, and a handler on it would be left referencing a
		// retired blueprint. The hold above orders this check against a
		// handler write for the merge's reason.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.TargetBlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonConflict,
				"an event handler references the target blueprint; detach it first", "target_blueprint_id")
			return nil
		}
		// Orphan name defaults to its new step-0 prompt's name (the prompt at the
		// boundary), consistent with split / auto-wrap.
		orphanName := ""
		if p, pe := tx.Prompts.Get(r.Context(), orgID, steps[atIndex].StepPromptID); pe != nil {
			return pe
		} else if p != nil {
			orphanName = p.Name
		}
		if orphanName == "" {
			orphanName = "Untitled blueprint"
		}
		if _, e = tx.Blueprints.SplitAt(r.Context(), orgID, id, atIndex, orphanID, orphanName); e != nil {
			return e
		}
		if e = tx.Blueprints.MergeInto(r.Context(), orgID, id, req.TargetBlueprintID); e != nil {
			return e
		}
		if hostOut, e = tx.Blueprints.Get(r.Context(), orgID, id); e != nil {
			return e
		}
		if hostOut == nil {
			// SplitAt + MergeInto just kept the host alive; an unreadable host is a
			// store inconsistency, not a 404 — fail the tx rather than emit a null.
			return fmt.Errorf("host blueprint %s not readable after reconnect", id)
		}
		hostSteps, e = tx.Blueprints.ListSteps(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if host == nil {
		httpx.NotFound(w, "blueprint")
		return
	}
	if target == nil {
		httpx.NotFound(w, "target blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	opBlueprintReconnect.Write(w, blueprintReconnectResponse{
		Host:              withSteps(hostOut, hostSteps),
		OrphanBlueprintID: orphanID,
	})
}

// --- Blueprint duplication (deep-copy) -----------------------------------

type duplicateBlueprintsRequest struct {
	// PromptIDs is a flat set; the endpoint derives the output structure (the
	// induced contiguous runs) from each prompt's resolved (blueprint, index).
	// Callers never pass blueprint ids or ranges.
	PromptIDs []string `json:"prompt_ids" doc:"The ids of the step prompts to copy, from one or more blueprints of the team; each unbroken run of steps of one blueprint becomes one new blueprint. Required."`
	// TeamID is the acting team the write picker supplied (see teamscope.ResolveActing);
	// optional, defaulted by resolution. The store rejects a prompt set that
	// resolves outside it.
	TeamID string `json:"team_id" doc:"The id of the team whose prompts are copied and which owns the copies, required when the caller writes to more than one team; absent is the caller's one team."`
}

var opBlueprintDuplicate = httpx.NewJSONOp[duplicateBlueprintsRequest, []blueprintWithSteps](httpx.OpSpec{
	Pattern:     "POST /api/blueprints/duplicate",
	ID:          "duplicate_blueprints",
	Summary:     "Copy blueprint steps into new blueprints",
	Description: "Copies the named step prompts into new blueprints with no event handler, one for each unbroken run of steps of one blueprint, and leaves the originals as they are. Returns every new blueprint with its steps, as an array.",
	Status:      http.StatusCreated,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

// handleBlueprintDuplicate deep-copies a flat set of prompt ids into new
// trigger-less blueprint(s) following the induced-contiguous-runs rule, in one
// transaction. Originals are untouched. Returns each new blueprint with its
// ordered step list so the caller can render/locate the copies.
func (h *Handler) handleBlueprintDuplicate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opBlueprintDuplicate.Decode(w, r)
	if !ok {
		return
	}
	if len(req.PromptIDs) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "prompt_ids is required", Field: "prompt_ids"})
		return
	}

	// Viewers can't duplicate prompts into a new blueprint (TFAC-447).
	if !h.az.RequireActingTeamWrite(w, r, orgID, userID, req.TeamID, "blueprints") {
		return
	}

	var (
		out   []blueprintWithSteps
		fault blueprintFault
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		newIDs, e := tx.Blueprints.DuplicatePrompts(r.Context(), orgID, teamID, req.PromptIDs)
		if e != nil {
			switch {
			case errors.Is(e, db.ErrDuplicateNoPrompts):
				fault.raise(http.StatusBadRequest, httpx.ReasonMissingField,
					"prompt_ids is required", "prompt_ids")
				return nil
			case errors.Is(e, db.ErrDuplicatePromptNotFound):
				fault.raise(http.StatusNotFound, httpx.ReasonNotFound,
					"a prompt id does not resolve to a blueprint step", "prompt_ids")
				return nil
			case errors.Is(e, db.ErrDuplicateCrossTeam):
				fault.raise(http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef,
					"prompt_ids include a prompt outside the acting team", "prompt_ids")
				return nil
			}
			return e
		}
		out = make([]blueprintWithSteps, 0, len(newIDs))
		for _, id := range newIDs {
			bp, ge := tx.Blueprints.Get(r.Context(), orgID, id)
			if ge != nil {
				return ge
			}
			if bp == nil {
				// Just INSERTed in this tx — a nil read is a store inconsistency,
				// not a 404. Fail the tx rather than emit "blueprint": null.
				return fmt.Errorf("duplicated blueprint %s not readable after insert", id)
			}
			steps, ge := tx.Blueprints.ListSteps(r.Context(), orgID, id)
			if ge != nil {
				return ge
			}
			out = append(out, withSteps(bp, steps))
		}
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if fault.write(w) {
		return
	}
	opBlueprintDuplicate.Write(w, out)
}

// --- Blueprint runs (multi-step instances) -------------------------------

// blueprintRunResponse bundles the blueprint run row with its per-step runs
// so the run-detail UI can render the timeline in one fetch instead of N+1.
// Each step run carries its terminal conversations.outcome inline
// (Run.Outcome); there is no separate verdict channel.
type blueprintRunResponse struct {
	BlueprintRun *domain.BlueprintRun   `json:"blueprint_run" doc:"The blueprint run, as the list returns it."`
	Steps        []blueprintRunStepView `json:"steps" doc:"The run's steps in order, as the blueprint stood when the run started, or as it stands now for a run that recorded no step plan."`
}

type blueprintRunStepView struct {
	Step blueprintStepView    `json:"step" doc:"The step."`
	Run  *domain.Conversation `json:"run,omitempty" doc:"The agent conversation that ran or is running the step, absent before the step starts."`
}

// blueprintStepView is the step shape in a blueprint-run projection. It mirrors
// domain.BlueprintStep but makes created_at a pointer so a step reconstructed
// from the run's frozen step_plan — which has no live blueprint_steps row to
// source a timestamp from — omits the field instead of serializing a zero
// "0001-01-01" created_at (TFAC-313). The live-steps fallback path still
// carries the real value.
//
// Name is the step prompt's human-readable name, so a rendered step can say
// which step it is rather than showing a bare pip. Only the frozen-plan
// projection can fill it — the plan snapshots the name, while the live
// blueprint_steps row behind domain.BlueprintStep holds a prompt id and no
// name — so it is omitted on the fallback path.
type blueprintStepView struct {
	BlueprintID  string     `json:"blueprint_id" doc:"The id of the run's blueprint."`
	StepIndex    int        `json:"step_index" doc:"The step's 0-based position in the run."`
	StepPromptID string     `json:"step_prompt_id" doc:"The id of the prompt the step runs."`
	Name         string     `json:"name,omitempty" doc:"The step prompt's name when the run started, absent for a run that recorded no step plan."`
	Brief        string     `json:"brief" doc:"Instructions for this step that were added to its prompt, or empty for none."`
	CreatedAt    *time.Time `json:"created_at,omitempty" doc:"When the step was added to its blueprint, as an RFC 3339 timestamp, present only for a run that recorded no step plan."`
}

// newBlueprintStepView adapts a domain step into the API view, dropping a zero
// created_at (a step rebuilt from the frozen plan) so the response never
// carries a placeholder timestamp.
func newBlueprintStepView(s domain.BlueprintStep) blueprintStepView {
	v := blueprintStepView{
		BlueprintID:  s.BlueprintID,
		StepIndex:    s.StepIndex,
		StepPromptID: s.StepPromptID,
		Brief:        s.Brief,
	}
	if !s.CreatedAt.IsZero() {
		v.CreatedAt = &s.CreatedAt
	}
	return v
}

// newBlueprintStepViewFromPlan projects a frozen plan step into the API view.
// It exists so the plan path doesn't round-trip through domain.BlueprintStep,
// which mirrors the live blueprint_steps table and would drop the prompt name.
// blueprintID is the owning run's blueprint_id. A plan step has no live row, so
// created_at stays omitted.
func newBlueprintStepViewFromPlan(ps domain.BlueprintPlanStep, blueprintID string) blueprintStepView {
	return blueprintStepView{
		BlueprintID:  blueprintID,
		StepIndex:    ps.StepIndex,
		StepPromptID: ps.PromptID,
		Name:         ps.PromptName,
		Brief:        ps.Brief,
	}
}

var opBlueprintRunGet = httpx.NewJSONOp[httpx.NoBody, blueprintRunResponse](httpx.OpSpec{
	Pattern:     "GET /api/blueprint-runs/{id}",
	ID:          "get_blueprint_run",
	Summary:     "Get a blueprint run",
	Description: "Returns one blueprint run with its steps, each with the agent conversation that ran it. The steps are the ones the blueprint had when the run started, whatever has changed in the blueprint since.",
	PathParams:  map[string]string{"id": "The blueprint run's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBlueprintRunGet returns a blueprint run with its per-step views. The
// step list is projected from the plan frozen onto the run at mint
// (br.StepPlan), NOT the live blueprint_steps — a run executes the plan it was
// minted with, immune to later edits of the blueprint, and its rendered step
// list must match (TFAC-313). Reading the live steps here let a settled run's
// board card change its step count after the blueprint was edited (e.g. a
// cancelled 1-step run sprouting a phantom second step once a prompt was
// appended). ListSteps is only a fallback for a run with no frozen plan
// (defensive — every run minted through the delegation path freezes one).
func (h *Handler) handleBlueprintRunGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	id, ok := httpx.UUIDPathOr404(w, r, "id", "blueprint run")
	if !ok {
		return
	}

	var br *domain.BlueprintRun
	var fallbackSteps []domain.BlueprintStep
	var stepConversations []domain.Conversation
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		br, e = tx.Blueprints.GetRun(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if br == nil {
			return nil
		}
		stepConversations, e = tx.Blueprints.ConversationsForBlueprint(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		// Only hit the live steps when the run carries no frozen plan.
		if len(br.StepPlan) == 0 {
			fallbackSteps, e = tx.Blueprints.ListSteps(r.Context(), orgID, br.BlueprintID)
		}
		return e
	}); err != nil {
		httpx.InternalError(w, "blueprints", err)
		return
	}
	if br == nil {
		httpx.NotFound(w, "blueprint run")
		return
	}

	// Reconstruct the step list from the frozen plan; fall back to the live
	// steps only when the plan is absent. The plan projects straight to the
	// view: a plan step carries the prompt's name, which domain.BlueprintStep
	// has nowhere to hold.
	var steps []blueprintStepView
	if len(br.StepPlan) > 0 {
		steps = make([]blueprintStepView, len(br.StepPlan))
		for i, ps := range br.StepPlan {
			steps[i] = newBlueprintStepViewFromPlan(ps, br.BlueprintID)
		}
	} else {
		steps = make([]blueprintStepView, len(fallbackSteps))
		for i, s := range fallbackSteps {
			steps[i] = newBlueprintStepView(s)
		}
	}

	convByStep := map[int]*domain.Conversation{}
	for i := range stepConversations {
		if stepConversations[i].BlueprintStepIndex != nil {
			convByStep[*stepConversations[i].BlueprintStepIndex] = &stepConversations[i]
		}
	}

	views := make([]blueprintRunStepView, 0, len(steps))
	for _, step := range steps {
		view := blueprintRunStepView{Step: step}
		if conv, ok := convByStep[step.StepIndex]; ok {
			view.Run = conv
		}
		views = append(views, view)
	}

	opBlueprintRunGet.Write(w, blueprintRunResponse{BlueprintRun: br, Steps: views})
}

// Register mounts the blueprint and blueprint-run routes.
func (h *Handler) Register(m httpx.Mounts) {
	m.Op(opBlueprintList, h.handleBlueprintsList)
	m.Op(opBlueprintGet, h.handleBlueprintGet)
	m.Op(opBlueprintCreate, h.handleBlueprintCreate)
	m.Op(opBlueprintRename, h.handleBlueprintUpdate)
	m.Op(opBlueprintDelete, h.handleBlueprintDelete)
	// One route for the canvas's bulk read and the per-blueprint read: they
	// differ only by a filter, and a filter is what a list body is for.
	m.Op(opBlueprintStepList, h.handleBlueprintStepsAll)
	m.Op(opBlueprintStepsReplace, h.handleBlueprintStepsPut)
	m.Op(opBlueprintMerge, h.handleBlueprintMerge)
	m.Op(opBlueprintSplit, h.handleBlueprintSplit)
	m.Op(opBlueprintReconnect, h.handleBlueprintReconnect)
	m.Op(opBlueprintDuplicate, h.handleBlueprintDuplicate)
	m.Op(opBlueprintRunList, h.handleBlueprintRunsList)
	m.Op(opBlueprintRunGet, h.handleBlueprintRunGet)
}
