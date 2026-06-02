package server

import (
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
	var conflictErr string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.Prompts.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if prompt == nil {
			return nil
		}

		// Block deletion if this prompt is a step inside any blueprint. The FK
		// (blueprint_steps.step_prompt_id) is ON DELETE RESTRICT so the
		// underlying constraint would fire anyway; we surface a friendlier
		// message and the count of blueprints. This also covers the trigger
		// case — a trigger fires a blueprint, and a 1-step blueprint a trigger
		// fires holds this prompt as its sole step, so the reference count is
		// non-zero and deletion is blocked here.
		stepRefs, e := tx.Blueprints.CountStepReferences(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if stepRefs > 0 {
			conflictErr = "This prompt is used as a step in one or more blueprints. Remove it from those blueprints first."
			return nil
		}

		// System and imported prompts are soft-deleted (hidden), user prompts are hard-deleted
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
	if conflictErr != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": conflictErr})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
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
