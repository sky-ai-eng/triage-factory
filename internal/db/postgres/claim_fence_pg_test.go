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
// once its claim has been released. The refusal itself is a both-dialect
// contract and lives in the conformance suite; what is Postgres-specific, and
// what this file covers, is the shape of the rival — a reaper releasing the
// claim from another connection while the writer is mid-transaction — and the
// row locking that keeps that release and the fenced write from interleaving.
//
// Everything here is driven through the public store methods. The reaper's
// side is a raw UPDATE on a separate connection, which is what it genuinely
// is from the fenced writer's point of view: another process, another
// transaction, taking the row lock.

// fenceFixture is one seeded conversation with one live claim on it.
type fenceFixture struct {
	store          db.ConversationStore
	orgID          string
	conversationID string
	claimID        string
	promptID       string
	seed           dbtest.ConversationSeeder
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
	conversationID := seed.Conversation(t, domain.Conversation{
		TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
		BlueprintRunID: seed.BlueprintRun(t, taskID),
	})
	if _, err := store.SetExecutorSystem(context.Background(), orgID, conversationID, executorID, 1); err != nil {
		t.Fatalf("mint claim: %v", err)
	}
	claims := seed.ClaimRows(t, conversationID)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want exactly the one minted engagement", claims)
	}
	return fenceFixture{store: store, orgID: orgID, conversationID: conversationID, claimID: claims[0].ID, promptID: promptID, seed: seed}
}

// seedSecondConversation adds another running conversation in the same org,
// so a claim can be pointed at a conversation that is not its own.
func seedSecondConversation(t *testing.T, fx fenceFixture) string {
	t.Helper()
	ent := fx.seed.Entity(t, "fence-other")
	ev := fx.seed.Event(t, ent, domain.EventGitHubPROpened)
	taskID := fx.seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	return fx.seed.Conversation(t, domain.Conversation{
		TaskID: taskID, PromptID: fx.promptID, Status: "running", Model: "m",
		BlueprintRunID: fx.seed.BlueprintRun(t, taskID),
	})
}

