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
	// unpolled is why the cycle deliberately polled nothing: the source is
	// turned off ("disabled"), has no credential or configuration
	// ("unconfigured"), or tracks nothing ("no_repos", "no_armed_projects").
	// Empty for a cycle that meant to poll.
	unpolled string
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

// skip records that the cycle deliberately polled nothing, and why.
func (c *cycleConnection) skip(reason string) {
	c.unpolled = reason
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
// says anything about the connection. recordConnection leaves the stored state
// as it is then, unless the cycle skipped the source on purpose.
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
// (source, orgID) and logs a change of state, or clears the state when the
// cycle skipped the source on purpose. It returns the observed state, or ""
// when the cycle observed none, so the caller can decide whether the cycle
// counts as a successful poll.
func (m *Manager) recordConnection(ctx context.Context, log *slog.Logger, source, orgID string, conn *cycleConnection) db.ConnectionState {
	state, class, observed := conn.outcome()
	// A cycle its caller cut short saw only part of what it meant to send, and
	// its ctx can no longer carry the write.
	if m.connections == nil || ctx.Err() != nil {
		return state
	}
	switch {
	case observed:
		stored, previous, err := m.connections.RecordConnection(ctx, orgID, source, state, string(class))
		if err != nil {
			log.ErrorContext(ctx, "record connection state failed", "org", orgID, "state", state, "error", err)
			return state
		}
		logConnectionChange(ctx, log, source, orgID, previous, stored)
	case conn.unpolled != "":
		m.clearConnection(ctx, log, source, orgID, conn.unpolled)
	}
	return state
}

// clearConnection forgets the state of a source the cycle deliberately did not
// poll. Without it, a connection that was down when an admin turned the source
// off, removed its credential or emptied its tracked set would stay down in the
// log, the gauge and the fleet alert for as long as nobody checks it. A source
// that stays unpolled costs one read per cycle, and a write only on the cycle
// that clears it.
func (m *Manager) clearConnection(ctx context.Context, log *slog.Logger, source, orgID, reason string) {
	current, err := m.connections.Connection(ctx, orgID, source)
	if err != nil {
		log.ErrorContext(ctx, "read connection state failed", "org", orgID, "error", err)
		return
	}
	if current.State == db.ConnectionUnknown {
		return
	}
	_, previous, err := m.connections.RecordConnection(ctx, orgID, source, db.ConnectionUnknown, "")
	if err != nil {
		log.ErrorContext(ctx, "clear connection state failed", "org", orgID, "error", err)
		return
	}
	// Info after a down state, because it closes the outage the WARN opened
	// in place of the restored line that will not come.
	level := slog.LevelDebug
	if previous.State == db.ConnectionDown {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, source+" connection no longer checked", "org", orgID, "reason", reason, "previous_state", previous.State)
}

// logConnectionChange is the logging contract for a source's connection: one
// Warn when it is lost, one Info when it is restored, and nothing while it
// stays where it was. The transient and auth failures in between log at Debug
// at their own sites (upstream.LogLevel).
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
