package agenthost

import (
	"context"
	"maps"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/memoryentities"
)

// linkSkips points memoryentities' skipped-link counter at a manual reader for
// the test and returns a reader of every series it recorded, keyed
// "role/source/reason".
func linkSkips(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	memoryentities.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[string]int64{}
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
					out[role.AsString()+"/"+source.AsString()+"/"+reason.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// TestRecordEntityTouch_CountsALinkItCouldNotWrite: a Jira touch whose read
// named no project finds no entity and cannot create one, so the touch link it
// owed is counted. A touch that resolves, and one that names no entity by
// design (a repo-level GitHub target), count nothing.
func TestRecordEntityTouch_CountsALinkItCouldNotWrite(t *testing.T) {
	skips := linkSkips(t)
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()

	if id := recordEntityTouch(ctx, stores, info, entityCoordinate{provider: domain.ProviderJira, target: "OPS-9", externalID: "10009"}); id != "" {
		t.Fatalf("touch resolved %q, want no entity without the issue's project", id)
	}
	withProject := entityCoordinate{
		provider: domain.ProviderJira, target: "OPS-10", externalID: "10010",
		container: domain.EntityContainer{JiraProject: dbseed.JiraIssueProject("OPS-10")},
	}
	if id := recordEntityTouch(ctx, stores, info, withProject); id == "" {
		t.Fatal("touch carrying its project resolved no entity")
	}
	if id := recordEntityTouch(ctx, stores, info, entityCoordinate{provider: domain.ProviderGitHub, target: "o/r"}); id != "" {
		t.Fatalf("repo-level touch resolved %q, want nothing", id)
	}

	want := map[string]int64{domain.MemoryRoleTouched + "/jira/no_entity": 1}
	if got := skips(); !maps.Equal(got, want) {
		t.Errorf("memory.entity_links_skipped = %v, want %v", got, want)
	}
}
