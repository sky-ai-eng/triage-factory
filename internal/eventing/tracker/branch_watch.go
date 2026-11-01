package tracker

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// The branch watch: a base branch's move can change the mergeability of the
// pull requests on it without moving their heads or updated_at, so the poll
// watches each base branch's tip and probes the pull requests a move can have
// affected. It is the only writer of branch_tips and of the pull requests'
// changed paths.

// basePR is one open pull request on a watched base branch.
type basePR struct {
	entity domain.Entity
	nodeID string
	// head is the pull request's current head: the refresh's this cycle, the
	// listing's, or the stored snapshot's, in that order.
	head string
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
// requests target in the pass's repositories, and admits mergeability probes
// for the pull requests a move can have affected (see describeMove and
// admitBaseMove). A branch is checked when its repository's open-PR listing
// answered 200 this cycle, since a merged pull request leaves the listing, or
// when it was last checked longer than staleRefreshFloor ago, which catches a
// push that changes no pull request. Branches no open pull request in the org
// targets lose their row, whichever pass sees it.
//
// The base is the stored snapshot's base_ref; open is every open pull request
// the cycle knows, and a pull request the refresh found terminal is left out.
// A pull request in a repository no pass is handed (one no team tracks any
// more) is left to the sweep. Failures are logged and leave the stored tip, so
// the move is processed again next cycle.
func (t *Tracker) watchBaseBranches(ctx context.Context, client *ghclient.Client, orgID string, open []mergeabilityCandidate, scope baseWatchScope, listed, refreshed map[string]domain.PRSnapshot) {
	bases := map[domain.BranchRef][]basePR{}
	for _, c := range open {
		if c.snap.BaseRef == "" || c.entity.RepositoryID == "" {
			continue
		}
		pr := basePR{entity: c.entity, nodeID: c.nodeID, head: c.snap.HeadSHA}
		if snap, ok := listed[c.nodeID]; ok && snap.HeadSHA != "" {
			pr.head = snap.HeadSHA
		}
		if snap, ok := refreshed[c.nodeID]; ok {
			if prSnapshotTerminal(snap) {
				continue
			}
			if snap.HeadSHA != "" {
				pr.head = snap.HeadSHA
			}
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
			t.admitBaseMove(ctx, orgID, c, bases[c.ref], paths)
		}
	}
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
		if err != nil {
			t.logBaseReadFailure(ctx, c, "comparing the branch's previous tip with its new one", err, rateLimited)
			return
		}
		c.done, c.outcome, c.movePaths = true, outcome, paths
	}
}

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
//     files;
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
// Any other failure is err, and the move is processed again next cycle.
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
	if len(cmp.Files) < ghclient.CompareFileCap {
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
		if len(step.Files) >= ghclient.CompareFileCap {
			return baseMoveTruncated, nil, nil
		}
		paths = append(paths, step.Files...)
		cur = ps[0]
	}
	return baseMoveOverlap, paths, nil
}

// overlapPaths reads the changed paths of every open pull request on a base
// whose move is an overlap, keyed by entity id: the stored list when it was
// read at the pull request's current head, and a fresh read otherwise, which
// is written back (UpsertPathsSystem). A pull request missing from the result
// could not be read this cycle.
func (t *Tracker) overlapPaths(ctx context.Context, client *ghclient.Client, orgID string, checks []baseCheck, bases map[domain.BranchRef][]basePR) map[string]domain.PRPaths {
	var ids []string
	for _, c := range checks {
		if c.done && c.outcome == baseMoveOverlap {
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
		if !c.done || c.outcome != baseMoveOverlap {
			continue
		}
		key := repoKey{c.owner, c.repo}
		for _, pr := range bases[c.ref] {
			if pr.entity.ExternalID == "" || seen[pr.entity.ID] {
				continue
			}
			seen[pr.entity.ID] = true
			if row, ok := stored[pr.entity.ID]; ok && row.Paths != nil && row.HeadSHA == pr.head {
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
			paths := domain.PRPaths{EntityID: pr.entity.ID, OrgID: orgID, Paths: read.Paths, HeadSHA: read.HeadSHA, Complete: read.Complete}
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
			if paths.HeadSHA == "" {
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

// admitBaseMove admits the probes one checked branch owes and then stores its
// tip, in that order: a failure between the two leaves the previous tip, so
// the next cycle processes the move again and admission collapses the repeat.
//
//   - no move: the tip's ETag and observed time are written;
//   - identical: nothing is probed;
//   - a reset: every open pull request on the base, base_reset;
//   - an overlap: base_moved for each pull request whose paths overlap the
//     move's, and for each whose paths could not be read or are incomplete.
func (t *Tracker) admitBaseMove(ctx context.Context, orgID string, c baseCheck, prs []basePR, paths map[string]domain.PRPaths) {
	var probes []db.PRMergeabilityProbe
	switch c.outcome {
	case "", baseMoveIdentical:
	case baseMoveOverlap:
		for _, pr := range prs {
			if pr.entity.ExternalID == "" {
				continue
			}
			p, ok := paths[pr.entity.ID]
			if !ok || !p.Complete || overlaps(p.Paths, c.movePaths) {
				probes = append(probes, db.PRMergeabilityProbe{EntityID: pr.entity.ID, Reason: domain.MergeabilityProbeBaseMoved})
			}
		}
	default:
		for _, pr := range prs {
			if pr.entity.ExternalID != "" {
				probes = append(probes, db.PRMergeabilityProbe{EntityID: pr.entity.ID, Reason: domain.MergeabilityProbeBaseReset})
			}
		}
	}
	if len(probes) > 0 {
		if _, err := t.mergeability.AdmitSystem(ctx, orgID, probes); err != nil {
			trackerLog.WarnContext(ctx, "branch watch: admitting a moved base's probes failed; the move is processed again next cycle",
				"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "probes", len(probes), "error", err)
			return
		}
	}
	if c.outcome != "" {
		recordBaseMove(c.outcome)
		if c.outcome == baseMoveOverlap {
			recordOverlapSetSize(len(probes), len(prs))
		}
		trackerLog.DebugContext(ctx, "branch watch: base moved", "repository_id", c.ref.RepositoryID, "branch", c.ref.Branch,
			"outcome", c.outcome, "open", len(prs), "probes", len(probes))
	}
	if _, err := t.mergeability.UpsertBranchTipSystem(ctx, orgID, domain.BranchTip{
		RepositoryID: c.ref.RepositoryID, Branch: c.ref.Branch, TipSHA: c.tip, ETag: c.etag, ObservedAt: time.Now().UTC(),
	}); err != nil {
		trackerLog.WarnContext(ctx, "branch watch: storing a branch's tip failed; its move is processed again next cycle",
			"repository_id", c.ref.RepositoryID, "branch", c.ref.Branch, "error", err)
	}
}
