package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// cycleConnection is what one org's poll cycle learned about its connection to
// a source: the outcome of every request the cycle made, from the clients'
// own count (upstream.Tally), plus the failures of calls whose requests no
// client counts.
type cycleConnection struct {
	tally *upstream.Tally
	// noted holds failures of calls made through internal/githubapp — the
	// App's installation-token mint behind ClientFor, and the installation
	// listing behind ReconcileGrant — which reach the same host and are not
	// counted by it. A mint that fails while the host is unreachable is
	// otherwise a cycle that made no counted request, which records nothing.
	noted map[upstream.Class]int
}

// newCycleConnection starts the count for one org's cycle. Every request made
// under the returned ctx is counted, including inside the tracker's
// discovery fan-out.
func newCycleConnection(ctx context.Context) (context.Context, *cycleConnection) {
	ctx, tally := upstream.WithTally(ctx)
	return ctx, &cycleConnection{tally: tally, noted: map[upstream.Class]int{}}
}

// note folds in the failure of a call whose requests no client counted. An
// error with no upstream class (a missing credential, a store read) says
// nothing about the connection and is ignored.
func (c *cycleConnection) note(err error) {
	if class, ok := upstream.ClassOf(err); ok {
		c.noted[class]++
	}
}

func (c *cycleConnection) count(class upstream.Class) int {
	return c.tally.Count(class) + c.noted[class]
}

// outcome is the connection state the cycle observed. Any answer from the
// upstream, a rejection included, means the connection is up: a 404 is an
// answer about the request, not about the connection. Otherwise an auth
// refusal outranks a transient failure, because it is the one a person has to
// fix. observed is false when the cycle made no request, or met only rate
// limits, which are a handled outcome with their own resume cursor; neither
// says anything about the connection, so the stored state is left as it is.
func (c *cycleConnection) outcome() (state db.ConnectionState, class upstream.Class, observed bool) {
	switch {
	case c.count(upstream.OK) > 0 || c.count(upstream.Rejected) > 0:
		return db.ConnectionUp, "", true
	case c.count(upstream.Auth) > 0:
		return db.ConnectionDown, upstream.Auth, true
	case c.count(upstream.Transient) > 0:
		return db.ConnectionDown, upstream.Transient, true
	}
	return "", "", false
}

// recordConnection stores the connection state the cycle observed for
// (source, orgID) and logs a change of state. It returns the observed state,
// or "" when the cycle observed none, so the caller can decide whether the
// cycle counts as a successful poll.
func (m *Manager) recordConnection(ctx context.Context, log *slog.Logger, source, orgID string, conn *cycleConnection) db.ConnectionState {
	state, class, observed := conn.outcome()
	if !observed || m.connections == nil {
		return state
	}
	stored, previous, err := m.connections.RecordConnection(ctx, orgID, source, state, string(class))
	if err != nil {
		log.ErrorContext(ctx, "record connection state failed", "org", orgID, "state", state, "error", err)
		return state
	}
	logConnectionChange(ctx, log, source, orgID, previous, stored)
	return state
}

// logConnectionChange is the logging contract for a source's connection: one
// Warn when it is lost, one Info when it is restored, and nothing while it
// stays where it was. The upstream failures in between log at Debug at their
// own sites (upstream.LogLevel).
func logConnectionChange(ctx context.Context, log *slog.Logger, source, orgID string, previous, current db.ConnectionStatus) {
	switch {
	case current.State == db.ConnectionDown && previous.State != db.ConnectionDown:
		log.WarnContext(ctx, source+" connection lost", "org", orgID, "class", current.FailureClass)
	case current.State == db.ConnectionDown && previous.FailureClass != current.FailureClass:
		// Still down, for another reason: a host that became reachable again
		// and now refuses the credential needs a different fix than the
		// outage the first line reported.
		log.WarnContext(ctx, source+" connection still down, failure changed",
			"org", orgID, "class", current.FailureClass, "previous_class", previous.FailureClass)
	case current.State == db.ConnectionUp && previous.State == db.ConnectionDown:
		attrs := []any{"org", orgID}
		if previous.ChangedAt != nil && current.ChangedAt != nil {
			attrs = append(attrs, "down_for", current.ChangedAt.Sub(*previous.ChangedAt).Round(time.Second))
		}
		log.InfoContext(ctx, source+" connection restored", attrs...)
	case current.State == db.ConnectionUp && previous.State == db.ConnectionUnknown:
		log.DebugContext(ctx, source+" connection up", "org", orgID)
	}
}
