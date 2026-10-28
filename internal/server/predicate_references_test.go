package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// referenceCheckEventType is the event type the tests below hang a stub check
// on. Its predicate's has_label field stands in for a reference: the stub
// refuses the label "forbidden".
const referenceCheckEventType = "github:pr:opened"

// checkCall is one invocation of the stub check.
type checkCall struct {
	teamID           string
	predicate, prior string
}

// stubPredicateReferenceCheck installs a check for referenceCheckEventType
// that refuses a predicate naming "forbidden", returning the calls it
// receives. Whatever was registered before is restored on cleanup.
func stubPredicateReferenceCheck(t *testing.T) *[]checkCall {
	t.Helper()
	calls := &[]checkCall{}
	predicateReferenceMu.Lock()
	prev, had := predicateReferenceChecks[referenceCheckEventType]
	predicateReferenceChecks[referenceCheckEventType] = func(_ context.Context, _ db.TxStores, _, teamID string, predicate, prior *string) ([]PredicateReferenceFault, error) {
		*calls = append(*calls, checkCall{teamID: teamID, predicate: orNil(predicate), prior: orNil(prior)})
		if predicate != nil && strings.Contains(*predicate, "forbidden") {
			return []PredicateReferenceFault{{Key: "has_label", Message: `has_label: "forbidden" is refused`}}, nil
		}
		return nil, nil
	}
	predicateReferenceMu.Unlock()
	t.Cleanup(func() {
		predicateReferenceMu.Lock()
		defer predicateReferenceMu.Unlock()
		if had {
			predicateReferenceChecks[referenceCheckEventType] = prev
		} else {
			delete(predicateReferenceChecks, referenceCheckEventType)
		}
	})
	return calls
}

func orNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// labelRule creates a rule on referenceCheckEventType scoped to label.
func labelRule(t *testing.T, s *Server, label string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      referenceCheckEventType,
		"name":            "labelled",
		"scope_predicate": map[string]any{"has_label": label},
	})
}

// wantReferenceFault fails the test unless rec is the stub's one fault: 422
// INVALID_FIELD on the key it names, scope_predicate.has_label.
func wantReferenceFault(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Errors []struct {
			Reason, Message, Field string
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(body.Errors) != 1 {
		t.Fatalf("errors = %+v, want the check's one fault", body.Errors)
	}
	got := body.Errors[0]
	if got.Reason != "INVALID_FIELD" || got.Field != "scope_predicate.has_label" || got.Message != `has_label: "forbidden" is refused` {
		t.Errorf("fault = %+v, want the check's message as INVALID_FIELD on scope_predicate.has_label", got)
	}
}

func TestPredicateReferenceCheck_CreateRule(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	calls := stubPredicateReferenceCheck(t)
	before := len(listEventHandlers(t, s, "rule"))

	wantReferenceFault(t, labelRule(t, s, "forbidden"))
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}

	if rec := labelRule(t, s, "ok"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// The check sees the acting team the write resolved, the canonical
	// predicate, and no prior on create.
	want := checkCall{teamID: runmode.LocalDefaultTeamID, predicate: `{"has_label":"ok"}`, prior: "<nil>"}
	if len(*calls) != 2 || (*calls)[1] != want {
		t.Errorf("calls = %+v, want the second to be %+v", *calls, want)
	}
}

func TestPredicateReferenceCheck_CreateTrigger(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	stubPredicateReferenceCheck(t)
	seedBlueprintForTrigger(t, s, "p-reference-check")

	wantReferenceFault(t, doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":      referenceCheckEventType,
		"blueprint_id":    "p-reference-check",
		"scope_predicate": map[string]any{"has_label": "forbidden"},
	}))
	if n := len(listEventHandlers(t, s, "trigger")); n != 0 {
		t.Errorf("a refused create left %d triggers", n)
	}
}

// PATCH hands the check the stored predicate as prior, which is what lets a
// check skip entries the row already carries.
func TestPredicateReferenceCheck_Patch(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	calls := stubPredicateReferenceCheck(t)
	rec := labelRule(t, s, "ok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	wantReferenceFault(t, doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"has_label": "forbidden"},
	}))
	want := checkCall{teamID: runmode.LocalDefaultTeamID, predicate: `{"has_label":"forbidden"}`, prior: `{"has_label":"ok"}`}
	if got := (*calls)[len(*calls)-1]; got != want {
		t.Errorf("call = %+v, want %+v", got, want)
	}

	got := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	var stored map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &stored)
	if stored["scope_predicate_json"] != `{"has_label":"ok"}` {
		t.Errorf("stored predicate = %v, want the refused PATCH not to land", stored["scope_predicate_json"])
	}
}

func TestPredicateReferenceCheck_Promote(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	calls := stubPredicateReferenceCheck(t)
	seedBlueprintForTrigger(t, s, "p-reference-promote")
	rec := labelRule(t, s, "ok")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	wantReferenceFault(t, doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id":    "p-reference-promote",
		"scope_predicate": map[string]any{"has_label": "forbidden"},
	}))
	want := checkCall{teamID: runmode.LocalDefaultTeamID, predicate: `{"has_label":"forbidden"}`, prior: `{"has_label":"ok"}`}
	if got := (*calls)[len(*calls)-1]; got != want {
		t.Errorf("call = %+v, want %+v", got, want)
	}
	if n := len(listEventHandlers(t, s, "trigger")); n != 0 {
		t.Errorf("a refused promote left %d triggers", n)
	}
}
