package agenthost

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The write stage's retry. Every record the funnel writes describes an act
// that already happened, so a failed write is not the agent's to retry — it
// has moved on — and nothing else would. The funnel retries it itself: on a
// context detached from the caller's, with a budget of its own, and only for a
// failure another attempt can get past (db.IsRetryable).
//
// A retry is safe because every write in the funnel is idempotent across
// attempts. The deliverable is an insert that does nothing when its row is
// there. The ledger row's dedup key is fixed
// before the first attempt (mintActionDedupKey), so an attempt whose commit
// landed but whose acknowledgement was lost is followed by one that conflicts
// and does nothing, rather than by a second row.

// recordWriteBudget bounds one recording, every attempt included. The funnel
// runs on a context detached from its caller's, so this is the only deadline it
// has: a run ending or an agent's CLI disconnecting mid-write no longer cancels
// the row describing what that run already did.
const recordWriteBudget = 30 * time.Second

// recordRetryPolicy is the write stage's backoff, a value so a test can shrink
// it.
type recordRetryPolicy struct {
	// attempts is the most times one write is tried.
	attempts int
	// initial and max bound the wait between attempts, which doubles from
	// one to the next.
	initial, max time.Duration
}

var recordRetry = recordRetryPolicy{attempts: 6, initial: 100 * time.Millisecond, max: 2 * time.Second}

// recordContext detaches ctx from its caller's cancellation and deadline, keeping
// its values (the trace, a relayed record), and gives it the write stage's own
// budget.
func recordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordWriteBudget)
}

// withRecordRetry runs write until it succeeds, fails on an error another
// attempt cannot get past, runs out of attempts, or ctx ends, and returns its
// last error. write must be safe to run more than once.
func withRecordRetry(ctx context.Context, write func() error) error {
	delay := recordRetry.initial
	for attempt := 1; ; attempt++ {
		err := write()
		if err == nil || attempt >= recordRetry.attempts || !db.IsRetryable(err) {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		delay = min(2*delay, recordRetry.max)
	}
}

// relayedRecord is one application of an audit record the sidecar relayed,
// carried on the context through the recorders that apply it. It exists
// because the sidecar holds the record until the orchestrator acknowledges it:
// a write that failed on a retryable error past the funnel's own retries can
// be tried again later by having the sidecar send the record again, so the
// failure is not yet a loss.
//
// Its state belongs to one application, which runs on one goroutine
// (RelayServer.DispatchRecord, through the recorders it calls), so it takes no
// lock.
type relayedRecord struct {
	// key is the sidecar-minted key of the record, the same on every resend.
	key string
	// minted counts the ledger keys this application has derived from key.
	minted int
	// deferred is set when a write failed on a retryable error: the record
	// will be sent again, so this application's losses are not final.
	deferred bool
	// held are the write-stage drops this application would count, by op.
	// They are counted only if the application is final — a deferred one is
	// applied again, and that application counts its own.
	held []string
}

type relayedRecordCtxKey struct{}

func withRelayedRecord(ctx context.Context, rec *relayedRecord) context.Context {
	return context.WithValue(ctx, relayedRecordCtxKey{}, rec)
}

func relayedRecordFrom(ctx context.Context) *relayedRecord {
	rec, _ := ctx.Value(relayedRecordCtxKey{}).(*relayedRecord)
	return rec
}

// countHeld counts the drops this application held back, now that it is
// final.
func (rec *relayedRecord) countHeld(ctx context.Context) {
	for _, op := range rec.held {
		recordAuditDrop(ctx, dropStageRecord, op)
	}
	rec.held = nil
}

// mintActionDedupKey fixes act's dedup key before its first write attempt, when
// the action carries none of its own. A relayed record derives it from the
// record's key, so every application of the record — the funnel's retries and
// the sidecar's resends alike — writes under the same one. Anything else gets
// a uuid, which covers the funnel's own retries.
func mintActionDedupKey(ctx context.Context, info ConversationInfo, act *domain.ExternalAction) {
	if act == nil || act.DedupKey != "" {
		return
	}
	if rec := relayedRecordFrom(ctx); rec != nil {
		act.DedupKey = domain.RelayedRecordDedupKey(info.ConversationID, rec.key, rec.minted)
		rec.minted++
		return
	}
	act.DedupKey = uuid.NewString()
}

// mintDedupKey is mintActionDedupKey's key for a ledger row built outside this
// package, whose writer takes the key as given.
func mintDedupKey(ctx context.Context, info ConversationInfo) string {
	act := &domain.ExternalAction{}
	mintActionDedupKey(ctx, info, act)
	return act.DedupKey
}

// recordWriteDrop accounts for a record lost at the write stage, cause being
// the error that lost it. Outside a relayed record it is counted at once. In
// one, it is held until the application's outcome is known, and a retryable
// cause defers the record: the sidecar will send it again.
func recordWriteDrop(ctx context.Context, op string, cause error) {
	if rec := relayedRecordFrom(ctx); rec != nil {
		rec.held = append(rec.held, op)
		if db.IsRetryable(cause) {
			rec.deferred = true
		}
		return
	}
	recordAuditDrop(ctx, dropStageRecord, op)
}
