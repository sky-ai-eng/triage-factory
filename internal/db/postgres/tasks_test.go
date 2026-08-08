package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskStore_Postgres runs the shared conformance suite against
// the Postgres TaskStore impl. Each subtest gets a fresh org + user +
// agent + entity/event/task fixture seeded through the harness's
// admin connection (BYPASSRLS), then exercises the store via the app
// pool. Skips cleanly when Docker isn't available — pgtest.Shared
// handles that.
func TestTaskStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the conformance suite's
	// FindOrCreate INSERT (which COALESCEs tf.current_user_id() →
	// org-owner) can resolve creator_user_id without a JWT-claims tx
	// — supabase_admin owns the tf.* functions and can EXECUTE them
	// even without the per-request tf_app grant. RLS bypass on
	// AdminDB is fine for behavior conformance; the cross-org leakage
	// test below exercises the defense-in-depth org_id filter
	// directly. Same pattern as TestSwipeStore_Postgres.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskStoreConformance(t, func(t *testing.T) (db.TaskStore, string, string, string, string, dbtest.TaskSeeder, dbtest.TeamSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgOrgUserAgent(t, h)
		// The org's default team — seeded by seedPgDefaultTeam inside
		// seedPgOrgUserAgent — is the teamID the conformance subtests
		// thread into FindOrCreate. firstTeamForOrg picks it up via
		// the same created_at ordering production used to do
		// implicitly.
		teamID := firstTeamForOrg(t, h, orgID)
		seeder := func(t *testing.T, suffix string) (entityID, eventID, taskID string) {
			t.Helper()
			return seedPgTaskChain(t, h.AdminDB, orgID, userID, suffix)
		}
		// The per-team conformance subtest needs a second team
		// inside the same org so the partial unique index fans out
		// instead of collapsing. Seed the team + a membership for
		// the harness's user so memberships-aware code paths stay
		// happy (RLS-bypassing AdminDB doesn't strictly need the
		// membership, but the canonical shape mirrors production).
		teamSeeder := func(t *testing.T, suffix string) string {
			t.Helper()
			return seedPgDefaultTeam(t, h, orgID, userID)
		}
		return stores.Tasks, orgID, teamID, agentID, userID, seeder, teamSeeder
	})
}

