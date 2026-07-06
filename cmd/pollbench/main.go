// Command pollbench benchmarks the GitHub polling path at large-org shape:
// it generates a deterministic synthetic org, serves it from a fake GitHub
// API (REST + GraphQL, with ETag conditional requests and optional primary
// rate limiting), points a real poller.Manager + tracker over real
// in-memory-SQLite stores at it, and reports what a cold-start sync and a
// warm follow-up cycle cost.
//
// Standalone binary, like cmd/sandbox-bench — not wired into the root CLI
// dispatch. See docs/poll-bench.md for usage and recorded baselines.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/pollbench"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// shapes are the standard benchmark shapes. dp1 approximates the large
// single-org GHES profile (hundreds of repos, thousands of open PRs);
// numbers are placeholders until the partner's real counts replace them —
// see docs/poll-bench.md.
var shapes = map[string]struct {
	repos   int
	prsMean float64
}{
	"smoke":  {repos: 20, prsMean: 5},   // CI tripwire, ~100 PRs
	"dp1":    {repos: 300, prsMean: 15}, // design-partner target, ~4,500 PRs
	"stress": {repos: 600, prsMean: 20}, // headroom check, ~12,000 PRs
}

func main() {
	var (
		shape           = flag.String("shape", "", "preset shape: smoke, dp1, or stress (sets -repos and -prs-mean; explicit flags win)")
		repos           = flag.Int("repos", 300, "number of tracked repos")
		prsMean         = flag.Float64("prs-mean", 15, "mean open PRs per repo (mixed distribution with a pathological tail)")
		commentsMean    = flag.Int("comments-mean", 5, "mean comments per PR")
		filesMean       = flag.Int("files-mean", 8, "mean changed files per PR")
		inflightPct     = flag.Float64("inflight-pct", 0, "fraction of PRs with an in-flight check run (forces warm-cycle refreshes)")
		conflictPct     = flag.Float64("conflict-pct", 0.02, "fraction of PRs in merge-conflict state")
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

	// A shape preset fills -repos / -prs-mean unless they were set explicitly.
	if *shape != "" {
		preset, ok := shapes[*shape]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown -shape %q (want smoke, dp1, or stress)\n", *shape)
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
	}

	// The modeled deployment is a multi-mode GHES install; multi is also the
	// polling path with no local-user dashboard backfill inflating counts.
	if err := runmode.Init(runmode.ModeMulti); err != nil {
		fmt.Fprintf(os.Stderr, "init runmode: %v\n", err)
		os.Exit(1)
	}

	cfg := pollbench.RunConfig{
		Dataset: pollbench.DatasetConfig{
			Repos:        *repos,
			AvgPRs:       *prsMean,
			CommentsMean: *commentsMean,
			FilesMean:    *filesMean,
			InflightPct:  *inflightPct,
			ConflictPct:  *conflictPct,
			Seed:         *seed,
		},
		Server: pollbench.ServerConfig{
			Latency:         *latency,
			RateLimit:       *rateLimit,
			RateLimitWindow: *rateLimitWindow,
		},
		WarmCycles: *warmCycles,
		MaxCycles:  *maxCycles,
	}

	name := *shape
	if name == "" {
		name = "custom"
	}
	fmt.Printf("poll-bench  shape=%s seed=%d latency=%s rate-limit=%s\n",
		name, *seed, *latency, rateLimitDesc(*rateLimit, *rateLimitWindow))

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
		if !requestCountsEqual(res, res2) {
			failed = true
			fmt.Println("check: FAIL — request counts differ between identically-seeded runs")
		} else {
			fmt.Println("check: request counts identical across runs")
		}
		if *rateLimit == 0 && *warmCycles > 0 {
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

// requestCountsEqual compares the per-cycle request-counter deltas of two
// runs. Wall time and allocation counts legitimately vary; request counts
// must not (with rate limiting off — retries under a live rate limit are
// timing-dependent, so -check is meant for non-rate-limited configs).
func requestCountsEqual(a, b *pollbench.Result) bool {
	if len(a.Cycles) != len(b.Cycles) {
		return false
	}
	for i := range a.Cycles {
		if !reflect.DeepEqual(a.Cycles[i].Requests, b.Cycles[i].Requests) {
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
