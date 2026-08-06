package pollbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// RunConfig is one benchmark run: a dataset shape, the fake server's
// behavior, and how many cycles to drive.
type RunConfig struct {
	Dataset DatasetConfig
	Server  ServerConfig

	// WarmCycles is how many additional cycles to run after the cold sync
	// completes — the conditional-request (ETag 304) measurement.
	WarmCycles int

	// MaxCycles bounds the cold-sync resume loop when a rate limit forces
	// the sync across multiple cycles.
	MaxCycles int
}

// CycleMetrics is what one driven poll cycle cost.
type CycleMetrics struct {
	Label      string
	Wall       time.Duration
	Requests   Counters // request-count delta for this cycle
	Events     int      // github:* events emitted this cycle
	Entities   int      // active github entities after this cycle
	AllocBytes uint64   // Go heap bytes allocated during this cycle
	Errors     []string // poll-cycle errors surfaced via Manager.OnError
}

// Result is the full benchmark output.
type Result struct {
	Repos              int
	TotalPRs           int
	MaxOpenPRs         int
	ExpectedColdEvents int

	Cycles []CycleMetrics

	// ColdCycles is how many cycles the cold sync took to complete (1 unless
	// a rate limit forced resumption); ColdSyncWall is the summed cold-cycle
	// wall time plus any between-cycle waits for a rate-limit window to
	// reset — harness bookkeeping between cycles is excluded.
	ColdCycles   int
	ColdSyncWall time.Duration
	ColdEvents   int
	WarmEvents   int

	PeakRSSBytes int64          // process high-water RSS; 0 when the platform doesn't expose it
	EventTypes   map[string]int // emitted github:* events by type, whole run

	// SanityErrors is non-empty when the run's behavior diverged from what
	// the dataset predicts: event count mismatch, incomplete sync,
	// warm-cycle noise, requests to endpoints the fake doesn't model, or an
	// unexpected poll-cycle error (anything but a rate-limit interrupt on a
	// rate-limited run). An empty slice is the "harness observed exactly the
	// expected transitions" signal the metrics are only meaningful under.
	SanityErrors []string
}

// countingPublisher is the tracker.Publisher for the bench. Counting happens
// synchronously in Publish — unlike a bus subscription there is no buffer to
// overflow, so event counts are exact even at stress shape. The bench
// deliberately stops at the tracker boundary: routing/task creation is not
// part of what a poll cycle costs.
type countingPublisher struct {
	mu     sync.Mutex
	total  int
	byType map[string]int
}

func (p *countingPublisher) Publish(_ context.Context, evt domain.Event) {
	if !strings.HasPrefix(evt.EventType, "github:") {
		return // system:poll:* sentinels aren't tracked-transition events
	}
	p.mu.Lock()
	p.total++
	if p.byType == nil {
		p.byType = make(map[string]int)
	}
	p.byType[evt.EventType]++
	p.mu.Unlock()
}

func (p *countingPublisher) snapshot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

func (p *countingPublisher) types() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.byType))
	for k, v := range p.byType {
		out[k] = v
	}
	return out
}

// benchResolver satisfies ghclient.Resolver with a single fixed client
// pointed at the fake server — the poll path resolves credentials through
// this seam, and the bench substitutes "whatever you ask for, here's the
// bench PAT client" (same pattern the poller's own tests use).
type benchResolver struct {
	client *ghclient.Client
	base   string
}

func (r *benchResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *benchResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *benchResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *benchResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return r.base, nil
}

func (r *benchResolver) OrgIdentityFor(ctx context.Context, orgID string) (name, email string, ok bool) {
	return "", "", false
}

