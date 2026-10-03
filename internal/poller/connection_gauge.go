package poller

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// meterName is the instrumentation scope every instrument here is created
// under.
const meterName = "internal/poller"

// DefaultConnectionInterval is how often the connection observer reads the
// stored connection states. The states change at most once per poll cycle,
// and the read is one small query, kept off the scrape path for the reason
// workmetrics.DefaultDepthInterval gives.
const DefaultConnectionInterval = 15 * time.Second

// ConnectionSource is what the connection observer reads: every recorded
// connection state of every active org. db.PollReadinessStore satisfies it.
type ConnectionSource interface {
	ListConnectionStatuses(ctx context.Context) ([]db.ConnectionStatus, error)
}

// ConnectionObserver exports each (source, org)'s stored connection state as
// tf_upstream_up: 1 when up, 0 when down, and no series while unknown. It
// reads the stored states on a ticker and reports them through an observable
// gauge at scrape time, the same shape and lifecycle as
// workmetrics.DepthObserver.
//
// It runs on the background brain, so exactly one process exports the
// gauge: the states are deployment-wide rows, and two pods reporting them
// would double every count an alert takes.
type ConnectionObserver struct {
	src ConnectionSource

	mu  sync.Mutex
	up  map[connectionKey]bool
	reg metric.Registration
}

type connectionKey struct{ source, orgID string }

// NewConnectionObserver creates the gauge against a provider and registers
// the callback that reports it. An instrument-creation error can only be a
// programmer error (an invalid name); the observer then reports nothing and
// its ticks still run.
func NewConnectionObserver(provider metric.MeterProvider, src ConnectionSource) *ConnectionObserver {
	c := &ConnectionObserver{src: src, up: map[connectionKey]bool{}}
	m := provider.Meter(meterName)
	gauge, err := m.Int64ObservableGauge("upstream.up",
		metric.WithDescription("Whether the org's last poll cycle that made requests reached the upstream: 1 up, 0 down. No series until a cycle has recorded a state."))
	if err != nil {
		pollerLog.Error("connection gauge setup failed", "instrument", "upstream.up", "error", err)
		return c
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for k, up := range c.up {
			v := int64(0)
			if up {
				v = 1
			}
			o.ObserveInt64(gauge, v, metric.WithAttributes(telemetry.Upstream(k.source), telemetry.OrgID(k.orgID)))
		}
		return nil
	}, gauge)
	if err != nil {
		pollerLog.Error("connection gauge callback registration failed", "error", err)
		return c
	}
	c.reg = reg
	return c
}

// Close unregisters the gauge callback, so a stopped observer stops reporting,
// and it has done so when it returns. See workmetrics.DepthObserver.Close for
// why the brain calls this itself on demotion. Safe to call more than once,
// and while Run is still ticking.
func (c *ConnectionObserver) Close() {
	c.mu.Lock()
	reg := c.reg
	c.reg = nil
	c.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			pollerLog.Error("connection gauge callback unregister failed", "error", err)
		}
	}
}

// Tick reads the stored states once. The result replaces the previous one
// whole, so an org the read no longer lists (a deleted one) stops reporting
// rather than freezing at its last value. A
// read failure keeps the previous values and logs, because a value this pass
// did not establish is worse than a stale one.
func (c *ConnectionObserver) Tick(ctx context.Context) {
	if c.src == nil {
		return
	}
	statuses, err := c.src.ListConnectionStatuses(ctx)
	if err != nil {
		pollerLog.Error("connection state read failed; keeping the previous values", "error", err)
		return
	}
	up := make(map[connectionKey]bool, len(statuses))
	for _, st := range statuses {
		switch st.State {
		case db.ConnectionUp:
			up[connectionKey{st.Source, st.OrgID}] = true
		case db.ConnectionDown:
			up[connectionKey{st.Source, st.OrgID}] = false
		}
	}
	c.mu.Lock()
	c.up = up
	c.mu.Unlock()
}

// Run ticks until ctx is cancelled, then unregisters the gauge for a caller
// that did not already. The first read happens on the first tick rather than
// at start, so a freshly promoted brain does not query inside the promotion
// callback.
func (c *ConnectionObserver) Run(ctx context.Context, interval time.Duration) {
	defer c.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Tick(ctx)
		}
	}
}
