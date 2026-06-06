package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestPendingPRStore_Postgres runs the shared conformance suite
// against the Postgres PendingPRStore impl. Wires both pools against
// AdminDB (BYPASSRLS) so behavior tests stay independent of the auth
// path; the cross-org leakage test below exercises the org_id filter
// directly.
func TestPendingPRStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunPendingPRStoreConformance(t, func(t *testing.T) (db.PendingPRStore, string, dbtest.PendingPRSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgPendingPROrg(t, h)
		promptID := seedPgPendingPRPrompt(t, h, orgID, userID)
		seed := newPgPendingPRSeeder(h, stores, orgID, userID, promptID)
		return stores.PendingPRs, orgID, seed
	})
}

// TestPendingPRStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path. RLS via pending_prs_all
// (EXISTS-against-runs) also enforces this, but the org_id = $1
// clause in each query is the belt to RLS's suspenders.
func TestPendingPRStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	orgA, userA, _ := seedPgPendingPROrg(t, h)
	promptA := seedPgPendingPRPrompt(t, h, orgA, userA)
	orgB, _, _ := seedPgPendingPROrg(t, h)
	ctx := context.Background()

	seedA := newPgPendingPRSeeder(h, stores, orgA, userA, promptA)
	runA := seedA.Run(t)
	prA := uuid.New().String()
	if err := stores.PendingPRs.Create(ctx, orgA, domain.PendingPR{
		ID: prA, RunID: runA,
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("Create in orgA: %v", err)
	}

	// Get(orgB, prA) must return nil despite the row existing.
	if got, err := stores.PendingPRs.Get(ctx, orgB, prA); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA pending PR %s", prA)
	}

	// ByRunID cross-org must return nil.
	if got, err := stores.PendingPRs.ByRunID(ctx, orgB, runA); err != nil {
		t.Fatalf("ByRunID cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB ByRunID returned orgA pending PR %s", prA)
	}

	// UpdateTitleBody cross-org must "not found" — and crucially must
	// NOT return ErrPendingPRSubmitted, since the existence probe
	// runs scoped to orgB and finds nothing.
	if err := stores.PendingPRs.UpdateTitleBody(ctx, orgB, prA, "hack", "hack"); err == nil {
		t.Errorf("orgB UpdateTitleBody(orgA PR) should error")
	} else if errors.Is(err, db.ErrPendingPRSubmitted) {
		t.Errorf("cross-org UpdateTitleBody returned ErrPendingPRSubmitted, want not-found")
	}

	// Lock cross-org must return "not found" (NOT
	// ErrPendingPRAlreadyQueued) so a confused orgB caller can't
	// reverse-engineer existence via the SKY-212 sentinel.
	if err := stores.PendingPRs.Lock(ctx, orgB, prA, "T", "B"); err == nil {
		t.Errorf("orgB Lock(orgA PR) should error")
	} else if errors.Is(err, db.ErrPendingPRAlreadyQueued) {
		t.Errorf("cross-org Lock returned ErrPendingPRAlreadyQueued, want not-found")
	}

	// MarkSubmitted cross-org must return "not found" (NOT
	// ErrPendingPRSubmitInFlight) — same sentinel-leakage concern.
	if err := stores.PendingPRs.MarkSubmitted(ctx, orgB, prA); err == nil {
		t.Errorf("orgB MarkSubmitted(orgA PR) returned nil err; want not-found")
	} else if errors.Is(err, db.ErrPendingPRSubmitInFlight) {
		t.Errorf("cross-org MarkSubmitted returned ErrPendingPRSubmitInFlight, want not-found")
	}

	// Delete cross-org must "not found" and must NOT remove the row.
	if err := stores.PendingPRs.Delete(ctx, orgB, prA); err == nil {
		t.Errorf("orgB Delete(orgA PR) should error")
	}
	// Verify orgA's row still exists.
	got, err := stores.PendingPRs.Get(ctx, orgA, prA)
	if err != nil || got == nil {
		t.Errorf("orgA's row was clobbered by cross-org Delete: got=%v err=%v", got, err)
	}
}

// TestPendingPRStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for pending_prs. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual pending_prs_all
// policy is exercised. Same-org reads succeed; cross-org reads are
// silently filtered (USING); cross-org Create raises 42501 from
// WITH CHECK.
func TestPendingPRStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgPendingPROrg(t, h)
	promptA := seedPgPendingPRPrompt(t, h, orgA, alice)
	orgB, bob, _ := seedPgPendingPROrg(t, h)
	_ = bob

	// Seed a pending PR in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()
	seedA := newPgPendingPRSeeder(h, stores, orgA, alice, promptA)
	runA := seedA.Run(t)
	prA := uuid.New().String()
	if err := stores.PendingPRs.Create(ctx, orgA, domain.PendingPR{
		ID: prA, RunID: runA,
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("seed pending PR in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).PendingPRs.Get(ctx, orgA, prA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, prA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).PendingPRs.Get(ctx, orgA, prA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, prA) returned %+v; RLS USING filter leaked orgA's pending PR to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; Create against orgA would land
		// a row with org_id=orgA. pending_prs_all WITH CHECK requires
		// org_id = tf.current_org_id(), so 42501 is the expected
		// outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx).PendingPRs.Create(ctx, orgA, domain.PendingPR{
				ID: uuid.New().String(), RunID: runA,
				Owner: "o", Repo: "r",
				HeadBranch: "h2", HeadSHA: "s2", BaseBranch: "main",
				Title: "x-write", Body: "x",
			})
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

func seedPgPendingPROrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("pendingpr-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "PendingPR Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "PendingPR Org "+orgID[:8], "ppr-"+orgID[:8], userID,
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
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'PendingPR Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

func seedPgPendingPRPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	id := "p_pending_pr_test_" + orgID[:8]
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'PendingPR Test', 'body', 'user', '', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// newPgPendingPRSeeder builds the seeder callbacks. The Run seeder
// inserts an entity → event → task → run chain in the harness's
// AdminDB so pending_prs.run_id can FK to a real row.
func newPgPendingPRSeeder(h *pgtest.Harness, stores db.Stores, orgID, userID, promptID string) dbtest.PendingPRSeeder {
	conn := h.AdminDB
	return dbtest.PendingPRSeeder{
		Run: func(t *testing.T) string {
			t.Helper()
			entityID := uuid.New().String()
			eventID := uuid.New().String()
			taskID := uuid.New().String()
			runID := uuid.New().String()
			sourceID := fmt.Sprintf("pending-pr-run-%s", entityID[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', 'PendingPR Test', '', '{}'::jsonb, now())
			`, entityID, orgID, sourceID); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
			`, eventID, orgID, entityID); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
			`, taskID, orgID, userID, entityID, eventID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			// runs.blueprint_run_id is NOT NULL — mint a 1-step blueprint_run.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
				VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'user', 'BP', now(), now())
			`, bpID, orgID, userID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
				VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
			`, brID, orgID, userID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			if err := stores.AgentRuns.Create(context.Background(), orgID, domain.AgentRun{
				ID: runID, TaskID: taskID, PromptID: promptID,
				Status: "running", Model: "m", CreatorUserID: userID,
				BlueprintRunID: brID,
			}); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			return runID
		},
	}
}

// silence unused-import lint if helpers shift.
var _ = sql.ErrNoRows
