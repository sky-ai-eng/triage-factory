package sqlite_test

import (
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_SQLite_DelegatorNoticePlans pins that the brain's two
// delegator-notice reads search their partial indexes, which hold only the
// rows still owed a notice, rather than walking every failed or open
// conversation.
func TestConversationStore_SQLite_DelegatorNoticePlans(t *testing.T) {
	conn := openSQLiteForTest(t)
	var reasons []any
	for _, r := range domain.ParkReasonsAwaitingPerson() {
		reasons = append(reasons, string(r))
	}
	for _, tc := range []struct {
		name, query, index string
		args               []any
	}{
		{"pause", sqlitestore.PauseNoticesDueQueryForTest, "idx_conversations_pause_notice_due", append(reasons, 100)},
		{"failure", sqlitestore.FailureNoticesDueQueryForTest, "idx_conversations_failure_notice_due", []any{100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := conn.Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
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
			if want := "INDEX " + tc.index; !strings.Contains(joined, want) {
				t.Errorf("query plan lacks %q:\n%s", want, joined)
			}
		})
	}
}
