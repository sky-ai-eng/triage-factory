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
// third kind of event handler: they tell their audience about a matching
// event through a notification and make no task. Reads, PATCH and DELETE are
// the shared handler routes; this file is the create route, the notify PATCH
// schema, and the write gate a notify rule adds to team write.

// createNotifyRequest is the body of POST /api/event-handlers/notify. name and
// event_type are required; audience defaults to "creator" and enabled to TRUE:
// a rule the person just wrote is meant to start telling them. A notify rule
// has no applies_to_unowned — it makes no task to widen the visibility of.
type createNotifyRequest struct {
	EventType      string          `json:"event_type"`
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        *bool           `json:"enabled"`
	// TeamID is the acting team the write picker supplied, as for a rule.
	TeamID   string  `json:"team_id"`
	Name     string  `json:"name"`
	Audience *string `json:"audience"`
}

// patchNotifyRequest is PATCH /api/event-handlers/{id} for a notify rule:
// absent keeps a field, and scope_predicate: null clears the filter. enabled,
// name and audience always hold a value, so a null for one is refused rather
// than read as "keep". event_type is immutable, as for every kind.
type patchNotifyRequest struct {
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        json.RawMessage `json:"enabled"`
	Name           json.RawMessage `json:"name"`
	Audience       json.RawMessage `json:"audience"`
}

// notifyAudienceField validates a named audience, reporting whether it was
// named. A value outside the two is a malformed body, like an unknown kind.
func notifyAudienceField(v *httpx.Validation, raw *string) (string, bool) {
	if raw == nil {
		return "", false
	}
	switch *raw {
	case domain.NotifyAudienceCreator, domain.NotifyAudienceTeam:
		return *raw, true
	}
	v.Invalid("audience", `audience must be "creator" or "team"`)
	return "", true
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

// requireNotifyWrite is a notify rule's gate beyond team write. A rule whose
// audience is the team, before or after the write, badges every member, so it
// is a team admin's; a personal rule is its creator's, or a team admin's. It
// writes the refusal and reports false when the caller may not write. The
// event_handlers RLS policies hold Postgres to the same rule.
func (eh *eventHandlersHandler) requireNotifyWrite(w http.ResponseWriter, r *http.Request, orgID, userID, teamID, creatorID string, audiences ...string) bool {
	team := false
	for _, a := range audiences {
		team = team || a == domain.NotifyAudienceTeam
	}
	if !team && creatorID == userID {
		return true
	}
	admin, err := eh.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		internalError(w, "event_handlers", err)
		return false
	}
	if admin {
		return true
	}
	msg := "a personal notify rule is its creator's to change, or a team admin's"
	if team {
		msg = "a notify rule that tells the whole team is a team admin's to write"
	}
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{Reason: httpx.ReasonForbidden, Message: msg})
	return false
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
	audience, named := notifyAudienceField(&bad, req.Audience)
	if !named {
		audience = domain.NotifyAudienceCreator
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
	teamID, ok := actingTeamForWrite(w, r, eh.tx, eh.az, orgID, userID, req.TeamID, "event_handlers")
	if !ok {
		return
	}
	if !eh.requireNotifyWrite(w, r, orgID, userID, teamID, userID, audience) {
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
		Audience:           audience,
		Enabled:            true,
	}
	if req.Enabled != nil {
		h.Enabled = *req.Enabled
	}
	eh.insertEventHandler(w, r, orgID, userID, teamID, h, found, catalogFaults)
}

// applyNotifyPatch decodes a notify rule's PATCH body onto updated and runs
// the notify write gate against the audience before and after. It reports
// false when it has written a response.
func (eh *eventHandlersHandler) applyNotifyPatch(w http.ResponseWriter, r *http.Request, orgID, userID string, existing domain.EventHandler, updated *domain.EventHandler, bad, unproc *httpx.Validation) bool {
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
	if raw, state := httpx.PatchString(bad, req.Audience, "audience"); state == httpx.PatchSet {
		if audience, _ := notifyAudienceField(bad, &raw); audience != "" {
			updated.Audience = audience
		}
	} else {
		refuseNotifyClear(bad, state, "audience")
	}
	if len(bad.Items()) > 0 {
		// The body's own faults answer first, as for the other kinds.
		return true
	}
	return eh.requireNotifyWrite(w, r, orgID, userID, existing.TeamID, existing.CreatorUserID, existing.Audience, updated.Audience)
}
