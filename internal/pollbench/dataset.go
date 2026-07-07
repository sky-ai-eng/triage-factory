// Package pollbench is the poll-scale benchmark harness: a deterministic
// fake GitHub API server plus a driver that points a real poller.Manager +
// tracker.Tracker at it and measures a cold-start sync at large-org shape
// (hundreds of repos, thousands of open PRs — the GHES design-partner
// profile). See docs/poll-bench.md for how to run it and cmd/pollbench for
// the CLI entrypoint.
//
// The harness exists to answer "what does the first full poll of a large
// tracked set cost, and what do conditional requests save on the second?"
// — and to measure before/after for rate-limit backoff, bounded refresh
// concurrency, and budgeted-cycle work on the polling path.
package pollbench

import (
	"fmt"
	"math/rand"
)

// DatasetConfig shapes the synthetic org. Every field feeds the seeded RNG,
// so equal configs generate byte-identical datasets (and therefore identical
// request counts — wall time is the only thing allowed to vary run-to-run).
type DatasetConfig struct {
	// Repos is the number of tracked repos, all under one owner (Owner) —
	// the single-GitHub-org shape a GHES deployment polls.
	Repos int

	// AvgPRs is the target mean open-PR count per repo. PR counts are drawn
	// from a mixed distribution around this mean: ~70% quiet repos below the
	// mean, ~25% busy repos up to 3×, and ~5% pathological repos at 6–10× —
	// the tail is what exercises multi-page REST listings (>100 open PRs).
	AvgPRs float64

	// CommentsMean / FilesMean shape the per-PR comment and changed-file
	// counts. On the tracker path these surface only as snapshot integers
	// (comments.totalCount, changedFiles), so they affect payload size and
	// diff work, not request counts.
	CommentsMean int
	FilesMean    int

	// InflightPct is the fraction of PRs carrying one in-flight (not yet
	// completed) check run. In-flight CI forces a refresh every cycle (the
	// gate can't trust a snapshot with running checks), so this directly
	// sets the floor of warm-cycle GraphQL work. 0 gives the cleanest
	// warm-cycle conditional-request measurement.
	InflightPct float64

	// ConflictPct is the fraction of PRs whose GraphQL mergeable state is
	// CONFLICTING. Each one emits a github:pr:conflicts event on the first
	// full refresh (REST discovery has no mergeable field, so the seeded
	// snapshot's empty value diffs against it).
	ConflictPct float64

	// Seed drives all randomness. Same seed + same config → same dataset.
	Seed int64

	// Owner is the GitHub org login every repo lives under.
	Owner string
}

// Check is one check run on a PR's head commit.
type Check struct {
	ID            int64
	Name          string
	Status        string // "completed" or "in_progress"
	Conclusion    string // "success" / "failure" / "" while in flight
	CompletedAt   string
	WorkflowRunID int64
}

// Review is one latest-review-per-author entry on a PR.
type Review struct {
	ID          string
	Author      string
	State       string // APPROVED / CHANGES_REQUESTED / COMMENTED
	SubmittedAt string
}

// PR is one synthetic open pull request. The same struct backs both the
// REST listing (discovery) and the GraphQL node (refresh), so the two views
// agree on the fields the tracker's gate compares (updatedAt, head SHA).
type PR struct {
	Number         int
	NodeID         string
	Title          string
	Author         string
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	CreatedAt      string
	UpdatedAt      string
	Mergeable      string // GraphQL only: MERGEABLE / CONFLICTING
	Additions      int
	Deletions      int
	ChangedFiles   int
	CommentCount   int
	Labels         []string
	ReviewRequests []string // requested reviewer logins
	Reviews        []Review
	Checks         []Check
}

// Repo is one tracked repo and its open PRs.
type Repo struct {
	Owner string
	Name  string
	PRs   []PR
}

// FullName returns the "owner/repo" slug.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Dataset is the full synthetic org the fake server serves.
type Dataset struct {
	Cfg      DatasetConfig
	Repos    []Repo
	TotalPRs int

	// byNodeID maps a PR's GraphQL node ID to its location, for the
	// nodes(ids:) refresh lookup.
	byNodeID map[string][2]int // nodeID → (repo index, pr index)
}

