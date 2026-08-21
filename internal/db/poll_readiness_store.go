package db

import (
	"context"
	"time"
)

// PollReadinessStore owns the poll_readiness table — the org-scoped,
// DB-backed replacement for two flags that used to be process-local state
// on whichever pod happened to run the poller: the /api/jira/stock
// readiness gate and the one-shot "config took effect" announce toast
// (TFAC-583). Under the control/standby split the pod serving a given API
// request is not necessarily the pod running the poller, so both flags
// have to live somewhere every control pod's API can read — this store.
//
// Admin-pool-only in Postgres, mirroring InstanceStore: a caller already
// has an authorized orgID in hand (session claims, or the poller's own
// system context) by the time it reaches these methods, so there is no
// browsable RLS surface to gate — these aren't tenant content, they're
// operational readiness bits. SQLite is N=1 (LocalDefaultOrgID).
type PollReadinessStore interface {
	// MarkRestarted resets readiness for (orgID, source): clears
	// last_poll_at and stamps restarted_at=now(), so a completion from
	// before this call (a stale pre-restart poll goroutine finishing late)
	// can't incorrectly flip readiness back to true. Upserts — safe to call
	// before any row exists.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping, same as
	// MarkPollComplete.
	MarkRestarted(ctx context.Context, orgID, source string) error

	// MarkPollComplete records a successful poll cycle's completion for
	// (orgID, source). startedAt is the wall-clock time the cycle began; a
	// completion whose startedAt precedes the last MarkRestarted call is
	// ignored (a straggler from before the restart). A zero startedAt is
	// always accepted — "unknown generation" fails open rather than
	// stalling readiness forever. Upserts.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping. The row
	// is a durable flag a later request read consults; the poll cycle that
	// stamps it never looks back.
	MarkPollComplete(ctx context.Context, orgID, source string, startedAt time.Time) error

	// Ready reports whether (orgID, source) has completed a poll cycle
	// since its last restart (or was never explicitly restarted and has
	// completed at least one poll ever). False, nil for an unknown pair.
	Ready(ctx context.Context, orgID, source string) (bool, error)

	// LastPollTimes returns each source's last completed poll time for the
	// org, keyed by source. A source with no row, or none since its last
	// restart cleared the stamp, is simply absent — "never completed" and
	// "no poller" both read as a missing key, which a caller renders as
	// unknown rather than as a time.
	LastPollTimes(ctx context.Context, orgID string) (map[string]time.Time, error)

	// TakeAnnouncePending atomically reads-and-clears the one-shot "config
	// took effect" toast flag for (orgID, source) — true at most once per
	// SetAnnouncePending call, even across pods racing this read (the CAS
	// is a single UPDATE ... WHERE announce_pending RETURNING).
	TakeAnnouncePending(ctx context.Context, orgID, source string) (bool, error)

	// SetAnnouncePending arms the one-shot toast flag for (orgID, source).
	// Upserts — safe to call before any row exists.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping, same as
	// MarkPollComplete.
	SetAnnouncePending(ctx context.Context, orgID, source string) error
}
