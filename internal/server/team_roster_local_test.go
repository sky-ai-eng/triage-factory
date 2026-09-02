package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The team roster answers in local mode too — that is the whole reason there is
// one roster route rather than two. These tests drive it through the mux at the
// "default" team alias, which is the address a local caller has (there is no
// picker and no uuid a user could know).

// TestTeamRosterList_LocalSingleMember: N=1 answers with the one implicit
// member, their captured GitHub identity, role admin, and the bootstrapped bot
// beside the page — no 404, and the bot is never counted as a member.
func TestTeamRosterList_LocalSingleMember(t *testing.T) {
	s := newTestServer(t)
	// Bind the identity on the host the capture paths use: the test server's
	// org_settings leaves github_base_url unset, which resolves to github.com
	// (db.EffectiveGitHubHost) — the same host ghbase.ResolveBaseURL hands the
	// PAT and Connect writers for an unconfigured org.
	if err := s.users.UpsertGitHubIdentity(t.Context(), runmode.LocalDefaultUserID,
		ghbase.DefaultBaseURL(), "AidanAllchin", "", "", "pat"); err != nil {
		t.Fatalf("seed github identity: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/teams/default/members/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
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

// TestTeamRosterList_LocalNoIdentityCaptured: the pre-GitHub-config state. The
// row exists with a null github_username and the read still succeeds, so the
// predicate editor renders (with its Variant-A toggle disabled) rather than
// erroring.
func TestTeamRosterList_LocalNoIdentityCaptured(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/teams/default/members/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].GitHubUsername != nil {
		t.Errorf("github_username = %q, want null (not yet captured)", *resp.Items[0].GitHubUsername)
	}
}

// TestTeamRosterList_RejectsBadPageSize: the paging fields are validated, not
// repaired — an out-of-range page_size is a 400 rather than a silently clamped
// window that answers a different question than the caller asked.
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
