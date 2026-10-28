package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Every write route that takes a scope predicate answers its faults the same
// way: a predicate that does not fit the schema is 400, an unknown key
// UNKNOWN_FIELD, a value the field refuses is 422, each fault on the key it is
// about, every one reported and none in the decoder's words.
func TestEventHandlerPredicateFaults_EveryWriteRoute(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-predicate-faults")
	ruleID := createUserRule(t, s)

	routes := map[string]func(predicate map[string]any) *httptest.ResponseRecorder{
		"create rule": func(predicate map[string]any) *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
				"event_type": "github:pr:opened", "name": "faults", "scope_predicate": predicate,
			})
		},
		"create trigger": func(predicate map[string]any) *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
				"event_type": "github:pr:ci_check_failed", "blueprint_id": "p-predicate-faults", "scope_predicate": predicate,
			})
		},
		"patch": func(predicate map[string]any) *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+ruleID, map[string]any{"scope_predicate": predicate})
		},
		"promote": func(predicate map[string]any) *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers/"+ruleID+"/promote", map[string]any{
				"blueprint_id": "p-predicate-faults", "scope_predicate": predicate,
			})
		},
	}
	cases := map[string]struct {
		predicate   map[string]any
		status      int
		wantReasons []string
		wantField   string
	}{
		"unknown key": {
			map[string]any{"repo": "acme/api"},
			http.StatusBadRequest, []string{"UNKNOWN_FIELD"}, "scope_predicate.repo",
		},
		"wrong type": {
			map[string]any{"author_id_in": "583231"},
			http.StatusBadRequest, []string{"INVALID_FIELD"}, "scope_predicate.author_id_in",
		},
		"repeated and empty entries": {
			map[string]any{"author_id_in": []string{"583231", "583231", ""}},
			http.StatusUnprocessableEntity, []string{"INVALID_FIELD", "INVALID_FIELD"}, "scope_predicate.author_id_in",
		},
	}
	for route, write := range routes {
		for name, tc := range cases {
			t.Run(route+"/"+name, func(t *testing.T) {
				rec := write(tc.predicate)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
				}
				items := decodeErrorItems(t, rec)
				if len(items) != len(tc.wantReasons) {
					t.Fatalf("items = %+v, want %d", items, len(tc.wantReasons))
				}
				for i, item := range items {
					if item.Reason != tc.wantReasons[i] || item.Field != tc.wantField {
						t.Errorf("item %d = %+v, want %s on %s", i, item, tc.wantReasons[i], tc.wantField)
					}
					if strings.Contains(item.Message, "json:") {
						t.Errorf("item %d message %q carries the decoder's text", i, item.Message)
					}
				}
			})
		}
	}

	// None of the refused writes touched the rule.
	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+ruleID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kind":"rule"`) || !strings.Contains(rec.Body.String(), `"scope_predicate_json":null`) {
		t.Errorf("rule after refused writes: %d %s", rec.Code, rec.Body.String())
	}
}

