package tracker

import (
	"context"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
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

// The GitHub reads of the branch watch that can fail: the
// mergeability.base_read_failures label vocabulary.
const (
	baseReadTip     = "tip"
	baseReadCompare = "compare"
	baseReadPaths   = "paths"
)

const (
	baseMovesMetric          = "mergeability.base_moves"
	baseReadFailuresMetric   = "mergeability.base_read_failures"
	overlapSetSizeMetric     = "mergeability.overlap_set_size"
	pathFetchesMetric        = "mergeability.path_fetches"
	missedHintsMetric        = "github.missed_hints"
	dependabotListingsMetric = "github.dependabot_listings"
)

// The outcomes of listing one repository's open Dependabot alerts: the
// github.dependabot_listings label vocabulary. A rate-limited listing is not
// counted here; the upstream request metrics count it.
const (
	dependabotListingListed      = "listed"
	dependabotListingNotModified = "not_modified"
	dependabotListingRefused     = "refused"
	dependabotListingFailed      = "failed"
)

// overlapSetSizeBounds are the histogram's buckets, as a share of the base's
// open pull requests. The measured mean is about 1%, so the low buckets are
// the fine ones.
var overlapSetSizeBounds = []float64{0, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5, 1}

// instruments are the branch watch's and the refresh gate's. Every label
// value but the missed hints' org_id is a closed vocabulary: a move outcome, a
// read, a path-fetch source or a timer rule. The missed hints carry the org
// because a hint's reliability is a property of the org's webhook delivery.
type instruments struct {
	baseMoves          metric.Int64Counter
	baseReadFailures   metric.Int64Counter
	overlapSetSize     metric.Float64Histogram
	pathFetches        metric.Int64Counter
	missedHints        metric.Int64Counter
	dependabotListings metric.Int64Counter
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
	if in.baseReadFailures, err = m.Int64Counter(baseReadFailuresMetric,
		metric.WithDescription("Branch watch reads GitHub failed, other than rate limits, by read: tip (a base branch's tip, per branch), compare (what a move changed, per branch) or paths (a pull request's changed files, per pull request). A failed tip read leaves the branch's move unprocessed until a read succeeds.")); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", baseReadFailuresMetric, "error", err)
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
	if in.missedHints, err = m.Int64Counter(missedHintsMetric,
		metric.WithDescription("Scheduled refreshes of a pull request that a timer rule forced, with no refresh mark naming it and no change in its listing, that emitted an event, by org and rule (change_window, floor, in_flight_ci): a change no webhook hint announced.")); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", missedHintsMetric, "error", err)
	}
	if in.dependabotListings, err = m.Int64Counter(dependabotListingsMetric,
		metric.WithDescription("Listings of a repository's open Dependabot alerts, by outcome: listed (GitHub answered with the alerts), not_modified (unchanged since the previous listing, answered 304), refused (GitHub refused the repository, which is skipped), or failed (any other failure, which ends the cycle's listings).")); err != nil {
		trackerLog.Warn("tracker instrument setup failed", "instrument", dependabotListingsMetric, "error", err)
	}
	return in
}

// recordDependabotListing counts one repository's alert listing by outcome.
func recordDependabotListing(outcome string) {
	if s := stats.Load(); s != nil {
		s.dependabotListings.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// recordMissedHint counts one timer-forced refresh that found a change no
// mark named.
func recordMissedHint(orgID string, rule refreshRule) {
	if s := stats.Load(); s != nil {
		s.missedHints.Add(context.Background(), 1, metric.WithAttributes(telemetry.OrgID(orgID), attribute.String("rule", string(rule))))
	}
}

func recordBaseMoveOutcome(outcome string) {
	if s := stats.Load(); s != nil {
		s.baseMoves.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

func recordBaseReadFailures(read string, n int) {
	if n == 0 {
		return
	}
	if s := stats.Load(); s != nil {
		s.baseReadFailures.Add(context.Background(), int64(n), metric.WithAttributes(attribute.String("read", read)))
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
