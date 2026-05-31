package server

import (
	"net/http"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const maxChainSteps = 50

// handleChainStepsGet returns the ordered step list for a chain prompt.
// Always returns an array (never null) so frontend code can iterate
// without a nil check.
func (s *Server) handleChainStepsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var prompt *domain.Prompt
	var steps []domain.ChainStep
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if prompt == nil {
			return nil
		}
		steps, e = tx.Chains.ListSteps(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "chains", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}
	if steps == nil {
		steps = []domain.ChainStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

type chainStepInput struct {
	StepPromptID string `json:"step_prompt_id"`
	Brief        string `json:"brief"`
}

type chainStepsPutRequest struct {
	Steps []chainStepInput `json:"steps"`
}

// handleChainStepsPut replaces the chain prompt's step list. Validates
// that the chain prompt exists and is kind='chain', and that no step
// references another chain prompt (recursion guard at the API layer).
func (s *Server) handleChainStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var prompt *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "chains", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}
	if prompt.Kind != domain.PromptKindChain {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this prompt is not a chain (kind != 'chain')",
		})
		return
	}

	var req chainStepsPutRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if len(req.Steps) > maxChainSteps {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "chain may not exceed " + strconv.Itoa(maxChainSteps) + " steps",
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
		if step.StepPromptID == id {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "a chain cannot reference itself as a step",
			})
			return
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}

	// Validate each step's prompt exists + is a leaf, then replace
	// in one tx so all lookups and the final write share claims.
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
			// Recursion guard: a chain step must point at a leaf prompt.
			// Nested chains aren't supported in v1 and would also create
			// cycles if a chain referenced itself transitively.
			if stepPrompt.Kind == domain.PromptKindChain {
				validationErr = "step " + strconv.Itoa(i) + " references another chain prompt; nested chains aren't supported"
				validationStatus = http.StatusUnprocessableEntity
				return nil
			}
			// Same-team guard (SKY-380): a chain may only step through prompts
			// its own team owns. The DB enforces this via the (step_prompt_id,
			// team_id) composite FK on ReplaceSteps; pre-check for a clean 422.
			if prompt.TeamID != "" && stepPrompt.TeamID != "" && stepPrompt.TeamID != prompt.TeamID {
				validationErr = "step " + strconv.Itoa(i) + " references a prompt owned by another team"
				validationStatus = http.StatusUnprocessableEntity
				return nil
			}
		}
		if validationErr != "" {
			return nil
		}
		return tx.Chains.ReplaceSteps(r.Context(), orgID, id, stepIDs, briefs)
	}); err != nil {
		internalError(w, "chains", err)
		return
	}
	if validationErr != "" {
		writeJSON(w, validationStatus, map[string]string{"error": validationErr})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// chainRunResponse bundles the chain run row with its per-step runs
// and verdicts so the run-detail UI can render the timeline in one
// fetch instead of N+1.
type chainRunResponse struct {
	ChainRun *domain.ChainRun   `json:"chain_run"`
	Steps    []chainRunStepView `json:"steps"`
}

type chainRunStepView struct {
	Step    domain.ChainStep     `json:"step"`
	Run     *domain.AgentRun     `json:"run,omitempty"`
	Verdict *domain.ChainVerdict `json:"verdict,omitempty"`
}

func (s *Server) handleChainRunGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var cr *domain.ChainRun
	var steps []domain.ChainStep
	var stepRuns []domain.AgentRun
	var verdictsByRun map[string]*domain.ChainVerdict
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		cr, e = tx.Chains.GetRun(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if cr == nil {
			return nil
		}
		steps, e = tx.Chains.ListSteps(r.Context(), orgID, cr.ChainPromptID)
		if e != nil {
			return e
		}
		stepRuns, e = tx.Chains.RunsForChain(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		runIDs := make([]string, 0, len(stepRuns))
		for i := range stepRuns {
			if stepRuns[i].ChainStepIndex != nil {
				runIDs = append(runIDs, stepRuns[i].ID)
			}
		}
		verdictsByRun, e = tx.Chains.LatestVerdictsForRuns(r.Context(), orgID, runIDs)
		return e
	}); err != nil {
		internalError(w, "chains", err)
		return
	}
	if cr == nil {
		notFound(w, "chain run")
		return
	}
	runByStep := map[int]*domain.AgentRun{}
	for i := range stepRuns {
		if stepRuns[i].ChainStepIndex != nil {
			runByStep[*stepRuns[i].ChainStepIndex] = &stepRuns[i]
		}
	}

	views := make([]chainRunStepView, 0, len(steps))
	for _, step := range steps {
		view := chainRunStepView{Step: step}
		if run, ok := runByStep[step.StepIndex]; ok {
			view.Run = run
			view.Verdict = verdictsByRun[run.ID]
		}
		views = append(views, view)
	}

	writeJSON(w, http.StatusOK, chainRunResponse{ChainRun: cr, Steps: views})
}

func (s *Server) handleChainRunCancel(w http.ResponseWriter, r *http.Request) {
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

	var cr *domain.ChainRun
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		cr, e = tx.Chains.GetRun(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "chains", err)
		return
	}
	if cr == nil {
		notFound(w, "chain run")
		return
	}

	switch cr.Status {
	case domain.ChainRunStatusCompleted, domain.ChainRunStatusFailed,
		domain.ChainRunStatusAborted, domain.ChainRunStatusCancelled:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "chain run already terminal"})
		return
	}

	if err := s.spawner.CancelChain(orgID, id, userID); err != nil {
		internalError(w, "chains", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}
