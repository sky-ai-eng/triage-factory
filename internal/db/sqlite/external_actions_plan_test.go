package sqlite_test

import (
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestExternalActionStore_SQLite_PRCreatedWithoutDeliverablePlan pins that the
// PR backstop's read, which runs every reconcile cycle, narrows its ledger
// scan to one org's pr_created rows inside the window: the index search must
// bind org_id, action and occurred_at. A function around occurred_at (the
// feeds' datetime() form) leaves the search bound on org_id alone, and every
// cycle walks the org's whole ledger. The anti-join must search the deliverables'
// node id index rather than scan.
func TestExternalActionStore_SQLite_PRCreatedWithoutDeliverablePlan(t *testing.T) {
	conn := openSQLiteForTest(t)
	rows, err := conn.Query(`EXPLAIN QUERY PLAN `+sqlitestore.PRCreatedWithoutDeliverableQueryForTest,
		runmode.LocalDefaultOrgID, domain.ActionPRCreated, time.Now().UTC().Format(time.DateTime), "https://github.com")
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
	for _, want := range []string{
		"SEARCH ea USING INDEX idx_external_actions_org_action_occurred (org_id=? AND action=? AND occurred_at>?)",
		"INDEX idx_deliverables_node_id (org_id=? AND scope=? AND node_id=?)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("query plan lacks %q:\n%s", want, joined)
		}
	}
}

// TestExternalActionStore_SQLite_PresentPostedCommentKeysPlan pins that the
// poll's read of which comments TF posted searches the ledger's object-key
// index on the org, the scope and the keys rather than walking every row the
// org holds on the host.
func TestExternalActionStore_SQLite_PresentPostedCommentKeysPlan(t *testing.T) {
	conn := openSQLiteForTest(t)
	rows, err := conn.Query(`EXPLAIN QUERY PLAN `+sqlitestore.PresentPostedCommentKeysQueryForTest,
		runmode.LocalDefaultOrgID, "https://github.com", domain.ObjectKindComment, domain.ActionCommentPosted, domain.ActionIssueCommentPosted,
		`["github:comment:1","github:comment:review:2"]`)
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
	if want := "INDEX idx_external_actions_object_key (org_id=? AND scope=? AND object_key=?)"; !strings.Contains(joined, want) {
		t.Errorf("query plan lacks %q:\n%s", want, joined)
	}
}
