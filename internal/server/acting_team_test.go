package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// seedTeam inserts an extra team into an org. SQLite's ListForUser returns
// every team in the org, so one more row makes the local caller a member of
// two teams.
func seedTeam(t *testing.T, s *Server, orgID, slug string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		id, orgID, slug, slug,
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
	return id
}

// TestResolveActingTeam pins the write resolver's order: a pick the caller is a
// member of, then the sticky default, then the sole team. A teamless caller and
// a pick of a team the caller is not on are selection errors, which map to 400.
func TestResolveActingTeam(t *testing.T) {
	const second = "second" // stands for the id of the seeded second team
	for _, tc := range []struct {
		name       string
		orgID      string
		seedSecond bool
		pick       string
		sticky     bool
		want       string
		wantErr    error
	}{
		{name: "sole team", orgID: runmode.LocalDefaultOrgID, want: runmode.LocalDefaultTeamID},
		{name: "teamless org", orgID: "00000000-0000-0000-0000-0000000009ff", wantErr: teamscope.ErrNoTeam},
		{name: "explicit pick", orgID: runmode.LocalDefaultOrgID, seedSecond: true, pick: second, want: second},
		{name: "foreign pick", orgID: runmode.LocalDefaultOrgID, pick: uuid.New().String(), wantErr: teamscope.ErrForbidden},
		{name: "sticky default", orgID: runmode.LocalDefaultOrgID, seedSecond: true, sticky: true, want: second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			pick, want := tc.pick, tc.want
			if tc.seedSecond {
				id := seedTeam(t, s, runmode.LocalDefaultOrgID, second)
				if pick == second {
					pick = id
				}
				if want == second {
					want = id
				}
				if tc.sticky {
					if err := s.users.SetLastActingTeam(t.Context(), runmode.LocalDefaultUserID, id); err != nil {
						t.Fatalf("set last-acting team: %v", err)
					}
				}
			}

			got, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, tc.orgID, runmode.LocalDefaultUserID, pick)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || !teamscope.IsSelectionError(err) {
					t.Fatalf("error = %v; want %v, a selection error", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveActing: %v", err)
			}
			if got != want {
				t.Errorf("acting team = %q; want %q", got, want)
			}
		})
	}
}

// TestResolveReadTeam_DefaultsNeverBlock: the read resolver never errors on a
// multi-team caller. With no filter and no sticky default it returns the
// oldest team, and a stale filter falls back to it rather than 4xx-ing a read.
func TestResolveReadTeam_DefaultsNeverBlock(t *testing.T) {
	s := newTestServer(t)
	seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	// No filter, no sticky → first (oldest) team = the local default.
	got, err := teamscope.ResolveRead(t.Context(), s.teams, s.users, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "")
	if err != nil {
		t.Fatalf("resolveReadTeam: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("read team = %q; want the first team %q", got, runmode.LocalDefaultTeamID)
	}

	// A bogus filter value is ignored, not an error.
	got, err = teamscope.ResolveRead(t.Context(), s.teams, s.users, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "not-a-real-team")
	if err != nil {
		t.Fatalf("resolveReadTeam with stale filter: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("read team with stale filter = %q; want fallback to first team %q", got, runmode.LocalDefaultTeamID)
	}
}

// TestPromptCreate_AmbiguousTeamIs400: with two teams and no team_id, a create
// is refused with 400 rather than landing on a guessed team.
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
