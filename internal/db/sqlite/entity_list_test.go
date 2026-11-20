package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityReads_SQLite runs the shared entity and event list conformance
// suite against the SQLite impls. Each subtest gets a fresh in-memory DB so
// timestamps and counts don't bleed across assertions. The seeder inserts
// rows via raw SQL — fast and schema-shape-stable.
func TestEntityReads_SQLite(t *testing.T) {
	dbtest.RunEntityReadConformance(t, func(t *testing.T) (db.EntityStore, db.EventStore, string, dbtest.FlowSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)

		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		// conversations.prompt_id FKs into prompts — seed a stable prompt row
		// once so every Run() call resolves the FK without per-call
		// setup.
		if _, err := conn.Exec(
			`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Flow Test', 'body', ?, ?)`,
			flowTestPromptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
		); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}

		seed := newSQLiteFlowSeeder(conn)
		seed.GroupedEntity = func(t *testing.T, source string) (string, string) {
			t.Helper()
			id, grouping := uuid.New().String(), "lt-eng"
			var err error
			switch source {
			case "github":
				id = seed.Entity(t, "grouped")
				err = conn.QueryRow(`SELECT repository_id FROM entities WHERE id = ?`, id).Scan(&grouping)
			case "jira":
				const site = "https://jira.example.com"
				grouping = dbseed.JiraProject(t, conn, runmode.LocalDefaultOrgID, site, "SKY")
				_, err = conn.Exec(`
					INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, jira_project_id)
					VALUES (?, 'jira', 'SKY-3', ?, 'issue', 'SKY-3', '', '{}', ?, ?, ?)`,
					id, dbseed.JiraIssueID("SKY-3"), time.Now().UTC(), site, grouping)
			default:
				_, err = conn.Exec(`
					INSERT INTO entities (id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
					VALUES (?, 'linear', 'ENG-1', 'uuid-ENG-1', 'issue', 'ENG-1', '', '{}', ?, 'ws-test', ?)`,
					id, time.Now().UTC(), grouping)
			}
			if err != nil {
				t.Fatalf("seed %s entity: %v", source, err)
			}
			return id, grouping
		}
		stores := sqlitestore.New(conn)
		return stores.Entities, stores.Events, runmode.LocalDefaultOrgID, seed
	})
}

const flowTestPromptID = "p_flow_test"

