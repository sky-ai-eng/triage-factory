package server

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// seedTeam inserts an extra team into an org so the resolver tests can
// exercise the ≥2-team branches. SQLite's ListForUser returns every team
// in the org (N=1 has no memberships join), so adding a row is enough to
// make the caller "belong to" two teams from the resolver's view.
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

// TestResolveActingTeam_ReturnsSoleTeam pins the byte-identical solo
// path: with one team in the org (the local default), no explicit pick,
// and no sticky default, the resolver returns the sole team. Every write
// site relies on this for solo behavior.
func TestResolveActingTeam_ReturnsSoleTeam(t *testing.T) {
	s := newTestServer(t)

	got, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, "")
	if err != nil {
		t.Fatalf("resolveActingTeam: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("acting team = %q; want %q (the org's sole/default team)", got, runmode.LocalDefaultTeamID)
	}
}

// TestResolveActingTeam_NoTeamErrors pins the bootstrap-bug guard: an org
// with no team resolves to a hard error rather than an empty string a
// caller might thread into a write.
func TestResolveActingTeam_NoTeamErrors(t *testing.T) {
	s := newTestServer(t)

	const orglessOrg = "00000000-0000-0000-0000-0000000009ff"
	_, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, orglessOrg, runmode.LocalDefaultUserID, "")
	if err == nil {
		t.Fatalf("resolveActingTeam on a teamless org returned nil error; want a hard error")
	}
	if !strings.Contains(err.Error(), "has no team") {
		t.Errorf("error = %q; want it to mention the missing team", err.Error())
	}
}

// TestResolveActingTeam_AmbiguousWithoutPick is the PR2 contract that
// resolves the old TODO: a caller on two teams, with no explicit pick and
// no sticky default, is refused rather than silently landing on a guessed
// team. The required picker means the UI never reaches this; a non-UI
// write that omits team_id gets a 400-mapped error.
func TestResolveActingTeam_AmbiguousWithoutPick(t *testing.T) {
	s := newTestServer(t)
	seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	_, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, "")
	if err == nil {
		t.Fatalf("resolveActingTeam with ≥2 teams and no pick returned nil error; want errActingTeamRequired")
	}
	if !teamscope.IsSelectionError(err) {
		t.Errorf("error %v is not a selection error; should map to 400", err)
	}
}

// TestResolveActingTeam_ExplicitPickHonored: a valid explicit pick wins
// over everything else.
func TestResolveActingTeam_ExplicitPickHonored(t *testing.T) {
	s := newTestServer(t)
	second := seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	got, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, second)
	if err != nil {
		t.Fatalf("resolveActingTeam: %v", err)
	}
	if got != second {
		t.Errorf("acting team = %q; want the explicitly picked team %q", got, second)
	}
}

// TestResolveActingTeam_PickForeignTeamRejected: picking a team the
// caller doesn't belong to is refused (not silently honored). A team id
// outside the caller's ListForUser set models a forged/stale pick.
func TestResolveActingTeam_PickForeignTeamRejected(t *testing.T) {
	s := newTestServer(t)
	foreign := uuid.New().String() // never in the caller's team set

	_, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, foreign)
	if err == nil {
		t.Fatalf("picking a non-member team returned nil error; want errActingTeamForbidden")
	}
	if !teamscope.IsSelectionError(err) {
		t.Errorf("error %v is not a selection error; should map to 400", err)
	}
}

// TestResolveActingTeam_StickyDefaultSeedsAmbiguous: with ≥2 teams and no
// explicit pick, a valid sticky default resolves the otherwise-ambiguous
// case instead of erroring.
func TestResolveActingTeam_StickyDefaultSeedsAmbiguous(t *testing.T) {
	s := newTestServer(t)
	second := seedTeam(t, s, runmode.LocalDefaultOrgID, "second")
	if err := s.users.SetLastActingTeam(t.Context(), runmode.LocalDefaultUserID, second); err != nil {
		t.Fatalf("set last-acting team: %v", err)
	}

	got, err := teamscope.ResolveActing(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, "")
	if err != nil {
		t.Fatalf("resolveActingTeam: %v", err)
	}
	if got != second {
		t.Errorf("acting team = %q; want the sticky-default team %q", got, second)
	}
}

// TestResolveReadTeam_DefaultsNeverBlock: the read-deck resolver never
// errors on a multi-team caller — absent a filter and a sticky default it
// returns the first (oldest) team, and a stale pick falls through rather
// than 4xx-ing a read.
func TestResolveReadTeam_DefaultsNeverBlock(t *testing.T) {
	s := newTestServer(t)
	seedTeam(t, s, runmode.LocalDefaultOrgID, "second")

	// No filter, no sticky → first (oldest) team = the local default.
	got, err := teamscope.ResolveRead(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, "")
	if err != nil {
		t.Fatalf("resolveReadTeam: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("read team = %q; want the first team %q", got, runmode.LocalDefaultTeamID)
	}

	// A bogus filter value is ignored, not an error.
	got, err = teamscope.ResolveRead(t.Context(), s.teams, s.users, runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, "not-a-real-team")
	if err != nil {
		t.Fatalf("resolveReadTeam with stale filter: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("read team with stale filter = %q; want fallback to first team %q", got, runmode.LocalDefaultTeamID)
	}
}
