package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ClaimPredicateHarness is what a per-backend test file hands to
// RunClaimPredicateConformance. Stores is the full bundle (the suite drives
// the claim, the message ledger, and the display read together); the seeder
// stages the shapes the store's own guarded writes cannot reach.
type ClaimPredicateHarness struct {
	Stores db.Stores
	OrgID  string
	UserID string

	// EnqueueDelegation mints one delegation conversation under a running
	// blueprint_run and returns its id. Successive calls are successive STEPS
	// of that one blueprint, and each advances the blueprint's
	// current_step_index to the step it is about to mint — the order the
	// reactor writes in, and the thing the claim gate's equality reads.
	// runtime is 'sdk' or 'native': the dialect stamps its own at mint
	// (Postgres native, SQLite sdk), so the seeder rewrites the column to
	// cover both engines against one backend.
	EnqueueDelegation func(t *testing.T, runtime string) (convID string)

	// EnqueueUnindexed attempts the same mint with NO blueprint step index
	// and hands back the store's error. It is the one shape the claim gate
	// could never admit — a conversation that names no position in its
	// sequence — so the mint has to refuse it rather than leave a row in the
	// work list that nothing will ever pick up.
	EnqueueUnindexed func(t *testing.T) error

	// SetStoredStatus writes conversations.status directly — the fixture
	// door for the parks and terminals the suite needs on demand. An empty
	// status writes SQL NULL (the mid-flight state).
	SetStoredStatus func(t *testing.T, convID, status string)

	// StoredStatus reads conversations.status back, SQL NULL as "".
	StoredStatus func(t *testing.T, convID string) string

	// SetBlueprintState writes the shared blueprint_run's status and
	// current_step_index directly. The store's own writes cannot reach a
	// terminal blueprint whose sequencing column still points at a step (the
	// guarded MarkRunStatus refuses a non-running row), and that combination
	// is exactly what the finished-blueprint arm of the claim gate reads.
	SetBlueprintState func(t *testing.T, status string, currentStepIndex int)

	// InsertRow inserts one messages row verbatim — the suite needs
	// undelivered/delivered, subtyped, window-stated and fractionally
	// sequenced rows that no production writer produces in combination.
	InsertRow func(t *testing.T, convID string, msg domain.Message) int64

	// SetSeq rewrites a message row's seq to a fractional value, the way a
	// compaction commit re-seqs queued input.
	SetSeq func(t *testing.T, msgID int64, seq float64)

	// DisplayStatus reads the conversation back through the display
	// projection — what the wire actually carries.
	DisplayStatus func(t *testing.T, convID string) string

	// CollapseClaimTimestamps rewrites every one of a conversation's claim
	// timestamps to a single identical value — the worst case a coarse clock
	// could produce, which no production writer reaches (they all bind a
	// high-resolution instant) and which nothing else can stage. It exists so
	// the retry budget's ordering can be tested for what it does when the
	// timestamps stop resolving, rather than only for what it does when they
	// happen to.
	CollapseClaimTimestamps func(t *testing.T, convID string)
}

// ClaimPredicateFactory builds a fresh harness per subtest.
type ClaimPredicateFactory func(t *testing.T) ClaimPredicateHarness

