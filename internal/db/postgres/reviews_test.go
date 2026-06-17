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

// TestReviewStore_Postgres runs the shared conformance suite against
// the Postgres ReviewStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path;
// the cross-org leakage test below exercises the org_id filter
// directly.
func TestReviewStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunReviewStoreConformance(t, func(t *testing.T) (db.ReviewStore, string, dbtest.ReviewSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgReviewOrg(t, h)
		promptID := seedPgReviewPrompt(t, h, orgID, userID)
		seed := newPgReviewSeeder(h, stores, orgID, userID, promptID)
		return stores.Reviews, orgID, seed
	})
}

// TestReviewStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read path.
func TestReviewStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, _, _ := seedPgReviewOrg(t, h)
	orgB, _, _ := seedPgReviewOrg(t, h)
	ctx := context.Background()

	revA := uuid.New().String()
	if err := stores.Reviews.Create(ctx, orgA, domain.PendingReview{
		ID: revA, PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
	}); err != nil {
		t.Fatalf("Create in orgA: %v", err)
	}
	commentA := uuid.New().String()
	if err := stores.Reviews.AddComment(ctx, orgA, domain.PendingReviewComment{
		ID: commentA, ReviewID: revA, Path: "f.go", Line: 1, Body: "x",
	}); err != nil {
		t.Fatalf("AddComment in orgA: %v", err)
	}

	// Get(orgB, revA) must return nil despite the row existing.
	if got, err := stores.Reviews.Get(ctx, orgB, revA); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA review %s", revA)
	}

	// IsCommentID cross-org must return false.
	if stores.Reviews.IsCommentID(ctx, orgB, commentA) {
		t.Errorf("orgB IsCommentID returned true for orgA comment")
	}

	// ListComments cross-org must return empty.
	got, err := stores.Reviews.ListComments(ctx, orgB, revA)
	if err != nil {
		t.Fatalf("ListComments cross-org: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("orgB ListComments(orgA review) returned %d comments, want 0", len(got))
	}

	// UpdateComment cross-org must return "not found".
	if err := stores.Reviews.UpdateComment(ctx, orgB, commentA, "hack"); err == nil {
		t.Errorf("orgB UpdateComment(orgA comment) should error")
	}

	// LockSubmission cross-org must return "not found" (NOT
	// ErrPendingReviewAlreadySubmitted) — the existence probe runs
	// scoped to orgB and finds nothing.
	err = stores.Reviews.LockSubmission(ctx, orgB, revA, "body", "COMMENT")
	if errors.Is(err, db.ErrPendingReviewAlreadySubmitted) {
		t.Errorf("cross-org LockSubmission returned already-submitted, want not-found")
	}
}

// TestReviewStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for pending_reviews. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual
// pending_reviews_select / pending_reviews_modify policies are
// exercised. Same-org reads succeed; cross-org reads are silently
// filtered (USING); cross-org Create raises 42501 from WITH CHECK.
func TestReviewStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgReviewOrg(t, h)
	orgB, bob, _ := seedPgReviewOrg(t, h)
	_ = alice
	_ = bob

	// Seed a review in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	revA := uuid.New().String()
	if err := stores.Reviews.Create(ctx, orgA, domain.PendingReview{
		ID: revA, PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
	}); err != nil {
		t.Fatalf("seed review in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Reviews.Get(ctx, orgA, revA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, revA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Reviews.Get(ctx, orgA, revA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, revA) returned %+v; RLS USING filter leaked orgA's review to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; Create against orgA would land
		// a row with org_id=orgA. pending_reviews_modify WITH CHECK
		// requires org_id = tf.current_org_id(), so 42501 is the
		// expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx, pgtest.SecretKey).Reviews.Create(ctx, orgA, domain.PendingReview{
				ID: uuid.New().String(), PRNumber: 2, Owner: "o", Repo: "r", CommitSHA: "x-write",
			})
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

func seedPgReviewOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("review-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Review Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Review Org "+orgID[:8], "rev-"+orgID[:8], userID,
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
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Review Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

func seedPgReviewPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	id := "p_review_test_" + orgID[:8]
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Review Test', 'body', 'user', '', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// newPgReviewSeeder builds the seeder callbacks. The Run seeder
// inserts an entity → event → task → run chain in the harness's
// AdminDB so pending_reviews.run_id can FK to a real row.
func newPgReviewSeeder(h *pgtest.Harness, stores db.Stores, orgID, userID, promptID string) dbtest.ReviewSeeder {
	conn := h.AdminDB
	return dbtest.ReviewSeeder{
		Run: func(t *testing.T) string {
			t.Helper()
			entityID := uuid.New().String()
			eventID := uuid.New().String()
			taskID := uuid.New().String()
			runID := uuid.New().String()
			sourceID := fmt.Sprintf("review-run-%s", entityID[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', 'Review Test', '', '{}'::jsonb, now())
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
		SetReviewOriginals: func(t *testing.T, reviewID string, body, event *string) {
			t.Helper()
			var bodyArg, eventArg any
			if body != nil {
				bodyArg = *body
			}
			if event != nil {
				eventArg = *event
			}
			if _, err := conn.Exec(
				`UPDATE pending_reviews SET original_review_body = $1, original_review_event = $2 WHERE id = $3`,
				bodyArg, eventArg, reviewID,
			); err != nil {
				t.Fatalf("SetReviewOriginals: %v", err)
			}
		},
		SetCommentOriginalNull: func(t *testing.T, commentID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE pending_review_comments SET original_body = NULL WHERE id = $1`,
				commentID,
			); err != nil {
				t.Fatalf("SetCommentOriginalNull: %v", err)
			}
		},
	}
}

// silence the sql import in case the file later drops the helper.
var _ = sql.ErrNoRows
