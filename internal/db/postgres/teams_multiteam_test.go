package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedMultiTeamTask inserts an entity + primary event + an unclaimed
// queued task owned by teamID (with its task_teams visibility row) under
// the admin pool. Returns the task id. The task is visible (tasks_select)
// to any user in teamID because it's unclaimed and task_teams carries the
// team — the same shape the router produces.
func seedMultiTeamTask(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, suffix string) string {
	t.Helper()
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, now())
	`, entityID, orgID, "mt-"+suffix+"-"+entityID[:8], "Multi-team "+suffix, "https://example/"+suffix); err != nil {
		t.Fatalf("seed entity %s: %v", suffix, err)
	}
	eventID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("seed event %s: %v", suffix, err)
	}
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5, now())
	`, taskID, orgID, userID, teamID, entityID, eventID); err != nil {
		t.Fatalf("seed task %s: %v", suffix, err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)
	`, taskID, teamID); err != nil {
		t.Fatalf("seed task_teams %s: %v", suffix, err)
	}
	return taskID
}

// TestMultiTeam_Postgres exercises the multi-team read filter +
// sticky default + team create end-to-end through the app pool (RLS
// active), the configuration the acceptance criteria call for: a user in
// teams A+B sees the union by default and one team when filtered, the
// sticky default round-trips, and the org-admin team create enrolls the
// creator. Skips cleanly without Docker.
func TestMultiTeam_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// Founder is org owner + member of the default team (team A).
	orgID, userID, teamA := pgtest.SeedOrgWithUser(t, h, "multiteam")
	// A second team in the same org, with the founder enrolled — now the
	// user belongs to ≥2 teams.
	teamB := pgtest.SeedTeam(t, h, orgID, "beta")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, userID, teamB)

	// One queued task per team, both visible to the founder.
	taskA := seedMultiTeamTask(t, h, orgID, userID, teamA, "alpha")
	taskB := seedMultiTeamTask(t, h, orgID, userID, teamB, "beta")

	t.Run("list_for_user_returns_both_teams", func(t *testing.T) {
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			teams, e := pgstore.NewForTx(tx).Teams.ListForUser(ctx, orgID)
			if e != nil {
				return e
			}
			if len(teams) != 2 {
				t.Errorf("ListForUser returned %d teams, want 2 (A+B)", len(teams))
			}
			got := map[string]bool{}
			for _, tm := range teams {
				got[tm.ID] = true
			}
			if !got[teamA] || !got[teamB] {
				t.Errorf("ListForUser = %+v; want both %s and %s", teams, teamA, teamB)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("list_for_user: %v", err)
		}
	})

	t.Run("union_by_default_one_team_when_filtered", func(t *testing.T) {
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).Tasks

			// No filter → the union of the viewer's teams (both tasks).
			all, e := store.Queued(ctx, orgID, "")
			if e != nil {
				return fmt.Errorf("Queued(union): %w", e)
			}
			if !containsTask(all, taskA) || !containsTask(all, taskB) {
				t.Errorf("union Queued missing a task: got %s, want both %s and %s", taskIDs(all), taskA, taskB)
			}

			// Filter to A → only A's task; B's row is hidden.
			onlyA, e := store.Queued(ctx, orgID, teamA)
			if e != nil {
				return fmt.Errorf("Queued(A): %w", e)
			}
			if !containsTask(onlyA, taskA) || containsTask(onlyA, taskB) {
				t.Errorf("Queued(team A) = %s; want only %s", taskIDs(onlyA), taskA)
			}

			// Filter to B → only B's task.
			onlyB, e := store.Queued(ctx, orgID, teamB)
			if e != nil {
				return fmt.Errorf("Queued(B): %w", e)
			}
			if !containsTask(onlyB, taskB) || containsTask(onlyB, taskA) {
				t.Errorf("Queued(team B) = %s; want only %s", taskIDs(onlyB), taskB)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("union/filter: %v", err)
		}
	})

	t.Run("sticky_default_round_trips", func(t *testing.T) {
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			users := pgstore.NewForTx(tx).Users
			if e := users.SetPreferredTeam(ctx, userID, teamB); e != nil {
				return fmt.Errorf("set: %w", e)
			}
			got, e := users.GetPreferredTeam(ctx, userID)
			if e != nil {
				return fmt.Errorf("get: %w", e)
			}
			if got != teamB {
				t.Errorf("preferred team = %q; want %q", got, teamB)
			}
			// Clearing resets to NULL.
			if e := users.SetPreferredTeam(ctx, userID, ""); e != nil {
				return fmt.Errorf("clear: %w", e)
			}
			got, e = users.GetPreferredTeam(ctx, userID)
			if e != nil {
				return fmt.Errorf("get after clear: %w", e)
			}
			if got != "" {
				t.Errorf("preferred team after clear = %q; want empty", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("sticky default: %v", err)
		}
	})

	t.Run("write_lands_on_picked_team", func(t *testing.T) {
		// The acceptance "a write lands on the picked team": a team-stamped
		// create (here a prompt) under the app pool persists the supplied
		// team, not the org default. Postgres binds team_id directly (the
		// SQLite store pins the sole local team, so this is multi-mode
		// behavior). The resolver that *chooses* teamB is unit-tested in the
		// server package; here we pin that the store honors it.
		promptID := "p_pick_" + uuid.New().String()
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx).Prompts.Create(ctx, orgID, teamB, domain.Prompt{
				ID:     promptID,
				Name:   "Scoped",
				Body:   "do the thing",
				Source: "user",
				Kind:   "leaf",
			})
		})
		if err != nil {
			t.Fatalf("create prompt on team B: %v", err)
		}
		var landed string
		if err := h.AdminDB.QueryRow(
			`SELECT team_id::text FROM prompts WHERE id = $1`, promptID,
		).Scan(&landed); err != nil {
			t.Fatalf("read prompt team: %v", err)
		}
		if landed != teamB {
			t.Errorf("prompt landed on team %q; want the picked team %q (B)", landed, teamB)
		}
	})

	t.Run("create_enrolls_creator", func(t *testing.T) {
		var newTeamID string
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			created, e := pgstore.NewForTx(tx).Teams.Create(ctx, orgID, "Gamma", "gamma", userID)
			if e != nil {
				return fmt.Errorf("create: %w", e)
			}
			newTeamID = created.ID
			// The creator is now a member, so ListForUser sees three teams.
			teams, e := pgstore.NewForTx(tx).Teams.ListForUser(ctx, orgID)
			if e != nil {
				return fmt.Errorf("list after create: %w", e)
			}
			if len(teams) != 3 {
				t.Errorf("after create, ListForUser = %d teams, want 3", len(teams))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if newTeamID == "" {
			t.Fatal("create returned an empty team id")
		}
	})
}

func containsTask(tasks []domain.Task, id string) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

func taskIDs(tasks []domain.Task) string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return fmt.Sprintf("%v", ids)
}
