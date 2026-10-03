package db

import (
	"context"
	"fmt"
	"time"
)

// PollReadinessStore owns the poll_readiness table — the org-scoped,
// DB-backed replacement for two flags that used to be process-local state
// on whichever pod happened to run the poller: the /api/jira/stock
// readiness gate and the one-shot "config took effect" announce toast
// (TFAC-583). Under the control/standby split the pod serving a given API
// request is not necessarily the pod running the poller, so both flags
// have to live somewhere every control pod's API can read — this store.
// It also holds each (org, source)'s connection state, which the poller
// writes at the end of every cycle and the connection gauge reads.
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

	// RecordConnection stores the connection state a poll cycle observed for
	// (orgID, source) and returns the row as stored together with the row it
	// replaced (an absent row reads as ConnectionUnknown with no ChangedAt).
	// ChangedAt moves only when the state changes; FailureClass is stored
	// only while the state is ConnectionDown and cleared otherwise.
	// Recording ConnectionUnknown clears the state: ChangedAt goes back to
	// nil, for a source the poller has stopped checking. The pair is
	// validated by ValidateConnection. The previous row is read and the new
	// one written in one transaction, with the read holding the row against a
	// concurrent writer, and the stored row comes from the write's RETURNING.
	// Upserts.
	//
	// Only the background-brain holder polls, so each row has one writer
	// outside a lease handover, when a demoted holder's in-flight cycle can
	// still finish beside its successor's first one.
	RecordConnection(ctx context.Context, orgID, source string, state ConnectionState, failureClass string) (stored ConnectionStatus, previous ConnectionStatus, err error)

	// Connection returns the connection status of (orgID, source). A pair no
	// cycle has recorded a state for reads as ConnectionUnknown with no
	// ChangedAt, not as an error.
	Connection(ctx context.Context, orgID, source string) (ConnectionStatus, error)

	// ListConnectionStatuses returns every recorded connection status (a
	// state other than ConnectionUnknown) of every active org, ordered by
	// org then source. Deployment-wide, for the system job that exports the
	// connection gauge.
	ListConnectionStatuses(ctx context.Context) ([]ConnectionStatus, error)
}

// ConnectionState is the closed vocabulary of a source's connection state.
type ConnectionState string

const (
	// ConnectionUnknown is the state before any poll cycle that made
	// requests has recorded one.
	ConnectionUnknown ConnectionState = "unknown"
	// ConnectionUp means the last cycle that made requests got an answer
	// from the upstream, whatever the answer was.
	ConnectionUp ConnectionState = "up"
	// ConnectionDown means the last cycle that made requests got none: every
	// request failed in transit or was refused for its credential.
	ConnectionDown ConnectionState = "down"
)

// The failure classes a down connection records: the upstream request outcome
// classes (internal/upstream) that put a connection down.
const (
	ConnectionFailureTransient = "transient"
	ConnectionFailureAuth      = "auth"
)

// ValidateConnection reports whether (state, failureClass) may be recorded:
// a state in the vocabulary and, for ConnectionDown, one of the failure
// classes. A class beside any other state is ignored, because it is cleared.
// The columns are app-validated rather than CHECK-constrained, like the other
// vocabulary text columns in both dialects, so this is the only gate.
func ValidateConnection(state ConnectionState, failureClass string) error {
	switch state {
	case ConnectionUnknown, ConnectionUp:
		return nil
	case ConnectionDown:
		switch failureClass {
		case ConnectionFailureTransient, ConnectionFailureAuth:
			return nil
		}
		return fmt.Errorf("poll readiness: a down connection needs a failure class of %q or %q, got %q",
			ConnectionFailureTransient, ConnectionFailureAuth, failureClass)
	}
	return fmt.Errorf("poll readiness: unknown connection state %q", state)
}

// ConnectionStatus is one (org, source)'s connection state as stored.
type ConnectionStatus struct {
	OrgID  string
	Source string
	State  ConnectionState
	// ChangedAt is when State began; nil while State is ConnectionUnknown.
	ChangedAt *time.Time
	// FailureClass is the upstream request outcome class that put the
	// connection down (ConnectionFailureTransient or ConnectionFailureAuth);
	// empty unless State is ConnectionDown.
	FailureClass string
}
