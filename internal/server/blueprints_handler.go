package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// blueprintsHandler serves the blueprint + blueprint-run endpoints. spawner is
// read through a getter so the handler always sees the current delegation
// spawner, which is wired onto the server after construction.
type blueprintsHandler struct {
	tx      db.TxRunner
	az      *authz.Checker
	spawner func() *delegate.Spawner
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
func (bh *blueprintsHandler) gateBlueprintWrite(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var bp *domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		bp, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return false
	}
	if bp == nil {
		return true // let the handler's own nil-check render the 404
	}
	return bh.az.RequireTeamWrite(w, r, orgID, userID, bp.TeamID)
}

// --- Blueprint header CRUD -----------------------------------------------

// blueprintListRequest is the body of POST /api/blueprints/list. TeamID
// narrows to one team's blueprints on the multi-team page; empty returns
// everything visible.
type blueprintListRequest struct {
	TeamID string `json:"team_id"`

	httpx.PageRequest
}

type blueprintListFilterKey struct {
	TeamID string `json:"team_id"`
}

// POST /api/blueprints/list
func (bh *blueprintsHandler) handleBlueprintsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req blueprintListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	teamIDFilterField(&v, req.TeamID)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(blueprintListFilterKey{TeamID: req.TeamID}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The entitlement gate (TFAC-524) hides a blueprint whose every attached
	// trigger fires on an event type this org isn't licensed for. It rides
	// into the query as a filter rather than trimming the page afterwards:
	// a post-window trim makes the page short and total_count a lie about
	// rows the caller cannot see.
	filter := db.BlueprintListFilter{TeamID: req.TeamID, GatedEventTypes: gatedEventTypes(orgID)}

	var (
		blueprints []domain.Blueprint
		total      int
	)
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprints, total, e = tx.Blueprints.List(r.Context(), orgID, filter, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	httpx.WriteList(w, page, blueprints, total)
}

// gatedEventTypes returns the event types orgID is NOT entitled to fire on.
// The catalog is a fixed, small registry, so deriving the complement here is
// cheaper than teaching the store about entitlements — and it keeps the gate's
// policy in one place while the store only applies a set.
func gatedEventTypes(orgID string) []string {
	var gated []string
	for _, et := range domain.AllEventTypes() {
		if !entitlements.EventTypeAllowed(orgID, et.ID) {
			gated = append(gated, et.ID)
		}
	}
	return gated
}

// handleBlueprintGet is the canonical single read: one blueprint, in the
// list's row shape. It is deliberately NOT entitlement-gated the way the list
// is — the gate is a visibility rule for a browse surface, and a caller
// holding a blueprint's id (from a run, a trigger, a link) must be able to
// resolve its name rather than meet a 404 that reads as deletion.
//
// GET /api/blueprints/{id}
func (bh *blueprintsHandler) handleBlueprintGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var blueprint *domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if blueprint == nil {
		notFound(w, "blueprint")
		return
	}
	writeJSON(w, http.StatusOK, blueprint)
}

// firstPromptInput is the optional inline prompt the canvas's "New Prompt"
// gesture posts to the blueprint-create endpoint. Present ⇒ the handler creates
// prompt + 1-step blueprint + step atomically (no orphan prompts — an
// un-wrapped prompt can't be a canvas node or a trigger target). Absent ⇒ a
// bare blueprint, as before.
type firstPromptInput struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
}

type createBlueprintRequest struct {
	Name string `json:"name"`
	// TeamID is the acting team the write picker supplied (see resolveActingTeam).
	TeamID string `json:"team_id"`
	// FirstPrompt, when set, auto-wraps a new prompt as this blueprint's sole
	// step in one transaction. Optional — bare-blueprint callers omit it.
	FirstPrompt *firstPromptInput `json:"first_prompt"`
}

// blueprintCreateResponse returns the created blueprint plus the id of the
// auto-wrapped first prompt (empty when no first_prompt was supplied).
type blueprintCreateResponse struct {
	*domain.Blueprint
	FirstPromptID string `json:"first_prompt_id,omitempty"`
}