// RunClaimPredicateConformance is the shared assertion suite for the
// needs-driving predicate and the display ladder derived from it — the two
// halves of "queued and running are read, never written". Both backends run
// the same subtests, and every arm runs against both runtimes: the ratchet
// decides which engine drives a claim, never whether the conversation is
// claimable.
func RunClaimPredicateConformance(t *testing.T, mk ClaimPredicateFactory) {
	t.Helper()
	ctx := context.Background()

	claim := func(t *testing.T, h ClaimPredicateHarness) *domain.Conversation {
		t.Helper()
		got, err := h.Stores.RunQueue.ClaimNextRun(ctx, "predicate-exec", 1, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		return got
	}
	mustClaim := func(t *testing.T, h ClaimPredicateHarness, convID string) *domain.Conversation {
		t.Helper()
		got := claim(t, h)
		if got == nil || got.ID != convID {
			t.Fatalf("ClaimNextRun = %+v, want conversation %s", got, convID)
		}
		return got
	}
	mustNotClaim := func(t *testing.T, h ClaimPredicateHarness) {
		t.Helper()
		if got := claim(t, h); got != nil {
			t.Fatalf("ClaimNextRun = %s, want nothing claimable", got.ID)
		}
	}
	release := func(t *testing.T, h ClaimPredicateHarness, orgID, convID, outcome string) {
		t.Helper()
		// The curator door is the only claims-release primitive shared by
		// both surfaces; it is keyed on the conversation's active claim and
		// carries no curator-specific write, so it is what the suite uses to
		// end an engagement.
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, orgID, convID, outcome, "", 0, 0, 0); err != nil {
			t.Fatalf("release claim: %v", err)
		}
	}
	userRow := func(content string, delivered bool) domain.Message {
		d := delivered
		return domain.Message{Role: "user", Content: content, Subtype: "", Delivered: &d, WindowState: domain.MessageWindowActive}
	}

	for _, runtime := range []string{domain.ConversationRuntimeSDK, domain.ConversationRuntimeNative} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			t.Run("FreshMint_IsClaimableThroughTheNullArm", func(t *testing.T) {
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				if st := h.StoredStatus(t, convID); st != "" {
					t.Fatalf("mint wrote status %q, want none", st)
				}
				got := mustClaim(t, h, convID)
				if got.Runtime != runtime {
					t.Errorf("claimed runtime = %q, want %q — the ratchet rides the claim", got.Runtime, runtime)
				}
				// The claim does not write a status either.
				if st := h.StoredStatus(t, convID); st != "" {
					t.Errorf("claim wrote status %q, want none", st)
				}
				mustNotClaim(t, h)
			})

			t.Run("ReapedMidRun_IsClaimableAgainWithNoOtherWrite", func(t *testing.T) {
				// The case the old predicate stranded: an engagement cut off
				// after every input was delivered. Nothing is queued, nothing
				// was concluded — and releasing the claim is the whole
				// recovery.
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, convID)
				h.InsertRow(t, convID, userRow("the opening turn", true))
				mustNotClaim(t, h)

				release(t, h, h.OrgID, convID, "reaped")

				if st := h.StoredStatus(t, convID); st != "" {
					t.Fatalf("the reap wrote status %q, want none — the release IS the requeue", st)
				}
				got := mustClaim(t, h, convID)
				if got.Attempts != 2 {
					t.Errorf("attempts = %d, want 2 (the re-claim counts)", got.Attempts)
				}
			})

			t.Run("Parked_NeedsInputToBecomeClaimableAgain", func(t *testing.T) {
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "parked")
				h.SetStoredStatus(t, convID, "open")
				mustNotClaim(t, h)

				// An injection is not drivable input — it rides whatever
				// engagement runs next and must not wake one.
				injected := userRow("a staged system note", false)
				injected.Subtype = "injection:system-note"
				h.InsertRow(t, convID, injected)
				mustNotClaim(t, h)

				// A withdrawn row never happened.
				withdrawn := userRow("withdrawn", false)
				withdrawn.WindowState = domain.MessageWindowInactive
				h.InsertRow(t, convID, withdrawn)
				mustNotClaim(t, h)

				// Real input, and nothing else, wakes it.
				h.InsertRow(t, convID, userRow("keep going", false))
				mustClaim(t, h, convID)
			})

			t.Run("Terminal_IsNeverClaimableWhateverRowsItHolds", func(t *testing.T) {
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "completed")
				h.SetStoredStatus(t, convID, "completed")
				h.SetBlueprintState(t, "completed", 0)
				h.InsertRow(t, convID, userRow("try again", false))
				mustNotClaim(t, h)

				// Only the explicit un-terminal write brings it back — and
				// then it is ordinary claimable work again. That is what makes
				// a follow-up on concluded work reachable at all: the queued
				// message alone never was, and never will be, enough.
				if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, convID); err != nil || !flipped {
					t.Fatalf("MarkQueuedForResume on a completed run = (%v, %v), want a flip", flipped, err)
				}
				mustClaim(t, h, convID)
			})

			t.Run("Failed_IsNotBroughtBackByTheUnTerminalWrite", func(t *testing.T) {
				// The one rest state with no way back. Its workspace did not
				// survive, so there is nothing to resume onto — the CAS has to
				// refuse rather than leave a row queued for a claim that would
				// find no tree.
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "failed")
				h.SetStoredStatus(t, convID, "failed")
				h.InsertRow(t, convID, userRow("try again", false))

				if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, convID); err != nil || flipped {
					t.Fatalf("MarkQueuedForResume on a failed run = (%v, %v), want no flip", flipped, err)
				}
				mustNotClaim(t, h)
				if st := h.StoredStatus(t, convID); st != "failed" {
					t.Errorf("stored status = %q, want failed — a refused flip writes nothing", st)
				}
			})

			// Ordinary sequential dispatch, which the gate must not narrow into
			// a stall: every step of a running blueprint is claimed exactly
			// once, in order, and exactly one is claimable at a time.
			t.Run("RunningBlueprint_EveryStepIsClaimedExactlyOnce", func(t *testing.T) {
				h := mk(t)
				for step := 0; step < 3; step++ {
					convID := h.EnqueueDelegation(t, runtime)
					mustClaim(t, h, convID)
					mustNotClaim(t, h)
					release(t, h, h.OrgID, convID, "completed")
					h.SetStoredStatus(t, convID, "completed")
				}
			})

			// A blueprint drives the step it is ON, and no other — while it is
			// still running, exactly as once it has stopped. The step being
			// dispatched holds the shared worktree, so an earlier one is not a
			// second unit of work but a second agent in one tree.
			t.Run("RunningBlueprint_DrivesOnlyItsCurrentStep", func(t *testing.T) {
				h := mk(t)

				// Step 0 runs and concludes; step 1 is enqueued behind it and
				// claimed, which is the ordinary sequential dispatch.
				first := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, first)
				release(t, h, h.OrgID, first, "completed")
				h.SetStoredStatus(t, first, "completed")

				second := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, second)

				// Now the follow-up on the COMPLETED earlier step, with step 1
				// still executing. Refused before it writes anything — the
				// un-terminal flip reads the parent's status too.
				if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, first); err != nil || flipped {
					t.Fatalf("MarkQueuedForResume(%s) = (%v, %v), want no flip under a running blueprint", first, flipped, err)
				}
				if st := h.StoredStatus(t, first); st != "completed" {
					t.Errorf("stored status = %q, want completed — a refused CAS writes nothing", st)
				}

				// And the queued row it would have woken changes nothing:
				// the conversation is terminal, so no predicate spans it.
				h.InsertRow(t, first, userRow("actually, one more thing", false))
				mustNotClaim(t, h)

				// It stays refused after the live step releases, now for the
				// second reason: the blueprint is on step 1, so step 0 is
				// behind it for good. That arm is the claim gate's — the flip
				// lands and nothing claims it.
				release(t, h, h.OrgID, second, "parked")
				h.SetStoredStatus(t, second, "open")
				h.SetBlueprintState(t, "aborted", 1)
				if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, first); err != nil || !flipped {
					t.Fatalf("MarkQueuedForResume(%s) = (%v, %v), want a flip once the blueprint stopped", first, flipped, err)
				}
				mustNotClaim(t, h)
			})

			// The finished-blueprint arm. A blueprint that reached a terminal
			// never restarts, but the conversation it came to rest on stays
			// claimable so a follow-up can be driven in the same workspace.
			// Every case below stages two conversations under one blueprint so
			// "only the last one" is asserted against a real sibling rather
			// than against nothing.
			t.Run("FinishedBlueprint", func(t *testing.T) {
				// stage conducts both conversations to a completed terminal and
				// settles the blueprint at currentStep, then re-queues the one
				// named by resumeStep. It returns the two conversation ids in
				// step order.
				stage := func(t *testing.T, h ClaimPredicateHarness, blueprintStatus string, currentStep int) (string, string) {
					t.Helper()
					var ids [2]string
					for i := range ids {
						ids[i] = h.EnqueueDelegation(t, runtime)
						mustClaim(t, h, ids[i])
						release(t, h, h.OrgID, ids[i], "completed")
						h.SetStoredStatus(t, ids[i], "completed")
					}
					h.SetBlueprintState(t, blueprintStatus, currentStep)
					return ids[0], ids[1]
				}
				resume := func(t *testing.T, h ClaimPredicateHarness, convID string) {
					t.Helper()
					if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, convID); err != nil || !flipped {
						t.Fatalf("MarkQueuedForResume(%s) = (%v, %v), want a flip", convID, flipped, err)
					}
				}

				t.Run("LastConversationIsClaimable_AnEarlierOneNever", func(t *testing.T) {
					h := mk(t)
					first, last := stage(t, h, "completed", 1)

					// The earlier step re-queues — the CAS is about the
					// conversation, not the plan — and then sits there. The
					// snapshot it would rehydrate is the last step's, so
					// driving it would answer out of the wrong workspace.
					resume(t, h, first)
					mustNotClaim(t, h)

					// The last one is the follow-up's home, and it is claimed
					// even with the un-claimable sibling ahead of it in the
					// queue.
					resume(t, h, last)
					mustClaim(t, h, last)
					mustNotClaim(t, h)

					// And again, indefinitely: the follow-up concludes, and
					// the next one is claimed the same way. Nothing about the
					// first resume consumed the conversation's claimability —
					// which is what "follow up on it again" means.
					release(t, h, h.OrgID, last, "completed")
					h.SetStoredStatus(t, last, "completed")
					resume(t, h, last)
					mustClaim(t, h, last)
				})

				t.Run("EarlyFinish_ResumesTheStepThatActuallyRan", func(t *testing.T) {
					// A blueprint that finished at step 0 of a two-step plan.
					// current_step_index is the durable record of where it
					// stopped, so the follow-up lands on step 0 — not on the
					// last step of the plan, which never ran.
					h := mk(t)
					ran, neverRan := stage(t, h, "completed", 0)

					resume(t, h, neverRan)
					mustNotClaim(t, h)

					resume(t, h, ran)
					mustClaim(t, h, ran)
				})

				t.Run("CancelledBlueprintDrivesNothing", func(t *testing.T) {
					// Called off is not the same as finished: nothing under a
					// cancelled blueprint is claimable, its last conversation
					// included.
					h := mk(t)
					_, last := stage(t, h, "cancelled", 1)
					resume(t, h, last)
					mustNotClaim(t, h)
				})
			})

			t.Run("Attempts_CountTheCurrentQueueEpisode_NotTheLifetime", func(t *testing.T) {
				// The retry budget's unit. The SQL is its definition, so
				// this is where it is pinned.
				//
				// Only the CLAIM path's Attempts. The display reads return a
				// lifetime engagement count under the same field name —
				// deliberately, and pinned in RunAgentRunConformance. See
				// domain.Conversation.Attempts for which is which.
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)

				// Four healthy engagements — the ordinary shape of a
				// conversation a user has followed up on a few times. Each
				// runs, parks, and is woken by the next message.
				for i := 0; i < 4; i++ {
					got := mustClaim(t, h, convID)
					if got.Attempts != 1 {
						t.Fatalf("engagement %d claimed with attempts = %d, want 1 — a healthy predecessor ends the episode", i+1, got.Attempts)
					}
					release(t, h, h.OrgID, convID, "parked")
					h.SetStoredStatus(t, convID, "open")
					h.InsertRow(t, convID, userRow("keep going", false))
				}

				// Now it hits a run of hand-backs. Those are the episode,
				// and only those: the budget counts from the fifth
				// engagement, not from the first.
				for want := 1; want <= 3; want++ {
					got := mustClaim(t, h, convID)
					if got.Attempts != want {
						t.Fatalf("hand-back %d claimed with attempts = %d, want %d", want, got.Attempts, want)
					}
					release(t, h, h.OrgID, convID, "requeued")
				}

				// A reap is a hand-back too — an engagement whose process
				// died recorded nothing about the conversation, so it stays
				// inside the episode. Excluding it is how a run that kills
				// its executor would crash-loop the dispatcher forever.
				got := mustClaim(t, h, convID)
				if got.Attempts != 4 {
					t.Fatalf("claim after three requeues = %d attempts, want 4", got.Attempts)
				}
				release(t, h, h.OrgID, convID, "reaped")
				if got := mustClaim(t, h, convID); got.Attempts != 5 {
					t.Fatalf("claim after three requeues and a reap = %d attempts, want 5 — a reap records nothing, so it does not end the episode", got.Attempts)
				}

				// One engagement that gets somewhere ends the episode, and
				// the next claim starts a fresh budget.
				release(t, h, h.OrgID, convID, "parked")
				h.SetStoredStatus(t, convID, "open")
				h.InsertRow(t, convID, userRow("try again", false))
				if got := mustClaim(t, h, convID); got.Attempts != 1 {
					t.Fatalf("claim after a healthy engagement = %d attempts, want 1", got.Attempts)
				}
			})

			t.Run("Attempts_ACoarseClockNeverOverCounts", func(t *testing.T) {
				// The ordering the episode is read off is timestamps, and
				// timestamps have a resolution. Every writer binds a
				// high-resolution instant today, so claims of one
				// conversation are always distinguishable — but the budget's
				// two failure directions are not symmetric, and the one that
				// matters is what happens when they stop being.
				//
				// Over-counting is the destructive one: a boundary hidden
				// between two same-instant claims leaves a spent episode
				// looking longer than it is, and a first engagement that
				// exhausts its budget is poison-pilled. Under-counting costs
				// at most one more round of retries. So the ordering is
				// written to collapse toward finding a boundary, and this
				// pins that at the limit: every timestamp identical, which
				// is as unresolved as a clock can get.
				h := mk(t)
				if h.CollapseClaimTimestamps == nil {
					t.Skip("harness cannot stage a coarse clock")
				}
				convID := h.EnqueueDelegation(t, runtime)

				// A hand-back, then a healthy engagement that ends the
				// episode, then one more hand-back.
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "requeued")
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "parked")
				h.SetStoredStatus(t, convID, "open")
				h.InsertRow(t, convID, userRow("keep going", false))
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "requeued")

				h.CollapseClaimTimestamps(t, convID)

				// Truthfully this is 2: one hand-back since the park. With
				// nothing left to order the claims by, the only answer that
				// is never destructive is to read the boundary as present.
				got := mustClaim(t, h, convID)
				if got.Attempts > 2 {
					t.Errorf("attempts = %d with every claim timestamped identically, want at most 2 — a clock that stops resolving must not spend budget the episode never used", got.Attempts)
				}
			})

			t.Run("Compaction_RequestDoesNotWake_FractionalSeqStillMatches", func(t *testing.T) {
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)
				mustClaim(t, h, convID)
				release(t, h, h.OrgID, convID, "parked")
				h.SetStoredStatus(t, convID, "open")

				// An in-flight compaction: the request row is inserted
				// DELIVERED, so it can never satisfy the undelivered arm and
				// wake a second engagement.
				req := userRow("<compaction request>", true)
				req.Subtype = "injection:compaction-request"
				h.InsertRow(t, convID, req)
				mustNotClaim(t, h)

				// A compaction commit re-seqs queued input to fractional
				// positions; nothing in the predicate reads seq, so the row
				// still wakes the conversation.
				id := h.InsertRow(t, convID, userRow("queued across the compaction", false))
				h.SetSeq(t, id, 12.5)
				mustClaim(t, h, convID)
			})

			t.Run("DisplayStatus_RendersTheFrozenVocabulary", func(t *testing.T) {
				h := mk(t)
				convID := h.EnqueueDelegation(t, runtime)

				// Derived queued: mid-flight, nobody driving.
				if st := h.DisplayStatus(t, convID); st != "queued" {
					t.Errorf("fresh mint displays %q, want queued", st)
				}
				// Running: an engagement exists.
				mustClaim(t, h, convID)
				if st := h.DisplayStatus(t, convID); st != "running" {
					t.Errorf("claimed conversation displays %q, want running", st)
				}
				// Every setup transient surfaces through the same field.
				// Coverage is derived from the canonical vocabulary, not a
				// copy of it: a phase added in Go and never taught to a store
				// fails here, on both dialects.
				for _, phase := range domain.AllClaimPhases() {
					if err := h.Stores.Conversations.SetActiveClaimPhaseSystem(ctx, h.OrgID, convID, phase); err != nil {
						t.Fatalf("set phase %s: %v", phase, err)
					}
					if st := h.DisplayStatus(t, convID); st != phase {
						t.Errorf("phase %s displays %q, want the phase itself", phase, st)
					}
				}
				if err := h.Stores.Conversations.SetActiveClaimPhaseSystem(ctx, h.OrgID, convID, ""); err != nil {
					t.Fatalf("clear phase: %v", err)
				}
				if st := h.DisplayStatus(t, convID); st != "running" {
					t.Errorf("cleared phase displays %q, want running", st)
				}

				// Parked.
				release(t, h, h.OrgID, convID, "parked")
				h.SetStoredStatus(t, convID, "open")
				if st := h.DisplayStatus(t, convID); st != "open" {
					t.Errorf("parked conversation displays %q, want open", st)
				}
				// Parked and woken: queued again, exactly as the resume
				// flip used to render it.
				h.InsertRow(t, convID, userRow("keep going", false))
				if st := h.DisplayStatus(t, convID); st != "queued" {
					t.Errorf("woken parked conversation displays %q, want queued", st)
				}

				// Terminals render verbatim.
				for _, term := range domain.AllTerminalRunStatuses() {
					h.SetStoredStatus(t, convID, term)
					if st := h.DisplayStatus(t, convID); st != term {
						t.Errorf("terminal %s displays %q", term, st)
					}
				}
			})
		})
	}

	// Runtime-independent: the mint refuses what the gate could never admit.
	t.Run("BlueprintConversationWithoutAStepIndexIsRefusedAtTheMint", func(t *testing.T) {
		h := mk(t)
		if h.EnqueueUnindexed == nil {
			t.Skip("harness cannot stage an unindexed mint")
		}
		err := h.EnqueueUnindexed(t)
		if !errors.Is(err, db.ErrBlueprintStepUnindexed) {
			t.Fatalf("EnqueueRun with no step index = %v, want ErrBlueprintStepUnindexed", err)
		}
		// The refusal is the whole point: nothing landed, so nothing is
		// sitting in the work list that no claim could ever reach.
		mustNotClaim(t, h)
	})

	// The claim gate's `open` arm is an UN-PARK, and it has to clear the
	// park's own columns — park_reason included.
	//
	// This is the second door out of a park, and the quieter one. The loud
	// one is MarkQueuedForResume; this is a parked conversation woken by
	// undelivered input and claimed straight off the queue, which is what an
	// ordinary follow-up does. park_reason answers "why is this parked", so
	// once the row is mid-flight again the old value is not history, it is a
	// wrong answer to the only question the column asks — and it is
	// single-valued, so nothing overwrites it until the NEXT park. A stale
	// one therefore rides this engagement all the way to whatever terminal
	// follows, and the run station prints it beside a failed run as "stopped
	// by user" on a run nobody stopped.
	t.Run("ClaimingAParkedConversationClearsItsParkReason", func(t *testing.T) {
		h := mk(t)
		convID := h.EnqueueDelegation(t, "sdk")
		if _, err := h.Stores.Conversations.ParkOpen(ctx, h.OrgID, convID,
			db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil {
			t.Fatalf("park: %v", err)
		}
		if got, _ := h.Stores.Conversations.Get(ctx, h.OrgID, convID); got.ParkReason != domain.ParkReasonUserCancelled {
			t.Fatalf("precondition: park_reason = %q, want user_cancelled", got.ParkReason)
		}

		// A follow-up wakes it, and the dispatcher claims it directly — no
		// resume flip in between. That is the whole path.
		h.InsertRow(t, convID, userRow("keep going", false))
		mustClaim(t, h, convID)

		got, err := h.Stores.Conversations.Get(ctx, h.OrgID, convID)
		if err != nil || got == nil {
			t.Fatalf("Get: err=%v got=%v", err, got)
		}
		if got.ParkReason != "" {
			t.Errorf("park_reason = %q after the claim un-parked it, want cleared — "+
				"the row is mid-flight, so it is not parked for any reason", got.ParkReason)
		}
	})
}
