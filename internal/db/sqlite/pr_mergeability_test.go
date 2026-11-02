package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestMigrate_PRMergeabilityBackfill_SQLite pins the upgrade from v1.17.0: an
// active pull request whose stored snapshot reads a known answer starts with
// that answer at its snapshot's head and base, so its first probe after the
// upgrade emits nothing and no pull request is probed as retargeted. A
// snapshot naming no base leaves the row with none; a snapshot reading
// UNKNOWN, and a closed pull request, start with no row.
func TestMigrate_PRMergeabilityBackfill_SQLite(t *testing.T) {
	ctx := context.Background()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	// The last migration v1.17.0 ships.
	const prior = 202610290001
	if err := db.BootstrapSchemaAtForTest(conn, prior); err != nil {
		t.Fatalf("schema at %d: %v", prior, err)
	}
	if err := db.SeedLocalTenantRows(ctx, conn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	stores := sqlitestore.New(conn)
	org := runmode.LocalDefaultOrgID
	seed := func(number string, snap map[string]any) *domain.Entity {
		t.Helper()
		e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", dbtest.TestGitHubHost,
			"octo/up#"+number, "PR_up"+number, domain.EntityKindPullRequest, "t", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		raw, _ := json.Marshal(snap)
		if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(raw), e.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		return e
	}
	conflicting := seed("1", map[string]any{"number": 1, "head_sha": "head-1", "base_ref": "main", "mergeable": domain.MergeableConflicting})
	noBase := seed("2", map[string]any{"number": 2, "head_sha": "head-2", "mergeable": domain.MergeableMergeable})
	seed("3", map[string]any{"number": 3, "head_sha": "head-3", "base_ref": "main", "mergeable": domain.MergeableUnknown})
	closed := seed("4", map[string]any{"number": 4, "head_sha": "head-4", "base_ref": "main", "mergeable": domain.MergeableConflicting})
	if _, err := stores.Entities.CloseSystem(ctx, org, closed.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}

	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	rows, err := stores.PRMergeability.ListSystem(ctx, org)
	if err != nil {
		t.Fatalf("ListSystem: %v", err)
	}
	type answer struct{ mergeable, head, base string }
	want := map[string]answer{
		conflicting.ID: {domain.MergeableConflicting, "head-1", "main"},
		noBase.ID:      {domain.MergeableMergeable, "head-2", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows after the upgrade = %+v, want one each for %s and %s", rows, conflicting.SourceID, noBase.SourceID)
	}
	for _, row := range rows {
		w, ok := want[row.EntityID]
		if got := (answer{row.Mergeable, row.MergeableHeadSHA, row.MergeableBaseRef}); !ok || got != w || row.CheckedAt == nil {
			t.Errorf("row of %s after the upgrade = %+v, want %+v, checked when it was polled", row.EntityID, row, w)
		}
	}

	fx := sqliteMergeabilityFixture(t, conn)
	if _, err := stores.PRMergeability.AdmitSystem(ctx, org, []db.PRMergeabilityProbe{{EntityID: conflicting.ID, Reason: domain.MergeabilityProbeSweep}}); err != nil {
		t.Fatalf("AdmitSystem: %v", err)
	}
	fx.Ripen(t)
	batch, err := stores.PRMergeability.Claim(ctx, workitem.Owner{ID: "upgrade-test", Epoch: 1}, org, 10)
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("Claim = %d items (err %v), want 1", len(batch.Items), err)
	}
	res, err := stores.PRMergeability.CompleteProbe(ctx, batch.Items[0].Receipt,
		db.PRMergeabilityAnswer{Found: true, Mergeable: domain.MergeableConflicting, HeadSHA: "head-1"}, "")
	if err != nil {
		t.Fatalf("CompleteProbe: %v", err)
	}
	if len(res.Events) != 0 {
		t.Errorf("the first probe after the upgrade emitted %+v, want nothing", res.Events)
	}
	if q, err := stores.EventQueue.ListForEntity(ctx, org, conflicting.ID); err != nil || len(q) != 0 {
		t.Errorf("event queue = %+v (err %v), want nothing", q, err)
	}
}
