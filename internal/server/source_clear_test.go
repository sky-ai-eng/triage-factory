package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// jiraSourceCondition is a condition Jira owns, for the routes that clear the
// source's conditions: core defines no Jira kind yet.
var jiraSourceCondition = notify.Condition(notify.ConditionSpec[notificationTestParams]{
	Name: "server_test.jira_source", Source: "jira", Priority: notify.Always(notify.High), Dismissible: true,
	Key: notificationTestKey, Audience: notify.OrgAdmins(),
	Render: func(p notificationTestParams, _ notify.Recipient) notify.Content {
		return notify.Content{Title: "jira " + p.ID}
	},
	Example: notificationTestParams{ID: "x"},
})

// raiseSourceConditions opens one Linear and one Jira condition in the test org.
func raiseSourceConditions(t *testing.T) *notifytest.Recorder {
	t.Helper()
	rec := notifytest.Record(t)
	ctx := context.Background()
	kinds.LinearTeamUnreadable.Sync(ctx, runmode.LocalDefaultOrgID, []kinds.LinearTeam{{LinearTeamID: "eng", TeamKey: "ENG"}})
	jiraSourceCondition.Set(ctx, runmode.LocalDefaultOrgID, notificationTestParams{ID: "j"}, true)
	return rec
}

func mustStatus(t *testing.T, s *Server, method, path string, body any) {
	t.Helper()
	if rec := doJSON(t, s, method, path, body); rec.Code != http.StatusOK {
		t.Fatalf("%s %s = %d: %s", method, path, rec.Code, rec.Body.String())
	}
}

// TestSourceOff_ClearsItsConditions: turning a source off resolves its open
// conditions and no other source's; turning it back on resolves nothing.
func TestSourceOff_ClearsItsConditions(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := raiseSourceConditions(t)

	mustStatus(t, s, http.MethodPatch, orgSourcePath("linear"), map[string]any{"disabled": false})
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable, "eng")

	mustStatus(t, s, http.MethodPatch, orgSourcePath("linear"), map[string]any{"disabled": true})
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable)
	rec.Active(t, runmode.LocalDefaultOrgID, jiraSourceCondition, "j")

	mustStatus(t, s, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true})
	rec.Inactive(t, runmode.LocalDefaultOrgID, jiraSourceCondition)
}

// TestCredentialDelete_ClearsItsSourceConditions: unbinding the org's Linear
// or Jira credential resolves that source's open conditions.
func TestCredentialDelete_ClearsItsSourceConditions(t *testing.T) {
	r := newLinearAccessRig(t)
	configureEventSources(t, r.s)
	r.bind(t, "lin_api_ada")
	rec := raiseSourceConditions(t)

	mustStatus(t, r.s, http.MethodDelete, linearCredentialPath(), nil)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable)
	rec.Active(t, runmode.LocalDefaultOrgID, jiraSourceCondition, "j")

	mustStatus(t, r.s, http.MethodDelete, jiraCredentialRoute(), nil)
	rec.Inactive(t, runmode.LocalDefaultOrgID, jiraSourceCondition)
}

// TestAppDelete_ClearsOnlyWithoutACredential: deleting the org's own OAuth
// app does not disconnect a source whose service credential is a key, so its
// conditions stay open; with no credential left for the source, they resolve.
func TestAppDelete_ClearsOnlyWithoutACredential(t *testing.T) {
	importApps := func(t *testing.T, s *Server) {
		t.Helper()
		mustStatus(t, s, http.MethodPost, linearAppPath(), map[string]any{"client_id": "lin-client", "client_secret": "lin-secret"})
		mustStatus(t, s, http.MethodPost, jiraAppPath(), map[string]any{"client_id": "atl-client", "client_secret": "atl-secret"})
	}

	t.Run("credential bound", func(t *testing.T) {
		r := newLinearAccessRig(t)
		configureEventSources(t, r.s)
		r.bind(t, "lin_api_ada")
		importApps(t, r.s)
		rec := raiseSourceConditions(t)

		mustStatus(t, r.s, http.MethodDelete, linearAppPath(), nil)
		mustStatus(t, r.s, http.MethodDelete, jiraAppPath(), nil)
		rec.Active(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable, "eng")
		rec.Active(t, runmode.LocalDefaultOrgID, jiraSourceCondition, "j")
	})

	t.Run("no credential", func(t *testing.T) {
		r := newLinearAccessRig(t)
		importApps(t, r.s)
		rec := raiseSourceConditions(t)

		mustStatus(t, r.s, http.MethodDelete, linearAppPath(), nil)
		mustStatus(t, r.s, http.MethodDelete, jiraAppPath(), nil)
		rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable)
		rec.Inactive(t, runmode.LocalDefaultOrgID, jiraSourceCondition)
	})

	// An app delete that finds no app removes nothing, so it clears nothing.
	t.Run("no app", func(t *testing.T) {
		r := newLinearAccessRig(t)
		rec := raiseSourceConditions(t)
		mustStatus(t, r.s, http.MethodDelete, linearAppPath(), nil)
		mustStatus(t, r.s, http.MethodDelete, jiraAppPath(), nil)
		rec.Active(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable, "eng")
		rec.Active(t, runmode.LocalDefaultOrgID, jiraSourceCondition, "j")
	})
}
