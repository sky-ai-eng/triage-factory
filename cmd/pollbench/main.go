// Command pollbench benchmarks the GitHub polling path at large-org shape:
// it generates a deterministic synthetic org, serves it from a fake GitHub
// API (REST + GraphQL, with ETag conditional requests, lazy mergeability and
// optional primary rate limiting), points a real poller.Manager + tracker and
// the mergeability probe worker over real in-memory-SQLite stores at it, and
// reports what a cold-start sync and the warm cycles after it cost, base
// moves and head pushes included.
//
// Standalone binary, like cmd/sandbox-bench — not wired into the root CLI
// dispatch. See docs/contributors/benchmarks/poll-bench.md for usage and recorded baselines.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/internal/pollbench"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// shapePreset is a shape: the dataset, and what changes during the warm cycles
// when base moves are on.
type shapePreset struct {
	repos      int
	prsMean    float64
	prsPerRepo int
	fileCounts []pollbench.Quantile
	tree       pollbench.TreeConfig
	warmCycles int
	baseMoves  bool
	moveEvery  int
	moveRepo   float64
	headPush   float64
}

// The cadence of base moves in the shapes other than mono: every 10 warm
// cycles on 10% of the repos, each changing its repo's listing as a merge
// does.
const (
	defaultMoveEvery   = 10
	defaultMoveRepoPct = 0.10
)

// shapes are the standard benchmark shapes. dp1 approximates the large
// single-org GHES profile (hundreds of repos, thousands of open PRs);
// numbers are placeholders until a real deployment's counts replace them —
// see docs/contributors/benchmarks/poll-bench.md. mono is one large monorepo
// shaped on kubernetes/kubernetes, its main moving every 2 warm cycles.
var shapes = map[string]shapePreset{
	"smoke":  {repos: 20, prsMean: 5, warmCycles: 1, moveEvery: defaultMoveEvery, moveRepo: defaultMoveRepoPct},   // CI tripwire, ~100 PRs
	"dp1":    {repos: 300, prsMean: 15, warmCycles: 1, moveEvery: defaultMoveEvery, moveRepo: defaultMoveRepoPct}, // large-org GHES target, ~4,500 PRs
	"stress": {repos: 600, prsMean: 20, warmCycles: 1, moveEvery: defaultMoveEvery, moveRepo: defaultMoveRepoPct}, // headroom check, ~12,000 PRs
	"mono": {
		repos: 1, prsPerRepo: 1500, fileCounts: pollbench.KubernetesFileCounts,
		tree:       pollbench.MonorepoTree,
		warmCycles: 100, baseMoves: true, moveEvery: 2, moveRepo: 1, headPush: 0.01,
	},
}