// An enum miss is a well-formed value outside the field's set: 422, like an
// out-of-range priority, reported beside the body's other 422 faults.
func TestEventHandlerPredicateFaults_EnumMissIs422(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":       "github:pr:mentioned",
		"name":             "mentions",
		"default_priority": 2,
		"scope_predicate":  map[string]any{"mention_source": "bogus"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	fields := map[string]string{}
	for _, item := range items {
		fields[item.Field] = item.Reason
	}
	if len(items) != 2 || fields["default_priority"] != "OUT_OF_RANGE" || fields["scope_predicate.mention_source"] != "INVALID_FIELD" {
		t.Errorf("items = %+v, want default_priority and scope_predicate.mention_source", items)
	}
}

// Every 422 a write meets comes back in one answer: the body's own, the
// predicate's value faults, and what the reference check refuses, last, each
// on its own field. The check reads the predicate with its refused entries
// taken out, and nothing is written.
func TestEventHandlerPredicateFaults_OneAnswerForEvery422(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	calls := stubPredicateReferenceCheck(t)
	seedBlueprintForTrigger(t, s, "p-every-422")
	seeded := labelRule(t, s, "ok")
	if seeded.Code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %s", seeded.Code, seeded.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(seeded.Body.Bytes(), &created)
	id := created["id"].(string)

	predicate := map[string]any{"author_id_in": []string{"583231", "583231"}, "has_label": "forbidden"}
	for _, tc := range []struct {
		route, method, path string
		body                map[string]any
		tuning              string
	}{
		{"create rule", http.MethodPost, "/api/event-handlers/rules", map[string]any{
			"event_type": referenceCheckEventType, "name": "every 422", "default_priority": 2, "scope_predicate": predicate,
		}, "default_priority"},
		{"create trigger", http.MethodPost, "/api/event-handlers/triggers", map[string]any{
			"event_type": referenceCheckEventType, "blueprint_id": "p-every-422", "breaker_threshold": -1, "scope_predicate": predicate,
		}, "breaker_threshold"},
		{"patch", http.MethodPatch, "/api/event-handlers/" + id, map[string]any{
			"default_priority": 2, "scope_predicate": predicate,
		}, "default_priority"},
		{"promote", http.MethodPost, "/api/event-handlers/" + id + "/promote", map[string]any{
			"blueprint_id": "p-every-422", "breaker_threshold": -1, "scope_predicate": predicate,
		}, "breaker_threshold"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
			items := decodeErrorItems(t, rec)
			got := map[string]string{}
			for _, item := range items {
				got[item.Field] = item.Reason
			}
			if len(items) != 3 || got[tc.tuning] != "OUT_OF_RANGE" || got["scope_predicate.author_id_in"] != "INVALID_FIELD" ||
				items[2].Field != "scope_predicate.has_label" || items[2].Reason != "INVALID_FIELD" {
				t.Errorf("items = %+v, want %s, the repeated author_id_in entry, and the check's has_label fault last", items, tc.tuning)
			}
			if last := (*calls)[len(*calls)-1]; last.predicate != `{"author_id_in":["583231"],"has_label":"forbidden"}` {
				t.Errorf("the check read %s, want the predicate without its repeated entry", last.predicate)
			}
		})
	}

	if n := len(listEventHandlers(t, s, "trigger")); n != 0 {
		t.Errorf("refused writes left %d triggers", n)
	}
	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	if !strings.Contains(rec.Body.String(), `"scope_predicate_json":"{\"has_label\":\"ok\"}"`) {
		t.Errorf("rule after refused writes: %s", rec.Body.String())
	}
}

// A blueprint another team owns is refused with the body's other 422s, on
// both doors into trigger-hood.
func TestEventHandlerPredicateFaults_CrossTeamBlueprintJoinsThe422s(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	other := seedTeam(t, s, runmode.LocalDefaultOrgID, "other")
	seedBlueprintForTrigger(t, s, "p-other-team")
	// The local store pins every blueprint it creates to the default team.
	if _, err := s.db.ExecContext(t.Context(), `UPDATE blueprints SET team_id = ? WHERE id = ?`, other, "p-other-team"); err != nil {
		t.Fatalf("move blueprint to the other team: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type": "github:pr:opened", "name": "to promote", "team_id": runmode.LocalDefaultTeamID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for route, write := range map[string]func() *httptest.ResponseRecorder{
		"create trigger": func() *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
				"event_type": "github:pr:ci_check_failed", "blueprint_id": "p-other-team", "breaker_threshold": -1,
				"team_id": runmode.LocalDefaultTeamID,
			})
		},
		"promote": func() *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPost, "/api/event-handlers/"+created["id"].(string)+"/promote", map[string]any{
				"blueprint_id": "p-other-team", "breaker_threshold": -1,
			})
		},
	} {
		rec := write()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", route, rec.Code, rec.Body.String())
			continue
		}
		items := decodeErrorItems(t, rec)
		got := map[string]string{}
		for _, item := range items {
			got[item.Field] = item.Reason
		}
		if len(items) != 2 || got["breaker_threshold"] != "OUT_OF_RANGE" || got["blueprint_id"] != "CROSS_TEAM_REF" {
			t.Errorf("%s: items = %+v, want breaker_threshold and blueprint_id", route, items)
		}
	}
}

// A list the row stores with a repeated entry does not block an edit that
// re-sends it unchanged, as the trigger panel does on every save; an edit
// that changes the list is told to fix it.
func TestEventHandlerPredicateFaults_StoredListKeptByAnEdit(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createUserRule(t, s)
	const stored = `{"author_id_in":["123","123"]}`
	if _, err := s.db.ExecContext(t.Context(), `UPDATE event_handlers SET scope_predicate_json = ? WHERE id = ?`, stored, id); err != nil {
		t.Fatalf("store a repeated entry: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"default_priority": 0.7, "scope_predicate": map[string]any{"author_id_in": []string{"123", "123"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH re-sending the stored list = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["scope_predicate_json"] != stored {
		t.Errorf("predicate = %v, want the stored one kept", got["scope_predicate_json"])
	}

	rec = doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"author_id_in": []string{"123", "123", "5"}},
	})
	items := decodeErrorItems(t, rec)
	if rec.Code != http.StatusUnprocessableEntity || len(items) != 1 || items[0].Field != "scope_predicate.author_id_in" {
		t.Errorf("PATCH changing the list = %d %+v, want one 422 on scope_predicate.author_id_in", rec.Code, items)
	}
}
