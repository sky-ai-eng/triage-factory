package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// listEventHandlers reads one page of the handler list, narrowed to modes
// when any are named; the page bound is the route's max, which every fixture
// fits in.
func listEventHandlers(t *testing.T, s *Server, modes ...string) []map[string]any {
	t.Helper()
	body := map[string]any{"page_size": 200}
	if len(modes) > 0 {
		body["modes"] = modes
	}
	return httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/event-handlers/list", body)).Items
}

// seedHandlerBlueprint inserts the user-source blueprint a handler fixture
// points at, which the handler and the same-team FKs both require. Seeding
// an id twice is a no-op.
func seedHandlerBlueprint(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := t.Context()
	if existing, _ := s.blueprints.Get(ctx, runmode.LocalDefaultOrgID, id); existing != nil {
		return
	}
	if _, err := s.blueprints.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: id, Name: id, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}

// createHandler creates a handler from body, failing the test unless the
// create answers 201, and returns the row it answered with.
func createHandler(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create handler %v: %d: %s", body, rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if id, _ := got["id"].(string); id == "" {
		t.Fatalf("create handler: missing id in response: %s", rec.Body.String())
	}
	return got
}

// createHandlerID is createHandler for a caller that needs only the id.
func createHandlerID(t *testing.T, s *Server, body map[string]any) string {
	t.Helper()
	return createHandler(t, s, body)["id"].(string)
}

func createWatchHandler(t *testing.T, s *Server) string {
	return createWatchHandlerWithSort(t, s, "test-handler", 0)
}

func createWatchHandlerWithSort(t *testing.T, s *Server, name string, sortOrder int) string {
	t.Helper()
	return createHandlerID(t, s, map[string]any{
		"event_type": "github:pr:opened", "name": name, "sort_order": sortOrder,
	})
}

// createWatchHandlerFor creates a handler in watch bound to eventType and
// returns its id.
func createWatchHandlerFor(t *testing.T, s *Server, eventType string) string {
	t.Helper()
	return createHandlerID(t, s, map[string]any{"event_type": eventType, "name": "handler for " + eventType})
}

// createAutomateHandler creates a handler in automate on an already-seeded
// blueprint and returns its id.
func createAutomateHandler(t *testing.T, s *Server, blueprintID string) string {
	t.Helper()
	return createHandlerID(t, s, map[string]any{
		"event_type": "github:pr:ci_check_failed", "mode": domain.EventHandlerModeAutomate, "blueprint_id": blueprintID,
	})
}

// getEventHandler reads one handler through the single read, failing the
// test unless it answers 200.
func getEventHandler(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/event-handlers/%s = %d: %s", id, rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return got
}

// catalogLabel is eventType's label in the event-type catalog.
func catalogLabel(t *testing.T, eventType string) string {
	t.Helper()
	for _, et := range domain.AllEventTypes() {
		if et.ID == eventType {
			return et.Label
		}
	}
	t.Fatalf("%s is not in the catalog", eventType)
	return ""
}

// --- POST /api/event-handlers ---------------------------------------------

