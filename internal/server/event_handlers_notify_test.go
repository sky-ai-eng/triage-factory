package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A notify rule is created enabled, is listed under kind=notify, and PATCHes
// its own fields.
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
	if got.Kind != domain.EventHandlerKindNotify || !got.Enabled ||
		got.Name != "CI failed on my PR" || got.CreatorUserID != runmode.LocalDefaultUserID || got.DefaultPriority != nil {
		t.Fatalf("created %+v; want an enabled notify rule named by its trimmed name, naming its creator", got)
	}
	if items := listEventHandlers(t, s, domain.EventHandlerKindNotify); len(items) != 1 || items[0]["id"] != got.ID {
		t.Errorf("list kind=notify = %v; want the one rule", items)
	}

	rec = doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, map[string]any{
		"name": "CI failed on the team's PRs", "enabled": false,
		"scope_predicate": map[string]any{"check_name": "build"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched domain.EventHandler
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched.Enabled || patched.Name != "CI failed on the team's PRs" ||
		patched.ScopePredicateJSON == nil || !strings.Contains(*patched.ScopePredicateJSON, "build") {
		t.Errorf("patched %+v; want the new name, filter and enabled flag", patched)
	}

	// Another kind's fields are unknown on a notify rule's PATCH, and a field
	// the rule always holds cannot be nulled; each refusal names its field.
	for _, body := range []map[string]any{{"default_priority": 0.5}, {"applies_to_unowned": true}, {"breaker_threshold": 3}} {
		if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, body); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %v: %d; want 400", body, rec.Code)
		}
	}
	for _, field := range []string{"name", "enabled"} {
		rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+got.ID, map[string]any{field: nil})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("patch %s: null: %d %s; want 400 naming %s", field, rec.Code, rec.Body.String(), field)
		}
	}
	if rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+got.ID, nil); !strings.Contains(rec.Body.String(), `"name":"CI failed on the team's PRs"`) {
		t.Errorf("after the refused nulls: %s; want the rule unchanged", rec.Body.String())
	}
	// A notify rule carries no sort order and cannot be promoted.
	if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{got.ID}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "notify rule") {
		t.Errorf("reorder with a notify rule: %d %s; want 422 naming it a notify rule", rec.Code, rec.Body.String())
	}
}

// Bad create bodies fail, each with the field named: a missing name, another
// kind's fields, and an event type whose source has no strict tracked-set
// gate.
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

// A notify rule tells its team and has no audience to name, so audience in a
// create or a PATCH body is an unknown field: a 400 naming it.
func TestNotifyRules_RefuseAudience(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/notify", map[string]any{
		"event_type": domain.EventGitHubPRCICheckFailed, "name": "x", "audience": "team",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with audience: %d %s; want 400", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "audience")

	rec = doJSON(t, s, http.MethodPost, "/api/event-handlers/notify", map[string]any{
		"event_type": domain.EventGitHubPRCICheckFailed, "name": "x",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created domain.EventHandler
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec = doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+created.ID, map[string]any{"audience": "creator"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch with audience: %d %s; want 400", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "audience")
}

// TestNotifyRules_TeamWriteGate_Postgres: a notify rule is gated by team write
// alone. A member who is not a team admin creates one, another such member
// changes and deletes it, and a viewer can do none of the three. The routes
// run under the callers' claims on the RLS pool, so the policies are held to
// the same rule.
func TestNotifyRules_TeamWriteGate_Postgres(t *testing.T) {
	r := newViewerRig(t)
	r.bindGitHubPAT(t)
	other := pgtest.SeedUser(t, r.h, "other")
	pgtest.AddOrgMember(t, r.h, other, r.orgID, r.teamID, "member", "member")

	create := func(caller string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r.eh.handleEventHandlerCreateNotify(rec, r.req(http.MethodPost, "/api/event-handlers/notify", caller, map[string]any{
			"event_type": domain.EventGitHubPRCICheckFailed, "name": "rule",
		}))
		return rec
	}
	patch := func(caller, id string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := r.req(http.MethodPatch, "/api/event-handlers/"+id, caller, body)
		req.SetPathValue("id", id)
		r.eh.handleEventHandlerUpdate(rec, req)
		return rec
	}
	del := func(caller, id string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := r.req(http.MethodDelete, "/api/event-handlers/"+id, caller, nil)
		req.SetPathValue("id", id)
		r.eh.handleEventHandlerDelete(rec, req)
		return rec.Code
	}

	if rec := create(r.viewer); rec.Code != http.StatusForbidden {
		t.Errorf("viewer creates a notify rule: %d %s; want 403", rec.Code, rec.Body.String())
	}
	rec := create(r.member)
	if rec.Code != http.StatusCreated {
		t.Fatalf("member creates a notify rule: %d %s; want 201", rec.Code, rec.Body.String())
	}
	var created domain.EventHandler
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created rule: %v %s", err, rec.Body.String())
	}
	if created.CreatorUserID != r.member || created.TeamID != r.teamID {
		t.Errorf("created %+v; want it on the team, naming the member who wrote it", created)
	}

	if rec := patch(r.viewer, created.ID, map[string]any{"enabled": false}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer changes the rule: %d %s; want 403", rec.Code, rec.Body.String())
	}
	rec = patch(other, created.ID, map[string]any{"name": "renamed", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("another member changes the rule: %d %s; want 200", rec.Code, rec.Body.String())
	}
	var patched domain.EventHandler
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil || patched.Name != "renamed" || patched.Enabled {
		t.Errorf("patched %+v, %v; want the new name, disabled", patched, err)
	}

	if code := del(r.viewer, created.ID); code != http.StatusForbidden {
		t.Errorf("viewer deletes the rule: %d; want 403", code)
	}
	if code := del(other, created.ID); code != http.StatusOK {
		t.Errorf("another member deletes the rule: %d; want 200", code)
	}
	var gone *domain.EventHandler
	if err := r.s.tx.WithReadTx(t.Context(), r.orgID, r.member, func(tx db.TxStores) error {
		var e error
		gone, e = tx.EventHandlers.Get(t.Context(), r.orgID, created.ID)
		return e
	}); err != nil || gone != nil {
		t.Errorf("after the delete: %+v, %v; want no rule", gone, err)
	}
}
