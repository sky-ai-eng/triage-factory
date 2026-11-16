package dbtest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ClaimTeamFixture is what the claim-team suite needs from a backend: a
// person who writes two active teams and only views a third, and a way to
// seed a task with a chosen owner and visibility set.
type ClaimTeamFixture struct {
	Store   db.TaskStore
	OrgID   string
	AgentID string
	// UserID writes both WriteTeamIDs and only views ViewTeamID.
	UserID string
	// WriteTeamIDs are two active teams, in team id order.
	WriteTeamIDs [2]string
	ViewTeamID   string
	// SeedTask inserts an unclaimed, queued task owned by ownerTeamID ("" for
	// no owner), with visibleTeams as its visibility set, and returns its id.
	SeedTask func(t *testing.T, ownerTeamID string, visibleTeams []string) string
}

// ClaimTeamFactory builds a fresh ClaimTeamFixture per subtest.
type ClaimTeamFactory func(t *testing.T) ClaimTeamFixture

// RunClaimTeamConformance pins which teams a claim or a delegate may land a
// task on (TaskStore.ClaimTeamCandidates), that the claim writes land on the
// team they are given and refuse one that read does not return, and what they
// answer a claimant who has no team at all.
func RunClaimTeamConformance(t *testing.T, factory ClaimTeamFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("ClaimTeamCandidates", func(t *testing.T) {
		fx := factory(t)
		first, second, view := fx.WriteTeamIDs[0], fx.WriteTeamIDs[1], fx.ViewTeamID
		all := []string{first, second, view}
		cases := []struct {
			name    string
			owner   string
			visible []string
			want    []string
		}{
			{"the owning team alone when the caller writes it", second, all, []string{second}},
			{"the owning team though it is not in the visibility set", second, []string{first}, []string{second}},
			{"the caller's writing teams that see an unowned task, in id order", "", []string{view, second, first}, []string{first, second}},
			{"the same when the caller only views the owning team", view, all, []string{first, second}},
			{"only a team in the visibility set", "", []string{second, view}, []string{second}},
			{"none when the caller only views every team on the task", view, []string{view}, nil},
			{"none for an unowned task no team sees", "", nil, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				taskID := fx.SeedTask(t, tc.owner, tc.visible)
				got, err := fx.Store.ClaimTeamCandidates(ctx, fx.OrgID, taskID, fx.UserID)
				if err != nil || !slices.Equal(got, tc.want) {
					t.Errorf("ClaimTeamCandidates = (%v, %v), want (%v, nil)", got, err, tc.want)
				}
			})
		}
		t.Run("none for a task that does not exist", func(t *testing.T) {
			got, err := fx.Store.ClaimTeamCandidates(ctx, fx.OrgID, uuid.NewString(), fx.UserID)
			if err != nil || len(got) != 0 {
				t.Errorf("ClaimTeamCandidates = (%v, %v), want none", got, err)
			}
		})
	})

	// Each claim door, against a task two of the caller's teams can take and
	// against one only the first can.
	doors := []struct {
		name string
		// prep puts the task in the state the door starts from.
		prep  func(t *testing.T, fx ClaimTeamFixture, taskID string)
		claim func(fx ClaimTeamFixture, taskID, teamID string, version int64) (bool, error)
	}{
		{
			name: "ClaimQueuedForUser",
			claim: func(fx ClaimTeamFixture, taskID, teamID string, version int64) (bool, error) {
				return fx.Store.ClaimQueuedForUser(ctx, fx.OrgID, taskID, fx.UserID, teamID, version)
			},
		},
		{
			name: "TakeoverClaimFromAgent",
			prep: func(t *testing.T, fx ClaimTeamFixture, taskID string) {
				t.Helper()
				// With no acting team the bot's stamp keeps the task's owner.
				if ok, err := fx.Store.StampAgentClaimIfUnclaimed(ctx, fx.OrgID, taskID, fx.AgentID, ""); err != nil || !ok {
					t.Fatalf("StampAgentClaimIfUnclaimed = (%v, %v)", ok, err)
				}
			},
			claim: func(fx ClaimTeamFixture, taskID, teamID string, version int64) (bool, error) {
				return fx.Store.TakeoverClaimFromAgent(ctx, fx.OrgID, taskID, fx.UserID, teamID, version)
			},
		},
		{
			name: "HandoffAgentClaim",
			claim: func(fx ClaimTeamFixture, taskID, teamID string, version int64) (bool, error) {
				result, _, err := fx.Store.HandoffAgentClaim(ctx, fx.OrgID, taskID, fx.AgentID, fx.UserID, teamID, version)
				return result == db.HandoffChanged, err
			},
		},
	}

	t.Run("AClaimLandsOnTheTeamItNames", func(t *testing.T) {
		fx := factory(t)
		for _, door := range doors {
			t.Run(door.name, func(t *testing.T) {
				// Owned by the team the caller only views, so both writing
				// teams are candidates and the lower id is not the one named.
				taskID := fx.SeedTask(t, fx.ViewTeamID, []string{fx.WriteTeamIDs[0], fx.WriteTeamIDs[1], fx.ViewTeamID})
				if door.prep != nil {
					door.prep(t, fx, taskID)
				}
				ok, err := door.claim(fx, taskID, fx.WriteTeamIDs[1], TaskVersion(t, fx.Store, fx.OrgID, taskID))
				if err != nil || !ok {
					t.Fatalf("%s = (%v, %v), want the claim to land", door.name, ok, err)
				}
				if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got.team != fx.WriteTeamIDs[1] {
					t.Errorf("team_id = %q after the claim, want the named team %q", got.team, fx.WriteTeamIDs[1])
				}
			})
		}
	})

	t.Run("AClaimRefusesATeamThatIsNotACandidate", func(t *testing.T) {
		fx := factory(t)
		for _, door := range doors {
			for _, named := range []struct{ name, team string }{
				{"another team the caller writes", fx.WriteTeamIDs[1]},
				{"a team the caller only views", fx.ViewTeamID},
				{"an id that names no team", noClaimTeam},
			} {
				t.Run(door.name+"/"+named.name, func(t *testing.T) {
					// Owned by a team the caller writes, which is then the
					// only candidate, whatever else can see the task.
					taskID := fx.SeedTask(t, fx.WriteTeamIDs[0], []string{fx.WriteTeamIDs[0], fx.WriteTeamIDs[1], fx.ViewTeamID})
					if door.prep != nil {
						door.prep(t, fx, taskID)
					}
					before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
					ok, err := door.claim(fx, taskID, named.team, TaskVersion(t, fx.Store, fx.OrgID, taskID))
					if ok || !errors.Is(err, db.ErrClaimTeamNotCandidate) {
						t.Errorf("%s = (%v, %v), want (false, ErrClaimTeamNotCandidate)", door.name, ok, err)
					}
					if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got != before {
						t.Errorf("task after a refused claim = %+v, want it unchanged at %+v", got, before)
					}
				})
			}
		}
	})

	// A claimant with no team to land the task on is refused by every door,
	// with the reason: an archived team is not why here, so the answer is not
	// the archived one (RunClaimArchivedTeamConformance covers that).
	t.Run("AClaimWithNoTeamIsRefusedAsNoTeam", func(t *testing.T) {
		fx := factory(t)
		for _, shape := range []struct {
			name    string
			owner   string
			visible []string
		}{
			{"the caller only views every team on the task", fx.ViewTeamID, []string{fx.ViewTeamID}},
			{"an unowned task no team sees", "", nil},
		} {
			t.Run(shape.name+"/NoClaimTeamReason", func(t *testing.T) {
				taskID := fx.SeedTask(t, shape.owner, shape.visible)
				if err := fx.Store.NoClaimTeamReason(ctx, fx.OrgID, taskID, fx.UserID); !errors.Is(err, db.ErrNoClaimTeam) {
					t.Errorf("NoClaimTeamReason = %v, want ErrNoClaimTeam", err)
				}
			})
			for _, door := range doors {
				// The bot's stamp needs an owner to keep, so the takeover
				// runs on the owned shape alone.
				if door.prep != nil && shape.owner == "" {
					continue
				}
				t.Run(shape.name+"/"+door.name, func(t *testing.T) {
					taskID := fx.SeedTask(t, shape.owner, shape.visible)
					if door.prep != nil {
						door.prep(t, fx, taskID)
					}
					before := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID)
					ok, err := door.claim(fx, taskID, fx.WriteTeamIDs[0], TaskVersion(t, fx.Store, fx.OrgID, taskID))
					if ok || !errors.Is(err, db.ErrNoClaimTeam) {
						t.Errorf("%s = (%v, %v), want (false, ErrNoClaimTeam)", door.name, ok, err)
					}
					if got := readClaimRow(ctx, t, fx.Store, fx.OrgID, taskID); got != before {
						t.Errorf("task after a refused claim = %+v, want it unchanged at %+v", got, before)
					}
				})
			}
		}
	})
}