// newSQLiteFlowSeeder builds the FlowSeeder callbacks against
// a SQLite test connection. Each callback owns its raw SQL — the
// conformance suite is per-backend agnostic, so column-list drift
// between SQLite and Postgres lands here, not in the assertions.
func newSQLiteFlowSeeder(conn *sql.DB) dbtest.FlowSeeder {
	return dbtest.FlowSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("octo/flow#%s-%s", suffix, id[:8])
			repoID := dbseed.GitHubEntityRepository(t, conn, runmode.LocalDefaultOrgID, "https://github.com", sourceID)
			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
				VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?, 'https://github.com', ?)
			`, id, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC(), repoID); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType, dedupKey string, createdAt, occurredAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			// occurredAt zero-value => NULL bind. sql.NullTime handles
			// the conversion; the events_catalog FK is satisfied by
			// the migration's seed.
			var occ sql.NullTime
			if !occurredAt.IsZero() {
				occ = sql.NullTime{Time: occurredAt, Valid: true}
			}
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at, occurred_at)
				VALUES (?, ?, ?, ?, '{}', ?, ?)
			`, id, entityID, eventType, dedupKey, createdAt, occ); err != nil {
				t.Fatalf("seed event %s on entity %s: %v", eventType, entityID, err)
			}
			return id
		},
		EventNullEntity: func(t *testing.T, eventType string, createdAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES (?, NULL, ?, '', '{}', ?)
			`, id, eventType, createdAt); err != nil {
				t.Fatalf("seed system event %s: %v", eventType, err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, dedupKey, primaryEventID, status string, createdAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
				                   status, priority_score, scoring_status, created_at,
				                   team_id, visibility)
				VALUES (?, ?, ?, ?, ?, ?, 0.5, 'pending', ?, ?, 'team')
			`, id, entityID, eventType, dedupKey, primaryEventID, status, createdAt, runmode.LocalDefaultTeamID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		Conversation: func(t *testing.T, taskID, status string) string {
			t.Helper()
			id := uuid.New().String()
			blueprintRunID := seedBlueprintRunForConversation(t, conn, taskID)
			// "running" is an engagement, not a stored value — mint the
			// claim the real claim path would and leave the column NULL.
			resolved, concluded := dbtest.SeedStatus(status)
			stored := any(resolved)
			if status == "running" {
				stored = nil
			}
			var completedAt any
			if concluded {
				completedAt = time.Now().UTC()
			}
			if _, err := conn.Exec(`
				INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, completed_at)
				VALUES (?, ?, ?, ?, 'manual', ?, ?)
			`, id, taskID, flowTestPromptID, stored, blueprintRunID, completedAt); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			if status == "running" {
				if _, err := conn.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
					VALUES (?, ?, ?, 'flow-seed-executor', 1, CURRENT_TIMESTAMP, strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
				`, uuid.New().String(), runmode.LocalDefaultOrgID, id); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
			}
			return id
		},
		FinishConversation: func(t *testing.T, conversationID, status string, completedAt time.Time) {
			t.Helper()
			var at any
			if !completedAt.IsZero() {
				at = completedAt.UTC()
			}
			if _, err := conn.Exec(
				`UPDATE conversations SET status = ?, completed_at = ? WHERE id = ?`,
				status, at, conversationID,
			); err != nil {
				t.Fatalf("finish conversation %s: %v", conversationID, err)
			}
		},
		WarmClaim: func(t *testing.T, conversationID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE claims SET phase = ? WHERE conversation_id = ? AND released_at IS NULL`,
				domain.ClaimPhaseWarm, conversationID,
			); err != nil {
				t.Fatalf("warm claim %s: %v", conversationID, err)
			}
		},
		CloseEntity: func(t *testing.T, entityID string, closedAt time.Time) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?`,
				closedAt.UTC(), entityID,
			); err != nil {
				t.Fatalf("close entity %s: %v", entityID, err)
			}
		},
		SetEntityKind: func(t *testing.T, entityID, kind string) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE entities SET kind = ? WHERE id = ?`, kind, entityID); err != nil {
				t.Fatalf("set entity %s kind: %v", entityID, err)
			}
		},
		SetConversationMemory: func(t *testing.T, conversationID, content string, source domain.MemorySource) {
			t.Helper()
			memID := uuid.New().String()
			var agentContent any
			if content != dbtest.NullMemorySentinel {
				agentContent = content
			}
			// Raw SQL, deliberately: this seeder's subject is the
			// memory_missing derivation, not the store door's validation.
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, conversation_id, agent_content, source)
				VALUES (?, ?, ?, ?)
			`, memID, conversationID, agentContent, string(source)); err != nil {
				t.Fatalf("seed conversation_memory (source=%s): %v", source, err)
			}
		},
	}
}

// TestEntityReads_SQLite_AssertLocalOrg pins the local-only invariant that's
// specific to the SQLite impls — the orgID guard at every method entry
// refuses anything other than LocalDefaultOrgID.
func TestEntityReads_SQLite_AssertLocalOrg(t *testing.T) {
	conn := openFlowTestDB(t)
	stores := sqlitestore.New(conn)
	if _, _, err := stores.Entities.List(t.Context(), "some-other-org", db.EntityListFilter{}, db.Unwindowed); err == nil {
		t.Error("Entities.List accepted non-LocalDefaultOrgID without error")
	}
	if _, _, err := stores.Events.List(t.Context(), "some-other-org", db.EventListFilter{}, db.Unwindowed); err == nil {
		t.Error("Events.List accepted non-LocalDefaultOrgID without error")
	}
}

// TestEntityReads_SQLite_ListsUntrackedEntities pins the local-mode contract:
// SQLite applies no tracked-set filter and ignores a team narrowing, so an
// entity in a repository no team tracks is listed, and so are its events. The
// local tracker already scopes discovery to the user's own involvement
// (author / review-requested / reviewed-by), so every entity is the user's.
// The converse (multi-mode exclusion) is Postgres-only.
func TestEntityReads_SQLite_ListsUntrackedEntities(t *testing.T) {
	conn := openFlowTestDB(t)
	id := uuid.New().String()
	sourceID := "octo/untracked#" + id[:8]
	repoID := dbseed.GitHubEntityRepository(t, conn, runmode.LocalDefaultOrgID, "https://github.com", sourceID)
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES (?, 'github', ?, 'pr', 'Untriaged PR', 'https://example/u', '{}', ?, 'https://github.com', ?)
	`, id, sourceID, time.Now().UTC(), repoID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
	`, uuid.New().String(), id, time.Now().UTC()); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	stores := sqlitestore.New(conn)
	items, _, err := stores.Entities.List(t.Context(), runmode.LocalDefaultOrgID,
		db.EntityListFilter{TeamIDs: []string{runmode.LocalDefaultTeamID}}, db.Unwindowed)
	if err != nil {
		t.Fatalf("Entities.List: %v", err)
	}
	if len(items) != 1 || items[0].Entity.ID != id || items[0].LatestEventType != "github:pr:opened" {
		t.Errorf("entity list = %+v, want the untracked entity with its event — local mode applies no tracked-set filter", items)
	}
	// A team that does not exist is nobody's, so naming it narrows to
	// nothing rather than widening to every entity.
	none, total, err := stores.Entities.List(t.Context(), runmode.LocalDefaultOrgID,
		db.EntityListFilter{TeamIDs: []string{uuid.New().String()}}, db.Unwindowed)
	if err != nil {
		t.Fatalf("Entities.List for an unknown team: %v", err)
	}
	if len(none) != 0 || total != 0 {
		t.Errorf("unknown team = %d rows of %d, want none", len(none), total)
	}
	events, _, err := stores.Events.List(t.Context(), runmode.LocalDefaultOrgID, db.EventListFilter{}, db.Unwindowed)
	if err != nil {
		t.Fatalf("Events.List: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("event list = %d rows, want the untracked entity's event", len(events))
	}
}

// openFlowTestDB opens a fresh in-memory database with the schema applied.
func openFlowTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return conn
}
