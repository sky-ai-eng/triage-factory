package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSourceStatus_Postgres wires the suite against AdminDB, like the swipe
// suite: a swipe written there is attributed to the org owner, who is the
// claimant here, so UndoLastSwipe's guard holds.
func TestSourceStatus_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunSourceStatusConformance(t, func(t *testing.T) dbtest.SourceStatusHarness {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgOrgUserAgent(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.SourceStatusHarness{
			Tasks:   stores.Tasks,
			Swipes:  stores.Swipes,
			OrgID:   orgID,
			UserID:  userID,
			AgentID: agentID,
			SeedTask: func(t *testing.T, source, snapshot string) string {
				t.Helper()
				return seedPgSourceTask(t, h.AdminDB, orgID, userID, source, snapshot)
			},
		}
	})
}

// seedPgSourceTask inserts a queued task on a fresh entity of source carrying
// snapshot, owned by the org's first team.
func seedPgSourceTask(t *testing.T, conn *sql.DB, orgID, userID, source, snapshot string) string {
	t.Helper()
	entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	var kind, scope, eventType string
	switch source {
	case "jira":
		kind, scope, eventType = "issue", "https://jira.example.com", "jira:issue:assigned"
	case "linear":
		kind, scope, eventType = "issue", "ws-test", "linear:issue:available"
	default:
		kind, scope, eventType = "pr", "https://github.com", "github:pr:opened"
	}
	if _, err := conn.Exec(
		`INSERT INTO entities (id, org_id, source, source_id, kind, snapshot_json, scope)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
		entityID, orgID, source, "src-"+entityID, kind, snapshot, scope,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json)
		 VALUES ($1, $2, $3, $4, '', '{}'::jsonb)`,
		eventID, orgID, entityID, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                    primary_event_id, status, scoring_status)
		 VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'team',
		         $4, $5, '', $6, 'queued', 'pending')`,
		taskID, orgID, userID, entityID, eventType, eventID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}
