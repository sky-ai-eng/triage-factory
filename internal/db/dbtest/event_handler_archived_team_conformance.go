package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ArchivedTeamHandlersFixture is what a backend hands
// RunArchivedTeamHandlersConformance: the handler and team stores, the org and
// its team, a door that seeds a second team in the same org, and one that seeds
// an enabled ci_check_failed rule on a named team (SQLite's Create pins every
// row to the local team, so a rule on the second team cannot go through it).
type ArchivedTeamHandlersFixture struct {
	Handlers      db.EventHandlerStore
	Teams         db.TeamsStore
	OrgID, TeamID string
	// SeedTeam inserts another team in OrgID and returns its id.
	SeedTeam func(t *testing.T) string
	// SeedRule inserts an enabled ci_check_failed rule on teamID and returns
	// its id.
	SeedRule func(t *testing.T, teamID string) string
}

// ArchivedTeamHandlersFactory builds a fresh ArchivedTeamHandlersFixture per
// subtest.
type ArchivedTeamHandlersFactory func(t *testing.T) ArchivedTeamHandlersFixture

// RunArchivedTeamHandlersConformance pins that the enabled-handler read the
// router matches against leaves out a handler whose team is archived, while
// the by-id read still returns it, and that restoring the team brings it back.
func RunArchivedTeamHandlersConformance(t *testing.T, factory ArchivedTeamHandlersFactory) {
	t.Helper()

	t.Run("GetEnabledForEventSystem_OmitsAnArchivedTeamsHandler", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()
		other := fx.SeedTeam(t)
		live := fx.SeedRule(t, fx.TeamID)
		archived := fx.SeedRule(t, other)

		enabled := func() map[string]bool {
			t.Helper()
			hs, err := fx.Handlers.GetEnabledForEventSystem(ctx, fx.OrgID, domain.EventGitHubPRCICheckFailed)
			if err != nil {
				t.Fatalf("GetEnabledForEventSystem: %v", err)
			}
			out := make(map[string]bool, len(hs))
			for _, h := range hs {
				out[h.ID] = true
			}
			return out
		}

		if got := enabled(); !got[live] || !got[archived] {
			t.Fatalf("before archive: enabled = %v, want both %s and %s", got, live, archived)
		}

		if _, err := fx.Teams.Archive(ctx, other); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		if got := enabled(); !got[live] || got[archived] {
			t.Errorf("after archive: enabled = %v, want %s without the archived team's %s", got, live, archived)
		}
		h, err := fx.Handlers.GetSystem(ctx, fx.OrgID, archived)
		if err != nil || h == nil {
			t.Errorf("GetSystem(archived team's handler) = %v, %v; want the row", h, err)
		}

		if _, err := fx.Teams.Restore(ctx, other); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := enabled(); !got[live] || !got[archived] {
			t.Errorf("after restore: enabled = %v, want both %s and %s", got, live, archived)
		}
	})
}
