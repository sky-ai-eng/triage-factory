package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// mapLocalGitHubTeam maps the local team to one GitHub team, by GitHub's ids.
func mapLocalGitHubTeam(t *testing.T, s *Server, githubTeamID, orgLogin, slug string) {
	t.Helper()
	if err := s.allStores.TeamGitHubGroups.SetForTeam(t.Context(), runmode.LocalDefaultTeamID, seedRepoHost(t, s),
		[]domain.TeamGitHubGroup{{GitHubTeamID: githubTeamID, GitHubOrgID: "7", OrgLogin: orgLogin, TeamSlug: slug}}); err != nil {
		t.Fatalf("map github team: %v", err)
	}
}

// createIdentityRule creates a rule on eventType with predicate and returns
// the status and decoded body.
func createIdentityRule(t *testing.T, s *Server, eventType string, predicate map[string]any) (int, map[string]any) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      eventType,
		"name":            "identity",
		"scope_predicate": predicate,
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

func TestEventHandlerGitHubIdentities_CreateAcceptsIDs(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	mapLocalGitHubTeam(t, s, "11", "acme", "backend")

	code, got := createIdentityRule(t, s, domain.EventGitHubPRReviewRequested, map[string]any{
		"author_id_in":         []string{"583231"},
		"requested_user_id_in": []string{"5"},
		"requested_team_id_in": []string{"11"},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := `{"author_id_in":["583231"],"requested_user_id_in":["5"],"requested_team_id_in":["11"]}`
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}

	for eventType, field := range map[string]string{
		domain.EventGitHubPRReviewApproved: "reviewer_id_in",
		domain.EventGitHubPRCommented:      "commenter_id_in",
		domain.EventGitHubPRMentioned:      "commenter_id_in",
	} {
		if code, got := createIdentityRule(t, s, eventType, map[string]any{field: []string{"49699333"}}); code != http.StatusCreated {
			t.Errorf("%s %s: status = %d, want 201: %v", eventType, field, code, got)
		}
	}
}

// Every entry that cannot match is reported, and nothing is written.
func TestEventHandlerGitHubIdentities_CreateRefusesWhatCannotMatch(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	mapLocalGitHubTeam(t, s, "11", "acme", "backend")
	before := len(listEventHandlers(t, s, "rule"))

	code, got := createIdentityRule(t, s, domain.EventGitHubPRReviewRequested, map[string]any{
		"author_id_in":         []string{"aidan", "583231"},
		"requested_user_id_in": []string{"0"},
		"requested_team_id_in": []string{"acme/backend", "99", "11"},
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 4 {
		t.Fatalf("faults = %q, want one each for the login, the zero id, the handle and the unmapped team", messages)
	}
	for i, want := range []string{`author_id_in: "aidan"`, `requested_user_id_in: "0"`, `requested_team_id_in: "acme/backend"`, `requested_team_id_in: "99"`} {
		if !strings.Contains(messages[i], want) {
			t.Errorf("fault %d = %q, want it to name %s", i, messages[i], want)
		}
	}
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}

	code, got = createIdentityRule(t, s, domain.EventGitHubPRReviewApproved, map[string]any{"reviewer_id_in": []string{"alice"}})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 1 || !strings.Contains(messages[0], "reviewer_id_in") {
		t.Errorf("reviewer login: faults = %q, want one naming reviewer_id_in", messages)
	}
}

// An entry the stored predicate already holds is not re-checked, so a team
// mapping removed since never blocks an edit that keeps it; an added entry is.
func TestEventHandlerGitHubIdentities_PatchChecksOnlyAddedEntries(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	mapLocalGitHubTeam(t, s, "11", "acme", "backend")
	code, created := createIdentityRule(t, s, domain.EventGitHubPRReviewRequested, map[string]any{"requested_team_id_in": []string{"11"}})
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	mapLocalGitHubTeam(t, s, "12", "acme", "frontend")

	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"requested_team_id_in": []string{"11", "12"}, "author_id_in": []string{"583231"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH keeping an unmapped team and adding a mapped one: %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"requested_team_id_in": []string{"11", "12"}, "author_id_in": []string{"583231", "aidan"}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 || !strings.Contains(messages[0], `"aidan"`) {
		t.Errorf("faults = %q, want one naming the added login", messages)
	}
}

func TestEventHandlerGitHubIdentities_RefusesLoginFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	for _, tc := range []struct {
		eventType string
		predicate map[string]any
	}{
		{domain.EventGitHubPRCICheckFailed, map[string]any{"author_in": []string{"aidan"}}},
		{domain.EventGitHubPRCICheckFailed, map[string]any{"author": "aidan"}},
		{domain.EventGitHubPRReviewApproved, map[string]any{"reviewer_in": []string{"alice"}}},
		{domain.EventGitHubPRReviewRequested, map[string]any{"requested_login": "alice"}},
		{domain.EventGitHubPRReviewRequested, map[string]any{"requested_team": "acme/backend"}},
		{domain.EventGitHubPRCommented, map[string]any{"commenter_in": []string{"alice"}}},
	} {
		if code, got := createIdentityRule(t, s, tc.eventType, tc.predicate); code != http.StatusBadRequest {
			t.Errorf("%s %v: status = %d, want 400: %v", tc.eventType, tc.predicate, code, got)
		}
	}
}

func TestGitHubAccountReads(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	base := "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/users/"

	// An org with no GitHub credential is told GitHub is not connected, not
	// that the account does not exist.
	assertOneFault(t, doJSON(t, s, http.MethodGet, base+"by-name/octocat", nil), http.StatusConflict, "NOT_CONFIGURED", "")

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v3/users/octocat", "/api/v3/user/583231":
			_, _ = w.Write([]byte(`{"id":583231,"login":"octocat"}`))
		case "/api/v3/users/dependabot%5Bbot%5D":
			_, _ = w.Write([]byte(`{"id":49699333,"login":"dependabot[bot]"}`))
		case "/api/v3/users/broken":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	s.ghResolver = repositoriesByIDResolver{url: gh.URL}

	for _, tc := range []struct {
		path   string
		status int
		want   string // "id login" on a 200, else the error's reason
	}{
		{"by-name/octocat", http.StatusOK, "583231 octocat"},
		{"by-name/dependabot%5Bbot%5D", http.StatusOK, "49699333 dependabot[bot]"},
		{"583231", http.StatusOK, "583231 octocat"},
		{"by-name/nobody", http.StatusNotFound, "NOT_FOUND"},
		{"42", http.StatusNotFound, "NOT_FOUND"},
		{"by-name/not%20a%20login", http.StatusBadRequest, "INVALID_ID"},
		{"octocat", http.StatusBadRequest, "INVALID_ID"},
		{"0", http.StatusBadRequest, "INVALID_ID"},
		{"by-name/broken", http.StatusBadGateway, "UPSTREAM_UNAVAILABLE"},
	} {
		rec := doJSON(t, s, http.MethodGet, base+tc.path, nil)
		var got string
		if rec.Code == http.StatusOK {
			var account gitHubAccountJSON
			_ = json.Unmarshal(rec.Body.Bytes(), &account)
			got = account.GitHubUserID + " " + account.Login
		} else {
			got = decodeErrorItems(t, rec)[0].Reason
		}
		if rec.Code != tc.status || got != tc.want {
			t.Errorf("%s = %d %q, want %d %q: %s", tc.path, rec.Code, got, tc.status, tc.want, rec.Body.String())
		}
	}
}
