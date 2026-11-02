package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ClaimArchivedTeamFixture is what a backend hands
// RunClaimArchivedTeamConformance: a task store over an org with two teams, one
// of them archived, and the people and agent the claim doors act for.
type ClaimArchivedTeamFixture struct {
	Store   db.TaskStore
	OrgID   string
	AgentID string
	// UserID is a member of both teams: the claimant every door acts for.
	UserID string
	// HolderID is a member of ArchivedTeamID alone: the person a reassign
	// moves a claim off.
	HolderID       string
	ActiveTeamID   string
	ArchivedTeamID string
	// SeedTask inserts an unclaimed, queued task owned by ownerTeamID, with
	// visibleTeams as its visibility set, and returns its id.
	SeedTask func(t *testing.T, ownerTeamID string, visibleTeams []string) string
	// HoldForUser writes userID's claim onto the task and leaves its team
	// as it is: the row a claim taken before its team was archived leaves.
	HoldForUser func(t *testing.T, taskID, userID string)
}

// ClaimArchivedTeamFactory builds a fresh ClaimArchivedTeamFixture per
// subtest.
type ClaimArchivedTeamFactory func(t *testing.T) ClaimArchivedTeamFixture

// claimRow is the part of a task the claim doors write.
type claimRow struct {
	user, agent, status, team string
}

func readClaimRow(ctx context.Context, t *testing.T, s db.TaskStore, orgID, taskID string) claimRow {
	t.Helper()
	task, err := s.Get(ctx, orgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("Get(%s) = (%v, %v)", taskID, task, err)
	}
	r := claimRow{user: task.ClaimedByUserID, agent: task.ClaimedByAgentID, status: task.Status}
	if task.TeamID != nil {
		r.team = *task.TeamID
	}
	return r
}