// seedPgOrgUserAgent builds the (auth.user, public.user, org,
// membership, default team, agent, team_agents-membership) graph the
// claim methods FK into. Mirrors the shape ScoreStore tests use for
// org/user, plus the agent half.
func seedPgOrgUserAgent(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("conformance-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)

	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Conformance Org "+orgID[:8], "conf-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	// Agents row backing the claim methods. Created on admin (bootstrap
	// has no JWT claims).
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Conformance Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// seedPgTaskChain inserts a fresh entity + event + task chain. Uses
// the suffix to keep source_id unique. Returns the IDs so the
// conformance harness can address them in subsequent calls.
func seedPgTaskChain(t *testing.T, conn *sql.DB, orgID, userID, suffix string) (entityID, eventID, taskID string) {
	t.Helper()
	now := time.Now().UTC()
	entityID = uuid.New().String()
	taskID = uuid.New().String()
	eventID = uuid.New().String()
	sourceID := fmt.Sprintf("conformance-%s-%d", suffix, now.UnixNano())
	// EventGitHubPRCICheckFailed is in the seeded events_catalog; using
	// it keeps the catalog FK happy without re-seeding inline. The
	// dashboard / "passed" variants are also catalogued in case future
	// expansions of the conformance suite need them.
	eventType := "github:pr:ci_check_failed"

	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
	`, entityID, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, eventID, orgID, entityID, eventType, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
		                   status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, $7)
	`, taskID, orgID, userID, entityID, eventType, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return entityID, eventID, taskID
}

// TestTaskStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// guarantee: even with the org_id filter as the only line of defense
// (AdminDB bypasses RLS), org A's queries can't see org B's tasks.
// In production the RLS policies add a second layer; this test
// validates the WHERE-clause filter on its own so a regression
// there can't silently rely on RLS to compensate.
func TestTaskStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := seedPgOrgUserAgent(t, h)
	orgB, userB, _ := seedPgOrgUserAgent(t, h)
	entA, _, taskA := seedPgTaskChain(t, h.AdminDB, orgA, userA, "cross-A")
	_, _, taskB := seedPgTaskChain(t, h.AdminDB, orgB, userB, "cross-B")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// Org A's view shouldn't see B's task.
	if task, err := stores.Tasks.Get(ctx, orgA, taskB); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if task != nil {
		t.Errorf("orgA Get returned orgB task %s; defense-in-depth filter leaked", taskB)
	}
	// Symmetric.
	if task, err := stores.Tasks.Get(ctx, orgB, taskA); err != nil {
		t.Fatalf("Get cross-org reverse: %v", err)
	} else if task != nil {
		t.Errorf("orgB Get returned orgA task %s", taskA)
	}

	// ListActiveRefsForEntities scoped to orgA, looking at entA, should
	// see exactly one row. Asking the same with orgB and entA must
	// return zero (entA isn't visible from orgB).
	refs, err := stores.Tasks.ListActiveRefsForEntities(ctx, orgA, []string{entA}, nil)
	if err != nil {
		t.Fatalf("ListActiveRefsForEntities orgA: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != taskA {
		t.Errorf("orgA refs = %+v, want exactly taskA", refs)
	}
	refs, err = stores.Tasks.ListActiveRefsForEntities(ctx, orgB, []string{entA}, nil)
	if err != nil {
		t.Fatalf("ListActiveRefsForEntities orgB→entA: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("orgB looking at entA returned %d refs; want 0 (cross-org leak)", len(refs))
	}
}

// TestTaskStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for tasks. Where TestTaskStore_Postgres_CrossOrgLeakage above
// wires both pools against AdminDB to prove the defense-in-depth
// WHERE-clause filter is intact, this test runs the store through the
// app pool under tf_app with real JWT claims so the actual
// tasks_select / tasks_insert policies are exercised. Same-org reads
// succeed; cross-org reads are silently filtered (USING); cross-org
// FindOrCreate raises 42501 from the INSERT WITH CHECK.
func TestTaskStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, _ := pgtest.SeedOrgWithUser(t, h, "bob")

	// Seed a task in orgA via admin so the row exists. Whether bob
	// (claims orgB) can see or mutate it is the question.
	_, _, taskA := seedPgTaskChain(t, h.AdminDB, orgA, alice, "rls")
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			task, err := pgstore.NewForTx(tx, pgtest.SecretKey).Tasks.Get(ctx, orgA, taskA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if task == nil {
				t.Errorf("alice Get(orgA, taskA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			task, err := pgstore.NewForTx(tx, pgtest.SecretKey).Tasks.Get(ctx, orgA, taskA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if task != nil {
				t.Errorf("bob Get(orgA, taskA) returned %+v; RLS USING filter leaked orgA's task to orgB", task)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// Seed a fresh entity+event in orgA so the FK chain is satisfied
		// — the rejection we want is from tasks_insert WITH CHECK
		// (bob's claims point at orgB; the row would land with
		// org_id=orgA), not from a missing FK target. Fetch orgA's team
		// via admin to fill the required teamID arg; in production a
		// user wouldn't know the cross-org team, but we're proving the
		// policy holds even if they did.
		entityID, eventID := seedPgEntityEvent(t, h.AdminDB, orgA, "rls-write")
		orgATeam := firstTeamForOrg(t, h, orgA)
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, _, e := pgstore.NewForTx(tx, pgtest.SecretKey).Tasks.FindOrCreate(
				ctx, orgA, orgATeam,
				entityID, "github:pr:ci_check_failed", "", eventID, 0.5,
			)
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// TestTaskStore_Postgres_OrgHandlerSentinel pins the fix for the case
// where org-visible event handlers (visibility='org', team_id NULL)
// route through handlerTeamID to runmode.LocalDefaultTeamID. The
// Postgres FindOrCreate used to reject that sentinel; this test
// verifies it now resolves to the org's canonical team and creates the
// task successfully.
func TestTaskStore_Postgres_OrgHandlerSentinel(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := seedPgOrgUserAgent(t, h)
	entityID, eventID := seedPgEntityEvent(t, h.AdminDB, orgID, "sentinel")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	task, created, err := stores.Tasks.FindOrCreate(
		ctx, orgID, runmode.LocalDefaultTeamID,
		entityID, "github:pr:ci_check_failed", "", eventID, 0.5,
	)
	if err != nil {
		t.Fatalf("FindOrCreate with LocalDefaultTeamID sentinel: %v", err)
	}
	if !created {
		t.Error("expected task to be created, got created=false")
	}
	if task == nil {
		t.Fatal("FindOrCreate returned nil task")
	}

	// Idempotent: second call with the same sentinel should find the
	// existing task, not create a second one.
	task2, created2, err := stores.Tasks.FindOrCreate(
		ctx, orgID, runmode.LocalDefaultTeamID,
		entityID, "github:pr:ci_check_failed", "", eventID, 0.5,
	)
	if err != nil {
		t.Fatalf("FindOrCreate sentinel idempotent: %v", err)
	}
	if created2 {
		t.Error("expected task to be found (not created) on second call")
	}
	if task2 == nil || task2.ID != task.ID {
		t.Errorf("idempotent call returned task %v, want %s", task2, task.ID)
	}
}

// TestTaskStore_Postgres_ReassignClaimToUser covers the TFAC-561 user↔user
// handoff arm: a CAS that only lands when the task is presently claimed by
// the expected "from" user AND the target shares a team with the task,
// moves the claim to a second seeded user, and refuses on a stale
// expectation, an unrelated target, an unclaimed/bot-claimed task, or a
// terminal row. Cross-team consolidation is covered separately in
// teams_multiteam_test.go, where the multi-team fixtures already live.
func TestTaskStore_Postgres_ReassignClaimToUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userA, agentID := seedPgOrgUserAgent(t, h)
	teamID := firstTeamForOrg(t, h, orgID)
	// userB shares teamID with the seeded task chain (every seedPgTaskChain
	// task's team_id) — a legitimate reassign target. userC has no team
	// membership at all — the target-relevance guard's negative case.
	userB := pgtest.SeedUser(t, h, "reassign-target")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, userB, teamID)
	userC := pgtest.SeedUser(t, h, "reassign-unrelated")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	t.Run("lands_then_refuses_stale_expectation", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-happy")
		if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign: %v", err)
		}
		if !ok {
			t.Fatal("reassign returned ok=false on a valid user-claimed task")
		}
		got, err := stores.Tasks.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ClaimedByUserID != userB {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userB)
		}
		if got.ClaimedByAgentID != "" {
			t.Errorf("ClaimedByAgentID=%q, want empty after reassign", got.ClaimedByAgentID)
		}
		// The claim already moved to userB — a second reassign expecting
		// userA as the "from" claimant must now be refused (stale CAS
		// expectation), and the successful reassign above must survive.
		ok, err = stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("stale reassign: %v", err)
		}
		if ok {
			t.Error("reassign returned ok=true against a stale expected-claimant; CAS guard broken")
		}
		got, err = stores.Tasks.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ClaimedByUserID != userB {
			t.Errorf("claim was disturbed by the refused CAS: got %q want %q", got.ClaimedByUserID, userB)
		}
	})

	t.Run("refuses_unclaimed_task", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-unclaimed")
		ok, err := stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on unclaimed: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning an unclaimed task; guard broken")
		}
	})

	t.Run("refuses_bot_claimed_task", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-bot")
		if _, err := stores.Tasks.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, ""); err != nil {
			t.Fatalf("stamp bot claim: %v", err)
		}
		ok, err := stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on bot-claimed: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning a bot-claimed task; the from-user CAS should never match a NULL claimed_by_user_id")
		}
	})

	t.Run("refuses_terminal_task", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-term")
		if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		if err := stores.Tasks.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning a closed task; status guard broken")
		}
	})

	// TFAC-561 review fix: a target with zero relationship to the task's
	// team must be refused — landing the claim anyway would leave userC
	// unable to even see the row afterward (tasks_select RLS requires team
	// membership on a claimed task).
	t.Run("refuses_target_with_no_team_membership", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-no-team")
		if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := stores.Tasks.ReassignClaimToUser(ctx, orgID, taskID, userA, userC)
		if err != nil {
			t.Fatalf("reassign to unrelated target: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning to a user with no team membership; target-relevance guard broken")
		}
		got, err := stores.Tasks.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ClaimedByUserID != userA {
			t.Errorf("claim was disturbed by the refused reassign: got %q want %q", got.ClaimedByUserID, userA)
		}
	})

	// ReassignClaimToUserSystem is the admin-pool sibling the reassign
	// handler actually calls — pin that it behaves identically to
	// ReassignClaimToUser (same guards, same success shape) rather than
	// silently diverging.
	t.Run("system_variant_matches_plain", func(t *testing.T) {
		_, _, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "reassign-system")
		if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := stores.Tasks.ReassignClaimToUserSystem(ctx, orgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("ReassignClaimToUserSystem: %v", err)
		}
		if !ok {
			t.Fatal("ReassignClaimToUserSystem returned ok=false on a valid user-claimed task")
		}
		got, err := stores.Tasks.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ClaimedByUserID != userB {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userB)
		}
	})
}

