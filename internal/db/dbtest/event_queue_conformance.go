package dbtest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventQueueStoreFactory is what a per-backend test file hands to
// RunEventQueueStoreConformance. Returns the wired EventQueueStore, the
// orgID to pass to org-scoped calls, and a seeder for the entity rows
// Enqueue's event-row FK needs.
type EventQueueStoreFactory func(t *testing.T) (
	store db.EventQueueStore,
	orgID string,
	seed EventQueueSeeder,
)

// EventQueueSeeder bags the raw-SQL fixtures a backend test provides.
type EventQueueSeeder struct {
	// Entity inserts a fresh entity row and returns its id. Enqueue's
	// events-audit row FKs to it (and the queue row denormalizes it).
	Entity func(t *testing.T) string

	// ExpireLease rewinds a leased row's lease_expires_at into the past,
	// standing in for a holder that died without a terminal write. Raw SQL
	// because the store offers no way to write the column — every other
	// caller gets it stamped by Claim and RenewLease — and a reclaim can
	// only be exercised against a lease that has run out.
	ExpireLease func(t *testing.T, queueID int64)

	// Ripen clears a ready row's next_attempt_at, so a requeued row is
	// claimable again without waiting out the kind's backoff.
	Ripen func(t *testing.T, queueID int64)

	// RequestCancel records a cancellation request on a row. The store
	// exposes no request path yet, and the claim's and renewal's settlement
	// of one is what these cases exercise.
	RequestCancel func(t *testing.T, queueID int64)

	// ClearEntityRef NULLs a queue row's entity_id, standing in for a row
	// the display join finds no entity for. Raw SQL because Enqueue is the
	// only writer of the column and it never writes NULL for a
	// router-bound event. This is the nullable column's reachable state at
	// read time: an actually-deleted entity cascades the queue row away
	// with it, so what a list can encounter is the empty reference, not a
	// dangling one.
	ClearEntityRef func(t *testing.T, queueID int64)

	// KeyedRow inserts a ready row carrying the entity's close-obligation
	// key under an ordinary event type. It is the device that forces the
	// admission race: the batch's in-transaction unsettled check matches on
	// event type and passes, while the uniqueness index matches on the key
	// and refuses, which is exactly the shape the batch must fail on.
	KeyedRow func(t *testing.T, entityID string)

	// EntitySnapshot reads an entity's stored snapshot_json and poll_seq.
	// The CAS half of EnqueueBatchWithSnapshotCAS is a write to a table
	// this store doesn't otherwise touch, so the assertions need a way to
	// see it — both that a winner advanced it and that a loser left it
	// exactly as it was.
	EntitySnapshot func(t *testing.T, entityID string) (snapshotJSON string, pollSeq int64)

	// CountEventRows counts the events audit rows for an entity. The queue
	// row's event_id FK proves an audit row landed, but nothing proves the
	// reverse — a rolled-back batch must leave neither, and only this can
	// say so.
	CountEventRows func(t *testing.T, entityID string) int
}

// conformanceOwner is the owner every claim in this suite stamps a row
// with, except where a case is specifically exercising a takeover.
var conformanceOwner = workitem.Owner{ID: "conformance-executor", Epoch: 1}

// handleOf is the store as the operator surface sees it. Every dialect's
// event queue store is also its work-kind handle.
func handleOf(t *testing.T, s db.EventQueueStore) db.WorkKindHandle {
	t.Helper()
	h, ok := s.(db.WorkKindHandle)
	if !ok {
		t.Fatalf("%T does not implement db.WorkKindHandle", s)
	}
	return h
}

// redriveOn is the operator redrive of one row, run the way the work handler
// runs it: the package's control on the kind's handle.
func redriveOn(ctx context.Context, s db.EventQueueStore, orgID string, id int64) error {
	h, ok := s.(db.WorkKindHandle)
	if !ok {
		return errors.New("store is not a work-kind handle")
	}
	return workitem.Redrive(ctx, h.Conn(), h.Kind(), orgID, id, "operator")
}

// enqueueOn is a local helper: Enqueue a ci_check_failed event against
// entityID and return the event id. ci_check_failed is a seeded catalog
// entry, so the events.event_type FK is satisfied. No traceparent — the
// untraced producer is the ordinary case, and the round-trip of a real one
// has its own case below.
func enqueueOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID string) string {
	t.Helper()
	return enqueueTracedOn(t, ctx, s, orgID, entityID, "")
}

// enqueueTracedOn is enqueueOn with the producer's trace context attached.
func enqueueTracedOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID, traceparent string) string {
	t.Helper()
	id, err := s.Enqueue(ctx, orgID, domain.Event{
		EntityID:     &entityID,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     "",
		MetadataJSON: "",
	}, traceparent)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatalf("Enqueue returned empty event id")
	}
	return id
}

// claimOne claims exactly one row and returns it.
func claimOne(t *testing.T, ctx context.Context, s db.EventQueueStore, owner workitem.Owner) db.ClaimedEvent {
	t.Helper()
	batch, err := s.Claim(ctx, owner, 1)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("Claim returned %d events, want 1 (cancelled=%d parked=%d)", len(batch.Events), batch.Cancelled, batch.Parked)
	}
	return batch.Events[0]
}

// parkOn drives one event to the parked terminal — enqueue, claim, a
// permanent requeue — and returns the claimed row so the caller has its
// queue id. The park is the only way a row enters the operator surface, so
// every case below that needs a parked row builds it this way rather than
// writing the status directly.
func parkOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID, reason string) db.ClaimedEvent {
	t.Helper()
	enqueueOn(t, ctx, s, orgID, entityID)
	ce := claimOne(t, ctx, s, conformanceOwner)
	parked, err := s.Requeue(ctx, ce.Receipt, workitem.OutcomePermanent, errors.New(reason))
	if err != nil {
		t.Fatalf("Requeue permanent: %v", err)
	}
	if !parked {
		t.Fatal("a permanent requeue did not park")
	}
	return ce
}

