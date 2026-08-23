package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestFollowUp_EmptyUserID guards the identity gate: every write a follow-up
// makes routes under the sending user's synthetic claims, so a message with no
// actor is refused before any of them.
func TestFollowUp_EmptyUserID(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-anon", "sess", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-anon'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-anon", "", "msg"); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
	if st := storedStatus(t, database, "r-anon"); st != "open" {
		t.Errorf("stored status = %q, want open — a refused follow-up writes nothing", st)
	}
}

// TestFollowUp_SDKOnlyValidationGuards walks the three fields an SDK resume
// needs on the row before a wake means anything: it re-invokes a Claude session
// BY ID, in the tree it ran in, on the model it started with. Each case blanks
// one of them and expects a refusal naming it.
//
// The native subtest is the other half of the same statement — these are the
// one place in the ladder where the runtime changes the answer, so the same row
// that refuses under `sdk` must be accepted under `native`, whose undelivered
// message rows ARE its resume state.
func TestFollowUp_SDKOnlyValidationGuards(t *testing.T) {
	cases := []struct {
		name    string
		mutate  string // SQL to blank the field under test
		wantSub string
	}{
		{"no session", `UPDATE conversations SET sdk_session_id = NULL WHERE id = ?`, "session id"},
		{"no worktree", `UPDATE conversations SET worktree_path = NULL WHERE id = ?`, "worktree path"},
		{"no model", `UPDATE conversations SET model = NULL WHERE id = ?`, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedConversation(t, database, "r-guard", "sess", "/tmp/wt")
			if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-guard'`); err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := database.Exec(tc.mutate, "r-guard"); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

			err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-guard", runmode.LocalDefaultUserID, "msg")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.wantSub)
			}
		})
	}

	t.Run("native needs none of them", func(t *testing.T) {
		database := newDelegateTestDB(t)
		// No session id, and the worktree it does carry is gone: a native
		// conversation resumes off its transcript, not off either of those.
		seedConversation(t, database, "r-native-guard", "", "")
		if _, err := database.Exec(`UPDATE conversations SET status='open', model=NULL WHERE id='r-native-guard'`); err != nil {
			t.Fatalf("open: %v", err)
		}
		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
		markNative(t, database, "r-native-guard")

		if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-native-guard", runmode.LocalDefaultUserID, "carry on"); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if got := pendingRows(t, s, "r-native-guard"); len(got) != 1 {
			t.Errorf("queued rows = %d, want 1 — the SDK's session guards must not reach a native conversation", len(got))
		}
	})
}

