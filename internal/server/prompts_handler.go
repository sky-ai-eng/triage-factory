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
	var prompts []domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompts, e = tx.Prompts.List(r.Context(), orgID)
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
	Kind  string `json:"kind"`
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
	kind := normalizePromptKind(req.Kind)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	// Leaf prompts must carry a body (the mission). Chain prompts may
	// store a description in body or leave it empty — the steps are
	// the real definition and live in prompt_chain_steps.
	if kind == domain.PromptKindLeaf && req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required for leaf prompts"})
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
		Kind:   kind,
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
	Kind  string `json:"kind"`
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
	kind := normalizePromptKind(req.Kind)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if kind == domain.PromptKindLeaf && req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required for leaf prompts"})
		return
	}
	if !validPromptModel(req.Model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": invalidPromptModelError()})
		return
	}

	var existing *domain.Prompt
	var conflictErr string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Prompts.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if existing == nil {
			return nil
		}
		if existing.Kind == kind {
			return nil
		}
		if existing.Kind == domain.PromptKindChain {
			// Reject chain→leaf if any chain steps exist.
			steps, e := tx.Chains.ListSteps(r.Context(), orgID, id)
			if e != nil {
				return e
			}
			if len(steps) > 0 {
				conflictErr = fmt.Sprintf("cannot change kind: prompt has %d chain step(s)", len(steps))
				return nil
			}
			return nil
		}
		// Reject leaf→chain if triggers, runs, or *other* chains
		// reference this prompt. The chain-step check matters because
		// existing chains that embed this prompt as a step would
		// suddenly point at a chain-kind prompt; the chain-step API
		// explicitly rejects nested chains, so the chain would fail
		// at delegate time instead of definition time.
		triggers, e := tx.EventHandlers.ListForPrompt(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		runCount, e := tx.Prompts.CountRunReferences(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		stepRefs, e := tx.Chains.CountStepReferences(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if len(triggers) > 0 || runCount > 0 || stepRefs > 0 {
			conflictErr = fmt.Sprintf("cannot change kind: prompt is referenced by %d trigger(s), %d run(s), and %d chain step(s)", len(triggers), runCount, stepRefs)
		}
		return nil
	}); err != nil {
		internalError(w, "prompts", err)
		return
	}
	if existing == nil {
		notFound(w, "prompt")
		return
	}
	if conflictErr != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": conflictErr})
		return
	}

	var updated *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.Prompts.Update(r.Context(), orgID, id, req.Name, req.Body, string(kind), req.Model); e != nil {
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

// normalizePromptKind defaults to leaf for blank or unknown values so
// legacy clients that don't send `kind` keep working.
func normalizePromptKind(k string) domain.PromptKind {
	switch domain.PromptKind(k) {
	case domain.PromptKindChain:
		return domain.PromptKindChain
	default:
		return domain.PromptKindLeaf
	}
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

		// Block deletion if the prompt is referenced by any auto-triggers.
		// Post-SKY-259 triggers live in event_handlers with kind='trigger';
		// ListForPrompt returns only those.
		triggers, e := tx.EventHandlers.ListForPrompt(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			conflictErr = "This prompt is used by an auto-delegation trigger. Remove the trigger first."
			return nil
		}

		// Block deletion if this prompt is a step inside any chain. The FK
		// is ON DELETE RESTRICT so the underlying constraint would fire
		// anyway; we surface a friendlier message and the count of chains.
		chainRefs, e := tx.Chains.CountStepReferences(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if chainRefs > 0 {
			conflictErr = "This prompt is used as a step in one or more chains. Remove it from those chains first."
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
