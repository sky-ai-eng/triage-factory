package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A notify rule is created with its defaults (a personal, enabled rule), is
// listed under kind=notify, and PATCHes its own fields.
func TestHandleEventHandlerCreateNotify_HappyPathAndPatch(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/notify", map[string]any{
		"event_type": domain.EventGitHubPRCICheckFailed,
		"name":       "  CI failed on my PR  ",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var got domain.EventHandler
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != domain.EventHandlerKindNotify || got.Audience != domain.NotifyAudienceCreator || !got.Enabled ||
		got.Name != "CI failed on my PR" || got.CreatorUserID != runmode.LocalDefaultUserID || got.DefaultPriority != nil {
		t.Fatalf("created %+v; want an enabled personal notify rule named by its trimmed name, naming its creator", got)
	}
	if items := listEventHandlers(t, s, domain.EventHandlerKindNotify); len(items) != 1 || items[0]["id"] != got.ID {
		t.Errorf("list kind=notify = %v; want the one rule", items)
	}

	rec = doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, map[string]any{
		"name": "CI failed on the team's PRs", "audience": "team", "enabled": false,
		"scope_predicate": map[string]any{"check_name": "build"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched domain.EventHandler
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Audience != domain.NotifyAudienceTeam || patched.Enabled || patched.Name != "CI failed on the team's PRs" ||
		patched.ScopePredicateJSON == nil || !strings.Contains(*patched.ScopePredicateJSON, "build") {
		t.Errorf("patched %+v; want the new name, audience, filter and enabled flag", patched)
	}

	// Another kind's fields are unknown on a notify rule's PATCH, and a field
	// the rule always holds cannot be nulled; each refusal names its field.
	for _, body := range []map[string]any{{"default_priority": 0.5}, {"applies_to_unowned": true}, {"breaker_threshold": 3}} {
		if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, body); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %v: %d; want 400", body, rec.Code)
		}
	}
	for _, field := range []string{"name", "audience", "enabled"} {
		rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, map[string]any{field: nil})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("patch %s: null: %d %s; want 400 naming %s", field, rec.Code, rec.Body.String(), field)
		}
	}
	if rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+got.ID, nil); !strings.Contains(rec.Body.String(), `"audience":"team"`) {
		t.Errorf("after the refused nulls: %s; want the rule unchanged", rec.Body.String())
	}
	// A notify rule carries no sort order and cannot be promoted.
	if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{got.ID}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "notify rule") {
		t.Errorf("reorder with a notify rule: %d %s; want 422 naming it a notify rule", rec.Code, rec.Body.String())
	}
}

// Bad create bodies fail, each with the field named: a missing name, an
// audience outside the two, another kind's fields, and an event type whose
// source has no strict tracked-set gate.
func TestHandleEventHandlerCreateNotify_Refusals(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	// An extension's event type, as ee/slack registers its own.
	if _, registered := events.Get(domain.EventSlackMessage); !registered {
		events.Register(events.EventSchema{
			EventType: domain.EventSlackMessage,
			Ownership: events.OwnershipPool,
			Match:     func(string, string) (bool, error) { return true, nil },
		})
		t.Cleanup(func() { events.Reset(domain.EventSlackMessage) })
	}
	cases := []struct {
		name   string
		body   map[string]any
		status int
		field  string
	}{
		{"no name", map[string]any{"event_type": domain.EventGitHubPRCICheckFailed}, http.StatusBadRequest, "name"},
		{"bad audience", map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "name": "x", "audience": "org"}, http.StatusBadRequest, "audience"},
		{"rule field", map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "name": "x", "default_priority": 0.5}, http.StatusBadRequest, "default_priority"},
		{"watch flag", map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "name": "x", "applies_to_unowned": true}, http.StatusBadRequest, "applies_to_unowned"},
		{"slack event", map[string]any{"event_type": domain.EventSlackMessage, "name": "x"}, http.StatusUnprocessableEntity, "event_type"},
	}
	for _, c := range cases {
		rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/notify", c.body)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"field":"`+c.field+`"`) {
			t.Errorf("%s: %d %s; want %d naming %s", c.name, rec.Code, rec.Body.String(), c.status, c.field)
		}
	}
}

