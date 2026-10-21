package memoryentities

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// skipKey is one series of memory.entity_links_skipped.
type skipKey struct{ role, source, reason string }

// linkSkips points the skipped-link counter at a manual reader for the test
// and returns a reader of every series it recorded.
func linkSkips(t *testing.T) func() map[skipKey]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return func() map[skipKey]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[skipKey]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "memory.entity_links_skipped" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("memory.entity_links_skipped is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					role, _ := dp.Attributes.Value(attribute.Key("role"))
					source, _ := dp.Attributes.Value(attribute.Key("source"))
					reason, _ := dp.Attributes.Value(attribute.Key("reason"))
					out[skipKey{role.AsString(), source.AsString(), reason.AsString()}] += dp.Value
				}
			}
		}
		return out
	}
}

// assertSkips checks every series the counter recorded against want, so a
// skip a test did not expect is caught as well as a missing one.
func assertSkips(t *testing.T, skips func() map[skipKey]int64, want map[skipKey]int64) {
	t.Helper()
	got := skips()
	if !maps.Equal(got, want) {
		t.Errorf("memory.entity_links_skipped = %v, want %v", got, want)
	}
}

// TestSkipReason: a resolve that answers with no entity, or that refuses to
// create a Linear or Jira entity without its team or project, is a missing
// entity; anything else is an error.
func TestSkipReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, skipNoEntity},
		{fmt.Errorf("create: %w", db.ErrEntityJiraProjectRequired), skipNoEntity},
		{fmt.Errorf("create: %w", db.ErrEntityLinearTeamRequired), skipNoEntity},
		{errors.New("connection reset"), skipError},
	} {
		if got := skipReason(tc.err); got != tc.want {
			t.Errorf("skipReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
