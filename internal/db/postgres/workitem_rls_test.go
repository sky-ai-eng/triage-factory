package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
)

// Two copies of the fixture shape, differing only in which policies govern
// them: the read/write pair this schema gives every table it mutates after
// creation, and the append-only set its audit ledgers carry.
const (
	rlsWritableTable   = "workitem_rls_writable"
	rlsAppendOnlyTable = "workitem_rls_append_only"
)

// A deduplicating admission resolves its conflict with DO UPDATE, and Postgres
// evaluates that arm against the table's UPDATE policy — so an INSERT policy
// alone does not admit it. That is the contract's requirement rather than
// admission's: every guarded write in the package is an UPDATE too, and this
// test pins both halves, because the two report a missing policy very
// differently. Admission raises; a guarded write quietly touches no rows, which
// is a queue that looks empty forever.
//
// It lives here rather than in the shared suite because the suite runs on the
// admin (BYPASSRLS) pool by design, and the harness that can produce a
// claims-carrying connection is Postgres-only.
func TestWorkItemAdmitUnderRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := t.Context()
	org, user := uuid.NewString(), uuid.NewString()

	for _, table := range []string{rlsWritableTable, rlsAppendOnlyTable} {
		for _, stmt := range workitemtest.TableDDL(workitem.Postgres, table, workitem.UniqueWhileUnsettled) {
			if _, err := h.AdminDB.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("fixture DDL %q: %v", table, err)
			}
		}
		// The privilege grant is identical for both; only the policies differ,
		// so a failure below is the policy set and nothing else.
		for _, stmt := range []string{
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON " + table + " TO tf_app",
			"CREATE POLICY " + table + "_select ON " + table + " FOR SELECT TO tf_app USING (org_id = tf.current_org_id())",
			"CREATE POLICY " + table + "_insert ON " + table + " FOR INSERT TO tf_app WITH CHECK (org_id = tf.current_org_id())",
		} {
			if _, err := h.AdminDB.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("govern %s: %v", table, err)
			}
		}
	}
	if _, err := h.AdminDB.ExecContext(ctx,
		"CREATE POLICY "+rlsWritableTable+"_update ON "+rlsWritableTable+
			" FOR UPDATE TO tf_app USING (org_id = tf.current_org_id()) WITH CHECK (org_id = tf.current_org_id())",
	); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	kind := func(table string) workitem.Kind {
		return workitem.Kind{
			Table: table, Dialect: workitem.Postgres,
			Unique: workitem.UniqueWhileUnsettled, Strategy: workitem.SingleTx,
			Policy:  workitem.Policy{MaxAttempts: 3, Lease: time.Minute},
			Columns: []string{"payload", "frozen_col"},
			Frozen:  []string{"frozen_col"},
		}
	}
	admit := func(t *testing.T, table, key string) (int64, bool, error) {
		t.Helper()
		var (
			id   int64
			dup  bool
			werr error
		)
		err := h.WithUser(t, user, org, func(tx *sql.Tx) error {
			id, dup, werr = workitem.Admit(ctx, tx, kind(table), org, key, map[string]any{
				"payload": "p", "frozen_col": 0,
			})
			return werr
		})
		if werr == nil && err != nil {
			t.Fatalf("admit %s: commit: %v", table, err)
		}
		return id, dup, werr
	}

	t.Run("WritablePolicySetAdmitsAndDeduplicates", func(t *testing.T) {
		first, dup, err := admit(t, rlsWritableTable, "k")
		if err != nil || dup {
			t.Fatalf("first admit: id=%d dup=%v err=%v", first, dup, err)
		}
		second, dup, err := admit(t, rlsWritableTable, "k")
		if err != nil {
			t.Fatalf("deduplicating admit under the read/write pair: %v", err)
		}
		if !dup || second != first {
			t.Fatalf("second admit: id=%d dup=%v, want %d/true", second, dup, first)
		}
	})

	t.Run("AppendOnlyPolicySetRefusesTheConflictArm", func(t *testing.T) {
		if _, dup, err := admit(t, rlsAppendOnlyTable, "k"); err != nil || dup {
			t.Fatalf("first admit: dup=%v err=%v", dup, err)
		}
		_, _, err := admit(t, rlsAppendOnlyTable, "k")
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Fatalf("deduplicating admit without an UPDATE policy = %v, want an insufficient_privilege (42501) row-security error", err)
		}
	})

	// The same missing policy silences every other operation instead. A guarded
	// write is an UPDATE, and an UPDATE with no policy admitting it matches no
	// rows: Claim leases nothing, a holder's completion lands nowhere, and both
	// look exactly like an empty queue. Reverting admission to a shape the
	// INSERT policy covers would only hide the first symptom.
	t.Run("AppendOnlyPolicySetSilencesGuardedWrites", func(t *testing.T) {
		for table, wantRows := range map[string]int64{rlsWritableTable: 1, rlsAppendOnlyTable: 0} {
			var affected int64
			if err := h.WithUser(t, user, org, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, fmt.Sprintf(
					"UPDATE %s SET status = 'leased' WHERE org_id = $1 AND status = 'ready'", table), org)
				if err != nil {
					return err
				}
				affected, err = res.RowsAffected()
				return err
			}); err != nil {
				t.Fatalf("guarded write on %s: %v", table, err)
			}
			if affected != wantRows {
				t.Fatalf("guarded write on %s affected %d rows, want %d", table, affected, wantRows)
			}
		}
	})
}
