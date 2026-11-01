package tracker

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope the tracker's instruments are created
// under.
const meterName = "internal/eventing/tracker"

var stats atomic.Pointer[instruments]

func init() {
	stats.Store(newInstruments(otel.GetMeterProvider()))
}

// The outcomes of a base branch move the watch processed: the
// mergeability.base_moves label vocabulary.
const (
	baseMoveOverlap    = "overlap"
	baseMoveFirstSight = "reset_first_sight"
	baseMoveTruncated  = "reset_truncated"
	baseMoveRewritten  = "reset_rewritten"
	baseMoveMissingTip = "reset_missing_tip"
	baseMoveUnreadable = "reset_unreadable"
	baseMoveIdentical  = "identical"
)

// The sources of a changed-paths read: the mergeability.path_fetches label
// vocabulary.
const (
	pathFetchGraphQL     = "graphql"
	pathFetchRESTRenames = "rest_renames"
)

const (
	baseMovesMetric      = "mergeability.base_moves"
	overlapSetSizeMetric = "mergeability.overlap_set_size"
	pathFetchesMetric    = "mergeability.path_fetches"
)

// overlapSetSizeBounds are the histogram's buckets, as a share of the base's
// open pull requests. The measured mean is about 1%, so the low buckets are
// the fine ones.
var overlapSetSizeBounds = []float64{0, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5, 1}

// instruments are the branch watch's. Every label value is a closed
// vocabulary: a move outcome or a path-fetch source.
type instruments struct {
	baseMoves      metric.Int64Counter
	overlapSetSize metric.Float64Histogram
	pathFetches    metric.Int64Counter
}

// newInstruments builds the instruments against mp; production passes the
// global provider, which telemetry.Init installs. A creation error can only be
// an invalid name, and the API hands back a usable no-op beside it, so it is
// logged rather than propagated.
func newInstruments(mp metric.MeterProvider) *instruments {
	m := mp.Meter(meterName)
	in := &instruments{}
	var err error
	if in.baseMoves, err = m.Int64Counter(baseMovesMetric,
		metric.WithDescription("Base branch moves the poll processed, by outcome: overlap (the pull requests whose changed paths overlap the move were probed), identical, or a reset that probed every pull request on the base (reset_first_sight, reset_truncated, reset_rewritten, reset_missing_tip, reset_unreadable).")); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", baseMovesMetric, "error", err)
	}
	if in.overlapSetSize, err = m.Float64Histogram(overlapSetSizeMetric,
		metric.WithDescription("The share of a base branch's open pull requests an overlap move probed, per move."),
		metric.WithExplicitBucketBoundaries(overlapSetSizeBounds...)); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", overlapSetSizeMetric, "error", err)
	}
	if in.pathFetches, err = m.Int64Counter(pathFetchesMetric,
		metric.WithDescription("Requests sent to read pull requests' changed paths, by source: graphql (the paths) or rest_renames (the previous paths of renamed and copied files).")); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", pathFetchesMetric, "error", err)
	}
	return in
}

func recordBaseMoveOutcome(outcome string) {
	if s := stats.Load(); s != nil {
		s.baseMoves.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

func recordOverlapSetSize(probed, open int) {
	if open == 0 {
		return
	}
	if s := stats.Load(); s != nil {
		s.overlapSetSize.Record(context.Background(), float64(probed)/float64(open))
	}
}

func recordPathFetches(source string, requests int) {
	if requests == 0 {
		return
	}
	if s := stats.Load(); s != nil {
		s.pathFetches.Add(context.Background(), int64(requests), metric.WithAttributes(attribute.String("source", source)))
	}
}
