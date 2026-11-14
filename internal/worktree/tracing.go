package worktree

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/worktree")

// gitOtherOp is the op a span records for a subcommand outside gitOps.
const gitOtherOp = "other"

// gitOps is the closed set of subcommands a worktree.git span names. The span
// leaves the process, so a subcommand is recorded only when the set holds it.
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
// valid span gets a no-op span back, so a git call outside a trace never
// becomes a trace of its own.
func startGitSpan(ctx context.Context, args []string) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, noop.Span{}
	}
	return tracer.Start(ctx, "worktree.git", trace.WithAttributes(telemetry.Op(gitOp(args))))
}

// endGitSpan ends a span from startGitSpan. A failed git command is an
// outcome, not an error status: a probe or a best-effort cleanup is expected
// to fail, so only the caller knows whether a failure is wrong, and the stage
// span that fails with it carries the error status. The error itself is not
// recorded, because its text carries the invocation's arguments and git's
// output.
func endGitSpan(ctx context.Context, span trace.Span, err error) {
	switch {
	case err == nil:
	case ctx.Err() != nil:
		span.SetAttributes(telemetry.Outcome("cancelled"))
	default:
		span.SetAttributes(telemetry.Outcome("failed"))
	}
	span.End()
}
