package sqlite_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sqliteMergeabilityFixture is the conformance fixture over one migrated
// in-memory database.
func sqliteMergeabilityFixture(t *testing.T, conn *sql.DB) dbtest.PRMergeabilityFixture {
	t.Helper()
	return dbtest.PRMergeabilityFixture{
		Stores: sqlitestore.New(conn),
		OrgID:  runmode.LocalDefaultOrgID,
		Ripen: func(t *testing.T) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
				t.Fatalf("ripen probes: %v", err)
			}
		},
		ExpireLease: func(t *testing.T, id int64) {
			t.Helper()
			execOne(t, conn, "expire lease",
				`UPDATE pr_mergeability_probes SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, id)
		},
		Probes: func(t *testing.T, entityID string) []dbtest.MergeabilityProbeRow {
			t.Helper()
			return readSQLiteMergeabilityProbes(t, conn, entityID)
		},
	}
}

func readSQLiteMergeabilityProbes(t *testing.T, conn *sql.DB, entityID string) []dbtest.MergeabilityProbeRow {
	t.Helper()
	rows, err := conn.Query(`SELECT id, status, reason, unknown_reads, attempt, probe_after FROM pr_mergeability_probes WHERE entity_id = ? ORDER BY id`, entityID)
	if err != nil {
		t.Fatalf("read probes: %v", err)
	}
	defer rows.Close()
	var out []dbtest.MergeabilityProbeRow
	for rows.Next() {
		var r dbtest.MergeabilityProbeRow
		var after string
		if err := rows.Scan(&r.ID, &r.Status, &r.Reason, &r.UnknownReads, &r.Attempt, &after); err != nil {
			t.Fatalf("scan probe: %v", err)
		}
		if r.ProbeAfter, err = time.Parse("2006-01-02 15:04:05.000", after); err != nil {
			t.Fatalf("probe_after %q: %v", after, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read probes: %v", err)
	}
	return out
}

// TestPRMergeabilityStore_SQLite runs the shared conformance suite against the
// SQLite impl, a fresh in-memory database per subtest.
func TestPRMergeabilityStore_SQLite(t *testing.T) {
	dbtest.RunPRMergeabilityConformance(t, func(t *testing.T) dbtest.PRMergeabilityFixture {
		return sqliteMergeabilityFixture(t, openSQLiteForTest(t))
	})
}

// TestPRMergeabilityWorkItem_SQLite runs the table-agnostic half of the
// work-item conformance suite against the production pr_mergeability_probes
// table under the kind production declares, each admission on a fresh pull
// request entity for the table's foreign key and a probe_after already past.
func TestPRMergeabilityWorkItem_SQLite(t *testing.T) {
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		cols := func(i int) map[string]any {
			e, _, err := stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "github", dbtest.TestGitHubHost,
				"octo/table#"+strconv.Itoa(i+1), "PR_table"+strconv.Itoa(i+1), domain.EntityKindPullRequest, "t", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			return db.PRMergeabilityProbeCols(e.ID, domain.MergeabilityProbeNew, "2000-01-01 00:00:00.000", 0)
		}
		return conn, workkinds.PRMergeabilityProbes(workitem.SQLite), runmode.LocalDefaultOrgID, cols
	})
}