// TestNotifyRules_WriteGate_Postgres: a personal rule is a team writer's to
// create and its creator's or a team admin's to change; a rule that tells the
// whole team, before or after the write, is a team admin's. The RLS policies
// hold the store to the same rule when the server's gate is not in the way.
func TestNotifyRules_WriteGate_Postgres(t *testing.T) {
	r := newViewerRig(t)
	r.bindGitHubPAT(t)
	other := pgtest.SeedUser(t, r.h, "other")
	pgtest.AddOrgMember(t, r.h, other, r.orgID, r.teamID, "member", "member")

	create := func(caller string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["event_type"] = domain.EventGitHubPRCICheckFailed
		body["name"] = "rule"
		rec := httptest.NewRecorder()
		r.eh.handleEventHandlerCreateNotify(rec, r.req(http.MethodPost, "/api/event-handlers/notify", caller, body))
		return rec
	}
	idOf := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		var h domain.EventHandler
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || h.ID == "" {
			t.Fatalf("decode created rule: %v %s", err, rec.Body.String())
		}
		return h.ID
	}
	patch := func(caller, id string, body map[string]any) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := r.req(http.MethodPatch, "/api/event-handlers/"+id, caller, body)
		req.SetPathValue("id", id)
		r.eh.handleEventHandlerUpdate(rec, req)
		return rec.Code
	}
	del := func(caller, id string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := r.req(http.MethodDelete, "/api/event-handlers/"+id, caller, nil)
		req.SetPathValue("id", id)
		r.eh.handleEventHandlerDelete(rec, req)
		return rec.Code
	}

	if rec := create(r.viewer, map[string]any{}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer creates a personal rule: %d; want 403", rec.Code)
	}
	if rec := create(r.member, map[string]any{"audience": "team"}); rec.Code != http.StatusForbidden {
		t.Errorf("member creates a team rule: %d %s; want 403", rec.Code, rec.Body.String())
	}
	personal := create(r.member, map[string]any{})
	if personal.Code != http.StatusCreated {
		t.Fatalf("member creates a personal rule: %d %s", personal.Code, personal.Body.String())
	}
	mine := idOf(personal)
	team := create(r.admin, map[string]any{"audience": "team"})
	if team.Code != http.StatusCreated {
		t.Fatalf("admin creates a team rule: %d %s", team.Code, team.Body.String())
	}
	teamRule := idOf(team)

	if code := patch(other, mine, map[string]any{"name": "taken over"}); code != http.StatusForbidden {
		t.Errorf("another member edits a personal rule: %d; want 403", code)
	}
	if code := patch(r.member, mine, map[string]any{"name": "renamed"}); code != http.StatusOK {
		t.Errorf("creator edits their personal rule: %d; want 200", code)
	}
	if code := patch(r.member, mine, map[string]any{"audience": "team"}); code != http.StatusForbidden {
		t.Errorf("creator moves their rule to the team: %d; want 403", code)
	}
	if code := patch(r.admin, mine, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Errorf("team admin edits a personal rule: %d; want 200", code)
	}
	if code := patch(r.member, teamRule, map[string]any{"enabled": false}); code != http.StatusForbidden {
		t.Errorf("member edits the team rule: %d; want 403", code)
	}
	if code := del(r.member, teamRule); code != http.StatusForbidden {
		t.Errorf("member deletes the team rule: %d; want 403", code)
	}
	if code := del(other, mine); code != http.StatusForbidden {
		t.Errorf("another member deletes a personal rule: %d; want 403", code)
	}
	if code := del(r.admin, teamRule); code != http.StatusOK {
		t.Errorf("admin deletes the team rule: %d; want 200", code)
	}
	if code := del(r.member, mine); code != http.StatusOK {
		t.Errorf("creator deletes their personal rule: %d; want 200", code)
	}

	// Past the server: RLS refuses a member's team rule outright.
	err := r.s.tx.WithTx(t.Context(), r.orgID, r.member, func(tx db.TxStores) error {
		_, e := tx.EventHandlers.Create(t.Context(), r.orgID, r.teamID, domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindNotify, EventType: domain.EventGitHubPRCICheckFailed,
			Name: "around the gate", Audience: domain.NotifyAudienceTeam, Enabled: true,
		})
		return e
	})
	if err == nil {
		t.Error("a member's team notify rule passed RLS; want it refused")
	}
}
