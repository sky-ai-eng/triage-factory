package postgres_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSwipeStore_Postgres runs the shared SwipeStore conformance suite
// against the Postgres impl. Wired against AdminDB so creator_user_id
// resolution can fall back to org.owner_user_id without needing JWT
// claims plumbed on every subtest (the production claims path is
// covered separately by pgtest's RLS suite — D5 will exercise the
// request-scoped path end-to-end).
func TestSwipeStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSwipeStoreConformance(t, func(t *testing.T) (db.SwipeStore, string, dbtest.TaskSeederForSwipes, dbtest.TaskReaderForSwipes, dbtest.SwipeAuditReader) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgAndUserForSwipes(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		seed := func(t *testing.T) string {
			t.Helper()
			return seedPgTaskForSwipes(t, h.AdminDB, orgID, userID)
		}
		read := func(t *testing.T, taskID string) (string, time.Time) {
			t.Helper()
			return readPgTask(t, h.AdminDB, taskID)
		}
		readAudit := func(t *testing.T, taskID string) []string {
			t.Helper()
			return readPgSwipeAudit(t, h.AdminDB, taskID)
		}
		return stores.Swipes, orgID, seed, read, readAudit
	})
}

func seedPgOrgAndUserForSwipes(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("swipe-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Swipe Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Swipe Conformance Org "+orgID[:8], "swipe-"+orgID[:8], userID,
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
	return orgID, userID
}

func seedPgTaskForSwipes(t *testing.T, conn *sql.DB, orgID, userID string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()
	sourceID := fmt.Sprintf("swipe-conformance-%d", now.UnixNano())

	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Swipe Conformance', 'https://example/x', '{}'::jsonb, $4)
	`, entityID, orgID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// team_id resolved inline from the org's first team — keeps the
	// helper signature stable (no need to thread teamID through tests
	// that only care about orgID/userID).
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, created_at)
		VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', $6)
	`, taskID, orgID, userID, entityID, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

func readPgTask(t *testing.T, conn *sql.DB, taskID string) (string, time.Time) {
	t.Helper()
	var status string
	var snoozeUntil sql.NullTime
	if err := conn.QueryRow(`SELECT status, snooze_until FROM tasks WHERE id = $1`, taskID).Scan(&status, &snoozeUntil); err != nil {
		t.Fatalf("readPgTask %s: %v", taskID, err)
	}
	if snoozeUntil.Valid {
		return status, snoozeUntil.Time
	}
	return status, time.Time{}
}

// readPgSwipeAudit returns swipe_events.action rows for a task,
// oldest first (ORDER BY id; BIGSERIAL is monotonic per insert).
// Used by the harness to pin the audit-log invariants — same
// shape as the SQLite reader.
func readPgSwipeAudit(t *testing.T, conn *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := conn.Query(`SELECT action FROM swipe_events WHERE task_id = $1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("readPgSwipeAudit %s: %v", taskID, err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan swipe_events action: %v", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("readPgSwipeAudit iteration: %v", err)
	}
	return actions
}

// TestSnoozeVisibility_Postgres is the Postgres half of the snooze-visibility
// conformance suite — see the SQLite twin.
func TestSnoozeVisibility_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSnoozeVisibilityConformance(t, func(t *testing.T) (db.SwipeStore, db.TaskStore, string, dbtest.TaskSeederForSwipes) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgAndUserForSwipes(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seed := func(t *testing.T) string {
			t.Helper()
			return seedPgTaskForSwipes(t, h.AdminDB, orgID, userID)
		}
		return stores.Swipes, stores.Tasks, orgID, seed
	})
}
