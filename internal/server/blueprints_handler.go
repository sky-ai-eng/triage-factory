package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const maxBlueprintSteps = 50

// --- Blueprint header CRUD -----------------------------------------------

func (s *Server) handleBlueprintsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID := singleTeamParam(r)
	var blueprints []domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprints, e = tx.Blueprints.List(r.Context(), orgID, teamID)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if blueprints == nil {
		blueprints = []domain.Blueprint{}
	}
	writeJSON(w, http.StatusOK, blueprints)
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

func (s *Server) handleBlueprintCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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
		if !validPromptModel(req.FirstPrompt.Model) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
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
	var created *domain.Blueprint
	var firstPromptID string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
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
		if writeIfActingTeamError(w, err) {
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
// chrome lets a user rename the blueprint without touching the prompt). The
// org-template family has the same endpoint; this is the team-scope mirror.
func (s *Server) handleBlueprintUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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

	var updated *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

// --- Blueprint steps -----------------------------------------------------

// handleBlueprintStepsAll returns every step of the scope's blueprints in one
// read — the binding canvas's bulk fetch, which avoids an N+1 of
// GET .../{id}/steps over the blueprint list. Always an array; the client
// groups by blueprint_id.
func (s *Server) handleBlueprintStepsAll(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID := singleTeamParam(r)
	var steps []domain.BlueprintStep
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
func (s *Server) handleBlueprintStepsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var blueprint *domain.Blueprint
	var steps []domain.BlueprintStep
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
func (s *Server) handleBlueprintStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var blueprint *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
func (s *Server) handleBlueprintMerge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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

	var (
		host, source *domain.Blueprint
		merged       *domain.Blueprint
		steps        []domain.BlueprintStep
		failStatus   int
		failMsg      string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
func (s *Server) handleBlueprintSplit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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

	newID := uuid.New().String()
	var (
		bp                   *domain.Blueprint
		upstream, downstream *domain.Blueprint
		upSteps, downSteps   []domain.BlueprintStep
		failStatus           int
		failMsg              string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
func (s *Server) handleBlueprintDuplicate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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

	var (
		out        []blueprintWithSteps
		failStatus int
		failMsg    string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
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
		if writeIfActingTeamError(w, err) {
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
	Step domain.BlueprintStep `json:"step"`
	Run  *domain.AgentRun     `json:"run,omitempty"`
}

func (s *Server) handleBlueprintRunGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var br *domain.BlueprintRun
	var steps []domain.BlueprintStep
	var stepRuns []domain.AgentRun
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		br, e = tx.Blueprints.GetRun(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if br == nil {
			return nil
		}
		steps, e = tx.Blueprints.ListSteps(r.Context(), orgID, br.BlueprintID)
		if e != nil {
			return e
		}
		stepRuns, e = tx.Blueprints.RunsForBlueprint(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if br == nil {
		notFound(w, "blueprint run")
		return
	}
	runByStep := map[int]*domain.AgentRun{}
	for i := range stepRuns {
		if stepRuns[i].BlueprintStepIndex != nil {
			runByStep[*stepRuns[i].BlueprintStepIndex] = &stepRuns[i]
		}
	}

	views := make([]blueprintRunStepView, 0, len(steps))
	for _, step := range steps {
		view := blueprintRunStepView{Step: step}
		if run, ok := runByStep[step.StepIndex]; ok {
			view.Run = run
		}
		views = append(views, view)
	}

	writeJSON(w, http.StatusOK, blueprintRunResponse{BlueprintRun: br, Steps: views})
}

func (s *Server) handleBlueprintRunCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	id := r.PathValue("id")

	var br *domain.BlueprintRun
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	if err := s.spawner.CancelBlueprint(orgID, id, userID); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}
