package worktree

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/worktree")

// gitOtherOp is the op a span records for a subcommand outside gitOps.
const gitOtherOp = "other"

// gitOps is the closed set of subcommands a worktree.git span names: the ones
// this package's git calls run. args[0] is a constant at every call site, but
// the span leaves the process, so the set is checked rather than trusted.
var gitOps = map[string]struct{}{
	"apply":            {},
	"branch":           {},
	"bundle":           {},
	"cat-file":         {},
	"check-ref-format": {},
	"checkout":         {},
	"clone":            {},
	"config":           {},
	"fetch":            {},
	"index-pack":       {},
	"remote":           {},
	"rev-parse":        {},
	"show-ref":         {},
	"symbolic-ref":     {},
	"update-ref":       {},
	"worktree":         {},
}

// gitOp is the op attribute for a git invocation: its subcommand when gitOps
// holds it, else gitOtherOp. No other argument is read, because refs, URLs, and
// paths are tenant data.
func gitOp(args []string) string {
	if len(args) == 0 {
		return gitOtherOp
	}
	if _, ok := gitOps[args[0]]; ok {
		return args[0]
	}
	return gitOtherOp
}

// startGitSpan opens a worktree.git span for one git invocation. A ctx with no
// valid span gets a no-op span back, the rule telemetry.TracedTransport
// follows, so a git call outside a trace never becomes a trace of its own.
func startGitSpan(ctx context.Context, args []string) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, noop.Span{}
	}
	return tracer.Start(ctx, "worktree.git", trace.WithAttributes(telemetry.Op(gitOp(args))))
}

// endGitSpan ends a span from startGitSpan. The status message is fixed and
// the error is not recorded, because a git error's text carries the
// invocation's arguments and git's output.
func endGitSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "git failed")
	}
	span.End()
}