// reap is the fleet reaper's release, from a connection of its own.
func reap(t *testing.T, conn *sql.DB, orgID, conversationID string) {
	t.Helper()
	res, err := conn.Exec(`
		UPDATE claims SET released_at = now(), outcome = 'reaped'
		WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
	`, orgID, conversationID)
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
			ConversationID: fx.conversationID, Role: "assistant", Content: "live",
		}); err != nil {
			t.Fatalf("insert while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		_, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.conversationID, Role: "assistant", Content: "zombie",
		})
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("insert after reap = %v, want ErrClaimReleased", err)
		}
		msgs, err := fx.store.Messages(ctx, fx.orgID, fx.conversationID)
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
			ConversationID: fx.conversationID, Role: "user", Content: "pending", Delivered: &pending,
		})
		if err != nil {
			t.Fatalf("insert pending row: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		if err := fx.store.MarkDeliveredForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, []int{int(id)}, ""); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("mark-delivered after reap = %v, want ErrClaimReleased", err)
		}
		// The row is still pending, so the successor's assembly still sees it
		// — which is the corruption the refusal prevented.
		msgs, err := fx.store.ListForAssemblySystem(ctx, fx.orgID, fx.conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListForAssembly = %+v (err %v)", msgs, err)
		}
		if msgs[0].Delivered == nil || *msgs[0].Delivered {
			t.Errorf("row delivered = %v, want still pending", msgs[0].Delivered)
		}
	})

	t.Run("Compact", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-compact")
		spanID, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.conversationID, Role: "assistant", Content: "history",
		})
		if err != nil {
			t.Fatalf("insert span row: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		resultRow := &domain.Message{Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionResult, Content: "summary"}
		err = fx.store.CompactForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, nil, resultRow, []int{int(spanID)})
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("compact after reap = %v, want ErrClaimReleased", err)
		}
		// Nothing landed: the span is still active and no result row exists —
		// a zombie must not rewrite the successor's window.
		msgs, err := fx.store.ListForAssemblySystem(ctx, fx.orgID, fx.conversationID)
		if err != nil || len(msgs) != 1 || msgs[0].ID != int(spanID) {
			t.Fatalf("assembly after refused compact = %+v (err %v), want the untouched span row", msgs, err)
		}
	})

	t.Run("SettleCompactionRequest", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-settle")
		reqID, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.conversationID, Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: "please compact",
		})
		if err != nil {
			t.Fatalf("insert request row: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		cost := 0.1
		_, err = fx.store.SettleCompactionRequestForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID,
			int(reqID), 1000, 10, 0, 0, &cost, "no-parseable-summary")
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("settle after reap = %v, want ErrClaimReleased", err)
		}
		msgs, err := fx.store.Messages(ctx, fx.orgID, fx.conversationID)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("Messages = %+v (err %v)", msgs, err)
		}
		if msgs[0].CostUSD != nil || msgs[0].Metadata["compaction_failure"] != nil {
			t.Errorf("refused settle landed anyway: cost=%v metadata=%v", msgs[0].CostUSD, msgs[0].Metadata)
		}
	})

	t.Run("Complete", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-complete")
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		_, err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "completed", 1.5, 100, 2, "done", "finish", "", "")
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("complete after reap = %v, want ErrClaimReleased", err)
		}
		got, err := fx.store.Get(ctx, fx.orgID, fx.conversationID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.Status != "running" {
			t.Errorf("status = %q, want running (the refused terminal must not land)", got.Status)
		}
		if got.CompletedAt != nil {
			t.Errorf("completed_at stamped by a refused terminal: %v", got.CompletedAt)
		}
		if claims := fx.seed.ClaimRows(t, fx.conversationID); claims[0].Outcome != "reaped" {
			t.Errorf("claim outcome = %q, want reaped (the refused terminal must not rewrite it)", claims[0].Outcome)
		}
	})

	t.Run("MarkFailed", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-fail")
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		ok, err := fx.store.MarkFailedIfActiveForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, string(domain.ConversationFailureCrash))
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("mark-failed after reap = (%v, %v), want ErrClaimReleased", ok, err)
		}
		if ok {
			t.Error("mark-failed after reap reported a flip")
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.Status != "running" {
			t.Errorf("status = %q, want running (the refused terminal must not land)", got.Status)
		}
	})

	t.Run("ParkCancelled", func(t *testing.T) {
		// The self-cancel an executor writes when its own run's ctx is killed
		// — which is precisely what a partition self-fence trip does to every
		// live run it owns. A late self-fence must not cancel a successor's
		// conversation on its way out.
		fx := newFenceFixture(t, h, "exec-fence-cancel")
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		ok, err := fx.store.ParkOpenForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, db.ParkStopped("user_cancelled", "Run cancelled by user"))
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("park-cancelled after reap = (%v, %v), want ErrClaimReleased", ok, err)
		}
		if ok {
			t.Error("park-cancelled after reap reported a flip")
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.Status != "running" {
			t.Errorf("status = %q, want running (the refused write must not land)", got.Status)
		}
		// The unfenced twin is what a user's cancel uses, and it still works
		// on the same row — the fence gates the executor, not the person.
		flipped, err := fx.store.ParkOpenSystem(ctx, fx.orgID, fx.conversationID, db.ParkStopped("user_cancelled", "Run cancelled by user"))
		if err != nil || !flipped {
			t.Fatalf("unfenced user cancel = (%v, %v), want it to flip", flipped, err)
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.Status != "open" {
			t.Errorf("status = %q, want open (a cancel parks; it never writes a terminal of its own)", got.Status)
		}
	})

	t.Run("SetSession", func(t *testing.T) {
		// The sharpest of the resume-coordinate writes: sdk_session_id is what
		// the next wake reconnects to, and a zombie's late system/init would
		// overwrite the id the successor is resuming against.
		fx := newFenceFixture(t, h, "exec-fence-session")
		if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "sess-live"); err != nil {
			t.Fatalf("session while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "sess-zombie"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("session after reap = %v, want ErrClaimReleased", err)
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.SessionID != "sess-live" {
			t.Errorf("sdk_session_id = %q, want sess-live (the refused write must not land)", got.SessionID)
		}
	})

	t.Run("SetWorktreePath", func(t *testing.T) {
		// Setup outlives its claim more often than anything else here — a
		// clone or a cold rehydrate takes minutes — and the path is host-local,
		// so a late stamp points the successor's resume at a directory that
		// does not exist where the work moved to.
		fx := newFenceFixture(t, h, "exec-fence-worktree")
		if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "/tmp/triagefactory-runs/live"); err != nil {
			t.Fatalf("worktree path while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "/tmp/triagefactory-runs/zombie"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("worktree path after reap = %v, want ErrClaimReleased", err)
		}
		if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.WorktreePath != "/tmp/triagefactory-runs/live" {
			t.Errorf("worktree_path = %q, want /tmp/triagefactory-runs/live (the refused write must not land)", got.WorktreePath)
		}
		// The unfenced twin is the claimless door and is untouched by any of
		// this — a mint-time stamp has no claim to be measured against.
		if _, err := fx.store.SetWorktreePathSystem(ctx, fx.orgID, fx.conversationID, "/tmp/triagefactory-runs/unfenced"); err != nil {
			t.Fatalf("unfenced stamp: %v", err)
		}
	})

	t.Run("SetExecutor", func(t *testing.T) {
		// The go-live ownership stamp. A run reaped mid-setup whose process
		// then comes up would re-stamp the successor's claim with a dead
		// executor's identity — which the reaper reads back as executor loss.
		fx := newFenceFixture(t, h, "exec-fence-executor")
		if _, err := fx.store.SetExecutorForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "exec-fence-executor-live", 4); err != nil {
			t.Fatalf("executor stamp while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		if _, err := fx.store.SetExecutorForClaimSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "exec-zombie", 9); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("executor stamp after reap = %v, want ErrClaimReleased", err)
		}
		claims := fx.seed.ClaimRows(t, fx.conversationID)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want one (a refused stamp must not mint a claim of its own)", claims)
		}
		if claims[0].ExecutorID != "exec-fence-executor-live" || claims[0].BootEpoch != 4 {
			t.Errorf("claim identity = (%q, %d), want (exec-fence-executor-live, 4) — the refused write must not land",
				claims[0].ExecutorID, claims[0].BootEpoch)
		}
	})

	t.Run("SetPhase", func(t *testing.T) {
		fx := newFenceFixture(t, h, "exec-fence-phase")
		if _, err := fx.store.SetClaimPhaseSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "cloning"); err != nil {
			t.Fatalf("phase while claimed: %v", err)
		}
		reap(t, h.AdminDB, fx.orgID, fx.conversationID)

		if _, err := fx.store.SetClaimPhaseSystem(ctx, fx.orgID, fx.conversationID, fx.claimID, "agent_starting"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("phase after reap = %v, want ErrClaimReleased", err)
		}
		if claims := fx.seed.ClaimRows(t, fx.conversationID); claims[0].Phase != "cloning" {
			t.Errorf("phase = %q, want cloning (the refused write must not land)", claims[0].Phase)
		}
	})

	t.Run("LiveClaimOnAnotherConversationIsRefused", func(t *testing.T) {
		// Liveness is not ownership. An engagement legitimately driving one
		// conversation must not be able to write to a different one just
		// because its own claim is unreleased — which is what a fence that
		// only checked released_at would allow the moment a caller
		// mis-threaded its (conversation, claim) pair.
		fx := newFenceFixture(t, h, "exec-fence-crossconv")
		other := seedSecondConversation(t, fx)

		_, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: other, Role: "assistant", Content: "wrong conversation",
		})
		if !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("insert onto another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, other, fx.claimID, "completed", 0, 0, 0, "", "finish", "", ""); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("complete on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.MarkFailedIfActiveForClaimSystem(ctx, fx.orgID, other, fx.claimID, string(domain.ConversationFailureCrash)); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("mark-failed on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.ParkOpenForClaimSystem(ctx, fx.orgID, other, fx.claimID, db.ParkStopped("user_cancelled", "Run cancelled by user")); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("park-cancelled on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.SetClaimPhaseSystem(ctx, fx.orgID, other, fx.claimID, "cloning"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("phase on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, other, fx.claimID, "sess-wrong"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("session on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, other, fx.claimID, "/tmp/triagefactory-runs/wrong"); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("worktree path on another conversation = %v, want ErrClaimReleased", err)
		}
		if _, err := fx.store.SetExecutorForClaimSystem(ctx, fx.orgID, other, fx.claimID, "exec-wrong", 3); !errors.Is(err, db.ErrClaimReleased) {
			t.Fatalf("executor stamp on another conversation = %v, want ErrClaimReleased", err)
		}

		got, err := fx.store.Get(ctx, fx.orgID, other)
		if err != nil || got == nil {
			t.Fatalf("Get other: err=%v got=%v", err, got)
		}
		if got.Status != "running" {
			t.Errorf("other conversation status = %q, want running (untouched)", got.Status)
		}
		if got.SessionID != "" || got.WorktreePath != "" {
			t.Errorf("other conversation coordinates = (%q, %q), want both empty (untouched)", got.SessionID, got.WorktreePath)
		}
		msgs, err := fx.store.Messages(ctx, fx.orgID, other)
		if err != nil {
			t.Fatalf("Messages other: %v", err)
		}
		if len(msgs) != 0 {
			t.Fatalf("other conversation transcript = %+v, want nothing written by a claim that does not own it", msgs)
		}
		// The engagement's own conversation is unaffected — this refuses
		// misdirected writes, not the caller.
		if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.conversationID, Role: "assistant", Content: "own conversation",
		}); err != nil {
			t.Fatalf("insert onto its own conversation: %v", err)
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
				ConversationID: fx.conversationID, Role: "assistant", Content: name,
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
	`, fx.orgID, fx.conversationID); err != nil {
		t.Fatalf("reaper release: %v", err)
	}

	// The zombie's write, racing it.
	done := make(chan error, 1)
	go func() {
		_, werr := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, fx.claimID, &domain.Message{
			ConversationID: fx.conversationID, Role: "assistant", Content: "zombie",
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

	msgs, err := fx.store.Messages(ctx, fx.orgID, fx.conversationID)
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

	reap(t, h.AdminDB, fx.orgID, fx.conversationID)
	// The requeued conversation is claimed again, by whoever picks it up.
	if _, err := fx.store.SetExecutorSystem(ctx, fx.orgID, fx.conversationID, "exec-successor", 7); err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	claims := fx.seed.ClaimRows(t, fx.conversationID)
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want the reaped engagement plus its successor", claims)
	}
	successorClaim := claims[1].ID
	if successorClaim == zombieClaim {
		t.Fatal("successor reused the reaped claim id")
	}

	if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, successorClaim, &domain.Message{
		ConversationID: fx.conversationID, Role: "assistant", Content: "successor",
	}); err != nil {
		t.Fatalf("successor insert: %v", err)
	}
	if _, err := fx.store.InsertMessageForClaimSystem(ctx, fx.orgID, zombieClaim, &domain.Message{
		ConversationID: fx.conversationID, Role: "assistant", Content: "zombie",
	}); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie insert = %v, want ErrClaimReleased", err)
	}
	// The zombie's terminal is refused too, so the successor keeps driving a
	// conversation that is still `running` rather than one buried under a
	// stale completion.
	if _, err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "failed", 0, 0, 0, "", "", "", string(domain.ConversationFailureCrash)); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie complete = %v, want ErrClaimReleased", err)
	}

	got, err := fx.store.Get(ctx, fx.orgID, fx.conversationID)
	if err != nil || got == nil {
		t.Fatalf("Get: err=%v got=%v", err, got)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running (the successor is still driving)", got.Status)
	}
	if got.ExecutorID != "exec-successor" {
		t.Errorf("executor = %q, want exec-successor", got.ExecutorID)
	}
	msgs, err := fx.store.Messages(ctx, fx.orgID, fx.conversationID)
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
	if _, err := fx.store.CompleteForClaimSystem(ctx, fx.orgID, fx.conversationID, successorClaim, "completed", 0.25, 900, 1, "done", "finish", "", ""); err != nil {
		t.Fatalf("successor complete: %v", err)
	}
	if got, _ := fx.store.Get(ctx, fx.orgID, fx.conversationID); got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

// TestClaimFence_ZombieCannotCorruptTheSuccessorsResumeCoordinate is the
// failure this ticket's fence exists to prevent, end to end.
//
// The zombie is not misbehaving: its subprocess came up late and reported the
// session it genuinely started, its clone genuinely landed at that path, and
// its process genuinely is running on that executor. Every one of those facts
// is true about the ZOMBIE and false about the conversation, which by then
// belongs to somebody else. The damage is silent — nothing reads these columns
// back until the next wake, which then resumes into a dead session, in a
// directory on a host that isn't running the work, under an executor the
// reaper has already buried.
func TestClaimFence_ZombieCannotCorruptTheSuccessorsResumeCoordinate(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()
	fx := newFenceFixture(t, h, "exec-zombie-coords")
	zombieClaim := fx.claimID

	// The zombie's own engagement, recorded while it legitimately owned the row.
	if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "s1"); err != nil {
		t.Fatalf("zombie session while claimed: %v", err)
	}
	if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "/tmp/triagefactory-runs/host-a"); err != nil {
		t.Fatalf("zombie worktree path while claimed: %v", err)
	}

	reap(t, h.AdminDB, fx.orgID, fx.conversationID)
	if _, err := fx.store.SetExecutorSystem(ctx, fx.orgID, fx.conversationID, "exec-successor-coords", 7); err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	claims := fx.seed.ClaimRows(t, fx.conversationID)
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want the reaped engagement plus its successor", claims)
	}
	successorClaim := claims[1].ID

	// The successor rehydrates onto its own host and starts its own session.
	if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, fx.conversationID, successorClaim, "s2"); err != nil {
		t.Fatalf("successor session: %v", err)
	}
	if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, fx.conversationID, successorClaim, "/tmp/triagefactory-runs/host-b"); err != nil {
		t.Fatalf("successor worktree path: %v", err)
	}

	// The zombie's late init, clone stamp, and go-live stamp all arrive now.
	if _, err := fx.store.SetSessionForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "s1"); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie session after handover = %v, want ErrClaimReleased", err)
	}
	if _, err := fx.store.SetWorktreePathForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "/tmp/triagefactory-runs/host-a"); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie worktree path after handover = %v, want ErrClaimReleased", err)
	}
	if _, err := fx.store.SetExecutorForClaimSystem(ctx, fx.orgID, fx.conversationID, zombieClaim, "exec-zombie-coords", 1); !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("zombie executor stamp after handover = %v, want ErrClaimReleased", err)
	}

	got, err := fx.store.Get(ctx, fx.orgID, fx.conversationID)
	if err != nil || got == nil {
		t.Fatalf("Get: err=%v got=%v", err, got)
	}
	if got.SessionID != "s2" {
		t.Errorf("sdk_session_id = %q, want s2 — a wake here resumes the successor's session, not the zombie's", got.SessionID)
	}
	if got.WorktreePath != "/tmp/triagefactory-runs/host-b" {
		t.Errorf("worktree_path = %q, want the successor's host-b tree", got.WorktreePath)
	}
	if got.ExecutorID != "exec-successor-coords" {
		t.Errorf("executor = %q, want exec-successor-coords", got.ExecutorID)
	}

	// And the coordinate the successor left behind is what a resume actually
	// reads: the un-terminal flip keeps it, so the next claim wakes on s2.
	if _, err := fx.store.ParkOpenForClaimSystem(ctx, fx.orgID, fx.conversationID, successorClaim, db.ParkIdle()); err != nil {
		t.Fatalf("successor park: %v", err)
	}
	if ok, err := fx.store.MarkQueuedForResume(ctx, fx.orgID, fx.conversationID); err != nil || !ok {
		t.Fatalf("requeue for resume: ok=%v err=%v", ok, err)
	}
	if got, _ = fx.store.Get(ctx, fx.orgID, fx.conversationID); got.SessionID != "s2" {
		t.Errorf("sdk_session_id at resume = %q, want s2", got.SessionID)
	}
}
