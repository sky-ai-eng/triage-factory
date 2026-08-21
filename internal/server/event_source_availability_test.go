package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// errorItem is one element of the standard error envelope, decoded whole:
// these tests are about the reason and the field, not only the status.
type errorItem struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

func firstError(t *testing.T, rec *httptest.ResponseRecorder) errorItem {
	t.Helper()
	var body struct {
		Errors []errorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("empty errors list; body=%s", rec.Body.String())
	}
	return body.Errors[0]
}

// TestEventHandlerCreate_UnconfiguredSourceRejected: an org with no Jira
// credential cannot author a jira rule or trigger. Create is the only
// enforcement point there will ever be — event_type is immutable afterwards,
// and the router matches on exact equality, so a handler authored here would
// wait forever for an event that cannot be produced.
func TestEventHandlerCreate_UnconfiguredSourceRejected(t *testing.T) {
	cases := map[string]struct {
		path string
		body map[string]any
	}{
		"rule": {"/api/event-handlers/rules", map[string]any{
			"event_type":       domain.EventJiraIssueAssigned,
			"name":             "jira rule",
			"default_priority": 0.5,
		}},
		"trigger": {"/api/event-handlers/triggers", map[string]any{
			"event_type":   domain.EventJiraIssueAssigned,
			"blueprint_id": fixtureUUID("bp_jira"),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			unconfigureEventSources(t)
			seedBlueprintForTrigger(t, s, fixtureUUID("bp_jira"))

			rec := doJSON(t, s, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
			got := firstError(t, rec)
			if got.Reason != "INVALID_FIELD" {
				t.Errorf("reason = %q, want INVALID_FIELD", got.Reason)
			}
			if got.Field != "event_type" {
				t.Errorf("field = %q, want event_type", got.Field)
			}
			// The message has to name the source and what is missing — the
			// caller's next move is to go bind that credential.
			if !strings.Contains(got.Message, "jira") || !strings.Contains(got.Message, "not configured") {
				t.Errorf("message = %q, want it to name jira and what is missing", got.Message)
			}
		})
	}
}

// TestEventHandlerCreate_AvailableSourceAccepted is the other half: the gate
// refuses the source, never the event type. With the credential bound, the
// same body is authored.
func TestEventHandlerCreate_AvailableSourceAccepted(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":       domain.EventJiraIssueAssigned,
		"name":             "jira rule",
		"default_priority": 0.5,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestEventHandlerCreate_UnlicensedKeepsItsOwn403 pins the split between the
// two refusals. A licence the org does not hold is a permission fault and
// stays 403 FORBIDDEN, unchanged; a source the org has not set up is a
// semantically invalid reference and is 422 INVALID_FIELD. Both are asserted
// in one place because it is their DIFFERENCE that is the contract.
func TestEventHandlerCreate_UnlicensedKeepsItsOwn403(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s) // jira IS configured — only the licence is missing
	gateJiraForTest(t)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":       domain.EventJiraIssueAssigned,
		"name":             "jira rule",
		"default_priority": 0.5,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := firstError(t, rec).Reason; got != "FORBIDDEN" {
		t.Errorf("reason = %q, want FORBIDDEN", got)
	}
}

// TestEventHandlerList_SurfacesAHandlerWhoseSourceWentAway pins D5. Unbinding
// a credential does not delete, disable or hide the handlers bound to it —
// forward-only, like every other tracking change. They stay listed, stay
// counted, and carry the derived flag the UI renders them inert from.
func TestEventHandlerList_SurfacesAHandlerWhoseSourceWentAway(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)

	ghRule := createUserRuleFor(t, s, domain.EventGitHubPROpened)
	jiraRule := createUserRuleFor(t, s, domain.EventJiraIssueAssigned)

	before := listRowsByID(t, s)
	if !before[ghRule].SourceAvailable || !before[jiraRule].SourceAvailable {
		t.Fatalf("both sources are bound; source_available = github:%v jira:%v",
			before[ghRule].SourceAvailable, before[jiraRule].SourceAvailable)
	}
	beforeTotal := len(before)

	if err := integrations.ClearJira(t.Context(), s.secrets, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("clear jira credential: %v", err)
	}

	after := listRowsByID(t, s)
	if _, listed := after[jiraRule]; !listed {
		t.Fatal("the jira handler vanished from the list; an unconfigured source is explained, not hidden")
	}
	if after[jiraRule].SourceAvailable {
		t.Error("jira handler still reports source_available after the credential was unbound")
	}
	if !after[ghRule].SourceAvailable {
		t.Error("github handler lost source_available; unbinding jira must not touch it")
	}
	if len(after) != beforeTotal {
		t.Errorf("list held %d rows, want the same %d — a handler that can no longer fire is still counted", len(after), beforeTotal)
	}
}

// TestEventHandlerGet_CarriesTheSameRowShape: the single read is the list's
// row shape, so it answers the derived flag too — a handler that is editable
// must not be readable in a shape the list never produces.
func TestEventHandlerGet_CarriesTheSameRowShape(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createUserRuleFor(t, s, domain.EventJiraIssueAssigned)

	if err := integrations.ClearJira(t.Context(), s.secrets, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("clear jira credential: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if avail, present := got["source_available"]; !present || avail != false {
		t.Errorf("source_available = %v (present=%v), want false", avail, present)
	}
}

// eventHandlerListRow is the list's row as a client sees it.
type eventHandlerListRow struct {
	ID              string `json:"id"`
	EventType       string `json:"event_type"`
	SourceAvailable bool   `json:"source_available"`
}

func listRowsByID(t *testing.T, s *Server) map[string]eventHandlerListRow {
	t.Helper()
	page := decodeList[eventHandlerListRow](t, doJSON(t, s, http.MethodPost, "/api/event-handlers/list", map[string]any{"page_size": 200}))
	out := make(map[string]eventHandlerListRow, len(page.Items))
	for _, row := range page.Items {
		out[row.ID] = row
	}
	if page.Total() != len(out) {
		t.Fatalf("total = %d but the page held %d rows", page.Total(), len(out))
	}
	return out
}
