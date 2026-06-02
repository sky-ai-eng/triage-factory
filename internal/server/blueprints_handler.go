package server

import (
	"net/http"
	"strconv"

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
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	writeJSON(w, http.StatusCreated, created)
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
