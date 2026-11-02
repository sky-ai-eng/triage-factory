package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedConversationOnTeam inserts a minimal non-blueprint conversation owned
// by teamID with the given status, via the admin pool. origin='manual'
// sidesteps the conversations_origin_requires_parents CHECK (no
// blueprint_run/task/prompt needed); trigger_type='manual' pairs with a
// non-NULL creator per conversations_creator_matches_trigger_type.
func seedConversationOnTeam(t *testing.T, h *Harness, orgID, creatorID, teamID, status string) string {
	t.Helper()
	var id string
	stored, concluded := dbtest.SeedStatus(status)
	if err := h.AdminDB.QueryRow(`
		INSERT INTO conversations (org_id, creator_user_id, team_id, trigger_type, origin, status, model, completed_at)
		VALUES ($1, $2, $3, 'manual', 'manual', $4, 'm', CASE WHEN $5 THEN now() END) RETURNING id
	`, orgID, creatorID, teamID, stored, concluded).Scan(&id); err != nil {
		t.Fatalf("seed %s conversation on team %s: %v", status, teamID, err)
	}
	return id
}

// TestConversationStore_Postgres_ActiveIDsForTeamSystem pins the team-archive
// force-stop enumeration on Postgres (TFAC-448): only active conversations
// owned by the queried team are returned — terminal conversations are
// excluded, and a sibling team's active conversation is not picked up (the
// team_id WHERE clause).
func TestConversationStore_Postgres_ActiveIDsForTeamSystem(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")

	running := seedConversationOnTeam(t, h, orgA, alice, teamA, "running")
	open := seedConversationOnTeam(t, h, orgA, alice, teamA, "open")
	seedConversationOnTeam(t, h, orgA, alice, teamA, dbtest.SeedConcluded)
	seedConversationOnTeam(t, h, orgA, alice, teamB, "running") // sibling team — excluded

	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)
	ids, err := stores.Conversations.ActiveIDsForTeamSystem(context.Background(), orgA, teamA)
	if err != nil {
		t.Fatalf("ActiveIDsForTeamSystem: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[running] || !got[open] {
		t.Fatalf("ActiveIDsForTeamSystem = %v; want exactly teamA's running + open conversations (%s, %s)", ids, running, open)
	}
}

// TestRLS_ArchivedTeamBlocksWrites is the row-level backstop for team archive:
// once a team is archived (teams.deleted_at IS NOT NULL), tf.user_can_write_team
// returns false even for an admin, so every team-scoped write policy keyed on it
// rejects the write. teams_select still resolves the archived team so the
// restore path can find it, while tf.user_in_team, the read gate for the team's
// own rows, answers false. Restoring re-enables writes. This is what makes
// "block all further writes" hold for the task-scoped mutations whose team isn't
// in the URL.
func TestRLS_ArchivedTeamBlocksWrites(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")

	// Seed a team prompt via the admin pool (RLS-bypassed fixture) so there's an
	// existing row to (fail to) UPDATE once the team is archived.
	var promptID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
		VALUES ($1, $2, $3, 'team-prompt', 'mission', 'user') RETURNING id
	`, orgA, alice, teamA).Scan(&promptID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	// Baseline: alice (admin) can write while the team is active.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var canWrite bool
		if err := tx.QueryRow(`SELECT tf.user_can_write_team($1::uuid)`, teamA).Scan(&canWrite); err != nil {
			return err
		}
		if !canWrite {
			t.Error("active team: user_can_write_team = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatalf("baseline check: %v", err)
	}

	// Archive the team.
	MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, teamA)

	// The write helper now returns false even for the admin, membership grants
	// no read of the team's rows, and the team itself is still visible (the
	// restore path depends on it).
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var canWrite, inTeam, visible bool
		if err := tx.QueryRow(
			`SELECT tf.user_can_write_team($1::uuid), tf.user_in_team($1::uuid),
			        EXISTS (SELECT 1 FROM teams WHERE id = $1::uuid)`,
			teamA,
		).Scan(&canWrite, &inTeam, &visible); err != nil {
			return err
		}
		if canWrite {
			t.Error("archived team: user_can_write_team = true, want false")
		}
		if inTeam {
			t.Error("archived team: user_in_team = true, want false (membership in an archived team grants no read)")
		}
		if !visible {
			t.Error("archived team: not visible to teams_select, want visible (restore needs it)")
		}
		return nil
	}); err != nil {
		t.Fatalf("archived check: %v", err)
	}

	// A team-scoped write on the archived team trips RLS.
	insertErr := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
			VALUES ($1, $2, $3, 'archived-prompt', 'mission', 'user')
		`, orgA, alice, teamA)
		return e
	})
	AssertRLSViolation(t, insertErr)

	// UPDATE of the existing prompt affects zero rows — the row isn't write-visible.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE prompts SET body = 'tampered' WHERE id = $1`, promptID)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("archived team UPDATE affected %d rows, want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("archived update: %v", err)
	}

	// Restore re-enables writes.
	MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = NULL WHERE id = $1`, teamA)
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
			VALUES ($1, $2, $3, 'restored-prompt', 'mission', 'user')
		`, orgA, alice, teamA)
		return e
	}); err != nil {
		t.Fatalf("restored team INSERT should succeed, got: %v", err)
	}

	// The original prompt survived the blocked writes untouched.
	var body string
	if err := h.AdminDB.QueryRow(`SELECT body FROM prompts WHERE id = $1`, promptID).Scan(&body); err != nil {
		t.Fatalf("re-read prompt: %v", err)
	}
	if body != "mission" {
		t.Errorf("prompt body = %q, want %q (archived-team write must not have landed)", body, "mission")
	}
}