// Run executes one benchmark: generate the dataset, start the fake server,
// wire a real Manager over real (in-memory SQLite) stores, drive the cold
// sync to completion plus WarmCycles warm cycles, and report.
//
// The caller must have initialized runmode to multi before calling — the
// scenario being modeled is a multi-mode GHES deployment, and multi is the
// path where polling carries no local-user perspective (no dashboard
// backfill searches inflating the request counts).
func Run(cfg RunConfig) (*Result, error) {
	if runmode.Current() != runmode.ModeMulti {
		return nil, fmt.Errorf("pollbench requires runmode multi (got %q): call runmode.Init(runmode.ModeMulti) first", runmode.Current())
	}
	if cfg.MaxCycles <= 0 {
		cfg.MaxCycles = 20
	}

	ds := Generate(cfg.Dataset)
	srv := NewServer(ds, cfg.Server)
	defer srv.Close()

	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID

	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(database); err != nil {
		return nil, fmt.Errorf("bootstrap schema: %w", err)
	}

	stores := sqlitestore.New(database)
	if err := stores.Repos.SetConfigured(ctx, orgID, ds.RepoNames()); err != nil {
		return nil, fmt.Errorf("configure repos: %w", err)
	}

	pub := &countingPublisher{}
	resolver := &benchResolver{
		client: ghclient.NewClient(srv.URL(), "bench-pat"),
		base:   srv.URL(),
	}
	mgr := poller.NewManager(database, pub,
		stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Orgs,
		stores.JiraStatusRules, stores.TeamGitHubGroups, stores.Secrets, stores.GitHubApps,
		resolver)

	// Poll-cycle errors surfaced via OnError are classified at capture: an
	// ErrRateLimited on a run that configured a rate limit is the expected
	// interrupt-and-resume behavior (still shown in the report); anything
	// else is a real failure and must fail the run — a bug that doesn't
	// happen to change the final entity/event counts would otherwise print
	// its evidence right next to "sanity: OK".
	type cycleErr struct {
		msg      string
		expected bool
	}
	var cycleErrs []cycleErr
	var cycleErrsMu sync.Mutex
	mgr.OnError = func(source, org string, err error) {
		var rl *ghclient.ErrRateLimited
		cycleErrsMu.Lock()
		cycleErrs = append(cycleErrs, cycleErr{
			msg:      fmt.Sprintf("%s: %v", source, err),
			expected: cfg.Server.RateLimit > 0 && errors.As(err, &rl),
		})
		cycleErrsMu.Unlock()
	}
	takeErrs := func() (display, unexpected []string) {
		cycleErrsMu.Lock()
		defer cycleErrsMu.Unlock()
		for _, e := range cycleErrs {
			display = append(display, e.msg)
			if !e.expected {
				unexpected = append(unexpected, e.msg)
			}
		}
		cycleErrs = nil
		return display, unexpected
	}

	res := &Result{
		Repos:              cfg.Dataset.Repos,
		TotalPRs:           ds.TotalPRs,
		MaxOpenPRs:         ds.MaxOpenPRs(),
		ExpectedColdEvents: ds.ExpectedColdEvents(),
	}

	runCycle := func(label string) CycleMetrics {
		reqBefore := srv.Snapshot()
		evtBefore := pub.snapshot()
		var msBefore runtime.MemStats
		runtime.ReadMemStats(&msBefore)

		start := time.Now()
		mgr.PollGitHubOnce(ctx, orgID)
		wall := time.Since(start)

		var msAfter runtime.MemStats
		runtime.ReadMemStats(&msAfter)
		entities, _, _ := syncState(ctx, stores, orgID, ds.TotalPRs)
		display, unexpected := takeErrs()
		m := CycleMetrics{
			Label:      label,
			Wall:       wall,
			Requests:   countersDelta(reqBefore, srv.Snapshot()),
			Events:     pub.snapshot() - evtBefore,
			Entities:   entities,
			AllocBytes: msAfter.TotalAlloc - msBefore.TotalAlloc,
			Errors:     display,
		}
		for _, e := range unexpected {
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cycle %s: unexpected poll error: %s", label, e))
		}
		res.Cycles = append(res.Cycles, m)
		return m
	}

	// Cold sync: run cycles until every PR has an entity with a fully
	// refreshed snapshot. One cycle unless a rate limit interrupts, in which
	// case the poller's next scheduled cycle resumes where the ETag and
	// snapshot state left off — the driver models "next cycle after the
	// window resets."
	//
	// ColdSyncWall sums the measured cycle walls plus the measured
	// between-cycle waits — NOT time.Since over the whole loop, which would
	// fold harness bookkeeping (the syncState snapshot parse over every
	// entity, counter/memstats snapshots) into a metric that claims to be
	// poll cost. At low latency the bookkeeping would dominate.
	for cycle := 1; ; cycle++ {
		m := runCycle(fmt.Sprintf("cold #%d", cycle))
		res.ColdEvents += m.Events
		res.ColdSyncWall += m.Wall
		entities, refreshed, done := syncState(ctx, stores, orgID, ds.TotalPRs)
		if done {
			res.ColdCycles = cycle
			break
		}
		if cycle >= cfg.MaxCycles {
			res.SanityErrors = append(res.SanityErrors,
				fmt.Sprintf("cold sync incomplete after %d cycles: %d/%d entities, %d fully refreshed",
					cycle, entities, ds.TotalPRs, refreshed))
			break
		}
		if reset := srv.NextReset(); !reset.IsZero() {
			if wait := time.Until(reset) + 100*time.Millisecond; wait > 0 {
				time.Sleep(wait)
				res.ColdSyncWall += wait
			}
		}
	}

	for i := 1; i <= cfg.WarmCycles; i++ {
		m := runCycle(fmt.Sprintf("warm #%d", i))
		res.WarmEvents += m.Events
	}

	res.PeakRSSBytes = peakRSSBytes()
	res.EventTypes = pub.types()

	// Sanity: the cold sync must emit exactly the transitions the dataset
	// predicts, and warm cycles (nothing changed) must emit none. These
	// failing means the harness (or the tracker) isn't doing what this
	// benchmark claims to measure.
	if res.ColdCycles > 0 && res.ColdEvents != res.ExpectedColdEvents {
		res.SanityErrors = append(res.SanityErrors,
			fmt.Sprintf("cold-sync events = %d, expected %d", res.ColdEvents, res.ExpectedColdEvents))
	}
	if res.WarmEvents != 0 {
		res.SanityErrors = append(res.SanityErrors,
			fmt.Sprintf("warm cycles emitted %d events, expected 0", res.WarmEvents))
	}
	// The Other counter is the fake server's bug tripwire: the poll path
	// reached an endpoint the fake doesn't model (path-shape drift, an
	// unknown repo, a GraphQL query that isn't the nodes refresh). The
	// benchmark's numbers can't be trusted to cover the real call set when
	// this fires, so it always fails the run.
	if other := srv.Snapshot().Other; other > 0 {
		res.SanityErrors = append(res.SanityErrors,
			fmt.Sprintf("%d request(s) hit endpoints the fake server doesn't model (other)", other))
	}
	return res, nil
}

