package postgres_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_Postgres_DelegatorNoticePlans pins that the brain's
// two delegator-notice reads can search their partial indexes, which hold only
// the rows still owed a notice, in the generic plan a reused statement runs
// under, where the bound reasons are not known. Sequential scans are turned
// off, so the plan shows what the indexes allow rather than what an empty
// table costs.
func TestConversationStore_Postgres_DelegatorNoticePlans(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	quoted := make([]string, 0, len(domain.ParkReasonsAwaitingPerson()))
	for _, r := range domain.ParkReasonsAwaitingPerson() {
		quoted = append(quoted, "'"+string(r)+"'")
	}
	for _, tc := range []struct {
		name, query, types, args, index string
	}{
		{"pause", pgstore.PauseNoticesDueQueryForTest, "text[], integer", "ARRAY[" + strings.Join(quoted, ", ") + "]::text[], 100", "idx_conversations_pause_notice_due"},
		{"failure", pgstore.FailureNoticesDueQueryForTest, "integer", "100", "idx_conversations_failure_notice_due"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := genericPlan(t, h.AdminDB, "notice_plan_"+tc.name, tc.query, tc.types, tc.args)
			if !strings.Contains(plan, "Index Scan using "+tc.index) {
				t.Errorf("generic plan does not search %s:\n%s", tc.index, plan)
			}
		})
	}
}

// genericPlan is EXPLAIN of query, prepared as name with parameter types
// types and executed with the SQL literals args under a forced generic plan,
// on one connection. EXPLAIN EXECUTE takes no bound parameters, so args are
// literals.
func genericPlan(t *testing.T, database *sql.DB, name, query, types, args string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`SET LOCAL plan_cache_mode = force_generic_plan`,
		`SET LOCAL enable_seqscan = off`,
		`PREPARE ` + name + ` (` + types + `) AS ` + query,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// A prepared statement belongs to the session, not the transaction, and
	// outlives the rollback.
	defer func() { _, _ = tx.ExecContext(ctx, `DEALLOCATE `+name) }()
	rows, err := tx.QueryContext(ctx, `EXPLAIN EXECUTE `+name+`(`+args+`)`)
	if err != nil {
		t.Fatalf("EXPLAIN EXECUTE: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return strings.Join(lines, "\n")
}