func main() {
	var (
		shape           = flag.String("shape", "", "preset shape: smoke, dp1, stress, or mono (sets -repos, -prs-mean, -warm-cycles and -base-moves; explicit flags win)")
		repos           = flag.Int("repos", 300, "number of tracked repos")
		prsMean         = flag.Float64("prs-mean", 15, "mean open PRs per repo (mixed distribution with a pathological tail)")
		commentsMean    = flag.Int("comments-mean", 5, "mean comments per PR")
		filesMean       = flag.Int("files-mean", 8, "mean changed files per PR")
		inflightPct     = flag.Float64("inflight-pct", 0, "fraction of PRs with an in-flight check run (forces warm-cycle refreshes)")
		conflictPct     = flag.Float64("conflict-pct", 0.02, "fraction of PRs in merge-conflict state when the run starts")
		baseMoves       = flag.Bool("base-moves", false, "land base moves during the warm cycles (on by default for mono: every 2 cycles; otherwise every 10 cycles on 10% of repos)")
		conflictOverlap = flag.Float64("conflict-on-overlap", 0.03, "chance a base move makes a PR whose paths it overlaps conflicting")
		latency         = flag.Duration("latency", 30*time.Millisecond, "injected per-request latency (GHES-in-VPC round trip)")
		rateLimit       = flag.Int("rate-limit", 0, "API request budget per window; 0 = unlimited")
		rateLimitWindow = flag.Duration("rate-limit-window", time.Minute, "rate-limit window (GHES uses 1h; short windows keep bench runs fast)")
		seed            = flag.Int64("seed", 42, "dataset seed; same seed + shape → same request counts")
		warmCycles      = flag.Int("warm-cycles", 1, "warm cycles to run after the cold sync completes")
		maxCycles       = flag.Int("max-cycles", 20, "cold-sync cycle cap when a rate limit forces resumption")
		check           = flag.Bool("check", false, "CI mode: run twice, assert determinism (identical request counts) and sanity, exit non-zero on failure")
		verbose         = flag.Bool("v", false, "keep info-level component logs (schema bootstrap, per-cycle tracker lines)")
	)
	flag.Parse()

	if !*verbose {
		logging.SetLevel(slog.LevelWarn)
	}

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q (flags only)\n", flag.Arg(0))
		os.Exit(2)
	}

	// A shape preset fills -repos / -prs-mean / -warm-cycles / -base-moves
	// unless they were set explicitly. A custom run moves bases at the
	// default cadence.
	preset := shapePreset{moveEvery: defaultMoveEvery, moveRepo: defaultMoveRepoPct}
	if *shape != "" {
		var ok bool
		if preset, ok = shapes[*shape]; !ok {
			fmt.Fprintf(os.Stderr, "unknown -shape %q (want smoke, dp1, stress, or mono)\n", *shape)
			os.Exit(2)
		}
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		if !explicit["repos"] {
			*repos = preset.repos
		}
		if !explicit["prs-mean"] {
			*prsMean = preset.prsMean
		}
		if !explicit["warm-cycles"] {
			*warmCycles = preset.warmCycles
		}
		if !explicit["base-moves"] {
			*baseMoves = preset.baseMoves
		}
	}

	// The modeled deployment is a multi-mode GHES install; multi is also the
	// polling path with no local-user dashboard backfill inflating counts.
	// The poller/tracker is a brain component, so this process models the
	// control role — multi mode has no fused single-process role to fall
	// back to.
	if err := runmode.Init(runmode.ModeMulti); err != nil {
		fmt.Fprintf(os.Stderr, "init runmode: %v\n", err)
		os.Exit(1)
	}
	if err := runmode.InitRole(runmode.RoleControl); err != nil {
		fmt.Fprintf(os.Stderr, "init role: %v\n", err)
		os.Exit(1)
	}

	cfg := pollbench.RunConfig{
		Dataset: pollbench.DatasetConfig{
			Repos:        *repos,
			AvgPRs:       *prsMean,
			PRsPerRepo:   preset.prsPerRepo,
			CommentsMean: *commentsMean,
			FilesMean:    *filesMean,
			FileCounts:   preset.fileCounts,
			Tree:         preset.tree,
			InflightPct:  *inflightPct,
			ConflictPct:  *conflictPct,
			Seed:         *seed,
		},
		Server: pollbench.ServerConfig{
			Latency:           *latency,
			RateLimit:         *rateLimit,
			RateLimitWindow:   *rateLimitWindow,
			ConflictOnOverlap: *conflictOverlap,
		},
		Scenario: pollbench.Scenario{
			BaseMoves:   *baseMoves,
			MoveEvery:   preset.moveEvery,
			MoveRepoPct: preset.moveRepo,
			HeadPushPct: preset.headPush,
		},
		WarmCycles: *warmCycles,
		MaxCycles:  *maxCycles,
	}

	name := *shape
	if name == "" {
		name = "custom"
	}
	fmt.Printf("poll-bench  shape=%s seed=%d latency=%s rate-limit=%s base-moves=%t\n",
		name, *seed, *latency, rateLimitDesc(*rateLimit, *rateLimitWindow), *baseMoves)

	res, err := pollbench.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pollbench: %v\n", err)
		os.Exit(1)
	}
	res.Render(os.Stdout)

	failed := len(res.SanityErrors) > 0

	if *check {
		fmt.Println("\ncheck: re-running for determinism comparison…")
		res2, err := pollbench.Run(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pollbench (second run): %v\n", err)
			os.Exit(1)
		}
		failed = failed || len(res2.SanityErrors) > 0
		for _, s := range res2.SanityErrors {
			fmt.Printf("check: second run sanity FAIL — %s\n", s)
		}
		if *rateLimit > 0 {
			// Retry counts and even cycles-to-complete legitimately vary with
			// timing under an active rate limit; only the sanity assertions
			// are meaningful, so don't fail on count differences.
			fmt.Println("check: skipping request-count comparison (rate limit active — retry timing varies run-to-run)")
		} else if !requestCountsEqual(res, res2) {
			failed = true
			fmt.Println("check: FAIL — request counts differ between identically-seeded runs")
		} else {
			fmt.Println("check: request counts identical across runs")
		}
		// Base moves and head pushes change listings on purpose.
		if *rateLimit == 0 && *warmCycles > 0 && !cfg.Scenario.BaseMoves && cfg.Scenario.HeadPushPct == 0 {
			if cond, hits := res.Warm304(); hits != cond {
				failed = true
				fmt.Printf("check: FAIL — warm 304 hit rate %d/%d, expected all conditional listings to hit\n", hits, cond)
			}
		}
	}

	if failed {
		os.Exit(1)
	}
}

// requestCountsEqual compares the per-cycle request-counter deltas (test
// merges and points included) and probes of two runs. Wall time and
// allocation counts legitimately vary; counts must not. Only called with rate limiting off — the -check flow skips the
// comparison under an active rate limit, where retry counts and
// cycles-to-complete are timing-dependent.
func requestCountsEqual(a, b *pollbench.Result) bool {
	if len(a.Cycles) != len(b.Cycles) {
		return false
	}
	for i := range a.Cycles {
		if !reflect.DeepEqual(a.Cycles[i].Requests, b.Cycles[i].Requests) || !reflect.DeepEqual(a.Cycles[i].Probes, b.Cycles[i].Probes) {
			return false
		}
	}
	return true
}

func rateLimitDesc(limit int, window time.Duration) string {
	if limit <= 0 {
		return "off"
	}
	return fmt.Sprintf("%d/%s", limit, window)
}
