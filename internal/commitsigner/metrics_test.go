package commitsigner

import (
	"context"
	"maps"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestCountCheck keeps GitHub's named reasons and TF's own results as they are
// and folds any other reason into other, so the label set stays bounded.
func TestCountCheck(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	ctx := context.Background()

	for _, result := range []string{"valid", "unknown_key", "no_user", "unverified_email", CheckResultKeyMissing,
		CheckResultError, "expired_key", "bad_email", "valid"} {
		CountCheck(ctx, CheckVerify, result)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "commit_signer.checks" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("commit_signer.checks is %T, want an int64 sum", m.Data)
			}
			for _, dp := range sum.DataPoints {
				check, _ := dp.Attributes.Value(attribute.Key("check"))
				result, _ := dp.Attributes.Value(attribute.Key("result"))
				got[check.AsString()+" "+result.AsString()] += dp.Value
			}
		}
	}
	want := map[string]int64{
		"verify valid": 2, "verify unknown_key": 1, "verify no_user": 1, "verify unverified_email": 1,
		"verify key_missing": 1, "verify error": 1, "verify other": 2,
	}
	if !maps.Equal(got, want) {
		t.Errorf("commit_signer.checks = %v, want %v", got, want)
	}
}