// TestHandleEventHandlerCreate_Defaults: event_type is the one field a create
// needs. Every other field takes its default, and the name is the event
// type's catalog label, or the blueprint's name when the body names one.
func TestHandleEventHandlerCreate_Defaults(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	got := createHandler(t, s, map[string]any{"event_type": domain.EventGitHubPROpened})
	want := map[string]any{
		"mode": "watch", "name": catalogLabel(t, domain.EventGitHubPROpened), "scope_predicate_json": nil,
		"default_priority": 0.5, "sort_order": float64(0), "blueprint_id": nil, "breaker_threshold": float64(4),
		"min_autonomy_suitability": float64(0), "applies_to_unowned": false, "source": "user",
		"creator_user_id": runmode.LocalDefaultUserID, "team_id": runmode.LocalDefaultTeamID,
	}
	for field, value := range want {
		if got[field] != value {
			t.Errorf("%s = %v, want %v", field, got[field], value)
		}
	}

	bp, _ := createWrappedBlueprint(t, s, "Named Blueprint")
	got = createHandler(t, s, map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": bp})
	if got["name"] != "Named Blueprint" || got["blueprint_id"] != bp || got["mode"] != "watch" {
		t.Errorf("created with a blueprint = %v, want it in watch, on the blueprint and named after it", got)
	}
	got = createHandler(t, s, map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": bp, "name": "  Own name  "})
	if got["name"] != "Own name" {
		t.Errorf("name = %v, want the body's own, trimmed", got["name"])
	}
}

// TestHandleEventHandlerCreate_EveryMode: a handler is created in each mode,
// and the single read answers the mode it was created in.
func TestHandleEventHandlerCreate_EveryMode(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-every-mode")

	for _, mode := range domain.EventHandlerModes {
		t.Run(mode, func(t *testing.T) {
			body := map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "mode": mode}
			if mode == domain.EventHandlerModeAutomate {
				body["blueprint_id"] = "p-every-mode"
			}
			id := createHandlerID(t, s, body)
			if got := getEventHandler(t, s, id)["mode"]; got != mode {
				t.Errorf("mode read back = %v, want %s", got, mode)
			}
		})
	}
}

// TestHandleEventHandlerCreate_AutomateNeedsBlueprint: automate runs the
// handler's blueprint, so a create in automate with none is a 422 on mode,
// and nothing is written.
func TestHandleEventHandlerCreate_AutomateNeedsBlueprint(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"event_type": domain.EventGitHubPRCICheckFailed, "mode": domain.EventHandlerModeAutomate,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "mode" {
		t.Errorf("faults = %+v, want one INVALID_FIELD on mode", items)
	}
	if n := len(listEventHandlers(t, s)); n != 0 {
		t.Errorf("a refused create left %d handlers", n)
	}
}

// TestEventHandlers_NotifyNeedsACoreSource: notify covers GitHub, Jira and
// Linear events, whose tracked-set gate core owns, so a create in notify on
// any other event, or a PATCH moving such a handler to notify, is a 422 on
// mode.
func TestEventHandlers_NotifyNeedsACoreSource(t *testing.T) {
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
	if domain.NotifyRuleSource(domain.EventSlackMessage) {
		t.Fatalf("%s is a notify source; the fixture needs one that is not", domain.EventSlackMessage)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"event_type": domain.EventSlackMessage, "mode": domain.EventHandlerModeNotify,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create in notify: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "mode" {
		t.Errorf("create in notify: faults = %+v, want one INVALID_FIELD on mode", items)
	}

	id := createHandlerID(t, s, map[string]any{"event_type": domain.EventSlackMessage})
	rec = doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"mode": domain.EventHandlerModeNotify})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH to notify: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "mode" {
		t.Errorf("PATCH to notify: faults = %+v, want one INVALID_FIELD on mode", items)
	}
	if got := getEventHandler(t, s, id)["mode"]; got != domain.EventHandlerModeWatch {
		t.Errorf("mode after the refused PATCH = %v, want watch", got)
	}
}

// TestEventHandlers_BlueprintMustBeLive: a blueprint_id naming no live
// blueprint is a 404 on create and PATCH alike, and nothing is written.
func TestEventHandlers_BlueprintMustBeLive(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createWatchHandler(t, s)
	const missing = "11111111-1111-1111-1111-111111111111"

	for name, send := range map[string]func() int{
		"create": func() int {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
				"event_type": domain.EventGitHubPROpened, "blueprint_id": missing,
			}).Code
		},
		"patch": func() int {
			return doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"blueprint_id": missing}).Code
		},
	} {
		if code := send(); code != http.StatusNotFound {
			t.Errorf("%s naming a missing blueprint = %d, want 404", name, code)
		}
	}
	if n := len(listEventHandlers(t, s)); n != 1 {
		t.Errorf("handlers after the refused writes = %d, want the one seeded", n)
	}
	if got := getEventHandler(t, s, id)["blueprint_id"]; got != nil {
		t.Errorf("blueprint_id after the refused PATCH = %v, want null", got)
	}
}

