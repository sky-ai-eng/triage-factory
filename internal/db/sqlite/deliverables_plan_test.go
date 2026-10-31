package sqlite_test

import (
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDeliverableStore_SQLite_PresentExternalIDsPlan pins that the GitHub poll's
// read of which reviews TF submitted searches idx_deliverables_external_id rather
// than walking every deliverable the org holds on the host.
func TestDeliverableStore_SQLite_PresentExternalIDsPlan(t *testing.T) {
	conn := openSQLiteForTest(t)
	rows, err := conn.Query(`EXPLAIN QUERY PLAN `+sqlitestore.PresentExternalIDsQueryForTest,
		runmode.LocalDefaultOrgID, "https://github.com", domain.ProviderGitHub, domain.DeliverableKindReview, `["6000000001","6000000002"]`)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	joined := strings.Join(plan, "\n")
	if want := "INDEX idx_deliverables_external_id (org_id=? AND scope=? AND kind=? AND external_id=?)"; !strings.Contains(joined, want) {
		t.Errorf("query plan lacks %q:\n%s", want, joined)
	}
}
