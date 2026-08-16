package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// testGateFeature is the synthetic feature these tests gate the "jira" event
// source behind — no real gated event source exists yet (TFAC-524 is the
// generic mechanism; TFAC-509/510 will call GateEventSource("slack", ...)).

const testGateFeature = entitlements.Feature("test-gate")

// gateJiraForTest gates the jira event source behind testGateFeature and
// registers Reset as a cleanup, so gating never leaks into a sibling test.
func gateJiraForTest(t *testing.T) {
	t.Helper()
	entitlements.GateEventSource("jira", testGateFeature)
	t.Cleanup(entitlements.Reset)
}

func grantTestGateFeature(t *testing.T) {
	t.Helper()
	entitlements.RegisterProvider(entitlements.Static(testGateFeature))
}

// --- GET /api/event-types (Part 2) ------------------------------------

func TestHandleEventTypes_GatedSourceHiddenWithoutFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawGithub bool
	for _, et := range got {
		id, _ := et["id"].(string)
		if strings.HasPrefix(id, "jira:") {
			t.Errorf("gated jira event type %q present in /api/event-types without the feature", id)
		}
		if strings.HasPrefix(id, "github:") {
			sawGithub = true
		}
	}
	if !sawGithub {
		t.Fatal("expected ungated github event types to remain present")
	}
}

func TestHandleEventTypes_GatedSourceVisibleWithFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)
	grantTestGateFeature(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	var sawJira bool
	for _, et := range got {
		if et["id"] == domain.EventJiraIssueAssigned {
			sawJira = true
		}
	}
	if !sawJira {
		t.Fatal("expected jira:issue:assigned present once the org holds the gating feature")
	}
}

// --- GET /api/event-schemas (Part 3) -----------------------------------

func TestHandleEventSchemasList_GatedSourceHiddenWithoutFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-schemas", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if _, ok := got[domain.EventJiraIssueAssigned]; ok {
		t.Error("gated jira:issue:assigned schema present without the feature")
	}
	if _, ok := got[domain.EventGitHubPROpened]; !ok {
		t.Error("expected ungated github:pr:opened schema to remain present")
	}
}

func TestHandleEventSchemasList_GatedSourceVisibleWithFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)
	grantTestGateFeature(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-schemas", nil)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if _, ok := got[domain.EventJiraIssueAssigned]; !ok {
		t.Error("expected jira:issue:assigned schema present once the org holds the gating feature")
	}
}

func TestHandleEventSchemaGet_GatedSource404sWithoutFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-schemas/"+domain.EventJiraIssueAssigned, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a gated-off event type, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventSchemaGet_GatedSourceOKWithFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)
	grantTestGateFeature(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-schemas/"+domain.EventJiraIssueAssigned, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 once entitled, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- GET/POST /api/event-handlers (Part 4, team-scoped) -----------------

func TestHandleEventHandlersList_HidesGatedHandler(t *testing.T) {
	s := newTestServer(t)
	// Create both handlers BEFORE gating (create isn't reachable for the
	// jira one once gated) — rows persist, only visibility is filtered.
	jiraID := createUserRuleFor(t, s, domain.EventJiraIssueAssigned)
	githubID := createUserRuleFor(t, s, domain.EventGitHubPROpened)

	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind=rule", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	var sawJira, sawGithub bool
	for _, h := range got {
		if h["id"] == jiraID {
			sawJira = true
		}
		if h["id"] == githubID {
			sawGithub = true
		}
	}
	if sawJira {
		t.Error("jira-typed handler visible in list despite gated source")
	}
	if !sawGithub {
		t.Error("github-typed handler unexpectedly hidden — gating jira must not affect github")
	}
}

func TestHandleEventHandlersList_GatedHandlerVisibleWithFeature(t *testing.T) {
	s := newTestServer(t)
	jiraID := createUserRuleFor(t, s, domain.EventJiraIssueAssigned)
	gateJiraForTest(t)
	grantTestGateFeature(t)

	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind=rule", nil)
	var got []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	var sawJira bool
	for _, h := range got {
		if h["id"] == jiraID {
			sawJira = true
		}
	}
	if !sawJira {
		t.Error("jira-typed handler should reappear once the org holds the gating feature")
	}
}

func TestHandleEventHandlerCreate_RejectsGatedEventType(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       domain.EventJiraIssueAssigned,
		"name":             "gated rule",
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	got := firstErrorMessage(t, rec)
	if got != "event source not enabled for this organization" {
		t.Errorf("error message = %q", got)
	}
}

func TestHandleEventHandlerCreate_UngatedEventTypeUnaffected(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       domain.EventGitHubPROpened,
		"name":             "github rule",
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an ungated github event type, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventHandlerCreate_AllowsGatedEventTypeWithFeature(t *testing.T) {
	s := newTestServer(t)
	gateJiraForTest(t)
	grantTestGateFeature(t)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       domain.EventJiraIssueAssigned,
		"name":             "now allowed",
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 once entitled, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- filterEventHandlersByGate (Part 4) ---

func TestFilterEventHandlersByGate_DropsGatedKeepsUngated(t *testing.T) {
	gateJiraForTest(t)

	handlers := []domain.EventHandler{
		{ID: "jira-1", EventType: domain.EventJiraIssueAssigned},
		{ID: "gh-1", EventType: domain.EventGitHubPROpened},
	}
	got := filterEventHandlersByGate("any-org", handlers)
	if len(got) != 1 || got[0].ID != "gh-1" {
		t.Fatalf("filterEventHandlersByGate = %v, want only gh-1 (jira gated off)", got)
	}
}

func TestFilterEventHandlersByGate_GrantedFeatureKeepsBoth(t *testing.T) {
	gateJiraForTest(t)
	grantTestGateFeature(t)

	handlers := []domain.EventHandler{
		{ID: "jira-1", EventType: domain.EventJiraIssueAssigned},
		{ID: "gh-1", EventType: domain.EventGitHubPROpened},
	}
	got := filterEventHandlersByGate("any-org", handlers)
	if len(got) != 2 {
		t.Fatalf("filterEventHandlersByGate = %v, want both handlers once entitled", got)
	}
}

func TestFilterEventHandlersByGate_EmptyInputReturnsNonNil(t *testing.T) {
	got := filterEventHandlersByGate("any-org", nil)
	if got == nil {
		t.Fatal("filterEventHandlersByGate(nil) returned nil, want a non-nil empty slice (serializes as [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("filterEventHandlersByGate(nil) = %v, want empty", got)
	}
}

// createUserRuleFor creates a user-source rule bound to eventType and returns
// its id.
func createUserRuleFor(t *testing.T, s *Server, eventType string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       eventType,
		"name":             "rule for " + eventType,
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed rule for %s: %d: %s", eventType, rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	id, ok := got["id"].(string)
	if !ok || id == "" {
		t.Fatalf("seed rule for %s: missing id in response: %s", eventType, rec.Body.String())
	}
	return id
}