// rowByID reads one queue row back through ListForEntity.
func rowByID(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID string, id int64) domain.QueuedEvent {
	t.Helper()
	rows, err := s.ListForEntity(ctx, orgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("queue row %d not found among %d rows for %s", id, len(rows), entityID)
	return domain.QueuedEvent{}
}

// RunEventQueueStoreConformance covers the durable-queue contract every
// backend impl must hold: admission under the work-item block, the CAS
// batch's atomicity and its close-obligation key, the claim / renew /
// terminal verbs' fencing, the retry outcomes, the prune's retention, the
// parked operator surface, and the unsettled-close predicate.
func RunEventQueueStoreConformance(t *testing.T, mk EventQueueStoreFactory) {
	t.Helper()
	ctx := context.Background()

	// batchEvent builds one diffed-transition event for the batch cases.
	// ci_check_failed is a seeded catalog entry, so the events.event_type
	// FK is satisfied; dedupKey keeps sibling events in a batch distinct
	// the way a real check-name discriminator does.
	batchEvent := func(entityID, dedupKey string) domain.Event {
		return domain.Event{
			EntityID:  &entityID,
			EventType: domain.EventGitHubPRCICheckFailed,
			DedupKey:  dedupKey,
		}
	}
	owedOn := func(entityID string) domain.Event {
		return domain.Event{EntityID: &entityID, EventType: domain.EventSystemEntityCloseOwed, MetadataJSON: `{"reason":"terminal_snapshot_active_entity"}`}
	}

	t.Run("Status_vocabulary_is_the_contracts", func(t *testing.T) {
		for _, pair := range [][2]string{
			{domain.QueuedEventStatusReady, workitem.StatusReady},
			{domain.QueuedEventStatusLeased, workitem.StatusLeased},
			{domain.QueuedEventStatusDone, workitem.StatusDone},
			{domain.QueuedEventStatusParked, workitem.StatusParked},
			{domain.QueuedEventStatusCancelled, workitem.StatusCancelled},
		} {
			if pair[0] != pair[1] {
				t.Errorf("domain status %q does not match workitem's %q", pair[0], pair[1])
			}
		}
	})

	t.Run("Enqueue_admits_a_ready_row_with_block_defaults", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		eventID := enqueueOn(t, ctx, s, orgID, entityID)

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 queue row, got %d", len(rows))
		}
		r := rows[0]
		if r.Status != domain.QueuedEventStatusReady {
			t.Errorf("status = %q, want ready", r.Status)
		}
		if r.EventID != eventID {
			t.Errorf("event_id = %q, want %q (the enqueued event)", r.EventID, eventID)
		}
		if r.EntityID != entityID {
			t.Errorf("entity_id = %q, want %q", r.EntityID, entityID)
		}
		if r.EventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("event_type = %q, want %q", r.EventType, domain.EventGitHubPRCICheckFailed)
		}
		if r.OrgID != orgID {
			t.Errorf("org_id = %q, want %q", r.OrgID, orgID)
		}
		if r.Attempt != 0 || r.MaxAttempts != 5 || r.LeaseGeneration != 0 {
			t.Errorf("attempt=%d max_attempts=%d generation=%d, want 0/5/0 before any claim", r.Attempt, r.MaxAttempts, r.LeaseGeneration)
		}
		if r.UniqueKey != "" {
			t.Errorf("unique_key = %q, want none on an ordinary event", r.UniqueKey)
		}
		if r.NextAttemptAt != nil || r.LeasedAt != nil || r.LeaseExpiresAt != nil || r.DoneAt != nil || r.LeaseOwner != "" {
			t.Errorf("a fresh row carries lease or settlement columns: %+v", r)
		}
		if r.FirstEnqueuedAt.IsZero() || !r.FirstEnqueuedAt.Equal(r.CreatedAt) {
			t.Errorf("first_enqueued_at = %s, created_at = %s, want both set and equal", r.FirstEnqueuedAt, r.CreatedAt)
		}
		if r.EntityPollSeq != nil {
			t.Errorf("entity_poll_seq = %d, want NULL on the ingest path", *r.EntityPollSeq)
		}
	})

	t.Run("Enqueue_distinct_rows_per_event", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		id1 := enqueueOn(t, ctx, s, orgID, entityID)
		id2 := enqueueOn(t, ctx, s, orgID, entityID)
		if id1 == id2 {
			t.Fatalf("two Enqueues should mint distinct event ids, got %q twice", id1)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 2 {
			t.Errorf("expected 2 distinct queue rows, got %d", len(rows))
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_commits_snapshot_and_batch", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		_, pollSeq := seed.EntitySnapshot(t, entityID)

		evts := []domain.Event{batchEvent(entityID, "build"), batchEvent(entityID, "lint")}
		ok, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"cas":"won"}`, pollSeq,
			evts, []string{"00-11111111111111111111111111111111-2222222222222222-01", ""})
		if err != nil {
			t.Fatalf("EnqueueBatchWithSnapshotCAS: %v", err)
		}
		if !ok {
			t.Fatal("ok = false against the entity's current poll_seq, want the CAS to win")
		}
		if len(ids) != len(evts) {
			t.Fatalf("returned %d event ids for a batch of %d", len(ids), len(evts))
		}
		if ids[0] == ids[1] {
			t.Errorf("batch minted the same event id twice: %q", ids[0])
		}

		snap, seq := seed.EntitySnapshot(t, entityID)
		if !strings.Contains(snap, `"cas"`) {
			t.Errorf("snapshot_json = %q, want the CAS'd write", snap)
		}
		if seq != pollSeq+1 {
			t.Errorf("poll_seq = %d, want %d (bumped exactly once)", seq, pollSeq+1)
		}

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != len(evts) {
			t.Fatalf("expected %d queue rows, got %d", len(evts), len(rows))
		}
		for i, r := range rows {
			if r.EventID != ids[i] {
				t.Errorf("row %d event_id = %q, want %q (ids are parallel to the batch)", i, r.EventID, ids[i])
			}
			if r.Status != domain.QueuedEventStatusReady {
				t.Errorf("row %d status = %q, want ready", i, r.Status)
			}
			if r.EntityID != entityID {
				t.Errorf("row %d entity_id = %q, want %q", i, r.EntityID, entityID)
			}
		}
		// traceparents are parallel too: the first event carries the
		// producer's context, the second was handed "" and reads back empty.
		if rows[0].Traceparent == "" || rows[1].Traceparent != "" {
			t.Errorf("traceparents = [%q, %q], want the batch's parallel entries", rows[0].Traceparent, rows[1].Traceparent)
		}
		if n := seed.CountEventRows(t, entityID); n != len(evts) {
			t.Errorf("events rows = %d, want %d (one audit row per queued event)", n, len(evts))
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_lost_cas_writes_nothing", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		_, pollSeq := seed.EntitySnapshot(t, entityID)

		// The current holder's write lands first and bumps poll_seq.
		if ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"winner":true}`, pollSeq, nil, nil); err != nil || !ok {
			t.Fatalf("priming write: ok=%v err=%v", ok, err)
		}
		won, wonSeq := seed.EntitySnapshot(t, entityID)

		// The straggler still holds the pre-bump poll_seq. Its CAS matches
		// zero rows, so its whole batch — snapshot AND events — must be
		// discarded rather than half-applied.
		ok, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"straggler":true}`, pollSeq,
			[]domain.Event{batchEvent(entityID, "build"), batchEvent(entityID, "lint")}, nil)
		if err != nil {
			t.Fatalf("straggler batch: unexpected error %v", err)
		}
		if ok {
			t.Fatal("ok = true for a stale poll_seq, want the CAS to lose")
		}
		if len(ids) != 0 {
			t.Errorf("a losing batch returned %d event ids, want none", len(ids))
		}

		snap, seq := seed.EntitySnapshot(t, entityID)
		if snap != won || seq != wonSeq {
			t.Errorf("a lost CAS moved the snapshot: (%q, %d), want the winner's (%q, %d)", snap, seq, won, wonSeq)
		}
		if rows, err := s.ListForEntity(ctx, orgID, entityID); err != nil {
			t.Fatalf("ListForEntity: %v", err)
		} else if len(rows) != 0 {
			t.Errorf("a losing writer wrote %d event_queue rows, want 0", len(rows))
		}
		if n := seed.CountEventRows(t, entityID); n != 0 {
			t.Errorf("a losing writer wrote %d events rows, want 0", n)
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_failed_insert_rolls_back_the_cas", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		before, pollSeq := seed.EntitySnapshot(t, entityID)

		// Second event carries an event_type no catalog row backs, so its
		// audit-row insert violates the FK — an enqueue failure arriving
		// mid-batch, after the CAS already matched. The whole transaction
		// must roll back, snapshot included, or the next cycle would diff
		// against a snapshot whose transitions were never recorded.
		ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"doomed":true}`, pollSeq,
			[]domain.Event{
				batchEvent(entityID, "build"),
				{EntityID: &entityID, EventType: "github:pr:no_such_event_type"},
			}, nil)
		if err == nil {
			t.Fatal("a batch containing an unqueueable event should error")
		}
		if ok {
			t.Error("ok = true for a batch that did not commit")
		}

		snap, seq := seed.EntitySnapshot(t, entityID)
		if snap != before || seq != pollSeq {
			t.Errorf("snapshot advanced despite the failed batch: (%q, %d), want (%q, %d)", snap, seq, before, pollSeq)
		}
		if rows, err := s.ListForEntity(ctx, orgID, entityID); err != nil {
			t.Fatalf("ListForEntity: %v", err)
		} else if len(rows) != 0 {
			t.Errorf("failed batch left %d event_queue rows, want 0 (including the event that inserted cleanly)", len(rows))
		}
		if n := seed.CountEventRows(t, entityID); n != 0 {
			t.Errorf("failed batch left %d events rows, want 0", n)
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_stamps_the_version_the_batch_was_judged_at", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		_, pollSeq := seed.EntitySnapshot(t, entityID)

		if ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"judged":true}`, pollSeq,
			[]domain.Event{batchEvent(entityID, "build")}, nil); err != nil || !ok {
			t.Fatalf("EnqueueBatchWithSnapshotCAS: ok=%v err=%v", ok, err)
		}
		// The ingest path carries no version.
		enqueueOn(t, ctx, s, orgID, entityID)

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil || len(rows) != 2 {
			t.Fatalf("ListForEntity: rows=%d err=%v", len(rows), err)
		}
		if rows[0].EntityPollSeq == nil || *rows[0].EntityPollSeq != pollSeq+1 {
			t.Errorf("CAS-path row entity_poll_seq = %v, want %d — the poll_seq the CAS advanced to", rows[0].EntityPollSeq, pollSeq+1)
		}
		if rows[1].EntityPollSeq != nil {
			t.Errorf("ingest-path row entity_poll_seq = %d, want NULL", *rows[1].EntityPollSeq)
		}
		// The claim reads the same column back: the consumer is where the
		// version is used.
		ce := claimOne(t, ctx, s, conformanceOwner)
		if ce.Event.EntityPollSeq == nil || *ce.Event.EntityPollSeq != pollSeq+1 {
			t.Errorf("claimed entity_poll_seq = %v, want %d", ce.Event.EntityPollSeq, pollSeq+1)
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_admits_one_close_obligation_while_one_is_unsettled", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		_, pollSeq := seed.EntitySnapshot(t, entityID)
		seq := pollSeq
		rowsOf := func(eventType string) []domain.QueuedEvent {
			t.Helper()
			rows, err := s.ListForEntity(ctx, orgID, entityID)
			if err != nil {
				t.Fatalf("ListForEntity: %v", err)
			}
			var out []domain.QueuedEvent
			for _, r := range rows {
				if r.EventType == eventType {
					out = append(out, r)
				}
			}
			return out
		}
		unsettled := func() bool {
			t.Helper()
			got, err := s.UnsettledCloseExistsSystem(ctx, orgID, entityID)
			if err != nil {
				t.Fatalf("UnsettledCloseExistsSystem: %v", err)
			}
			return got
		}
		// owe runs one cycle's obligation through the CAS and reports whether
		// it was admitted.
		owe := func() bool {
			t.Helper()
			ok, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"terminal":true}`, seq, []domain.Event{owedOn(entityID)}, nil)
			if err != nil || !ok {
				t.Fatalf("obligation cycle at seq %d: ok=%v err=%v", seq, ok, err)
			}
			seq++
			if len(ids) != 1 {
				t.Fatalf("obligation ids = %v, want one slot", ids)
			}
			return ids[0] != ""
		}

		if unsettled() {
			t.Fatal("unsettled = true on an entity with no queue rows")
		}

		// First cycle after a lost close: the obligation lands, under its key.
		if !owe() {
			t.Fatal("first obligation was declined")
		}
		owed := rowsOf(domain.EventSystemEntityCloseOwed)
		if len(owed) != 1 || owed[0].UniqueKey != workkinds.EventQueueCloseOwedKey(entityID) {
			t.Fatalf("obligation rows = %+v, want one carrying the entity's close key", owed)
		}
		if !unsettled() {
			t.Error("unsettled = false with the obligation ready")
		}
		// Next cycle, the obligation still ready: the CAS still wins (the
		// snapshot advances) but the obligation is declined — no second
		// events row, no second queue row.
		if owe() {
			t.Error("a second obligation was admitted while the first is ready")
		}
		if n := seed.CountEventRows(t, entityID); n != 1 {
			t.Errorf("events rows = %d, want 1 — a declined obligation records nothing", n)
		}
		if _, got := seed.EntitySnapshot(t, entityID); got != seq {
			t.Errorf("poll_seq = %d, want %d — the snapshot still advances when the obligation is declined", got, seq)
		}

		// Leased still counts as unsettled.
		ce := claimOne(t, ctx, s, conformanceOwner)
		if !unsettled() {
			t.Error("unsettled = false with the obligation leased")
		}
		if owe() {
			t.Error("an obligation was admitted while one is leased")
		}

		// Parked does too: the parked row holds the key, and the operator
		// surface is where it is dealt with.
		if parked, err := s.Requeue(ctx, ce.Receipt, workitem.OutcomePermanent, errors.New("cannot close")); err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}
		if !unsettled() {
			t.Error("unsettled = false with the obligation parked; it still holds the key")
		}
		if owe() {
			t.Error("an obligation was admitted while one is parked")
		}
		if n := len(rowsOf(domain.EventSystemEntityCloseOwed)); n != 1 {
			t.Errorf("obligation rows = %d, want 1 across ready, leased and parked", n)
		}

		// Done releases it: a redriven, completed obligation lets the next
		// cycle owe again.
		if err := redriveOn(ctx, s, orgID, ce.Event.ID); err != nil {
			t.Fatalf("Redrive: %v", err)
		}
		ce2 := claimOne(t, ctx, s, conformanceOwner)
		if err := s.MarkDone(ctx, ce2.Receipt); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		if unsettled() {
			t.Error("unsettled = true with only a done obligation")
		}
		if !owe() {
			t.Error("an obligation was declined after the previous one settled done")
		}
		if n := len(rowsOf(domain.EventSystemEntityCloseOwed)); n != 2 {
			t.Errorf("obligation rows = %d, want 2 (the done one and the fresh one)", n)
		}

		// Cancelled releases it too.
		fresh := rowsOf(domain.EventSystemEntityCloseOwed)[1]
		seed.RequestCancel(t, fresh.ID)
		if batch, err := s.Claim(ctx, conformanceOwner, 5); err != nil || batch.Cancelled != 1 || len(batch.Events) != 0 {
			t.Fatalf("claim settling the request: %+v err=%v", batch, err)
		}
		if unsettled() {
			t.Error("unsettled = true with the obligation cancelled")
		}
		if !owe() {
			t.Error("an obligation was declined after the previous one was cancelled")
		}

		// A real terminating transition in flight declines the obligation
		// too: the router will close from the transition.
		other := seed.Entity(t)
		_, otherSeq := seed.EntitySnapshot(t, other)
		merged := domain.Event{EntityID: &other, EventType: domain.EventGitHubPRMerged}
		if ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, other, `{"m":1}`, otherSeq, []domain.Event{merged}, nil); err != nil || !ok {
			t.Fatalf("merged transition: ok=%v err=%v", ok, err)
		}
		if _, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, other, `{"m":2}`, otherSeq+1, []domain.Event{owedOn(other)}, nil); err != nil || ids[0] != "" {
			t.Fatalf("obligation behind a ready transition: ids=%v err=%v, want declined", ids, err)
		}

		// Only the obligation is deduplicated: an ordinary event in the
		// same batch still lands beside a declined one.
		if _, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, other, `{"m":3}`, otherSeq+2,
			[]domain.Event{batchEvent(other, "build"), owedOn(other)}, nil); err != nil || ids[0] == "" || ids[1] != "" {
			t.Fatalf("mixed batch ids = %v err=%v, want the transition minted and the obligation declined", ids, err)
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_forced_admission_race_rolls_back", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		before, pollSeq := seed.EntitySnapshot(t, entityID)

		// A row holding the entity's close key under an event type the
		// unsettled check does not look at: the check passes, the index
		// refuses, and the batch must fail whole rather than leave the
		// obligation's events row behind.
		seed.KeyedRow(t, entityID)
		seededEvents := seed.CountEventRows(t, entityID)
		ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"raced":true}`, pollSeq,
			[]domain.Event{batchEvent(entityID, "build"), owedOn(entityID)}, nil)
		if err == nil || !strings.Contains(err.Error(), "raced its own check") {
			t.Fatalf("batch err = %v, want the admission-race error", err)
		}
		if ok {
			t.Error("ok = true for a batch that did not commit")
		}
		snap, seq := seed.EntitySnapshot(t, entityID)
		if snap != before || seq != pollSeq {
			t.Errorf("snapshot moved despite the rolled-back batch: (%q, %d), want (%q, %d)", snap, seq, before, pollSeq)
		}
		if n := seed.CountEventRows(t, entityID); n != seededEvents {
			t.Errorf("events rows = %d, want the %d seeded — the batch's rows rolled back with it", n, seededEvents)
		}
		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 || rows[0].EventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("queue rows = %+v, want only the seeded keyed row", rows)
		}
	})

	t.Run("EnqueueBatchWithSnapshotCAS_empty_batch_is_a_pure_cas", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		_, pollSeq := seed.EntitySnapshot(t, entityID)

		// A cycle that refreshed an entity and diffed no transitions takes
		// this same call, so it has to behave like the CAS it replaces.
		ok, ids, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"quiet":true}`, pollSeq, nil, nil)
		if err != nil {
			t.Fatalf("EnqueueBatchWithSnapshotCAS: %v", err)
		}
		if !ok {
			t.Fatal("ok = false for an empty batch against the current poll_seq")
		}
		if len(ids) != 0 {
			t.Errorf("empty batch returned %d event ids, want none", len(ids))
		}

		snap, seq := seed.EntitySnapshot(t, entityID)
		if !strings.Contains(snap, `"quiet"`) || seq != pollSeq+1 {
			t.Errorf("snapshot = (%q, %d), want the write landed at seq %d", snap, seq, pollSeq+1)
		}
		if rows, _ := s.ListForEntity(ctx, orgID, entityID); len(rows) != 0 {
			t.Errorf("empty batch wrote %d queue rows, want 0", len(rows))
		}
		if n := seed.CountEventRows(t, entityID); n != 0 {
			t.Errorf("empty batch wrote %d events rows, want 0", n)
		}
	})

	t.Run("Claim_fifo_with_receipts_and_typed_columns", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		first := enqueueOn(t, ctx, s, orgID, entityID)
		second := enqueueOn(t, ctx, s, orgID, entityID)

		batch, err := s.Claim(ctx, workitem.Owner{ID: "worker-a", Epoch: 7}, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Events) != 2 {
			t.Fatalf("Claim returned %d events, want 2", len(batch.Events))
		}
		if batch.Cancelled != 0 || batch.Parked != 0 || batch.Reclaimed != 0 {
			t.Errorf("fresh claim counted cancelled=%d parked=%d reclaimed=%d, want zeros", batch.Cancelled, batch.Parked, batch.Reclaimed)
		}
		if batch.Events[0].Event.EventID != first || batch.Events[1].Event.EventID != second {
			t.Errorf("claim order = [%q, %q], want FIFO [%q, %q]", batch.Events[0].Event.EventID, batch.Events[1].Event.EventID, first, second)
		}
		for _, ce := range batch.Events {
			r, e := ce.Receipt, ce.Event
			if r.ItemID != e.ID || r.OrgID != e.OrgID || r.OrgID != orgID {
				t.Errorf("receipt %+v does not address its row %d/%s", r, e.ID, e.OrgID)
			}
			if r.Attempt != 1 || r.LeaseGeneration != 1 || r.LeaseExpiresAt.IsZero() || r.Reclaimed {
				t.Errorf("receipt = %+v, want attempt 1, generation 1, an expiry, not reclaimed", r)
			}
			if e.Status != domain.QueuedEventStatusLeased || e.Attempt != 1 || e.LeaseGeneration != 1 {
				t.Errorf("claimed row = %+v, want leased at attempt 1 generation 1", e)
			}
			if e.LeaseOwner != "worker-a" || e.LeaseEpoch == nil || *e.LeaseEpoch != 7 {
				t.Errorf("claimed row owner = %q/%v, want worker-a/7", e.LeaseOwner, e.LeaseEpoch)
			}
			if e.LeasedAt == nil || e.LeaseExpiresAt == nil || !e.LeaseExpiresAt.Equal(r.LeaseExpiresAt) {
				t.Errorf("claimed row lease times %v/%v disagree with the receipt's %s", e.LeasedAt, e.LeaseExpiresAt, r.LeaseExpiresAt)
			}
			if e.EntityID != entityID || e.EventType != domain.EventGitHubPRCICheckFailed {
				t.Errorf("claimed row kind columns = %q/%q, want the admitted values", e.EntityID, e.EventType)
			}
		}

		// Nothing left: an empty batch, no error.
		empty, err := s.Claim(ctx, workitem.Owner{ID: "worker-a", Epoch: 7}, 10)
		if err != nil || len(empty.Events) != 0 {
			t.Fatalf("Claim on an empty queue: %+v err=%v", empty, err)
		}
	})

	t.Run("Claim_counts_settled_rows_and_reports_reclaims", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		// A row whose holder died: its lease expires, and the next claim
		// takes it over and says so.
		enqueueOn(t, ctx, s, orgID, entityID)
		dead := claimOne(t, ctx, s, workitem.Owner{ID: "vanished-pod", Epoch: 1})
		seed.ExpireLease(t, dead.Event.ID)
		// A row with a cancellation request, settled at claim.
		enqueueOn(t, ctx, s, orgID, entityID)
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		seed.RequestCancel(t, rows[1].ID)

		batch, err := s.Claim(ctx, workitem.Owner{ID: "successor", Epoch: 1}, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if batch.Cancelled != 1 || batch.Reclaimed != 1 || batch.Parked != 0 {
			t.Errorf("counts cancelled=%d reclaimed=%d parked=%d, want 1/1/0", batch.Cancelled, batch.Reclaimed, batch.Parked)
		}
		if len(batch.Events) != 1 {
			t.Fatalf("Claim returned %d events, want the reclaimed one", len(batch.Events))
		}
		got := batch.Events[0]
		if got.Event.ID != dead.Event.ID || !got.Receipt.Reclaimed || got.Receipt.PreviousOwner != "vanished-pod" {
			t.Errorf("reclaimed = %+v, want row %d taken from vanished-pod", got.Receipt, dead.Event.ID)
		}
		if got.Receipt.LeaseGeneration != dead.Receipt.LeaseGeneration+1 || got.Receipt.Attempt != 2 {
			t.Errorf("reclaim receipt generation=%d attempt=%d, want %d/2", got.Receipt.LeaseGeneration, got.Receipt.Attempt, dead.Receipt.LeaseGeneration+1)
		}
		cancelled := rowByID(t, ctx, s, orgID, entityID, rows[1].ID)
		if cancelled.Status != domain.QueuedEventStatusCancelled || cancelled.DoneAt == nil || cancelled.LastOutcome != "cancelled" {
			t.Errorf("cancelled row = %+v, want settled cancelled", cancelled)
		}

		// A row whose budget is spent parks at claim, counted rather than
		// returned.
		enqueueOn(t, ctx, s, orgID, entityID)
		var spent db.ClaimedEvent
		for i := 0; i < 5; i++ {
			spent = claimOne(t, ctx, s, conformanceOwner)
			if i < 4 {
				if parked, err := s.Requeue(ctx, spent.Receipt, workitem.OutcomeTransient, errors.New("blip")); err != nil || parked {
					t.Fatalf("Requeue %d: parked=%v err=%v", i, parked, err)
				}
				seed.Ripen(t, spent.Event.ID)
			}
		}
		// The fifth holder dies.
		seed.ExpireLease(t, spent.Event.ID)
		batch, err = s.Claim(ctx, conformanceOwner, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if batch.Parked != 1 || len(batch.Events) != 0 {
			t.Errorf("claim over a spent row: parked=%d events=%d, want 1/0", batch.Parked, len(batch.Events))
		}
		if p := rowByID(t, ctx, s, orgID, entityID, spent.Event.ID); p.Status != domain.QueuedEventStatusParked || p.LastOutcome != "transient" {
			t.Errorf("spent row = %+v, want parked under the failure that spent it", p)
		}
	})

	t.Run("RenewLease_observes_and_settles_a_cancellation_request", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		ce := claimOne(t, ctx, s, conformanceOwner)

		renewed, err := s.RenewLease(ctx, ce.Receipt)
		if err != nil {
			t.Fatalf("RenewLease: %v", err)
		}
		if renewed.LeaseExpiresAt.Before(ce.Receipt.LeaseExpiresAt) {
			t.Errorf("renewed expiry %s is before the original %s", renewed.LeaseExpiresAt, ce.Receipt.LeaseExpiresAt)
		}

		seed.RequestCancel(t, ce.Event.ID)
		if _, err := s.RenewLease(ctx, renewed); !errors.Is(err, workitem.ErrCancelled) {
			t.Fatalf("RenewLease over a request = %v, want ErrCancelled", err)
		}
		row := rowByID(t, ctx, s, orgID, entityID, ce.Event.ID)
		if row.Status != domain.QueuedEventStatusCancelled || row.CancelRequestedAt == nil {
			t.Errorf("row = %+v, want cancelled with the request retained", row)
		}
		// The settled row is no longer the holder's.
		if err := s.MarkDone(ctx, renewed); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("MarkDone after settlement = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("MarkDone_and_Requeue_refuse_a_stale_receipt", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		stale := claimOne(t, ctx, s, workitem.Owner{ID: "slow-executor", Epoch: 1})
		seed.ExpireLease(t, stale.Event.ID)

		// Expiry alone ends authority, before any successor exists.
		if err := s.MarkDone(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Fatalf("MarkDone after expiry = %v, want ErrLeaseLost", err)
		}
		successor := claimOne(t, ctx, s, workitem.Owner{ID: "successor", Epoch: 1})
		before, _ := s.ListForEntity(ctx, orgID, entityID)

		if err := s.MarkDone(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("late MarkDone = %v, want ErrLeaseLost", err)
		}
		if _, err := s.Requeue(ctx, stale.Receipt, workitem.OutcomeTransient, errors.New("late")); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("late Requeue = %v, want ErrLeaseLost", err)
		}
		if _, err := s.RenewLease(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("late RenewLease = %v, want ErrLeaseLost", err)
		}
		after, _ := s.ListForEntity(ctx, orgID, entityID)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("a stale receipt changed the row\nbefore: %+v\nafter:  %+v", before, after)
		}

		// The successor's receipt still works.
		if err := s.MarkDone(ctx, successor.Receipt); err != nil {
			t.Fatalf("successor MarkDone: %v", err)
		}
		if row := rowByID(t, ctx, s, orgID, entityID, successor.Event.ID); row.Status != domain.QueuedEventStatusDone || row.DoneAt == nil || row.LeaseOwner != "" {
			t.Errorf("row after the successor's MarkDone = %+v, want done with the lease released", row)
		}
	})

	t.Run("Requeue_outcomes_land_where_the_contract_says", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		// Transient under budget: ready with a future retry time, the
		// charge kept, the lease released.
		enqueueOn(t, ctx, s, orgID, entityID)
		ce := claimOne(t, ctx, s, conformanceOwner)
		parked, err := s.Requeue(ctx, ce.Receipt, workitem.OutcomeTransient, errors.New("db blip"))
		if err != nil || parked {
			t.Fatalf("Requeue transient: parked=%v err=%v", parked, err)
		}
		row := rowByID(t, ctx, s, orgID, entityID, ce.Event.ID)
		if row.Status != domain.QueuedEventStatusReady || row.Attempt != 1 || row.LastOutcome != "transient" || row.LastError != "db blip" {
			t.Errorf("requeued row = %+v, want ready at attempt 1 under transient", row)
		}
		if row.NextAttemptAt == nil || !row.NextAttemptAt.After(time.Now().Add(-time.Second)) {
			t.Errorf("next_attempt_at = %v, want a backoff in the future", row.NextAttemptAt)
		}
		if row.LeaseOwner != "" || row.LeaseExpiresAt != nil {
			t.Errorf("requeued row still carries a lease: %+v", row)
		}
		// Not claimable until it ripens.
		if batch, _ := s.Claim(ctx, conformanceOwner, 10); len(batch.Events) != 0 {
			t.Errorf("a deferred row was claimed before its retry time")
		}

		// The fifth failure parks.
		for i := 2; i <= 5; i++ {
			seed.Ripen(t, ce.Event.ID)
			next := claimOne(t, ctx, s, conformanceOwner)
			if next.Receipt.Attempt != i {
				t.Fatalf("claim %d charged attempt %d", i, next.Receipt.Attempt)
			}
			parked, err := s.Requeue(ctx, next.Receipt, workitem.OutcomeDeadline, errors.New("slow"))
			if err != nil {
				t.Fatalf("Requeue %d: %v", i, err)
			}
			if parked != (i == 5) {
				t.Fatalf("Requeue %d reported parked=%v", i, parked)
			}
		}
		row = rowByID(t, ctx, s, orgID, entityID, ce.Event.ID)
		if row.Status != domain.QueuedEventStatusParked || row.Attempt != 5 || row.LastOutcome != "deadline" || row.DoneAt == nil {
			t.Errorf("row after five failures = %+v, want parked under deadline", row)
		}

		// Permanent parks at once, whatever the budget.
		enqueueOn(t, ctx, s, orgID, entityID)
		perm := claimOne(t, ctx, s, conformanceOwner)
		parked, err = s.Requeue(ctx, perm.Receipt, workitem.OutcomePermanent, errors.New("event row not found"))
		if err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}
		row = rowByID(t, ctx, s, orgID, entityID, perm.Event.ID)
		if row.Status != domain.QueuedEventStatusParked || row.Attempt != 1 || row.LastOutcome != "permanent" {
			t.Errorf("row after a permanent outcome = %+v, want parked at attempt 1", row)
		}
	})

	t.Run("PruneSettled_removes_done_and_cancelled_never_parked", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		enqueueOn(t, ctx, s, orgID, entityID)
		done := claimOne(t, ctx, s, conformanceOwner)
		if err := s.MarkDone(ctx, done.Receipt); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		enqueueOn(t, ctx, s, orgID, entityID)
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		seed.RequestCancel(t, rows[1].ID)
		if batch, err := s.Claim(ctx, conformanceOwner, 10); err != nil || batch.Cancelled != 1 {
			t.Fatalf("claim settling the request: %+v err=%v", batch, err)
		}
		parkOn(t, ctx, s, orgID, entityID, "stuck")
		enqueueOn(t, ctx, s, orgID, entityID) // ready

		// A cutoff in the past keeps everything.
		if n, err := s.PruneSettled(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
			t.Fatalf("PruneSettled with a past cutoff: n=%d err=%v", n, err)
		}
		// A cutoff in the future removes the settled rows and only those.
		n, err := s.PruneSettled(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("PruneSettled: %v", err)
		}
		if n != 2 {
			t.Errorf("pruned %d rows, want the done and cancelled rows", n)
		}
		rows, _ = s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 2 {
			t.Fatalf("%d rows survive the prune, want the parked and ready rows", len(rows))
		}
		for _, r := range rows {
			if r.Status != domain.QueuedEventStatusParked && r.Status != domain.QueuedEventStatusReady {
				t.Errorf("row %d survived the prune in status %q", r.ID, r.Status)
			}
		}
	})

	t.Run("Describe_names_rows_by_their_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		older := parkOn(t, ctx, s, orgID, entityID, "route: upsert task: db down")
		newer := parkOn(t, ctx, s, orgID, entityID, "route: fire trigger: db down")

		// The operator surface reads the block through the package and asks
		// the kind for the subjects of the page it got.
		h := handleOf(t, s)
		got, total, err := workitem.List(ctx, h.Conn(), h.Kind(), orgID, workitem.StatusParked, 50, 0)
		if err != nil {
			t.Fatalf("List parked: %v", err)
		}
		if total != 2 || len(got) != 2 || got[0].ID != newer.Event.ID || got[1].ID != older.Event.ID {
			t.Fatalf("parked page = %+v (total %d), want the two parked rows newest first", got, total)
		}
		subjects, err := h.Describe(ctx, orgID, []int64{newer.Event.ID, older.Event.ID})
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if len(subjects) != 2 {
			t.Fatalf("Describe returned %d subjects, want 2", len(subjects))
		}
		subject := subjects[newer.Event.ID]
		if subject.Label == "" || subject.Detail != "Test PR" {
			t.Errorf("subject = %+v, want the entity's source id as the label and its title as the detail", subject)
		}
		if subject.Fields["event_type"] != domain.EventGitHubPRCICheckFailed || subject.Fields["entity_id"] != entityID {
			t.Errorf("subject fields = %+v, want the event's type and entity", subject.Fields)
		}
		if subject.Fields["entity_source"] != "github" || subject.Fields["entity_source_id"] != subject.Label {
			t.Errorf("subject fields = %+v, want the entity's source pair", subject.Fields)
		}
		if subject.Fields["event_id"] != newer.Event.EventID {
			t.Errorf("subject event_id = %q, want %q", subject.Fields["event_id"], newer.Event.EventID)
		}

		// An empty selection describes nothing, and an id the org does not
		// have is absent rather than an error.
		if none, err := h.Describe(ctx, orgID, nil); err != nil || len(none) != 0 {
			t.Errorf("Describe of nothing = %+v err=%v", none, err)
		}
		if miss, err := h.Describe(ctx, orgID, []int64{999999}); err != nil || len(miss) != 0 {
			t.Errorf("Describe of an unknown id = %+v err=%v, want nothing", miss, err)
		}
	})

	t.Run("Describe_survives_a_missing_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "orphaned")
		seed.ClearEntityRef(t, parked.Event.ID)

		h := handleOf(t, s)
		subjects, err := h.Describe(ctx, orgID, []int64{parked.Event.ID})
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		subject, ok := subjects[parked.Event.ID]
		if !ok {
			t.Fatal("the entity-less row was not described; the surface would hide exactly the row worth looking at")
		}
		if subject.Label != domain.EventGitHubPRCICheckFailed || subject.Detail != "" {
			t.Errorf("subject = %+v, want the event type as the label and no detail", subject)
		}
		if subject.Fields["entity_id"] != "" || subject.Fields["entity_source"] != "" || subject.Fields["entity_source_id"] != "" {
			t.Errorf("subject fields = %+v, want empty entity fields for a row with no entity", subject.Fields)
		}
	})

	t.Run("Handle_declares_the_kind", func(t *testing.T) {
		s, _, _ := mk(t)
		h := handleOf(t, s)
		if h.Name() != workkinds.EventQueueName || h.Label() != workkinds.EventQueueLabel {
			t.Errorf("handle = %q/%q, want the event queue's registered name and label", h.Name(), h.Label())
		}
		if h.Kind().Table != workkinds.EventQueue(h.Kind().Dialect).Table {
			t.Errorf("handle kind table = %q, want the declared kind", h.Kind().Table)
		}
		if h.Access() != db.WorkAccessOrgAdmin {
			t.Errorf("access = %d, want org admin", h.Access())
		}
		if c := h.Controls(); !c.Redrive || !c.Cancel || c.Supersede {
			t.Errorf("controls = %+v, want redrive and cancel without supersede", c)
		}
		if h.Objective().OldestReadyAge != workkinds.EventQueueOldestReadyObjective {
			t.Errorf("objective = %s, want %s", h.Objective().OldestReadyAge, workkinds.EventQueueOldestReadyObjective)
		}
	})

	t.Run("Redrive_grants_a_fresh_budget_and_counts_only_rows_that_moved", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "stuck")
		enqueueOn(t, ctx, s, orgID, entityID)
		ready := rowByID(t, ctx, s, orgID, entityID, parked.Event.ID+1)

		if err := redriveOn(ctx, s, orgID, parked.Event.ID); err != nil {
			t.Fatalf("Redrive: %v", err)
		}
		// Neither a ready row nor an unknown id is redrivable, and each is
		// counted out with ErrNotParked rather than failing.
		for _, id := range []int64{ready.ID, 999999} {
			if err := redriveOn(ctx, s, orgID, id); !errors.Is(err, workitem.ErrNotParked) {
				t.Errorf("Redrive of %d = %v, want ErrNotParked", id, err)
			}
		}
		row := rowByID(t, ctx, s, orgID, entityID, parked.Event.ID)
		if row.Status != domain.QueuedEventStatusReady || row.Attempt != 0 || row.NextAttemptAt != nil || row.DoneAt != nil {
			t.Errorf("redriven row = %+v, want ready with a fresh budget", row)
		}
		if row.LastOutcome != "redriven" || row.LastError != "" {
			t.Errorf("redriven row outcome/error = %q/%q, want redriven and cleared", row.LastOutcome, row.LastError)
		}
		if row.LeaseGeneration != parked.Receipt.LeaseGeneration+1 {
			t.Errorf("redriven generation = %d, want %d — the pre-park receipt must be dead", row.LeaseGeneration, parked.Receipt.LeaseGeneration+1)
		}
		if err := s.MarkDone(ctx, parked.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("pre-park receipt after a redrive = %v, want ErrLeaseLost", err)
		}
		if !row.FirstEnqueuedAt.Equal(parked.Event.FirstEnqueuedAt) {
			t.Errorf("first_enqueued_at moved on a redrive: %s -> %s", parked.Event.FirstEnqueuedAt, row.FirstEnqueuedAt)
		}

		// A second redrive of the same id moves nothing.
		if err := redriveOn(ctx, s, orgID, parked.Event.ID); !errors.Is(err, workitem.ErrNotParked) {
			t.Errorf("second Redrive = %v, want ErrNotParked", err)
		}

		// The redriven row is claimable and drives to done.
		again := claimOne(t, ctx, s, conformanceOwner)
		if again.Event.ID != parked.Event.ID || again.Receipt.Attempt != 1 {
			t.Errorf("claim after redrive = %+v, want the redriven row at attempt 1", again.Receipt)
		}
		if err := s.MarkDone(ctx, again.Receipt); err != nil {
			t.Fatalf("MarkDone after redrive: %v", err)
		}
	})

	t.Run("Traceparent_round_trips_through_claim", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		enqueueTracedOn(t, ctx, s, orgID, entityID, traceparent)

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 || rows[0].Traceparent != traceparent {
			t.Fatalf("stored traceparent = %+v, want %q", rows, traceparent)
		}
		ce := claimOne(t, ctx, s, conformanceOwner)
		if ce.Event.Traceparent != traceparent {
			t.Errorf("claimed traceparent = %q, want %q — the claim is where the consumer picks the link up", ce.Event.Traceparent, traceparent)
		}
	})

	t.Run("Traceparent_absent_reads_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		ce := claimOne(t, ctx, s, conformanceOwner)
		if ce.Event.Traceparent != "" {
			t.Errorf("traceparent = %q, want empty for an untraced producer", ce.Event.Traceparent)
		}
		if ce.Event.Status != domain.QueuedEventStatusLeased {
			t.Errorf("status = %q, want leased — an untraced row claims like any other", ce.Event.Status)
		}
	})

	t.Run("ListForEntity_id_order", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		first := enqueueOn(t, ctx, s, orgID, entityID)
		second := enqueueOn(t, ctx, s, orgID, entityID)
		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(rows))
		}
		if rows[0].EventID != first || rows[1].EventID != second {
			t.Errorf("id order broken: got [%q, %q], want [%q, %q]",
				rows[0].EventID, rows[1].EventID, first, second)
		}
	})

	t.Run("UnsettledCloseExistsSystem_per_status", func(t *testing.T) {
		s, orgID, seed := mk(t)
		unsettled := func(entityID string) bool {
			t.Helper()
			got, err := s.UnsettledCloseExistsSystem(ctx, orgID, entityID)
			if err != nil {
				t.Fatalf("UnsettledCloseExistsSystem: %v", err)
			}
			return got
		}
		// A terminating transition through the CAS path, walked through
		// each status on its own entity.
		mergedOn := func(entityID string) db.ClaimedEvent {
			t.Helper()
			_, seq := seed.EntitySnapshot(t, entityID)
			merged := domain.Event{EntityID: &entityID, EventType: domain.EventGitHubPRMerged}
			if ok, _, err := s.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, `{"m":1}`, seq, []domain.Event{merged}, nil); err != nil || !ok {
				t.Fatalf("merged transition: ok=%v err=%v", ok, err)
			}
			if !unsettled(entityID) {
				t.Errorf("ready: unsettled = false")
			}
			ce := claimOne(t, ctx, s, conformanceOwner)
			if !unsettled(entityID) {
				t.Errorf("leased: unsettled = false")
			}
			return ce
		}

		e := seed.Entity(t)
		ce := mergedOn(e)
		if err := s.MarkDone(ctx, ce.Receipt); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		if unsettled(e) {
			t.Errorf("done: unsettled = true")
		}

		e = seed.Entity(t)
		ce = mergedOn(e)
		if _, err := s.Requeue(ctx, ce.Receipt, workitem.OutcomePermanent, errors.New("stuck")); err != nil {
			t.Fatalf("Requeue permanent: %v", err)
		}
		if !unsettled(e) {
			t.Errorf("parked: unsettled = false — a parked close still holds the entity")
		}

		e = seed.Entity(t)
		ce = mergedOn(e)
		seed.RequestCancel(t, ce.Event.ID)
		if _, err := s.RenewLease(ctx, ce.Receipt); !errors.Is(err, workitem.ErrCancelled) {
			t.Fatalf("RenewLease over a request = %v, want ErrCancelled", err)
		}
		if unsettled(e) {
			t.Errorf("cancelled: unsettled = true")
		}

		// An ordinary event never counts, whatever its status.
		e = seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, e)
		if unsettled(e) {
			t.Errorf("an ordinary ready event counted as an unsettled close")
		}
	})
}
