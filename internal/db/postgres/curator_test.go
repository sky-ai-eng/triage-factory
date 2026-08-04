package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCuratorStore_Postgres_Conformance runs the shared curator suite over
// the production two-pool store (admin + RLS-active app pool): every
// claims-bound call rides SyntheticClaimsWithTx under a real user's
// identity, so the private-visibility RLS arm is live for the whole suite.
func TestCuratorStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunCuratorStoreConformance(t, func(t *testing.T) dbtest.CuratorHarness {
		h := pgtest.Shared(t)
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
		return dbtest.CuratorHarness{
			Stores: stores,
			OrgID:  orgID,
			UserID: userID,
			SeedProject: func(t *testing.T, name string) string {
				t.Helper()
				return seedCuratorPgProject(t, h, orgID, userID, teamID, name)
			},
		}
	})
}

// TestCuratorStore_Postgres_PrivateVisibility_SelfOnly pins the RLS arm the
// whole curator model leans on: a curator conversation is
// visibility='private', so a SECOND user in the SAME org cannot read (or
// reach through to) another user's conversation, messages, or claims — and
// cannot write into them. The System variants on the admin pool are what
// the claim loop / sweeps / provisioner use to cross that boundary
// deliberately.
func TestCuratorStore_Postgres_PrivateVisibility_SelfOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, alice, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	bob := seedPgMember(t, h, orgID, "bob", "member")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, bob, teamID)
	projectID := seedCuratorPgProject(t, h, orgID, alice, teamID, "shared")

	// Alice's turn: conversation + queued message + one delivered turn with
	// a streamed row.
	var convID string
	var msgID int64
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(ctx, orgID, projectID, alice)
		if err != nil {
			return err
		}
		convID = conv.ID
		msgID, err = ts.Curator.EnqueueUserMessage(ctx, orgID, conv.ID, alice, "alice's turn")
		return err
	}); err != nil {
		t.Fatalf("alice seed: %v", err)
	}
	claimed, err := stores.RunQueue.ClaimNextRun(ctx, "exec-1", 1, db.ClaimPlacement{})
	if err != nil || claimed == nil || claimed.ID != convID {
		t.Fatalf("claim = (%+v, %v), want conversation %s", claimed, err, convID)
	}
	claimID := claimed.ClaimID
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		if _, err := ts.Curator.BeginTurn(ctx, orgID, projectID, convID, msgID); err != nil {
			return err
		}
		_, err := ts.Conversations.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: convID, UserID: alice, ClaimID: claimID,
			Role: "assistant", Content: "private ack",
		})
		return err
	}); err != nil {
		t.Fatalf("alice turn: %v", err)
	}

	// Bob, same org: alice's conversation is invisible in every read shape.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		if conv, err := ts.Curator.GetLiveConversation(ctx, orgID, projectID, alice); err != nil {
			return err
		} else if conv != nil {
			t.Errorf("bob read alice's conversation %s — private-visibility RLS leak", conv.ID)
		}
		if msgs, err := ts.Curator.ListConversationMessages(ctx, orgID, convID); err != nil {
			return err
		} else if len(msgs) != 0 {
			t.Errorf("bob read %d of alice's messages — private-visibility RLS leak", len(msgs))
		}
		if claims, err := ts.Curator.ListClaims(ctx, orgID, convID); err != nil {
			return err
		} else if len(claims) != 0 {
			t.Errorf("bob read %d of alice's claims — private-visibility RLS leak", len(claims))
		}
		if turn, err := ts.Curator.InFlightTurn(ctx, orgID, projectID, alice); err != nil {
			return err
		} else if turn != nil {
			t.Errorf("bob located alice's in-flight turn %+v — private-visibility RLS leak", turn)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob reads: %v", err)
	}

	// Bob cannot write into alice's conversation either: the messages RLS
	// WITH CHECK composes through the conversation's private arm.
	writeErr := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		_, err := ts.Curator.EnqueueUserMessage(ctx, orgID, convID, bob, "bob intrudes")
		return err
	})
	if writeErr == nil {
		t.Error("bob inserted a message into alice's conversation — private-visibility RLS leak")
	}
	// And cannot delete her queued rows or archive her conversation.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		deleted, err := ts.Curator.DeleteQueuedTurn(ctx, orgID, convID, msgID)
		if err != nil {
			return err
		}
		if deleted {
			t.Error("bob deleted alice's queued turn — private-visibility RLS leak")
		}
		_, e := ts.Curator.ArchiveLiveConversation(ctx, orgID, projectID, alice)
		return e
	}); err != nil {
		t.Fatalf("bob writes: %v", err)
	}
	var archived bool
	if err := h.AdminDB.QueryRow(
		`SELECT archived_at IS NOT NULL FROM conversations WHERE id = $1`, convID,
	).Scan(&archived); err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if archived {
		t.Error("bob archived alice's conversation — private-visibility RLS leak")
	}

	// Bob has his own parallel private conversation on the same project —
	// per-creator, not per-project.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(ctx, orgID, projectID, bob)
		if err != nil {
			return err
		}
		if conv.ID == convID {
			t.Error("bob's find-or-mint returned alice's conversation")
		}
		return nil
	}); err != nil {
		t.Fatalf("bob mint: %v", err)
	}

	// The System doors DO cross the boundary — that's the sweeps /
	// provisioner contract.
	if flipped, err := stores.Curator.ReleaseActiveTurnSystem(ctx, orgID, convID, "cancelled", "sweep", 0, 0, 0); err != nil || !flipped {
		t.Fatalf("system release across users: flipped=%v err=%v, want the admin door to reach it", flipped, err)
	}
}

// seedCuratorPgProject inserts a project row via the admin pool (a bare
// test context has no JWT claims, so the app-pool projects_insert RLS
// would reject it).
func seedCuratorPgProject(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, name string) string {
	t.Helper()
	id := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, id, orgID, userID, teamID, name)
	return id
}