// TestTaskStore_Postgres_MarkEventInjectedSystem pins TFAC-621 part 2:
// MarkEventInjectedSystem flips an EXISTING (task_id, event_id) row's kind
// to "injected" in place, and no-ops (no error, no row created) when the
// row is absent — it must never mask over RecordEventSystem's own
// ON-CONFLICT-DO-NOTHING no-op the way calling RecordEventSystem(...,
// "injected") a second time on the same PK silently did.
func TestTaskStore_Postgres_MarkEventInjectedSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userA, _ := seedPgOrgUserAgent(t, h)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	readKind := func(t *testing.T, taskID, eventID string) (kind string, found bool) {
		t.Helper()
		err := h.AdminDB.QueryRow(`SELECT kind FROM task_events WHERE task_id = $1 AND event_id = $2`, taskID, eventID).Scan(&kind)
		if err == sql.ErrNoRows {
			return "", false
		}
		if err != nil {
			t.Fatalf("read task_events kind: %v", err)
		}
		return kind, true
	}

	t.Run("upgrades_existing_row", func(t *testing.T) {
		_, eventID, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "mark-injected-existing")
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, taskID, eventID, "bumped"); err != nil {
			t.Fatalf("seed bumped row: %v", err)
		}
		if _, err := stores.Tasks.MarkEventInjectedSystem(ctx, orgID, taskID, eventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("MarkEventInjectedSystem: %v", err)
		}
		kind, found := readKind(t, taskID, eventID)
		if !found {
			t.Fatal("row disappeared after MarkEventInjectedSystem")
		}
		if kind != "injected" {
			t.Errorf("kind = %q, want %q", kind, "injected")
		}
	})

	t.Run("absent_row_is_noop", func(t *testing.T) {
		_, eventID, taskID := seedPgTaskChain(t, h.AdminDB, orgID, userA, "mark-injected-absent")
		// No RecordEventSystem seed at all — (taskID, eventID) has no row.
		if _, err := stores.Tasks.MarkEventInjectedSystem(ctx, orgID, taskID, eventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("MarkEventInjectedSystem on absent row: %v", err)
		}
		if _, found := readKind(t, taskID, eventID); found {
			t.Error("MarkEventInjectedSystem created a row on a no-op absent-row call")
		}
	})
}

// seedPgEntityEvent inserts a bare entity + event (no task) and
// returns their IDs. Used by tests that call FindOrCreate themselves.
func seedPgEntityEvent(t *testing.T, conn *sql.DB, orgID, suffix string) (entityID, eventID string) {
	t.Helper()
	now := time.Now().UTC()
	entityID = uuid.New().String()
	eventID = uuid.New().String()
	sourceID := fmt.Sprintf("sentinel-%s-%d", suffix, now.UnixNano())

	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
	`, entityID, orgID, sourceID, "Sentinel "+suffix, "https://example/"+sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return entityID, eventID
}
