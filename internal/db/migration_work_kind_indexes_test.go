package db

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// The SQLite migration that moves each kind's table onto the work-item block,
// and the status literals and index names its previous lifecycle used, which
// the Postgres baseline's block for the table must no longer carry.
var workKindSchemas = []struct {
	name       string
	sqliteFile string
	kind       func(workitem.Dialect) workitem.Kind
	old        []string
}{
	{
		name:       "event_queue",
		sqliteFile: eventQueueWorkItemFile,
		kind:       workkinds.EventQueue,
		old:        []string{"'pending'", "'processing'", "idx_event_queue_pending", "idx_event_queue_status_processed"},
	},
	{
		name:       "pending_firings",
		sqliteFile: pendingFiringsWorkItemFile,
		kind:       workkinds.PendingFirings,
		old:        []string{"'pending'", "'draining'", "'fired'", "'skipped_stale'", "idx_pending_firings_dedup", "idx_pending_firings_entity_pending"},
	},
	{
		name:       "task_rederive_queue",
		sqliteFile: taskReDeriveQueueFile,
		kind:       workkinds.TaskReDerive,
	},
}

// TestWorkKindSchemasCarryWorkItemIndexes pins that both dialects' schema
// files carry every statement workitem.IndexDDL renders for each kind — the
// SQLite migration verbatim, the Postgres baseline with the table named
// through its schema like every statement beside it — so the index shape the
// claim relies on cannot drift from the package that owns it. The Postgres
// block for each table must also carry none of its previous lifecycle's
// status literals or index names.
func TestWorkKindSchemasCarryWorkItemIndexes(t *testing.T) {
	const baseline = "migrations-postgres/202605130001_pg_baseline.sql"
	for _, ks := range workKindSchemas {
		for _, tc := range []struct {
			name    string
			fsys    fs.FS
			file    string
			dialect workitem.Dialect
		}{
			{"postgres baseline", migrationsPostgresFS, baseline, workitem.Postgres},
			{"sqlite migration", migrationsSQLiteFS, ks.sqliteFile, workitem.SQLite},
		} {
			t.Run(ks.name+"/"+tc.name, func(t *testing.T) {
				raw, err := fs.ReadFile(tc.fsys, tc.file)
				if err != nil {
					t.Fatalf("read %s: %v", tc.file, err)
				}
				kind := ks.kind(tc.dialect)
				for i, stmt := range workitem.IndexDDL(kind) {
					want := stmt
					if tc.dialect == workitem.Postgres {
						want = strings.Replace(stmt, " ON "+kind.Table+" (", " ON public."+kind.Table+" (", 1)
					}
					if !strings.Contains(string(raw), want+";") {
						t.Errorf("%s lacks index %s:\n%s", tc.file, workitem.IndexNames(kind)[i], want)
					}
				}
				if tc.dialect != workitem.Postgres {
					return
				}
				block := tableBlock(string(raw), kind.Table)
				if block == "" {
					t.Fatalf("the baseline has no CREATE TABLE public.%s", kind.Table)
				}
				for _, old := range ks.old {
					if strings.Contains(block, old) {
						t.Errorf("the baseline's %s block still carries %s", kind.Table, old)
					}
				}
			})
		}
	}
}

// tableBlock cuts the baseline down to one table's own statements, from its
// CREATE TABLE to the next table's.
func tableBlock(baseline, table string) string {
	start := strings.Index(baseline, "CREATE TABLE public."+table+" (")
	if start < 0 {
		return ""
	}
	rest := baseline[start:]
	if end := strings.Index(rest[1:], "CREATE TABLE public."); end >= 0 {
		return rest[:end+1]
	}
	return rest
}