// syncState reports cold-sync progress: how many active github entities
// exist, how many carry a fully refreshed snapshot (CheckRuns != nil — the
// discovery seed leaves it nil until the GraphQL refresh lands), and whether
// the sync is complete against the dataset (every expected PR discovered AND
// fully refreshed — a rate-limited cycle can leave either phase partial).
func syncState(ctx context.Context, stores db.Stores, orgID string, expectedTotal int) (entities, refreshed int, done bool) {
	list, err := stores.Entities.ListActiveSystem(ctx, orgID, "github")
	if err != nil {
		return 0, 0, false
	}
	for _, e := range list {
		if e.SnapshotJSON == "" {
			continue
		}
		var snap domain.PRSnapshot
		if json.Unmarshal([]byte(e.SnapshotJSON), &snap) == nil && snap.CheckRuns != nil {
			refreshed++
		}
	}
	entities = len(list)
	return entities, refreshed, entities == expectedTotal && refreshed == entities
}

func countersDelta(before, after Counters) Counters {
	return Counters{
		Total:       after.Total - before.Total,
		PullsList:   after.PullsList - before.PullsList,
		PullsPages:  after.PullsPages - before.PullsPages,
		Pulls304:    after.Pulls304 - before.Pulls304,
		GraphQL:     after.GraphQL - before.GraphQL,
		Teams:       after.Teams - before.Teams,
		RateLimited: after.RateLimited - before.RateLimited,
		Other:       after.Other - before.Other,
	}
}