// RunClaimArchivedTeamConformance pins that a claim never lands a task on an
// archived team and never keeps it on one: every claim door, and
// ResolveClaimTeam beside them, moves a task off an archived owner onto the
// claimant's active team, and refuses before writing when the claimant has no
// active team to move it to.
func RunClaimArchivedTeamConformance(t *testing.T, factory ClaimArchivedTeamFactory) {
	t.Helper()
	ctx := context.Background()

	// The case a member of an active team and an archived one meets: the
	// archived team owns the task and both teams can see it. The doors used
	// to prefer the owner, which kept the task on the archived team.
	t.Run("EachClaimMovesTheTaskOffAnArchivedOwnerOntoTheActiveTeam", func(t *testing.T) {
		fx := factory(t)
		seed := func(t *testing.T) string {
			t.Helper()
			return fx.SeedTask(t, fx.ArchivedTeamID, []string{fx.ActiveTeamID, fx.ArchivedTeamID})
		}
		landed := func(t *testing.T, taskID string) {
			t.Helper()
			if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got.team != fx.ActiveTeamID {
				t.Errorf("team_id = %q after the claim, want the active team %q (archived owner %q)",
					got.team, fx.ActiveTeamID, fx.ArchivedTeamID)
			}
		}

		t.Run("ResolveClaimTeam", func(t *testing.T) {
			taskID := seed(t)
			team, err := fx.Store.ResolveClaimTeam(ctx, fx.OrgID, taskID, fx.UserID)
			if err != nil || team != fx.ActiveTeamID {
				t.Errorf("ResolveClaimTeam = (%q, %v), want (%q, nil)", team, err, fx.ActiveTeamID)
			}
		})
		t.Run("ClaimQueuedForUser", func(t *testing.T) {
			taskID := seed(t)
			if ok, err := fx.Store.ClaimQueuedForUser(ctx, fx.OrgID, taskID, fx.UserID); err != nil || !ok {
				t.Fatalf("ClaimQueuedForUser = (%v, %v), want (true, nil)", ok, err)
			}
			landed(t, taskID)
		})
		t.Run("HandoffAgentClaim", func(t *testing.T) {
			taskID := seed(t)
			result, err := fx.Store.HandoffAgentClaim(ctx, fx.OrgID, taskID, fx.AgentID, fx.UserID)
			if err != nil || result != db.HandoffChanged {
				t.Fatalf("HandoffAgentClaim = (%v, %v), want (HandoffChanged, nil)", result, err)
			}
			landed(t, taskID)
		})
		t.Run("TakeoverClaimFromAgent", func(t *testing.T) {
			taskID := seed(t)
			// The bot's stamp with no acting team keeps the archived owner.
			if ok, err := fx.Store.StampAgentClaimIfUnclaimed(ctx, fx.OrgID, taskID, fx.AgentID, ""); err != nil || !ok {
				t.Fatalf("StampAgentClaimIfUnclaimed = (%v, %v)", ok, err)
			}
			if ok, err := fx.Store.TakeoverClaimFromAgent(ctx, fx.OrgID, taskID, fx.UserID); err != nil || !ok {
				t.Fatalf("TakeoverClaimFromAgent = (%v, %v), want (true, nil)", ok, err)
			}
			landed(t, taskID)
		})
		t.Run("ReassignClaimToUser", func(t *testing.T) {
			taskID := seed(t)
			fx.HoldForUser(t, taskID, fx.HolderID)
			if ok, err := fx.Store.ReassignClaimToUser(ctx, fx.OrgID, taskID, fx.HolderID, fx.UserID); err != nil || !ok {
				t.Fatalf("ReassignClaimToUser = (%v, %v), want (true, nil)", ok, err)
			}
			landed(t, taskID)
		})
	})

	// The archived team owns the task and is the only team that can see it,
	// so the claimant's active team is no candidate and the owner cannot be
	// kept. Every door refuses before it writes, and the person-claim doors
	// say why, because no retry gets a different answer.
	t.Run("AClaimWithOnlyArchivedTeamsIsRefusedAndWritesNothing", func(t *testing.T) {
		fx := factory(t)
		seed := func(t *testing.T) string {
			t.Helper()
			return fx.SeedTask(t, fx.ArchivedTeamID, []string{fx.ArchivedTeamID})
		}
		unchanged := func(t *testing.T, taskID string, before claimRow) {
			t.Helper()
			if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got != before {
				t.Errorf("task after a refused claim = %+v, want it unchanged at %+v", got, before)
			}
		}

		t.Run("ResolveClaimTeam", func(t *testing.T) {
			taskID := seed(t)
			team, err := fx.Store.ResolveClaimTeam(ctx, fx.OrgID, taskID, fx.UserID)
			if !errors.Is(err, db.ErrNoActiveClaimTeam) {
				t.Errorf("ResolveClaimTeam = (%q, %v), want ErrNoActiveClaimTeam", team, err)
			}
		})
		t.Run("ClaimQueuedForUser", func(t *testing.T) {
			taskID := seed(t)
			before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
			ok, err := fx.Store.ClaimQueuedForUser(ctx, fx.OrgID, taskID, fx.UserID)
			if ok || !errors.Is(err, db.ErrNoActiveClaimTeam) {
				t.Errorf("ClaimQueuedForUser = (%v, %v), want (false, ErrNoActiveClaimTeam)", ok, err)
			}
			unchanged(t, taskID, before)
		})
		t.Run("HandoffAgentClaim", func(t *testing.T) {
			taskID := seed(t)
			before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
			result, err := fx.Store.HandoffAgentClaim(ctx, fx.OrgID, taskID, fx.AgentID, fx.UserID)
			if result != db.HandoffRefused || !errors.Is(err, db.ErrNoActiveClaimTeam) {
				t.Errorf("HandoffAgentClaim = (%v, %v), want (HandoffRefused, ErrNoActiveClaimTeam)", result, err)
			}
			unchanged(t, taskID, before)
		})
		t.Run("TakeoverClaimFromAgent", func(t *testing.T) {
			taskID := seed(t)
			if ok, err := fx.Store.StampAgentClaimIfUnclaimed(ctx, fx.OrgID, taskID, fx.AgentID, ""); err != nil || !ok {
				t.Fatalf("StampAgentClaimIfUnclaimed = (%v, %v)", ok, err)
			}
			before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
			ok, err := fx.Store.TakeoverClaimFromAgent(ctx, fx.OrgID, taskID, fx.UserID)
			if ok || !errors.Is(err, db.ErrNoActiveClaimTeam) {
				t.Errorf("TakeoverClaimFromAgent = (%v, %v), want (false, ErrNoActiveClaimTeam)", ok, err)
			}
			unchanged(t, taskID, before)
		})
		// The reassign's target guard already requires an active team the
		// task belongs to, and it is what refuses here: the caller re-reads
		// and answers that the target is on no team that can see the task.
		t.Run("ReassignClaimToUser", func(t *testing.T) {
			taskID := seed(t)
			fx.HoldForUser(t, taskID, fx.HolderID)
			before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
			ok, err := fx.Store.ReassignClaimToUser(ctx, fx.OrgID, taskID, fx.HolderID, fx.UserID)
			if ok || err != nil {
				t.Errorf("ReassignClaimToUser = (%v, %v), want (false, nil)", ok, err)
			}
			unchanged(t, taskID, before)
		})
	})

	// A plain lost race is still a plain refusal: the team refusal is only
	// for a claim with nowhere to go.
	t.Run("ALostClaimRaceOnAnActiveTeamIsNotATeamRefusal", func(t *testing.T) {
		fx := factory(t)
		taskID := fx.SeedTask(t, fx.ActiveTeamID, []string{fx.ActiveTeamID})
		fx.HoldForUser(t, taskID, fx.HolderID)
		ok, err := fx.Store.ClaimQueuedForUser(ctx, fx.OrgID, taskID, fx.UserID)
		if ok || err != nil {
			t.Errorf("ClaimQueuedForUser on a held task = (%v, %v), want (false, nil)", ok, err)
		}
		if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got.user != fx.HolderID || got.status != "in_progress" {
			t.Errorf("task = %+v, want it still held by %s", got, fx.HolderID)
		}
	})
}
