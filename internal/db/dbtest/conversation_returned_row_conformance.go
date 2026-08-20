package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationReturnedRowFactory is what a per-backend test file hands to
// RunConversationReturnedRowConformance: the ConversationStore under test,
// its ConversationQueueStore sibling (ClaimByIDSystem is the claims-row
// point read this suite asserts against — see agent.go's interface doc for
// why a claims-row write shares that projection rather than one of its
// own), the org/user to pass, and the same seeder every other
// ConversationStore conformance subtest uses.
type ConversationReturnedRowFactory func(t *testing.T) (
	store db.ConversationStore,
	queue db.ConversationQueueStore,
	orgID, userID string,
	seed ConversationSeeder,
)

// RunConversationReturnedRowConformance covers the returned-row standard
// for every ConversationStore write that isn't exempt:
//
//   - conversations-row writes (Complete, SetSession, SetWorktreePath and
//     their System/ForClaimSystem twins) return the row Get would read.
//   - claims-row writes (SetExecutorSystem, SetExecutorForClaimSystem,
//     SetClaimPhaseSystem, SetActiveClaimPhaseSystem,
//     RecordClaimSandboxStatsSystem) return the row
//     ConversationQueueStore.ClaimByIDSystem would read.
//   - the one messages-row write (SettleCompactionRequestForClaimSystem)
//     returns the row a point read of that message id would find.
//
// CompactForClaimSystem is not here — it is the standard's stated bulk
// exemption (multiple rows, no single row a return value could name; see
// its doc on the interface).
//
// One seeded conversation carries a single live engagement through the
// claim-scoped writes in a plausible order (mint, session, worktree, phase,
// sandbox stats, executor re-stamp, compaction settle) so every
// ForClaimSystem call has the live claim it fences on; three more seeded
// conversations each carry Complete/CompleteSystem/CompleteForClaimSystem to
// its own terminal, since completing releases the claim and only one
// terminal write is meaningful per engagement. A fifth exercises
// SetExecutorSystem's release arm (executorID == "") directly.
func RunConversationReturnedRowConformance(t *testing.T, mk ConversationReturnedRowFactory) {
	t.Helper()

	t.Run("claim_scoped_writes_return_the_stored_row", func(t *testing.T) {
		store, queue, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")
		readConv := func() (*domain.Conversation, error) { return store.Get(ctx, orgID, conversationID) }

		claim, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-rr", 1)
		if err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if claim == nil {
			t.Fatal("SetExecutorSystem returned a nil claim on a successful mint")
		}
		readClaim := func() (*domain.ExecutorClaim, error) { return queue.ClaimByIDSystem(ctx, claim.ID) }
		AssertWriteReturnedStoredRow(t, "SetExecutorSystem (mint)", *claim, readClaim)
		claimID := claim.ID

		conv, err := store.SetSession(ctx, orgID, conversationID, "sess-rr-1")
		if err != nil {
			t.Fatalf("SetSession: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSession", *conv, readConv)

		conv, err = store.SetSessionSystem(ctx, orgID, conversationID, "sess-rr-2")
		if err != nil {
			t.Fatalf("SetSessionSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSessionSystem", *conv, readConv)

		conv, err = store.SetSessionForClaimSystem(ctx, orgID, conversationID, claimID, "sess-rr-3")
		if err != nil {
			t.Fatalf("SetSessionForClaimSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetSessionForClaimSystem", *conv, readConv)

		conv, err = store.SetWorktreePath(ctx, orgID, conversationID, "/tmp/rr-1")
		if err != nil {
			t.Fatalf("SetWorktreePath: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetWorktreePath", *conv, readConv)

		conv, err = store.SetWorktreePathSystem(ctx, orgID, conversationID, "/tmp/rr-2")
		if err != nil {
			t.Fatalf("SetWorktreePathSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetWorktreePathSystem", *conv, readConv)

		conv, err = store.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, "/tmp/rr-3")
		if err != nil {
			t.Fatalf("SetWorktreePathForClaimSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetWorktreePathForClaimSystem", *conv, readConv)

		claim, err = store.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, "cloning")
		if err != nil {
			t.Fatalf("SetActiveClaimPhaseSystem: %v", err)
		}
		if claim == nil {
			t.Fatal("SetActiveClaimPhaseSystem returned nil on a conversation with a live claim")
		}
		AssertWriteReturnedStoredRow(t, "SetActiveClaimPhaseSystem", *claim, readClaim)

		claim, err = store.SetClaimPhaseSystem(ctx, orgID, conversationID, claimID, "agent_starting")
		if err != nil {
			t.Fatalf("SetClaimPhaseSystem: %v", err)
		}
		if claim == nil {
			t.Fatal("SetClaimPhaseSystem returned nil for a live, owned claim")
		}
		AssertWriteReturnedStoredRow(t, "SetClaimPhaseSystem", *claim, readClaim)

		peak, cpu := 128, int64(500000)
		claim, err = store.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, &peak, &cpu)
		if err != nil {
			t.Fatalf("RecordClaimSandboxStatsSystem: %v", err)
		}
		if claim == nil {
			t.Fatal("RecordClaimSandboxStatsSystem returned nil for a known claim id")
		}
		AssertWriteReturnedStoredRow(t, "RecordClaimSandboxStatsSystem", *claim, readClaim)

		claim, err = store.SetExecutorForClaimSystem(ctx, orgID, conversationID, claimID, "exec-rr-relive", 2)
		if err != nil {
			t.Fatalf("SetExecutorForClaimSystem: %v", err)
		}
		if claim == nil {
			t.Fatal("SetExecutorForClaimSystem returned nil for a live, owned claim")
		}
		AssertWriteReturnedStoredRow(t, "SetExecutorForClaimSystem", *claim, readClaim)

		reqID, err := store.InsertMessageForClaimSystem(ctx, orgID, claimID, &domain.Message{
			ConversationID: conversationID, Role: "user",
			Subtype: domain.MessageSubtypeInjectionCompactionRequest, Content: "please compact",
		})
		if err != nil {
			t.Fatalf("InsertMessageForClaimSystem (compaction request): %v", err)
		}
		readMsg := func() (*domain.Message, error) { return findMessageByID(ctx, store, orgID, conversationID, int(reqID)) }
		cost := 0.05
		msg, err := store.SettleCompactionRequestForClaimSystem(ctx, orgID, conversationID, claimID,
			int(reqID), 100, 20, 0, 0, &cost, "unparseable summary")
		if err != nil {
			t.Fatalf("SettleCompactionRequestForClaimSystem: %v", err)
		}
		if msg == nil {
			t.Fatal("SettleCompactionRequestForClaimSystem returned nil for its own request row")
		}
		AssertWriteReturnedStoredRow(t, "SettleCompactionRequestForClaimSystem", *msg, readMsg)
	})

	t.Run("SetExecutorSystem_release_arm_returns_the_released_claim", func(t *testing.T) {
		store, queue, orgID, _, seed := mk(t)
		ctx := context.Background()
		conversationID := seedConversationForTest(t, orgID, seed, "running")

		claim, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-rr-release", 1)
		if err != nil {
			t.Fatalf("SetExecutorSystem (mint): %v", err)
		}
		released, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0)
		if err != nil {
			t.Fatalf("SetExecutorSystem (release): %v", err)
		}
		if released == nil {
			t.Fatal("SetExecutorSystem (release) returned nil for a conversation with an active claim to release")
		}
		AssertWriteReturnedStoredRow(t, "SetExecutorSystem (release)", *released,
			func() (*domain.ExecutorClaim, error) { return queue.ClaimByIDSystem(ctx, claim.ID) })

		// The guard-declined shape: nothing active left to release.
		declined, err := store.SetExecutorSystem(ctx, orgID, conversationID, "", 0)
		if err != nil {
			t.Fatalf("SetExecutorSystem (release, nothing active): %v", err)
		}
		if declined != nil {
			t.Errorf("SetExecutorSystem (release) with no active claim = %+v, want nil (the guard declining)", declined)
		}
	})

	for _, tc := range []struct {
		name string
		call func(store db.ConversationStore, ctx context.Context, orgID, conversationID, claimID string) (*domain.Conversation, error)
	}{
		{"Complete", func(store db.ConversationStore, ctx context.Context, orgID, conversationID, _ string) (*domain.Conversation, error) {
			return store.Complete(ctx, orgID, conversationID, "completed", 0.5, 1000, 2, "done", "finish", "", "")
		}},
		{"CompleteSystem", func(store db.ConversationStore, ctx context.Context, orgID, conversationID, _ string) (*domain.Conversation, error) {
			return store.CompleteSystem(ctx, orgID, conversationID, "completed", 0.5, 1000, 2, "done", "finish", "", "")
		}},
		{"CompleteForClaimSystem", func(store db.ConversationStore, ctx context.Context, orgID, conversationID, claimID string) (*domain.Conversation, error) {
			return store.CompleteForClaimSystem(ctx, orgID, conversationID, claimID, "completed", 0.5, 1000, 2, "done", "finish", "", "")
		}},
	} {
		t.Run(tc.name+"_returns_the_stored_row", func(t *testing.T) {
			store, _, orgID, _, seed := mk(t)
			ctx := context.Background()
			conversationID := seedConversationForTest(t, orgID, seed, "running")
			claim, err := store.SetExecutorSystem(ctx, orgID, conversationID, "exec-rr-"+tc.name, 1)
			if err != nil {
				t.Fatalf("SetExecutorSystem (mint): %v", err)
			}
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: conversationID, Role: "assistant", Content: "work", ClaimID: claim.ID,
			}); err != nil {
				t.Fatalf("InsertMessage: %v", err)
			}

			conv, err := tc.call(store, ctx, orgID, conversationID, claim.ID)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			AssertWriteReturnedStoredRow(t, tc.name, *conv,
				func() (*domain.Conversation, error) { return store.Get(ctx, orgID, conversationID) })
		})
	}

	t.Run("miss_semantics", func(t *testing.T) {
		store, _, orgID, _, _ := mk(t)
		ctx := context.Background()
		missingID := "00000000-0000-4000-8000-000000000000"

		if _, err := store.Complete(ctx, orgID, missingID, "completed", 0, 0, 0, "", "", "", ""); !errors.Is(err, db.ErrNoSuchConversation) {
			t.Errorf("Complete on a missing conversation id = %v, want db.ErrNoSuchConversation", err)
		}
		if _, err := store.SetSession(ctx, orgID, missingID, "sess"); !errors.Is(err, db.ErrNoSuchConversation) {
			t.Errorf("SetSession on a missing conversation id = %v, want db.ErrNoSuchConversation", err)
		}
		if _, err := store.SetWorktreePath(ctx, orgID, missingID, "/tmp/x"); !errors.Is(err, db.ErrNoSuchConversation) {
			t.Errorf("SetWorktreePath on a missing conversation id = %v, want db.ErrNoSuchConversation", err)
		}
	})
}