func (bh *blueprintsHandler) handleBlueprintCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	var req createBlueprintRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// With first_prompt the blueprint name defaults to the prompt's name; the
	// prompt itself must carry a name + body (it's the step's mission).
	// Name defaulting happens here (before resolveActingTeam inside the tx)
	// because it is a pure string assignment independent of team resolution.
	if req.FirstPrompt != nil {
		if req.FirstPrompt.Name == "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "first_prompt.name is required", Field: "first_prompt.name"})
			return
		}
		if req.FirstPrompt.Body == "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "first_prompt.body is required", Field: "first_prompt.body"})
			return
		}
		if universe := deploymentUniverse(); !prompts.ValidModel(universe, req.FirstPrompt.Model) {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: prompts.InvalidModelError(universe), Field: "first_prompt.model"})
			return
		}
		if req.Name == "" {
			req.Name = req.FirstPrompt.Name
		}
	} else if req.Name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name"})
		return
	}

	id := uuid.New().String()
	userID := ClaimsFrom(r.Context()).Subject

	// Reject viewers before authoring a blueprint (TFAC-447): resolve the acting
	// team read-only (no last-acting stamp — we may 403) and gate. The main tx
	// re-resolves (and stamps) only for callers that pass.
	if !gateActingTeamWrite(w, r, bh.tx, bh.az, orgID, userID, req.TeamID, "blueprints") {
		return
	}

	var created *domain.Blueprint
	var firstPromptID string
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusCreated, blueprintCreateResponse{Blueprint: created, FirstPromptID: firstPromptID})
}

type renameBlueprintRequest struct {
	Name string `json:"name"`
}

