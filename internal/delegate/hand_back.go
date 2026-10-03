package delegate

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// handBacks owns this package's hand-back counter. Package-level because a
// metric instrument is process-global by nature, and swapped atomically so a
// test can back it with a manual reader while an engagement may still be
// counting.
var handBacks atomic.Pointer[handBackStats]

func init() {
	handBacks.Store(newHandBackStats(otel.GetMeterProvider()))
}

type handBackStats struct {
	handedBack metric.Int64Counter
}

// newHandBackStats builds the instrument against mp — production passes the
// global provider, which telemetry.Init installs. An instrument-creation error
// can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newHandBackStats(mp metric.MeterProvider) *handBackStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("conversations.handed_back",
		metric.WithDescription("Claims an engagement or its executor handed back with the conversation still mid-flight, by hand-back outcome."))
	if err != nil {
		dispatchLog.Warn("hand-back counter setup failed", "error", err)
	}
	return &handBackStats{handedBack: counter}
}

// recordHandBack counts n claims of orgID released with outcome, one of the
// db.HandBackPolicies outcomes this package writes. Called after the write
// committed, so a refused or failed hand-back counts nothing.
func recordHandBack(orgID, outcome string, n int) {
	stats := handBacks.Load()
	if stats == nil || stats.handedBack == nil || n <= 0 {
		return
	}
	stats.handedBack.Add(context.Background(), int64(n),
		metric.WithAttributes(attribute.String("outcome", outcome), telemetry.OrgID(orgID)))
}
