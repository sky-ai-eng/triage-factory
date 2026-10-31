package sqlite_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestExternalObjectScope_SQLite runs the shared scope conformance suite for
// deliverables and the audit ledger against the SQLite backend, a fresh in-memory
// DB per subtest.
func TestExternalObjectScope_SQLite(t *testing.T) {
	dbtest.RunExternalObjectScopeConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}

// TestCommentDeliverables_SQLite runs the shared comment-deliverable conformance
// suite against the SQLite backend, a fresh in-memory DB per subtest.
func TestCommentDeliverables_SQLite(t *testing.T) {
	dbtest.RunCommentDeliverableConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}

// TestDeliverableUpsert_SQLite runs the shared deliverable upsert conformance suite
// against the SQLite backend, a fresh in-memory DB per subtest.
func TestDeliverableUpsert_SQLite(t *testing.T) {
	dbtest.RunDeliverableUpsertConformance(t, func(t *testing.T) (db.Stores, string, string) {
		t.Helper()
		return sqlitestore.New(openSQLiteForTest(t)), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}

// TestDeliverableConversationList_SQLite runs the shared conversation-scoped
// deliverable list conformance suite against the SQLite backend.
func TestDeliverableConversationList_SQLite(t *testing.T) {
	dbtest.RunDeliverableConversationListConformance(t, func(t *testing.T) dbtest.DeliverableListFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		return dbtest.DeliverableListFixture{
			Stores: sqlitestore.New(conn),
			OrgID:  runmode.LocalDefaultOrgID,
			TeamID: runmode.LocalDefaultTeamID,
			NewConversation: func(t *testing.T) string {
				return seedDeliverableConversationWithID(t, conn, uuid.NewString())
			},
		}
	})
}

// TestExternalActionNodeID_SQLite runs the shared ledger node-id conformance
// suite against the SQLite backend, a fresh in-memory DB per subtest.
func TestExternalActionNodeID_SQLite(t *testing.T) {
	dbtest.RunExternalActionNodeIDConformance(t, sqliteExternalActionFixture)
}

// TestExternalActionReferences_SQLite runs the shared ledger reference
// conformance suite against the SQLite backend, on the node-id suite's
// fixture.
func TestExternalActionReferences_SQLite(t *testing.T) {
	dbtest.RunExternalActionReferenceConformance(t, sqliteExternalActionFixture)
}

// sqliteExternalActionFixture is a fresh in-memory DB with the local org's
// team and a conversation in it.
func sqliteExternalActionFixture(t *testing.T) dbtest.ExternalActionNodeIDFixture {
	t.Helper()
	conn := openSQLiteForTest(t)
	return dbtest.ExternalActionNodeIDFixture{
		Stores:         sqlitestore.New(conn),
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		ConversationID: seedDeliverableConversation(t, conn),
		BackdateAction: func(t *testing.T, actionID string, age time.Duration) {
			t.Helper()
			// datetime('now', ...) renders the 'YYYY-MM-DD HH:MM:SS' shape
			// the column's CURRENT_TIMESTAMP default writes.
			if _, err := conn.Exec(`UPDATE external_actions SET occurred_at = datetime('now', ?) WHERE id = ?`,
				fmt.Sprintf("-%d seconds", int64(age.Seconds())), actionID); err != nil {
				t.Fatalf("backdate occurred_at of %s: %v", actionID, err)
			}
		},
	}
}
