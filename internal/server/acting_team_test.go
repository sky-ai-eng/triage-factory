package server

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveActingTeam_ReturnsSoleTeam pins the only path that executes
// today: with one team in the org (the local default), the resolver
// returns it. This is the contract every write site relies on for
// byte-identical solo behavior — when PR2 adds the sticky-default /
// most-recent / explicit-pick steps, this case must still hold for a
// 1-team org.
func TestResolveActingTeam_ReturnsSoleTeam(t *testing.T) {
	s := newTestServer(t)

	got, err := resolveActingTeam(t.Context(), s.teams, runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("resolveActingTeam: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("acting team = %q; want %q (the org's sole/default team)", got, runmode.LocalDefaultTeamID)
	}
}

// TestResolveActingTeam_NoTeamErrors pins the bootstrap-bug guard: an org
// with no team resolves to a hard error rather than an empty string a
// caller might thread into a write (which would trip the stores' own
// team-required guards with a less obvious message). Every install gets a
// default team at provisioning time, so this only happens on a broken
// fixture — the resolver fails loudly so that's easy to spot.
func TestResolveActingTeam_NoTeamErrors(t *testing.T) {
	s := newTestServer(t)

	// An org id with no teams row. SQLite GetDefaultForOrg filters by
	// org_id and returns "" (nil error) when nothing matches; the
	// resolver turns that empty into an explicit error.
	const orglessOrg = "00000000-0000-0000-0000-0000000009ff"
	_, err := resolveActingTeam(t.Context(), s.teams, orglessOrg)
	if err == nil {
		t.Fatalf("resolveActingTeam on a teamless org returned nil error; want a hard error")
	}
	if !strings.Contains(err.Error(), "has no team") {
		t.Errorf("error = %q; want it to mention the missing team", err.Error())
	}
}
