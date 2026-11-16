package delegate

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// restoreMissingCommits owns the counter of checkouts a workspace restore
// could not rebuild because the git host would not send a commit the snapshot
// needs. Package-level and swapped atomically for the reason
// workspaceCheckpoints is.
var restoreMissingCommits atomic.Pointer[restoreMissingCommitStats]

func init() {
	restoreMissingCommits.Store(newRestoreMissingCommitStats(otel.GetMeterProvider()))
}

type restoreMissingCommitStats struct {
	missing metric.Int64Counter
}

// restoreMissingCommitKinds is every kind the counter is labeled with.
// OpenRestoreMissingCommitSeries opens each at zero, so a new kind belongs
// here as well.
var restoreMissingCommitKinds = []worktree.MissingCommitKind{worktree.MissingHead, worktree.MissingPrerequisite}

// newRestoreMissingCommitStats builds the instrument against mp. A creation
// error can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newRestoreMissingCommitStats(mp metric.MeterProvider) *restoreMissingCommitStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("workspace.restore_missing_commits",
		metric.WithDescription("Checkouts a workspace restore could not rebuild because the git host would not send a commit the snapshot needs, "+
			"by kind: head (the captured HEAD), prerequisite (a commit the snapshot's bundle stops at)."))
	if err != nil {
		delegateLog.Warn("restore missing-commit counter setup failed", "error", err)
	}
	return &restoreMissingCommitStats{missing: counter}
}

// OpenRestoreMissingCommitSeries creates each kind's series at zero, so the
// first restore to fail this way after a restart is a change increase() can
// see. Called at dispatcher startup, after telemetry.Init, for the reason
// OpenWorkspaceCheckpointSeries is.
func OpenRestoreMissingCommitSeries() {
	stats := restoreMissingCommits.Load()
	if stats == nil || stats.missing == nil {
		return
	}
	for _, kind := range restoreMissingCommitKinds {
		stats.missing.Add(context.Background(), 0, metric.WithAttributes(attribute.String("kind", string(kind))))
	}
}

// recordRestoreMissingCommit counts one checkout whose restore failed on a
// commit of kind.
func recordRestoreMissingCommit(kind worktree.MissingCommitKind) {
	stats := restoreMissingCommits.Load()
	if stats == nil || stats.missing == nil {
		return
	}
	stats.missing.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", string(kind))))
}
