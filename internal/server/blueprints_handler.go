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

func (bh *blueprintsHandler) handleBlueprintsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID := teamscope.SingleParam(r)
	var blueprints []domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		listed, e := tx.Blueprints.List(r.Context(), orgID, teamID)
		if e != nil {
			return e
		}
		// One query for every trigger visible at this scope, grouped by
		// blueprint_id, instead of a per-blueprint ListForBlueprint call
		// (TFAC-524 code review: avoids N+1 / a longer-held tx as the
		// blueprint count grows). Safe to scope by the same teamID as the
		// blueprint list: a trigger's team_id is pinned to its blueprint's
		// team_id by the (blueprint_id, team_id) composite FK, so this set
		// is exactly what per-blueprint ListForBlueprint calls would have
		// returned.
		triggers, e := tx.EventHandlers.List(r.Context(), orgID, domain.EventHandlerKindTrigger, teamID)
		if e != nil {
			return e
		}
		triggersByBlueprint := make(map[string][]domain.EventHandler, len(triggers))
		for _, tr := range triggers {
			triggersByBlueprint[tr.BlueprintID] = append(triggersByBlueprint[tr.BlueprintID], tr)
		}
		// Hide a blueprint iff it has ≥1 attached trigger AND every one of its
		// triggers' EventType fails EventTypeAllowed (TFAC-524). A blueprint
		// with at least one allowed trigger stays (it has live non-gated
		// behavior); a trigger-less blueprint stays (unrelated to gating).
		// Rows persist — visibility only.
		visible := make([]domain.Blueprint, 0, len(listed))
		for _, bp := range listed {
			if blueprintGatedOff(orgID, triggersByBlueprint[bp.ID]) {
				continue
			}
			visible = append(visible, bp)
		}
		blueprints = visible
		return nil
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if blueprints == nil {
		blueprints = []domain.Blueprint{}
	}
	writeJSON(w, http.StatusOK, blueprints)
}