// ConversationAppPoolFactory is what the Postgres app-pool test file hands to
// RunConversationAppPoolReturnedRowConformance: the ConversationStore wired
// under a claims-carrying transaction, and the seeder.
//
// Claims-row writes (SetExecutorSystem and every other claims-scoped method)
// have no plain door — the interface routes them through the admin pool
// always, because claims is a system-written table tf_app holds no UPDATE
// grant on (see agent.go's "Claim-fenced engagement writes" doc). RLS has
// nothing to narrow on a connection that never runs those statements, so this
// suite doesn't cover them; RunConversationReturnedRowConformance already
// does, on whichever pool its caller wires.
//
// Complete is excluded here too, despite having a plain door: its cost-lump
// settlement and claim release (settleCompletionCostAndClaim) touch the same
// admin-only claims table BEFORE the conversations flip this suite cares
// about, so calling it through a single tf_app-bound transaction (the shape
// NewForTx gives a test — see its doc) trips the same missing UPDATE grant,
// for a reason that has nothing to do with the conversations-row RETURNING
// this suite exists to check. Production Complete never hits that: its
// settlement genuinely runs on the real admin pool. RunConversationReturnedRowConformance
// covers Complete/CompleteSystem/CompleteForClaimSystem on the admin pool,
// and its conversations flip shares writeConversationReturning byte-for-byte
// with SetSession/SetWorktreePath below — proving RLS-visibility for those
// two is evidence the shared helper works under RLS, which is what this
// suite is actually checking.
//
// What's left — SetSession, SetWorktreePath — are the two conversations-row
// methods with a plain, app-pool-doored form that touch nothing else: their
// RETURNING has to satisfy the SELECT policy for the row it hands back, and a
// policy that admits the write but not the read-back yields zero rows from a
// statement that updated one. See AssertWriteReturnedStoredRow's doc for the
// failure this wiring exists to catch.
type ConversationAppPoolFactory func(t *testing.T) (store db.ConversationStore, orgID, userID string, seed ConversationSeeder)

