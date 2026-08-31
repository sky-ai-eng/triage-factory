package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamsList_LocalReturnsSoleTeam: in local mode (N=1) the teams list
// returns the single local team and marks no sticky default — the frontend's
// ≥2 gate keeps every selector hidden off this one-team response.
func TestTeamsList_LocalReturnsSoleTeam(t *testing.T) {
	s := newTestServer(t)

	page := decodeList[teamJSON](t, doJSON(t, s, http.MethodPost, "/api/teams/list", map[string]any{}))
	if len(page.Items) != 1 || page.Total() != 1 {
		t.Fatalf("teams = %d (total %d), want 1 / 1", len(page.Items), page.Total())
	}
	if page.Items[0].ID != runmode.LocalDefaultTeamID {
		t.Errorf("team id = %q, want %q", page.Items[0].ID, runmode.LocalDefaultTeamID)
	}
	// Local N=1: the sole user admins the sole team, so the settings
	// surface's Team-section gate (role == "admin") reads true.
	if page.Items[0].Role != "admin" {
		t.Errorf("role = %q, want %q (local sole team)", page.Items[0].Role, "admin")
	}
	if page.Items[0].IsLastActing {
		t.Errorf("is_last_acting = true, want false (no sticky default set)")
	}
}

// TestTeamsList_TwoTeams: with a second team seeded into the local org, the
// list surfaces both (the SQLite store returns every org team as the single
// user's set), which is what flips the frontend into rendering the selectors.
// A one-row window then proves the pages partition that set rather than
// repeating its head.
func TestTeamsList_TwoTeams(t *testing.T) {
	s := newTestServer(t)
	second := seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	page := decodeList[teamJSON](t, doJSON(t, s, http.MethodPost, "/api/teams/list", map[string]any{}))
	if len(page.Items) != 2 || page.Total() != 2 {
		t.Fatalf("teams = %d (total %d), want 2 / 2", len(page.Items), page.Total())
	}
	got := map[string]bool{}
	for _, tm := range page.Items {
		got[tm.ID] = true
	}
	if !got[runmode.LocalDefaultTeamID] || !got[second] {
		t.Errorf("teams = %+v; want both default and %s", page.Items, second)
	}

	first := decodeList[teamJSON](t, doJSON(t, s, http.MethodPost, "/api/teams/list", map[string]any{"page_size": 1}))
	if len(first.Items) != 1 || first.Total() != 2 || first.NextPageToken == "" {
		t.Fatalf("page 1 = %+v (total %d, token %q), want one row, total 2, a next token",
			first.Items, first.Total(), first.NextPageToken)
	}
	next := decodeList[teamJSON](t, doJSON(t, s, http.MethodPost, "/api/teams/list",
		map[string]any{"page_size": 1, "page_token": first.NextPageToken}))
	if len(next.Items) != 1 || next.Items[0].ID == first.Items[0].ID {
		t.Errorf("page 2 = %+v, want the other team (pages must partition the set)", next.Items)
	}
}

// The single read is the only reader of a team's description, and it answers
// with the list's row shape. {team_id} takes the local "default" alias.
func TestTeamGet_Local(t *testing.T) {
	s := newTestServer(t)

	for _, addr := range []string{runmode.LocalDefaultTeamID, "default"} {
		rec := doJSON(t, s, http.MethodGet, "/api/teams/"+addr, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/teams/%s = %d, want 200; body=%s", addr, rec.Code, rec.Body.String())
		}
		var got teamJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != runmode.LocalDefaultTeamID || got.Role != "admin" {
			t.Errorf("GET /api/teams/%s = %+v, want the local team as admin", addr, got)
		}
		if got.ArchivedAt != "" {
			t.Errorf("archived_at = %q, want empty for a live team", got.ArchivedAt)
		}
	}

	// A well-formed id for a team that doesn't exist is not-found, and so is a
	// segment that is not an id at all — a path id that names nothing must not
	// reach the store and must not disclose which of the two it was.
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "not-an-id"} {
		if rec := doJSON(t, s, http.MethodGet, "/api/teams/"+id, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/teams/%s = %d, want 404; body=%s", id, rec.Code, rec.Body.String())
		}
	}
}

// TestTeamCreate_LocalIsNotFound: "add team" is multi-only; in local
// mode POST /api/teams is 404 (the feature is absent at N=1).
func TestTeamCreate_LocalIsNotFound(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/teams", map[string]string{"name": "Platform"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (multi-only); body=%s", rec.Code, rec.Body.String())
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