// TestHandleEventHandlerCreate_RejectsSystemEventType pins that a system
// sentinel — including the poll's close obligation — is never a handler's
// subject: the router matches no handler on one, so the create refuses it as
// a field fault rather than storing a handler that would never fire.
func TestHandleEventHandlerCreate_RejectsSystemEventType(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	for _, eventType := range []string{domain.EventSystemEntityCloseOwed, domain.EventSystemPollCompleted, domain.EventSystemRoutingDisposition} {
		rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
			"event_type": eventType,
			"name":       "x",
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", eventType, rec.Code, rec.Body.String())
			continue
		}
		items := decodeErrorItems(t, rec)
		if len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "event_type" {
			t.Errorf("%s: want one INVALID_FIELD item on event_type, got %+v", eventType, items)
		}
	}
}

func TestHandleEventHandlerCreate_RejectsUnknownEventType(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"event_type": "not:a:real:event",
		"name":       "x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleEventHandlerCreate_CanonicalizesPredicate(t *testing.T) {
	// The predicate canonicalizer normalizes empty / {} / null to "" so
	// the wire-out form is identical for match-all regardless of the
	// shape the client sent. Pin the round-trip.
	s := newTestServer(t)
	configureEventSources(t, s)
	for name, predicate := range map[string]any{
		"empty object":  map[string]any{},
		"explicit null": nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := createHandler(t, s, map[string]any{
				"event_type":      "github:pr:new_commits",
				"name":            "match-all " + name,
				"scope_predicate": predicate,
			})
			if got["scope_predicate_json"] != nil {
				t.Errorf("empty predicate must round-trip as null, got %v", got["scope_predicate_json"])
			}
		})
	}
}

// TestHandleEventHandlerCreate_PredicateIsAnObject: the predicate's one wire
// type is a JSON object, stored canonically; a string-wrapped object is refused
// rather than unwrapped, so the field has one spelling.
func TestHandleEventHandlerCreate_PredicateIsAnObject(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	body := func(predicate any) map[string]any {
		return map[string]any{
			"event_type": "github:pr:new_commits", "name": "predicate", "scope_predicate": predicate,
		}
	}

	got := createHandler(t, s, body(map[string]any{"author_id_in": []string{"583231"}}))
	if got["scope_predicate_json"] != `{"author_id_in":["583231"]}` {
		t.Errorf("stored predicate = %v, want the canonical object text", got["scope_predicate_json"])
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", body(`{"author_id_in":["583231"]}`)); rec.Code != http.StatusBadRequest {
		t.Errorf("string-wrapped predicate: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEventHandlerWrites_RejectUnknownFields: create and PATCH decode one
// fixed schema strictly, so a field outside it — a kind or an enabled flag, an
// audience, or the immutable team_id and event_type on a PATCH — is
// UNKNOWN_FIELD rather than accepted and dropped.
func TestEventHandlerWrites_RejectUnknownFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createWatchHandler(t, s)

	for name, tc := range map[string]struct {
		method, path string
		body         map[string]any
		field        string
	}{
		"kind on create":       {http.MethodPost, "/api/event-handlers", map[string]any{"event_type": "github:pr:opened", "kind": "rule"}, "kind"},
		"audience on create":   {http.MethodPost, "/api/event-handlers", map[string]any{"event_type": "github:pr:opened", "audience": "team"}, "audience"},
		"enabled on PATCH":     {http.MethodPatch, "/api/event-handlers/" + id, map[string]any{"enabled": false}, "enabled"},
		"event_type on PATCH":  {http.MethodPatch, "/api/event-handlers/" + id, map[string]any{"event_type": "github:pr:new_commits"}, "event_type"},
		"team_id on PATCH":     {http.MethodPatch, "/api/event-handlers/" + id, map[string]any{"team_id": runmode.LocalDefaultTeamID}, "team_id"},
		"trigger_type on POST": {http.MethodPost, "/api/event-handlers", map[string]any{"event_type": "github:pr:opened", "trigger_type": "event"}, "trigger_type"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if reason := firstErrorReason(t, rec); reason != "UNKNOWN_FIELD" {
				t.Errorf("reason = %q, want UNKNOWN_FIELD", reason)
			}
			assertFieldError(t, rec, tc.field)
		})
	}
	if n := len(listEventHandlers(t, s)); n != 1 {
		t.Errorf("handlers after the refused writes = %d, want the one seeded", n)
	}
}

// --- PATCH /api/event-handlers/{id} --------------------------------------

// TestHandleEventHandlerUpdate_PatchesFields: PATCH writes the named fields,
// mode among them, and the list reads the write back.
func TestHandleEventHandlerUpdate_PatchesFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createWatchHandler(t, s)

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"name":             "renamed",
		"default_priority": 0.9,
		"mode":             domain.EventHandlerModeOff,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["name"] != "renamed" || got["default_priority"] != 0.9 || got["mode"] != "off" {
		t.Errorf("patched = %v, want the new name, priority and mode", got)
	}
	var listed map[string]any
	for _, h := range listEventHandlers(t, s) {
		if h["id"] == id {
			listed = h
		}
	}
	if listed == nil || listed["mode"] != "off" || listed["name"] != "renamed" {
		t.Errorf("listed handler = %v, want it present, renamed and off", listed)
	}
}

