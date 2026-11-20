package db

import (
	"database/sql"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The migration under test, and the last migration v1.19.0 ships, whose
// schema the test seeds.
const (
	notifyRulesTellTheirTeam       int64 = 202611200001
	beforeNotifyRulesTellTheirTeam int64 = 202611120001
)

// TestMigrate_NotifyRulesTellTheirTeam: a rule that told its creator and one
// that told its team both come through the rebuild with every column but
// notify_audience as it was, and the table keeps every index it had.
func TestMigrate_NotifyRulesTellTheirTeam(t *testing.T) {
	database := openMigrationsTestDBAt(t, beforeNotifyRulesTellTheirTeam)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	execAll(t, database,
		`INSERT INTO orgs (id, slug, name) VALUES ('`+org+`', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('`+team+`', '`+org+`', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('`+user+`', 'Local')`,
	)
	// event_handlers.event_type references the catalog, which Migrate seeds
	// after the last migration and this database has not reached.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}
	execAll(t, database,
		`INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type, scope_predicate_json, enabled, source,
			 name, notify_audience, created_at, updated_at, user_modified)
		 VALUES ('personal', '`+org+`', '`+team+`', '`+user+`', 'notify', 'github:pr:ci_check_failed',
			 '{"check_name":"build"}', 1, 'user', 'CI failed on my PR', 'creator',
			 '2026-11-01 10:00:00', '2026-11-02 11:00:00', 1)`,
		`INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type, scope_predicate_json, enabled, source,
			 name, notify_audience, created_at, updated_at, deleted_at)
		 VALUES ('team', '`+org+`', '`+team+`', '`+user+`', 'notify', 'github:pr:review_commented',
			 NULL, 0, 'user', 'Reviews on the team''s PRs', 'team',
			 '2026-11-03 10:00:00', '2026-11-04 11:00:00', '2026-11-05 12:00:00')`,
	)

	before := handlerRows(t, database)
	beforeIndexes := handlerIndexes(t, database)
	migrateThrough(t, database, notifyRulesTellTheirTeam)

	after := handlerRows(t, database)
	for id, row := range before {
		delete(row, "notify_audience")
		if !reflect.DeepEqual(after[id], row) {
			t.Errorf("rule %s after the migration = %v; want %v", id, after[id], row)
		}
	}
	if len(after) != len(before) {
		t.Errorf("rules after the migration = %v; want the %d seeded", after, len(before))
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM pragma_table_info('event_handlers') WHERE name = 'notify_audience'`); n != 0 {
		t.Error("event_handlers still has notify_audience")
	}
	if got := handlerIndexes(t, database); !slices.Equal(got, beforeIndexes) {
		t.Errorf("indexes after the migration = %v; want %v", got, beforeIndexes)
	}
}

// handlerRows reads every event_handlers row by id, each as its columns'
// values.
func handlerRows(t *testing.T, database *sql.DB) map[string]map[string]any {
	t.Helper()
	rows, err := database.Query(`SELECT * FROM event_handlers`)
	if err != nil {
		t.Fatalf("read event_handlers: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	out := map[string]map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out[row["id"].(string)] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// handlerIndexes lists event_handlers' indexes with their definitions,
// sorted.
func handlerIndexes(t *testing.T, database *sql.DB) []string {
	t.Helper()
	rows, err := database.Query(`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND tbl_name = 'event_handlers' ORDER BY name`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out = append(out, name+" "+def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	return out
}
