package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConfig_LocalDefaults reports deployment_mode=local. The
// endpoint carries only the deployment-shape signal AuthGate needs to
// pick a login flow; per-user identity lives on /api/me.
func TestHandleConfig_LocalDefaults(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeploymentMode != string(runmode.ModeLocal) {
		t.Errorf("deployment_mode: got %q want %q", resp.DeploymentMode, runmode.ModeLocal)
	}
}

// TestHandleTeamMembers_LocalSingleEntry returns one member (the synthetic
// local user) with is_current_user=true.
func TestHandleTeamMembers_LocalSingleEntry(t *testing.T) {
	s := newTestServer(t)
	// Seed the host-scoped identity at the empty host — the test
	// server's org_settings has no github_base_url configured, so the
	// handler resolves host="" and looks up (user, "").
	if err := s.users.UpsertGitHubIdentity(t.Context(), runmode.LocalDefaultUserID, "", "AidanAllchin", "pat"); err != nil {
		t.Fatalf("seed github identity: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/team/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp teamMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(resp.Members))
	}
	m := resp.Members[0]
	if m.UserID != runmode.LocalDefaultUserID {
		t.Errorf("member.user_id: got %q want %q", m.UserID, runmode.LocalDefaultUserID)
	}
	if !m.IsCurrentUser {
		t.Error("local user should be marked is_current_user")
	}
	if m.GitHubUsername == nil || *m.GitHubUsername != "AidanAllchin" {
		t.Errorf("github_username: got %v want AidanAllchin", m.GitHubUsername)
	}
	// Bot presence depends on agent bootstrap + team_agents.enabled.
	// The test server bootstraps a default-enabled agent (see newTestServer),
	// so bot should be non-nil with the bootstrapped agent's display name.
	if resp.Bot == nil {
		t.Fatal("bot: expected non-nil (test server bootstraps a default-enabled agent)")
	}
	if resp.Bot.AgentID == "" {
		t.Error("bot.agent_id: expected non-empty")
	}
}

// TestHandleTeamMembers_NoIdentityCaptured handles the pre-GitHub-config
// state: the row exists, github_username is NULL, the endpoint still
// returns successfully so the FE can render the editor (just with the
// Variant-A toggle disabled).
func TestHandleTeamMembers_NoIdentityCaptured(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/team/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp teamMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(resp.Members))
	}
	if resp.Members[0].GitHubUsername != nil {
		t.Errorf("expected null github_username (not yet captured), got %q", *resp.Members[0].GitHubUsername)
	}
}
