package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// BootstrapSchemaForTest applies the full schema and seed data to db
// from a cached SQL bundle. Equivalent to Migrate, but the bundle is
// built once per process — each test pays one Exec instead of running
// goose's full Up cycle.
//
// The bundle is captured by running Migrate against a fresh in-memory
// template, then dumping the resulting schema via sqlite_master plus
// rows from goose_db_version (so a follow-up Migrate call sees head)
// and events_catalog (FK target most tests rely on; seeded by the
// baseline migration). The migration runner itself is still covered
// by migrations_test.go, which uses Migrate directly.
//
// Tests-only. Production code uses Migrate.
func BootstrapSchemaForTest(db *sql.DB) error {
	bundle, err := cachedSchemaBundle()
	if err != nil {
		return err
	}
	_, err = db.Exec(bundle)
	return err
}

var (
	schemaBundleOnce sync.Once
	schemaBundleSQL  string
	schemaBundleErr  error
)

func cachedSchemaBundle() (string, error) {
	schemaBundleOnce.Do(func() {
		schemaBundleSQL, schemaBundleErr = buildSchemaBundle()
	})
	return schemaBundleSQL, schemaBundleErr
}

func buildSchemaBundle() (string, error) {
	template, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		return "", fmt.Errorf("open template: %w", err)
	}
	defer template.Close()
	template.SetMaxOpenConns(1)
	template.SetMaxIdleConns(1)

	if err := Migrate(template, "sqlite3"); err != nil {
		return "", fmt.Errorf("migrate template: %w", err)
	}

	// Seed the synthetic local tenant into the template so the dump
	// below captures it. Production no longer provisions at boot or in
	// the migration (provisioning is the explicit "Start your Triage
	// Factory" action via BootstrapLocalOrg) — but the vast majority of
	// store + handler tests assume a provisioned local install as their
	// fixture (inserting agents / team_agents / org_settings / etc. that
	// FK to these rows). BootstrapSchemaForTest therefore represents a
	// provisioned install; tests that want the genuine tenant-less boot
	// state use Migrate directly instead.
	if err := SeedLocalTenantRows(context.Background(), template); err != nil {
		return "", fmt.Errorf("seed local tenant into template: %w", err)
	}

	var b strings.Builder

	// DDL in sqlite_master rowid order so any dependency ordering
	// observed during creation is preserved on replay.
	rows, err := template.Query(`
		SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY rowid
	`)
	if err != nil {
		return "", fmt.Errorf("dump sqlite_master: %w", err)
	}
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			rows.Close()
			return "", err
		}
		b.WriteString(stmt)
		b.WriteString(";\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	// Seed rows preserved across the bundle:
	//   - goose_db_version  — a follow-up Migrate sees head.
	//   - events_catalog    — FK target for event_handlers.event_type;
	//     many tests INSERT against it. Seeded by the baseline migration.
	//   - instance_config   — singleton row (id=1); seeded by the baseline.
	//   - tenancy sentinels — orgs/teams/users/org_memberships/memberships
	//     + org_settings/team_settings, seeded into the template above by
	//     SeedLocalTenantRows (production provisions these via the explicit
	//     BootstrapLocalOrg action, not the migration). Resource tables
	//     carry org_id/team_id/creator_user_id columns with NOT NULL
	//     DEFAULT pointing at these sentinel UUIDs. The agents + team_agents
	//     tables declare FKs to orgs/teams/agents, so tests that INSERT
	//     into them need the parent rows present.
	//
	// agents + team_agents are included in the dump list defensively.
	// They're empty in the template (SeedLocalTenantRows seeds the bare
	// tenant — orgs/teams/users/memberships/settings — but not the agent,
	// which the provision action's BootstrapLocalOrg chain creates). The
	// dumpTableInserts call produces zero INSERTs for empty tables, so
	// listing them is a no-op today; the list documents intent in case a
	// future fixture starts seeding default agent rows.
	for _, table := range []string{
		"goose_db_version", "events_catalog",
		"orgs", "teams", "users", "org_memberships", "memberships",
		"agents", "team_agents",
		"instance_config", "org_settings", "team_settings",
	} {
		if err := dumpTableInserts(template, table, &b); err != nil {
			return "", err
		}
	}

	return b.String(), nil
}

func dumpTableInserts(db *sql.DB, table string, w *strings.Builder) error {
	cols, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM %s`,
		strings.Join(cols, ", "), table))
	if err != nil {
		return err
	}
	defer rows.Close()

	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	colList := strings.Join(cols, ", ")
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		w.WriteString("INSERT INTO ")
		w.WriteString(table)
		w.WriteString(" (")
		w.WriteString(colList)
		w.WriteString(") VALUES (")
		for i, v := range values {
			if i > 0 {
				w.WriteString(", ")
			}
			w.WriteString(sqlLiteral(v))
		}
		w.WriteString(");\n")
	}
	return rows.Err()
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func sqlLiteral(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case []byte:
		return "x'" + hex.EncodeToString(v) + "'"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case time.Time:
		return "'" + v.UTC().Format("2006-01-02 15:04:05.999999999") + "'"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
	}
}
