package workmetrics

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// fakeExpiredClaims is the store read as the claim observer sees it.
type fakeExpiredClaims struct {
	count  int
	oldest time.Duration
	err    error
	calls  int
}

func (f *fakeExpiredClaims) ExpiredClaimsSystem(context.Context) (int, time.Duration, error) {
	f.calls++
	return f.count, f.oldest, f.err
}

// TestClaimObserver_ReportsCountsAndKeepsThemAcrossAFailure covers the whole
// contract: nothing before the first measure, both gauges after it, and a
// read failure keeping the last values rather than publishing a zero the pass
// did not establish — which is the one answer an alert on a dead engagement
// must never get for free.
func TestClaimObserver_ReportsCountsAndKeepsThemAcrossAFailure(t *testing.T) {
	src := &fakeExpiredClaims{}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	c := NewClaimObserver(provider, src)
	defer c.Close()

	ctx := context.Background()
	if _, ok := gaugeWith(t, collect(t, reader), "claims.expired"); ok {
		t.Error("claims.expired reported before any measure")
	}

	src.count, src.oldest = 3, 90*time.Second
	c.Tick(ctx)
	ms := collect(t, reader)
	if got, ok := gaugeWith(t, ms, "claims.expired"); !ok || got != 3 {
		t.Errorf("claims.expired = %d (present=%v), want 3", got, ok)
	}
	if got, ok := gaugeWith(t, ms, "claims.oldest_expired_age"); !ok || got != 90 {
		t.Errorf("claims.oldest_expired_age = %d (present=%v), want 90", got, ok)
	}

	src.err = errors.New("database unreachable")
	src.count, src.oldest = 0, 0
	c.Tick(ctx)
	ms = collect(t, reader)
	if got, _ := gaugeWith(t, ms, "claims.expired"); got != 3 {
		t.Errorf("claims.expired = %d after a failed read, want the previous 3 kept", got)
	}

	src.err = nil
	c.Tick(ctx)
	if got, _ := gaugeWith(t, collect(t, reader), "claims.expired"); got != 0 {
		t.Errorf("claims.expired = %d after a clean read of zero, want 0", got)
	}

	// A closed observer stops reporting, so a re-acquired brain's new
	// observer never doubles up with this one's series.
	c.Close()
	if _, ok := gaugeWith(t, collect(t, reader), "claims.expired"); ok {
		t.Error("a closed observer is still reporting")
	}
}

// TestClaimObserver_CloseWhileRunningStopsReporting is the same-pod flap the
// brain drives: demotion closes the observer before its goroutine has noticed
// the cancellation, and re-acquisition registers a new one while the old
// goroutine is still on its way out. The old callback must already be gone
// when Close returns, or the two report the same series together and the
// alert reads whichever point the exporter happened to keep.
func TestClaimObserver_CloseWhileRunningStopsReporting(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	old := NewClaimObserver(provider, &fakeExpiredClaims{count: 3})
	old.Tick(context.Background())

	// The old goroutine keeps running: nothing cancels it until the end. The
	// interval is long enough that it never ticks, so the fake stays the
	// test's alone.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { old.Run(ctx, time.Hour); close(done) }()
	old.Close()

	fresh := NewClaimObserver(provider, &fakeExpiredClaims{count: 1})
	defer fresh.Close()
	fresh.Tick(context.Background())

	if got, ok := gaugeWith(t, collect(t, reader), "claims.expired"); !ok || got != 1 {
		t.Errorf("claims.expired = %d (present=%v) with the old observer closed but still running, want the fresh observer's 1 alone", got, ok)
	}

	cancel()
	<-done
}
