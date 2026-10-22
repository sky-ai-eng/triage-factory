package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
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

// A login where an account id belongs, an org/slug where a team id belongs,
// and a team id the team does not map are each refused, every one reported,
// and nothing is written.
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
// mapping removed since never blocks an edit that keeps it; an entry the edit
// adds is still checked.
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

// The login-valued fields are gone from the GitHub predicates, so a caller
// still sending one is told so rather than having it dropped.
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

// accountResolver is a ghclient.Resolver whose every client reads the GitHub
// at url, or which fails with err.
type accountResolver struct {
	url string
	err error
}

func (r accountResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	if r.err != nil {
		return nil, r.err
	}
	return ghclient.NewClient(r.url, "pat"), nil
}

func (r accountResolver) ClientForRepo(ctx context.Context, orgID, owner, _ string) (*ghclient.Client, error) {
	return r.ClientFor(ctx, orgID, owner)
}

func (accountResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("no token in this test")
}

func (r accountResolver) BaseURLFor(context.Context, string) (string, error) { return r.url, nil }

func (accountResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// serveAccounts is a GitHub that knows octocat and dependabot[bot], and fails
// every request for "broken".
func serveAccounts(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestGitHubAccountReads(t *testing.T) {
	s := newTestServer(t)
	s.ghResolver = accountResolver{url: serveAccounts(t)}
	base := "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/users/"

	for _, tc := range []struct {
		path       string
		wantStatus int
		want       gitHubAccountJSON
		wantReason string
	}{
		{"by-name/octocat", http.StatusOK, gitHubAccountJSON{GitHubUserID: "583231", Login: "octocat"}, ""},
		{"by-name/dependabot%5Bbot%5D", http.StatusOK, gitHubAccountJSON{GitHubUserID: "49699333", Login: "dependabot[bot]"}, ""},
		{"583231", http.StatusOK, gitHubAccountJSON{GitHubUserID: "583231", Login: "octocat"}, ""},
		{"by-name/nobody", http.StatusNotFound, gitHubAccountJSON{}, "NOT_FOUND"},
		{"42", http.StatusNotFound, gitHubAccountJSON{}, "NOT_FOUND"},
		{"by-name/not%20a%20login", http.StatusBadRequest, gitHubAccountJSON{}, "INVALID_ID"},
		{"octocat", http.StatusBadRequest, gitHubAccountJSON{}, "INVALID_ID"},
		{"0", http.StatusBadRequest, gitHubAccountJSON{}, "INVALID_ID"},
		{"by-name/broken", http.StatusBadGateway, gitHubAccountJSON{}, "UPSTREAM_UNAVAILABLE"},
	} {
		rec := doJSON(t, s, http.MethodGet, base+tc.path, nil)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d: %s", tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			continue
		}
		if tc.wantReason != "" {
			if !strings.Contains(rec.Body.String(), fmt.Sprintf("%q", tc.wantReason)) {
				t.Errorf("%s: body = %s, want reason %s", tc.path, rec.Body.String(), tc.wantReason)
			}
			continue
		}
		var got gitHubAccountJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != tc.want {
			t.Errorf("%s: body = %s, want %+v", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// An org with no GitHub credential is told GitHub is not connected, not that
// the account does not exist.
func TestGitHubAccountReads_NoCredential(t *testing.T) {
	s := newTestServer(t)
	s.ghResolver = accountResolver{err: fmt.Errorf("%w: org has none", ghclient.ErrNoGitHubCredentials)}
	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/users/by-name/octocat", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"NOT_CONFIGURED"`) {
		t.Errorf("status = %d, want 409 NOT_CONFIGURED: %s", rec.Code, rec.Body.String())
	}
}
