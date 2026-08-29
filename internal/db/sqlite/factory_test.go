package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestFactoryReadStore_SQLite runs the shared conformance suite
// against the SQLite FactoryReadStore impl. Each subtest gets a
// fresh in-memory DB so timestamps and counts don't bleed across
// assertions. The seeder inserts rows via raw SQL — fast and
// schema-shape-stable.
func TestFactoryReadStore_SQLite(t *testing.T) {
	dbtest.RunFactoryReadStoreConformance(t, func(t *testing.T) (db.FactoryReadStore, string, dbtest.FactorySeeder) {
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
			`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Factory Test', 'body', ?, ?)`,
			factoryTestPromptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
		); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}

		seed := newSQLiteFactorySeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.Factory, runmode.LocalDefaultOrgID, seed
	})
}

const factoryTestPromptID = "p_factory_test"

// newSQLiteFactorySeeder builds the FactorySeeder callbacks against
// a SQLite test connection. Each callback owns its raw SQL — the
// conformance suite is per-backend agnostic, so column-list drift
// between SQLite and Postgres lands here, not in the assertions.
func newSQLiteFactorySeeder(conn *sql.DB) dbtest.FactorySeeder {
	return dbtest.FactorySeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("factory-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
			`, id, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
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
			stored := any(status)
			if status == "running" {
				stored = nil
			}
			if _, err := conn.Exec(`
				INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id)
				VALUES (?, ?, ?, ?, 'manual', ?)
			`, id, taskID, factoryTestPromptID, stored, blueprintRunID); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			if status == "running" {
				if _, err := conn.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at)
					VALUES (?, ?, ?, 'factory-seed-executor', 1, CURRENT_TIMESTAMP)
				`, uuid.New().String(), runmode.LocalDefaultOrgID, id); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
			}
			return id
		},
		FinishConversation: func(t *testing.T, conversationID, status string, completedAt time.Time) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE conversations SET status = ?, completed_at = ? WHERE id = ?`,
				status, completedAt.UTC(), conversationID,
			); err != nil {
				t.Fatalf("finish conversation %s: %v", conversationID, err)
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
		SetConversationMemory: func(t *testing.T, conversationID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content)
					VALUES (?, ?, ?, NULL)
				`, memID, conversationID, entityID); err != nil {
					t.Fatalf("seed null conversation_memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content)
				VALUES (?, ?, ?, ?)
			`, memID, conversationID, entityID, content); err != nil {
				t.Fatalf("seed conversation_memory: %v", err)
			}
		},
	}
}

// TestFactoryReadStore_SQLite_AssertLocalOrg pins the local-only
// invariant that's specific to the SQLite impl — the orgID guard at
// every method entry refuses anything other than LocalDefaultOrgID.
// The conformance suite exercises the happy path; this test pins
// the SQLite-specific rejection.
func TestFactoryReadStore_SQLite_AssertLocalOrg(t *testing.T) {
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
	store := sqlitestore.New(conn).Factory

	if _, err := store.EventCountsSince(t.Context(), "some-other-org", time.Now()); err == nil {
		t.Error("EventCountsSince accepted non-LocalDefaultOrgID without error")
	}
}

// TestFactoryReadStore_SQLite_ShowsUntaskedEntities pins the local-
// mode contract: SQLite applies NO team-membership filter, so an
// active entity that no rule ever tasked still rides the factory
// snapshot. The local tracker already scopes discovery to the user's
// own involvement (author / review-requested / reviewed-by), so every
// entity is personally relevant — filtering by task existence would
// hide untriaged-but-relevant PRs the user expects to see. The
// converse (multi-mode membership exclusion) is Postgres-only.
func TestFactoryReadStore_SQLite_ShowsUntaskedEntities(t *testing.T) {
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

	// An active entity with an event but no task at all.
	id := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Untriaged PR', 'https://example/u', '{}', ?)
	`, id, "untasked-"+id[:8], time.Now().UTC()); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
	`, uuid.New().String(), id, time.Now().UTC()); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	store := sqlitestore.New(conn).Factory
	rows, err := store.Entities(t.Context(), runmode.LocalDefaultOrgID, 100, nil)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Entity.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("untasked entity %s missing from local-mode snapshot — local mode must not apply the membership filter", id)
	}
}

// TestFactoryReadStore_SQLite_CountersUnscoped pins the local-mode
// counterpart to the Postgres membership-scoped aggregates: SQLite
// applies no membership filter, so an untasked entity's events still
// contribute to EventCountsSince and LifetimeDistinctByEventType. This
// keeps the station header consistent with the unfiltered local belt
// (every polled entity is the user's own).
func TestFactoryReadStore_SQLite_CountersUnscoped(t *testing.T) {
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

	id := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Untriaged', 'https://example/u', '{}', ?)
	`, id, "ctr-"+id[:8], time.Now().UTC()); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
	`, uuid.New().String(), id, time.Now().UTC()); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	store := sqlitestore.New(conn).Factory
	ctx := t.Context()

	since, err := store.EventCountsSince(ctx, runmode.LocalDefaultOrgID, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("EventCountsSince: %v", err)
	}
	if since["github:pr:opened"] != 1 {
		t.Errorf("EventCountsSince[opened] = %d, want 1 (local mode must not scope by membership)", since["github:pr:opened"])
	}
	life, err := store.LifetimeDistinctByEventType(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("LifetimeDistinctByEventType: %v", err)
	}
	if life["github:pr:opened"] != 1 {
		t.Errorf("Lifetime[opened] = %d, want 1 (local mode unscoped)", life["github:pr:opened"])
	}
}

// TestParseDBDatetime is SQLite-specific — the COALESCE-loses-type-
// hint quirk that motivates parseDBDatetime is unique to modernc's
// SQLite driver. Postgres' pgx round-trips timestamps cleanly, so
// the helper doesn't exist there. Coverage matters because a row in
// the events table with an unrecognized format would silently break
// the entire factory snapshot — moved here from the pre-D2
// internal/db/factory_test.go so the SQLite impl's only non-trivial
// helper stays pinned.
//
// Two format families show up in the wild:
//
//   - SQLite-canonical (modernc with _time_format=sqlite, current default):
//     "2006-01-02 15:04:05.999999999-07:00", with the fractional segment
//     dropped when nanos==0 ("2026-04-27 19:02:11+00:00").
//   - Legacy Go time.String() (modernc default before _time_format=sqlite):
//     "2006-01-02 15:04:05.999999999 -0700 MST", optionally with a
//     " m=+..." monotonic clock suffix, optionally with the fractional
//     segment dropped when nanos==0.
//
// Go's time.Parse treats `.999...` as an optional fractional component,
// so a single layout matches both fractional and non-fractional inputs.
// This test pins that behavior so a future layout edit or stdlib change
// can't silently regress the no-fractional path — which would manifest
// as the factory page going blank with a 500 the moment any zero-nano
// timestamp hits the events table.
func TestParseDBDatetime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time // zero means "expected to error"
	}{
		{
			name: "sqlite_canonical_with_fractional",
			in:   "2026-04-27 19:02:11.123456789-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("", -7*3600)),
		},
		{
			name: "sqlite_canonical_no_fractional",
			in:   "2026-04-27 19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "go_string_with_fractional_pdt",
			in:   "2026-04-27 19:02:11.123456789 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123456789, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_pdt",
			in:   "2026-04-27 19:02:11 -0700 PDT",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_no_fractional_utc",
			in:   "2026-04-27 19:02:11 +0000 UTC",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "go_string_with_monotonic_suffix",
			in:   "2026-04-27 19:02:11.123 -0700 PDT m=+1.500",
			want: time.Date(2026, 4, 27, 19, 2, 11, 123000000, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "go_string_with_negative_monotonic",
			in:   "2026-04-27 19:02:11 -0700 PDT m=-0.250",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "sqlite_current_timestamp",
			in:   "2026-04-27 19:02:11",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_zulu",
			in:   "2026-04-27T19:02:11Z",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.UTC),
		},
		{
			name: "rfc3339_with_offset",
			in:   "2026-04-27T19:02:11-07:00",
			want: time.Date(2026, 4, 27, 19, 2, 11, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "empty",
			in:   "",
			want: time.Time{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sqlitestore.ParseDBDatetimeForTest(tc.in)
			if err != nil {
				t.Fatalf("parseDBDatetime(%q): unexpected error: %v", tc.in, err)
			}
			// Use Equal so location/abbreviation differences don't
			// fail the test as long as the instant is the same.
			if !got.Equal(tc.want) {
				t.Errorf("parseDBDatetime(%q) = %v, want %v (equal-instant)", tc.in, got, tc.want)
			}
		})
	}
}
