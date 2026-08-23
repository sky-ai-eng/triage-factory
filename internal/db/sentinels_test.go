package db

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMigrationDefaults_MatchRuntimeConstants catches drift between
// the baseline's DEFAULT '00000000-...' literals on
// org_id / team_id / creator_user_id columns and the runmode
// constants those literals are meant to mirror.
//
// These DEFAULT literals are load-bearing in local mode: a resource row
// inserted without an explicit org_id/team_id (the N=1 happy path) takes
// the sentinel from the column DEFAULT, and the store impls then look it
// up by runmode.LocalDefaultOrgID. There's no FK on these columns in the
// SQLite baseline, so a DEFAULT literal that drifts from the runtime
// constant produces orphan rows that no other check notices — this test
// is the gate. (It runs against the migrated, tenant-less schema and
// never needs the tenant rows themselves, since the probed columns have
// no org/team FK.)
//
// Strategy: insert minimal probe rows that exercise every column's
// DEFAULT clause, then read back and assert the column landed on
// the expected sentinel. Each probe is rolled back in a transaction
// so the test leaves no state behind.
func TestMigrationDefaults_MatchRuntimeConstants(t *testing.T) {
	d := openMigrationsTestDB(t)
	if err := Migrate(d, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Provision the local tenant so the FK-bearing probe columns
	// (conversations.creator_user_id → users) resolve. The migration no longer
	// seeds tenant rows; this test exercises the DEFAULT literals against
	// a provisioned install, which is the state those defaults serve.
	if err := SeedLocalTenantRows(context.Background(), d); err != nil {
		t.Fatalf("SeedLocalTenantRows: %v", err)
	}

	// Each probe: (table, columns to set explicitly, defaulted column
	// to read back, expected sentinel). The "extra setup" closures
	// satisfy FK chains and required-not-default columns.
	cases := []struct {
		name           string
		setup          func(t *testing.T, tx *sql.Tx)
		probe          string
		probeArgs      []any
		readBackColumn string
		readBackQuery  string
		readBackArgs   []any
		expected       string
	}{
		{
			name: "prompts.org_id",
			probe: `INSERT INTO prompts (id, name, body, team_id, creator_user_id)
			        VALUES ('probe-prompt', 'p', 'b', ?, ?)`,
			probeArgs:      []any{runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID},
			readBackColumn: "org_id",
			readBackQuery:  `SELECT org_id FROM prompts WHERE id = 'probe-prompt'`,
			expected:       runmode.LocalDefaultOrgID,
		},
		{
			name: "tasks.org_id+team_id+creator_user_id",
			setup: func(t *testing.T, tx *sql.Tx) {
				t.Helper()
				if _, err := tx.Exec(`INSERT INTO entities (id, source, source_id, kind)
				                      VALUES ('probe-ent', 'github', '1', 'pr')`); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO events (id, entity_id, event_type)
				                      VALUES ('probe-evt', 'probe-ent', 'github:pr:opened')`); err != nil {
					t.Fatalf("seed event: %v", err)
				}
			},
			probe: `INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
			        VALUES ('probe-task', 'probe-ent', 'github:pr:opened', 'probe-evt')`,
			readBackQuery: `SELECT org_id || '|' || team_id || '|' || creator_user_id
			                FROM tasks WHERE id = 'probe-task'`,
			readBackColumn: "org_id|team_id|creator_user_id",
			expected: runmode.LocalDefaultOrgID + "|" +
				runmode.LocalDefaultTeamID + "|" +
				runmode.LocalDefaultUserID,
		},
		{
			name: "conversations.org_id+team_id",
			setup: func(t *testing.T, tx *sql.Tx) {
				t.Helper()
				if _, err := tx.Exec(`INSERT INTO entities (id, source, source_id, kind)
				                      VALUES ('probe-ent-r', 'github', '2', 'pr')`); err != nil {
					t.Fatalf("seed entity: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO events (id, entity_id, event_type)
				                      VALUES ('probe-evt-r', 'probe-ent-r', 'github:pr:opened')`); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
				                      VALUES ('probe-task-r', 'probe-ent-r', 'github:pr:opened', 'probe-evt-r')`); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO prompts (id, name, body, team_id, creator_user_id)
				                      VALUES ('probe-prompt-r', 'p', 'b', ?, ?)`,
					runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("seed prompt: %v", err)
				}
				// The origin CHECK requires the blueprint chain — seed it.
				if _, err := tx.Exec(`INSERT INTO blueprints (id, team_id, name, source, creator_user_id)
				                      VALUES ('probe-bp-r', ?, 'bp', 'user', ?)`,
					runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("seed blueprint: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
				                      VALUES ('probe-bp-run-r', 'probe-bp-r', 'probe-task-r', 'manual', 'running', '/tmp/wt', '[]')`); err != nil {
					t.Fatalf("seed blueprint_run: %v", err)
				}
			},
			probe: `INSERT INTO conversations (id, task_id, prompt_id, trigger_type, creator_user_id, blueprint_run_id, status)
			        VALUES ('probe-run', 'probe-task-r', 'probe-prompt-r', 'manual', ?, 'probe-bp-run-r', 'queued')`,
			probeArgs:      []any{runmode.LocalDefaultUserID},
			readBackQuery:  `SELECT org_id || '|' || team_id FROM conversations WHERE id = 'probe-run'`,
			readBackColumn: "org_id|team_id",
			expected:       runmode.LocalDefaultOrgID + "|" + runmode.LocalDefaultTeamID,
		},
		{
			name: "event_handlers.org_id+team_id",
			// Don't pass team_id explicitly — exercise the DEFAULT so a
			// drift between the literal in the migration and
			// runmode.LocalDefaultTeamID surfaces here.
			probe: `INSERT INTO event_handlers
			        (id, kind, event_type, source, name, default_priority, sort_order, creator_user_id)
			        VALUES ('probe-eh', 'rule', 'github:pr:opened', 'user', 'n', 0.5, 0, ?)`,
			probeArgs:      []any{runmode.LocalDefaultUserID},
			readBackQuery:  `SELECT org_id || '|' || team_id FROM event_handlers WHERE id = 'probe-eh'`,
			readBackColumn: "org_id|team_id",
			expected:       runmode.LocalDefaultOrgID + "|" + runmode.LocalDefaultTeamID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := d.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			if tc.setup != nil {
				tc.setup(t, tx)
			}
			if _, err := tx.Exec(tc.probe, tc.probeArgs...); err != nil {
				t.Fatalf("probe insert: %v", err)
			}
			var got string
			if err := tx.QueryRow(tc.readBackQuery, tc.readBackArgs...).Scan(&got); err != nil {
				t.Fatalf("read back %s: %v", tc.readBackColumn, err)
			}
			if got != tc.expected {
				t.Errorf("%s DEFAULT drift: got %q, want %q — "+
					"the migration's DEFAULT clause has diverged from "+
					"runmode.LocalDefault*ID. Reconcile both sides.",
					tc.readBackColumn, got, tc.expected)
			}
		})
	}
}
