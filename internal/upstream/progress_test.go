package upstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// TestWithProgress_CalledForEveryRecordedAttempt pins that a progress report
// hears every attempt Record counts under its context, whatever the outcome,
// and nothing recorded outside it.
func TestWithProgress_CalledForEveryRecordedAttempt(t *testing.T) {
	SetMeterProviderForTest(t, sdkmetric.NewMeterProvider())
	n := 0
	ctx := WithProgress(context.Background(), func() { n++ })
	ctx, tally := WithTally(ctx)

	Record(ctx, GitHub, "org-1", OK)
	Record(ctx, GitHub, "org-1", Transient)
	Record(ctx, Jira, "org-1", RateLimited)
	Record(context.Background(), GitHub, "org-1", OK)

	if n != 3 {
		t.Errorf("progress reported %d attempts, want 3", n)
	}
	if got := tally.Attempts(); got != 3 {
		t.Errorf("a progress report displaced the tally: it counted %d attempts, want 3", got)
	}
}

// TestSleep_ReportsProgressWhileItWaits: a client waiting out a rate limit or
// a backoff is still working, so a wait under a progress report reports at
// its start and then at intervals until it ends. A poll cycle waiting minutes
// on a Retry-After would otherwise read as a stuck loop.
func TestSleep_ReportsProgressWhileItWaits(t *testing.T) {
	interval := sleepProgressInterval
	sleepProgressInterval = 10 * time.Millisecond
	t.Cleanup(func() { sleepProgressInterval = interval })

	var n atomic.Int32
	ctx := WithProgress(context.Background(), func() { n.Add(1) })
	if err := Sleep(ctx, 55*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := n.Load(); got < 4 {
		t.Errorf("a 55ms wait at a 10ms interval reported progress %d times, want at least 4", got)
	}
	reported := n.Load()
	time.Sleep(30 * time.Millisecond)
	if got := n.Load(); got != reported {
		t.Errorf("progress was still reported %d times after the wait ended", got-reported)
	}

	if err := Sleep(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("Sleep without a progress report: %v", err)
	}
}
