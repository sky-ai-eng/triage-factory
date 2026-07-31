package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The claim fence: the DB-layer backstop that refuses an executor's writes
// once its claim has been released. Postgres-only — the race it guards needs
// two executors and a reaper, neither of which exists in local mode.
//
// Everything here is driven through the public store methods. The reaper's
// side is a raw UPDATE on a separate connection, which is what it genuinely
// is from the fenced writer's point of view: another process, another
// transaction, taking the row lock.

// fenceFixture is one seeded conversation with one live claim on it.
type fenceFixture struct {
	store   db.ConversationStore
	orgID   string
	runID   string
	claimID string
	seed    dbtest.ConversationSeeder
}

func newFenceFixture(t *testing.T, h *pgtest.Harness, executorID string) fenceFixture {
	t.Helper()
	h.Reset(t)
	orgID, userID, agentID := seedPgConversationOrg(t, h)
	promptID := seedPgConversationPrompt(t, h, orgID, userID)
	seed := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	store := stores.Conversations

	ent := seed.Entity(t, "fence")
	ev := seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	runID := seed.Run(t, domain.Conversation{
		TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	})
	if err := store.SetExecutorSystem(context.Background(), orgID, runID, executorID, 1); err != nil {
		t.Fatalf("mint claim: %v", err)
	}
	claims := seed.ClaimRows(t, runID)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want exactly the one minted engagement", claims)
	}
	return fenceFixture{store: store, orgID: orgID, runID: runID, claimID: claims[0].ID, seed: seed}
}

// reap is the fleet reaper's release, from a connection of its own.
func reap(t *testing.T, conn *sql.DB, orgID, runID string) {
	t.Helper()
	res, err := conn.Exec(`
		UPDATE claims SET released_at = now(), outcome = 'reaped'
		WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
	`, orgID, runID)
	if err != nil {
		t.Fatalf("reap claim: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("reap released %d claims, want 1", n)
	}
}

// TestClaimFence_ReleasedClaimRefusesEveryEngagementWrite: the fenced write
// set, each driven once with a live claim and once after a reap. The whole
// point of the fence is that the second call is a refusal rather than a
// silently-corrupting success, so every method is asserted to both work and
// refuse.
func TestClaimFence_ReleasedClaimRefusesEveryEngagementWrite(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()

	t.Run("InsertMessage", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-insert")
		if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.runID, Role: "assistant", Subtype: "text", Content: "live",
		}); err != nil {
			t.Fatalf("insert while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.runID)

		_, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.runID, Role: "assistant", Subtype: "text", Content: "zombie",
		})
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("insert after reap = %v, want ErrClaimReleased", err)
		}
		msgs, err := fx.store.Messages(ctx, fx.orgID, fx.runID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Content != "live" {
			t.Fatalf("transcript = %+v, want only the row written while the claim was live", msgs)
		}
	})

	t.Run("MarkDelivered", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-deliver")
		pending := false
		id, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.runID, Role: "user", Content: "pending", Delivered: &pending,
		})
		if err != nil {
			t.Fatalf("insert pending row: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.runID)

		if err := fx.store.MarkDeliveredForClaimSystem(ctx, fx.orgID, fx.runID, fx.claimID, []int{int(id)}); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("mark-delivered after reap = %v, want ErrClaimReleased", err)
		}
		// The row is still pending, so the successor's assembly still sees it
		// — which is the corruption the refusal prevented.
		msgs, err := fx.store.ListForAssembly(ctx, fx.orgID, fx.runID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListForAssembly = %+v (err %v)", msgs, err)
		}
		if msgs[0].Delivered == nil || *msgs[0].Delivered {
			t.Errorf("row delivered = %v, want still pending", msgs[0].Delivered)
		}
	})

	t.Run("Complete", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-complete")
		reap(t, h.AdminDB, fx.orgID, fx.runID)

		err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.runID, fx.claimID, "completed", 1.5, 100, 2, "ok", "done", "finish", "", "")
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("complete after reap = %v, want ErrClaimReleased", err)
		}
		got, err := fx.store.Get(ctx, fx.orgID, fx.runID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "running" {
			t.Errorf("status = %q, want running (the refused terminal must not land)", got.Status)
		}
		if got.CompletedAt != nil {
			t.Errorf("completed_at stamped by a refused terminal: %v", got.CompletedAt)
		}
		if claims := fx.seed.ClaimRows(t, fx.runID); claims[0].Outcome != "reaped" {
			t.Errorf("claim outcome = %q, want reaped (the refused terminal must not rewrite it)", claims[0].Outcome)
		}
	})

	t.Run("MarkFailed", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-fail")
		reap(t, h.AdminDB, fx.orgID, fx.runID)

		ok, err := fx.store.MarkFailedIfActiveForClaimSystem(ctx, fx.orgID, fx.runID, fx.claimID, string(domain.RunFailureCrash))
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("mark-failed after reap = (%v, %v), want ErrClaimReleased", ok, err)
		}
		if ok {
			t.Error("mark-failed after reap reported a flip")
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.runID); got.Status != "running" {
			t.Errorf("status = %q, want running (the refused terminal must not land)", got.Status)
		}
	})

	t.Run("SetPhase", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-phase")
		if err := fx.store.SetClaimPhaseSystem(ctx, fx.orgID, fx.claimID, "cloning"); err != nil {
			t.Fatalf("phase while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.runID)

		if err := fx.store.SetClaimPhaseSystem(ctx, fx.orgID, fx.claimID, "agent_starting"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("phase after reap = %v, want ErrClaimReleased", err)
		}
		if claims := fx.seed.ClaimRows(t, fx.runID); claims[0].Phase != "cloning" {
			t.Errorf("phase = %q, want cloning (the refused write must not land)", claims[0].Phase)
		}
	})

	t.Run("UnknownClaimIsRefusedLikeAReleasedOne", func(t *testing.T) {
		// A claim id from another org, a malformed one, or none at all: the
		// fence is asking one question — does this caller own the row — and
		// every id that fails to resolve answers it the same way.
		fx := newFenceFixture(t, h, "exec-fence-unknown")
		for name, claimID := range map[string]string{
			"empty":     "",
			"malformed": "not-a-uuid",
			"unknown":   "6f1b7d3e-0000-4000-8000-000000000000",
		} {
			_, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, claimID, &domain.Message{
				ConversationID: fx.runID, Role: "assistant", Content: name,
			})
			if !errors.Is(err, db.ErrClaimReleased) {
				t.Errorf("insert with %s claim id = %v, want ErrClaimReleased", name, err)
			}
		}
	})
}