// handleBlueprintUpdate renames a blueprint header. A blueprint's name is
// independent of its entry prompt's (auto-wrap defaults them equal, but the box
// chrome lets a user rename the blueprint without touching the prompt).
func (bh *blueprintsHandler) handleBlueprintUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var req renameBlueprintRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name"})
		return
	}

	// Viewers can't rename a blueprint (TFAC-447).
	if !bh.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	var updated domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Blueprints.Rename(r.Context(), orgID, id, name)
		return e
	}); err != nil {
		// The rename itself reports a missing or soft-deleted row now, so the
		// probe read this handler used to bracket the write with is gone.
		if errors.Is(err, db.ErrNoSuchBlueprint) {
			notFound(w, "blueprint")
			return
		}
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleBlueprintDelete soft-deletes a whole blueprint and cascades, in one
// tx (team-scoped, mutating, RLS under the acting team). The cascade is
// richer than a plain delete because team blueprints carry copy-only step
// prompts and a bound trigger:
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
//   - The bound trigger is detached: every handler ListForBlueprint resolves is
//     deleted via EventHandlers.Delete, matching handleEventHandlerDelete and
//     the prompt head-delete path — a system_slug (shipped) trigger
//     soft-deletes so the shipped-content sync never resurrects it, a
//     user-created trigger hard-deletes. The blueprint header stays present
//     (soft-deleted, not gone), so the trigger's (blueprint_id, org_id) /
//     (blueprint_id, team_id) FKs are always satisfied regardless of order.
//
// 404 by re-read (Get filters soft-deleted) when the blueprint is missing or
// already deleted.
func (bh *blueprintsHandler) handleBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	// Viewers can't delete a blueprint (TFAC-447).
	if !bh.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	var found bool
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.Blueprints.Get(r.Context(), orgID, id)
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

		// Detach the bound trigger(s) — hard-delete, system rows included.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		for _, tr := range triggers {
			if de := tx.EventHandlers.Delete(r.Context(), orgID, tr.ID); de != nil {
				return de
			}
		}

		// Retire the header last (audit row + steps stay; deleted_at stamped).
		return tx.Blueprints.Delete(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if !found {
		notFound(w, "blueprint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
	TeamID string `json:"team_id"`
	// BlueprintIDs narrows to specific blueprints. Empty is the canvas's
	// bulk read; a single id is the old per-blueprint read.
	BlueprintIDs []string `json:"blueprint_ids"`

	httpx.PageRequest
}

type blueprintStepListFilterKey struct {
	TeamID       string   `json:"team_id"`
	BlueprintIDs []string `json:"blueprint_ids"`
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
func (bh *blueprintsHandler) handleBlueprintStepsAll(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req blueprintStepListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	teamIDFilterField(&v, req.TeamID)
	blueprintIDs := canonicalStrings(req.BlueprintIDs)
	if len(blueprintIDs) > maxBlueprintStepIDs {
		v.OutOfRange("blueprint_ids", fmt.Sprintf("at most %d blueprint ids per request", maxBlueprintStepIDs))
	}
	for _, id := range blueprintIDs {
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("blueprint_ids", fmt.Sprintf("blueprint id %q is not a valid blueprint id", id))
		}
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(blueprintStepListFilterKey{
		TeamID: req.TeamID, BlueprintIDs: blueprintIDs,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		steps []domain.BlueprintStep
		total int
	)
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		steps, total, e = tx.Blueprints.ListAllSteps(r.Context(), orgID,
			db.BlueprintStepListFilter{TeamID: req.TeamID, BlueprintIDs: blueprintIDs},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	httpx.WriteList(w, page, steps, total)
}

// blueprintRunListRequest is the body of POST /api/blueprint-runs/list. Before
// this route the collection was unenumerable — a run could only be read by an
// id the caller already held.
type blueprintRunListRequest struct {
	BlueprintID string   `json:"blueprint_id"`
	Statuses    []string `json:"statuses"`

	httpx.PageRequest
}

type blueprintRunListFilterKey struct {
	BlueprintID string   `json:"blueprint_id"`
	Statuses    []string `json:"statuses"`
}

// POST /api/blueprint-runs/list
func (bh *blueprintsHandler) handleBlueprintRunsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req blueprintRunListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.BlueprintID != "" {
		if _, err := uuid.Parse(req.BlueprintID); err != nil {
			v.Invalid("blueprint_id", fmt.Sprintf("blueprint_id %q is not a valid blueprint id", req.BlueprintID))
		}
	}
	statuses := canonicalStrings(req.Statuses)
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
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		runs, total, e = tx.Blueprints.ListRuns(r.Context(), orgID,
			db.BlueprintRunListFilter{BlueprintID: req.BlueprintID, Statuses: statuses},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	// The rows are the run objects themselves — the same shape the single
	// read carries under its "blueprint_run" key. The single read adds the
	// per-step views, which are a composite of the run and its conversations
	// and belong to opening one run, not to enumerating many.
	httpx.WriteList(w, page, runs, total)
}

type blueprintStepInput struct {
	StepPromptID string `json:"step_prompt_id"`
	Brief        string `json:"brief"`
}

type blueprintStepsPutRequest struct {
	Steps []blueprintStepInput `json:"steps"`
}

// handleBlueprintStepsPut replaces the blueprint's step list. Validates that
// the blueprint exists and that every step references a prompt the blueprint's
// own team owns (same-team guard).
func (bh *blueprintsHandler) handleBlueprintStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var blueprint *domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if blueprint == nil {
		notFound(w, "blueprint")
		return
	}
	// Viewers can't rewrite a blueprint's steps (TFAC-447). Gate on the
	// already-loaded blueprint's team — no extra round-trip.
	if !bh.az.RequireTeamWrite(w, r, orgID, userID, blueprint.TeamID) {
		return
	}

	var req blueprintStepsPutRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	if len(req.Steps) > maxBlueprintSteps {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "blueprint may not exceed " + strconv.Itoa(maxBlueprintSteps) + " steps", Field: "steps"})
		return
	}

	stepIDs := make([]string, 0, len(req.Steps))
	briefs := make([]string, 0, len(req.Steps))
	for _, step := range req.Steps {
		if step.StepPromptID == "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "step_prompt_id is required for every step", Field: "steps"})
			return
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}

	// Validate each step's prompt exists + is same-team, then replace in one
	// tx so all lookups and the final write share claims.
	var fault blueprintFault
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		_, e := tx.Blueprints.ReplaceSteps(r.Context(), orgID, id, stepIDs, briefs)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if fault.write(w) {
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
	Blueprint *domain.Blueprint      `json:"blueprint"`
	Steps     []domain.BlueprintStep `json:"steps"`
}

type mergeBlueprintRequest struct {
	// SourceBlueprintID (B) is absorbed at {id}'s (the host's) tail and retired.
	SourceBlueprintID string `json:"source_blueprint_id"`
}

// handleBlueprintMerge absorbs a trigger-less source blueprint onto the tail of
// the host ({id}) and retires the source, atomically. The host keeps its
// identity, name, and trigger.
func (bh *blueprintsHandler) handleBlueprintMerge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	hostID := r.PathValue("id")

	var req mergeBlueprintRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
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
	if !bh.gateBlueprintWrite(w, r, orgID, userID, hostID) {
		return
	}

	var (
		host, source *domain.Blueprint
		merged       *domain.Blueprint
		steps        []domain.BlueprintStep
		fault        blueprintFault
	)
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		// Source must be trigger-less. Unreachable from the canvas (you can only
		// connect a tail into a trigger-less entry), but the endpoint is
		// third-party-callable so it asserts rather than corrupting state. The
		// ≤1-trigger-per-blueprint partial-unique is the hard backstop.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.SourceBlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonConflict,
				"the absorbed blueprint has its own event trigger; detach it first", "source_blueprint_id")
			return nil
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
		if e := tx.Blueprints.MergeInto(r.Context(), orgID, hostID, req.SourceBlueprintID); e != nil {
			return e
		}
		if merged, e = tx.Blueprints.Get(r.Context(), orgID, hostID); e != nil {
			return e
		}
		steps, e = tx.Blueprints.ListSteps(r.Context(), orgID, hostID)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if host == nil {
		notFound(w, "blueprint")
		return
	}
	if source == nil {
		notFound(w, "source blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	if steps == nil {
		steps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, blueprintWithSteps{Blueprint: merged, Steps: steps})
}

type splitBlueprintRequest struct {
	// AtStepIndex k splits the boundary before step k: steps [0,k) stay on
	// {id}, steps [k,N) move to a new trigger-less blueprint. Pointer so a
	// missing field 400s ("required") instead of silently defaulting to 0.
	AtStepIndex *int `json:"at_step_index"`
}

// blueprintSplitResponse returns both halves of a split: the upstream blueprint
// (trigger retained) and the new trigger-less downstream blueprint.
type blueprintSplitResponse struct {
	Upstream   blueprintWithSteps `json:"upstream"`
	Downstream blueprintWithSteps `json:"downstream"`
}

// handleBlueprintSplit partitions a blueprint at an index into the upstream
// (trigger-retained) half and a new trigger-less downstream blueprint,
// atomically.
func (bh *blueprintsHandler) handleBlueprintSplit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var req splitBlueprintRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
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
	if !bh.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	newID := uuid.New().String()
	var (
		bp                   *domain.Blueprint
		upstream, downstream *domain.Blueprint
		upSteps, downSteps   []domain.BlueprintStep
		fault                blueprintFault
	)
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if bp, e = tx.Blueprints.Get(r.Context(), orgID, id); e != nil || bp == nil {
			return e
		}
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
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
		internalError(w, "blueprints", err)
		return
	}
	if bp == nil {
		notFound(w, "blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	if upSteps == nil {
		upSteps = []domain.BlueprintStep{}
	}
	if downSteps == nil {
		downSteps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, blueprintSplitResponse{
		Upstream:   blueprintWithSteps{Blueprint: upstream, Steps: upSteps},
		Downstream: blueprintWithSteps{Blueprint: downstream, Steps: downSteps},
	})
}

type reconnectBlueprintRequest struct {
	// AtStepIndex k re-points the boundary before step k: steps [0,k) stay on
	// {id} and continue into the target's chain; steps [k,N) peel off as a new
	// orphaned trigger-less blueprint. Pointer so a missing field 400s
	// ("required") instead of silently defaulting to 0.
	AtStepIndex *int `json:"at_step_index"`
	// TargetBlueprintID (C) is the trigger-less blueprint whose entry the
	// upstream half now connects to; its steps are absorbed onto {id}'s tail.
	TargetBlueprintID string `json:"target_blueprint_id"`
}

// blueprintReconnectResponse returns the updated host (the upstream half with
// the target's steps absorbed onto its tail) plus the id of the peeled-off
// orphan downstream blueprint, so the caller can refresh + surface a toast.
type blueprintReconnectResponse struct {
	Host              blueprintWithSteps `json:"host"`
	OrphanBlueprintID string             `json:"orphan_blueprint_id"`
}

// handleBlueprintReconnect re-points a sequence edge's head onto another
// blueprint's entry — the canvas "drag a step arrow's head to a new prompt"
// gesture (full re-target). It splits {id} at at_step_index (steps [k,N) peel
// off as a new orphaned trigger-less blueprint) and merges target_blueprint_id
// onto the surviving upstream half, atomically: SplitAt + MergeInto run in one
// tx (inTx reuses the WithTx tx), so a mid-sequence failure can't leave a
// half-split blueprint behind. The host keeps its id + trigger.
func (bh *blueprintsHandler) handleBlueprintReconnect(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint")
	if !ok {
		return
	}

	var req reconnectBlueprintRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.AtStepIndex == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "at_step_index is required", Field: "at_step_index"})
		return
	}
	if req.TargetBlueprintID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "target_blueprint_id is required", Field: "target_blueprint_id"})
		return
	}

	// Viewers can't restructure blueprints (TFAC-447). Gate on the host; the
	// same-team guard below refuses a cross-team target.
	if !bh.gateBlueprintWrite(w, r, orgID, userID, id) {
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
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		// 0 < k < N: both the surviving upstream and the orphaned downstream must
		// be non-empty (a sequence edge always sits at such a boundary).
		if atIndex <= 0 || atIndex >= len(steps) {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonOutOfRange,
				"at_step_index must split the blueprint into two non-empty halves", "at_step_index")
			return nil
		}
		// Target must be trigger-less to be absorbed (mirrors merge). The
		// one-trigger-per-blueprint partial-unique index is the hard backstop.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.TargetBlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			fault.raise(http.StatusUnprocessableEntity, httpx.ReasonConflict,
				"the target blueprint has its own event trigger; detach it first", "target_blueprint_id")
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
		internalError(w, "blueprints", err)
		return
	}
	if host == nil {
		notFound(w, "blueprint")
		return
	}
	if target == nil {
		notFound(w, "target blueprint")
		return
	}
	if fault.write(w) {
		return
	}
	if hostSteps == nil {
		hostSteps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, blueprintReconnectResponse{
		Host:              blueprintWithSteps{Blueprint: hostOut, Steps: hostSteps},
		OrphanBlueprintID: orphanID,
	})
}

