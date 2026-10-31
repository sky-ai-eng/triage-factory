package notify

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var stats atomic.Pointer[counters]

func init() {
	stats.Store(newCounters(otel.GetMeterProvider()))
}

type counters struct {
	writeFailures metric.Int64Counter
}

// newCounters builds the instrument against mp; production passes the global
// provider, which telemetry.Init installs. A creation error can only be an
// invalid name, and the API hands back a usable no-op beside it, so it is
// logged rather than propagated.
func newCounters(mp metric.MeterProvider) *counters {
	c, err := mp.Meter("internal/notify").Int64Counter("notify.write_failures",
		metric.WithDescription("Notification verb calls that wrote nothing because an audience, a write or the kind itself failed, by kind."))
	if err != nil {
		notifyLog.Warn("notification failure counter setup failed", "error", err)
	}
	return &counters{writeFailures: c}
}

func recordWriteFailure(kind string) {
	s := stats.Load()
	if s == nil || s.writeFailures == nil {
		return
	}
	s.writeFailures.Add(context.Background(), 1, metric.WithAttributes(telemetry.NotificationKind(kind)))
}
