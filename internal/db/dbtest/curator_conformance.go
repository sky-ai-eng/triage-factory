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

		claimID, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-1", 7)
		if err != nil || !ok || claimID == "" {
			t.Fatalf("ClaimTurnSystem: id=%q ok=%v err=%v", claimID, ok, err)
		}
		// Single-active: a duplicate claim conflicts and reports skip.
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-1", 7); err != nil || ok {
			t.Fatalf("duplicate ClaimTurnSystem: ok=%v err=%v, want conflict skip", ok, err)
		}

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
		err = h.Stores.Tx.SyntheticClaimsWithTx(ctx, h.OrgID, h.UserID, func(ts db.TxStores) error {
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
			_, err := ts.AgentRuns.InsertMessage(ctx, h.OrgID, &domain.AgentMessage{
				RunID: convID, UserID: h.UserID, ClaimID: claimID,
				Role: "assistant", Subtype: "text", Content: "ack",
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
		if cl.CostUSD == nil || *cl.CostUSD < 0.049 || *cl.CostUSD > 0.051 {
			t.Errorf("claim cost = %v, want ~0.05", cl.CostUSD)
		}
		if cl.InputTokens != 11 || cl.OutputTokens != 22 || cl.CacheReadTokens != 33 || cl.CacheCreationTokens != 44 {
			t.Errorf("claim tokens = (%d,%d,%d,%d), want the per-claim messages SUM (11,22,33,44)",
				cl.InputTokens, cl.OutputTokens, cl.CacheReadTokens, cl.CacheCreationTokens)
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
		var msgs []domain.AgentMessage
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID)
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

		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-1", 1); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
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
			id, err := ts.AgentRuns.InsertMessage(ctx, h.OrgID, &domain.AgentMessage{
				RunID: convID, UserID: h.UserID,
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
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-1", 1); err != nil || !ok {
			t.Fatalf("re-claim: ok=%v err=%v", ok, err)
		}
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
		var msgs []domain.AgentMessage
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID)
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
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-1", 1); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		err := h.Stores.Tx.SyntheticClaimsWithTx(ctx, h.OrgID, h.UserID, func(ts db.TxStores) error {
			return ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
		})
		if !errors.Is(err, db.ErrCuratorInFlight) {
			t.Fatalf("archive with live claim err = %v, want ErrCuratorInFlight", err)
		}

		if _, err := h.Stores.Curator.ReleaseActiveTurnSystem(ctx, h.OrgID, convID, "cancelled", "user cancelled", 0, 0, 0); err != nil {
			t.Fatalf("release: %v", err)
		}
		withClaims(t, h, func(ts db.TxStores) error {
			return ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
		})

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
		var msgs []domain.AgentMessage
		withClaims(t, h, func(ts db.TxStores) error {
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, convID)
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
			if err := ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID); err != nil {
				return err
			}
			return ts.Curator.ArchiveLiveConversation(ctx, h.OrgID, projectID, h.UserID)
		})
	})

	t.Run("Sweeps_ReleaseClaimsOnly", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "sweeps")
		convID, msgID := seedTurn(t, h, projectID, "engaged")
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-old", 1); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
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
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "exec-old", 2); err != nil || !ok {
			t.Fatalf("re-claim: ok=%v err=%v", ok, err)
		}
		if n, err := h.Stores.Curator.CancelOrphanedTurnsSystem(ctx); err != nil || n != 1 {
			t.Fatalf("global sweep released %d (err=%v), want 1", n, err)
		}
	})

	t.Run("ClaimLoopScan_OnePerConversationHomedOnly", func(t *testing.T) {
		h := mk(t)
		projectID := h.SeedProject(t, "scan")
		convID, first := seedTurn(t, h, projectID, "first")
		_, _ = seedTurn(t, h, projectID, "second")

		// Un-homed: invisible to every executor's scan.
		if turns, err := h.Stores.Curator.ListClaimableTurnsForHomeSystem(ctx, "home-1"); err != nil || len(turns) != 0 {
			t.Fatalf("un-homed scan = %v (err=%v), want empty", turns, err)
		}
		if err := h.Stores.CuratorHomes.Upsert(ctx, h.OrgID, projectID, "home-1", 1); err != nil {
			t.Fatalf("home upsert: %v", err)
		}
		turns, err := h.Stores.Curator.ListClaimableTurnsForHomeSystem(ctx, "home-1")
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(turns) != 1 || turns[0].MessageID != first || turns[0].ConversationID != convID {
			t.Fatalf("scan = %+v, want exactly the conversation's OLDEST queued turn %d", turns, first)
		}
		if turns[0].OrgID != h.OrgID || turns[0].ProjectID != projectID || turns[0].CreatorUserID != h.UserID {
			t.Errorf("scan projection = %+v, want identity fields populated", turns[0])
		}
		// Another executor sees nothing.
		if other, err := h.Stores.Curator.ListClaimableTurnsForHomeSystem(ctx, "home-2"); err != nil || len(other) != 0 {
			t.Fatalf("other-home scan = %v (err=%v), want empty", other, err)
		}
		// An active claim hides the conversation entirely.
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "home-1", 1); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if turns, err := h.Stores.Curator.ListClaimableTurnsForHomeSystem(ctx, "home-1"); err != nil || len(turns) != 0 {
			t.Fatalf("scan with active claim = %v (err=%v), want empty", turns, err)
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

		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "home-1", 3); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
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
		if _, ok, err := h.Stores.Curator.ClaimTurnSystem(ctx, h.OrgID, convID, "e", 1); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
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
		claim := domain.Claim{
			ID:             uuid.New().String(),
			ConversationID: conv.ID,
			ExecutorID:     "src-exec",
			BootEpoch:      1,
			ClaimedAt:      released.Add(-time.Second),
			ReleasedAt:     &released,
			Outcome:        "completed",
			CostUSD:        &cost,
			InputTokens:    5,
		}
		delivered := true
		msgs := []domain.AgentMessage{
			{RunID: conv.ID, UserID: h.UserID, Role: "user", Subtype: "text", Content: "imported turn", Delivered: &delivered},
			{RunID: conv.ID, UserID: h.UserID, ClaimID: claim.ID, Role: "assistant", Subtype: "text", Content: "imported ack"},
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
		var gotMsgs []domain.AgentMessage
		withClaims(t, h, func(ts db.TxStores) error {
			cs, err := ts.Curator.ListClaims(ctx, h.OrgID, conv.ID)
			if err != nil {
				return err
			}
			claims = cs
			ms, err := ts.Curator.ListConversationMessages(ctx, h.OrgID, conv.ID)
			gotMsgs = ms
			return err
		})
		if len(claims) != 1 || claims[0].Outcome != "completed" || claims[0].ExecutorID != "src-exec" {
			t.Fatalf("imported claims = %+v, want the source claim", claims)
		}
		if len(gotMsgs) != 2 || gotMsgs[0].Content != "imported turn" || gotMsgs[1].ClaimID != claim.ID {
			t.Fatalf("imported messages = %+v, want the source transcript with claim attribution", gotMsgs)
		}
	})
}