// --- Blueprint duplication (deep-copy) -----------------------------------

type duplicateBlueprintsRequest struct {
	// PromptIDs is a flat set; the endpoint derives the output structure (the
	// induced contiguous runs) from each prompt's resolved (blueprint, index).
	// Callers never pass blueprint ids or ranges.
	PromptIDs []string `json:"prompt_ids"`
	// TeamID is the acting team the write picker supplied (see resolveActingTeam);
	// optional, defaulted by resolution. The store rejects a prompt set that
	// resolves outside it.
	TeamID string `json:"team_id"`
}

// handleBlueprintDuplicate deep-copies a flat set of prompt ids into new
// trigger-less blueprint(s) following the induced-contiguous-runs rule, in one
// transaction. Originals are untouched. Returns each new blueprint with its
// ordered step list so the caller can render/locate the copies.
func (bh *blueprintsHandler) handleBlueprintDuplicate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req duplicateBlueprintsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if len(req.PromptIDs) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "prompt_ids is required", Field: "prompt_ids"})
		return
	}

	// Viewers can't duplicate prompts into a new blueprint (TFAC-447).
	if !gateActingTeamWrite(w, r, bh.tx, bh.az, orgID, userID, req.TeamID, "blueprints") {
		return
	}

	var (
		out   []blueprintWithSteps
		fault blueprintFault
	)
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
					"prompt_ids span more than one team", "prompt_ids")
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
			if steps == nil {
				steps = []domain.BlueprintStep{}
			}
			out = append(out, blueprintWithSteps{Blueprint: bp, Steps: steps})
		}
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "blueprints", err)
		return
	}
	if fault.write(w) {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// --- Blueprint runs (multi-step instances) -------------------------------

// blueprintRunResponse bundles the blueprint run row with its per-step runs
// so the run-detail UI can render the timeline in one fetch instead of N+1.
// Each step run carries its terminal conversations.outcome inline
// (Run.Outcome); there is no separate verdict channel.
type blueprintRunResponse struct {
	BlueprintRun *domain.BlueprintRun   `json:"blueprint_run"`
	Steps        []blueprintRunStepView `json:"steps"`
}

type blueprintRunStepView struct {
	Step blueprintStepView    `json:"step"`
	Run  *domain.Conversation `json:"run,omitempty"`
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
	BlueprintID  string     `json:"blueprint_id"`
	StepIndex    int        `json:"step_index"`
	StepPromptID string     `json:"step_prompt_id"`
	Name         string     `json:"name,omitempty"`
	Brief        string     `json:"brief"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
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

// handleBlueprintRunGet returns a blueprint run with its per-step views. The
// step list is projected from the plan frozen onto the run at mint
// (br.StepPlan), NOT the live blueprint_steps — a run executes the plan it was
// minted with, immune to later edits of the blueprint, and its rendered step
// list must match (TFAC-313). Reading the live steps here let a settled run's
// board card change its step count after the blueprint was edited (e.g. a
// cancelled 1-step run sprouting a phantom second step once a prompt was
// appended). ListSteps is only a fallback for a run with no frozen plan
// (defensive — every run minted through the delegation path freezes one).
func (bh *blueprintsHandler) handleBlueprintRunGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint run")
	if !ok {
		return
	}

	var br *domain.BlueprintRun
	var fallbackSteps []domain.BlueprintStep
	var stepConversations []domain.Conversation
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		internalError(w, "blueprints", err)
		return
	}
	if br == nil {
		notFound(w, "blueprint run")
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

	writeJSON(w, http.StatusOK, blueprintRunResponse{BlueprintRun: br, Steps: views})
}

func (bh *blueprintsHandler) handleBlueprintRunCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "blueprint run")
	if !ok {
		return
	}

	var br *domain.BlueprintRun
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		br, e = tx.Blueprints.GetRun(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if br == nil {
		notFound(w, "blueprint run")
		return
	}

	switch br.Status {
	case domain.BlueprintRunStatusCompleted, domain.BlueprintRunStatusFailed,
		domain.BlueprintRunStatusAborted, domain.BlueprintRunStatusCancelled:
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonAlreadyTerminal, Message: "blueprint run already terminal"})
		return
	}

	// Availability check after the existence + terminal checks so a missing or
	// already-terminal run gets a 404/409 rather than a 503.
	spawner := bh.spawner()
	if spawner == nil {
		writeDelegationUnavailable(w)
		return
	}
	if err := spawner.CancelBlueprintRun(orgID, id, userID); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}
