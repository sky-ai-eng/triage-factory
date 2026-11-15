package worktree

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// One provider for the test binary: the package's tracer binds to the first
// provider installed on the OTel global and never rebinds.
var (
	gitTraceOnce     sync.Once
	gitTraceRecorder = tracetest.NewSpanRecorder()
)

// TestGitHelpersTraceOnlyInsideATrace checks that a git call under a span
// records a worktree.git span naming its subcommand and nothing else of its
// arguments, that a failure is an outcome carrying no error text rather than
// an error status, and that a call with no span on ctx records nothing.
func TestGitHelpersTraceOnlyInsideATrace(t *testing.T) {
	gitTraceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(gitTraceRecorder)))
	})
	dir := t.TempDir()
	gitAt(t, dir, "init", "-q", "-b", "main")
	gitAt(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")

	start := len(gitTraceRecorder.Ended())
	if _, err := gitOutputCtx(context.Background(), dir, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := gitRunCtx(context.Background(), dir, "not-a-git-command"); err == nil {
		t.Fatal("an unknown subcommand succeeded")
	}
	if got := gitTraceRecorder.Ended()[start:]; len(got) != 0 {
		t.Fatalf("git calls with no span on ctx recorded %d spans", len(got))
	}

	ctx, parent := otel.Tracer("test").Start(context.Background(), "test.parent")
	if _, err := gitOutputCtx(ctx, dir, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := gitRunCtx(ctx, dir, "not-a-git-command", "refs/heads/main"); err == nil {
		t.Fatal("an unknown subcommand succeeded")
	}
	parent.End()

	var spans []sdktrace.ReadOnlySpan
	for _, s := range gitTraceRecorder.Ended()[start:] {
		if s.Name() == "worktree.git" {
			spans = append(spans, s)
		}
	}
	if len(spans) != 2 {
		t.Fatalf("recorded %d worktree.git spans, want 2", len(spans))
	}
	want := []struct {
		attrs  []attribute.KeyValue
		status sdktrace.Status
	}{
		{
			attrs:  []attribute.KeyValue{attribute.String("op", "rev-parse")},
			status: sdktrace.Status{Code: codes.Unset},
		},
		{
			attrs:  []attribute.KeyValue{attribute.String("op", "other"), attribute.Int("attempt", 1), attribute.String("outcome", "failed")},
			status: sdktrace.Status{Code: codes.Unset},
		},
	}
	for i, s := range spans {
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("span %d is not a child of the span on ctx", i)
		}
		got := attribute.NewSet(s.Attributes()...)
		if wantSet := attribute.NewSet(want[i].attrs...); !got.Equals(&wantSet) {
			t.Errorf("span %d attributes = %v, want %v", i, got.ToSlice(), wantSet.ToSlice())
		}
		if s.Status() != want[i].status {
			t.Errorf("span %d status = %+v, want %+v", i, s.Status(), want[i].status)
		}
		if len(s.Events()) != 0 {
			t.Errorf("span %d recorded %d events, want none", i, len(s.Events()))
		}
	}
}
