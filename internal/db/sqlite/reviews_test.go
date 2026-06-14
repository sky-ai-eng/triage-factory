package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// anyReview is a minimal fixture for orgID-guard tests where the
// review's actual fields don't matter — the assertion fires before
// the INSERT runs.
func anyReview() domain.PendingReview {
	return domain.PendingReview{
		ID: uuid.New().String(), PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
	}
}

// TestReviewStore_SQLite runs the shared conformance suite against
// the SQLite ReviewStore impl. Each subtest gets a fresh in-memory
// DB.
func TestReviewStore_SQLite(t *testing.T) {
	dbtest.RunReviewStoreConformance(t, func(t *testing.T) (db.ReviewStore, string, dbtest.ReviewSeeder) {
		t.Helper()
		conn := newSQLiteForReviewTest(t)
		seed := newSQLiteReviewSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Reviews, runmode.LocalDefaultOrgID, seed
	})
}

// TestReviewStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard — every method must refuse a non-local orgID.
func TestReviewStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForReviewTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if err := stores.Reviews.Create(t.Context(), bogusOrg, anyReview()); err == nil {
		t.Errorf("Create with non-local orgID should error")
	}
	if _, err := stores.Reviews.Get(t.Context(), bogusOrg, "any"); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
}

func newSQLiteForReviewTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// newSQLiteReviewSeeder returns the bag of raw-SQL helpers the
// conformance suite drives. pending_reviews.run_id is now FK'd to runs(id)
// ON DELETE SET NULL (this baseline, matching PG), so the Run seeder must
// chain a real entity/event/task/blueprint_run/run for the FK to resolve.
func newSQLiteReviewSeeder(conn *sql.DB) dbtest.ReviewSeeder {
	return dbtest.ReviewSeeder{
		Run: func(t *testing.T) string {
			t.Helper()
			return seedSQLiteRunForReview(t, conn)
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
				`UPDATE pending_reviews SET original_review_body = ?, original_review_event = ? WHERE id = ?`,
				bodyArg, eventArg, reviewID,
			); err != nil {
				t.Fatalf("SetReviewOriginals: %v", err)
			}
		},
		SetCommentOriginalNull: func(t *testing.T, commentID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE pending_review_comments SET original_body = NULL WHERE id = ?`,
				commentID,
			); err != nil {
				t.Fatalf("SetCommentOriginalNull: %v", err)
			}
		},
	}
}

// seedSQLiteRunForReview chains a real entity/event/task/blueprint_run/run so a
// runs row exists for pending_reviews.run_id to FK-point at. Returns the run id.
// Each test gets a fresh in-memory DB, so the fixed prompt id is collision-free.
func seedSQLiteRunForReview(t *testing.T, conn *sql.DB) string {
	t.Helper()
	entityID := uuid.New().String()
	sourceID := "review-run-" + entityID
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json)
		VALUES (?, 'github', ?, 'pr', 'Review Conformance', 'https://example/x', '{}')
	`, entityID, sourceID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO prompts (id, name, body, creator_user_id, team_id)
		VALUES ('p_review', 'Review', 'body', ?, ?)
	`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'queued')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	runID := uuid.New().String()
	blueprintRunID := seedBlueprintRunForRun(t, conn, taskID)
	if _, err := conn.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, model, blueprint_run_id)
		VALUES (?, ?, 'p_review', 'running', 'm', ?)
	`, runID, taskID, blueprintRunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}