// TestFollowUp_TerminalRefused pins the ladder's floor, for both runtimes: a
// failed conversation's workspace did not survive, so nothing will ever read a
// row queued against it and SendMessage refuses before writing one.
func TestFollowUp_TerminalRefused(t *testing.T) {
	for _, runtime := range []string{"sdk", "native"} {
		t.Run(runtime, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedConversation(t, database, "r-term", "sess", "/tmp/wt")
			if _, err := database.Exec(`UPDATE conversations SET status='failed' WHERE id='r-term'`); err != nil {
				t.Fatalf("fail: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
			if runtime == "native" {
				markNative(t, database, "r-term")
			}

			err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-term", runmode.LocalDefaultUserID, "msg")
			if !errors.Is(err, ErrConversationNotSteerable) {
				t.Errorf("err = %v, want ErrConversationNotSteerable", err)
			}
			if got := pendingRows(t, s, "r-term"); len(got) != 0 {
				t.Errorf("a refused message must not leave a row behind: %+v", got)
			}
		})
	}
}

// TestFollowUp_RefusalTaxonomy pins which of three answers a parked run
// gets, and the ORDER the two structural ones are asked in. All three rows
// look identically resumable from the conversation alone, so the whole
// distinction lives in what is true around them:
//
//   - workspace reaped by the retention TTL → 410 Gone for an SDK
//     conversation. A native one is NOT refused: its continuity is the
//     transcript in `messages`, so the claim builds it a fresh workspace.
//   - workspace intact, blueprint cancelled → 409 concluded. A blueprint that
//     merely FINISHED is not in this set — that is the follow-up case and it
//     succeeds; called off is the one that still refuses.
//   - both → whichever refusal is real for that runtime. Under the SDK the
//     workspace answer wins, because telling someone their sequence was
//     cancelled implies something left to cancel back into and there isn't; a
//     native row has no workspace refusal, so the blueprint's answer stands.
//
// The third case is not hypothetical: it is every run stopped by an older
// build, whose cancel path discarded the snapshot AND cancelled the blueprint.
//
// Every case runs under BOTH runtimes. Most of the ladder is shared; the
// workspace rung is where the engines genuinely differ, and running both keeps
// that a stated contract rather than a surprise.
func TestFollowUp_RefusalTaxonomy(t *testing.T) {
	park := func(t *testing.T, database *sql.DB, id, runtime, blueprintStatus string, keepWorkspace bool) *Spawner {
		t.Helper()
		wt := "/tmp/does-not-exist-" + id
		if keepWorkspace {
			wt = t.TempDir() // a warm worktree on disk === recoverable
		}
		seedConversation(t, database, id, "sess-"+id, wt)
		if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id=?`, id); err != nil {
			t.Fatalf("park: %v", err)
		}
		if _, err := database.Exec(`UPDATE blueprint_runs SET status=? WHERE id=?`, blueprintStatus, "seedbpr-"+id); err != nil {
			t.Fatalf("set blueprint status: %v", err)
		}
		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
		if runtime == "native" {
			markNative(t, database, id)
		}
		// A blob store with nothing in it: no snapshot to rehydrate from, so a
		// run whose worktree is also gone is unrecoverable for real.
		blobs, err := storage.New()
		if err != nil {
			t.Fatalf("storage.New: %v", err)
		}
		s.SetStorage(blobs)
		return s
	}

	for _, runtime := range []string{"sdk", "native"} {
		t.Run(runtime, func(t *testing.T) {
			t.Run("workspace reaped, blueprint running", func(t *testing.T) {
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				s := park(t, database, "r-reaped", runtime, "running", false)
				err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-reaped", runmode.LocalDefaultUserID, "msg")
				if runtime == "native" {
					// The transcript is the conversation, so a lost workspace
					// costs uncommitted work rather than the whole run.
					if err != nil {
						t.Fatalf("err = %v, want the wake accepted — a native conversation resumes into a fresh workspace", err)
					}
					if got := pendingRows(t, s, "r-reaped"); len(got) != 1 {
						t.Errorf("queued rows = %d, want the follow-up waiting for the next claim", len(got))
					}
					return
				}
				if !errors.Is(err, ErrWorkspaceExpired) {
					t.Errorf("err = %v, want ErrWorkspaceExpired (410)", err)
				}
				if got := pendingRows(t, s, "r-reaped"); len(got) != 0 {
					t.Errorf("a refused wake must write nothing: %+v", got)
				}
			})

			t.Run("workspace intact, blueprint cancelled", func(t *testing.T) {
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				s := park(t, database, "r-concluded", runtime, "cancelled", true)
				err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-concluded", runmode.LocalDefaultUserID, "msg")
				if !errors.Is(err, ErrBlueprintCancelled) {
					t.Errorf("err = %v, want ErrBlueprintCancelled (409)", err)
				}
				if st := storedStatus(t, database, "r-concluded"); st != "open" {
					t.Errorf("stored status = %q, want open — a refused wake must leave the row parked, not queued", st)
				}
				if got := pendingRows(t, s, "r-concluded"); len(got) != 0 {
					t.Errorf("a refused wake must write nothing: %+v", got)
				}
			})

			t.Run("workspace intact, blueprint finished — the follow-up case", func(t *testing.T) {
				// The same shape as the refusal above, one word apart in the
				// blueprint's terminal, and the opposite answer. Finished work is
				// followed up on; called-off work is not.
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				s := park(t, database, "r-followup", runtime, "completed", true)
				if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-followup", runmode.LocalDefaultUserID, "msg"); err != nil {
					t.Fatalf("follow-up on a finished blueprint: %v", err)
				}
				if st := storedStatus(t, database, "r-followup"); st != "" {
					t.Errorf("stored status = %q, want none — the wake un-terminals the row", st)
				}
				if got := pendingRows(t, s, "r-followup"); len(got) != 1 {
					t.Errorf("queued rows = %d, want the follow-up waiting for the next claim", len(got))
				}
			})

			t.Run("both gone — the migrated legacy row", func(t *testing.T) {
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				s := park(t, database, "r-legacy", runtime, "cancelled", false)
				err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-legacy", runmode.LocalDefaultUserID, "msg")
				want := ErrWorkspaceExpired // the permanent answer wins over the temporary one
				if runtime == "native" {
					// Nothing about the workspace refuses a native row, so the
					// cancelled blueprint is the only refusal there is — and it
					// is the true one: nothing would ever drive this step again.
					want = ErrBlueprintCancelled
				}
				if !errors.Is(err, want) {
					t.Errorf("err = %v, want %v", err, want)
				}
			})
		})
	}
}

// TestFollowUp_EnqueuesRatherThanSpawning is resume-by-enqueue's core
// contract (TFAC-585, decision log #7): the follow-up records the message
// durably and flips the run's SAME row back to `queued` — no in-process
// resume goroutine, no s.cancels registration, at any point during the
// call. The message is only consumed later, by whichever executor claims
// the row.
func TestFollowUp_EnqueuesRatherThanSpawning(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-wake", "sess-wake", "/tmp/does-not-exist-wake")
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-wake'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-wake", runmode.LocalDefaultUserID, "the answer"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if st := storedStatus(t, database, "r-wake"); st != "" {
		t.Errorf("stored status = %q, want none — resume-by-enqueue clears the park rather than writing a queue status", st)
	}

	msg, userID, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-wake")
	if err != nil {
		t.Fatalf("consume pending input: %v", err)
	}
	if !ok || msg != "the answer" || userID != runmode.LocalDefaultUserID {
		t.Errorf("pending input = (ok=%v msg=%q user=%q), want (true, %q, %q)", ok, msg, userID, "the answer", runmode.LocalDefaultUserID)
	}

	// No in-process resume goroutine — decision log #7's "no in-process
	// resume variant survives, in any mode".
	s.mu.Lock()
	_, active := s.cancels["r-wake"]
	s.mu.Unlock()
	if active {
		t.Error("the follow-up registered a cancel handle — an in-process resume goroutine must not spawn")
	}
}

// staleOpenConversations reports the FIRST conversation read as still parked
// `open`, whatever the row has since become, and every read after it honestly.
// That is exactly the losing half of a wake race, and the one thing a
// sequential caller cannot have: a validation read taken before the winner's
// flip, followed by a re-read that sees the truth. Everything in between —
// including the MarkQueuedForResume CAS that then genuinely fails — runs
// against the real row.
type staleOpenConversations struct {
	db.ConversationStore
	reads *int
}

func staleOpenOnce(inner db.ConversationStore) staleOpenConversations {
	return staleOpenConversations{ConversationStore: inner, reads: new(int)}
}

func (c staleOpenConversations) GetSystem(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	conv, err := c.ConversationStore.GetSystem(ctx, orgID, conversationID)
	*c.reads++
	if conv != nil && *c.reads == 1 {
		conv.Status, conv.Outcome = "open", ""
	}
	return conv, err
}

// TestFollowUp_LostWakeRaceStillDelivers pins what append buys: a wake
// that loses the MarkQueuedForResume CAS is a SUCCESS, not a 409, because its
// message is already queued and the winner's claim drains whatever is queued.
//
// Under the replace contract this could not be true — the loser's write would
// have clobbered the winner's — which is exactly why the input write used to be
// bound into the CAS's transaction. It no longer is, so this walks the race to
// its end: the winner flips, the loser queues onto the row it read as still
// parked, and one claim carries both messages away.
func TestFollowUp_LostWakeRaceStillDelivers(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	// A real worktree, so the claim gets far enough to deliver: a missing one
	// is a runtime failure and hands the claim back with both messages still
	// queued, which is a different test.
	seedConversation(t, database, "r-race", "sess-race", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-race'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-race", runmode.LocalDefaultUserID, "ship it"); err != nil {
		t.Fatalf("winning wake: %v", err)
	}

	// The loser reached its flip a moment later, still holding the pre-flip
	// read. Its CAS finds the row already re-queued and fails for real.
	loserStores := testSpawnerStores(database)
	loserStores.Conversations = staleOpenOnce(loserStores.Conversations)
	loser := NewSpawner(database, loserStores, nil, nil, "claude-sonnet-4-6")
	if err := loser.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-race", runmode.LocalDefaultUserID, "and check the migration"); err != nil {
		t.Fatalf("losing wake returned %v, want nil — its message is queued, so there is nothing to report", err)
	}

	want := "ship it" + db.PendingInputSeparator + "and check the migration"
	msg, _, ok, err := s.pendingInput.Peek(context.Background(), runmode.LocalDefaultOrgID, "r-race")
	if err != nil || !ok {
		t.Fatalf("peek = (ok=%v, err=%v); want ok=true", ok, err)
	}
	if msg != want {
		t.Errorf("queued input = %q, want %q — the loser's message must survive alongside the winner's", msg, want)
	}

	// And the winner's claim carries both away: nothing is left queued for a
	// later claim to re-deliver.
	claimAndDispatch(t, s, database)
	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-race"); err != nil || ok {
		t.Errorf("input still queued after the winner's claim: ok=%v err=%v", ok, err)
	}
}

// TestFollowUp_LostToTerminalIsAConflict is the other half of the same
// refusal, and the reason a lost flip cannot simply report success: the CAS
// declines a conversation that went terminal exactly as it declines one
// another wake already re-queued, but here nothing will ever claim the row, so
// the queued message is never delivered. A 200 would tell the user their
// message landed when it did not.
func TestFollowUp_LostToTerminalIsAConflict(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-died", "sess-died", "/tmp/does-not-exist-died")
	// The conversation failed while this wake was mid-flight; the wake still
	// holds the read it validated against.
	if _, err := database.Exec(`UPDATE conversations SET status='failed' WHERE id='r-died'`); err != nil {
		t.Fatalf("fail: %v", err)
	}
	stores := testSpawnerStores(database)
	stores.Conversations = staleOpenOnce(stores.Conversations)
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-died", runmode.LocalDefaultUserID, "one more thing")
	if !errors.Is(err, ErrConversationNotResumable) {
		t.Fatalf("err = %v, want ErrConversationNotResumable — nothing claims a terminal conversation, so the message is not delivered", err)
	}
	if st := storedStatus(t, database, "r-died"); st != "failed" {
		t.Errorf("stored status = %q, want failed — a lost flip must not move the row", st)
	}
}

// TestFollowUp_CompletedAbortReopensBlueprintAtomically: a
// completed+abort run's blueprint (already terminal 'aborted') is reopened
// to 'running' in the SAME tx as the requeue flip — required for
// ClaimNextConversation to ever claim the row (it only claims under a 'running'
// blueprint_run).
func TestFollowUp_CompletedAbortReopensBlueprintAtomically(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-ab", "sess-ab", "/tmp/wt-ab")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort' WHERE id='r-ab'`); err != nil {
		t.Fatalf("completed+abort: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, "r-ab")
	if _, err := database.Exec(`UPDATE blueprint_runs SET status='aborted' WHERE id=?`, bpr); err != nil {
		t.Fatalf("abort blueprint_run: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-ab", runmode.LocalDefaultUserID, "pick it back up"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running (reopened so ClaimNextConversation can claim the resumed row)", bpStatus)
	}
}

// claimAndDispatch claims the globally-oldest queued run (mirroring the
// dispatcher's own ClaimNextConversation call) and drives it synchronously through
// dispatchClaimedConversation — the same entry a real drainConversationQueue goroutine would
// call, minus the goroutine wrapper, so resume-claim tests don't need to
// poll for completion.
func claimAndDispatch(t *testing.T, s *Spawner, database *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conv, err := s.conversationQueue.ClaimNextConversation(ctx, "test-executor", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("claim next run: %v", err)
	}
	if conv == nil {
		t.Fatal("claim next run: nothing claimable (is the row queued under a running blueprint_run?)")
	}
	conv.OrgID = runmode.LocalDefaultOrgID
	s.dispatchClaimedConversation(ctx, conv)
}

// TestDispatchResumeClaim_DeliversRecordedInput proves the delivery half of
// resume-by-enqueue end to end: after the follow-up enqueues, claiming and
// dispatching the row consumes the pending input exactly once (a second
// consume finds nothing) and drives the resume — failing fast here on the
// session transcript the warm worktree does not hold, landing the run
// terminal. What matters is that delivery was attempted off the ordinary
// claim path, not an in-process goroutine.
//
// The worktree is real: a missing one is a runtime that would not come up,
// which hands the claim straight back with the input untouched, and this test
// is about what happens once it does come up.
func TestDispatchResumeClaim_DeliversRecordedInput(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-deliver", "sess-deliver", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-deliver'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-deliver", runmode.LocalDefaultUserID, "the answer"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	claimAndDispatch(t, s, database)

	// The pending input was consumed exactly once.
	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-deliver"); err != nil || ok {
		t.Errorf("pending input still present after dispatch: ok=%v err=%v", ok, err)
	}

	// The run reached an outcome — delivery was attempted (and failed fast
	// here for lack of a warm worktree/snapshot, landing terminal).
	st := storedStatus(t, database, "r-deliver")
	if st == "" || st == "open" {
		t.Errorf("stored status = %q, want a terminal status (delivery was not attempted)", st)
	}

	// No cancel handle survives dispatch (deferred cleanup ran).
	s.mu.Lock()
	_, active := s.cancels["r-deliver"]
	s.mu.Unlock()
	if active {
		t.Error("cancel handle leaked past dispatchResumeClaim's return")
	}
}

// TestDispatchResumeClaim_WorkspaceFailureRetriesThenParks is the
// resume-by-enqueue counterpart of the native path's launch-failure contract:
// a rehydrate that will not complete is the resume's runtime failing to come
// up, so the claim goes back on the queue rather than ending the conversation.
// A snapshot is seeded (garbage content) so the enqueue-time recoverability
// pre-flight passes but the actual rehydrate fails at claim — the realistic
// enqueue-then-workspace-lost race, not an already-expired workspace (which
// the follow-up path refuses up front — see the sibling test).
//
// The exhausted budget then parks `open`, which is also the answer to the
// worry this test was originally written for: `open` is a state the retention
// sweep selects, so the snapshot the retries preserved is collected on its own
// TTL rather than orphaned behind a blueprint stranded 'running' forever.
func TestDispatchResumeClaim_WorkspaceFailureRetriesThenParks(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, _ := setupAdvanceFixture(t, "open-strand")
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	wireBlobStore(t, s)
	putTestSnapshot(t, s, bpr) // garbage blob: passes Exists, fails rehydrate
	if _, err := database.Exec(`UPDATE conversations SET status='open', worktree_path='/tmp/does-not-exist-open-strand' WHERE id=?`, conversationID); err != nil {
		t.Fatalf("park open: %v", err)
	}

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "carry on"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// The dispatcher's whole budget, one claim at a time.
	for i := 0; i < maxClaimAttempts; i++ {
		claimAndDispatch(t, s, database)
		var bpStatus string
		if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
			t.Fatalf("read blueprint_run status: %v", err)
		}
		if bpStatus != "running" {
			t.Fatalf("blueprint_run moved to %q on attempt %d; a runtime that never started moves no blueprint", bpStatus, i+1)
		}
		assertSnapshotPresent(t, s, bpr, true)
	}

	if st := storedStatus(t, database, conversationID); st != domain.StatusOpen {
		t.Errorf("stored status = %q after the budget ran out, want open", st)
	}
	// Nothing is claimable any more: the queued follow-up was settled on the
	// way down, so the retries stop rather than spinning forever.
	claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "test-executor", 1, db.ClaimPlacement{})
	if err != nil || claimed != nil {
		t.Fatalf("ClaimNextConversation after the park = (%v, %v), want nothing claimable", claimed, err)
	}
}

// TestFollowUp_ExpiredWorkspaceRefusedAtEnqueue pins the workspace-expiry
// contract: a wake whose workspace is gone for good (no warm worktree, no
// durable snapshot) is refused with ErrWorkspaceExpired at enqueue, with NO
// side effect — the run stays resumable, no pending-input row is recorded, and
// the blueprint is left running. Without the pre-flight the flip succeeds and
// the run is destroyed at claim time (a generic failConversation) instead of the caller
// seeing a clean 410.
func TestFollowUp_ExpiredWorkspaceRefusedAtEnqueue(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, _ := setupAdvanceFixture(t, "expired")
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	wireBlobStore(t, s) // storage present but empty: no snapshot to recover from
	if _, err := database.Exec(`UPDATE conversations SET status='open', worktree_path='/tmp/does-not-exist-expired' WHERE id=?`, conversationID); err != nil {
		t.Fatalf("park open: %v", err)
	}

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "carry on")
	if !errors.Is(err, ErrWorkspaceExpired) {
		t.Fatalf("SendMessage err = %v, want ErrWorkspaceExpired", err)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id=?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "open" {
		t.Errorf("run status = %q after refused wake; want unchanged 'open'", status)
	}
	var pending int
	if err := database.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND delivered=0`, conversationID).Scan(&pending); err != nil {
		t.Fatalf("count pending input: %v", err)
	}
	if pending != 0 {
		t.Errorf("undelivered message rows = %d after refused wake; want 0 (no side effect)", pending)
	}
	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id=?`, bpr).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q after refused wake; want untouched 'running'", bpStatus)
	}
}
