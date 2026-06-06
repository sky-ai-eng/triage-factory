package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func (s *Server) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	types, err := db.ListEventTypes(s.db)
	if err != nil {
		internalError(w, "prompts", err)
		return
	}
	if types == nil {
		types = []domain.EventType{}
	}
	writeJSON(w, http.StatusOK, types)
}

func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	// ?team_id= narrows to one team's prompts (+ org-visible) on the
	// multi-team prompts page; absent/solo returns everything visible.
	teamID := singleTeamParam(r)
	var prompts []domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompts, e = tx.Prompts.List(r.Context(), orgID, teamID)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if prompts == nil {
		prompts = []domain.Prompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
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
		internalError(w, "prompts", err)
		return
	}
	if prompt == nil {
		notFound(w, "prompt")
		return
	}

	writeJSON(w, http.StatusOK, prompt)
}

type createPromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
	// TeamID is the acting team the write picker supplied. Required in
	// the UI whenever the caller belongs to ≥2 teams; empty for a solo
	// caller (the picker is hidden), where the resolver falls back to the
	// sole team. See resolveActingTeam.
	TeamID string `json:"team_id"`
}

// allowedPromptModelOverrides is the set of non-empty values accepted
// for prompts.model. "" is always allowed and means "inherit the
// global default from settings.AI.Model at dispatch". Kept aligned
// with the picker in frontend/src/pages/Settings.tsx.
var allowedPromptModelOverrides = []string{"haiku", "sonnet", "opus"}

func validPromptModel(m string) bool {
	if m == "" {
		return true
	}
	for _, v := range allowedPromptModelOverrides {
		if m == v {
			return true
		}
	}
	return false
}

func invalidPromptModelError() string {
	return `model must be "" or one of: ` + strings.Join(allowedPromptModelOverrides, ", ")
}

func (s *Server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req createPromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	// A prompt is the step content unit — it always carries a body (the
	// mission). Ordering prompts into a multi-step composition is the
	// blueprint's job, not a prompt-kind discriminator.
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
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
	var created *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		if e := tx.Prompts.Create(r.Context(), orgID, teamID, prompt); e != nil {
			return e
		}
		var ge error
		created, ge = tx.Prompts.Get(r.Context(), orgID, id)
		return ge
	}); err != nil {
		if writeIfActingTeamError(w, err) {
			return
		}
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

type updatePromptRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req updatePromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
		return
	}

	var existing *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	var updated *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.Prompts.Update(r.Context(), orgID, id, req.Name, req.Body, req.Model); e != nil {
			return e
		}
		var ge error
		updated, ge = tx.Prompts.Get(r.Context(), orgID, id)
		return ge
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var prompt *domain.Prompt
	var status string
	// orphaned is set when fragmenting a multi-step blueprint leaves the steps
	// after the deleted prompt as a new, trigger-less blueprint (head / mid
	// delete) — the canvas surfaces a toast so the now-untriggered downstream
	// doesn't go unnoticed.
	var orphaned bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
				// target). Hard-delete unconditionally — system rows included —
				// matching handleEventHandlerDelete: nothing re-seeds at boot anymore
				// (provisioning's materializer runs once per fresh tenant), so a
				// deleted shipped default is durable.
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
		// Delete (also soft — runs.prompt_id is RESTRICT). Both leave the row +
		// its runs so historical timelines still resolve the prompt.
		if prompt.Source == "system" || prompt.Source == "imported" {
			if e := tx.Prompts.Hide(r.Context(), orgID, id); e != nil {
				return e
			}
			status = "hidden"
			return nil
		}

		if e := tx.Prompts.Delete(r.Context(), orgID, id); e != nil {
			return e
		}
		status = "deleted"
		return nil
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

func (s *Server) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var stats *domain.PromptStats
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		stats, e = tx.Prompts.Stats(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
