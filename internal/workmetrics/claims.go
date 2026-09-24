package workmetrics

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// ExpiredClaimSource is what the claim observer needs: the store read that
// counts live claims past their expiry deployment-wide and says how far past
// expiry the oldest is, and the one that says how long the idlest live
// engagement has gone without activity. db.ConversationQueueStore satisfies
// it.
//
// The narrow interface is what keeps this package off the store bundle, the
// same reason DepthSource is narrow.
type ExpiredClaimSource interface {
	ExpiredClaimsSystem(ctx context.Context) (count int, oldestPastExpiry time.Duration, err error)
	OldestIdleClaimSystem(ctx context.Context) (time.Duration, error)
}

// ClaimObserver measures expired claims on a ticker and reports the result
// through observable gauges at scrape time — the same shape, lifecycle and
// reasons as DepthObserver, which its doc carries.
//
// It runs on the control pod's background brain rather than inside any
// dispatcher. A dead engagement is exactly the thing a stuck dispatcher
// cannot report, so the alarm must not be collected by the loop it is about.
type ClaimObserver struct {
	src ExpiredClaimSource

	mu      sync.Mutex
	count   int
	oldest  time.Duration
	reg     metric.Registration
	sampled bool
	// idle is sampled on its own: the two reads fail independently, and a
	// failure of one must not withhold the other's fresh value.
	idle        time.Duration
	idleSampled bool
}

// NewClaimObserver creates the gauges against a provider and registers the
// callback that reports them. As with the depth gauges, a provider that
// cannot build an instrument leaves them unregistered and the ticks still
// run.
func NewClaimObserver(provider metric.MeterProvider, src ExpiredClaimSource) *ClaimObserver {
	c := &ClaimObserver{src: src}
	m := provider.Meter(meterName)

	expired, err := m.Int64ObservableGauge("claims.expired",
		metric.WithDescription("Unreleased executor claims past their lease expiry: engagements nobody is driving."))
	if err != nil {
		log.Error("claim gauge setup failed", "instrument", "claims.expired", "error", err)
		return c
	}
	oldest, err := m.Int64ObservableGauge("claims.oldest_expired_age",
		metric.WithDescription("Seconds past expiry of the oldest unreleased claim."), metric.WithUnit("s"))
	if err != nil {
		log.Error("claim gauge setup failed", "instrument", "claims.oldest_expired_age", "error", err)
		return c
	}

	idle, err := m.Int64ObservableGauge("claims.oldest_idle",
		metric.WithDescription("Seconds since the idlest live engagement last did anything the stall watchdog counts as activity, as its last renewal stamped it."), metric.WithUnit("s"))
	if err != nil {
		log.Error("claim gauge setup failed", "instrument", "claims.oldest_idle", "error", err)
		return c
	}

	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		// Nothing is reported before the first successful measure: a zero
		// this process has not established would read as "no dead
		// engagements", which is the one answer the alert must not be given
		// for free.
		if c.sampled {
			o.ObserveInt64(expired, int64(c.count))
			o.ObserveInt64(oldest, int64(c.oldest.Seconds()))
		}
		if c.idleSampled {
			o.ObserveInt64(idle, int64(c.idle.Seconds()))
		}
		return nil
	}, expired, oldest, idle)
	if err != nil {
		log.Error("claim gauge callback registration failed", "error", err)
		return c
	}
	c.reg = reg
	return c
}

// Close unregisters the gauge callback, so a stopped observer stops
// reporting, and it has done so when it returns. See DepthObserver.Close for
// why the brain calls this itself on demotion rather than relying on Run's
// deferred call. Safe to call more than once, and while Run is still ticking.
func (c *ClaimObserver) Close() {
	c.mu.Lock()
	reg := c.reg
	c.reg = nil
	c.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			log.Error("claim gauge callback unregister failed", "error", err)
		}
	}
}

// Tick measures once. A read failure leaves the previous values in place and
// logs, for DepthObserver.Tick's reason: a zero the pass did not establish is
// worse than a stale number.
func (c *ClaimObserver) Tick(ctx context.Context) {
	if c.src == nil {
		return
	}
	if count, oldest, err := c.src.ExpiredClaimsSystem(ctx); err != nil {
		log.Error("expired-claim measure failed; keeping the previous values", "error", err)
	} else {
		c.mu.Lock()
		c.count, c.oldest, c.sampled = count, oldest, true
		c.mu.Unlock()
	}
	if idle, err := c.src.OldestIdleClaimSystem(ctx); err != nil {
		log.Error("idle-claim measure failed; keeping the previous value", "error", err)
	} else {
		c.mu.Lock()
		c.idle, c.idleSampled = idle, true
		c.mu.Unlock()
	}
}

// Run ticks until ctx is cancelled, then unregisters the gauges for a caller
// that did not already. The first measure happens on the first tick rather
// than at start, matching DepthObserver.Run.
func (c *ClaimObserver) Run(ctx context.Context, interval time.Duration) {
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
