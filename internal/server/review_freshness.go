package server

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Per-comment freshness verdicts (TFAC-500), the human-facing mirror of the
// finalize gate's forward line-map (TFAC-499). They translate the LineMap status
// into the wire vocabulary the review overlay renders as a badge.
const (
	// reviewFreshnessCurrent: the anchored code is unchanged at the same line on
	// the live PR head (LineMapUnchanged).
	reviewFreshnessCurrent = "current"
	// reviewFreshnessMoved: the anchored code survived but its location changed —
	// line shifted and/or file renamed (LineMapMoved). The remapped position is on
	// MappedLine/MappedPath.
	reviewFreshnessMoved = "moved"
	// reviewFreshnessOutdated: the anchored code changed or was deleted
	// (LineMapOutdated) — the comment no longer points at the same code.
	reviewFreshnessOutdated = "outdated"
	// reviewFreshnessUnknown: freshness couldn't be computed (the live head wasn't
	// reachable, GitHub wasn't configured, or the comment carries no line anchor).
	// The overlay still renders so the human can act; the badge just says so.
	reviewFreshnessUnknown = "unknown"
)

// annotateReviewFreshness fills in each comment's freshness verdict and the
// commits-since-finalize count on out, in place (TFAC-500). It resolves the PR's
// live head and forward-maps each staged comment's anchor commit onto it via the
// line-map primitive (TFAC-497), then counts how far the live head is ahead of
// the finalize-time head.
//
// Best-effort by contract: ANY failure (GitHub not configured, live-head fetch
// error, a compare that won't load) degrades to "unknown" freshness and a nil
// count rather than failing the overlay — a stale review must still render so the
// human can read and act on it. Comments are left at the caller's pre-seeded
// "unknown" default whenever their anchor can't be mapped, so this never needs to
// roll anything back on a partial failure.
//
// Freshness lands on each out-comment by matching the staged comment's stable
// TF-local id, not by slice position — out.Comments is built 1:1 from
// StagedComments today, but joining on id keeps that from becoming a silent
// correctness dependency on iteration order if either side is later filtered or
// reordered.
func (ah *artifactsHandler) annotateReviewFreshness(ctx context.Context, orgID string, art *domain.Artifact, details domain.ReviewArtifactDetails, out *reviewArtifactJSON) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return // malformed target — leave everything "unknown" (reviewGet already 500s a bad target before us)
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

	cmp := newCompareCache(ctx, gh, owner, repo, liveHead)

	// commits-since-finalize: how many commits the live head is ahead of the
	// finalize head. Skipped (left nil/unknown) if we have no finalize anchor or
	// the compare won't load.
	if cc, ok := cmp.from(finalizeHead); ok {
		n := cc.aheadBy
		out.CommitsSinceFinalize = &n
	}

	// Index the staged comments by their stable TF-local id so freshness joins the
	// right out-comment without depending on slice order (see the doc comment).
	staged := make(map[string]domain.ReviewArtifactComment, len(details.StagedComments))
	for _, c := range details.StagedComments {
		if c.ID != "" {
			staged[c.ID] = c
		}
	}

	// Per-comment freshness. Each comment maps from its own anchor (normally the
	// shared finalize head, so the cache fetches one compare) forward to the live
	// head.
	for i := range out.Comments {
		c, ok := staged[out.Comments[i].ID]
		if !ok || c.Line == nil {
			continue // no matching staged comment, or no line anchor — stays "unknown"
		}
		anchor := c.CommitSHA
		if anchor == "" {
			anchor = finalizeHead // staged before per-comment anchoring landed
		}
		cc, ok := cmp.from(anchor)
		if !ok {
			continue // compare unavailable for this anchor — stays "unknown"
		}
		res := cc.lineMap.MapComment(c.Path, *c.Line, c.StartLine)
		switch res.Status {
		case ghclient.LineMapUnchanged:
			out.Comments[i].Freshness = reviewFreshnessCurrent
		case ghclient.LineMapMoved:
			out.Comments[i].Freshness = reviewFreshnessMoved
			out.Comments[i].MappedLine = res.Line
			out.Comments[i].MappedPath = res.Path
		default: // ghclient.LineMapOutdated
			out.Comments[i].Freshness = reviewFreshnessOutdated
		}
	}
}

// compareCache memoizes base...liveHead compares for a single reviewGet, keyed by
// base commit. In the normal case every staged comment shares the finalize head
// as its anchor, so the per-comment line-map and the commits-since-finalize count
// all resolve from one fetched compare. A base equal to the live head needs no
// fetch — there's no drift — so it yields an empty line-map (every anchor maps
// unchanged) and aheadBy 0.
type compareCache struct {
	ctx      context.Context
	gh       *ghclient.Client
	owner    string
	repo     string
	liveHead string
	byBase   map[string]cachedCompare
}

// cachedCompare is one base...liveHead result. ok records whether the fetch
// succeeded so a cached miss (zero value) is distinguishable from a real compare
// without re-requesting.
type cachedCompare struct {
	lineMap ghclient.LineMap
	aheadBy int
	ok      bool
}

func newCompareCache(ctx context.Context, gh *ghclient.Client, owner, repo, liveHead string) *compareCache {
	return &compareCache{ctx: ctx, gh: gh, owner: owner, repo: repo, liveHead: liveHead, byBase: map[string]cachedCompare{}}
}

// from returns the cached compare for base...liveHead, fetching it once. ok is
// false when base is empty or the compare can't be loaded — the caller then
// leaves the affected fields at "unknown".
func (m *compareCache) from(base string) (cachedCompare, bool) {
	if base == "" {
		return cachedCompare{}, false
	}
	if c, seen := m.byBase[base]; seen {
		return c, c.ok
	}
	var c cachedCompare
	if base == m.liveHead {
		// No drift from this base: an empty line-map maps every anchor unchanged.
		c = cachedCompare{lineMap: ghclient.ParseLineMap(""), ok: true}
	} else {
		cc, err := m.gh.CompareCommits(m.ctx, m.owner, m.repo, base, m.liveHead)
		if err != nil {
			artifactsLog.Warn("review freshness: compare failed; serving unknown for this anchor",
				"owner", m.owner, "repo", m.repo, "base", base, "head", m.liveHead, "error", err)
			// Cache the miss so a shared anchor doesn't re-request.
			m.byBase[base] = cachedCompare{}
			return cachedCompare{}, false
		}
		c = cachedCompare{lineMap: ghclient.ParseLineMapFromPatches(cc.Files), aheadBy: cc.AheadBy, ok: true}
	}
	m.byBase[base] = c
	return c, true
}
