package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PendingFiringsStoreFactory is what a per-backend test file hands to
// RunPendingFiringsStoreConformance. Returns the wired PendingFiringsStore,
// the orgID to pass to every call, and a PendingFiringsSeeder for fixtures
// the store can't create itself (entity → task → event_handler → event
// chains, live conversations for the gate, lease surgery).
type PendingFiringsStoreFactory func(t *testing.T) (
	store db.PendingFiringsStore,
	orgID string,
	seed PendingFiringsSeeder,
)

// PendingFiringsTuple is the identifier set Enqueue needs, plus the prompt
// the chain's trigger fires so a live conversation can be seeded against
// the task.
type PendingFiringsTuple struct {
	EntityID  string
	TaskID    string
	TriggerID string
	EventID   string
	PromptID  string
}

// PendingFiringsSeeder bags raw-SQL helpers backend tests provide.
type PendingFiringsSeeder struct {
	// Tuple inserts a fresh entity/task/trigger/event chain and returns the
	// ids Enqueue needs.
	Tuple func(t *testing.T) PendingFiringsTuple

	// AgentID is an agents row in the harness's org, usable as the
	// AgentClaimStamp agent for the claim-coupling subtests
	// (tasks.claimed_by_agent_id FKs agents(id) in both dialects).
	AgentID string

	// TaskClaim reads a task's two claim columns so the harness can assert
	// what the coupled stamp did without knowing either backend's schema.
	// Empty strings for NULL.
	TaskClaim func(t *testing.T, taskID string) (agentID, userID string)

	// ClaimTaskForUser stamps a user claim on the task, so the harness can
	// exercise the stamp's no-steal refusal against a real competing claim.
	ClaimTaskForUser func(t *testing.T, taskID string)

	// RunForTask inserts a blueprint_run against the task, marked running,
	// and returns its id. It satisfies MarkFired's fired_run_id foreign key,
	// and with no conversation beside it, it is the run half of the claim
	// filter's gate on its own.
	RunForTask func(t *testing.T, taskID string) string

	// SettleRuns marks every running blueprint_run on the task completed,
	// which is what reopens the run half of the gate.
	SettleRuns func(t *testing.T, taskID string)

	// LiveConversation inserts a live top-level conversation on the task —
	// ended_at NULL, no parent, no terminal status — and returns its id. No
	// running blueprint_run stands behind it, so it is the conversation half
	// of the claim filter's gate on its own.
	LiveConversation func(t *testing.T, taskID, promptID string) string

	// EndConversation ends a conversation: a terminal status and an ended_at
	// stamp, which is what reopens the gate.
	EndConversation func(t *testing.T, conversationID string)

	// ExpireLease rewinds a leased row's lease_expires_at into the past,
	// standing in for a holder that died without a terminal write.
	ExpireLease func(t *testing.T, firingID int64)

	// Ripen clears a ready row's next_attempt_at, so a requeued row is
	// claimable again without waiting out the kind's backoff.
	Ripen func(t *testing.T, firingID int64)

	// InTx runs fn against a store bound to one transaction, and returns
	// what fn returned after committing or rolling back on it.
	InTx func(t *testing.T, fn func(s db.PendingFiringsStore) error) error
}

// firingOwner is the owner every claim in this suite stamps a row with.
var firingOwner = workitem.Owner{ID: "conformance-firing-worker", Epoch: 1}

