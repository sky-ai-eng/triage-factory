package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CuratorHarness is what a per-backend test file hands to
// RunCuratorStoreConformance: the full store bundle (the suite drives the
// app-pool methods through Tx.SyntheticClaimsWithTx exactly like the
// production session goroutine, and the System methods on the top-level
// handle), the identity every claims-bound call runs under, and a project
// seeder against the backend's own connection.
type CuratorHarness struct {
	Stores db.Stores
	OrgID  string
	UserID string
	// SeedProject creates a project (owned by the harness identity's team)
	// and returns its id.
	SeedProject func(t *testing.T, name string) string
}

// CuratorStoreFactory builds a fresh harness per subtest.
type CuratorStoreFactory func(t *testing.T) CuratorHarness

// RunCuratorStoreConformance is the shared assertion suite for any
// db.CuratorStore implementation over the conversations/messages/claims
// tables. Backend tests invoke it with their factory; both backends run the
// same subtests.
func RunCuratorStoreConformance(t *testing.T, mk CuratorStoreFactory) {
	t.Helper()
	ctx := context.Background()

	withClaims := func(t *testing.T, h CuratorHarness, fn func(ts db.TxStores) error) {
		t.Helper()
		if err := h.Stores.Tx.SyntheticClaimsWithTx(ctx, h.OrgID, h.UserID, fn); err != nil {
			t.Fatalf("claims-bound call: %v", err)
		}
	}

	// claimTurn drives the ONE claim loop far enough to own convID's oldest
	// queued turn, returning the minted claim id. There is no curator-only
	// claim door any more: a curator conversation holding an undelivered
	// user message is simply one of the shapes the needs-driving predicate
	// matches, so the fixture claims it the way production does. The claim
	// is cross-org and takes the globally-oldest eligible conversation, so
	// this asserts the identity rather than let a mis-claim pass silently.
	claimTurnAs := func(t *testing.T, h CuratorHarness, convID, executorID string, bootEpoch int64) string {
		t.Helper()
		got, err := h.Stores.RunQueue.ClaimNextRun(ctx, executorID, bootEpoch, db.ClaimPlacement{})
		if err != nil || got == nil || got.ID != convID {
			t.Fatalf("ClaimNextRun = (%+v, %v), want a claim on conversation %s", got, err, convID)
		}
		return got.ClaimID
	}
	claimTurn := func(t *testing.T, h CuratorHarness, convID string) string {
		t.Helper()
		return claimTurnAs(t, h, convID, "exec-1", 1)
	}

	// claimTurnRefused asserts the loop finds convID unclaimable — the
	// derived answer to "another engagement owns it" / "the turn is gone".
	claimTurnRefused := func(t *testing.T, h CuratorHarness, convID string) {
		t.Helper()
		got, err := h.Stores.RunQueue.ClaimNextRun(ctx, "exec-1", 1, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		if got != nil && got.ID == convID {
			t.Fatalf("conversation %s was claimable; want it refused", convID)
		}
	}

	seedTurn := func(t *testing.T, h CuratorHarness, projectID, input string) (convID string, msgID int64) {
		t.Helper()
		withClaims(t, h, func(ts db.TxStores) error {
			conv, err := ts.Curator.GetOrCreateConversation(ctx, h.OrgID, projectID, h.UserID)
			if err != nil {
				return err
			}
			convID = conv.ID
			id, err := ts.Curator.EnqueueUserMessage(ctx, h.OrgID, conv.ID, h.UserID, input)
			if err != nil {
				return err
			}
			msgID = id
			return nil
		})
		return convID, msgID
	}

	t.Run("Conversation_FindOrMint_IsStable", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "mint")
		var first, second *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetOrCreateConversation(ctx, h.OrgID, projectID, h.UserID)
			first = c
			return err
		})
		if first == nil {
			t.Fatal("mint returned nil conversation")
		}
		if first.Type != "curator" || first.Visibility != "private" {
			t.Errorf("minted conversation (type=%q, visibility=%q), want (curator, private)", first.Type, first.Visibility)
		}
		if first.ProjectID != projectID || first.CreatorUserID != h.UserID {
			t.Errorf("minted conversation (project=%q, creator=%q), want (%q, %q)", first.ProjectID, first.CreatorUserID, projectID, h.UserID)
		}
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetOrCreateConversation(ctx, h.OrgID, projectID, h.UserID)
			second = c
			return err
		})
		if second == nil || second.ID != first.ID {
			t.Errorf("second find-or-mint returned a different conversation (%v), want the live one %s", second, first.ID)
		}
		var live *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			live = c
			return err
		})
		if live == nil || live.ID != first.ID {
			t.Errorf("GetLiveConversation = %v, want %s", live, first.ID)
		}
	})

	t.Run("TurnLifecycle_ClaimBeginRelease", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "lifecycle")
		convID, msgID := seedTurn(t, h, projectID, "hello world")

		var inFlight *db.CuratorInFlightTurn
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight == nil || inFlight.Running || inFlight.MessageID != msgID {
			t.Fatalf("InFlightTurn = %+v, want queued turn for message %d", inFlight, msgID)
		}

		claimID := claimTurnAs(t, h, convID, "exec-1", 7)
		if claimID == "" {
			t.Fatal("claim carried no id")
		}
		// Single-active: the claimed conversation is no longer claimable.
		claimTurnRefused(t, h, convID)

		var start *db.CuratorTurnStart
		withClaims(t, h, func(ts db.TxStores) error {
			s, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			start = s
			return err
		})
		if start == nil || start.UserInput != "hello world" {
			t.Fatalf("BeginTurn start = %+v, want user input round-tripped", start)
		}
		if start.Project == nil || start.Project.ID != projectID {
			t.Fatalf("BeginTurn project = %+v, want %s", start.Project, projectID)
		}

		// A duplicate begin (re-fed turn) finds the message delivered.
		err := h.Stores.Tx.SyntheticClaimsWithTx(ctx, h.OrgID, h.UserID, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			return err
		})
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("duplicate BeginTurn err = %v, want sql.ErrNoRows", err)
		}

		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight == nil || !inFlight.Running || inFlight.MessageID != msgID {
			t.Fatalf("InFlightTurn after begin = %+v, want running turn for message %d", inFlight, msgID)
		}

		// The sink's write set: one assistant row stamped with the claim.
		ip := func(n int) *int { return &n }
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Conversations.InsertMessage(ctx, h.OrgID, &domain.Message{
				ConversationID: convID, UserID: h.UserID, ClaimID: claimID,
				Role: "assistant", Content: "ack",
				InputTokens: ip(11), OutputTokens: ip(22), CacheReadTokens: ip(33), CacheCreationTokens: ip(44),
			})
			return err
		})

		flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0.05, 1200, 3)
		if err != nil || !flipped {
			t.Fatalf("ReleaseActiveTurnSystem: flipped=%v err=%v", flipped, err)
		}
		if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0, 0, 0); err != nil || flipped {
			t.Fatalf("second release: flipped=%v err=%v, want no-op", flipped, err)
		}

		var claims []domain.Claim
		withClaims(t, h, func(ts db.TxStores) error {
			cs, err := ts.Curator.ListClaims(ctx, h.OrgID, convID)
			claims = cs
			return err
		})
		if len(claims) != 1 {
			t.Fatalf("claims = %d, want 1", len(claims))
		}
		cl := claims[0]
		if cl.ID != claimID || cl.ExecutorID != "exec-1" || cl.BootEpoch != 7 {
			t.Errorf("claim identity = (%s, %s, %d), want (%s, exec-1, 7)", cl.ID, cl.ExecutorID, cl.BootEpoch, claimID)
		}
		if cl.ReleasedAt == nil || cl.Outcome != "completed" {
			t.Errorf("claim terminal = (released=%v, outcome=%q), want released completed", cl.ReleasedAt, cl.Outcome)
		}
		if cl.DurationMs == nil || *cl.DurationMs != 1200 || cl.NumTurns == nil || *cl.NumTurns != 3 {
			t.Errorf("claim telemetry = (%v, %v), want (1200, 3)", cl.DurationMs, cl.NumTurns)
		}

		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight != nil {
			t.Errorf("InFlightTurn after release = %+v, want nil", inFlight)
		}

		// Transcript read: user turn + assistant row, Delivered/ClaimID
		// surfaced for the history synthesizer.
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		if len(msgs) != 2 {
			t.Fatalf("messages = %d, want 2", len(msgs))
		}
		if msgs[0].Role != "user" || msgs[0].Delivered == nil || !*msgs[0].Delivered {
			t.Errorf("user row = (role=%q, delivered=%v), want delivered user row", msgs[0].Role, msgs[0].Delivered)
		}
		if msgs[1].ClaimID != claimID {
			t.Errorf("assistant row claim = %q, want %q", msgs[1].ClaimID, claimID)
		}
		// The release settled the turn's dollars as ONE lump on the claim's
		// last row — the assistant row here, not the user row.
		if c := msgs[1].CostUSD; c == nil || *c < 0.049 || *c > 0.051 {
			t.Errorf("last row cost_usd = %v, want ~0.05 (the release's lump)", msgs[1].CostUSD)
		}
		if msgs[0].CostUSD != nil {
			t.Errorf("user row cost_usd = %v, want nil (the lump lands on the LAST row only)", *msgs[0].CostUSD)
		}
	})

	t.Run("Transcript_LimitKeepsTheNewestRowsOldestFirst", func(t *testing.T) {
		// The history read is bounded so a months-old conversation isn't a
		// whole-transcript scan per page load. The bound has to keep the NEWEST
		// rows and still hand them back oldest-first — an ASC read with a LIMIT
		// would return the oldest N and call it the transcript.
		h := mk(t)
		projectID := h.SeedProject(t, "tail-bound")
		convID, _ := seedTurn(t, h, projectID, "turn 1")
		for _, body := range []string{"turn 2", "turn 3", "turn 4"} {
			withClaims(t, h, func(ts db.TxStores) error {
				_, err := ts.Curator.EnqueueUserMessage(ctx, h.OrgID, convID, h.UserID, body)
				return err
			})
		}

		var all, tail []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			a, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			if err != nil {
				return err
			}
			all = a
			tl, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 2)
			tail = tl
			return err
		})
		if len(all) != 4 {
			t.Fatalf("unbounded read = %d rows, want 4", len(all))
		}
		if len(tail) != 2 {
			t.Fatalf("limit=2 read = %d rows, want 2", len(tail))
		}
		if tail[0].Content != "turn 3" || tail[1].Content != "turn 4" {
			t.Errorf("tail = %q/%q, want the last two turns oldest-first (turn 3, turn 4)",
				tail[0].Content, tail[1].Content)
		}
	})

	t.Run("Release_NothingStreamed_SettlesOnUserRow", func(t *testing.T) {
		// A turn that delivered but streamed nothing still settles its lump:
		// BeginTurn stamped the user row with the claim, so that row is the
		// claim's last (and only) message.
		h := mk(t)
		projectID := h.SeedProject(t, "lump-fallback")
		convID, msgID := seedTurn(t, h, projectID, "no stream")
		claimTurn(t, h, convID)
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			return err
		})
		if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0.03, 500, 1); err != nil || !flipped {
			t.Fatalf("release: flipped=%v err=%v", flipped, err)
		}
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		if len(msgs) != 1 {
			t.Fatalf("messages = %d, want 1 (just the user row)", len(msgs))
		}
		if c := msgs[0].CostUSD; c == nil || *c < 0.029 || *c > 0.031 {
			t.Errorf("user row cost_usd = %v, want ~0.03 (fallback settlement)", msgs[0].CostUSD)
		}
	})

	t.Run("Release_SkipsSyntheticRowWhenSettling", func(t *testing.T) {
		// A turn that streamed a real answer and then died on an API error
		// ends on a runtime-composed row. The lump belongs to the model that
		// actually ran, so it settles on the assistant row behind it.
		h := mk(t)
		projectID := h.SeedProject(t, "synthetic-settle")
		convID, msgID := seedTurn(t, h, projectID, "answer then fail")
		claimID := claimTurn(t, h, convID)
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			return err
		})
		withClaims(t, h, func(ts db.TxStores) error {
			if _, err := ts.Conversations.InsertMessage(ctx, h.OrgID, &domain.Message{
				ConversationID: convID, UserID: h.UserID, ClaimID: claimID,
				Role: "assistant", Content: "real turn", Model: "claude-opus-5",
			}); err != nil {
				return err
			}
			// A newer no-model row (the turn got as far as a tool call): not
			// synthetic, so only the real-model preference keeps the lump off
			// it and inside the per-model breakdown.
			if _, err := ts.Conversations.InsertMessage(ctx, h.OrgID, &domain.Message{
				ConversationID: convID, UserID: h.UserID, ClaimID: claimID,
				Role: "tool", Content: "tool result",
			}); err != nil {
				return err
			}
			_, err := ts.Conversations.InsertMessage(ctx, h.OrgID, &domain.Message{
				ConversationID: convID, UserID: h.UserID, ClaimID: claimID,
				Role: "assistant", Content: "API Error: overloaded",
				Model: domain.ModelSynthetic,
			})
			return err
		})
		if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "overloaded", 0.07, 900, 2); err != nil || !flipped {
			t.Fatalf("release: flipped=%v err=%v", flipped, err)
		}
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		if len(msgs) != 4 {
			t.Fatalf("messages = %d, want 4 (user + assistant + tool + synthetic)", len(msgs))
		}
		if c := msgs[1].CostUSD; c == nil || *c < 0.069 || *c > 0.071 {
			t.Errorf("real-model row cost_usd = %v, want ~0.07 (the turn's lump)", msgs[1].CostUSD)
		}
		if msgs[2].CostUSD != nil {
			t.Errorf("no-model row cost_usd = %v, want nil (a real-model row outranks it however much older)", *msgs[2].CostUSD)
		}
		if msgs[3].CostUSD != nil {
			t.Errorf("synthetic row cost_usd = %v, want nil (never a settle target)", *msgs[3].CostUSD)
		}
	})

	t.Run("BeginTurn_StampsDeliveringClaimOnUserRow", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "claim-stamp")
		convID, msgID := seedTurn(t, h, projectID, "stamp me")

		claimID := claimTurn(t, h, convID)
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			return err
		})

		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		if len(msgs) != 1 || msgs[0].ClaimID != claimID {
			t.Fatalf("user row claim after BeginTurn = %+v, want stamped with %s", msgs, claimID)
		}
		// The delivering claim therefore never reads as a failed pickup, even
		// when the turn itself ends failed with nothing streamed.
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "agent blew up", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		count, lastErr, err := h.Stores.Curator.FailedTurnAttemptsSystem(ctx, h.OrgID, convID, msgID)
		if err != nil {
			t.Fatalf("FailedTurnAttemptsSystem: %v", err)
		}
		if count != 0 || lastErr != "" {
			t.Errorf("attempts for a delivered turn = (%d, %q), want (0, \"\") — the stamp attaches the user row to the claim", count, lastErr)
		}
	})

	t.Run("DeadLetter_PoisonedTurnRetiresAtCap_TransientFailureStaysClaimable", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "dead-letter")
		convID, msgID := seedTurn(t, h, projectID, "poisoned")

		// Two failed pickups: claim minted, BeginTurn never delivered (no
		// stamp), claim released failed. Exactly the loop shape a permanent
		// BeginTurn failure produces.
		for i, boom := range []string{"boom one", "boom two"} {
			claimTurn(t, h, convID)
			if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", boom, 0, 0, 0); err != nil || !flipped {
				t.Fatalf("release %d: flipped=%v err=%v", i, flipped, err)
			}
		}
		count, lastErr, err := h.Stores.Curator.FailedTurnAttemptsSystem(ctx, h.OrgID, convID, msgID)
		if err != nil {
			t.Fatalf("FailedTurnAttemptsSystem: %v", err)
		}
		if count != 2 || lastErr != "boom two" {
			t.Fatalf("attempts = (%d, %q), want (2, \"boom two\")", count, lastErr)
		}

		// Under the cap the message is untouched — still the queued turn a
		// later scan re-claims (the transient-failure retry path).
		var inFlight *db.CuratorInFlightTurn
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight == nil || inFlight.Running || inFlight.MessageID != msgID {
			t.Fatalf("turn after transient failures = %+v, want still queued message %d", inFlight, msgID)
		}

		// A third pickup, whose engagement reaches the cap: the dead-letter
		// runs holding that claim (the loop mints it before handing the turn
		// over), retiring the message and releasing the claim in one op.
		claimTurn(t, h, convID)
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "boom three", 0, 0, 0); err != nil {
			t.Fatalf("release third pickup: %v", err)
		}
		claimTurn(t, h, convID)
		finalErr := "curator turn failed 3 times, giving up: boom three"
		matched, err := h.Stores.Curator.DeadLetterTurnSystem(ctx, h.OrgID, convID, msgID, finalErr)
		if err != nil || !matched {
			t.Fatalf("dead-letter: matched=%v err=%v", matched, err)
		}
		// Idempotence: the message is delivered now, so a duplicate feed's
		// dead-letter reports a miss (and finds no claim left to release).
		if matched, err := h.Stores.Curator.DeadLetterTurnSystem(ctx, h.OrgID, convID, msgID, finalErr); err != nil || matched {
			t.Fatalf("duplicate dead-letter: matched=%v err=%v, want miss", matched, err)
		}

		var claims []domain.Claim
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			cs, err := ts.Curator.ListClaims(ctx, h.OrgID, convID)
			if err != nil {
				return err
			}
			claims = cs
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		if len(claims) != 4 {
			t.Fatalf("claims = %d, want 4 (three failed pickups + the engagement that dead-lettered)", len(claims))
		}
		final := claims[len(claims)-1]
		if final.ReleasedAt == nil || final.Outcome != "failed" || final.Error != finalErr {
			t.Errorf("final claim = %+v, want released failed with the give-up error", final)
		}
		// The retired message stays visible history — delivered, inactive,
		// stamped with the final claim so synthesis renders the turn failed.
		if len(msgs) != 1 {
			t.Fatalf("messages = %d, want the retired user row still visible", len(msgs))
		}
		m := msgs[0]
		if m.Delivered == nil || !*m.Delivered || m.WindowState != domain.MessageWindowInactive || m.ClaimID != final.ID {
			t.Errorf("retired row = (delivered=%v, window=%q, claim=%q), want (true, inactive, %s)", m.Delivered, m.WindowState, m.ClaimID, final.ID)
		}
		// Out of the claimable scan and the in-flight lookup for good.
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight != nil {
			t.Errorf("InFlightTurn after dead-letter = %+v, want nil", inFlight)
		}
	})

	t.Run("FailedAttempts_AttributeToExactTurn", func(t *testing.T) {
		// The cross-attribution regression: two queued turns on ONE
		// conversation, one failed pickup each. A time-window count would
		// fold B's failure into A's total (A read 2); exact message_id
		// attribution keeps each turn at its own 1.
		h := mk(t)
		projectID := h.SeedProject(t, "exact-attribution")
		convID, msgA := seedTurn(t, h, projectID, "turn A")
		_, msgB := seedTurn(t, h, projectID, "turn B")

		// One failed pickup on A (the oldest queued turn, so the claim is
		// minted to it), then A is retired so the next claim is minted to B.
		claimTurn(t, h, convID)
		if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "boom A", 0, 0, 0); err != nil || !flipped {
			t.Fatalf("release for A: flipped=%v err=%v", flipped, err)
		}
		claimTurn(t, h, convID)
		if matched, err := h.Stores.Curator.DeadLetterTurnSystem(ctx, h.OrgID, convID, msgA, "retire A"); err != nil || !matched {
			t.Fatalf("retire A: matched=%v err=%v", matched, err)
		}
		claimTurn(t, h, convID)
		if flipped, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "boom B", 0, 0, 0); err != nil || !flipped {
			t.Fatalf("release for B: flipped=%v err=%v", flipped, err)
		}

		countA, lastA, err := h.Stores.Curator.FailedTurnAttemptsSystem(ctx, h.OrgID, convID, msgA)
		if err != nil {
			t.Fatalf("attempts for A: %v", err)
		}
		if countA != 1 || lastA != "boom A" {
			t.Errorf("attempts for A = (%d, %q), want (1, \"boom A\") — B's failure must not count", countA, lastA)
		}
		countB, lastB, err := h.Stores.Curator.FailedTurnAttemptsSystem(ctx, h.OrgID, convID, msgB)
		if err != nil {
			t.Fatalf("attempts for B: %v", err)
		}
		if countB != 1 || lastB != "boom B" {
			t.Errorf("attempts for B = (%d, %q), want (1, \"boom B\")", countB, lastB)
		}
	})

	t.Run("DeleteQueuedTurnsForProject_DrainsOnlyQueuedTurns", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "drain")
		otherProject := h.SeedProject(t, "drain-other")

		// One delivered turn, one queued turn, one pending injection on the
		// drained project; a queued turn on the other project.
		convID, deliveredID := seedTurn(t, h, projectID, "delivered turn")
		claimTurn(t, h, convID)
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, deliveredID)
			return err
		})
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		_, queuedID := seedTurn(t, h, projectID, "queued turn")
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
			t.Fatalf("queue injection: %v", err)
		}
		_, otherQueued := seedTurn(t, h, otherProject, "other project's turn")

		n, err := h.Stores.Curator.DeleteQueuedTurnsForProjectSystem(ctx, h.OrgID, projectID)
		if err != nil || n != 1 {
			t.Fatalf("drain = (%d, %v), want exactly the queued turn deleted", n, err)
		}
		if n, err := h.Stores.Curator.DeleteQueuedTurnsForProjectSystem(ctx, h.OrgID, projectID); err != nil || n != 0 {
			t.Fatalf("second drain = (%d, %v), want 0", n, err)
		}

		// Nothing claimable remains on the drained project; delivered history
		// and the pending injection survive.
		var inFlight *db.CuratorInFlightTurn
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight != nil {
			t.Errorf("InFlightTurn after drain = %+v, want nil", inFlight)
		}
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		sawDelivered, sawInjection := false, false
		for _, m := range msgs {
			switch {
			case m.ID == int(deliveredID):
				sawDelivered = true
			case m.ID == int(queuedID):
				t.Errorf("queued row %d survived the drain", m.ID)
			case m.Subtype == "injection:context":
				sawInjection = true
			}
		}
		if !sawDelivered || !sawInjection {
			t.Errorf("post-drain transcript (delivered=%v, injection=%v), want both surviving", sawDelivered, sawInjection)
		}

		// The other project's queued turn is untouched.
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, otherProject, h.UserID)
			inFlight = f
			return err
		})
		if inFlight == nil || inFlight.Running || inFlight.MessageID != otherQueued {
			t.Errorf("other project's turn = %+v, want queued message %d untouched", inFlight, otherQueued)
		}
	})

	t.Run("QueuedTurn_CancelsByDeletion", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "queued-cancel")
		convID, msgID := seedTurn(t, h, projectID, "never runs")

		var deleted bool
		withClaims(t, h, func(ts db.TxStores) error {
			d, err := ts.Curator.DeleteQueuedTurn(ctx, h.OrgID, convID, msgID)
			deleted = d
			return err
		})
		if !deleted {
			t.Fatal("DeleteQueuedTurn = false, want true for an undelivered turn")
		}
		withClaims(t, h, func(ts db.TxStores) error {
			d, err := ts.Curator.DeleteQueuedTurn(ctx, h.OrgID, convID, msgID)
			deleted = d
			return err
		})
		if deleted {
			t.Error("second DeleteQueuedTurn = true, want false (row gone)")
		}
		var inFlight *db.CuratorInFlightTurn
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight != nil {
			t.Errorf("InFlightTurn after deletion = %+v, want nil", inFlight)
		}
	})

	t.Run("PendingContext_ProduceConsumeRevert", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "pending")

		// No live conversation → the producer is a no-op, not an error.
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
			t.Fatalf("producer with no conversations: %v", err)
		}

		convID, msgID := seedTurn(t, h, projectID, "with context")
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
			t.Fatalf("queue pinned: %v", err)
		}
		// One-pending-per-type: a second delta of the same type REPLACES the
		// first (newest baseline wins).
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypePinnedRepos, `["a/b","c/d"]`); err != nil {
			t.Fatalf("re-queue pinned: %v", err)
		}
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypeJiraProjectKey, `"SKY"`); err != nil {
			t.Fatalf("queue jira: %v", err)
		}

		claimTurn(t, h, convID)
		var start *db.CuratorTurnStart
		withClaims(t, h, func(ts db.TxStores) error {
			s, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			start = s
			return err
		})
		if len(start.Consumed) != 2 {
			t.Fatalf("consumed = %d rows (%+v), want 2 (one per change_type)", len(start.Consumed), start.Consumed)
		}
		byType := map[string]domain.CuratorContextChange{}
		for _, c := range start.Consumed {
			byType[c.ChangeType] = c
		}
		if got := byType[domain.ChangeTypePinnedRepos].BaselineValue; got != `["a/b","c/d"]` {
			t.Errorf("pinned baseline = %q, want the replacement %q", got, `["a/b","c/d"]`)
		}
		if got := byType[domain.ChangeTypeJiraProjectKey].BaselineValue; got != `"SKY"` {
			t.Errorf("jira baseline = %q, want %q", got, `"SKY"`)
		}

		// The audit row the dispatch writes after rendering.
		var auditID int64
		withClaims(t, h, func(ts db.TxStores) error {
			id, err := ts.Conversations.InsertMessage(ctx, h.OrgID, &domain.Message{
				ConversationID: convID, UserID: h.UserID,
				Role: "system", Subtype: "context_change", Content: "[system note] ...",
			})
			auditID = id
			return err
		})

		// A NEW pinned delta lands while the turn runs — on revert, the
		// consumed pinned row must NOT resurrect (the newer row already
		// carries the pending delta), while the jira row flips back.
		if err := h.Stores.Curator.QueueContextChangeSystem(ctx, h.OrgID, projectID, domain.ChangeTypePinnedRepos, `["x/y"]`); err != nil {
			t.Fatalf("mid-turn queue: %v", err)
		}
		withClaims(t, h, func(ts db.TxStores) error {
			return ts.Curator.RevertTurnContext(ctx, h.OrgID, convID, start.Consumed, auditID)
		})

		// Release, then a second turn consumes exactly the undelivered set:
		// the fresh pinned row + the reverted jira row.
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "failed", "boom", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		_, msg2 := seedTurn(t, h, projectID, "retry")
		claimTurn(t, h, convID)
		withClaims(t, h, func(ts db.TxStores) error {
			s, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msg2)
			start = s
			return err
		})
		if len(start.Consumed) != 2 {
			t.Fatalf("retry consumed = %d rows (%+v), want 2", len(start.Consumed), start.Consumed)
		}
		byType = map[string]domain.CuratorContextChange{}
		for _, c := range start.Consumed {
			byType[c.ChangeType] = c
		}
		if got := byType[domain.ChangeTypePinnedRepos].BaselineValue; got != `["x/y"]` {
			t.Errorf("retry pinned baseline = %q, want the newer %q (stale revert must not double)", got, `["x/y"]`)
		}
		if got := byType[domain.ChangeTypeJiraProjectKey].BaselineValue; got != `"SKY"` {
			t.Errorf("retry jira baseline = %q, want the reverted %q", got, `"SKY"`)
		}

		// The audit row was deleted by the revert.
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		for _, m := range msgs {
			if m.Subtype == "context_change" {
				t.Errorf("context_change audit row %d survived the revert", m.ID)
			}
		}
	})

	t.Run("Archive_ResetsPerCreator", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "reset")
		convID, msgID := seedTurn(t, h, projectID, "queued at reset")

		// A live engagement blocks the reset.
		claimTurn(t, h, convID)
		err := h.Stores.Tx.SyntheticClaimsWithTx(ctx, h.OrgID, h.UserID, func(ts db.TxStores) error {
			_, e := ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			return e
		})
		if !errors.Is(err, db.ErrCuratorInFlight) {
			t.Fatalf("archive with live claim err = %v, want ErrCuratorInFlight", err)
		}

		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "cancelled", "user cancelled", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		var archivedID string
		withClaims(t, h, func(ts db.TxStores) error {
			id, e := ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			archivedID = id
			return e
		})
		if archivedID != convID {
			t.Fatalf("archive returned id %q, want the archived conversation %q", archivedID, convID)
		}

		var live *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			live = c
			return err
		})
		if live != nil {
			t.Fatalf("live conversation after archive = %+v, want nil", live)
		}
		// The queued row was deleted with the archive; the archived
		// transcript keeps only delivered history.
		var msgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID, 0)
			msgs = ms
			return err
		})
		for _, m := range msgs {
			if m.ID == int(msgID) {
				t.Errorf("undelivered queued row %d survived the archive", m.ID)
			}
		}
		// The next find-or-mint starts a fresh conversation.
		var fresh *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetOrCreateConversation(ctx, h.OrgID, projectID, h.UserID)
			fresh = c
			return err
		})
		if fresh == nil || fresh.ID == convID {
			t.Errorf("post-archive mint = %+v, want a fresh conversation (old %s)", fresh, convID)
		}
		// Archiving with no live conversation is a tolerated no-op... after
		// archiving the fresh one too.
		withClaims(t, h, func(ts db.TxStores) error {
			if _, err := ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID); err != nil {
				return err
			}
			id, err := ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			if err != nil {
				return err
			}
			if id != "" {
				t.Errorf("archive with no live conversation returned id %q, want empty", id)
			}
			return nil
		})
	})

	t.Run("Sweeps_ReleaseClaimsOnly", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "sweeps")
		convID, msgID := seedTurn(t, h, projectID, "engaged")
		claimTurnAs(t, h, convID, "exec-old", 1)
		withClaims(t, h, func(ts db.TxStores) error {
			_, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			return err
		})
		// A second QUEUED turn must survive every sweep untouched.
		_, queuedID := seedTurn(t, h, projectID, "still queued")

		// Ownership scoping: a different executor's sweep and a same-boot
		// sweep both leave the claim alone.
		if n, err := h.Stores.Curator.CancelStrandedTurnsForHomeSystem(ctx, "exec-other", 99, "restart"); err != nil || n != 0 {
			t.Fatalf("other-executor sweep released %d (err=%v), want 0", n, err)
		}
		if n, err := h.Stores.Curator.CancelStrandedTurnsForHomeSystem(ctx, "exec-old", 1, "restart"); err != nil || n != 0 {
			t.Fatalf("same-boot sweep released %d (err=%v), want 0", n, err)
		}
		n, err := h.Stores.Curator.CancelStrandedTurnsForHomeSystem(ctx, "exec-old", 2, "process restarted")
		if err != nil || n != 1 {
			t.Fatalf("own newer-boot sweep released %d (err=%v), want 1", n, err)
		}

		var claims []domain.Claim
		withClaims(t, h, func(ts db.TxStores) error {
			cs, err := ts.Curator.ListClaims(ctx, h.OrgID, convID)
			claims = cs
			return err
		})
		if len(claims) != 1 || claims[0].Outcome != "cancelled" || claims[0].Error != "process restarted" {
			t.Fatalf("swept claim = %+v, want released cancelled with the sweep's error", claims[0])
		}

		// The queued turn survived and is claimable again (global sweep also
		// leaves it alone — nothing active remains to release).
		if n, err := h.Stores.Curator.CancelOrphanedTurnsSystem(ctx); err != nil || n != 0 {
			t.Fatalf("global sweep released %d (err=%v), want 0 with no active claims", n, err)
		}
		var inFlight *db.CuratorInFlightTurn
		withClaims(t, h, func(ts db.TxStores) error {
			f, err := ts.Curator.InFlightTurn(ctx, h.OrgID, projectID, h.UserID)
			inFlight = f
			return err
		})
		if inFlight == nil || inFlight.Running || inFlight.MessageID != queuedID {
			t.Fatalf("queued turn after sweeps = %+v, want queued message %d surviving", inFlight, queuedID)
		}

		// The global sweep releases a fresh engagement.
		claimTurnAs(t, h, convID, "exec-old", 2)
		if n, err := h.Stores.Curator.CancelOrphanedTurnsSystem(ctx); err != nil || n != 1 {
			t.Fatalf("global sweep released %d (err=%v), want 1", n, err)
		}
	})

	// The one claim loop reaches curator conversations through the same
	// needs-driving predicate every other surface uses, with the home stamp
	// as its type-conditional gate — the curator-only claimable-turn scan is
	// gone.
	t.Run("ClaimLoop_ClaimsAHomedCuratorTurnOldestFirst", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "scan")
		convID, first := seedTurn(t, h, projectID, "first")
		_, second := seedTurn(t, h, projectID, "second")

		// Un-homed: claimable by anyone (the local/role=all shape — one
		// process, nothing to home to).
		got, err := h.Stores.RunQueue.ClaimNextRun(ctx, "home-1", 1, db.ClaimPlacement{})
		if err != nil || got == nil || got.ID != convID {
			t.Fatalf("un-homed claim = (%+v, %v), want conversation %s", got, err, convID)
		}
		// The claim is minted to the conversation's OLDEST queued turn, so a
		// backlog behind a running turn is driven one turn at a time.
		if got.ClaimMessageID != first {
			t.Fatalf("claim message id = %d, want the oldest queued turn %d (not %d)", got.ClaimMessageID, first, second)
		}
		if got.ProjectID != projectID || got.CreatorUserID != h.UserID {
			t.Errorf("claimed projection = %+v, want identity fields populated", got)
		}
		// An active claim hides the conversation entirely.
		claimTurnRefused(t, h, convID)
		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}

		// Homed elsewhere: invisible to this executor, claimable by the home.
		if err := h.Stores.CuratorHomes.Upsert(ctx, h.OrgID, projectID, "home-1", 1); err != nil {
			t.Fatalf("home upsert: %v", err)
		}
		if other, err := h.Stores.RunQueue.ClaimNextRun(ctx, "home-2", 1, db.ClaimPlacement{}); err != nil || other != nil {
			t.Fatalf("non-home claim = (%+v, %v), want nothing", other, err)
		}
		if mine, err := h.Stores.RunQueue.ClaimNextRun(ctx, "home-1", 1, db.ClaimPlacement{}); err != nil || mine == nil || mine.ID != convID {
			t.Fatalf("home claim = (%+v, %v), want conversation %s", mine, err, convID)
		}
	})

	t.Run("CredPubKey_StampsActiveClaimOnce", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "cred")
		convID, _ := seedTurn(t, h, projectID, "cred turn")

		// No active claim → nothing to stamp, nothing to provision.
		if matched, err := h.Stores.Curator.PublishTurnCredPubKeySystem(ctx, h.OrgID, convID, "pk1"); err != nil || matched {
			t.Fatalf("publish with no claim: matched=%v err=%v, want miss", matched, err)
		}
		if _, ok, err := h.Stores.Curator.GetTurnProvisionInfoSystem(ctx, h.OrgID, convID); err != nil || ok {
			t.Fatalf("provision info with no claim: ok=%v err=%v, want miss", ok, err)
		}

		claimTurnAs(t, h, convID, "home-1", 3)
		matched, err := h.Stores.Curator.PublishTurnCredPubKeySystem(ctx, h.OrgID, convID, "pk1")
		if err != nil || !matched {
			t.Fatalf("publish: matched=%v err=%v, want stamp", matched, err)
		}
		// Guarded: a duplicate publish never overwrites the recorded key.
		if matched, err := h.Stores.Curator.PublishTurnCredPubKeySystem(ctx, h.OrgID, convID, "pk2"); err != nil || matched {
			t.Fatalf("duplicate publish: matched=%v err=%v, want guard", matched, err)
		}

		info, ok, err := h.Stores.Curator.GetTurnProvisionInfoSystem(ctx, h.OrgID, convID)
		if err != nil || !ok {
			t.Fatalf("provision info: ok=%v err=%v", ok, err)
		}
		if info.CredPubKey != "pk1" || info.HomeInstanceID != "home-1" || info.ProjectID != projectID {
			t.Errorf("provision info = %+v, want (pk1, home-1, %s)", info, projectID)
		}

		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "completed", "", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, ok, err := h.Stores.Curator.GetTurnProvisionInfoSystem(ctx, h.OrgID, convID); err != nil || ok {
			t.Fatalf("provision info after release: ok=%v err=%v, want miss", ok, err)
		}
	})

	t.Run("SDKSession_RoundTrips", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "session")
		convID, msgID := seedTurn(t, h, projectID, "hello")
		withClaims(t, h, func(ts db.TxStores) error {
			return ts.Curator.SetSDKSession(ctx, h.OrgID, convID, "sess-123")
		})
		var live *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			live = c
			return err
		})
		if live == nil || live.SessionID != "sess-123" {
			t.Fatalf("conversation session = %+v, want sess-123", live)
		}
		claimTurn(t, h, convID)
		var start *db.CuratorTurnStart
		withClaims(t, h, func(ts db.TxStores) error {
			s, err := ts.Curator.BeginTurn(ctx, h.OrgID, projectID, convID, msgID)
			start = s
			return err
		})
		if start.SDKSessionID != "sess-123" {
			t.Errorf("BeginTurn session = %q, want sess-123", start.SDKSessionID)
		}
	})

	t.Run("ImportConversationState_RoundTrips", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "import")
		conv := domain.Conversation{
			ID:            uuid.New().String(),
			ProjectID:     projectID,
			CreatorUserID: h.UserID,
			SessionID:     "sess-imported",
		}
		released := time.Now().UTC().Truncate(time.Second)
		cost := 0.02
		tokens := 5
		claim := domain.Claim{
			ID:             uuid.New().String(),
			ConversationID: conv.ID,
			ExecutorID:     "src-exec",
			BootEpoch:      1,
			ClaimedAt:      released.Add(-time.Second),
			ReleasedAt:     &released,
			Outcome:        "completed",
		}
		delivered := true
		seq := 1.5
		msgs := []domain.Message{
			{ConversationID: conv.ID, UserID: h.UserID, Role: "user", Content: "imported turn", Delivered: &delivered},
			{ConversationID: conv.ID, UserID: h.UserID, ClaimID: claim.ID, Role: "assistant", Content: "imported ack",
				InputTokens: &tokens, CostUSD: &cost},
			// A compacted row: retired from the active window with a
			// fractional assembly override. Both fields must survive the
			// round-trip or the row reappears active after an import.
			{ConversationID: conv.ID, UserID: h.UserID, Role: "assistant", Content: "imported compaction",
				Delivered: &delivered, WindowState: domain.MessageWindowInactive, Seq: &seq},
		}
		if err := h.Stores.Curator.ImportConversationStateSystem(ctx, h.OrgID, conv, []domain.Claim{claim}, msgs); err != nil {
			t.Fatalf("import: %v", err)
		}

		var live *domain.Conversation
		withClaims(t, h, func(ts db.TxStores) error {
			c, err := ts.Curator.GetLiveConversation(ctx, h.OrgID, projectID, h.UserID)
			live = c
			return err
		})
		if live == nil || live.ID != conv.ID || live.SessionID != "sess-imported" {
			t.Fatalf("imported conversation = %+v, want %s with sess-imported", live, conv.ID)
		}
		var claims []domain.Claim
		var gotMsgs []domain.Message
		withClaims(t, h, func(ts db.TxStores) error {
			cs, err := ts.Curator.ListClaims(ctx, h.OrgID, conv.ID)
			if err != nil {
				return err
			}
			claims = cs
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, conv.ID, 0)
			gotMsgs = ms
			return err
		})
		if len(claims) != 1 || claims[0].Outcome != "completed" || claims[0].ExecutorID != "src-exec" {
			t.Fatalf("imported claims = %+v, want the source claim", claims)
		}
		// The seq override changes assembly order relative to the backend's
		// auto-assigned ids, so rows are located by content, not position.
		byContent := map[string]domain.Message{}
		for _, m := range gotMsgs {
			byContent[m.Content] = m
		}
		if len(gotMsgs) != 3 {
			t.Fatalf("imported messages = %+v, want the 3-row source transcript", gotMsgs)
		}
		if _, ok := byContent["imported turn"]; !ok {
			t.Fatalf("imported messages = %+v, want the source user turn", gotMsgs)
		}
		ack := byContent["imported ack"]
		if ack.ClaimID != claim.ID {
			t.Fatalf("imported assistant row claim_id = %q, want claim attribution %q", ack.ClaimID, claim.ID)
		}
		// float4 storage: compare with an epsilon, and dereference for the
		// error message.
		if c := ack.CostUSD; c == nil || *c < cost-1e-4 || *c > cost+1e-4 {
			t.Errorf("imported assistant row cost_usd = %v, want ~%v (the ledger travels with the bundle)", c, cost)
		}
		if it := ack.InputTokens; it == nil || *it != tokens {
			t.Errorf("imported assistant row input_tokens = %v, want %d", it, tokens)
		}
		compacted := byContent["imported compaction"]
		if compacted.WindowState != domain.MessageWindowInactive {
			t.Errorf("imported compaction row window_state = %q, want %q (must not reappear active)",
				compacted.WindowState, domain.MessageWindowInactive)
		}
		if compacted.Seq == nil || *compacted.Seq != seq {
			t.Errorf("imported compaction row seq = %v, want %v (assembly override travels with the bundle)",
				compacted.Seq, seq)
		}
	})
}