// TestHandleEventHandlerUpdate_ModeAloneKeepsEveryField: a handler keeps every
// field in every mode, so a PATCH naming only mode moves it through each mode
// and leaves the rest of the row as it was.
func TestHandleEventHandlerUpdate_ModeAloneKeepsEveryField(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-mode-only")
	before := createHandler(t, s, map[string]any{
		"event_type": domain.EventGitHubPROpened, "mode": domain.EventHandlerModeAutomate, "name": "every field",
		"scope_predicate": map[string]any{"author_id_in": []string{"583231"}}, "default_priority": 0.8,
		"sort_order": 7, "blueprint_id": "p-mode-only", "breaker_threshold": 9,
		"min_autonomy_suitability": 0.6, "applies_to_unowned": true,
	})
	id := before["id"].(string)

	for _, mode := range []string{domain.EventHandlerModeWatch, domain.EventHandlerModeNotify, domain.EventHandlerModeOff, domain.EventHandlerModeAutomate} {
		rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"mode": mode})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH mode %s: %d: %s", mode, rec.Code, rec.Body.String())
		}
		var after map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &after)
		if after["mode"] != mode {
			t.Errorf("mode = %v, want %s", after["mode"], mode)
		}
		for field, want := range before {
			if field == "mode" || field == "updated_at" {
				continue
			}
			if fmt.Sprint(after[field]) != fmt.Sprint(want) {
				t.Errorf("PATCH mode %s: %s = %v, want %v kept", mode, field, after[field], want)
			}
		}
	}
}

// TestHandleEventHandlerUpdate_AutomateKeepsItsBlueprint: a handler in
// automate needs its blueprint. Clearing it on a handler staying in automate
// is the blueprint_id field's 422, moving a handler with none into automate
// is the mode field's, and leaving automate and clearing it in one PATCH is
// accepted.
func TestHandleEventHandlerUpdate_AutomateKeepsItsBlueprint(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-keeps-blueprint")
	automating := createAutomateHandler(t, s, "p-keeps-blueprint")
	watching := createWatchHandler(t, s)

	for name, tc := range map[string]struct {
		id    string
		body  map[string]any
		field string
	}{
		"clear the blueprint of a handler in automate": {automating, map[string]any{"blueprint_id": nil}, "blueprint_id"},
		"move a handler with no blueprint to automate": {watching, map[string]any{"mode": domain.EventHandlerModeAutomate}, "mode"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+tc.id, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
			if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != tc.field {
				t.Errorf("faults = %+v, want one INVALID_FIELD on %s", items, tc.field)
			}
		})
	}
	if got := getEventHandler(t, s, automating); got["mode"] != "automate" || got["blueprint_id"] != "p-keeps-blueprint" {
		t.Errorf("after the refused PATCH = %v, want it still automating its blueprint", got)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+automating, map[string]any{
		"mode": domain.EventHandlerModeWatch, "blueprint_id": nil,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("leave automate and clear the blueprint: %d: %s", rec.Code, rec.Body.String())
	}
	if got := getEventHandler(t, s, automating); got["mode"] != "watch" || got["blueprint_id"] != nil {
		t.Errorf("after leaving automate = %v, want it in watch with no blueprint", got)
	}
}