// RunPendingFiringsStoreConformance covers the pending-firings contract every
// backend impl must hold, on the shared work-item contract as
// workkinds.PendingFirings declares it: admission under the (task, trigger)
// key with the claim stamp riding the insert, the claim with its per-task
// gate, the holder verbs with their fence, the gate read, and the
// transaction-bound store's two faces.
func RunPendingFiringsStoreConformance(t *testing.T, mk PendingFiringsStoreFactory) {
	t.Helper()
	ctx := context.Background()

	enqueue := func(t *testing.T, s db.PendingFiringsStore, orgID string, tup PendingFiringsTuple, taskID string, claim db.AgentClaimStamp) (bool, bool) {
		t.Helper()
		inserted, claimed, err := s.Enqueue(ctx, orgID, tup.EntityID, taskID, tup.TriggerID, tup.EventID, claim)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return inserted, claimed
	}
	list := func(t *testing.T, s db.PendingFiringsStore, orgID, entityID string) []domain.PendingFiring {
		t.Helper()
		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		return rows
	}
	one := func(t *testing.T, s db.PendingFiringsStore, orgID, entityID string) domain.PendingFiring {
		t.Helper()
		rows := list(t, s, orgID, entityID)
		if len(rows) != 1 {
			t.Fatalf("expected 1 firing row for entity %s, got %d", entityID, len(rows))
		}
		return rows[0]
	}
	claimOne := func(t *testing.T, s db.PendingFiringsStore) db.ClaimedFiring {
		t.Helper()
		batch, err := s.Claim(ctx, firingOwner, 1)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Firings) != 1 {
			t.Fatalf("Claim returned %d firings, want 1 (cancelled=%d parked=%d)", len(batch.Firings), batch.Cancelled, batch.Parked)
		}
		return batch.Firings[0]
	}
	handle := func(t *testing.T, s db.PendingFiringsStore) db.WorkKindHandle {
		t.Helper()
		h, ok := s.(db.WorkKindHandle)
		if !ok {
			t.Fatalf("%T does not implement db.WorkKindHandle", s)
		}
		return h
	}

	t.Run("Enqueue_admits_a_ready_row_under_the_key", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		inserted, _ := enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		if !inserted {
			t.Errorf("first Enqueue should report inserted=true")
		}
		f := one(t, s, orgID, tup.EntityID)
		if f.Status != workitem.StatusReady {
			t.Errorf("status = %q, want ready", f.Status)
		}
		if f.OrgID != orgID || f.EntityID != tup.EntityID || f.TaskID != tup.TaskID || f.TriggerID != tup.TriggerID || f.TriggeringEventID != tup.EventID {
			t.Errorf("row identity mismatch: %+v", f)
		}
		if f.UniqueKey != workkinds.PendingFiringKey(tup.TaskID, tup.TriggerID) {
			t.Errorf("unique_key = %q, want %q", f.UniqueKey, workkinds.PendingFiringKey(tup.TaskID, tup.TriggerID))
		}
		if f.Attempt != 0 || f.MaxAttempts != 5 || f.LeaseGeneration != 0 || f.NextAttemptAt != nil {
			t.Errorf("block defaults = attempt %d / max %d / generation %d / next %v", f.Attempt, f.MaxAttempts, f.LeaseGeneration, f.NextAttemptAt)
		}
		if f.FirstEnqueuedAt.IsZero() || f.CreatedAt.IsZero() || f.DoneAt != nil || f.LeasedAt != nil {
			t.Errorf("timestamps = first %v / created %v / done %v / leased %v", f.FirstEnqueuedAt, f.CreatedAt, f.DoneAt, f.LeasedAt)
		}
		if f.SkipReason != "" || f.FiredBlueprintRunID != nil {
			t.Errorf("terminal columns set at admission: %q / %v", f.SkipReason, f.FiredBlueprintRunID)
		}
	})

	// One firing per (task, trigger) while one is unsettled: a duplicate
	// collapses against a ready, leased or parked row, and admits again
	// once the row is done or cancelled.
	t.Run("Enqueue_collapses_while_unsettled_and_admits_after_settlement", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		dup := func(want bool, when string) {
			t.Helper()
			inserted, _ := enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
			if inserted != want {
				t.Errorf("Enqueue while %s: inserted=%v, want %v", when, inserted, want)
			}
		}
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		dup(false, "ready")

		cf := claimOne(t, s)
		dup(false, "leased")

		parked, err := s.Requeue(ctx, cf.Receipt, workitem.OutcomePermanent, errors.New("rejected"))
		if err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}
		dup(false, "parked")
		if rows := list(t, s, orgID, tup.EntityID); len(rows) != 1 {
			t.Fatalf("dedup should keep one row across ready/leased/parked, got %d", len(rows))
		}

		// Redrive the parked row so it can be settled done.
		h := handle(t, s)
		if err := workitem.Redrive(ctx, h.Conn(), h.Kind(), orgID, cf.Firing.ID, "operator"); err != nil {
			t.Fatalf("Redrive: %v", err)
		}
		cf = claimOne(t, s)
		if err := s.MarkSkipped(ctx, cf.Receipt, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		dup(true, "done")

		// The new row, cancelled, admits again too.
		rows := list(t, s, orgID, tup.EntityID)
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows after re-admission, got %d", len(rows))
		}
		if err := workitem.RequestCancel(ctx, h.Conn(), h.Kind(), orgID, rows[1].ID, "operator", "stop"); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		batch, err := s.Claim(ctx, firingOwner, 5)
		if err != nil || batch.Cancelled != 1 || len(batch.Firings) != 0 {
			t.Fatalf("claim of a cancelled row = %+v err=%v, want one settled", batch, err)
		}
		dup(true, "cancelled")
		if rows := list(t, s, orgID, tup.EntityID); len(rows) != 3 {
			t.Errorf("expected 3 rows after the second re-admission, got %d", len(rows))
		}
	})

	// The claim rides the insert's transaction — a queued firing is a real
	// commitment, so the board must never show the task free while the
	// firing waits. These three subtests pin the whole contract: the stamp
	// lands with the row, it is skipped when nothing was committed, and a
	// refusal never costs the commitment.
	t.Run("Enqueue_stamps_the_claim_with_the_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		inserted, claimed := enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{AgentID: seed.AgentID})
		if !inserted || !claimed {
			t.Fatalf("Enqueue with claim = (inserted=%v, claimed=%v), want (true, true)", inserted, claimed)
		}
		if agentID, _ := seed.TaskClaim(t, tup.TaskID); agentID != seed.AgentID {
			t.Errorf("task claimed_by_agent_id = %q, want %q — the firing landed without its claim", agentID, seed.AgentID)
		}
	})

	t.Run("Enqueue_collapse_leaves_the_claim_alone", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{AgentID: seed.AgentID})
		// A user takes the task over between the two enqueues. The duplicate
		// commits nothing (the queued firing already carries the intent), so
		// it must not re-stamp over them.
		seed.ClaimTaskForUser(t, tup.TaskID)
		inserted, claimed := enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{AgentID: seed.AgentID})
		if inserted || claimed {
			t.Errorf("collapsed Enqueue = (inserted=%v, claimed=%v), want (false, false)", inserted, claimed)
		}
		if agentID, userID := seed.TaskClaim(t, tup.TaskID); agentID != "" || userID == "" {
			t.Errorf("claim after collapse = (agent=%q, user=%q), want the user's claim intact", agentID, userID)
		}
	})

	t.Run("Enqueue_commits_even_when_the_stamp_is_refused", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		// A user owns the task, so the stamp must refuse rather than steal —
		// and the firing must still be queued, because refusing a claim race
		// is not a reason to lose the intent.
		seed.ClaimTaskForUser(t, tup.TaskID)
		inserted, claimed := enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{AgentID: seed.AgentID})
		if !inserted {
			t.Error("a refused stamp must not roll back the firing insert")
		}
		if claimed {
			t.Error("claimed=true on a user-claimed task — the stamp stole the claim")
		}
		if agentID, userID := seed.TaskClaim(t, tup.TaskID); agentID != "" || userID == "" {
			t.Errorf("claim = (agent=%q, user=%q), want the user's claim untouched", agentID, userID)
		}
		one(t, s, orgID, tup.EntityID)
	})

	t.Run("Claim_returns_rows_FIFO_with_receipts_and_typed_columns", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t)
		tup2 := seed.Tuple(t)
		// Two firings on ONE task, distinct triggers: the key is (task,
		// trigger), so two triggers on one task are two rows.
		enqueue(t, s, orgID, tup1, tup1.TaskID, db.AgentClaimStamp{})
		enqueue(t, s, orgID, tup2, tup1.TaskID, db.AgentClaimStamp{})

		batch, err := s.Claim(ctx, firingOwner, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Firings) != 2 || batch.Cancelled != 0 || batch.Parked != 0 || batch.Reclaimed != 0 {
			t.Fatalf("Claim = %+v, want two leased firings and nothing settled", batch)
		}
		first, second := batch.Firings[0], batch.Firings[1]
		if first.Firing.TriggerID != tup1.TriggerID || second.Firing.TriggerID != tup2.TriggerID {
			t.Errorf("claim order = %q, %q; want FIFO %q, %q", first.Firing.TriggerID, second.Firing.TriggerID, tup1.TriggerID, tup2.TriggerID)
		}
		for _, cf := range batch.Firings {
			if cf.Receipt.ItemID != cf.Firing.ID || cf.Receipt.OrgID != orgID || cf.Receipt.LeaseGeneration != 1 || cf.Receipt.Attempt != 1 {
				t.Errorf("receipt %+v does not match its firing %+v", cf.Receipt, cf.Firing)
			}
			if cf.Firing.Status != workitem.StatusLeased || cf.Firing.LeaseOwner != firingOwner.ID || cf.Firing.LeaseEpoch == nil || *cf.Firing.LeaseEpoch != firingOwner.Epoch {
				t.Errorf("claimed row = %+v, want leased by %s", cf.Firing, firingOwner.ID)
			}
			if cf.Firing.LeasedAt == nil || cf.Firing.LeaseExpiresAt == nil || !cf.Firing.LeaseExpiresAt.After(*cf.Firing.LeasedAt) {
				t.Errorf("lease timestamps = %v / %v", cf.Firing.LeasedAt, cf.Firing.LeaseExpiresAt)
			}
			if cf.Firing.TaskID != tup1.TaskID {
				t.Errorf("claimed row task = %q, want %q", cf.Firing.TaskID, tup1.TaskID)
			}
		}
		// Nothing left to claim.
		if again, err := s.Claim(ctx, firingOwner, 10); err != nil || len(again.Firings) != 0 {
			t.Errorf("second claim = %+v err=%v, want empty", again, err)
		}
	})

	t.Run("Claim_counts_settled_rows_and_reclaims_an_expired_lease", func(t *testing.T) {
		s, orgID, seed := mk(t)
		h := handle(t, s)
		tupA := seed.Tuple(t)
		tupB := seed.Tuple(t)
		enqueue(t, s, orgID, tupA, tupA.TaskID, db.AgentClaimStamp{})
		enqueue(t, s, orgID, tupB, tupB.TaskID, db.AgentClaimStamp{})
		a := one(t, s, orgID, tupA.EntityID)
		if err := workitem.RequestCancel(ctx, h.Conn(), h.Kind(), orgID, a.ID, "operator", "stop"); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		batch, err := s.Claim(ctx, firingOwner, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if batch.Cancelled != 1 || len(batch.Firings) != 1 || batch.Firings[0].Firing.TaskID != tupB.TaskID {
			t.Fatalf("Claim = %+v, want the cancelled row settled and B leased", batch)
		}
		if got := one(t, s, orgID, tupA.EntityID); got.Status != workitem.StatusCancelled || got.CancelRequestedBy != "operator" {
			t.Errorf("cancelled row = %+v", got)
		}

		// B's holder dies: its lease expires, and the next claim takes the
		// row as a reclaim that names the previous holder.
		seed.ExpireLease(t, batch.Firings[0].Firing.ID)
		again, err := s.Claim(ctx, workitem.Owner{ID: "successor", Epoch: 2}, 10)
		if err != nil {
			t.Fatalf("Claim after expiry: %v", err)
		}
		if len(again.Firings) != 1 || again.Reclaimed != 1 {
			t.Fatalf("Claim after expiry = %+v, want one reclaim", again)
		}
		r := again.Firings[0].Receipt
		if !r.Reclaimed || r.PreviousOwner != firingOwner.ID || r.LeaseGeneration != 2 || r.Attempt != 2 {
			t.Errorf("reclaim receipt = %+v, want reclaimed from %s at generation 2, attempt 2", r, firingOwner.ID)
		}
		// The dead holder's receipt is refused everywhere.
		if err := s.MarkSkipped(ctx, batch.Firings[0].Receipt, domain.PendingFiringSkipTaskClosed); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale MarkSkipped = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("Claim_skips_a_task_with_a_live_conversation_until_it_ends", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tupBusy := seed.Tuple(t)
		tupFree := seed.Tuple(t)
		enqueue(t, s, orgID, tupBusy, tupBusy.TaskID, db.AgentClaimStamp{})
		enqueue(t, s, orgID, tupFree, tupFree.TaskID, db.AgentClaimStamp{})
		conv := seed.LiveConversation(t, tupBusy.TaskID, tupBusy.PromptID)

		batch, err := s.Claim(ctx, firingOwner, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Firings) != 1 || batch.Firings[0].Firing.TaskID != tupFree.TaskID {
			t.Fatalf("Claim with a busy task = %+v, want only the free task's row", batch)
		}
		// The busy row is deferred, not ready, and holds no attempt charge.
		busy := one(t, s, orgID, tupBusy.EntityID)
		if busy.Status != workitem.StatusReady || busy.Attempt != 0 {
			t.Errorf("busy row = %+v, want ready and uncharged", busy)
		}
		h := handle(t, s)
		d, err := workitem.Measure(ctx, h.Conn(), h.Kind(), orgID)
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		if d.Ready != 0 || d.Deferred != 1 || d.Leased != 1 {
			t.Errorf("depths = %+v, want the busy row deferred and the free one leased", d)
		}

		seed.EndConversation(t, conv)
		again, err := s.Claim(ctx, firingOwner, 10)
		if err != nil {
			t.Fatalf("Claim after the conversation ended: %v", err)
		}
		if len(again.Firings) != 1 || again.Firings[0].Firing.TaskID != tupBusy.TaskID || again.Reclaimed != 0 {
			t.Fatalf("Claim after the conversation ended = %+v, want the busy task's row, freshly", again)
		}
	})

	t.Run("Claim_skips_a_task_with_a_running_run_until_it_settles", func(t *testing.T) {
		// The run's last conversation is already terminal and the run is
		// not yet: no conversation is live, and the fenced insert would
		// still refuse. The filter holds the row rather than claim it into
		// that refusal.
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		seed.RunForTask(t, tup.TaskID)

		batch, err := s.Claim(ctx, firingOwner, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Firings) != 0 {
			t.Fatalf("Claim behind a running run = %+v, want nothing", batch)
		}
		held := one(t, s, orgID, tup.EntityID)
		if held.Status != workitem.StatusReady || held.Attempt != 0 {
			t.Errorf("held row = %+v, want ready and uncharged", held)
		}
		h := handle(t, s)
		d, err := workitem.Measure(ctx, h.Conn(), h.Kind(), orgID)
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		if d.Ready != 0 || d.Deferred != 1 {
			t.Errorf("depths = %+v, want the held row deferred", d)
		}

		seed.SettleRuns(t, tup.TaskID)
		if cf := claimOne(t, s); cf.Firing.TaskID != tup.TaskID || cf.Receipt.Attempt != 1 {
			t.Errorf("claim after the run settled = %+v, want the held row on its first attempt", cf)
		}
	})

	t.Run("RenewLease_observes_a_cancellation_request_and_settles_it", func(t *testing.T) {
		s, orgID, seed := mk(t)
		h := handle(t, s)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		renewed, err := s.RenewLease(ctx, cf.Receipt)
		if err != nil {
			t.Fatalf("RenewLease: %v", err)
		}
		if !renewed.LeaseExpiresAt.After(cf.Receipt.LeaseExpiresAt) && !renewed.LeaseExpiresAt.Equal(cf.Receipt.LeaseExpiresAt) {
			t.Errorf("renewal moved the expiry backwards: %v -> %v", cf.Receipt.LeaseExpiresAt, renewed.LeaseExpiresAt)
		}
		if err := workitem.RequestCancel(ctx, h.Conn(), h.Kind(), orgID, cf.Firing.ID, "operator", "stop"); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if _, err := s.RenewLease(ctx, renewed); !errors.Is(err, workitem.ErrCancelled) {
			t.Fatalf("RenewLease with a pending request = %v, want ErrCancelled", err)
		}
		if got := one(t, s, orgID, tup.EntityID); got.Status != workitem.StatusCancelled || got.LeaseOwner != "" {
			t.Errorf("row after settlement = %+v, want cancelled with the lease released", got)
		}
	})

	t.Run("MarkFired_records_the_run_and_flips_done", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		runID := seed.RunForTask(t, tup.TaskID)
		if err := s.MarkFired(ctx, cf.Receipt, runID); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
		got := one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusDone || got.DoneAt == nil || got.LastOutcome != "done" {
			t.Errorf("row after MarkFired = %+v, want done", got)
		}
		if got.FiredBlueprintRunID == nil || *got.FiredBlueprintRunID != runID {
			t.Errorf("fired_run_id = %v, want %q", got.FiredBlueprintRunID, runID)
		}
		if got.SkipReason != "" || got.LeaseOwner != "" || got.LeaseExpiresAt != nil {
			t.Errorf("row after MarkFired = %+v, want no skip reason and the lease released", got)
		}
	})

	t.Run("MarkSkipped_records_the_reason_and_flips_done", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		if err := s.MarkSkipped(ctx, cf.Receipt, domain.PendingFiringSkipBreakerTripped); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		got := one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusDone || got.DoneAt == nil || got.SkipReason != domain.PendingFiringSkipBreakerTripped {
			t.Errorf("row after MarkSkipped = %+v, want done with the reason", got)
		}
		if got.FiredBlueprintRunID != nil {
			t.Errorf("fired_run_id = %v on a skipped row", got.FiredBlueprintRunID)
		}
	})

	t.Run("Terminal_writes_refuse_a_stale_receipt_and_write_nothing", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		stale := claimOne(t, s)
		// The row is taken over: the successor's generation is what the row
		// carries now.
		seed.ExpireLease(t, stale.Firing.ID)
		successor := claimOne(t, s)
		before := list(t, s, orgID, tup.EntityID)
		runID := seed.RunForTask(t, tup.TaskID)

		if err := s.MarkFired(ctx, stale.Receipt, runID); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale MarkFired = %v, want ErrLeaseLost", err)
		}
		if err := s.MarkSkipped(ctx, stale.Receipt, domain.PendingFiringSkipTaskClosed); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale MarkSkipped = %v, want ErrLeaseLost", err)
		}
		if _, err := s.Requeue(ctx, stale.Receipt, workitem.OutcomeTransient, errors.New("x")); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale Requeue = %v, want ErrLeaseLost", err)
		}
		if err := s.DeferWhileTaskBusy(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale DeferWhileTaskBusy = %v, want ErrLeaseLost", err)
		}
		if _, err := s.RenewLease(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("stale RenewLease = %v, want ErrLeaseLost", err)
		}
		after := list(t, s, orgID, tup.EntityID)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("a stale receipt changed the row:\n before %+v\n after  %+v", before, after)
		}
		// The successor's receipt still works.
		if err := s.MarkFired(ctx, successor.Receipt, runID); err != nil {
			t.Errorf("successor MarkFired: %v", err)
		}
	})

	t.Run("Requeue_outcomes_land_where_the_package_says", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		cause := errors.New("spawner said no")
		parked, err := s.Requeue(ctx, cf.Receipt, workitem.OutcomeTransient, cause)
		if err != nil || parked {
			t.Fatalf("Requeue transient: parked=%v err=%v", parked, err)
		}
		got := one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusReady || got.Attempt != 1 || got.NextAttemptAt == nil || got.LastOutcome != "transient" || got.LastError != cause.Error() {
			t.Errorf("row after a transient requeue = %+v", got)
		}
		// Not ripe until its retry time; ripened, it is claimed again and a
		// permanent outcome parks it whatever the budget.
		if batch, err := s.Claim(ctx, firingOwner, 10); err != nil || len(batch.Firings) != 0 {
			t.Fatalf("claim before the retry time = %+v err=%v, want nothing", batch, err)
		}
		seed.Ripen(t, got.ID)
		cf = claimOne(t, s)
		parked, err = s.Requeue(ctx, cf.Receipt, workitem.OutcomePermanent, errors.New("rejected"))
		if err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}
		got = one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusParked || got.DoneAt == nil || got.LastOutcome != "permanent" || got.Attempt != 2 {
			t.Errorf("row after a permanent requeue = %+v", got)
		}
	})

	t.Run("DeferWhileTaskBusy_refunds_the_attempt_only_while_the_task_is_busy", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		// Nothing live: the deferral is refused and the row stays leased.
		if err := s.DeferWhileTaskBusy(ctx, cf.Receipt); !errors.Is(err, workitem.ErrDeferRefused) {
			t.Fatalf("DeferWhileTaskBusy with no live conversation = %v, want ErrDeferRefused", err)
		}
		if got := one(t, s, orgID, tup.EntityID); got.Status != workitem.StatusLeased || got.Attempt != 1 {
			t.Errorf("row after a refused deferral = %+v, want still leased and charged", got)
		}
		// A conversation goes live under the lease: the deferral refunds the
		// attempt, and the claim filter is what holds the row afterwards.
		conv := seed.LiveConversation(t, tup.TaskID, tup.PromptID)
		if err := s.DeferWhileTaskBusy(ctx, cf.Receipt); err != nil {
			t.Fatalf("DeferWhileTaskBusy: %v", err)
		}
		got := one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusReady || got.Attempt != 0 || got.LastOutcome != "deferred" || got.LastError != workkinds.PendingFiringDeferTaskBusy {
			t.Errorf("row after the deferral = %+v, want ready, refunded, deferred as task_busy", got)
		}
		if batch, err := s.Claim(ctx, firingOwner, 10); err != nil || len(batch.Firings) != 0 {
			t.Fatalf("claim with the task busy = %+v err=%v, want nothing", batch, err)
		}
		seed.EndConversation(t, conv)
		if cf := claimOne(t, s); cf.Receipt.Attempt != 1 {
			t.Errorf("claim after the deferral charged attempt %d, want 1", cf.Receipt.Attempt)
		}
	})

	t.Run("DeferWhileTaskBusy_refunds_behind_a_running_run_with_no_live_conversation", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		cf := claimOne(t, s)
		seed.RunForTask(t, tup.TaskID)
		if err := s.DeferWhileTaskBusy(ctx, cf.Receipt); err != nil {
			t.Fatalf("DeferWhileTaskBusy behind a running run: %v", err)
		}
		got := one(t, s, orgID, tup.EntityID)
		if got.Status != workitem.StatusReady || got.Attempt != 0 || got.LastError != workkinds.PendingFiringDeferTaskBusy {
			t.Errorf("row after the deferral = %+v, want ready, refunded, deferred as task_busy", got)
		}
		// The row the deferral released is one the filter holds: the two
		// read the same condition.
		if batch, err := s.Claim(ctx, firingOwner, 10); err != nil || len(batch.Firings) != 0 {
			t.Fatalf("claim behind the running run = %+v err=%v, want nothing", batch, err)
		}
		seed.SettleRuns(t, tup.TaskID)
		if cf := claimOne(t, s); cf.Receipt.Attempt != 1 {
			t.Errorf("claim after the run settled charged attempt %d, want 1", cf.Receipt.Attempt)
		}
	})

	t.Run("HasUnsettledForTask_by_status", func(t *testing.T) {
		s, orgID, seed := mk(t)
		h := handle(t, s)
		tup := seed.Tuple(t)
		has := func(want bool, when string) {
			t.Helper()
			got, err := s.HasUnsettledForTask(ctx, orgID, tup.TaskID)
			if err != nil {
				t.Fatalf("HasUnsettledForTask: %v", err)
			}
			if got != want {
				t.Errorf("HasUnsettledForTask while %s = %v, want %v", when, got, want)
			}
		}
		has(false, "empty")
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		has(true, "ready")
		cf := claimOne(t, s)
		has(true, "leased")
		if parked, err := s.Requeue(ctx, cf.Receipt, workitem.OutcomePermanent, errors.New("x")); err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}
		has(true, "parked")
		if err := workitem.Redrive(ctx, h.Conn(), h.Kind(), orgID, cf.Firing.ID, "operator"); err != nil {
			t.Fatalf("Redrive: %v", err)
		}
		cf = claimOne(t, s)
		if err := s.MarkSkipped(ctx, cf.Receipt, domain.PendingFiringSkipTriggerDisabled); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		has(false, "done")
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		rows := list(t, s, orgID, tup.EntityID)
		if err := workitem.RequestCancel(ctx, h.Conn(), h.Kind(), orgID, rows[1].ID, "operator", "stop"); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if _, err := s.Claim(ctx, firingOwner, 10); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		has(false, "cancelled")
	})

	t.Run("ListForEntity_orders_by_id_and_stays_within_the_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t)
		tup2 := seed.Tuple(t)
		enqueue(t, s, orgID, tup1, tup1.TaskID, db.AgentClaimStamp{})
		enqueue(t, s, orgID, tup2, tup2.TaskID, db.AgentClaimStamp{})
		// A second task's firing on the first entity, so the list has two
		// rows in known order and the other entity keeps its own.
		inserted, _, err := s.Enqueue(ctx, orgID, tup1.EntityID, tup2.TaskID, tup1.TriggerID, tup1.EventID, db.AgentClaimStamp{})
		if err != nil || !inserted {
			t.Fatalf("Enqueue on entity 1 for task 2: inserted=%v err=%v", inserted, err)
		}
		rows := list(t, s, orgID, tup1.EntityID)
		if len(rows) != 2 || rows[0].TaskID != tup1.TaskID || rows[1].TaskID != tup2.TaskID || rows[0].ID >= rows[1].ID {
			t.Errorf("ListForEntity = %+v, want the two rows oldest first", rows)
		}
		if other := list(t, s, orgID, tup2.EntityID); len(other) != 1 {
			t.Errorf("entity 2 lists %d rows, want 1", len(other))
		}
	})

	t.Run("Describe_names_the_entity_and_the_firing", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		enqueue(t, s, orgID, tup, tup.TaskID, db.AgentClaimStamp{})
		f := one(t, s, orgID, tup.EntityID)
		subjects, err := handle(t, s).Describe(ctx, orgID, []int64{f.ID})
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		subj, ok := subjects[f.ID]
		if !ok {
			t.Fatalf("Describe returned no subject for row %d", f.ID)
		}
		if subj.Label == "" || subj.Fields["task_id"] != tup.TaskID || subj.Fields["trigger_id"] != tup.TriggerID || subj.Fields["triggering_event_id"] != tup.EventID {
			t.Errorf("subject = %+v", subj)
		}
		if _, has := subj.Fields["trigger"]; !has {
			t.Errorf("subject %+v lacks the handler's name field", subj)
		}
		if _, has := subj.Fields["skip_reason"]; has {
			t.Errorf("subject %+v carries a skip reason the row does not have", subj)
		}
	})

	t.Run("Transaction_bound_store_admits_inside_the_transaction", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		rolledBack := errors.New("roll it back")
		err := seed.InTx(t, func(tx db.PendingFiringsStore) error {
			inserted, _, err := tx.Enqueue(ctx, orgID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{})
			if err != nil || !inserted {
				t.Fatalf("Enqueue on the transaction: inserted=%v err=%v", inserted, err)
			}
			if has, err := tx.HasUnsettledForTask(ctx, orgID, tup.TaskID); err != nil || !has {
				t.Errorf("HasUnsettledForTask inside the transaction = %v err=%v, want true", has, err)
			}
			if _, err := tx.Claim(ctx, firingOwner, 1); !errors.Is(err, db.ErrTxBoundStore) {
				t.Errorf("Claim on a transaction-bound store = %v, want ErrTxBoundStore", err)
			}
			return rolledBack
		})
		if !errors.Is(err, rolledBack) {
			t.Fatalf("InTx returned %v, want the body's error", err)
		}
		if rows := list(t, s, orgID, tup.EntityID); len(rows) != 0 {
			t.Errorf("a rolled-back admission left %d rows", len(rows))
		}
		if err := seed.InTx(t, func(tx db.PendingFiringsStore) error {
			_, _, err := tx.Enqueue(ctx, orgID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{})
			return err
		}); err != nil {
			t.Fatalf("InTx: %v", err)
		}
		one(t, s, orgID, tup.EntityID)
	})
}
