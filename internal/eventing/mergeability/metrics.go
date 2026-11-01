package mergeability

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// meterName is the instrumentation scope the worker's instruments are created
// under.
const meterName = "internal/eventing/mergeability"

var stats atomic.Pointer[instruments]

func init() {
	stats.Store(newInstruments(otel.GetMeterProvider()))
}

// instruments are the probe worker's counters. Every label value but the
// dropped counter's org_id is a closed vocabulary: an admission reason, one of
// GitHub's three answers, or a drop outcome. The drops carry the org so a
// probe failure rate per org can be read against the work-item counters,
// which carry it too.
type instruments struct {
	probes        metric.Int64Counter
	answers       metric.Int64Counter
	unknownGiveUp metric.Int64Counter
	sweepChanges  metric.Int64Counter
	dropped       metric.Int64Counter
}

// newInstruments builds the counters against mp; production passes the global
// provider, which telemetry.Init installs. A creation error can only be an
// invalid name, and the API hands back a usable no-op beside it, so it is
// logged rather than propagated.
func newInstruments(mp metric.MeterProvider) *instruments {
	m := mp.Meter(meterName)
	in := &instruments{}
	mk := func(dst *metric.Int64Counter, name, desc string) {
		c, err := m.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			log.Warn("mergeability counter setup failed", "instrument", name, "error", err)
		}
		*dst = c
	}
	mk(&in.probes, "mergeability.probes", "Pull requests whose mergeability the probe worker asked GitHub for, by the reason the probe was admitted.")
	mk(&in.answers, "mergeability.answers", "Mergeability answers GitHub gave the probe worker, by value: MERGEABLE, CONFLICTING or UNKNOWN.")
	mk(&in.unknownGiveUp, "mergeability.unknown_giveups", "Pull requests left to the sweep after GitHub answered UNKNOWN on every retry.")
	mk(&in.sweepChanges, "mergeability.sweep_changes", "Sweep reads that changed a pull request's known mergeability: a change nothing else probed for.")
	mk(&in.dropped, "mergeability.dropped", "Probes completed without a read, by org and outcome: the failure that ended their last attempt, or no_credential when no credential the org holds reaches their repository. The poll queues the pull request again.")
	return in
}

func recordProbe(reason string) {
	if s := stats.Load(); s != nil {
		s.probes.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func recordAnswer(value string) {
	if s := stats.Load(); s != nil {
		s.answers.Add(context.Background(), 1, metric.WithAttributes(attribute.String("value", value)))
	}
}

func recordUnknownGiveUp() {
	if s := stats.Load(); s != nil {
		s.unknownGiveUp.Add(context.Background(), 1)
	}
}

func recordSweepChange() {
	if s := stats.Load(); s != nil {
		s.sweepChanges.Add(context.Background(), 1)
	}
}

func recordDrop(orgID, outcome string) {
	if s := stats.Load(); s != nil {
		s.dropped.Add(context.Background(), 1, metric.WithAttributes(telemetry.OrgID(orgID), attribute.String("outcome", outcome)))
	}
}
