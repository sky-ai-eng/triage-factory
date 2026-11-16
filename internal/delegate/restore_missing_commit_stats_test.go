package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// restoreMissingCommitCounter swaps the counter for one backed by a manual
// reader and returns a read of every kind's series that exists.
func restoreMissingCommitCounter(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := restoreMissingCommits.Swap(newRestoreMissingCommitStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { restoreMissingCommits.Store(prev) })
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		got := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "workspace.restore_missing_commits" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("workspace.restore_missing_commits is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if dp.Attributes.Len() != 1 {
						t.Errorf("series labels = %v, want kind alone", dp.Attributes.ToSlice())
					}
					v, _ := dp.Attributes.Value(attribute.Key("kind"))
					got[v.AsString()] = dp.Value
				}
			}
		}
		return got
	}
}

// Executor startup creates both kinds' series at zero, so the first restore
// that cannot obtain a commit after a restart is a change increase() can see.
func TestOpenRestoreMissingCommitSeries_CreatesEachKindAtZero(t *testing.T) {
	read := restoreMissingCommitCounter(t)

	OpenRestoreMissingCommitSeries()

	got := read()
	if len(got) != 2 {
		t.Errorf("series = %v, want exactly head and prerequisite", got)
	}
	for _, kind := range []string{"head", "prerequisite"} {
		if v, ok := got[kind]; !ok || v != 0 {
			t.Errorf("workspace.restore_missing_commits{kind=%q} = (%d, present %v), want present at 0", kind, v, ok)
		}
	}
}

// A restore whose checkout fails on a captured head the git host will not
// send counts that checkout once, under the head kind alone.
func TestEnsureWorkspace_RestoreMissingAHeadIsCounted(t *testing.T) {
	f := newSnapshotFixture(t, "task-missing-head")
	f.addCheckout(t, "acme/docs", "ref-main")
	f.snapshot(t, domain.ConversationRuntimeNative)
	f.loseRoot(t)
	read := restoreMissingCommitCounter(t)
	OpenRestoreMissingCommitSeries()

	restore := restoreCheckout
	restoreCheckout = func(_ context.Context, r worktree.CheckoutRestore) (worktree.RestoredCheckout, error) {
		return worktree.RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: %w", r.Owner, r.Repo, r.Slug,
			&worktree.MissingCommitError{Kind: worktree.MissingHead, OID: strings.Repeat("ab", 20), Err: errors.New("upload-pack: not our ref")})
	}
	t.Cleanup(func() { restoreCheckout = restore })

	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), failingFreshBuilder(t)); err == nil {
		t.Fatal("ensureWorkspace succeeded with a checkout whose head could not be obtained")
	}
	if got := read(); got["head"] != 1 || got["prerequisite"] != 0 {
		t.Errorf("workspace.restore_missing_commits = %v, want head 1 and prerequisite 0", got)
	}
}