// Generate builds the synthetic org from cfg. Deterministic: all draws come
// from one seeded RNG consumed in a fixed order.
func Generate(cfg DatasetConfig) *Dataset {
	if cfg.Owner == "" {
		cfg.Owner = "benchorg"
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	ds := &Dataset{
		Cfg:      cfg,
		Repos:    make([]Repo, 0, cfg.Repos),
		byNodeID: make(map[string][2]int),
	}

	for ri := 0; ri < cfg.Repos; ri++ {
		repo := Repo{Owner: cfg.Owner, Name: fmt.Sprintf("repo-%04d", ri)}
		n := drawPRCount(rng, cfg.AvgPRs)
		repo.PRs = make([]PR, 0, n)
		for pi := 0; pi < n; pi++ {
			pr := generatePR(rng, cfg, ri, pi)
			ds.byNodeID[pr.NodeID] = [2]int{ri, pi}
			repo.PRs = append(repo.PRs, pr)
		}
		ds.TotalPRs += n
		ds.Repos = append(ds.Repos, repo)
	}
	return ds
}

// drawPRCount draws one repo's open-PR count from the mixed distribution.
// The buckets average out to 1.25× their base mean m, so m is calibrated to
// avg/1.25 to land the configured overall mean.
func drawPRCount(rng *rand.Rand, avg float64) int {
	m := int(avg / 1.25)
	if m < 1 {
		m = 1
	}
	switch p := rng.Float64(); {
	case p < 0.70: // quiet: 0..m
		return rng.Intn(m + 1)
	case p < 0.95: // busy: m..3m
		return m + rng.Intn(2*m+1)
	default: // pathological tail: 6m..10m — exercises >100-PR pagination at dp1 scale
		return 6*m + rng.Intn(4*m+1)
	}
}

var (
	labelPool = []string{
		"bug", "enhancement", "docs", "dependencies", "backend", "frontend",
		"security", "performance", "breaking-change", "needs-triage", "ci", "refactor",
	}
	reviewStates = []string{"APPROVED", "APPROVED", "COMMENTED", "COMMENTED", "CHANGES_REQUESTED"}
)

// benchEpoch anchors every generated timestamp. Fixed (not time.Now) so a
// dataset is byte-identical across runs of the same seed.
const benchEpoch = "2026-06-0%dT%02d:%02d:%02dZ"

func stamp(rng *rand.Rand) string {
	return fmt.Sprintf(benchEpoch, 1+rng.Intn(9), rng.Intn(24), rng.Intn(60), rng.Intn(60))
}

func generatePR(rng *rand.Rand, cfg DatasetConfig, repoIdx, prIdx int) PR {
	number := prIdx + 1
	pr := PR{
		Number:       number,
		NodeID:       fmt.Sprintf("BPR_%d_%d", repoIdx, number),
		Title:        fmt.Sprintf("bench: change %d in repo-%04d", number, repoIdx),
		Author:       fmt.Sprintf("dev-%03d", rng.Intn(200)),
		HeadRef:      fmt.Sprintf("feature/change-%d", number),
		BaseRef:      "main",
		HeadSHA:      fmt.Sprintf("%040x", rng.Int63()),
		CreatedAt:    stamp(rng),
		UpdatedAt:    stamp(rng),
		Mergeable:    "MERGEABLE",
		Additions:    rng.Intn(500),
		Deletions:    rng.Intn(200),
		ChangedFiles: intAround(rng, cfg.FilesMean),
		CommentCount: intAround(rng, cfg.CommentsMean),
	}
	if rng.Float64() < cfg.ConflictPct {
		pr.Mergeable = "CONFLICTING"
	}

	// Labels: ~half of PRs carry 1–3.
	if rng.Float64() < 0.5 {
		for _, li := range rng.Perm(len(labelPool))[:1+rng.Intn(3)] {
			pr.Labels = append(pr.Labels, labelPool[li])
		}
	}

	// Requested reviewers: ~40% of PRs have 1–2. None resolve to a TF-known
	// identity in the harness (empty identity stores), so these exercise the
	// resolver lookup path without emitting review_requested events.
	if rng.Float64() < 0.4 {
		for i := 0; i < 1+rng.Intn(2); i++ {
			pr.ReviewRequests = append(pr.ReviewRequests, fmt.Sprintf("reviewer-%03d", rng.Intn(150)))
		}
	}

	// Latest reviews: ~45% of PRs have 1–3, distinct authors (GitHub's
	// latestReviews is one entry per reviewer).
	if rng.Float64() < 0.45 {
		n := 1 + rng.Intn(3)
		for i := 0; i < n; i++ {
			pr.Reviews = append(pr.Reviews, Review{
				ID:          fmt.Sprintf("REV_%d_%d_%d", repoIdx, number, i),
				Author:      fmt.Sprintf("rev-%d-%d", number%50, i), // distinct within the PR
				State:       reviewStates[rng.Intn(len(reviewStates))],
				SubmittedAt: stamp(rng),
			})
		}
	}

	// Check runs: 2–7 per PR, unique names (the snapshot dedups by name).
	// ~15% of PRs have one failing check; InflightPct carry one still-running.
	nChecks := 2 + rng.Intn(6)
	failing := rng.Float64() < 0.15
	inflight := rng.Float64() < cfg.InflightPct
	for i := 0; i < nChecks; i++ {
		check := Check{
			ID:            int64(repoIdx)*1_000_000 + int64(number)*100 + int64(i),
			Name:          fmt.Sprintf("ci/job-%d", i),
			Status:        "completed",
			Conclusion:    "success",
			CompletedAt:   stamp(rng),
			WorkflowRunID: int64(repoIdx)*1_000_000 + int64(number)*10 + int64(i),
		}
		if failing && i == 0 {
			check.Conclusion = "failure"
		}
		if inflight && i == nChecks-1 {
			check.Status = "in_progress"
			check.Conclusion = ""
			check.CompletedAt = ""
		}
		pr.Checks = append(pr.Checks, check)
	}

	return pr
}

// intAround draws a value in [0, 2·mean] — uniform around the mean, floored
// at 0 for a zero mean.
func intAround(rng *rand.Rand, mean int) int {
	if mean <= 0 {
		return 0
	}
	return rng.Intn(2*mean + 1)
}

// RepoNames returns the "owner/repo" slugs to configure as the tracked set.
func (d *Dataset) RepoNames() []string {
	names := make([]string, len(d.Repos))
	for i, r := range d.Repos {
		names[i] = r.FullName()
	}
	return names
}

// ExpectedColdEvents computes the number of events one full cold-start sync
// must emit. Discovery seeds each PR's snapshot from the REST listing (no
// CI, no reviews, no mergeable state); the same cycle's GraphQL refresh then
// diffs the full snapshot against that seed, emitting:
//
//   - one ci_check_failed / ci_check_passed per COMPLETED check run
//     (in-flight checks have no conclusion yet → no event),
//   - one review event per latest review (the seeded snapshot has none),
//   - one pr:conflicts per PR whose mergeable state is CONFLICTING.
//
// Review-request events don't fire: no requested reviewer resolves to a
// TF-known identity against the harness's empty identity stores. Labels,
// head SHA, and updatedAt are identical in both views, so they diff clean.
func (d *Dataset) ExpectedColdEvents() int {
	total := 0
	for _, repo := range d.Repos {
		for _, pr := range repo.PRs {
			for _, c := range pr.Checks {
				if c.Status == "completed" && c.Conclusion != "" {
					total++
				}
			}
			total += len(pr.Reviews)
			if pr.Mergeable == "CONFLICTING" {
				total++
			}
		}
	}
	return total
}

// MaxOpenPRs returns the largest per-repo open-PR count — >100 means the
// REST listing paginates, which the pathological tail is meant to force.
func (d *Dataset) MaxOpenPRs() int {
	m := 0
	for _, r := range d.Repos {
		if len(r.PRs) > m {
			m = len(r.PRs)
		}
	}
	return m
}
