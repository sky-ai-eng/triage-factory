package marketplacestats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeStore is a narrow db.MarketplaceStore test double: it embeds the nil
// interface so it satisfies db.MarketplaceStore without implementing every
// method, and overrides only RecomputeStatsSystem, the sole method
// Aggregator calls. Any other method would nil-panic if exercised, which
// these tests never do.
type fakeStore struct {
	db.MarketplaceStore

	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeStore) RecomputeStatsSystem(ctx context.Context, orgID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, orgID)
	f.mu.Unlock()
	return f.err
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestAggregator_RunOrg_RecomputesOnFirstCall pins that a never-before-seen
// org always recomputes — there's no stale in-memory entry to skip on.
func TestAggregator_RunOrg_RecomputesOnFirstCall(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false on first call for a never-seen org; want true")
	}
	if got := store.callCount(); got != 1 {
		t.Errorf("store called %d times; want 1", got)
	}
}

// TestAggregator_RunOrg_TTLSkipsWithinWindow pins the whole point of the
// TTL gate: a second call for the same org within ttl is a no-op, so a
// poll-cycle-triggered subscriber doesn't hammer the DB every cycle.
func TestAggregator_RunOrg_TTLSkipsWithinWindow(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err != nil {
		t.Fatalf("first RunOrg: %v", err)
	}
	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("second RunOrg: %v", err)
	}
	if recomputed {
		t.Error("recomputed = true on a call within ttl; want false (TTL skip)")
	}
	if got := store.callCount(); got != 1 {
		t.Errorf("store called %d times; want 1 (second call skipped)", got)
	}
}

// TestAggregator_RunOrg_DistinctOrgsIndependent pins that the TTL gate is
// per-org, not global — a fresh org triggers its own recompute regardless
// of another org's recent cycle.
func TestAggregator_RunOrg_DistinctOrgsIndependent(t *testing.T) {
	store := &fakeStore{}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err != nil {
		t.Fatalf("org-a RunOrg: %v", err)
	}
	recomputed, err := a.RunOrg(context.Background(), "org-b")
	if err != nil {
		t.Fatalf("org-b RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false for a distinct org; want true (per-org TTL)")
	}
	if got := store.callCount(); got != 2 {
		t.Errorf("store called %d times; want 2 (one per org)", got)
	}
}

// TestAggregator_RunOrg_ErrorDoesNotArmTTL pins that a failed recompute
// leaves the TTL gate untouched, so the next trigger retries rather than
// silently sitting stale for a full ttl window after a transient failure.
func TestAggregator_RunOrg_ErrorDoesNotArmTTL(t *testing.T) {
	boom := context.DeadlineExceeded
	store := &fakeStore{err: boom}
	a := NewAggregator(store)

	if _, err := a.RunOrg(context.Background(), "org-a"); err == nil {
		t.Fatal("RunOrg with a failing store returned nil error")
	}
	store.err = nil
	recomputed, err := a.RunOrg(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("retry RunOrg: %v", err)
	}
	if !recomputed {
		t.Error("recomputed = false on retry after a failed cycle; want true (TTL not armed by the failure)")
	}
	if got := store.callCount(); got != 2 {
		t.Errorf("store called %d times; want 2 (failed attempt + retry)", got)
	}
}

// traceRecorder is installed on the OTel global once for the package: the job
// cycle tracer resolves through the global delegate, which binds to the first
// provider installed, so a recorder installed per test would capture nothing
// after the first.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordSpans returns a function reporting the spans ended since it was
// called.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

// cycleSpans is the systemjob.cycle spans among spans.
func cycleSpans(spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "systemjob.cycle" {
			out = append(out, s)
		}
	}
	return out
}

// spanAttr is the string value of key on s, and whether s carries it.
func spanAttr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestAggregator_RunOrg_SpansOnlyARecompute pins that the span opens after
// the TTL gate: a recompute is one root carrying the job and org, and a
// TTL skip, which touches nothing, exports no trace at all.
func TestAggregator_RunOrg_SpansOnlyARecompute(t *testing.T) {
	a := NewAggregator(&fakeStore{})

	spans := recordSpans(t)
	if _, err := a.RunOrg(t.Context(), "org-a"); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	cycles := cycleSpans(spans())
	if len(cycles) != 1 {
		t.Fatalf("%d systemjob.cycle spans for one recompute; want 1", len(cycles))
	}
	s := cycles[0]
	if job, _ := spanAttr(s, "job"); job != "marketplace_aggregator" {
		t.Errorf("job = %q; want marketplace_aggregator", job)
	}
	if org, _ := spanAttr(s, "org.id"); org != "org-a" {
		t.Errorf("org.id = %q; want org-a", org)
	}
	if outcome, ok := spanAttr(s, "outcome"); ok {
		t.Errorf("outcome = %q on a recompute; want none", outcome)
	}
	if s.Status().Code == codes.Error {
		t.Error("a successful recompute carries an error status")
	}

	skipped := recordSpans(t)
	if _, err := a.RunOrg(t.Context(), "org-a"); err != nil {
		t.Fatalf("second RunOrg: %v", err)
	}
	if n := len(cycleSpans(skipped())); n != 0 {
		t.Errorf("%d systemjob.cycle spans for a TTL skip; want 0", n)
	}
}

// TestAggregator_RunOrg_StoreErrorSpan pins that a failed recompute marks its
// span an error, while a recompute cut off by shutdown is an anticipated stop.
func TestAggregator_RunOrg_StoreErrorSpan(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		cancelled   bool
		wantOutcome string
		wantError   bool
	}{
		{name: "failed", err: errors.New("recompute failed"), wantError: true},
		{name: "shutdown", err: context.Canceled, cancelled: true, wantOutcome: "shutdown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewAggregator(&fakeStore{err: c.err})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.cancelled {
				cancel()
			}

			spans := recordSpans(t)
			if _, err := a.RunOrg(ctx, "org-a"); err == nil {
				t.Fatal("RunOrg with a failing store returned nil error")
			}
			cycles := cycleSpans(spans())
			if len(cycles) != 1 {
				t.Fatalf("%d systemjob.cycle spans for one recompute; want 1", len(cycles))
			}
			if outcome, _ := spanAttr(cycles[0], "outcome"); outcome != c.wantOutcome {
				t.Errorf("outcome = %q; want %q", outcome, c.wantOutcome)
			}
			if gotError := cycles[0].Status().Code == codes.Error; gotError != c.wantError {
				t.Errorf("error status = %v; want %v", gotError, c.wantError)
			}
		})
	}
}

// TestAggregator_TTLConstant is a sanity pin on the generous-freshness
// intent from the ticket ("daily-ish freshness is plenty") — catches an
// accidental order-of-magnitude change (e.g. minutes instead of hours).
func TestAggregator_TTLConstant(t *testing.T) {
	if ttl < 12*time.Hour || ttl > 48*time.Hour {
		t.Errorf("ttl = %v; want roughly daily (12h-48h)", ttl)
	}
}