// TestRLS_ArchivedTeamHidesItsRows pins the read half of team archive: for a
// person's request, membership in an archived team grants no read of its
// blueprints, prompts, event handlers or team-visibility tasks, and neither
// does having written them — the creator arm of the blueprint, prompt and
// event-handler policies is cut by tf.team_active. Restoring the team brings
// every row back, because archiving keeps the memberships.
func TestRLS_ArchivedTeamHidesItsRows(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgID, "team-b")
	MustExec(t, h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, alice, teamB)

	type teamRows struct{ blueprint, prompt, handler, task string }
	seed := func(teamID, suffix string) teamRows {
		var r teamRows
		if err := h.AdminDB.QueryRow(`
			INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
			VALUES ($1, $2, $3, $4, 'user') RETURNING id
		`, orgID, alice, teamID, "bp-"+suffix).Scan(&r.blueprint); err != nil {
			t.Fatalf("seed blueprint %s: %v", suffix, err)
		}
		if err := h.AdminDB.QueryRow(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
			VALUES ($1, $2, $3, $4, 'mission', 'user') RETURNING id
		`, orgID, alice, teamID, "prompt-"+suffix).Scan(&r.prompt); err != nil {
			t.Fatalf("seed prompt %s: %v", suffix, err)
		}
		if err := h.AdminDB.QueryRow(`
			INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, name, default_priority, sort_order)
			VALUES ($1, $2, $3, 'rule', 'github:pr:opened', $4, 0.5, 0) RETURNING id
		`, orgID, alice, teamID, "rule-"+suffix).Scan(&r.handler); err != nil {
			t.Fatalf("seed rule %s: %v", suffix, err)
		}
		r.task = seedMarketplaceStatsTask(t, h, orgID, alice, teamID)
		return r
	}
	a := seed(teamA, "a")
	b := seed(teamB, "b")

	// visible reads every list as alice, in one claims-carrying transaction.
	visible := func() map[string]bool {
		t.Helper()
		all := map[string]bool{}
		if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
			st := pgstore.NewForTx(tx, SecretKey)
			bps, _, err := st.Blueprints.List(ctx, orgID, db.BlueprintListFilter{}, db.ListOpts{Limit: 200})
			if err != nil {
				return fmt.Errorf("blueprints: %w", err)
			}
			for _, x := range bps {
				all[x.ID] = true
			}
			prompts, _, err := st.Prompts.List(ctx, orgID, "", db.ListOpts{Limit: 200})
			if err != nil {
				return fmt.Errorf("prompts: %w", err)
			}
			for _, x := range prompts {
				all[x.ID] = true
			}
			handlers, _, err := st.EventHandlers.List(ctx, orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
			if err != nil {
				return fmt.Errorf("event handlers: %w", err)
			}
			for _, x := range handlers {
				all[x.ID] = true
			}
			tasks, _, err := st.Tasks.List(ctx, orgID, db.TaskListFilter{Statuses: []string{"queued"}}, db.ListOpts{Limit: 200})
			if err != nil {
				return fmt.Errorf("tasks: %w", err)
			}
			for _, x := range tasks {
				all[x.ID] = true
			}
			return nil
		}); err != nil {
			t.Fatalf("list as alice: %v", err)
		}
		return all
	}
	expect := func(when string, rows teamRows, want bool) {
		t.Helper()
		all := visible()
		for kind, id := range map[string]string{"blueprint": rows.blueprint, "prompt": rows.prompt, "event handler": rows.handler, "task": rows.task} {
			if all[id] != want {
				t.Errorf("%s: %s %s visible = %v, want %v", when, kind, id, all[id], want)
			}
		}
	}

	expect("both active", a, true)
	expect("both active", b, true)

	MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, teamB)
	expect("B archived", a, true)
	expect("B archived", b, false)

	MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = NULL WHERE id = $1`, teamB)
	expect("B restored", a, true)
	expect("B restored", b, true)
}