// RunConversationAppPoolReturnedRowConformance is
// RunConversationReturnedRowConformance's RLS-focused sibling — see
// ConversationAppPoolFactory for why it covers only these two methods.
func RunConversationAppPoolReturnedRowConformance(t *testing.T, mk ConversationAppPoolFactory) {
	t.Helper()
	store, orgID, _, seed := mk(t)
	ctx := context.Background()
	conversationID := seedConversationForTest(t, orgID, seed, "running")
	readConv := func() (*domain.Conversation, error) { return store.Get(ctx, orgID, conversationID) }

	conv, err := store.SetSession(ctx, orgID, conversationID, "sess-app-pool")
	if err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	AssertWriteReturnedStoredRow(t, "SetSession", *conv, readConv)

	conv, err = store.SetWorktreePath(ctx, orgID, conversationID, "/tmp/app-pool")
	if err != nil {
		t.Fatalf("SetWorktreePath: %v", err)
	}
	AssertWriteReturnedStoredRow(t, "SetWorktreePath", *conv, readConv)
}

// findMessageByID is the point read SettleCompactionRequestForClaimSystem's
// returned row is asserted against: ConversationStore projects messages as a
// conversation-scoped list, so this drains that list and picks the one row —
// the same shape a real caller would need if it wanted to name one message,
// and unrelated to the write under test (Messages is unconverted).
func findMessageByID(ctx context.Context, store db.ConversationStore, orgID, conversationID string, id int) (*domain.Message, error) {
	msgs, err := store.Messages(ctx, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		if msgs[i].ID == id {
			return &msgs[i], nil
		}
	}
	return nil, nil
}