// TestClaimFence_SerializesAgainstAConcurrentRelease is the property a plain
// EXISTS check cannot provide, on two connections.
//
// The reaper holds its release open, so it owns the claim row's lock. A
// fenced write starting in that window must BLOCK on the locking read rather
// than read around it — and once the release commits, it must see the
// release rather than the version its own statement snapshot started with.
// An unlocked existence check would do neither: it would return immediately,
// observe an unreleased claim, and let the zombie's row land.
func TestClaimFence_SerializesAgainstAConcurrentRelease(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()
	fx := newFenceFixture(t, h, "exec-fence-race")

	// The reaper's release, held open on its own connection.
	reaperTx, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin reaper tx: %v", err)
	}
	defer func() { _ = reaperTx.Rollback() }()
	if _, err := reaperTx.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = 'reaped'
		WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
	`, fx.orgID, fx.runID); err != nil {
		t.Fatalf("reaper release: %v", err)
	}

	// The zombie's write, racing it.
	done := make(chan error, 1)
	go func() {
		_, werr := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.runID, Role: "assistant", Subtype: "text", Content: "zombie",
		})
		done <- werr
	}()

	select {
	case werr := <-done:
		t.Fatalf("fenced write returned (%v) while the release held the row lock; it must block on the locking read", werr)
	case <-time.After(750 * time.Millisecond):
		// Blocked, as required.
	}

	if err := reaperTx.Commit(); err != nil {
		t.Fatalf("commit reaper tx: %v", err)
	}

	select {
	case werr := <-done:
		if !errors.Is(werr, db.ErrClaimReleased) {
			t.Fatalf("fenced write after the release committed = %v, want ErrClaimReleased", werr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fenced write never returned after the release committed")
	}

	msgs, err := fx.store.Messages(ctx, fx.orgID, fx.runID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("transcript = %+v, want nothing written by the losing engagement", msgs)
	}
}

// TestClaimFence_SuccessorWritesWhileTheZombieIsRefused is the full
// requeue-and-reclaim shape: reap, hand the conversation to a second
// executor, and then have both engagements write. Only one of them owns the
// row, and the DB is what decides which.
func TestClaimFence_SuccessorWritesWhileTheZombieIsRefused(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()
	fx := newFenceFixture(t, h, "exec-zombie")
	zombieClaim := fx.claimID

	reap(t, h.AdminDB, fx.orgID, fx.runID)
	// The requeued conversation is claimed again, by whoever picks it up.
	if err := fx.store.SetExecutorSystem(ctx, fx.orgID, fx.runID, "exec-successor", 7); err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	claims := fx.seed.ClaimRows(t, fx.runID)
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want the reaped engagement plus its successor", claims)
	}
	successorClaim := claims[1].ID
	if successorClaim == zombieClaim {
		t.Fatal("successor reused the reaped claim id")
	}

	if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, successorClaim, &domain.Message{
		ConversationID: fx.runID, Role: "assistant", Subtype: "text", Content: "successor",
	}); err != nil {
		t.Fatalf("successor insert: %v", err)
	}
	if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, zombieClaim, &domain.Message{
		ConversationID: fx.runID, Role: "assistant", Subtype: "text", Content: "zombie",
	}); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie insert = %v, want ErrClaimReleased", err)
	}
	// The zombie's terminal is refused too, so the successor keeps driving a
	// conversation that is still `running` rather than one buried under a
	// stale completion.
	if err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.runID, zombieClaim, "failed", 0, 0, 0, "crash", "", "", "", string(domain.RunFailureCrash)); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie complete = %v, want ErrClaimReleased", err)
	}

	got, err := fx.store.Get(ctx, fx.orgID, fx.runID)
	if err != nil || got == nil {
		t.Fatalf("Get: err=%v got=%v", err, got)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running (the successor is still driving)", got.Status)
	}
	if got.ExecutorID != "exec-successor" {
		t.Errorf("executor = %q, want exec-successor", got.ExecutorID)
	}
	msgs, err := fx.store.Messages(ctx, fx.orgID, fx.runID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "successor" {
		t.Fatalf("transcript = %+v, want only the successor's row (no interleaving)", msgs)
	}
	if msgs[0].ClaimID != successorClaim {
		t.Errorf("row claim_id = %q, want the successor's claim %q", msgs[0].ClaimID, successorClaim)
	}

	// And the successor's own terminal still works — the fence refuses
	// zombies, not owners.
	if err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.runID, successorClaim, "completed", 0.25, 900, 1, "ok", "done", "finish", "", ""); err != nil {
		t.Fatalf("successor complete: %v", err)
	}
	if got, _ := fx.store.Get(ctx, fx.orgID, fx.runID); got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
}
