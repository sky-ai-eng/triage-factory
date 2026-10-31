package memoryentities

import (
	"context"
	"errors"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// The reasons CountSkippedLink records. A link nothing owed — an object in a
// scope the org has left, a key that names no provider id, a target that maps
// to no entity, a source the org has not configured — is never counted: there
// was nothing to link.
const (
	// skipNoEntity: no entity carries the object, and the writer cannot create
	// one. A Linear or Jira entity is created only with its team or project,
	// which a deliverable does not record and a touch has only when the response
	// it read named it.
	skipNoEntity = "no_entity"
	// skipError: resolving the entity or its scope, or writing the link,
	// failed.
	skipError = "error"
)

// linkStats owns this package's skip counter. Package-level because a metric
// instrument is process-global by nature, and swapped atomically so a test can
// back it with a manual reader.
var linkStats atomic.Pointer[stats]

func init() {
	linkStats.Store(newStats(otel.GetMeterProvider()))
}

type stats struct {
	skipped metric.Int64Counter
}

// newStats builds the instrument against mp — production passes the global
// provider, which telemetry.Init installs. An instrument-creation error can
// only be a programmer error, and the API hands back a usable no-op alongside
// it, so it is logged rather than propagated.
func newStats(mp metric.MeterProvider) *stats {
	counter, err := mp.Meter("internal/memoryentities").Int64Counter("memory.entity_links_skipped",
		metric.WithDescription("Conversation-to-entity memory links a writer owed and did not write, by role, source and reason."))
	if err != nil {
		attachLog.Warn("memory link skip counter setup failed", "error", err)
	}
	return &stats{skipped: counter}
}

// CountSkippedLink counts one link of role (domain.MemoryRoleTouched or
// domain.MemoryRoleProduced) between orgID's conversation and an entity of
// source that its writer owed and did not write. err is why: nil when the
// resolve answered with no entity, else the resolve's or the write's error.
// Call it only where a link was owed; the skips a writer makes by design are
// not counted.
func CountSkippedLink(ctx context.Context, orgID, role, source string, err error) {
	s := linkStats.Load()
	if s == nil || s.skipped == nil {
		return
	}
	s.skipped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("role", role), telemetry.Source(source), telemetry.Reason(skipReason(err)), telemetry.OrgID(orgID)))
}

func skipReason(err error) string {
	if err == nil || errors.Is(err, db.ErrEntityJiraProjectRequired) || errors.Is(err, db.ErrEntityLinearTeamRequired) {
		return skipNoEntity
	}
	return skipError
}
