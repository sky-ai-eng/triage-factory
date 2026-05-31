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
			all, e := store.Queued(ctx, orgID, nil)
			if e != nil {
				return fmt.Errorf("Queued(union): %w", e)
			}
			if !containsTask(all, taskA) || !containsTask(all, taskB) {
				t.Errorf("union Queued missing a task: got %s, want both %s and %s", taskIDs(all), taskA, taskB)
			}

			// Filter to A → only A's task; B's row is hidden.
			onlyA, e := store.Queued(ctx, orgID, []string{teamA})
			if e != nil {
				return fmt.Errorf("Queued(A): %w", e)
			}
			if !containsTask(onlyA, taskA) || containsTask(onlyA, taskB) {
				t.Errorf("Queued(team A) = %s; want only %s", taskIDs(onlyA), taskA)
			}

			// Filter to B → only B's task.
			onlyB, e := store.Queued(ctx, orgID, []string{teamB})
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

	t.Run("pending_refs_respect_team_filter", func(t *testing.T) {
		// The factory station drawer attaches pending task refs per
		// entity; that read must honor the same team filter as the entity
		// belt, or a drawer for a team-A-filtered entity would still
		// surface (and let you delegate) team B's task refs. (Codex P2 ⑤)
		var entA, entB string
		if err := h.AdminDB.QueryRow(`SELECT entity_id::text FROM tasks WHERE id = $1`, taskA).Scan(&entA); err != nil {
			t.Fatalf("entity for taskA: %v", err)
		}
		if err := h.AdminDB.QueryRow(`SELECT entity_id::text FROM tasks WHERE id = $1`, taskB).Scan(&entB); err != nil {
			t.Fatalf("entity for taskB: %v", err)
		}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).Tasks
			// Unfiltered: refs for both entities surface.
			all, e := store.ListActiveRefsForEntities(ctx, orgID, []string{entA, entB}, nil)
			if e != nil {
				return e
			}
			if !hasRef(all, taskA) || !hasRef(all, taskB) {
				t.Errorf("unfiltered refs missing one: %+v", all)
			}
			// Filtered to A: only taskA's ref, even though entB was asked for.
			onlyA, e := store.ListActiveRefsForEntities(ctx, orgID, []string{entA, entB}, []string{teamA})
			if e != nil {
				return e
			}
			if !hasRef(onlyA, taskA) || hasRef(onlyA, taskB) {
				t.Errorf("filter=teamA refs = %+v; want only taskA's ref", onlyA)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("pending refs filter: %v", err)
		}
	})

	t.Run("sticky_default_round_trips", func(t *testing.T) {
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			users := pgstore.NewForTx(tx).Users
			if e := users.SetLastActingTeam(ctx, userID, teamB); e != nil {
				return fmt.Errorf("set: %w", e)
			}
			got, e := users.GetLastActingTeam(ctx, userID)
			if e != nil {
				return fmt.Errorf("get: %w", e)
			}
			if got != teamB {
				t.Errorf("preferred team = %q; want %q", got, teamB)
			}
			// Clearing resets to NULL.
			if e := users.SetLastActingTeam(ctx, userID, ""); e != nil {
				return fmt.Errorf("clear: %w", e)
			}
			got, e = users.GetLastActingTeam(ctx, userID)
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

	t.Run("prompts_list_scoped_to_active_team", func(t *testing.T) {
		// The single-team prompts page narrows List to one team. The founder
		// is a member of A and B, so the union shows both, but List(teamA)
		// must show only team A's prompts plus org-visible (system) ones —
		// never team B's. This is what closes the cross-team trigger→prompt
		// hole structurally: team B's prompts simply aren't on team A's canvas.
		mkPrompt := func(team, name string) string {
			id := "p_scope_" + uuid.New().String()
			if e := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
				return pgstore.NewForTx(tx).Prompts.Create(ctx, orgID, team, domain.Prompt{
					ID: id, Name: name, Body: "b", Source: "user", Kind: "leaf",
				})
			}); e != nil {
				t.Fatalf("create prompt %s: %v", name, e)
			}
			return id
		}
		pA := mkPrompt(teamA, "alpha-only")
		pB := mkPrompt(teamB, "beta-only")
		// An org-visible (system) prompt is visible to every team's view.
		pOrg := "p_org_" + uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO prompts (id, org_id, creator_user_id, name, body, source, visibility, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, NULL, 'shared', 'b', 'system', 'org', 0, false, now(), now())`, pOrg, orgID)

		promptIDs := func(ps []domain.Prompt) map[string]bool {
			m := make(map[string]bool, len(ps))
			for _, p := range ps {
				m[p.ID] = true
			}
			return m
		}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).Prompts
			a, e := store.List(ctx, orgID, teamA)
			if e != nil {
				return e
			}
			if ga := promptIDs(a); !ga[pA] || !ga[pOrg] || ga[pB] {
				t.Errorf("List(teamA): want {A, org} without B; got A=%v org=%v B=%v", ga[pA], ga[pOrg], ga[pB])
			}
			b, e := store.List(ctx, orgID, teamB)
			if e != nil {
				return e
			}
			if gb := promptIDs(b); !gb[pB] || !gb[pOrg] || gb[pA] {
				t.Errorf("List(teamB): want {B, org} without A; got B=%v org=%v A=%v", gb[pB], gb[pOrg], gb[pA])
			}
			all, e := store.List(ctx, orgID, "")
			if e != nil {
				return e
			}
			if gall := promptIDs(all); !gall[pA] || !gall[pB] || !gall[pOrg] {
				t.Errorf("List(unfiltered): want all three; got A=%v B=%v org=%v", gall[pA], gall[pB], gall[pOrg])
			}
			return nil
		})
		if err != nil {
			t.Fatalf("prompts list scope: %v", err)
		}
	})

	t.Run("event_handlers_list_scoped_to_active_team", func(t *testing.T) {
		// Mirror of the prompts narrowing for the binding graph's handlers
		// (exercised with rules — same team_id predicate, no prompt FK
		// needed). List(teamA) shows team A's handlers (plus org-visible
		// team_id-NULL ones), never team B's.
		priority := 0.5
		sortOrder := 0
		mkRule := func(team, name string) string {
			id := uuid.New().String()
			if e := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
				return pgstore.NewForTx(tx).EventHandlers.Create(ctx, orgID, team, domain.EventHandler{
					ID:              id,
					Kind:            domain.EventHandlerKindRule,
					EventType:       domain.EventGitHubPRCICheckFailed,
					Name:            name,
					DefaultPriority: &priority,
					SortOrder:       &sortOrder,
					Enabled:         true,
					Source:          domain.EventHandlerSourceUser,
				})
			}); e != nil {
				t.Fatalf("create rule %s: %v", name, e)
			}
			return id
		}
		rA := mkRule(teamA, "rule-alpha")
		rB := mkRule(teamB, "rule-beta")

		handlerIDs := func(hs []domain.EventHandler) map[string]bool {
			m := make(map[string]bool, len(hs))
			for _, x := range hs {
				m[x.ID] = true
			}
			return m
		}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).EventHandlers
			a, e := store.List(ctx, orgID, domain.EventHandlerKindRule, teamA)
			if e != nil {
				return e
			}
			if ga := handlerIDs(a); !ga[rA] || ga[rB] {
				t.Errorf("List(rule, teamA): want A without B; got A=%v B=%v", ga[rA], ga[rB])
			}
			b, e := store.List(ctx, orgID, domain.EventHandlerKindRule, teamB)
			if e != nil {
				return e
			}
			if gb := handlerIDs(b); !gb[rB] || gb[rA] {
				t.Errorf("List(rule, teamB): want B without A; got B=%v A=%v", gb[rB], gb[rA])
			}
			return nil
		})
		if err != nil {
			t.Fatalf("event handlers list scope: %v", err)
		}
	})

	t.Run("forged_team_id_cannot_leak_via_other_team", func(t *testing.T) {
		// A task visible to the founder's team A AND to an outside team C
		// the founder is NOT a member of. RLS admits the row (via A), but
		// a forged ?team_id=C narrow must NOT match it — the filter is
		// bounded to the viewer's teams via tf.user_in_team. (Codex P2)
		teamC := pgtest.SeedTeam(t, h, orgID, "gamma-outside") // founder NOT enrolled
		shared := seedMultiTeamTask(t, h, orgID, userID, teamA, "shared-ac")
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, shared, teamC)

		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).Tasks
			// Sanity: visible in the union (via A).
			all, e := store.Queued(ctx, orgID, nil)
			if e != nil {
				return e
			}
			if !containsTask(all, shared) {
				t.Errorf("union Queued missing the A/C task %s", shared)
			}
			// Forged narrow to C (founder isn't in C) must NOT surface it.
			forged, e := store.Queued(ctx, orgID, []string{teamC})
			if e != nil {
				return e
			}
			if containsTask(forged, shared) {
				t.Errorf("Queued(team C) leaked task %s; the caller isn't a member of C", shared)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("forged filter: %v", err)
		}
	})

	t.Run("claimed_task_matches_only_owning_team", func(t *testing.T) {
		// A task owned by team A but carrying a stale task_teams
		// visibility row for team B, then claimed by the user. Claim
		// consolidates to team_id=A without draining the B visibility row;
		// filtering to B must NOT show it, because task_teams only widens
		// visibility while unclaimed. (Codex P2)
		claimed := seedMultiTeamTask(t, h, orgID, userID, teamA, "claimed-a")
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, claimed, teamB)
		pgtest.MustExec(t, h.AdminDB,
			`UPDATE tasks SET claimed_by_user_id = $2 WHERE id = $1`, claimed, userID)

		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			store := pgstore.NewForTx(tx).Tasks
			// Owning team A still matches.
			onA, e := store.ByStatus(ctx, orgID, "claimed", []string{teamA})
			if e != nil {
				return e
			}
			if !containsTask(onA, claimed) {
				t.Errorf("ByStatus(claimed, team A) missing the A-owned task %s", claimed)
			}
			// Stale visibility team B must NOT match a claimed task.
			onB, e := store.ByStatus(ctx, orgID, "claimed", []string{teamB})
			if e != nil {
				return e
			}
			if containsTask(onB, claimed) {
				t.Errorf("ByStatus(claimed, team B) matched task %s via a stale task_teams row; claimed tasks consolidate to team_id", claimed)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("claimed scoping: %v", err)
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

	t.Run("resolve_claim_team_for_visible_nonowner", func(t *testing.T) {
		// The ① regression case: an unclaimed task OWNED by a team the
		// caller is NOT in (teamD), but VISIBLE to the caller's team B via
		// a task_teams row. HandoffAgentClaim would consolidate the claim
		// onto B, so the bot-enablement gate must check B — not the owner
		// teamD, whose team_agents row RLS hides from the caller (which
		// would wrongly report the bot disabled and reject the delegate).
		// ResolveClaimTeam is what the handlers consult for that gate.
		teamD := pgtest.SeedTeam(t, h, orgID, "delta-owner") // founder NOT in D
		task := seedMultiTeamTask(t, h, orgID, userID, teamD, "owned-by-d")
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, task, teamB)

		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			got, e := pgstore.NewForTx(tx).Tasks.ResolveClaimTeam(ctx, orgID, task, userID)
			if e != nil {
				return e
			}
			if got != teamB {
				t.Errorf("ResolveClaimTeam = %q; want the caller's visible team B %q (not owner D %q)", got, teamB, teamD)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("resolve claim team: %v", err)
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

func hasRef(refs []domain.PendingTaskRef, taskID string) bool {
	for _, r := range refs {
		if r.ID == taskID {
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
