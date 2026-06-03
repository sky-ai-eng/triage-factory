package server

import (
	"context"
	"errors"
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

type createBlueprintRequest struct {
	Name string `json:"name"`
	// TeamID is the acting team the write picker supplied (see resolveActingTeam).
	TeamID string `json:"team_id"`
	// Steps creates the blueprint and its ordered steps in one atomic request.
	// If step validation fails the header insert rolls back, so a failed save
	// never leaves a zero-step/orphan blueprint on the graph. Optional — an
	// omitted/empty list creates a header with no steps.
	Steps []blueprintStepInput `json:"steps"`
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
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	id := uuid.New().String()
	userID := ClaimsFrom(r.Context()).Subject
	var created *domain.Blueprint
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		if e := tx.Blueprints.Create(r.Context(), orgID, teamID, domain.Blueprint{
			ID:     id,
			Name:   req.Name,
			Source: "user",
		}); e != nil {
			return e
		}
		// Atomic create-with-steps: a validation failure returns an error that
		// rolls back the header insert above, so no orphan blueprint survives.
		if len(req.Steps) > 0 {
			if e := validateAndReplaceBlueprintSteps(r.Context(), tx, orgID, id, teamID, req.Steps); e != nil {
				return e
			}
		}
		var ge error
		created, ge = tx.Blueprints.Get(r.Context(), orgID, id)
		return ge
	})
	if writeIfActingTeamError(w, err) {
		return
	}
	var ve *blueprintStepValidation
	if errors.As(err, &ve) {
		writeJSON(w, ve.status, map[string]string{"error": ve.msg})
		return
	}
	if err != nil {
		internalError(w, "blueprints", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleBlueprintGet returns one blueprint header by id. This is the read the
// editor drawer opens with — clicking a node in the binding graph carries a
// blueprint id, so the drawer loads the header here and its steps from
// handleBlueprintStepsGet.
func (s *Server) handleBlueprintGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var bp *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		bp, e = tx.Blueprints.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if bp == nil {
		notFound(w, "blueprint")
		return
	}
	writeJSON(w, http.StatusOK, bp)
}

type updateBlueprintRequest struct {
	Name string `json:"name"`
}

// handleBlueprintPut renames a blueprint (header/meta save). Steps are saved
// separately via handleBlueprintStepsPut.
func (s *Server) handleBlueprintPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var req updateBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	var updated *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.Blueprints.Get(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if e := tx.Blueprints.Update(r.Context(), orgID, id, strings.TrimSpace(req.Name)); e != nil {
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

// handleBlueprintDelete soft-deletes a blueprint (stamps deleted_at) and clears
// the triggers bound to it. Deletion is soft because blueprint_runs RESTRICT a
// hard delete and run history (incl. opened PRs / posted reviews) is a durable
// audit trail — the row, its steps, and its runs are kept; request-facing reads
// filter it out. Triggers are cleared explicitly here: a hard delete cascaded
// them, but a soft delete leaves the row, so a lingering trigger would still
// fire the deleted blueprint. User triggers are removed; system triggers (which
// can't be hard-deleted) are disabled. In-flight runs keep resolving the
// blueprint via the orchestrator's System reads and finish normally.
func (s *Server) handleBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var found bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.Blueprints.Get(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		found = true
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		for _, t := range triggers {
			if t.Source == domain.EventHandlerSourceUser {
				if e := tx.EventHandlers.Delete(r.Context(), orgID, t.ID); e != nil {
					return e
				}
			} else if e := tx.EventHandlers.SetEnabled(r.Context(), orgID, t.ID, false); e != nil {
				return e
			}
		}
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

// blueprintStepValidation is a client-facing step-validation failure carried
// out of a WithTx callback as an error so the surrounding tx rolls back. This
// matters for atomic create: a committed header with no/invalid steps would
// orphan the blueprint, so the callback must return (not swallow) the failure.
// The handler unwraps it with errors.As to write the status + message.
type blueprintStepValidation struct {
	status int
	msg    string
}

func (e *blueprintStepValidation) Error() string { return e.msg }

// validateAndReplaceBlueprintSteps validates each step's prompt (exists +
// same-team as teamID) and replaces the blueprint's step list, all inside tx.
// A client-side failure returns *blueprintStepValidation (which, returned as an
// error, rolls the surrounding tx back); the caller maps it to an HTTP status.
func validateAndReplaceBlueprintSteps(ctx context.Context, tx db.TxStores, orgID, blueprintID, teamID string, steps []blueprintStepInput) error {
	if len(steps) > maxBlueprintSteps {
		return &blueprintStepValidation{http.StatusUnprocessableEntity, "blueprint may not exceed " + strconv.Itoa(maxBlueprintSteps) + " steps"}
	}
	stepIDs := make([]string, 0, len(steps))
	briefs := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.StepPromptID == "" {
			return &blueprintStepValidation{http.StatusBadRequest, "step_prompt_id is required for every step"}
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}
	for i, sid := range stepIDs {
		stepPrompt, e := tx.Prompts.Get(ctx, orgID, sid)
		if e != nil {
			return e
		}
		if stepPrompt == nil {
			return &blueprintStepValidation{http.StatusUnprocessableEntity, "step " + strconv.Itoa(i) + " references a non-existent prompt"}
		}
		// Same-team guard: a blueprint may only step through prompts its own team
		// owns. The (step_prompt_id, team_id) composite FK enforces it on the
		// write; pre-check here for a clean 422.
		if teamID != "" && stepPrompt.TeamID != "" && stepPrompt.TeamID != teamID {
			return &blueprintStepValidation{http.StatusUnprocessableEntity, "step " + strconv.Itoa(i) + " references a prompt owned by another team"}
		}
	}
	return tx.Blueprints.ReplaceSteps(ctx, orgID, blueprintID, stepIDs, briefs)
}

// handleBlueprintStepsPut replaces an existing blueprint's step list (the edit
// path; create writes steps atomically in handleBlueprintCreate).
func (s *Server) handleBlueprintStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req blueprintStepsPutRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var missing bool
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		blueprint, e := tx.Blueprints.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if blueprint == nil {
			missing = true
			return nil
		}
		return validateAndReplaceBlueprintSteps(r.Context(), tx, orgID, id, blueprint.TeamID, req.Steps)
	})
	var ve *blueprintStepValidation
	if errors.As(err, &ve) {
		writeJSON(w, ve.status, map[string]string{"error": ve.msg})
		return
	}
	if err != nil {
		internalError(w, "blueprints", err)
		return
	}
	if missing {
		notFound(w, "blueprint")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
