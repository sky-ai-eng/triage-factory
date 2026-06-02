package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// errTemplateBlueprintMissing signals (inside a WithTx closure) that a trigger's
// referenced template blueprint doesn't exist, so the handler can 404 instead of
// surfacing the downstream FK error.
var errTemplateBlueprintMissing = errors.New("template blueprint not found")

// /api/org-template/* — the org-admin editor over the org template that
// new teams are seeded from (SKY-381). Full parity with the team-scoped
// /api/prompts + /api/event-handlers families, minus the team picker: the
// template is org-scoped, so there is no acting team to resolve. Every
// endpoint is multi-mode + org-admin gated (requireOrgTemplate); local mode
// (N=1, no template) returns 404 so the route is absent there.
//
// Wire shape:
//
//	GET    /api/org-template/prompts                     — list
//	POST   /api/org-template/prompts                     — create
//	GET    /api/org-template/prompts/{id}                — get
//	PUT    /api/org-template/prompts/{id}                — update
//	DELETE /api/org-template/prompts/{id}                — delete (RESTRICT if a blueprint step)
//	GET    /api/org-template/blueprints                  — list
//	POST   /api/org-template/blueprints                  — create
//	GET    /api/org-template/blueprints/{id}             — get
//	PUT    /api/org-template/blueprints/{id}             — rename
//	DELETE /api/org-template/blueprints/{id}             — delete (cascades steps + triggers)
//	GET    /api/org-template/blueprints/{id}/steps       — list steps
//	PUT    /api/org-template/blueprints/{id}/steps       — replace steps
//	GET    /api/org-template/event-handlers[?kind=]      — list
//	POST   /api/org-template/event-handlers              — create
//	PATCH  /api/org-template/event-handlers/{id}         — partial update
//	PUT    /api/org-template/event-handlers/{id}         — update (alias)
//	DELETE /api/org-template/event-handlers/{id}         — delete
//	POST   /api/org-template/event-handlers/{id}/toggle  — flip enabled
//	POST   /api/org-template/event-handlers/{id}/promote — rule → trigger
//	PUT    /api/org-template/event-handlers/reorder      — rule sort_order

// requireOrgTemplate gates an org-template endpoint: multi-mode only, active
// org resolved, caller is an org admin. Returns (orgID, userID, true) on
// success. Local mode 404s (no template concept) — mirrors the POST /api/teams
// local-absent posture. The org-admin check is also enforced server-side by
// the org_template_*_all RLS policies; this is the friendly front gate.
func (s *Server) requireOrgTemplate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = s.requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject
	if !s.requireOrgAdminRole(w, r, orgID, userID) {
		return "", "", false
	}
	return orgID, userID, true
}

// newTemplateSlug mints a stable system_slug for an admin-authored template
// row. Every template row carries a non-empty slug so the per-team copy
// dedupes on (org_id, team_id, system_slug) uniformly — shipped rows reuse
// their real slug, admin-authored rows get this generated one.
func newTemplateSlug() string {
	return "tmpl-" + uuid.New().String()
}

// --- prompts --------------------------------------------------------

func (s *Server) handleOrgTemplatePromptsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var prompts []domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompts, e = tx.OrgTemplate.ListPrompts(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if prompts == nil {
		prompts = []domain.Prompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) handleOrgTemplatePromptGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var prompt *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		prompt, e = tx.OrgTemplate.GetPrompt(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if prompt == nil {
		notFound(w, "template prompt")
		return
	}
	writeJSON(w, http.StatusOK, prompt)
}

func (s *Server) handleOrgTemplatePromptCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var req createPromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if req.Body == "" {
		badRequest(w, "body is required")
		return
	}
	if !validPromptModel(req.Model) {
		badRequest(w, invalidPromptModelError())
		return
	}
	prompt := domain.Prompt{
		ID:         uuid.New().String(),
		SystemSlug: newTemplateSlug(),
		Name:       req.Name,
		Body:       req.Body,
		Source:     "user",
		Model:      req.Model,
	}
	var created *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.OrgTemplate.CreatePrompt(r.Context(), orgID, prompt); e != nil {
			return e
		}
		var ge error
		created, ge = tx.OrgTemplate.GetPrompt(r.Context(), orgID, prompt.ID)
		return ge
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleOrgTemplatePromptPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req updatePromptRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if req.Body == "" {
		badRequest(w, "body is required")
		return
	}
	if !validPromptModel(req.Model) {
		badRequest(w, invalidPromptModelError())
		return
	}
	var updated *domain.Prompt
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.OrgTemplate.GetPrompt(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if e := tx.OrgTemplate.UpdatePrompt(r.Context(), orgID, id, req.Name, req.Body, req.Model); e != nil {
			return e
		}
		updated, e = tx.OrgTemplate.GetPrompt(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if updated == nil {
		notFound(w, "template prompt")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleOrgTemplatePromptDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var found bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.OrgTemplate.GetPrompt(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		found = true
		// Hard delete — the template isn't re-seeded on boot, so removing a
		// shipped default sticks. Triggers referencing it cascade (FK).
		return tx.OrgTemplate.DeletePrompt(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if !found {
		notFound(w, "template prompt")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- blueprints -----------------------------------------------------

// orgTemplateBlueprintRequest is the create/rename body for a template
// blueprint header. Steps are managed separately via the /steps endpoints.
type orgTemplateBlueprintRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleOrgTemplateBlueprintsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var blueprints []domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		blueprints, e = tx.OrgTemplate.ListBlueprints(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if blueprints == nil {
		blueprints = []domain.Blueprint{}
	}
	writeJSON(w, http.StatusOK, blueprints)
}

func (s *Server) handleOrgTemplateBlueprintCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var req orgTemplateBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		badRequest(w, "name is required")
		return
	}
	b := domain.Blueprint{
		ID:         uuid.New().String(),
		SystemSlug: newTemplateSlug(),
		Name:       strings.TrimSpace(req.Name),
		Source:     "user",
	}
	var created *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.OrgTemplate.CreateBlueprint(r.Context(), orgID, b); e != nil {
			return e
		}
		var ge error
		created, ge = tx.OrgTemplate.GetBlueprint(r.Context(), orgID, b.ID)
		return ge
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleOrgTemplateBlueprintGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var bp *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		bp, e = tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if bp == nil {
		notFound(w, "template blueprint")
		return
	}
	writeJSON(w, http.StatusOK, bp)
}

func (s *Server) handleOrgTemplateBlueprintPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req orgTemplateBlueprintRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		badRequest(w, "name is required")
		return
	}
	var updated *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if e := tx.OrgTemplate.UpdateBlueprint(r.Context(), orgID, id, strings.TrimSpace(req.Name)); e != nil {
			return e
		}
		updated, e = tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if updated == nil {
		notFound(w, "template blueprint")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleOrgTemplateBlueprintDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var found bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		found = true
		// Hard delete — its steps and any template triggers referencing it
		// cascade (FK). The team copies already made are untouched (forward-only).
		return tx.OrgTemplate.DeleteBlueprint(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if !found {
		notFound(w, "template blueprint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleOrgTemplateBlueprintStepsGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var bp *domain.Blueprint
	var steps []domain.BlueprintStep
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		bp, e = tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		if e != nil || bp == nil {
			return e
		}
		steps, e = tx.OrgTemplate.ListBlueprintSteps(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if bp == nil {
		notFound(w, "template blueprint")
		return
	}
	if steps == nil {
		steps = []domain.BlueprintStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

func (s *Server) handleOrgTemplateBlueprintStepsPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
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
			badRequest(w, "step_prompt_id is required for every step")
			return
		}
		stepIDs = append(stepIDs, step.StepPromptID)
		briefs = append(briefs, step.Brief)
	}

	var bpMissing bool
	var validationErr string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bp, e := tx.OrgTemplate.GetBlueprint(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if bp == nil {
			bpMissing = true
			return nil
		}
		// Each step must reference a template prompt in the same org; the
		// composite FK enforces it, but pre-check for a clean 422.
		for i, sid := range stepIDs {
			p, e := tx.OrgTemplate.GetPrompt(r.Context(), orgID, sid)
			if e != nil {
				return e
			}
			if p == nil {
				validationErr = "step " + strconv.Itoa(i) + " references a non-existent template prompt"
				return nil
			}
		}
		return tx.OrgTemplate.ReplaceBlueprintSteps(r.Context(), orgID, id, stepIDs, briefs)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if bpMissing {
		notFound(w, "template blueprint")
		return
	}
	if validationErr != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": validationErr})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- event handlers -------------------------------------------------

func (s *Server) handleOrgTemplateHandlersList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != domain.EventHandlerKindRule && kind != domain.EventHandlerKindTrigger {
		badRequest(w, "kind must be 'rule', 'trigger', or omitted (returns both)")
		return
	}
	var handlers []domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handlers, e = tx.OrgTemplate.ListHandlers(r.Context(), orgID, kind)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if handlers == nil {
		handlers = []domain.EventHandler{}
	}
	writeJSON(w, http.StatusOK, handlers)
}

func (s *Server) handleOrgTemplateHandlerCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var req createEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Kind != domain.EventHandlerKindRule && req.Kind != domain.EventHandlerKindTrigger {
		badRequest(w, "kind must be 'rule' or 'trigger'")
		return
	}
	if req.EventType == "" {
		badRequest(w, "event_type is required")
		return
	}
	if _, ok := events.Get(req.EventType); !ok {
		badRequest(w, "unknown event_type: "+req.EventType)
		return
	}
	canonical, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	h := domain.EventHandler{
		ID:         uuid.New().String(),
		SystemSlug: newTemplateSlug(),
		Kind:       req.Kind,
		EventType:  req.EventType,
		Source:     domain.EventHandlerSourceUser,
	}
	if canonical != "" {
		h.ScopePredicateJSON = &canonical
	}

	switch req.Kind {
	case domain.EventHandlerKindRule:
		if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
			badRequest(w, "name is required for kind=rule")
			return
		}
		h.Name = strings.TrimSpace(*req.Name)
		priority := 0.5
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				badRequest(w, "default_priority must be between 0 and 1")
				return
			}
			priority = *req.DefaultPriority
		}
		h.DefaultPriority = &priority
		sortOrder := 0
		if req.SortOrder != nil {
			sortOrder = *req.SortOrder
		}
		h.SortOrder = &sortOrder
		h.Enabled = true
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}
	case domain.EventHandlerKindTrigger:
		if req.BlueprintID == "" {
			badRequest(w, "blueprint_id is required for kind=trigger")
			return
		}
		h.BlueprintID = req.BlueprintID
		h.TriggerType = domain.TriggerTypeEvent
		threshold := 4
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				badRequest(w, "breaker_threshold must be positive")
				return
			}
			threshold = *req.BreakerThreshold
		}
		h.BreakerThreshold = &threshold
		minAutonomy := 0.0
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				badRequest(w, "min_autonomy_suitability must be between 0 and 1")
				return
			}
			minAutonomy = *req.MinAutonomySuitability
		}
		h.MinAutonomySuitability = &minAutonomy
		h.Enabled = false
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}
	}

	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// A template trigger may only fire a template blueprint in the same org.
		// Pre-check for a clean 404 instead of the downstream FK error.
		if h.Kind == domain.EventHandlerKindTrigger {
			b, e := tx.OrgTemplate.GetBlueprint(r.Context(), orgID, h.BlueprintID)
			if e != nil {
				return e
			}
			if b == nil {
				return errTemplateBlueprintMissing
			}
		}
		if e := tx.OrgTemplate.CreateHandler(r.Context(), orgID, h); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.OrgTemplate.GetHandler(r.Context(), orgID, h.ID)
		return ge
	}); err != nil {
		if err == errTemplateBlueprintMissing {
			notFound(w, "template blueprint")
			return
		}
		internalError(w, "org_template", err)
		return
	}
	writeJSON(w, http.StatusCreated, fresh)
}

func (s *Server) handleOrgTemplateHandlerUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req patchEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var existing *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if existing == nil {
		notFound(w, "template handler")
		return
	}

	updated := *existing
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	// Predicate: absent → unchanged; explicit null → clear; value → validate.
	if len(req.ScopePredicateJSON) > 0 {
		raw := string(req.ScopePredicateJSON)
		if raw == "null" {
			updated.ScopePredicateJSON = nil
		} else {
			var asString string
			if err := json.Unmarshal(req.ScopePredicateJSON, &asString); err == nil {
				raw = asString
			}
			canonical, err := events.ValidatePredicateJSON(existing.EventType, raw)
			if err != nil {
				badRequest(w, err.Error())
				return
			}
			if canonical == "" {
				updated.ScopePredicateJSON = nil
			} else {
				updated.ScopePredicateJSON = &canonical
			}
		}
	}

	switch existing.Kind {
	case domain.EventHandlerKindRule:
		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if trimmed == "" {
				badRequest(w, "name cannot be empty")
				return
			}
			updated.Name = trimmed
		}
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				badRequest(w, "default_priority must be between 0 and 1")
				return
			}
			v := *req.DefaultPriority
			updated.DefaultPriority = &v
		}
		if req.SortOrder != nil {
			v := *req.SortOrder
			updated.SortOrder = &v
		}
	case domain.EventHandlerKindTrigger:
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				badRequest(w, "breaker_threshold must be positive")
				return
			}
			v := *req.BreakerThreshold
			updated.BreakerThreshold = &v
		}
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				badRequest(w, "min_autonomy_suitability must be between 0 and 1")
				return
			}
			v := *req.MinAutonomySuitability
			updated.MinAutonomySuitability = &v
		}
	}

	var matched bool
	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		matched, e = tx.OrgTemplate.UpdateHandler(r.Context(), orgID, updated)
		if e != nil {
			return e
		}
		fresh, e = tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	// matched=false means our kind-pinned UPDATE hit 0 rows — the handler was
	// deleted (fresh nil → 404) or promoted out from under us (fresh non-nil
	// with a changed kind → 409) between the read and the write. Either way the
	// PATCH didn't apply, so don't report a misleading 200.
	if !matched {
		if fresh == nil {
			notFound(w, "template handler")
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "handler changed concurrently (kind no longer matches); reload and retry",
		})
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleOrgTemplateHandlerDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var found bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		found = true
		// Hard delete regardless of source — the template isn't re-seeded on
		// boot, so a deleted shipped default stays gone (the "remove a shipped
		// trigger" lever). The team copies already made are untouched
		// (forward-only).
		return tx.OrgTemplate.DeleteHandler(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if !found {
		notFound(w, "template handler")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleOrgTemplateHandlerToggle(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	var existing *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		return tx.OrgTemplate.SetHandlerEnabled(r.Context(), orgID, id, req.Enabled)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if existing == nil {
		notFound(w, "template handler")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": req.Enabled})
}

func (s *Server) handleOrgTemplateHandlerPromote(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req promoteEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.BlueprintID == "" {
		badRequest(w, "blueprint_id is required")
		return
	}
	if req.BreakerThreshold == nil || req.MinAutonomySuitability == nil {
		badRequest(w, "breaker_threshold and min_autonomy_suitability are required")
		return
	}

	var existing *domain.EventHandler
	var blueprint *domain.Blueprint
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if existing.Kind != domain.EventHandlerKindRule {
			return nil
		}
		blueprint, e = tx.OrgTemplate.GetBlueprint(r.Context(), orgID, req.BlueprintID)
		return e
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	if existing == nil {
		notFound(w, "template handler")
		return
	}
	if existing.Kind != domain.EventHandlerKindRule {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only rules can be promoted"})
		return
	}
	if blueprint == nil {
		notFound(w, "template blueprint")
		return
	}

	predicate := existing.ScopePredicateJSON
	if req.ScopePredicateJSON != nil {
		canonical, verr := events.ValidatePredicateJSON(existing.EventType, *req.ScopePredicateJSON)
		if verr != nil {
			badRequest(w, verr.Error())
			return
		}
		if canonical == "" {
			predicate = nil
		} else {
			predicate = &canonical
		}
	}

	target := domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            req.BlueprintID,
		BreakerThreshold:       req.BreakerThreshold,
		MinAutonomySuitability: req.MinAutonomySuitability,
		ScopePredicateJSON:     predicate,
	}
	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.OrgTemplate.PromoteHandler(r.Context(), orgID, id, target); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.OrgTemplate.GetHandler(r.Context(), orgID, id)
		return ge
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleOrgTemplateHandlerReorder(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.requireOrgTemplate(w, r)
	if !ok {
		return
	}
	var ids []string
	if !decodeJSON(w, r, &ids, "expected array of handler IDs") {
		return
	}
	if len(ids) == 0 {
		badRequest(w, "empty ID list")
		return
	}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.OrgTemplate.ReorderHandlers(r.Context(), orgID, ids)
	}); err != nil {
		internalError(w, "org_template", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
