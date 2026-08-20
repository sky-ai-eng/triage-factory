package server

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/review"
)

// annotateReviewFreshness fills in each comment's freshness verdict and the
// commits-since-finalize count on out, in place (TFAC-500). It resolves the PR's
// live head and forward-maps each staged comment's anchor commit onto it via the
// shared reconcile (internal/review, the same transform the agent finalize gate
// uses), then counts how far the live head is ahead of the finalize-time head.
//
// Best-effort by contract, and deliberately exempt from the sweep that turned
// swallowed store/vault errors into 500s: ANY failure (GitHub not configured,
// live-head fetch error, a compare that won't load) degrades to "unknown"
// freshness and a nil count rather than failing the overlay — this is a display
// annotation on a review the caller can already read and act on, and "unknown"
// is an honest value the wire shape carries. Comments are left at the caller's
// pre-seeded "unknown" default whenever their anchor can't be mapped.
//
// Freshness joins each out-comment to its staged comment by stable TF-local id, so
// it never depends on slice ordering.
func (ah *artifactsHandler) annotateReviewFreshness(ctx context.Context, orgID string, art *domain.Artifact, details domain.ReviewArtifactDetails, out *reviewArtifactDetailsJSON) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return // malformed target — leave everything "unknown" (reviewArtifactDetails 500s a bad target before us)
	}
	// Resolve the per-repo client directly (not ghForArtifact, which writes an
	// error response): a missing credential must degrade to "unknown", not 503 the
	// overlay.
	gh, err := ah.ghResolver.ClientForRepo(ctx, orgID, owner, repo)
	if err != nil {
		artifactsLog.Warn("review freshness: GitHub client unavailable; serving unknown freshness",
			"artifact", art.ID, "owner", owner, "repo", repo, "error", err)
		return
	}
	pr, err := gh.GetPRBasic(ctx, owner, repo, number)
	if err != nil || pr.HeadSHA == "" {
		artifactsLog.Warn("review freshness: live PR head unavailable; serving unknown freshness",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		return
	}
	liveHead := pr.HeadSHA

	// The finalize-time head the drift is measured from. Rows finalized before
	// TFAC-500 have no FinalizedHeadSHA; fall back to the start-review head so the
	// count + map still resolve against *some* anchor rather than going blank.
	finalizeHead := details.FinalizedHeadSHA
	if finalizeHead == "" {
		finalizeHead = details.HeadSHA
	}

	// Prefetch finalizeHead...liveHead once. It yields BOTH the commits-since count
	// (ahead_by) and the forward line-map for the common case (every comment shares
	// the finalize head as its anchor), so the resolver below reuses it instead of
	// re-fetching. A base equal to the live head means no drift — count 0, no fetch.
	var baseCmp *ghclient.CommitComparison
	switch finalizeHead {
	case "":
		// no anchor to count from — leave CommitsSinceFinalize nil (unknown)
	case liveHead:
		zero := 0
		out.CommitsSinceFinalize = &zero
	default:
		if cmp, e := gh.CompareCommits(ctx, owner, repo, finalizeHead, liveHead); e == nil {
			baseCmp = cmp
			n := cmp.AheadBy
			out.CommitsSinceFinalize = &n
		} else {
			artifactsLog.Warn("review freshness: commits-since-finalize compare failed; serving unknown count",
				"artifact", art.ID, "owner", owner, "repo", repo, "base", finalizeHead, "head", liveHead, "error", e)
		}
	}

	lineMapFor := func(anchor string) (ghclient.LineMap, error) {
		if anchor == finalizeHead && baseCmp != nil {
			return ghclient.ParseLineMapFromPatches(baseCmp.Files), nil
		}
		cmp, e := gh.CompareCommits(ctx, owner, repo, anchor, liveHead)
		if e != nil {
			artifactsLog.Warn("review freshness: compare failed; serving unknown for this anchor",
				"artifact", art.ID, "owner", owner, "repo", repo, "base", anchor, "head", liveHead, "error", e)
			return ghclient.LineMap{}, e
		}
		return ghclient.ParseLineMapFromPatches(cmp.Files), nil
	}

	// Forward-map every staged comment to the live head, then join the verdicts back
	// onto the wire comments by id. We keep the original staged comment alongside
	// each outcome so a "moved" verdict can report a renamed path (Outcome.Comment
	// carries the new path only when it actually changed).
	type joined struct {
		outcome  review.Outcome
		original domain.ReviewArtifactComment
	}
	outcomes := review.ReconcileToHead(details.StagedComments, liveHead, finalizeHead, lineMapFor)
	byID := make(map[string]joined, len(outcomes))
	for i, o := range outcomes {
		if c := details.StagedComments[i]; c.ID != "" {
			byID[c.ID] = joined{outcome: o, original: c}
		}
	}

	for i := range out.Comments {
		j, ok := byID[out.Comments[i].ID]
		if !ok || !j.outcome.Classified() {
			continue // unmatched, unpositioned, or compare unavailable — stays "unknown"
		}
		out.Comments[i].Freshness = review.Freshness(j.outcome.Status)
		if j.outcome.Status == ghclient.LineMapMoved {
			if j.outcome.Comment.Line != nil {
				out.Comments[i].MappedLine = *j.outcome.Comment.Line
			}
			if j.outcome.Comment.Path != j.original.Path {
				out.Comments[i].MappedPath = j.outcome.Comment.Path // rename only
			}
		}
	}
}
