package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The team roster answers in local mode too, at the "default" team alias, which
// is the address a local caller has.

// listLocalRoster reads the default team's roster through the mux.
func listLocalRoster(t *testing.T, s *Server) teamRosterListResponse {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/teams/default/members/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// TestTeamRosterList_LocalSingleMember: N=1 answers with the one implicit member
// as admin, with null identity fields until GitHub is bound and the bound ones
// after, and the bootstrapped bot beside the page, never counted as a member.
func TestTeamRosterList_LocalSingleMember(t *testing.T) {
	s := newTestServer(t)

	// Before any identity is captured the row is still there, with nulls, so
	// the predicate editor renders rather than erroring.
	if before := listLocalRoster(t, s); len(before.Items) != 1 ||
		before.Items[0].GitHubUsername != nil || before.Items[0].GitHubUserID != nil {
		t.Fatalf("roster before an identity = %+v, want one row with null github_username / github_user_id", before.Items)
	}

	// An unset github_base_url resolves to the deployment's default GitHub,
	// the host the capture paths write the identity under.
	if err := s.users.UpsertGitHubIdentity(t.Context(), runmode.LocalDefaultUserID,
		ghbase.DefaultBaseURL(), "AidanAllchin", "583231", "", "pat"); err != nil {
		t.Fatalf("seed github identity: %v", err)
	}

	resp := listLocalRoster(t, s)
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1 (the single implicit user)", len(resp.Items))
	}
	if resp.TotalCount != 1 {
		t.Errorf("total_count = %d, want 1 — the members, never the bot", resp.TotalCount)
	}
	m := resp.Items[0]
	if m.UserID != runmode.LocalDefaultUserID {
		t.Errorf("user_id = %q, want %q", m.UserID, runmode.LocalDefaultUserID)
	}
	if !m.IsCurrentUser {
		t.Error("is_current_user = false; the local user is always the caller")
	}
	if m.Role != "admin" {
		t.Errorf("role = %q, want admin", m.Role)
	}
	if m.GitHubUsername == nil || *m.GitHubUsername != "AidanAllchin" {
		t.Errorf("github_username = %v, want AidanAllchin", m.GitHubUsername)
	}
	if m.GitHubUserID == nil || *m.GitHubUserID != "583231" {
		t.Errorf("github_user_id = %v, want 583231", m.GitHubUserID)
	}
	// newTestServer bootstraps a default-enabled agent + team_agents row, so
	// the bot rides beside the page with its roleful fields filled in.
	if resp.Bot == nil {
		t.Fatal("bot = nil; the test server bootstraps a default-enabled agent")
	}
	if resp.Bot.AgentID == "" {
		t.Error("bot.agent_id is empty")
	}
	if !resp.Bot.Enabled {
		t.Error("bot.enabled = false, want true (team_agents row is enabled)")
	}
}

// TestHandleMe_LocalMode_GitHubAccountID: local /api/me carries GitHub's id
// for the local user's bound account beside its login, the value the predicate
// editor writes for "match me".
func TestHandleMe_LocalMode_GitHubAccountID(t *testing.T) {
	s := newTestServer(t)
	if err := s.users.UpsertGitHubIdentity(t.Context(), runmode.LocalDefaultUserID,
		ghbase.DefaultBaseURL(), "AidanAllchin", "583231", "", "pat"); err != nil {
		t.Fatalf("seed github identity: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/me", nil)
	var me struct {
		GitHubUsername string `json:"github_username"`
		GitHubUserID   string `json:"github_user_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("/api/me = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if me.GitHubUsername != "AidanAllchin" || me.GitHubUserID != "583231" {
		t.Errorf("/api/me github = (%q, %q), want (AidanAllchin, 583231)", me.GitHubUsername, me.GitHubUserID)
	}
}

// TestTeamRosterList_RejectsBadPageSize: an out-of-range page_size is a 400, not
// a silently clamped window.
func TestTeamRosterList_RejectsBadPageSize(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/teams/default/members/list",
		map[string]any{"page_size": 5000})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (page_size out of range): %s", rec.Code, rec.Body.String())
	}
}

// TestTeamRosterList_RejectsUnknownField: strict decode. A caller that sends a
// filter this route doesn't have learns it, rather than getting the whole
// roster back and believing it was narrowed.
func TestTeamRosterList_RejectsUnknownField(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/teams/default/members/list",
		map[string]any{"role": "admin"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field): %s", rec.Code, rec.Body.String())
	}
}
