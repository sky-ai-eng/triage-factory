package pgtest

import (
	"context"
	"database/sql"
	"testing"

	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedRunOnTeam inserts a minimal non-blueprint run owned by teamID with the
// given status, via the admin pool. origin='manual' sidesteps the
// conversations_origin_requires_parents CHECK (no blueprint_run/task/prompt needed);
// trigger_type='manual' pairs with a non-NULL creator per
// conversations_creator_matches_trigger_type.
func seedRunOnTeam(t *testing.T, h *Harness, orgID, creatorID, teamID, status string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO conversations (org_id, creator_user_id, team_id, trigger_type, origin, status, model)
		VALUES ($1, $2, $3, 'manual', 'manual', $4, 'm') RETURNING id
	`, orgID, creatorID, teamID, status).Scan(&id); err != nil {
		t.Fatalf("seed %s run on team %s: %v", status, teamID, err)
	}
	return id
}

// TestConversationStore_Postgres_ActiveIDsForTeamSystem pins the team-archive
// force-stop enumeration on Postgres (TFAC-448): only active runs owned by the
// queried team are returned — terminal runs are excluded, and
// a sibling team's active run is not picked up (the team_id WHERE clause).
func TestConversationStore_Postgres_ActiveIDsForTeamSystem(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")

	running := seedRunOnTeam(t, h, orgA, alice, teamA, "running")
	open := seedRunOnTeam(t, h, orgA, alice, teamA, "open")
	seedRunOnTeam(t, h, orgA, alice, teamA, "completed")
	seedRunOnTeam(t, h, orgA, alice, teamB, "running") // sibling team — excluded

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
		t.Fatalf("ActiveIDsForTeamSystem = %v; want exactly teamA's running + open runs (%s, %s)", ids, running, open)
	}
}

// TestRLS_ArchivedTeamBlocksWrites is the row-level backstop for team archive
// (TFAC-448): once a team is archived (teams.deleted_at IS NOT NULL),
// tf.user_can_write_team returns false even for an admin, so every team-scoped
// write policy keyed on it rejects the write — while reads (teams_select,
// user_in_team) still resolve the archived team so the restore path can find it.
// Restoring re-enables writes. This is what makes "block all further writes"
// hold for the task-scoped mutations whose team isn't in the URL.
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

	// The write helper now returns false even for the admin, and reads still see
	// the team (the restore path depends on it).
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
		if !inTeam {
			t.Error("archived team: user_in_team = false, want true (reads must still resolve it)")
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
