package postgres_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestPRMergeabilityStore_Postgres runs the shared conformance suite against
// the Postgres impl, both pools wired against AdminDB like the other system
// stores' suites, with every timestamp moved against the server clock the
// claim and the guard read.
func TestPRMergeabilityStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunPRMergeabilityConformance(t, func(t *testing.T) dbtest.PRMergeabilityFixture {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "mergeability")
		return dbtest.PRMergeabilityFixture{
			Stores: stores,
			OrgID:  orgID,
			Ripen: func(t *testing.T) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`UPDATE pr_mergeability_probes SET probe_after = statement_timestamp() - interval '1 second' WHERE status = 'ready'`); err != nil {
					t.Fatalf("ripen probes: %v", err)
				}
			},
			ExpireLease: func(t *testing.T, id int64) {
				t.Helper()
				pgExecOne(t, h, "expire lease",
					`UPDATE pr_mergeability_probes SET lease_expires_at = statement_timestamp() - interval '1 hour' WHERE id = $1 AND status = 'leased'`, id)
			},
			Probes: func(t *testing.T, entityID string) []dbtest.MergeabilityProbeRow {
				t.Helper()
				rows, err := h.AdminDB.Query(`SELECT id, status, reason, unknown_reads, probe_after FROM pr_mergeability_probes WHERE entity_id = $1 ORDER BY id`, entityID)
				if err != nil {
					t.Fatalf("read probes: %v", err)
				}
				defer rows.Close()
				var out []dbtest.MergeabilityProbeRow
				for rows.Next() {
					var r dbtest.MergeabilityProbeRow
					if err := rows.Scan(&r.ID, &r.Status, &r.Reason, &r.UnknownReads, &r.ProbeAfter); err != nil {
						t.Fatalf("scan probe: %v", err)
					}
					out = append(out, r)
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("read probes: %v", err)
				}
				return out
			},
		}
	})
}

// TestPRMergeabilityWorkItem_Postgres runs the table-agnostic half of the
// work-item conformance suite against the production pr_mergeability_probes
// table under the kind production declares, each admission on a fresh pull
// request entity for the table's foreign key and a probe_after already past.
func TestPRMergeabilityWorkItem_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "mergeability-table")
		cols := func(i int) map[string]any {
			e, _, err := stores.Entities.FindOrCreateSystem(context.Background(), orgID, "github", dbtest.TestGitHubHost,
				"octo/table#"+strconv.Itoa(i+1), "PR_table"+strconv.Itoa(i+1), domain.EntityKindPullRequest, "t", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			return db.PRMergeabilityProbeCols(e.ID, domain.MergeabilityProbeNew, "2000-01-01T00:00:00Z", 0)
		}
		return h.AdminDB, workkinds.PRMergeabilityProbes(workitem.Postgres), orgID, cols
	})
}
