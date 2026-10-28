package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
