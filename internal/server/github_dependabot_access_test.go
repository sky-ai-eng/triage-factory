package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubDependabotAccess_CountsTrackedRepositories: the node counts the
// tracked repositories on the org's host by their recorded answer, a
// repository with none as unchecked.
func TestGitHubDependabotAccess_CountsTrackedRepositories(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	setOrgGitHubBase(t, s, "https://github.com")
	ctx := context.Background()
	stores := sqlitestore.New(s.db)

	readable := trackHintRepo(t, s, "101", "readable")
	refused := trackHintRepo(t, s, "102", "refused")
	off := trackHintRepo(t, s, "103", "off")
	trackHintRepo(t, s, "104", "unchecked")
	for id, access := range map[string]domain.DependabotAlertsAccess{
		readable: domain.DependabotAlertsReadable,
		refused:  domain.DependabotAlertsNoPermission,
		off:      domain.DependabotAlertsDisabled,
	} {
		if _, err := stores.DependabotAccess.RecordSystem(ctx, runmode.LocalDefaultOrgID, id, access); err != nil {
			t.Fatalf("record %s: %v", access, err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/dependabot-access", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got dependabotAccessJSON
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := (dependabotAccessJSON{Readable: 1, NoPermission: 1, Disabled: 1, Unchecked: 1}); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// TestGitHubDependabotAccess_NoGitHubURL: an org with no GitHub URL has
// nothing to count and is told so.
func TestGitHubDependabotAccess_NoGitHubURL(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	setOrgGitHubBase(t, s, "")
	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/dependabot-access", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}
