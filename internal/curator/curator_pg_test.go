package curator

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestCurator_Postgres_SystemRelease_ReleasesActiveClaim pins the
// system-side cancel door the shutdown / handler-backstop paths use:
// releasing one tenant's active curator claim via the admin pool flips
// exactly that engagement to cancelled and leaves another tenant's live
// engagement untouched. Under RLS with no claims the app pool couldn't
// write claims at all — the System door is the only cancel write.
func TestCurator_Postgres_SystemRelease_ReleasesActiveClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, teamB := pgtest.SeedOrgWithUser(t, h, "bob")
	projectA := seedPgProject(t, h, orgA, alice, teamA, "inflight-a")
	projectB := seedPgProject(t, h, orgB, bob, teamB, "inflight-b")

	convA, msgA := seedTurn(t, ctx, stores, orgA, alice, projectA, "running-a")
	convB, msgB := seedTurn(t, ctx, stores, orgB, bob, projectB, "running-b")
	beginSeededTurn(t, ctx, stores, orgA, alice, projectA, convA, msgA)
	beginSeededTurn(t, ctx, stores, orgB, bob, projectB, convB, msgB)

	flipped, err := stores.Curator.ReleaseActiveTurnSystem(ctx, orgA, convA, "cancelled", "process shutting down", 0, 0, 0)
	if err != nil {
		t.Fatalf("system release A: %v", err)
	}
	if !flipped {
		t.Fatal("ReleaseActiveTurnSystem did not release a live claim; the admin-pool door should always reach it")
	}
	if got := turnStatusPg(t, h, msgA); got != "cancelled" {
		t.Errorf("turn A status = %q, want cancelled", got)
	}
	if got := turnStatusPg(t, h, msgB); got != "running" {
		t.Errorf("turn B status = %q, want running (cross-tenant engagement must be untouched)", got)
	}

	// The release is exactly-once: a second system release finds no live
	// claim and reports no flip.
	flipped, err = stores.Curator.ReleaseActiveTurnSystem(ctx, orgA, convA, "cancelled", "duplicate", 0, 0, 0)
	if err != nil {
		t.Fatalf("second system release A: %v", err)
	}
	if flipped {
		t.Error("second ReleaseActiveTurnSystem flipped, want no-op on an already-released claim")
	}
}

// seedPgProject inserts a project row via the admin pool (no JWT claims
// in a bare test context, so the app-pool projects_insert RLS would
// reject it). Mirrors seedPgEntityProject in the postgres test package.
func seedPgProject(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, name string) string {
	t.Helper()
	id := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, id, orgID, userID, teamID, name)
	return id
}

// seedTurn records a queued turn under the requesting user's synthetic
// claims — the production SendMessage path: find-or-mint the conversation,
// insert the undelivered user message.
func seedTurn(t *testing.T, ctx context.Context, stores db.Stores, orgID, userID, projectID, input string) (conversationID string, messageID int64) {
	t.Helper()
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(ctx, orgID, projectID, userID)
		if err != nil {
			return err
		}
		conversationID = conv.ID
		id, err := ts.Curator.EnqueueUserMessage(ctx, orgID, conv.ID, userID, input)
		if err != nil {
			return err
		}
		messageID = id
		return nil
	}); err != nil {
		t.Fatalf("seed turn %q: %v", input, err)
	}
	return conversationID, messageID
}

// beginSeededTurn claims + delivers a seeded turn — the dispatch's opening
// two steps — leaving the conversation with a live engagement.
func beginSeededTurn(t *testing.T, ctx context.Context, stores db.Stores, orgID, userID, projectID, conversationID string, messageID int64) string {
	t.Helper()
	claimID := claimSeededTurn(t, ctx, stores, conversationID)
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		_, err := ts.Curator.BeginTurn(ctx, orgID, projectID, conversationID, messageID)
		return err
	}); err != nil {
		t.Fatalf("begin turn: %v", err)
	}
	return claimID
}

// turnStatusPg derives a turn's wire status from the raw rows via the admin
// pool (bypassing RLS so the assertion sees true state regardless of
// tenant): queued while the user message is undelivered, otherwise the
// conversation's latest claim decides.
func turnStatusPg(t *testing.T, h *pgtest.Harness, messageID int64) string {
	t.Helper()
	var status string
	if err := h.AdminDB.QueryRow(`
		SELECT CASE
		    WHEN m.delivered = false THEN 'queued'
		    ELSE COALESCE((
		        SELECT CASE
		            WHEN cl.released_at IS NULL THEN 'running'
		            WHEN cl.outcome = 'completed' THEN 'done'
		            ELSE cl.outcome
		        END
		        FROM claims cl
		        WHERE cl.conversation_id = m.conversation_id
		        ORDER BY cl.claimed_at DESC, cl.created_at DESC
		        LIMIT 1
		    ), 'done')
		END
		FROM messages m WHERE m.id = $1
	`, messageID).Scan(&status); err != nil {
		t.Fatalf("read turn status %d: %v", messageID, err)
	}
	return status
}

// claimSeededTurn drives the ONE claim loop far enough to own the
// conversation's oldest queued turn, returning the minted claim id — the
// production path now that there is no curator-only claim door.
func claimSeededTurn(t *testing.T, ctx context.Context, stores db.Stores, conversationID string) string {
	t.Helper()
	got, err := stores.RunQueue.ClaimNextRun(ctx, "test-executor", 1, db.ClaimPlacement{})
	if err != nil || got == nil || got.ID != conversationID {
		t.Fatalf("ClaimNextRun = (%+v, %v), want a claim on conversation %s", got, err, conversationID)
	}
	return got.ClaimID
}
