package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamsList_LocalReturnsSoleTeam: in local mode (N=1) GET /api/teams
// returns the single local team and no sticky default — the frontend's
// ≥2 gate keeps every selector hidden off this one-team response.
func TestTeamsList_LocalReturnsSoleTeam(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/teams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Teams) != 1 {
		t.Fatalf("teams = %d, want 1", len(resp.Teams))
	}
	if resp.Teams[0].ID != runmode.LocalDefaultTeamID {
		t.Errorf("team id = %q, want %q", resp.Teams[0].ID, runmode.LocalDefaultTeamID)
	}
	// Local N=1: the sole user admins the sole team, so the settings
	// surface's Team-section gate (role == "admin") reads true.
	if resp.Teams[0].Role != "admin" {
		t.Errorf("role = %q, want %q (local sole team)", resp.Teams[0].Role, "admin")
	}
	if resp.LastActingTeamID != "" {
		t.Errorf("last_acting_team_id = %q, want empty (unset)", resp.LastActingTeamID)
	}
}

// TestTeamsList_TwoTeams: with a second team seeded into the local org,
// GET /api/teams surfaces both (the SQLite store returns every org team
// as the single user's set), which is what flips the frontend into
// rendering the selectors.
func TestTeamsList_TwoTeams(t *testing.T) {
	s := newTestServer(t)
	second := seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	rec := doJSON(t, s, http.MethodGet, "/api/teams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp teamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Teams) != 2 {
		t.Fatalf("teams = %d, want 2", len(resp.Teams))
	}
	got := map[string]bool{}
	for _, tm := range resp.Teams {
		got[tm.ID] = true
	}
	if !got[runmode.LocalDefaultTeamID] || !got[second] {
		t.Errorf("teams = %+v; want both default and %s", resp.Teams, second)
	}
}

// TestTeamCreate_LocalIsNotFound: "add team" is hosted-only; in local
// mode POST /api/teams is 404 (the feature is absent at N=1).
func TestTeamCreate_LocalIsNotFound(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/teams", map[string]string{"name": "Platform"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (hosted-only); body=%s", rec.Code, rec.Body.String())
	}
}

// TestPromptCreate_AmbiguousTeamIs400: with two teams visible and no
// team_id supplied, the create is refused (400) rather than silently
// guessing — the server-side mirror of the required picker. (The
// positive "a write lands on the picked team" assertion is a pgtest:
// the SQLite store pins the sole local team by design, so honoring an
// explicit pick is a Postgres/multi-mode behavior.)
func TestPromptCreate_AmbiguousTeamIs400(t *testing.T) {
	s := newTestServer(t)
	seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]string{
		"name": "Unscoped prompt",
		"body": "do the thing",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (ambiguous team); body=%s", rec.Code, rec.Body.String())
	}
}