// blueprintGatedOff reports whether every one of triggers' event types fails
// EventTypeAllowed for orgID. A trigger-less blueprint (len(triggers) == 0)
// is never gated off by this check — that's the pre-existing orphaned/
// trigger-less state, unrelated to entitlement gating.
func blueprintGatedOff(orgID string, triggers []domain.EventHandler) bool {
	if len(triggers) == 0 {
		return false
	}
	for _, tr := range triggers {
		if entitlements.EventTypeAllowed(orgID, tr.EventType) {
			return false
		}
	}
	return true
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
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// With first_prompt the blueprint name defaults to the prompt's name; the
	// prompt itself must carry a name + body (it's the step's mission).
	// Name defaulting happens here (before resolveActingTeam inside the tx)
	// because it is a pure string assignment independent of team resolution.
	if req.FirstPrompt != nil {
		if req.FirstPrompt.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "first_prompt.name is required"})
			return
		}
		if req.FirstPrompt.Body == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "first_prompt.body is required"})
			return
		}
		if !prompts.ValidModel(req.FirstPrompt.Model) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": prompts.InvalidModelError()})
			return
		}
		if req.Name == "" {
			req.Name = req.FirstPrompt.Name
		}
	} else if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
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
			if e := tx.Prompts.Create(r.Context(), orgID, teamID, domain.Prompt{
				ID:     firstPromptID,
				Name:   req.FirstPrompt.Name,
				Body:   req.FirstPrompt.Body,
				Source: "user",
				Model:  req.FirstPrompt.Model,
			}); e != nil {
				return e
			}
		}
		if e := tx.Blueprints.Create(r.Context(), orgID, teamID, domain.Blueprint{
			ID:     id,
			Name:   req.Name,
			Source: "user",
		}); e != nil {
			return e
		}
		if firstPromptID != "" {
			if e := tx.Blueprints.ReplaceSteps(r.Context(), orgID, id, []string{firstPromptID}, nil); e != nil {
				return e
			}
		}
		var ge error
		created, ge = tx.Blueprints.Get(r.Context(), orgID, id)
		return ge
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
	id := r.PathValue("id")

	var req renameBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	// Viewers can't rename a blueprint (TFAC-447).
	if !bh.gateBlueprintWrite(w, r, orgID, userID, id) {
		return
	}

	var updated *domain.Blueprint
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.Blueprints.Get(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if e := tx.Blueprints.Rename(r.Context(), orgID, id, name); e != nil {
			return e
		}
		updated, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if updated == nil {
		notFound(w, "blueprint")
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
//     hard-deleted unconditionally, system rows included, matching
//     handleEventHandlerDelete and the prompt head-delete path. Nothing re-seeds
//     at boot, so a deleted shipped default is durable.
//
// 404 by re-read (Get filters soft-deleted) when the blueprint is missing or
// already deleted.
func (bh *blueprintsHandler) handleBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

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

// handleBlueprintStepsAll returns every step of the scope's blueprints in one
// read — the binding canvas's bulk fetch, which avoids an N+1 of
// GET .../{id}/steps over the blueprint list. Always an array; the client
// groups by blueprint_id.
func (bh *blueprintsHandler) handleBlueprintStepsAll(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID := teamscope.SingleParam(r)
	var steps []domain.BlueprintStep
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		steps, e = tx.Blueprints.ListAllSteps(r.Context(), orgID, teamID)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if steps == nil {
		steps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

// handleBlueprintStepsGet returns the ordered step list for a blueprint.
// Always returns an array (never null) so frontend code can iterate without a
// nil check.
func (bh *blueprintsHandler) handleBlueprintStepsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var blueprint *domain.Blueprint
	var steps []domain.BlueprintStep
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if blueprint == nil {
			return nil
		}
		steps, e = tx.Blueprints.ListSteps(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if blueprint == nil {
		notFound(w, "blueprint")
		return
	}
	if steps == nil {
		steps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, steps)
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
	id := r.PathValue("id")

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
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if len(req.Steps) > maxBlueprintSteps {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "blueprint may not exceed " + strconv.Itoa(maxBlueprintSteps) + " steps",
		})
		return
	}

	stepIDs := make([]string, 0, len(req.Steps))
	briefs := make([]string, 0, len(req.Steps))
	for _, step := range req.Steps {
		if step.StepPromptID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "step_prompt_id is required for every step",
			})
			return
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}

	// Validate each step's prompt exists + is same-team, then replace in one
	// tx so all lookups and the final write share claims.
	var validationErr string
	var validationStatus int
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		for i, sid := range stepIDs {
			stepPrompt, e := tx.Prompts.Get(r.Context(), orgID, sid)
			if e != nil {
				return e
			}
			if stepPrompt == nil {
				validationErr = "step " + strconv.Itoa(i) + " references a non-existent prompt"
				validationStatus = http.StatusUnprocessableEntity
				return nil
			}
			// Same-team guard: a blueprint may only step through prompts its own
			// team owns. The DB enforces this via the (step_prompt_id, team_id)
			// composite FK on ReplaceSteps; pre-check for a clean 422.
			if blueprint.TeamID != "" && stepPrompt.TeamID != "" && stepPrompt.TeamID != blueprint.TeamID {
				validationErr = "step " + strconv.Itoa(i) + " references a prompt owned by another team"
				validationStatus = http.StatusUnprocessableEntity
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
				validationErr = "step " + strconv.Itoa(i) + " references a prompt that already belongs to another blueprint — copy it to reuse."
				validationStatus = http.StatusUnprocessableEntity
				return nil
			}
		}
		if validationErr != "" {
			return nil
		}
		return tx.Blueprints.ReplaceSteps(r.Context(), orgID, id, stepIDs, briefs)
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if validationErr != "" {
		writeJSON(w, validationStatus, map[string]string{"error": validationErr})
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.SourceBlueprintID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_blueprint_id is required"})
		return
	}
	if req.SourceBlueprintID == hostID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "cannot merge a blueprint into itself"})
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
		failStatus   int
		failMsg      string
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
			failStatus, failMsg = http.StatusUnprocessableEntity, "host and source blueprints belong to different teams"
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
			failStatus, failMsg = http.StatusUnprocessableEntity, "the absorbed blueprint has its own event trigger; detach it first"
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
			failStatus, failMsg = http.StatusUnprocessableEntity, "merged blueprint would exceed "+strconv.Itoa(maxBlueprintSteps)+" steps"
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
	if failMsg != "" {
		writeJSON(w, failStatus, map[string]string{"error": failMsg})
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
	id := r.PathValue("id")

	var req splitBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	// *int so a missing field is distinguishable from an explicit 0 (which is a
	// real index whose 422 message is about non-empty halves, not absence).
	if req.AtStepIndex == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at_step_index is required"})
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
		failStatus           int
		failMsg              string
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
			failStatus, failMsg = http.StatusUnprocessableEntity, "at_step_index must split the blueprint into two non-empty halves"
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
	if failMsg != "" {
		writeJSON(w, failStatus, map[string]string{"error": failMsg})
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
	id := r.PathValue("id")

	var req reconnectBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.AtStepIndex == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at_step_index is required"})
		return
	}
	if req.TargetBlueprintID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_blueprint_id is required"})
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
		failStatus   int
		failMsg      string
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
			failStatus, failMsg = http.StatusUnprocessableEntity, "cannot reconnect a blueprint into itself"
			return nil
		}
		// Same-team guard (MergeInto leaves team_id on the reparented steps
		// unchanged).
		if host.TeamID != "" && target.TeamID != "" && host.TeamID != target.TeamID {
			failStatus, failMsg = http.StatusUnprocessableEntity, "host and target blueprints belong to different teams"
			return nil
		}
		steps, e := tx.Blueprints.ListSteps(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		// 0 < k < N: both the surviving upstream and the orphaned downstream must
		// be non-empty (a sequence edge always sits at such a boundary).
		if atIndex <= 0 || atIndex >= len(steps) {
			failStatus, failMsg = http.StatusUnprocessableEntity, "at_step_index must split the blueprint into two non-empty halves"
			return nil
		}
		// Target must be trigger-less to be absorbed (mirrors merge). The
		// one-trigger-per-blueprint partial-unique index is the hard backstop.
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.TargetBlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			failStatus, failMsg = http.StatusUnprocessableEntity, "the target blueprint has its own event trigger; detach it first"
			return nil
		}
		// Cap: the surviving upstream (atIndex steps) + the absorbed target must
		// stay editable via the steps-PUT, which rejects lists over the max.
		targetSteps, e := tx.Blueprints.ListSteps(r.Context(), orgID, req.TargetBlueprintID)
		if e != nil {
			return e
		}
		if atIndex+len(targetSteps) > maxBlueprintSteps {
			failStatus, failMsg = http.StatusUnprocessableEntity, "reconnected blueprint would exceed "+strconv.Itoa(maxBlueprintSteps)+" steps"
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
	if failMsg != "" {
		writeJSON(w, failStatus, map[string]string{"error": failMsg})
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
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if len(req.PromptIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_ids is required"})
		return
	}

	// Viewers can't duplicate prompts into a new blueprint (TFAC-447).
	if !gateActingTeamWrite(w, r, bh.tx, bh.az, orgID, userID, req.TeamID, "blueprints") {
		return
	}

	var (
		out        []blueprintWithSteps
		failStatus int
		failMsg    string
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
				failStatus, failMsg = http.StatusBadRequest, "prompt_ids is required"
				return nil
			case errors.Is(e, db.ErrDuplicatePromptNotFound):
				failStatus, failMsg = http.StatusNotFound, "a prompt id does not resolve to a blueprint step"
				return nil
			case errors.Is(e, db.ErrDuplicateCrossTeam):
				failStatus, failMsg = http.StatusUnprocessableEntity, "prompt_ids span more than one team"
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
	if failMsg != "" {
		writeJSON(w, failStatus, map[string]string{"error": failMsg})
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// --- Blueprint runs (multi-step instances) -------------------------------

// blueprintRunResponse bundles the blueprint run row with its per-step runs
// so the run-detail UI can render the timeline in one fetch instead of N+1.
// Each step run carries its terminal runs.outcome inline (Run.Outcome); there
// is no separate verdict channel.
type blueprintRunResponse struct {
	BlueprintRun *domain.BlueprintRun   `json:"blueprint_run"`
	Steps        []blueprintRunStepView `json:"steps"`
}

type blueprintRunStepView struct {
	Step blueprintStepView `json:"step"`
	Run  *domain.AgentRun  `json:"run,omitempty"`
}

// blueprintStepView is the step shape in a blueprint-run projection. It mirrors
// domain.BlueprintStep but makes created_at a pointer so a step reconstructed
// from the run's frozen step_plan — which has no live blueprint_steps row to
// source a timestamp from — omits the field instead of serializing a zero
// "0001-01-01" created_at (TFAC-313). The live-steps fallback path still
// carries the real value.
type blueprintStepView struct {
	BlueprintID  string     `json:"blueprint_id"`
	StepIndex    int        `json:"step_index"`
	StepPromptID string     `json:"step_prompt_id"`
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
	id := r.PathValue("id")

	var br *domain.BlueprintRun
	var fallbackSteps []domain.BlueprintStep
	var stepRuns []domain.AgentRun
	if err := bh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		br, e = tx.Blueprints.GetRun(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if br == nil {
			return nil
		}
		stepRuns, e = tx.Blueprints.RunsForBlueprint(r.Context(), orgID, id)
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
	// steps only when the plan is absent.
	steps := fallbackSteps
	if len(br.StepPlan) > 0 {
		steps = make([]domain.BlueprintStep, len(br.StepPlan))
		for i, ps := range br.StepPlan {
			steps[i] = ps.Step(br.BlueprintID)
		}
	}

	runByStep := map[int]*domain.AgentRun{}
	for i := range stepRuns {
		if stepRuns[i].BlueprintStepIndex != nil {
			runByStep[*stepRuns[i].BlueprintStepIndex] = &stepRuns[i]
		}
	}

	views := make([]blueprintRunStepView, 0, len(steps))
	for _, step := range steps {
		view := blueprintRunStepView{Step: newBlueprintStepView(step)}
		if run, ok := runByStep[step.StepIndex]; ok {
			view.Run = run
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
	id := r.PathValue("id")

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
		writeJSON(w, http.StatusConflict, map[string]string{"error": "blueprint run already terminal"})
		return
	}

	// Availability check after the existence + terminal checks so a missing or
	// already-terminal run gets a 404/409 rather than a 503.
	spawner := bh.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := spawner.CancelBlueprint(orgID, id, userID); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}
