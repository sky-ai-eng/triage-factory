package tracker

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// The branch watch: a base branch's move can change the mergeability of the
// pull requests on it without moving their heads or updated_at, so the poll
// watches each base branch's tip and records the move on the row of each pull
// request it can have affected, which admitMergeabilityProbes admits a probe
// for until a read after the move settles. It is the only writer of
// branch_tips, of the pull requests' changed paths and of their recorded base
// moves.

// basePR is one open pull request on a watched base branch.
type basePR struct {
	entity domain.Entity
	nodeID string
	// head and base are the pull request's current head and base branch: the
	// refresh's this cycle, the listing's, or the stored snapshot's, in that
	// order. base differs from the branch the pull request is grouped under
	// in the cycle that sees it retargeted.
	head, base string
}

// baseCheck is one watched branch's check this cycle: what the ref read and
// the compares found. Written by one goroutine of the check pool and read
// after it, like discovery's repoListResult.
type baseCheck struct {
	ref         domain.BranchRef
	owner, repo string
	stored      *domain.BranchTip
	// done is false when a read failed: the stored tip stays and the move,
	// if any, is processed next cycle.
	done bool
	// tip and etag are what the branch's row is written with.
	tip, etag string
	// outcome is the move's, one of the baseMove* values; empty when the
	// tip did not move.
	outcome string
	// movePaths is what an overlap move changed.
	movePaths []domain.ChangedPath
}

// baseWatchScope is which base branches one RefreshGitHub pass checks.
type baseWatchScope struct {
	// passRepos is the registry row ids of the repositories the pass was
	// handed. Only their branches are checked, since the pass's client may
	// reach no other: the poller runs one pass per App installation.
	passRepos map[string]bool
	// listedOK is the registry row ids whose open-PR listing answered 200.
	listedOK map[string]bool
}

// watchBaseBranches checks the tip of each base branch the org's open pull
// requests target in the pass's repositories, and records each move on the
// pull requests it can have affected (see describeMove and recordBaseMove). A
// branch is checked when its repository's open-PR listing answered 200 this
// cycle, since a merged pull request leaves the listing, or when it was last
// checked longer than staleRefreshFloor ago, which catches a push that changes
// no pull request. Branches no open pull request in the org
// targets lose their row, whichever pass sees it.
//
// The base is the stored snapshot's base_ref; open is every open pull request
// the cycle knows, and a pull request the refresh found terminal is left out.
// A pull request in a repository no pass is handed (one no team tracks any
// more) is left to the sweep. Failures are logged and leave the stored tip, so
// the move is processed again next cycle, except a compare that has failed
// past unreadableMoveAfter.
func (t *Tracker) watchBaseBranches(ctx context.Context, client *ghclient.Client, orgID string, open []mergeabilityCandidate, scope baseWatchScope, listed, refreshed map[string]domain.PRSnapshot) {
	// Its own span: the watch's ref reads, compares and path reads are
	// GitHub requests, which a reader of the cycle's trace should find here
	// rather than under the network-free diff phase.
	ctx, span := tracer.Start(ctx, "tracker.github.base_watch")
	defer span.End()

	bases := map[domain.BranchRef][]basePR{}
	for _, c := range open {
		if c.snap.BaseRef == "" || c.entity.RepositoryID == "" {
			continue
		}
		pr := basePR{entity: c.entity, nodeID: c.nodeID, head: c.snap.HeadSHA, base: c.snap.BaseRef}
		if snap, ok := listed[c.nodeID]; ok {
			pr.head, pr.base = fresher(pr.head, snap.HeadSHA), fresher(pr.base, snap.BaseRef)
		}
		if snap, ok := refreshed[c.nodeID]; ok {
			if prSnapshotTerminal(snap) {
				continue
			}
			pr.head, pr.base = fresher(pr.head, snap.HeadSHA), fresher(pr.base, snap.BaseRef)
		}
		ref := domain.BranchRef{RepositoryID: c.entity.RepositoryID, Branch: c.snap.BaseRef}
		bases[ref] = append(bases[ref], pr)
	}
	targeted := make([]domain.BranchRef, 0, len(bases))
	for ref := range bases {
		targeted = append(targeted, ref)
	}
	sort.Slice(targeted, func(i, j int) bool {
		if targeted[i].RepositoryID != targeted[j].RepositoryID {
			return targeted[i].RepositoryID < targeted[j].RepositoryID
		}
		return targeted[i].Branch < targeted[j].Branch
	})

	rows, err := t.mergeability.ListBranchTipsSystem(ctx, orgID)
	if err != nil {
		trackerLog.WarnContext(ctx, "branch watch: reading the stored tips failed; no base branch is checked this cycle", "error", err)
		return
	}
	if _, err := t.mergeability.DeleteBranchTipsExceptSystem(ctx, orgID, targeted); err != nil {
		trackerLog.WarnContext(ctx, "branch watch: deleting the tips of branches no open pull request targets failed; the next cycle retries", "error", err)
	}
	stored := make(map[domain.BranchRef]domain.BranchTip, len(rows))
	for _, row := range rows {
		stored[row.Ref()] = row
	}

	now := time.Now()
	repoNames := map[string]*domain.Repository{}
	var checks []baseCheck
	for _, ref := range targeted {
		if !scope.passRepos[ref.RepositoryID] {
			continue
		}
		tip, ok := stored[ref]
		if ok && !scope.listedOK[ref.RepositoryID] && now.Sub(tip.ObservedAt) <= staleRefreshFloor {
			continue
		}
		repo, seen := repoNames[ref.RepositoryID]
		if !seen {
			repo = t.entityRepository(ctx, bases[ref][0].entity)
			repoNames[ref.RepositoryID] = repo
		}
		if repo == nil {
			continue
		}
		check := baseCheck{ref: ref, owner: repo.Owner, repo: repo.Repo}
		if ok {
			check.stored = &tip
		}
		checks = append(checks, check)
	}
	if len(checks) == 0 {
		return
	}

	// The checks are GitHub reads only, on the listing's bounded pool. The
	// first rate limit stops new checks from starting: the rest would be
	// refused the same way.
	var rateLimited atomic.Bool
	g := new(errgroup.Group)
	g.SetLimit(repoConcurrency())
	for i := range checks {
		if rateLimited.Load() {
			break
		}
		g.Go(func() error {
			t.checkBaseBranch(ctx, client, &checks[i], &rateLimited)
			return nil
		})
	}
	_ = g.Wait()

	paths := t.overlapPaths(ctx, client, orgID, checks, bases)
	for _, c := range checks {
		if c.done {
			t.recordBaseMove(ctx, orgID, c, bases[c.ref], paths)
		}
	}
}

