package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// GET /api/task-rules
//
// Returns all task rules (system + user) ordered by sort_order then name.
func (s *Server) handleTaskRulesList(w http.ResponseWriter, r *http.Request) {
	rules, err := db.ListTaskRules(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if rules == nil {
		rules = []domain.TaskRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// POST /api/task-rules
//
// Creates a user-authored task rule. Predicate is validated + canonicalised
// via events.ValidatePredicateJSON.
type createTaskRuleRequest struct {
	EventType          string   `json:"event_type"`
	ScopePredicateJSON string   `json:"scope_predicate_json"`
	Enabled            *bool    `json:"enabled"` // pointer so absent → default true
	Name               string   `json:"name"`
	DefaultPriority    *float64 `json:"default_priority"` // pointer so absent → default 0.5
	SortOrder          *int     `json:"sort_order"`       // pointer so absent → default 0
}

func (s *Server) handleTaskRuleCreate(w http.ResponseWriter, r *http.Request) {
	var req createTaskRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_type is required"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	// event_type must be registered.
	if _, ok := events.Get(req.EventType); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown event_type: " + req.EventType})
		return
	}

	// Validate + canonicalise the predicate. "" / "{}" / "null" → "" (match-all).
	canonical, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	rule := domain.TaskRule{
		ID:              uuid.New().String(),
		EventType:       req.EventType,
		Enabled:         true,
		Name:            req.Name,
		DefaultPriority: 0.5,
		SortOrder:       0,
		Source:          "user",
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.DefaultPriority != nil {
		if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
			return
		}
		rule.DefaultPriority = *req.DefaultPriority
	}
	if req.SortOrder != nil {
		rule.SortOrder = *req.SortOrder
	}
	if canonical != "" {
		rule.ScopePredicateJSON = &canonical
	}

	if err := db.CreateTaskRule(s.db, rule); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Re-read so the response reflects server-set timestamps.
	fresh, err := db.GetTaskRule(s.db, rule.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "created but failed to re-read: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, fresh)
}

// PATCH /api/task-rules/{id}
//
// Partial update. Any field left nil/absent is unchanged. event_type is
// immutable (changing it would invalidate the predicate schema); users who
// want to "change" the event type should delete + recreate.
type patchTaskRuleRequest struct {
	// ScopePredicateJSON uses json.RawMessage to distinguish three cases
	// that a *string can't: absent (len==0 → leave unchanged), explicit
	// JSON null (bytes "null" → clear to match-all), and any other value.
	// A plain *string would collapse absent and null to the same nil,
	// silently no-op'ing clients who send `{"scope_predicate_json": null}`
	// to clear the predicate.
	ScopePredicateJSON json.RawMessage `json:"scope_predicate_json"`
	Enabled            *bool           `json:"enabled"`
	Name               *string         `json:"name"`
	DefaultPriority    *float64        `json:"default_priority"`
	SortOrder          *int            `json:"sort_order"`
}

func (s *Server) handleTaskRuleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchTaskRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	existing, err := db.GetTaskRule(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task rule not found"})
		return
	}

	updated := *existing

	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
		updated.Name = trimmed
	}
	if req.DefaultPriority != nil {
		if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
			return
		}
		updated.DefaultPriority = *req.DefaultPriority
	}
	if req.SortOrder != nil {
		updated.SortOrder = *req.SortOrder
	}
	// Predicate update — three distinguishable cases:
	//   - absent (len==0):            leave unchanged
	//   - explicit null ("null"):     clear to match-all
	//   - any JSON string (quoted):   unquote, then validate + canonicalise
	//   - bare JSON object / other:   treat as predicate body directly
	if len(req.ScopePredicateJSON) > 0 {
		raw := string(req.ScopePredicateJSON)
		if raw == "null" {
			updated.ScopePredicateJSON = nil
		} else {
			// The body may be either a JSON string wrapping the predicate
			// (e.g. `"{\"author_is_self\":true}"`) — which is how responses
			// round-trip back in — or a bare JSON object. Handle both by
			// unquoting strings, otherwise passing through.
			var asString string
			if err := json.Unmarshal(req.ScopePredicateJSON, &asString); err == nil {
				raw = asString
			}
			canonical, err := events.ValidatePredicateJSON(existing.EventType, raw)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if canonical == "" {
				updated.ScopePredicateJSON = nil
			} else {
				updated.ScopePredicateJSON = &canonical
			}
		}
	}

	if err := db.UpdateTaskRule(s.db, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	fresh, err := db.GetTaskRule(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "updated but failed to re-read: " + err.Error()})
		return
	}
	if fresh != nil {
		writeJSON(w, http.StatusOK, fresh)
	} else {
		writeJSON(w, http.StatusOK, updated)
	}
}

// DELETE /api/task-rules/{id}
//
// Hard-deletes user rules; disables system rules in place. System rules are
// soft-disabled because SeedTaskRules runs on every boot with INSERT OR
// IGNORE — a hard-delete would be resurrected as enabled on the next boot.
// Returns 200 with {status: "deleted"|"disabled"} so the frontend can show
// the right confirmation.
func (s *Server) handleTaskRuleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	existing, err := db.GetTaskRule(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task rule not found"})
		return
	}

	if existing.Source == "system" {
		// Soft-disable instead of delete.
		if err := db.SetTaskRuleEnabled(s.db, id, false); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "disabled",
			"reason": "system rules cannot be deleted (they would be resurrected on next boot); disabled instead",
		})
		return
	}

	if err := db.DeleteTaskRule(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// PUT /api/task-rules/reorder
//
// Accepts an ordered array of rule IDs. Each rule's sort_order is set to
// its index in the array. IDs not in the list keep their current order.
func (s *Server) handleTaskRuleReorder(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected array of rule IDs"})
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty ID list"})
		return
	}
	if err := db.ReorderTaskRules(s.db, ids); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
