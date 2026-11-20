package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Notify rules (specs/durable-notifications.md, "Event notifications") are the
// third kind of event handler: they tell their team's members about a matching
// event through a notification and make no task. Reads, PATCH and DELETE are
// the shared handler routes, gated by team write as for every kind; this file
// is the create route and the notify PATCH schema.

// createNotifyRequest is the body of POST /api/event-handlers/notify. name and
// event_type are required; enabled defaults to TRUE: a rule the person just
// wrote is meant to start telling the team. A notify rule has no
// applies_to_unowned — it makes no task to widen the visibility of.
type createNotifyRequest struct {
	EventType      string          `json:"event_type"`
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        *bool           `json:"enabled"`
	// TeamID is the acting team the write picker supplied, as for a rule.
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
}

// patchNotifyRequest is PATCH /api/event-handlers/{id} for a notify rule:
// absent keeps a field, and scope_predicate: null clears the filter. enabled
// and name always hold a value, so a null for either is refused rather than
// read as "keep". event_type is immutable, as for every kind.
type patchNotifyRequest struct {
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        json.RawMessage `json:"enabled"`
	Name           json.RawMessage `json:"name"`
}

// refuseNotifyClear records a null for a field a notify rule always holds.
func refuseNotifyClear(v *httpx.Validation, state httpx.PatchState, field string) {
	if state == httpx.PatchClear {
		v.Invalid(field, field+" cannot be null on a notify rule")
	}
}

// notifyEventTypeField refuses a known event type a notify rule cannot be
// written on: a source whose tracked-set gate can fail open would tell people
// about entities their team does not track. A reference the caller can fix,
// so 422.
func notifyEventTypeField(v *httpx.Validation, eventType string) {
	if !domain.NotifyRuleSource(eventType) {
		v.Invalid("event_type", "notify rules cover GitHub, Jira and Linear events, and "+eventType+" is not one")
	}
}

// handleEventHandlerCreateNotify authors a kind='notify' handler.
//
// POST /api/event-handlers/notify
func (eh *eventHandlersHandler) handleEventHandlerCreateNotify(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req createNotifyRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	// The two statuses' accumulators — see handleEventHandlerCreateRule.
	var bad, unproc httpx.Validation
	name := strings.TrimSpace(req.Name)
	if name == "" {
		bad.Missing("name")
	}
	eventTypeOK := eventTypeField(&bad, req.EventType)
	if eventTypeOK && !gateEventTypeEntitlement(w, orgID, req.EventType) {
		return
	}
	var predicate *string
	if eventTypeOK {
		notifyEventTypeField(&unproc, req.EventType)
		predicate, _ = scopePredicateField(&bad, &unproc, req.ScopePredicate, req.EventType, nil)
	}
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	sourceAvailable, ok := eh.gateEventTypeSourceAvailability(w, r, &unproc, orgID, userID, req.EventType)
	if !ok {
		return
	}
	teamID, ok := eh.az.ActingTeamForWrite(w, r, orgID, userID, req.TeamID, "event_handlers")
	if !ok {
		return
	}
	found := unproc.Items()
	var catalogFaults []httpx.ErrorItem
	if sourceAvailable && domain.NotifyRuleSource(req.EventType) {
		if catalogFaults, ok = eh.checkPredicateCatalog(w, r, orgID, userID, teamID, req.EventType, predicate, nil, len(found) > 0); !ok {
			return
		}
	}
	h := domain.EventHandler{
		ID:                 uuid.New().String(),
		Kind:               domain.EventHandlerKindNotify,
		EventType:          req.EventType,
		Source:             domain.EventHandlerSourceUser,
		ScopePredicateJSON: predicate,
		Name:               name,
		Enabled:            true,
	}
	if req.Enabled != nil {
		h.Enabled = *req.Enabled
	}
	eh.insertEventHandler(w, r, orgID, userID, teamID, h, found, catalogFaults)
}

// applyNotifyPatch decodes a notify rule's PATCH body onto updated. It reports
// false when it has written a response.
func applyNotifyPatch(w http.ResponseWriter, r *http.Request, existing domain.EventHandler, updated *domain.EventHandler, bad, unproc *httpx.Validation) bool {
	var req patchNotifyRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return false
	}
	if enabled, state := httpx.PatchBool(bad, req.Enabled, "enabled"); state == httpx.PatchSet {
		updated.Enabled = enabled
	} else {
		refuseNotifyClear(bad, state, "enabled")
	}
	if predicate, named := scopePredicateField(bad, unproc, req.ScopePredicate, existing.EventType, existing.ScopePredicateJSON); named {
		updated.ScopePredicateJSON = predicate
	}
	if name, state := httpx.PatchString(bad, req.Name, "name"); state == httpx.PatchSet {
		if trimmed := strings.TrimSpace(name); trimmed == "" {
			bad.Invalid("name", "name cannot be empty")
		} else {
			updated.Name = trimmed
		}
	} else {
		refuseNotifyClear(bad, state, "name")
	}
	return true
}
