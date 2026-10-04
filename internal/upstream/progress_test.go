package upstream

import (
	"context"
	"testing"

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
