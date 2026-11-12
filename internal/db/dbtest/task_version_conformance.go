package dbtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskVersionHarness is what a backend hands RunTaskVersionConformance.
type TaskVersionHarness struct {
	Tasks  db.TaskStore
	Scores db.ScoreStore
	// OrgID is the org every call names; AgentID and UserID are an agent and
	// a person the claim writes can name.
	OrgID, AgentID, UserID string
	// OtherTeamID is a second team in the org, for a write that moves a task
	// to another team.
	OtherTeamID string
	// Seed makes a fresh queued, unclaimed task on the org's default team.
	Seed TaskSeeder
	// Exec runs a statement that binds nothing, on a connection RLS does not
	// narrow. The per-column cases write one column at a time, which no store
	// method does.
	Exec func(t *testing.T, query string)
}

// TaskVersionFactory builds a fresh harness per subtest.
type TaskVersionFactory func(t *testing.T) TaskVersionHarness

// RunTaskVersionConformance pins tasks.version: a write that changes one of
// db.TaskVersionColumns raises it by one, a write that changes none of them
// leaves it, and a person's write naming a version the task has moved past is
// refused with db.ErrTaskVersion and changes nothing.
func RunTaskVersionConformance(t *testing.T, mk TaskVersionFactory) {
	t.Helper()
	ctx := context.Background()

	version := func(t *testing.T, h TaskVersionHarness, taskID string) int64 {
		t.Helper()
		return TaskVersion(t, h.Tasks, h.OrgID, taskID)
	}
	setColumn := func(t *testing.T, h TaskVersionHarness, taskID, col, expr string) {
		t.Helper()
		h.Exec(t, fmt.Sprintf(`UPDATE tasks SET %s WHERE id = '%s'`,
			db.TaskSetSQL(db.TaskAssign{Col: col, Expr: expr}), taskID))
	}
	wantVersion := func(t *testing.T, h TaskVersionHarness, taskID, what string, want int64) {
		t.Helper()
		if got := version(t, h, taskID); got != want {
			t.Errorf("%s: version = %d, want %d", what, got, want)
		}
	}

	t.Run("a_new_task_starts_at_version_1", func(t *testing.T) {
		h := mk(t)
		_, _, taskID := h.Seed(t, "ver-new")
		wantVersion(t, h, taskID, "seeded task", 1)
	})

	// One case per person-acted column, each changing that column alone. The
	// table must name exactly db.TaskVersionColumns, so a column added there
	// gets a case here.
	t.Run("each_person_column_raises_the_version_once_and_only_on_a_change", func(t *testing.T) {
		h := mk(t)
		cases := []struct {
			col string
			// held puts the task where the column may take the value: a claim
			// needs a task out of the queue (tasks_queue_unclaimed).
			held bool
			set  string
			// nullable columns are also cleared, which is a change too.
			nullable bool
		}{
			{col: "status", set: "'in_progress'"},
			{col: "claimed_by_user_id", held: true, set: "'" + h.UserID + "'", nullable: true},
			{col: "claimed_by_agent_id", held: true, set: "'" + h.AgentID + "'", nullable: true},
			{col: "snooze_until", set: "CURRENT_TIMESTAMP", nullable: true},
			{col: "closed_at", set: "CURRENT_TIMESTAMP", nullable: true},
			{col: "close_reason", set: "'reconciled'", nullable: true},
			{col: "close_event_type", set: "'" + domain.EventGitHubPRMerged + "'", nullable: true},
			{col: "team_id", set: "'" + h.OtherTeamID + "'", nullable: true},
		}
		var covered []string
		for _, c := range cases {
			covered = append(covered, c.col)
		}
		if !slices.Equal(covered, db.TaskVersionColumns) {
			t.Fatalf("cases cover %v, want db.TaskVersionColumns %v", covered, db.TaskVersionColumns)
		}
		for _, c := range cases {
			t.Run(c.col, func(t *testing.T) {
				_, _, taskID := h.Seed(t, "ver-col-"+c.col)
				if c.held {
					h.Exec(t, fmt.Sprintf(`UPDATE tasks SET status = 'in_progress' WHERE id = '%s'`, taskID))
				}
				v := version(t, h, taskID)
				setColumn(t, h, taskID, c.col, c.set)
				wantVersion(t, h, taskID, "set "+c.col, v+1)
				// The column set to the value it holds changes nothing.
				setColumn(t, h, taskID, c.col, c.col)
				wantVersion(t, h, taskID, c.col+" set to itself", v+1)
				if c.nullable {
					setColumn(t, h, taskID, c.col, "NULL")
					wantVersion(t, h, taskID, "clear "+c.col, v+2)
					setColumn(t, h, taskID, c.col, "NULL")
					wantVersion(t, h, taskID, "clear "+c.col+" again", v+2)
				}
			})
		}
	})

	t.Run("a_write_to_any_other_column_leaves_the_version", func(t *testing.T) {
		h := mk(t)
		_, _, taskID := h.Seed(t, "ver-other")
		for _, a := range []db.TaskAssign{
			{Col: "ai_summary", Expr: "'a summary'"},
			{Col: "priority_reasoning", Expr: "'a reason'"},
			{Col: "priority_score", Expr: "0.9"},
			{Col: "autonomy_suitability", Expr: "0.4"},
			{Col: "scoring_status", Expr: "'scored'"},
			{Col: "severity", Expr: "'high'"},
			{Col: "relevance_reason", Expr: "'authored'"},
			{Col: "source_status", Expr: `'{"status_name":"To Do"}'`},
		} {
			if set := db.TaskSetSQL(a); strings.Contains(set, "version") {
				t.Errorf("TaskSetSQL(%s) = %q, want no version term", a.Col, set)
			}
			setColumn(t, h, taskID, a.Col, a.Expr)
			wantVersion(t, h, taskID, "set "+a.Col, 1)
		}

		// The scorer's writes are the system's own bookkeeping.
		if err := h.Scores.MarkScoring(ctx, h.OrgID, []string{taskID}); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		if err := h.Scores.UpdateTaskScores(ctx, h.OrgID, []domain.TaskScoreUpdate{{
			ID: taskID, PriorityScore: 0.8, AutonomySuitability: 0.7,
			PriorityReasoning: "scored", Summary: "a summary",
		}}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
		if err := h.Scores.ResetScoringToPending(ctx, h.OrgID, []string{taskID}); err != nil {
			t.Fatalf("ResetScoringToPending: %v", err)
		}
		wantVersion(t, h, taskID, "the scorer's writes", 1)
	})

	t.Run("the_store_writers_raise_it_when_they_change_a_person_column", func(t *testing.T) {
		h := mk(t)
		s := h.Tasks
		_, eventID, taskID := h.Seed(t, "ver-writers")
		// step runs one write and checks the version moved by delta, and
		// that a write which returns the row returned the version it stored.
		step := func(what string, delta int64, write func(v int64) (*domain.Task, error)) {
			t.Helper()
			v := version(t, h, taskID)
			returned, err := write(v)
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			got := version(t, h, taskID)
			if got != v+delta {
				t.Errorf("%s: version %d → %d, want %d", what, v, got, v+delta)
			}
			if returned != nil && returned.Version != got {
				t.Errorf("%s: returned version %d, stored %d", what, returned.Version, got)
			}
		}
		landed := func(what string) func(ok bool, err error) (*domain.Task, error) {
			return func(ok bool, err error) (*domain.Task, error) {
				if err == nil && !ok {
					err = fmt.Errorf("%s refused", what)
				}
				return nil, err
			}
		}
		refused := func(what string) func(ok bool, err error) (*domain.Task, error) {
			return func(ok bool, err error) (*domain.Task, error) {
				if err == nil && ok {
					err = fmt.Errorf("%s landed, want refused", what)
				}
				return nil, err
			}
		}
		row := func(task domain.Task, err error) (*domain.Task, error) { return &task, err }

		step("Bump on a queued task", 0, func(int64) (*domain.Task, error) { return row(s.Bump(ctx, h.OrgID, taskID, eventID)) })
		step("SnoozeTask", 1, func(v int64) (*domain.Task, error) {
			return landed("SnoozeTask")(s.SnoozeTask(ctx, h.OrgID, taskID, time.Now().Add(time.Hour), v))
		})
		step("Bump on a snoozed task", 1, func(int64) (*domain.Task, error) { return row(s.Bump(ctx, h.OrgID, taskID, eventID)) })
		step("SnoozeTask again", 1, func(v int64) (*domain.Task, error) {
			return landed("SnoozeTask")(s.SnoozeTask(ctx, h.OrgID, taskID, time.Now().Add(time.Hour), v))
		})
		step("ClearSnooze", 1, func(v int64) (*domain.Task, error) {
			return landed("ClearSnooze")(s.ClearSnooze(ctx, h.OrgID, taskID, v))
		})
		step("ClaimQueuedForUser", 1, func(v int64) (*domain.Task, error) {
			return landed("ClaimQueuedForUser")(s.ClaimQueuedForUser(ctx, h.OrgID, taskID, h.UserID, v))
		})
		step("AdvanceStatusForUser, which changes nothing", 0, func(v int64) (*domain.Task, error) {
			return landed("AdvanceStatusForUser")(s.AdvanceStatusForUser(ctx, h.OrgID, taskID, h.UserID, "in_progress", v))
		})
		step("RequeueTask from a person's claim", 1, func(v int64) (*domain.Task, error) {
			return landed("RequeueTask")(s.RequeueTask(ctx, h.OrgID, taskID, v))
		})
		step("StampAgentClaimIfUnclaimed", 1, func(int64) (*domain.Task, error) {
			return landed("StampAgentClaimIfUnclaimed")(s.StampAgentClaimIfUnclaimed(ctx, h.OrgID, taskID, h.AgentID, ""))
		})
		step("StampAgentClaimIfUnclaimed by the same agent", 0, func(int64) (*domain.Task, error) {
			return refused("StampAgentClaimIfUnclaimed")(s.StampAgentClaimIfUnclaimed(ctx, h.OrgID, taskID, h.AgentID, ""))
		})
		step("TakeoverClaimFromAgent", 1, func(v int64) (*domain.Task, error) {
			return landed("TakeoverClaimFromAgent")(s.TakeoverClaimFromAgent(ctx, h.OrgID, taskID, h.UserID, v))
		})
		step("HandoffAgentClaim", 1, func(v int64) (*domain.Task, error) {
			result, task, err := s.HandoffAgentClaim(ctx, h.OrgID, taskID, h.AgentID, h.UserID, v)
			if err == nil && result != db.HandoffChanged {
				err = fmt.Errorf("HandoffAgentClaim = %v, want HandoffChanged", result)
			}
			return &task, err
		})
		step("HandoffAgentClaim to the agent that holds it", 0, func(v int64) (*domain.Task, error) {
			result, task, err := s.HandoffAgentClaim(ctx, h.OrgID, taskID, h.AgentID, h.UserID, v)
			if err == nil && result != db.HandoffNoOp {
				err = fmt.Errorf("HandoffAgentClaim = %v, want HandoffNoOp", result)
			}
			return &task, err
		})
		step("CloseByUser", 1, func(v int64) (*domain.Task, error) { return row(s.CloseByUser(ctx, h.OrgID, taskID, "dismissed", v)) })
		step("RequeueTask from a close", 1, func(v int64) (*domain.Task, error) {
			return landed("RequeueTask")(s.RequeueTask(ctx, h.OrgID, taskID, v))
		})
		step("RequeueTask of a task already in the queue", 0, func(v int64) (*domain.Task, error) {
			return landed("RequeueTask")(s.RequeueTask(ctx, h.OrgID, taskID, v))
		})
		step("SetStatus", 1, func(int64) (*domain.Task, error) { return row(s.SetStatus(ctx, h.OrgID, taskID, "in_progress")) })
		step("SetStatus to the status it holds", 0, func(int64) (*domain.Task, error) { return row(s.SetStatus(ctx, h.OrgID, taskID, "in_progress")) })
		step("Close", 1, func(int64) (*domain.Task, error) { return row(s.Close(ctx, h.OrgID, taskID, "run_completed", "")) })
	})

	// Each person's write, at a version the task has moved past, is refused
	// with ErrTaskVersion and leaves the row as it was; at the version the
	// task holds, the same call lands, so the version is what refused it.
	t.Run("a_write_naming_a_stale_version_changes_nothing", func(t *testing.T) {
		h := mk(t)
		s := h.Tasks
		claim := func(t *testing.T, taskID string) {
			t.Helper()
			if ok, err := s.ClaimQueuedForUser(ctx, h.OrgID, taskID, h.UserID, version(t, h, taskID)); err != nil || !ok {
				t.Fatalf("ClaimQueuedForUser: ok=%v err=%v", ok, err)
			}
		}
		cases := []struct {
			name  string
			prep  func(t *testing.T, taskID string)
			write func(taskID string, v int64) error
			// lands says the write succeeds at the current version; reassign
			// to the claimant it already has is refused by its own CAS.
			lands bool
		}{
			{name: "CloseByUser", lands: true, write: func(id string, v int64) error {
				_, err := s.CloseByUser(ctx, h.OrgID, id, "done", v)
				return err
			}},
			{name: "SnoozeTask", lands: true, write: func(id string, v int64) error {
				return okOrRefused(s.SnoozeTask(ctx, h.OrgID, id, time.Now().Add(time.Hour), v))
			}},
			{name: "ClearSnooze", lands: true, prep: func(t *testing.T, id string) {
				t.Helper()
				if ok, err := s.SnoozeTask(ctx, h.OrgID, id, time.Now().Add(time.Hour), version(t, h, id)); err != nil || !ok {
					t.Fatalf("SnoozeTask: ok=%v err=%v", ok, err)
				}
			}, write: func(id string, v int64) error {
				return okOrRefused(s.ClearSnooze(ctx, h.OrgID, id, v))
			}},
			{name: "AdvanceStatusForUser", lands: true, prep: claim, write: func(id string, v int64) error {
				return okOrRefused(s.AdvanceStatusForUser(ctx, h.OrgID, id, h.UserID, "in_progress", v))
			}},
			{name: "RequeueTask", lands: true, prep: claim, write: func(id string, v int64) error {
				return okOrRefused(s.RequeueTask(ctx, h.OrgID, id, v))
			}},
			{name: "ClaimQueuedForUser", lands: true, write: func(id string, v int64) error {
				return okOrRefused(s.ClaimQueuedForUser(ctx, h.OrgID, id, h.UserID, v))
			}},
			{name: "TakeoverClaimFromAgent", lands: true, prep: func(t *testing.T, id string) {
				t.Helper()
				if ok, err := s.StampAgentClaimIfUnclaimed(ctx, h.OrgID, id, h.AgentID, ""); err != nil || !ok {
					t.Fatalf("StampAgentClaimIfUnclaimed: ok=%v err=%v", ok, err)
				}
			}, write: func(id string, v int64) error {
				return okOrRefused(s.TakeoverClaimFromAgent(ctx, h.OrgID, id, h.UserID, v))
			}},
			{name: "ReassignClaimToUser", prep: claim, write: func(id string, v int64) error {
				return okOrRefused(s.ReassignClaimToUser(ctx, h.OrgID, id, h.UserID, h.UserID, v))
			}},
			{name: "ReassignClaimToUserSystem", prep: claim, write: func(id string, v int64) error {
				return okOrRefused(s.ReassignClaimToUserSystem(ctx, h.OrgID, id, h.UserID, h.UserID, v))
			}},
			{name: "HandoffAgentClaim", lands: true, write: func(id string, v int64) error {
				result, _, err := s.HandoffAgentClaim(ctx, h.OrgID, id, h.AgentID, h.UserID, v)
				if err == nil && result != db.HandoffChanged {
					return errRefused
				}
				return err
			}},
			// The no-op arm takes no write, and is refused all the same: a
			// re-delegation replaces the task's runs.
			{name: "HandoffAgentClaim to the agent that holds it", lands: true, prep: func(t *testing.T, id string) {
				t.Helper()
				if r, _, err := s.HandoffAgentClaim(ctx, h.OrgID, id, h.AgentID, h.UserID, version(t, h, id)); err != nil || r != db.HandoffChanged {
					t.Fatalf("HandoffAgentClaim: %v %v", r, err)
				}
			}, write: func(id string, v int64) error {
				result, _, err := s.HandoffAgentClaim(ctx, h.OrgID, id, h.AgentID, h.UserID, v)
				if err == nil && result != db.HandoffNoOp {
					return errRefused
				}
				return err
			}},
		}
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, _, taskID := h.Seed(t, fmt.Sprintf("ver-stale-%d", i))
				if c.prep != nil {
					c.prep(t, taskID)
				}
				stale := version(t, h, taskID)
				// Another writer moves the task past the caller's read with a
				// change none of these writes' other guards look at.
				setColumn(t, h, taskID, "close_event_type", "'"+domain.EventGitHubPRMerged+"'")
				before := mustGetTask(t, s, h.OrgID, taskID)
				if err := c.write(taskID, stale); !errors.Is(err, db.ErrTaskVersion) {
					t.Fatalf("at the stale version %d: err = %v, want ErrTaskVersion", stale, err)
				}
				if after := mustGetTask(t, s, h.OrgID, taskID); !reflect.DeepEqual(after, before) {
					t.Errorf("the refused write changed the task\nbefore %+v\nafter  %+v", before, after)
				}
				if !c.lands {
					return
				}
				if err := c.write(taskID, before.Version); err != nil {
					t.Errorf("at the current version %d: %v, want it to land", before.Version, err)
				}
			})
		}
	})
}

var errRefused = errors.New("refused")

// okOrRefused folds an ok-returning write into one error, errRefused for a
// write that matched no row for a reason other than its error.
func okOrRefused(ok bool, err error) error {
	if err == nil && !ok {
		return errRefused
	}
	return err
}