// TestHandleEventHandlerUpdate_RefusesNullForValueFields: only scope_predicate
// and blueprint_id clear to null. Every other field always holds a value, so
// a null for one is a 400 naming it, and the handler is left as it was.
func TestHandleEventHandlerUpdate_RefusesNullForValueFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createWatchHandler(t, s)
	before := getEventHandler(t, s, id)

	for _, field := range []string{
		"mode", "name", "default_priority", "sort_order", "breaker_threshold", "min_autonomy_suitability", "applies_to_unowned",
	} {
		rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{field: nil})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: null: %d %s; want 400", field, rec.Code, rec.Body.String())
			continue
		}
		assertFieldError(t, rec, field)
	}
	if after := getEventHandler(t, s, id); !maps.Equal(after, before) {
		t.Errorf("after the refused nulls = %v, want the handler unchanged: %v", after, before)
	}
}

func TestHandleEventHandlerUpdate_NotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/does-not-exist", map[string]any{
		"name": "x",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestHandleEventHandlerRemovedRoutes: one create, one PATCH and one delete
// are the whole write surface, so a PUT alias, the verb routes and the
// per-kind creates answer 404 and leave the handler as it was.
func TestHandleEventHandlerRemovedRoutes(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-removed-routes")
	id := createWatchHandler(t, s)
	before := getEventHandler(t, s, id)

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"PUT alias":      {http.MethodPut, "/api/event-handlers/" + id, map[string]any{"name": "x"}},
		"toggle verb":    {http.MethodPost, "/api/event-handlers/" + id + "/toggle", map[string]any{"enabled": false}},
		"rule create":    {http.MethodPost, "/api/event-handlers/rules", map[string]any{"event_type": "github:pr:opened", "name": "x"}},
		"trigger create": {http.MethodPost, "/api/event-handlers/triggers", map[string]any{"event_type": "github:pr:opened", "blueprint_id": "p-removed-routes"}},
		"notify create":  {http.MethodPost, "/api/event-handlers/notify", map[string]any{"event_type": "github:pr:opened", "name": "x"}},
		"promote":        {http.MethodPost, "/api/event-handlers/" + id + "/promote", map[string]any{"blueprint_id": "p-removed-routes"}},
		"retarget":       {http.MethodPost, "/api/event-handlers/" + id + "/retarget", map[string]any{"blueprint_id": "p-removed-routes"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	if after := getEventHandler(t, s, id); !maps.Equal(after, before) {
		t.Errorf("after the refused calls = %v, want the handler unchanged: %v", after, before)
	}
	if n := len(listEventHandlers(t, s)); n != 1 {
		t.Errorf("handlers after the refused calls = %d, want the one seeded", n)
	}
}

// --- DELETE /api/event-handlers/{id} -------------------------------------

// TestHandleEventHandlerDelete_SystemRowSoftDeletes: a shipped (system_slug)
// handler is soft-deleted, so its slug stays occupied and the boot-time sync
// never resurrects it, while every read answers 404.
func TestHandleEventHandlerDelete_SystemRowSoftDeletes(t *testing.T) {
	s := newTestServer(t)
	// Insert a system-source handler directly (newTestServer seeds the
	// tenant but no handlers; provisioning would, but a single row keeps
	// the test focused). source='system' requires creator_user_id NULL.
	id := fixtureUUID("sys-handler-del")
	if _, err := s.db.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, mode, event_type, source, system_slug, name, default_priority, sort_order, creator_user_id)
		VALUES (?, ?, ?, 'watch', 'github:pr:opened', 'system', 'system-test-del', 'Shipped Handler', 0.5, 0, NULL)
	`, id, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed system handler: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "deleted" {
		t.Errorf("status=%v want deleted", got["status"])
	}
	// The reads answer 404 (deleted_at IS NULL filter) to every caller but the
	// sync.
	if follow := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil); follow.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", follow.Code)
	}
	if follow := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "x"}); follow.Code != http.StatusNotFound {
		t.Errorf("PATCH after delete = %d, want 404", follow.Code)
	}
	// The row survives (deleted_at stamped, not removed) so the slug slot
	// stays occupied, and its mode is untouched: it is hidden from every read
	// path, not switched off.
	var (
		deletedAt sql.NullString
		mode      string
	)
	if err := s.db.QueryRow(`SELECT deleted_at, mode FROM event_handlers WHERE id = ?`, id).Scan(&deletedAt, &mode); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if !deletedAt.Valid || mode != domain.EventHandlerModeWatch {
		t.Errorf("deleted_at = %v, mode = %q; want the shipped row soft-deleted with its mode kept", deletedAt, mode)
	}
}

// TestHandleEventHandlerDelete_HandlerThatHasRun: a handler that has run is
// referenced by its conversation and blueprint run, and the delete keeps it as
// a deleted row. It leaves every read, and the runs keep their trigger_id.
func TestHandleEventHandlerDelete_HandlerThatHasRun(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	bp, _ := createWrappedBlueprint(t, s, "Ran")
	handler := attachHandler(t, s, bp, "github:pr:ci_check_failed")
	run := seedHandlerRun(t, s, handler, "trgdel")

	rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+handler, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if g := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+handler, nil); g.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", g.Code)
	}
	if n := len(handlersForBlueprint(t, s, bp)); n != 0 {
		t.Errorf("handlers listed for the blueprint after delete = %d, want 0", n)
	}
	if c, br := runTriggerIDs(t, s, run); c != handler || br != handler {
		t.Errorf("run trigger_id after delete = (conversation %q, blueprint run %q), want %q", c, br, handler)
	}
}

// --- PUT /api/event-handlers/reorder -------------------------------------

// TestHandleEventHandlerReorder_AppliesOrderAcrossModes: the order is one list
// over every mode, so a reorder names a handler in each and the list reads
// back in the order it was given.
func TestHandleEventHandlerReorder_AppliesOrderAcrossModes(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-reorder-modes")
	var ids []string
	for i, mode := range domain.EventHandlerModes {
		body := map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "mode": mode, "name": "handler-" + mode, "sort_order": i}
		if mode == domain.EventHandlerModeAutomate {
			body["blueprint_id"] = "p-reorder-modes"
		}
		ids = append(ids, createHandlerID(t, s, body))
	}
	slices.Reverse(ids)

	rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": ids})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []string
	for _, h := range listEventHandlers(t, s) {
		got = append(got, h["id"].(string))
	}
	if !slices.Equal(got, ids) {
		t.Errorf("list order after reorder = %v, want %v", got, ids)
	}
}

func TestHandleEventHandlerReorder_RejectsEmpty(t *testing.T) {
	s := newTestServer(t)
	for name, body := range map[string]any{
		"empty list": map[string]any{"ids": []string{}},
		"no ids key": map[string]any{},
		"bare array": []string{},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleEventHandlerReorder_RejectsInexactSet: the body must name every
// visible handler exactly once, so an unknown id, a duplicate or an omitted
// handler is a 422, each fault naming its id and all reported at once.
func TestHandleEventHandlerReorder_RejectsInexactSet(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	idA := createWatchHandlerWithSort(t, s, "handler-a", 0)
	idB := createWatchHandlerWithSort(t, s, "handler-b", 1)
	unknownA := "11111111-1111-1111-1111-111111111111"
	unknownB := "22222222-2222-2222-2222-222222222222"

	t.Run("every unknown id is named", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idA, idB, unknownA, unknownB}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{unknownA, unknownB} {
			if !strings.Contains(body, want) {
				t.Errorf("error body does not name %s: %s", want, body)
			}
		}
		if items := decodeErrorItems(t, rec); len(items) != 2 {
			t.Errorf("got %d error items, want one per bad id: %v", len(items), items)
		}
	})

	t.Run("a partial list is refused", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{idA}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), idB) {
			t.Errorf("error body does not name the omitted handler %s: %s", idB, rec.Body.String())
		}
	})

	t.Run("duplicates are refused", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{idA, idA}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a deleted handler leaves the list", func(t *testing.T) {
		idC := createWatchHandlerWithSort(t, s, "handler-c", 2)
		if rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+idC, nil); rec.Code != http.StatusOK {
			t.Fatalf("delete handler-c: %d: %s", rec.Code, rec.Body.String())
		}
		// Naming it is now a fault...
		if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idB, idA, idC}}); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("deleted id: expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		// ...and omitting it is the whole visible set again.
		if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idB, idA}}); rec.Code != http.StatusOK {
			t.Errorf("exact set after delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- POST /api/event-handlers/list ---------------------------------------

// TestHandleEventHandlersList_Narrowing: modes, event_type and has_blueprint
// each narrow the list, and total_count counts what they leave.
func TestHandleEventHandlersList_Narrowing(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "p-list")
	watching := createHandlerID(t, s, map[string]any{"event_type": domain.EventGitHubPROpened, "mode": "watch"})
	automating := createHandlerID(t, s, map[string]any{
		"event_type": domain.EventGitHubPRCICheckFailed, "mode": "automate", "blueprint_id": "p-list",
	})
	notifying := createHandlerID(t, s, map[string]any{"event_type": domain.EventGitHubPRCICheckFailed, "mode": "notify"})
	off := createHandlerID(t, s, map[string]any{
		"event_type": domain.EventGitHubPROpened, "mode": "off", "blueprint_id": "p-list",
	})

	for name, tc := range map[string]struct {
		body map[string]any
		want []string
	}{
		"one mode":         {map[string]any{"modes": []string{"watch"}}, []string{watching}},
		"two modes":        {map[string]any{"modes": []string{"notify", "automate"}}, []string{automating, notifying}},
		"event type":       {map[string]any{"event_type": domain.EventGitHubPRCICheckFailed}, []string{automating, notifying}},
		"has a blueprint":  {map[string]any{"has_blueprint": true}, []string{automating, off}},
		"has no blueprint": {map[string]any{"has_blueprint": false}, []string{watching, notifying}},
		"every narrowing": {map[string]any{
			"modes": []string{"off", "watch"}, "event_type": domain.EventGitHubPROpened, "has_blueprint": true,
		}, []string{off}},
	} {
		t.Run(name, func(t *testing.T) {
			page := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/event-handlers/list", tc.body))
			var got []string
			for _, h := range page.Items {
				got = append(got, h["id"].(string))
			}
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) || page.Total() != len(want) {
				t.Errorf("listed %v (total %d), want %v", got, page.Total(), want)
			}
		})
	}
}

// TestHandleEventHandlersList_RejectsUnknownFilterValues: a mode or an event
// type the server does not know is a 400 naming it, never a filter dropped
// to widen the read.
func TestHandleEventHandlersList_RejectsUnknownFilterValues(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"modes": []string{"watch", "banana"}}, "modes[1]"},
		{map[string]any{"event_type": "not:a:real:event"}, "event_type"},
	} {
		rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/list", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400; body=%s", tc.body, rec.Code, rec.Body.String())
			continue
		}
		assertFieldError(t, rec, tc.field)
	}
}

// TestHandleEventHandlersList_RefusesFieldsItDoesNotFilterOn: the list has no
// kind, audience or creator filter, so each in a body is an unknown field, a
// 400 naming it rather than a filter that is dropped.
func TestHandleEventHandlersList_RefusesFieldsItDoesNotFilterOn(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"kind": "rule"}, "kind"},
		{map[string]any{"audience": "team"}, "audience"},
		{map[string]any{"creator_user_id": runmode.LocalDefaultUserID}, "creator_user_id"},
	} {
		rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/list", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400; body=%s", tc.body, rec.Code, rec.Body.String())
			continue
		}
		assertFieldError(t, rec, tc.field)
	}
}

// TestHandleEventHandlersList_TokenIsBoundToItsFilters: a page token minted
// under one mode set is refused under another, so a walk cannot continue into
// a different result set.
func TestHandleEventHandlersList_TokenIsBoundToItsFilters(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	createWatchHandlerWithSort(t, s, "a", 0)
	createWatchHandlerWithSort(t, s, "b", 1)
	first := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/event-handlers/list",
		map[string]any{"modes": []string{"watch"}, "page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatal("modes=[watch] with page_size 1 minted no next page token")
	}
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/list",
		map[string]any{"modes": []string{"notify"}, "page_size": 1, "page_token": first.NextPageToken})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token minted under modes=[watch] reused under modes=[notify] = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "page_token")
}

// --- GET /api/event-handlers/{id} ----------------------------------------

// TestHandleEventHandlerGet_ShapeParityAndNotFound: a write answers the
// stored row's fields and no others, the single read answers the list's row
// field for field, which is the stored row plus source_available, and an
// unknown or malformed id is a 404 either way.
func TestHandleEventHandlerGet_ShapeParityAndNotFound(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	row := createHandler(t, s, map[string]any{
		"event_type":       "github:pr:new_commits",
		"name":             "Heads-up on new commits",
		"default_priority": 0.6,
	})
	id := row["id"].(string)
	stored := []string{
		"id", "team_id", "event_type", "mode", "name", "scope_predicate_json", "default_priority", "sort_order",
		"blueprint_id", "breaker_threshold", "min_autonomy_suitability", "applies_to_unowned", "source",
		"creator_user_id", "created_at", "updated_at",
	}
	if got := slices.Sorted(maps.Keys(row)); !slices.Equal(got, slices.Sorted(slices.Values(stored))) {
		t.Errorf("created row fields = %v, want %v", got, slices.Sorted(slices.Values(stored)))
	}

	got := getEventHandler(t, s, id)
	if want := slices.Sorted(slices.Values(append(stored, "source_available"))); !slices.Equal(slices.Sorted(maps.Keys(got)), want) {
		t.Errorf("single read fields = %v, want %v", slices.Sorted(maps.Keys(got)), want)
	}
	for _, field := range stored {
		if fmt.Sprint(got[field]) != fmt.Sprint(row[field]) {
			t.Errorf("field %q: single read = %v, created row = %v", field, got[field], row[field])
		}
	}

	// Shape parity with the list row — same keys, same values. A bespoke
	// single-read projection is exactly what this asserts against.
	var listed map[string]any
	for _, h := range listEventHandlers(t, s) {
		if h["id"] == id {
			listed = h
		}
	}
	if listed == nil {
		t.Fatalf("seeded handler %s absent from the list", id)
	}
	if len(got) != len(listed) {
		t.Errorf("single read has %d keys, list row has %d — the projections have diverged\nsingle=%v\nlist=%v",
			len(got), len(listed), got, listed)
	}
	for k, want := range listed {
		if fmt.Sprint(got[k]) != fmt.Sprint(want) {
			t.Errorf("field %q: single read = %v, list row = %v", k, got[k], want)
		}
	}

	for _, badID := range []string{"11111111-1111-1111-1111-111111111111", "not-a-uuid"} {
		if rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+badID, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/event-handlers/%s = %d, want 404; body=%s", badID, rec.Code, rec.Body.String())
		}
	}
}