// fresher is next when a read gave one, and prev otherwise.
func fresher(prev, next string) string {
	if next != "" {
		return next
	}
	return prev
}

// checkBaseBranch reads one branch's tip, conditional on its stored ETag, and
// when it moved, what the move changed. It writes only to c.
func (t *Tracker) checkBaseBranch(ctx context.Context, client *ghclient.Client, c *baseCheck, rateLimited *atomic.Bool) {
	etag := ""
	if c.stored != nil {
		etag = c.stored.ETag
	}
	sha, newETag, notModified, err := client.BranchTip(ctx, c.owner, c.repo, c.ref.Branch, etag)
	if err != nil {
		t.logBaseReadFailure(ctx, c, "reading the branch's tip", err, rateLimited)
		return
	}
	switch {
	case notModified && c.stored != nil:
		c.done, c.tip, c.etag = true, c.stored.TipSHA, c.stored.ETag
		return
	case notModified:
		trackerLog.WarnContext(ctx, "branch watch: GitHub answered 304 to an unconditional ref read; the branch is checked again next cycle",
			"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch)
		return
	}
	c.tip, c.etag = sha, newETag
	switch {
	case c.stored == nil:
		c.done, c.outcome = true, baseMoveFirstSight
	case sha == c.stored.TipSHA:
		c.done = true
	default:
		outcome, paths, err := describeMove(ctx, client, c.owner, c.repo, c.stored.TipSHA, sha)
		var rl *ghclient.ErrRateLimited
		switch {
		case err == nil:
		case errors.As(err, &rl) || time.Since(c.stored.ObservedAt) <= unreadableMoveAfter:
			t.logBaseReadFailure(ctx, c, "comparing the branch's previous tip with its new one", err, rateLimited)
			return
		default:
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "branch watch: comparing the branch's previous tip with its new one keeps failing; every pull request on it is probed",
				"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "since", c.stored.ObservedAt, "error", err)
			outcome, paths = baseMoveUnreadable, nil
		}
		c.done, c.outcome, c.movePaths = true, outcome, paths
	}
}

// unreadableMoveAfter is how long after a branch's tip was last stored a
// compare that still fails, for a reason other than a rate limit, resets the
// branch instead of being tried again. GitHub can fail to generate a
// comparison whose diff is too large every time it is asked, and the move
// would otherwise never be processed. Twice staleRefreshFloor, so that a
// branch checked only at the floor has failed for a whole floor first.
const unreadableMoveAfter = 2 * staleRefreshFloor

func (t *Tracker) logBaseReadFailure(ctx context.Context, c *baseCheck, what string, err error, rateLimited *atomic.Bool) {
	var rl *ghclient.ErrRateLimited
	if errors.As(err, &rl) {
		rateLimited.Store(true)
	}
	trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "branch watch: "+what+" failed; the branch is checked again next cycle",
		"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "error", err)
}

// describeMove reads what moving a branch from old to tip changed, through
// the compare old...tip, and returns the move's outcome and, for an overlap,
// its paths:
//
//   - ahead with fewer than CompareFileCap files: an overlap move of those
//     files. A compare carrying no file list is read as truncated;
//   - ahead with CompareFileCap files: the list may be truncated. Over
//     CompareCommitCap commits, the range cannot be walked and the base is
//     reset; otherwise its first-parent chain is walked from tip to old, one
//     compare per step, and the steps' files are the move's. A step listing
//     CompareFileCap files, or a chain that leaves the range without reaching
//     old, resets the base;
//   - identical: nothing to probe;
//   - behind or diverged: the base was rewound or force-pushed, and a compare
//     lists only the new side's changes, so it cannot say what the rewrite
//     removed. Reset;
//   - a 404, the old tip no longer existing: reset.
//
// Any other failure is err: the move is processed again next cycle, or reset
// once it has failed past unreadableMoveAfter (checkBaseBranch).
func describeMove(ctx context.Context, client *ghclient.Client, owner, repo, old, tip string) (outcome string, paths []domain.ChangedPath, err error) {
	cmp, err := client.Compare(ctx, owner, repo, old, tip)
	if ghclient.IsHTTP404(err) {
		return baseMoveMissingTip, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	switch cmp.Status {
	case "identical":
		return baseMoveIdentical, nil, nil
	case "ahead":
	default:
		// behind, diverged, and any status GitHub does not document: TF
		// cannot tell what the move removed.
		return baseMoveRewritten, nil, nil
	}
	if cmp.Files != nil && len(cmp.Files) < ghclient.CompareFileCap {
		return baseMoveOverlap, cmp.Files, nil
	}
	if cmp.TotalCommits > ghclient.CompareCommitCap {
		return baseMoveTruncated, nil, nil
	}
	parents := make(map[string][]string, len(cmp.Commits))
	for _, c := range cmp.Commits {
		parents[c.SHA] = c.Parents
	}
	for cur, steps := tip, 0; cur != old; steps++ {
		ps := parents[cur]
		if len(ps) == 0 || steps > len(cmp.Commits) {
			return baseMoveTruncated, nil, nil
		}
		step, err := client.Compare(ctx, owner, repo, ps[0], cur)
		if ghclient.IsHTTP404(err) {
			return baseMoveMissingTip, nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		if step.Files == nil || len(step.Files) >= ghclient.CompareFileCap {
			return baseMoveTruncated, nil, nil
		}
		paths = append(paths, step.Files...)
		cur = ps[0]
	}
	return baseMoveOverlap, paths, nil
}

// overlapPaths reads the changed paths of every open pull request on a base
// whose move is an overlap, keyed by entity id: the stored list when it was
// read at the pull request's current head and base, since a list is the
// difference between the two, and a fresh read otherwise, which is written
// back (UpsertPathsSystem). A pull request missing from the result could not
// be read this cycle.
func (t *Tracker) overlapPaths(ctx context.Context, client *ghclient.Client, orgID string, checks []baseCheck, bases map[domain.BranchRef][]basePR) map[string]domain.PRPaths {
	var ids []string
	for _, c := range checks {
		if readsPaths(c) {
			for _, pr := range bases[c.ref] {
				if pr.entity.ExternalID != "" {
					ids = append(ids, pr.entity.ID)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]domain.PRPaths, len(ids))
	rows, err := t.mergeability.ListPathsSystem(ctx, orgID, ids)
	if err != nil {
		trackerLog.WarnContext(ctx, "branch watch: reading the stored changed paths failed; every pull request on a moved base is probed", "error", err)
		return nil
	}
	stored := make(map[string]domain.PRPaths, len(rows))
	for _, row := range rows {
		stored[row.EntityID] = row
	}

	// The stale lists, grouped by repository, since a pull request's
	// previous paths are read from its repository.
	type repoKey struct{ owner, repo string }
	var order []repoKey
	stale := map[repoKey][]basePR{}
	seen := map[string]bool{}
	for _, c := range checks {
		if !readsPaths(c) {
			continue
		}
		key := repoKey{c.owner, c.repo}
		for _, pr := range bases[c.ref] {
			if pr.entity.ExternalID == "" || seen[pr.entity.ID] {
				continue
			}
			seen[pr.entity.ID] = true
			if row, ok := stored[pr.entity.ID]; ok && row.Paths != nil && row.HeadSHA == pr.head && row.BaseRef == pr.base {
				out[pr.entity.ID] = row
				continue
			}
			if _, ok := stale[key]; !ok {
				order = append(order, key)
			}
			stale[key] = append(stale[key], pr)
		}
	}
	for _, key := range order {
		if !t.readPRPaths(ctx, client, orgID, key.owner, key.repo, stale[key], out) {
			break
		}
	}
	return out
}

// readsPaths reports whether deciding c's move needs its pull requests'
// changed paths: an overlap move that changed at least one file.
func readsPaths(c baseCheck) bool {
	return c.done && c.outcome == baseMoveOverlap && len(c.movePaths) > 0
}

// readPRPaths reads prs' changed paths, up to ghclient.MaxChangedPathsBatch
// per query, and the previous path of each renamed or copied file, adding
// each pull request read to out and writing it back. It reports false once
// GitHub rate-limited a read, after which nothing more is sent this cycle.
// A pull request whose read failed is left out of out.
func (t *Tracker) readPRPaths(ctx context.Context, client *ghclient.Client, orgID, owner, repo string, prs []basePR, out map[string]domain.PRPaths) bool {
	var rl *ghclient.ErrRateLimited
	for start := 0; start < len(prs); start += ghclient.MaxChangedPathsBatch {
		chunk := prs[start:min(start+ghclient.MaxChangedPathsBatch, len(prs))]
		nodeIDs := make([]string, len(chunk))
		for i, pr := range chunk {
			nodeIDs[i] = pr.entity.ExternalID
		}
		reads, requests, err := client.PRChangedPaths(ctx, nodeIDs)
		recordPathFetches(pathFetchGraphQL, requests)
		if err != nil {
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "branch watch: reading changed paths failed; their pull requests are probed",
				"owner", owner, "repo", repo, "pull_requests", len(chunk), "error", err)
			if errors.As(err, &rl) {
				return false
			}
			continue
		}
		for _, pr := range chunk {
			read, ok := reads[pr.entity.ExternalID]
			if !ok {
				continue
			}
			paths := domain.PRPaths{EntityID: pr.entity.ID, OrgID: orgID, Paths: read.Paths, HeadSHA: read.HeadSHA, BaseRef: read.BaseRef, Complete: read.Complete}
			if paths.Paths == nil {
				paths.Paths = []domain.ChangedPath{}
			}
			if hasRenameOrCopy(read.Paths) {
				previous, requests, err := client.PreviousPaths(ctx, owner, repo, domain.GitHubPRNumber(pr.entity.SourceID))
				recordPathFetches(pathFetchRESTRenames, requests)
				if err != nil {
					trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "branch watch: reading a pull request's renamed files failed; it is probed",
						"source_id", pr.entity.SourceID, "error", err)
					if errors.As(err, &rl) {
						return false
					}
					continue
				}
				paths.Paths = withPrevious(paths.Paths, previous, &paths.Complete)
			}
			if paths.HeadSHA == "" || paths.BaseRef == "" {
				continue
			}
			if written, err := t.mergeability.UpsertPathsSystem(ctx, orgID, paths); err != nil {
				trackerLog.WarnContext(ctx, "branch watch: writing a pull request's changed paths failed; the next move reads them again",
					"source_id", pr.entity.SourceID, "error", err)
			} else {
				paths = written
			}
			out[pr.entity.ID] = paths
		}
	}
	return true
}

func hasRenameOrCopy(paths []domain.ChangedPath) bool {
	for _, p := range paths {
		if p.Change == domain.PathRenamed || p.Change == domain.PathCopied {
			return true
		}
	}
	return false
}

// withPrevious sets each renamed or copied path's previous path from
// previous. One with none there leaves the list incomplete, since the path
// the rule would compare is unknown.
func withPrevious(paths []domain.ChangedPath, previous map[string]string, complete *bool) []domain.ChangedPath {
	out := make([]domain.ChangedPath, len(paths))
	for i, p := range paths {
		if p.Change == domain.PathRenamed || p.Change == domain.PathCopied {
			p.Previous = previous[p.Path]
			if p.Previous == "" {
				*complete = false
			}
		}
		out[i] = p
	}
	return out
}

// recordBaseMove records a checked branch's move on the rows of the pull
// requests it can have affected (MarkBaseMovedSystem) and then stores its tip,
// in that order: a failure between the two leaves the previous tip, so the
// next cycle processes the move again.
//
//   - no move: the tip's ETag and observed time are written;
//   - identical: nothing is recorded;
//   - a reset: every open pull request on the base, base_reset. A reset that
//     can have taken commits out of the base (a rewrite, a previous tip that
//     is gone, a compare that would not answer, or a branch whose history TF
//     has not seen) can also move a pull request's merge base back at an
//     unchanged head, which adds files to its list, so the stored lists are
//     cleared first and read again at the next move;
//   - an overlap: base_moved for each pull request whose paths overlap the
//     move's, and for each whose paths could not be read or are incomplete.
//     A move that changed no file overlaps nothing.
//
// The probe is the cycle's admission's, which runs after the watch and admits
// one for every row owing a move until a read leased after the move settles,
// so a probe that is dropped, or was being read when the move was recorded,
// is followed by another.
func (t *Tracker) recordBaseMove(ctx context.Context, orgID string, c baseCheck, prs []basePR, paths map[string]domain.PRPaths) {
	var owed []string
	reason := domain.MergeabilityProbeBaseMoved
	switch c.outcome {
	case "", baseMoveIdentical:
	case baseMoveOverlap:
		if len(c.movePaths) == 0 {
			break
		}
		move := indexMove(c.movePaths)
		for _, pr := range prs {
			if pr.entity.ExternalID == "" {
				continue
			}
			p, ok := paths[pr.entity.ID]
			if !ok || !p.Complete || move.overlaps(p.Paths) {
				owed = append(owed, pr.entity.ID)
			}
		}
	default:
		reason = domain.MergeabilityProbeBaseReset
		ids := make([]string, 0, len(prs))
		for _, pr := range prs {
			if pr.entity.ExternalID != "" {
				owed = append(owed, pr.entity.ID)
			}
			ids = append(ids, pr.entity.ID)
		}
		if c.outcome != baseMoveTruncated {
			if _, err := t.mergeability.ClearPathsSystem(ctx, orgID, ids); err != nil {
				trackerLog.WarnContext(ctx, "branch watch: clearing the changed paths of a rewritten base's pull requests failed; the move is processed again next cycle",
					"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "error", err)
				return
			}
		}
	}
	if len(owed) > 0 {
		if _, err := t.mergeability.MarkBaseMovedSystem(ctx, orgID, owed, reason); err != nil {
			trackerLog.WarnContext(ctx, "branch watch: recording a moved base on its pull requests failed; the move is processed again next cycle",
				"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "pull_requests", len(owed), "error", err)
			return
		}
	}
	if c.outcome != "" {
		recordBaseMoveOutcome(c.outcome)
		if c.outcome == baseMoveOverlap {
			recordOverlapSetSize(len(owed), len(prs))
		}
		trackerLog.DebugContext(ctx, "branch watch: base moved", "repository_id", c.ref.RepositoryID, "branch", c.ref.Branch,
			"outcome", c.outcome, "open", len(prs), "owed", len(owed))
	}
	if _, err := t.mergeability.UpsertBranchTipSystem(ctx, orgID, domain.BranchTip{
		RepositoryID: c.ref.RepositoryID, Branch: c.ref.Branch, TipSHA: c.tip, ETag: c.etag, ObservedAt: time.Now().UTC(),
	}); err != nil {
		trackerLog.WarnContext(ctx, "branch watch: storing a branch's tip failed; its move is processed again next cycle",
			"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "error", err)
	}
}
