package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgCommitSignerStore_SQLite_Conformance runs the shared contract against
// the SQLite impl, on the local sentinel org and user BootstrapSchemaForTest
// seeds for the foreign keys.
func TestOrgCommitSignerStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunOrgCommitSignerStoreConformance(t, func(t *testing.T) (db.OrgCommitSignerStore, string, string) {
		conn := openSQLiteForTest(t)
		return sqlitestore.New(conn).OrgCommitSigners, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	})
}

// TestCommitSignatureStore_SQLite_Conformance runs the shared signing-log
// contract against the SQLite impl, seeding the task → conversation → claim
// chain the way the workspace-snapshot suite does.
func TestCommitSignatureStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunCommitSignatureStoreConformance(t, func(t *testing.T) (db.CommitSignatureStore, string, dbtest.CommitSignatureSeeder) {
		conn := openSQLiteForTest(t)
		seed := dbtest.CommitSignatureSeeder{
			Claim: func(t *testing.T) (string, string) {
				t.Helper()
				taskID := seedSQLiteTaskForSnapshot(t, conn, uuid.New().String()[:8])
				conversationID := uuid.New().String()
				dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID, TaskID: taskID})
				return conversationID, seedSQLiteClaimForSnapshot(t, conn, conversationID, time.Now(), false)
			},
			DeleteConversation: func(t *testing.T, conversationID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
					t.Fatalf("delete conversation: %v", err)
				}
			},
		}
		return sqlitestore.New(conn).CommitSignatures, runmode.LocalDefaultOrgID, seed
	})
}

// TestCommitSignatureQueue_SQLite_Conformance runs the control-plane queue's
// contract against the SQLite impl. Local mode never queues a request, but the
// store answers the interface the same way in both dialects.
func TestCommitSignatureQueue_SQLite_Conformance(t *testing.T) {
	dbtest.RunCommitSignatureQueueConformance(t, func(t *testing.T) dbtest.CommitSignatureQueueFixture {
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		return dbtest.CommitSignatureQueueFixture{
			Signatures: stores.CommitSignatures,
			Claims:     stores.ConversationQueue,
			OrgID:      runmode.LocalDefaultOrgID,
			Claim: func(t *testing.T, claimedAt time.Time, released bool) (string, string) {
				t.Helper()
				taskID := seedSQLiteTaskForSnapshot(t, conn, uuid.New().String()[:8])
				conversationID := uuid.New().String()
				dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID, TaskID: taskID})
				return conversationID, seedSQLiteClaimForSnapshot(t, conn, conversationID, claimedAt, released)
			},
		}
	})
}
