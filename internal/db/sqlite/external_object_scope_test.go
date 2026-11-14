package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestExternalActionPerformedBy_SQLite runs the shared ledger performed_by
// conformance suite against the SQLite backend, on the node-id suite's
// fixture.
func TestExternalActionPerformedBy_SQLite(t *testing.T) {
	dbtest.RunExternalActionPerformedByConformance(t, sqliteExternalActionFixture)
}

// TestExternalActionObject_SQLite runs the shared ledger object-identity
// conformance suite against the SQLite backend, on the node-id suite's
// fixture.
func TestExternalActionObject_SQLite(t *testing.T) {
	dbtest.RunExternalActionObjectConformance(t, sqliteExternalActionFixture)
}

// TestExternalActionCommentKeys_SQLite runs the shared conformance suite for
// the tracker's read of which comments TF posted against the SQLite backend,
// on the node-id suite's fixture.
func TestExternalActionCommentKeys_SQLite(t *testing.T) {
	dbtest.RunExternalActionCommentKeysConformance(t, sqliteExternalActionFixture)
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
		ObjectIDOf: func(t *testing.T, actionID string) (string, bool) {
			t.Helper()
			var id sql.NullString
			if err := conn.QueryRow(`SELECT `+sqlitestore.ActionObjectIDSQLForTest+` FROM external_actions ea WHERE ea.id = ?`, actionID).Scan(&id); err != nil {
				t.Fatalf("object id of %s: %v", actionID, err)
			}
			return id.String, id.Valid
		},
	}
}

// TestArtifacts_SQLite_SameSecondRowsBreakInWriteOrder pins the tie-break
// SQLite needs and Postgres does not: occurred_at is whole seconds here, so two
// rows on one object in one second are told apart by write order, never by
// their random ids. The edit is written second under the smaller id, and it
// is both the artifact's last action and the first row of its action list.
func TestArtifacts_SQLite_SameSecondRowsBreakInWriteOrder(t *testing.T) {
	ctx := t.Context()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	conv := seedDeliverableConversationWithID(t, conn, uuid.NewString())
	for _, row := range []struct{ id, action string }{
		{"ffffffff-0000-4000-8000-000000000000", domain.ActionCommentPosted},
		{"00000000-0000-4000-8000-000000000000", domain.ActionCommentEdited},
	} {
		if err := stores.ExternalActions.Record(ctx, runmode.LocalDefaultOrgID, domain.ExternalAction{
			ID: row.id, TeamID: runmode.LocalDefaultTeamID, ConversationID: conv,
			Provider: domain.ProviderGitHub, Scope: dbtest.TestGitHubHost, Action: row.action, Target: "octo/api#7",
			Credential: domain.CredentialGitHubApp, PerformedBy: domain.PerformedByAgent,
			ObjectKind: domain.ObjectKindComment, ObjectKey: domain.GitHubCommentKey(domain.GitHubIssueComment, 5),
		}); err != nil {
			t.Fatalf("record %s: %v", row.action, err)
		}
	}
	if _, err := conn.Exec(`UPDATE external_actions SET occurred_at = '2026-10-01 12:00:00'`); err != nil {
		t.Fatalf("put both rows in one second: %v", err)
	}

	artifacts, _, err := stores.Artifacts.ListByConversation(ctx, runmode.LocalDefaultOrgID, conv, db.Unwindowed)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("ListByConversation = %+v, %v; want one artifact", artifacts, err)
	}
	if artifacts[0].LastAction != domain.ActionCommentEdited {
		t.Errorf("last action = %q, want the later write %q", artifacts[0].LastAction, domain.ActionCommentEdited)
	}
	rows, _, err := stores.ExternalActions.ListByConversation(ctx, runmode.LocalDefaultOrgID, conv, domain.ExternalActionListOpts{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("actions = %+v, %v; want two rows", rows, err)
	}
	if rows[0].Action != domain.ActionCommentEdited {
		t.Errorf("newest action = %q, want the later write %q", rows[0].Action, domain.ActionCommentEdited)
	}
}

// TestArtifacts_SQLite runs the shared artifact conformance suite against the
// SQLite backend, a fresh in-memory DB per subtest.
func TestArtifacts_SQLite(t *testing.T) {
	dbtest.RunArtifactConformance(t, func(t *testing.T) dbtest.ArtifactFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		otherTeam := uuid.NewString()
		if _, err := conn.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
			otherTeam, runmode.LocalDefaultOrgID, "artifacts-other", "Other"); err != nil {
			t.Fatalf("seed the other team: %v", err)
		}
		return dbtest.ArtifactFixture{
			Stores:      sqlitestore.New(conn),
			OrgID:       runmode.LocalDefaultOrgID,
			TeamID:      runmode.LocalDefaultTeamID,
			OtherTeamID: otherTeam,
			NewConversation: func(t *testing.T) string {
				return seedDeliverableConversationWithID(t, conn, uuid.NewString())
			},
			SetOccurredAt: func(t *testing.T, actionID string, at time.Time) {
				t.Helper()
				// The shape the column's CURRENT_TIMESTAMP default writes.
				if _, err := conn.Exec(`UPDATE external_actions SET occurred_at = ? WHERE id = ?`,
					at.UTC().Format(time.DateTime), actionID); err != nil {
					t.Fatalf("set occurred_at of %s: %v", actionID, err)
				}
			},
			SetDeliverableCreatedAt: func(t *testing.T, deliverableID string, at time.Time) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE deliverables SET created_at = ? WHERE id = ?`,
					at.UTC().Format(time.DateTime), deliverableID); err != nil {
					t.Fatalf("set created_at of %s: %v", deliverableID, err)
				}
			},
		}
	})
}
