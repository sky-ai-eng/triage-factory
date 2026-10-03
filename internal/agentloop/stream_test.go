package agentloop

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// upstreamCounts reads the request and retry counters into
// "<instrument> <upstream>/<org>/<outcome>" → value.
func upstreamCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	attr := func(set attribute.Set, kv attribute.KeyValue) string {
		v, _ := set.Value(kv.Key)
		return v.AsString()
	}
	out := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				key := m.Name + " " + attr(dp.Attributes, telemetry.Upstream("")) + "/" +
					attr(dp.Attributes, telemetry.OrgID("")) + "/" + attr(dp.Attributes, telemetry.Outcome(""))
				out[key] += dp.Value
			}
		}
	}
	return out
}

func meterForTest(t *testing.T) *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	upstream.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

func assertCounts(t *testing.T, got, want map[string]int64) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected %s = %d", k, v)
		}
	}
}

// TestStreamWithRetry_RecordsEachAttemptAndEachRetry: every attempt counts
// once under its own outcome, and every retry the loop decides on counts once
// under the outcome that caused it.
func TestStreamWithRetry_RecordsEachAttemptAndEachRetry(t *testing.T) {
	reader := meterForTest(t)
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: errors.New("inference: provider error: Overloaded [overloaded_error]")},
		{err: errors.New("inference: provider error: rate limited (HTTP 429)")},
		{text: "done"},
	}}
	e := newTestEngine(newMemTranscript(), p, newScriptedToolHost())

	req := inference.Request{Provider: inference.ProviderAnthropic, Model: "claude-sonnet-4-5"}
	if _, err := e.streamWithRetry(context.Background(), "org-1", p, req); err != nil {
		t.Fatalf("streamWithRetry: %v", err)
	}
	assertCounts(t, upstreamCounts(t, reader), map[string]int64{
		"upstream.requests anthropic/org-1/transient":    1,
		"upstream.requests anthropic/org-1/rate_limited": 1,
		"upstream.requests anthropic/org-1/ok":           1,
		"upstream.retries anthropic/org-1/transient":     1,
		"upstream.retries anthropic/org-1/rate_limited":  1,
	})
}

// TestStreamWithRetry_ExhaustionCountsNoRetryForTheLastAttempt: the attempt
// that exhausts the budget is a request, not a retry.
func TestStreamWithRetry_ExhaustionCountsNoRetryForTheLastAttempt(t *testing.T) {
	reader := meterForTest(t)
	failure := scriptedTurn{err: errors.New("inference: provider error: Service Unavailable (HTTP 503)")}
	p := &scriptedProvider{turns: []scriptedTurn{failure, failure, failure}}
	e := newTestEngine(newMemTranscript(), p, newScriptedToolHost())
	e.Retry.MaxAttempts = 3

	req := inference.Request{Provider: inference.ProviderBedrock, Model: "m"}
	if _, err := e.streamWithRetry(context.Background(), "org-1", p, req); err == nil {
		t.Fatal("expected the last error once the attempts ran out")
	}
	assertCounts(t, upstreamCounts(t, reader), map[string]int64{
		"upstream.requests bedrock/org-1/transient": 3,
		"upstream.retries bedrock/org-1/transient":  2,
	})
}

// TestStreamWithRetry_RejectedIsCountedAndNotRetried covers the classes the
// loop does not retry: one attempt, one request, no retry.
func TestStreamWithRetry_RejectedIsCountedAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		err     string
		outcome string
	}{
		{"inference: provider error: invalid x-api-key (HTTP 401)", "auth"},
		{"inference: provider error: max_tokens: field required (HTTP 400)", "rejected"},
		{"inference: request has no model", "rejected"},
	} {
		t.Run(tc.outcome+" "+tc.err, func(t *testing.T) {
			reader := meterForTest(t)
			p := &scriptedProvider{turns: []scriptedTurn{{err: errors.New(tc.err)}}}
			e := newTestEngine(newMemTranscript(), p, newScriptedToolHost())

			req := inference.Request{Provider: inference.ProviderAnthropic, Model: "m"}
			if _, err := e.streamWithRetry(context.Background(), "org-1", p, req); err == nil {
				t.Fatal("expected the error back")
			}
			if p.calls != 1 {
				t.Errorf("attempts = %d, want 1", p.calls)
			}
			assertCounts(t, upstreamCounts(t, reader), map[string]int64{
				"upstream.requests anthropic/org-1/" + tc.outcome: 1,
			})
		})
	}
}

// TestStreamWithRetry_NothingCountedWithoutAnOutcome: a provider outside the
// closed label vocabulary records nothing, and neither does an attempt the
// caller's own cancellation ended.
func TestStreamWithRetry_NothingCountedWithoutAnOutcome(t *testing.T) {
	t.Run("unlabeled provider", func(t *testing.T) {
		reader := meterForTest(t)
		p := &scriptedProvider{turns: []scriptedTurn{
			{err: errors.New("inference: provider error: Overloaded [overloaded_error]")},
			{text: "done"},
		}}
		e := newTestEngine(newMemTranscript(), p, newScriptedToolHost())

		req := inference.Request{Provider: schemas.OpenAI, Model: "m"}
		if _, err := e.streamWithRetry(context.Background(), "org-1", p, req); err != nil {
			t.Fatalf("streamWithRetry: %v", err)
		}
		if p.calls != 2 {
			t.Errorf("attempts = %d, want 2: an unlabeled provider is still retried", p.calls)
		}
		assertCounts(t, upstreamCounts(t, reader), map[string]int64{})
	})

	t.Run("cancelled mid-attempt", func(t *testing.T) {
		reader := meterForTest(t)
		ctx, cancel := context.WithCancel(context.Background())
		p := &scriptedProvider{turns: []scriptedTurn{{
			err:    errors.New("inference: provider error: context canceled"),
			onCall: cancel,
		}}}
		e := newTestEngine(newMemTranscript(), p, newScriptedToolHost())

		req := inference.Request{Provider: inference.ProviderAnthropic, Model: "m"}
		if _, err := e.streamWithRetry(ctx, "org-1", p, req); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		assertCounts(t, upstreamCounts(t, reader), map[string]int64{})
	})
}
