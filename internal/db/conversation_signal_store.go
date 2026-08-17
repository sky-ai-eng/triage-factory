package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ConversationSignalStore is the cross-pod run-control outbox (TFAC-585, Postgres
// only — see docs/for-agents/specs/horizontal-scaling/README.md §5.2, "RunController
// gets its intended second implementation"). Every method is admin-pool,
// no claims-scoped variant and no "...System" suffix: conversation_signals is pure
// system-to-system coordination, never read under a user's RLS context —
// same unsuffixed shape as ConversationQueueStore/EventQueueStore.
//
// The SQLite implementation returns ErrNotApplicableInLocal from every
// method (see internal/db/sqlite/conversation_signals.go): local mode is always its
// own run's owner (TF_ROLE=all is one process), so no code path may ever
// reach this store there — internal/delegate only constructs the cross-pod
// RunController when runmode.Current() == ModeMulti, so the SQLite stub is
// a defense-in-depth backstop, not a path any real request takes.
type ConversationSignalStore interface {
	// Insert records a new signal targeting an executor and returns its id
	// — the correlation id a waiting control pod polls/LISTENs on. payload
	// is pre-marshaled JSON, or "" for a signal with no payload (cancel/
	// interrupt carry none). The caller NOTIFYs tf_ctl with the returned id
	// right after (see internal/pgnotify) — not inside this call, so the
	// notify rides the same commit boundary the caller controls.
	Insert(ctx context.Context, orgID, conversationID string, kind domain.ConversationSignalKind, payload, target string) (id int64, err error)

	// ListUnackedForTarget returns every unacked signal whose target is me,
	// ascending id — the owner's apply-loop scan
	// (WHERE target=me AND acked_at IS NULL ORDER BY id). Ascending id
	// across the whole target set trivially preserves the required
	// per-run apply order too; cross-run ordering is unspecified by
	// contract.
	ListUnackedForTarget(ctx context.Context, target string) ([]domain.ConversationSignal, error)

	// Ack marks a signal applied (or resolved gone/stale). Idempotent: a
	// signal that's already acked is left untouched, never an error —
	// duplicate signals are legal (see the ticket's idempotency decision).
	Ack(ctx context.Context, id int64, result string) error

	// AckStatus polls one signal's ack state — the waiting control pod's
	// 250ms backstop poll, run alongside its tf_ctl ack-NOTIFY LISTEN
	// (whichever observes the ack first wins).
	AckStatus(ctx context.Context, id int64) (acked bool, result string, err error)

	// PurgeAcked deletes acked rows whose acked_at is older than olderThan
	// (the 24h audit-convenience window, TF_CONVERSATION_SIGNAL_PURGE_AFTER) and
	// returns the count removed.
	PurgeAcked(ctx context.Context, olderThan time.Duration) (int, error)
}
