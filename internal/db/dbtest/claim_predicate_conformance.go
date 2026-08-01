package dbtest

import (
	"context"
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
	// blueprint_run and returns its id. runtime is 'sdk' or 'native': the
	// dialect stamps its own at mint (Postgres native, SQLite sdk), so the
	// seeder rewrites the column to cover both engines against one backend.
	EnqueueDelegation func(t *testing.T, runtime string) (convID string)

	// SetStoredStatus writes conversations.status directly — the fixture
	// door for the parks and terminals the suite needs on demand. An empty
	// status writes SQL NULL (the mid-flight state).
	SetStoredStatus func(t *testing.T, convID, status string)

	// StoredStatus reads conversations.status back, SQL NULL as "".
	StoredStatus func(t *testing.T, convID string) string

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
				h.InsertRow(t, convID, userRow("try again", false))
				mustNotClaim(t, h)

				// Only the explicit un-terminal write brings it back, and
				// only for the resumable shape (completed + abort).
				if flipped, err := h.Stores.Conversations.MarkQueuedForResume(ctx, h.OrgID, convID); err != nil || flipped {
					t.Fatalf("MarkQueuedForResume on a plain completed run = (%v, %v), want no flip", flipped, err)
				}
				mustNotClaim(t, h)
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
				for _, term := range []string{"completed", "failed", "cancelled", "task_unsolvable"} {
					h.SetStoredStatus(t, convID, term)
					if st := h.DisplayStatus(t, convID); st != term {
						t.Errorf("terminal %s displays %q", term, st)
					}
				}
			})
		})
	}
}
