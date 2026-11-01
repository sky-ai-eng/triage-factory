package pollbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/mergeability"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// RunConfig is one benchmark run: a dataset shape, the fake server's
// behavior, what changes during the warm cycles, and how many cycles to
// drive.
type RunConfig struct {
	Dataset  DatasetConfig
	Server   ServerConfig
	Scenario Scenario

	// WarmCycles is how many additional cycles to run after the cold sync
	// completes — the conditional-request (ETag 304) measurement.
	WarmCycles int

	// MaxCycles bounds the cold-sync resume loop when a rate limit forces
	// the sync across multiple cycles.
	MaxCycles int
}

// Scenario is what changes on GitHub at the start of each warm cycle, before
// the poll: base moves, which change the moved repositories' listings as a
// merge does, and head pushes. The cold sync sees a world that does not
// move.
type Scenario struct {
	// BaseMoves lands a base move every MoveEvery warm cycles on
	// MoveRepoPct of the repositories, drawn afresh each time.
	BaseMoves   bool
	MoveEvery   int
	MoveRepoPct float64
	// HeadPushPct is the fraction of all open PRs whose head is pushed at
	// the start of every warm cycle.
	HeadPushPct float64
}

// scenarioSeedSalt separates the scenario's draws (which repositories move,
// which heads are pushed) from the dataset's and the world's.
const scenarioSeedSalt = 0x7363656e

// CycleMetrics is what one driven poll cycle cost: the poll and the
// mergeability probe pass after it.
type CycleMetrics struct {
	Label      string
	Wall       time.Duration
	Requests   Counters       // request-count delta for this cycle
	Probes     map[string]int // mergeability probes read this cycle, by reason
	Moves      int            // base moves that landed before this cycle's poll
	HeadPushes int            // head pushes that landed before this cycle's poll
	Events     int            // github:* events emitted this cycle
	Entities   int            // active github entities after this cycle
	AllocBytes uint64         // Go heap bytes allocated during this cycle
	Errors     []string       // poll-cycle errors surfaced via Manager.OnError
}

// ProbeTotal is how many mergeability probes were read in the cycle.
func (m CycleMetrics) ProbeTotal() int {
	n := 0
	for _, v := range m.Probes {
		n += v
	}
	return n
}

// Result is the full benchmark output.
type Result struct {
	Repos              int
	TotalPRs           int
	TrackedPRs         int
	MaxOpenPRs         int
	BaseBranches       int
	ExpectedColdEvents int

	// Cycles is the cold cycles, the warm cycles, and a final settle row
	// when probes were still owed after the last warm cycle: probe passes
	// with no poll, until none is ready, so the last moves' answers land.
	Cycles []CycleMetrics

	// Totals is the server's counters over the whole run, and World what
	// the scenario's moves and pushes did.
	Totals Counters
	World  WorldStats

	// ColdCycles is how many cycles the cold sync took to complete (1 unless
	// a rate limit forced resumption); ColdSyncWall is the summed cold-cycle
	// wall time plus any between-cycle waits for a rate-limit window to
	// reset — harness bookkeeping between cycles is excluded.
	ColdCycles   int
	ColdSyncWall time.Duration
	ColdEvents   int

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

// countingPublisher is the tracker.Publisher for the bench, and the probe
// worker's. Counting happens synchronously in Publish — unlike a bus
// subscription there is no buffer to overflow, so event counts are exact
// even at stress shape. The bench deliberately stops at the tracker and probe
// worker boundary: routing/task creation is not part of what a poll cycle
// costs.
type countingPublisher struct {
	mu     sync.Mutex
	total  int
	byType map[string]int
	// conflicts counts github:pr:conflicts by entity id.
	conflicts map[string]int
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
	if evt.EventType == domain.EventGitHubPRConflicts && evt.EntityID != nil {
		if p.conflicts == nil {
			p.conflicts = make(map[string]int)
		}
		p.conflicts[*evt.EntityID]++
	}
	p.mu.Unlock()
}

// PublishPreEnqueued is the post-commit fan-out for the events the tracker
// and the probe worker enqueued themselves (the diff arms' snapshot-CAS emit,
// a probe's answer). Counted the same way — from the bench's perspective an
// emitted event is an emitted event, whichever side of the durable write it
// arrives on.
func (p *countingPublisher) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}

func (p *countingPublisher) snapshot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

func (p *countingPublisher) types() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.byType)
}

func (p *countingPublisher) conflictsByEntity() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.conflicts)
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

func (r *benchResolver) ClientForInstallation(ctx context.Context, orgID string, inst domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
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
// wire a real Manager and probe worker over real (in-memory SQLite) stores,
// drive the cold sync to completion plus WarmCycles warm cycles, each after
// the scenario's changes for it, settle the probes still owed, and report.
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

	database, err := sql.Open("sqlite", db.TestDSNMemory)
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
	// The org's GitHub is the fake server: the poller reads the tracked set
	// and stamps entity scopes on this host, and the resolver sends every
	// request to it.
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, orgID, "github", srv.URL()); err != nil {
		return nil, fmt.Errorf("set github url: %w", err)
	}
	host, err := db.OrgGitHubHostSystem(ctx, stores.Orgs, orgID)
	if err != nil {
		return nil, fmt.Errorf("read github host: %w", err)
	}
	// Track the repos on the local default team: the poller enumerates the
	// tracked set, and a registry row on its own is not tracking. The tracked
	// set is written by registry id, so each repo is registered first.
	tracked := make([]string, 0, len(ds.RepoNames()))
	for _, name := range ds.RepoNames() {
		owner, repo, ok := strings.Cut(name, "/")
		if !ok {
			return nil, fmt.Errorf("bench repo %q is not an owner/repo slug", name)
		}
		row, err := stores.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: host, Owner: owner, Repo: repo})
		if err != nil {
			return nil, fmt.Errorf("register bench repo %s: %w", name, err)
		}
		tracked = append(tracked, row.ID)
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, runmode.LocalDefaultTeamID, host, tracked); err != nil {
		return nil, fmt.Errorf("configure repos: %w", err)
	}

	pub := &countingPublisher{}
	resolver := &benchResolver{
		client: ghclient.NewClient(srv.URL(), "bench-pat"),
		base:   srv.URL(),
	}
	// The probe worker drains the probes each cycle's poll admitted. A probe
	// is not ripe until well after its admission; the harness stands in for
	// that wait by moving every ready probe's probe_after into the past
	// before the pass.
	prober := mergeability.NewWorker(stores.PRMergeability, stores.Orgs, resolver, pub, workitem.Owner{ID: "pollbench", Epoch: 1})
	probe := func() error {
		if _, err := database.ExecContext(ctx, `UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
			return fmt.Errorf("ripen probes: %w", err)
		}
		return prober.Drain(ctx)
	}

	mgr := poller.NewManager(database, pub,
		stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, stores.Orgs,
		stores.JiraStatusRules, stores.JiraProjects, stores.LinearTeamRules, stores.TeamGitHubGroups, stores.Secrets, stores.GitHubApps, stores.PollReadiness,
		resolver, nil)

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
		TrackedPRs:         ds.TrackedPRs,
		MaxOpenPRs:         ds.MaxOpenPRs(),
		BaseBranches:       ds.BaseBranches(),
		ExpectedColdEvents: ds.ExpectedColdEvents(),
	}

	// measure runs one cycle's work and records what it cost. poll is false
	// for the settle row, which only drains probes.
	measure := func(label string, poll bool, work func() error) CycleMetrics {
		reqBefore := srv.Snapshot()
		evtBefore := pub.snapshot()
		probesBefore, probesErr := probesRead(ctx, database)
		var msBefore runtime.MemStats
		runtime.ReadMemStats(&msBefore)

		start := time.Now()
		if poll {
			mgr.PollGitHubOnce(ctx, orgID)
		}
		probeErr := work()
		wall := time.Since(start)

		var msAfter runtime.MemStats
		runtime.ReadMemStats(&msAfter)
		entities, _, _ := syncState(ctx, stores, orgID, ds.TrackedPRs)
		display, unexpected := takeErrs()
		probesAfter, err := probesRead(ctx, database)
		probesErr = errors.Join(probesErr, err)
		m := CycleMetrics{
			Label:      label,
			Wall:       wall,
			Requests:   countersDelta(reqBefore, srv.Snapshot()),
			Probes:     map[string]int{},
			Events:     pub.snapshot() - evtBefore,
			Entities:   entities,
			AllocBytes: msAfter.TotalAlloc - msBefore.TotalAlloc,
			Errors:     display,
		}
		for reason, n := range probesAfter {
			if d := n - probesBefore[reason]; d > 0 {
				m.Probes[reason] = d
			}
		}
		for _, e := range unexpected {
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cycle %s: unexpected poll error: %s", label, e))
		}
		if probeErr != nil {
			m.Errors = append(m.Errors, "mergeability: "+probeErr.Error())
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cycle %s: mergeability probe pass: %v", label, probeErr))
		}
		if probesErr != nil {
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cycle %s: counting probes: %v", label, probesErr))
		}
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
		m := measure(fmt.Sprintf("cold #%d", cycle), true, probe)
		res.Cycles = append(res.Cycles, m)
		res.ColdEvents += m.Events
		res.ColdSyncWall += m.Wall
		entities, refreshed, done := syncState(ctx, stores, orgID, ds.TrackedPRs)
		if done {
			res.ColdCycles = cycle
			break
		}
		if cycle >= cfg.MaxCycles {
			res.SanityErrors = append(res.SanityErrors,
				fmt.Sprintf("cold sync incomplete after %d cycles: %d/%d entities, %d fully refreshed",
					cycle, entities, ds.TrackedPRs, refreshed))
			break
		}
		if reset := srv.NextReset(); !reset.IsZero() {
			if wait := time.Until(reset) + 100*time.Millisecond; wait > 0 {
				time.Sleep(wait)
				res.ColdSyncWall += wait
			}
		}
	}

	coldTypes := pub.types()

	// Each warm cycle starts with the scenario's changes, then polls and
	// drains probes like a cold one.
	scenario := rand.New(rand.NewSource(cfg.Dataset.Seed ^ scenarioSeedSalt))
	var prIndex [][2]int
	for ri, repo := range ds.Repos {
		for pi := range repo.PRs {
			prIndex = append(prIndex, [2]int{ri, pi})
		}
	}
	sc := cfg.Scenario
	for i := 1; i <= cfg.WarmCycles; i++ {
		moves, pushes := 0, 0
		if sc.BaseMoves && sc.MoveEvery > 0 && i%sc.MoveEvery == 0 {
			n := int(math.Ceil(sc.MoveRepoPct * float64(len(ds.Repos))))
			for _, ri := range scenario.Perm(len(ds.Repos))[:min(n, len(ds.Repos))] {
				srv.MoveBase(ri)
				moves++
			}
		}
		if sc.HeadPushPct > 0 {
			n := int(math.Round(sc.HeadPushPct * float64(len(prIndex))))
			for _, k := range scenario.Perm(len(prIndex))[:min(n, len(prIndex))] {
				srv.PushHead(prIndex[k][0], prIndex[k][1])
				pushes++
			}
		}
		m := measure(fmt.Sprintf("warm #%d", i), true, probe)
		m.Moves, m.HeadPushes = moves, pushes
		res.Cycles = append(res.Cycles, m)
	}

	// A move's overlap set is answered a cycle after the move, once the
	// UNKNOWN its first read returns is read again, so the last cycles'
	// answers are drained here, with no poll and nothing moving.
	if owed, err := probesOwed(ctx, database); err != nil {
		res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("counting owed probes: %v", err))
	} else if owed > 0 {
		m := measure("settle", false, func() error {
			for pass := 0; pass < settlePasses; pass++ {
				if err := probe(); err != nil {
					return err
				}
				if owed, err := probesOwed(ctx, database); err != nil || owed == 0 {
					return err
				}
			}
			return fmt.Errorf("probes still owed after %d passes", settlePasses)
		})
		res.Cycles = append(res.Cycles, m)
	}

	res.PeakRSSBytes = peakRSSBytes()
	res.EventTypes = pub.types()
	res.Totals = srv.Snapshot()
	res.World = srv.Stats()

	// Sanity: the cold sync must emit exactly the transitions the dataset
	// predicts, and the warm cycles exactly the ones the scenario's changes
	// do: a new_commits per head push and a conflicts per PR a move made
	// CONFLICTING. These failing means the harness (or the tracker) isn't
	// doing what this benchmark claims to measure.
	if res.ColdCycles > 0 && res.ColdEvents != res.ExpectedColdEvents {
		res.SanityErrors = append(res.SanityErrors,
			fmt.Sprintf("cold-sync events = %d, expected %d", res.ColdEvents, res.ExpectedColdEvents))
	}
	expectedWarm := map[string]int{}
	for typ := range res.EventTypes {
		expectedWarm[typ] = 0
	}
	expectedWarm[domain.EventGitHubPRNewCommits] = res.World.HeadPushes
	expectedWarm[domain.EventGitHubPRConflicts] = len(res.World.Conflicted)
	for _, typ := range slices.Sorted(maps.Keys(expectedWarm)) {
		if got, want := res.EventTypes[typ]-coldTypes[typ], expectedWarm[typ]; got != want {
			res.SanityErrors = append(res.SanityErrors,
				fmt.Sprintf("warm cycles emitted %d %s events, expected %d", got, typ, want))
		}
	}
	res.SanityErrors = append(res.SanityErrors, conflictErrors(ctx, stores, orgID, pub.conflictsByEntity(), append(ds.Conflicting(), res.World.Conflicted...))...)
	// Every test merge must be one a probe needed: a pull request whose head
	// was pushed or whose paths a move overlapped, or one read again after
	// an UNKNOWN. Anything else is a read TF made that it did not need.
	if n := res.Totals.TestMergesUnaccounted; n > 0 {
		res.SanityErrors = append(res.SanityErrors,
			fmt.Sprintf("%d test merge(s) not accounted for by a head push, an overlapping move, or a re-read after UNKNOWN", n))
	}
	if cfg.Server.RateLimit <= 0 && res.ColdCycles > 0 {
		cold := res.Cycles[0]
		if probes := cold.ProbeTotal(); probes > res.TrackedPRs {
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cold cycle read %d probes for %d tracked PRs", probes, res.TrackedPRs))
		}
		if refs := cold.Requests.Refs + cold.Requests.Refs304; refs > res.BaseBranches {
			res.SanityErrors = append(res.SanityErrors, fmt.Sprintf("cold cycle read %d refs for %d base branches", refs, res.BaseBranches))
		}
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

// settlePasses bounds the settle row's probe passes. What is owed after the
// last warm cycle is the re-reads of its UNKNOWNs, which one pass answers
// once nothing moves; the rest is headroom.
const settlePasses = 10

// probesRead counts the probes the worker has completed, by reason. Nothing
// in the bench closes a pull request, so every completed probe was read.
func probesRead(ctx context.Context, database *sql.DB) (map[string]int, error) {
	rows, err := database.QueryContext(ctx, `SELECT reason, COUNT(*) FROM pr_mergeability_probes WHERE status = 'done' GROUP BY reason`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		out[reason] = n
	}
	return out, rows.Err()
}

// probesOwed counts the probes not yet completed.
func probesOwed(ctx context.Context, database *sql.DB) (int, error) {
	var n int
	err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_mergeability_probes WHERE status IN ('ready','leased')`).Scan(&n)
	return n, err
}

// conflictErrors checks that every PR in implied (by node id) emitted
// github:pr:conflicts exactly once, and no other PR emitted one. emitted
// counts the events by entity id.
func conflictErrors(ctx context.Context, stores db.Stores, orgID string, emitted map[string]int, implied []string) []string {
	entities, err := stores.Entities.ListActiveSystem(ctx, orgID, "github")
	if err != nil {
		return []string{fmt.Sprintf("listing entities for the conflicts check: %v", err)}
	}
	byNode := make(map[string]int, len(emitted))
	for _, e := range entities {
		if n, ok := emitted[e.ID]; ok {
			byNode[e.ExternalID] = n
		}
	}
	want := make(map[string]bool, len(implied))
	for _, id := range implied {
		want[id] = true
	}
	missing, repeated, unexpected := 0, 0, 0
	for id := range want {
		switch n := byNode[id]; {
		case n == 0:
			missing++
		case n > 1:
			repeated++
		}
	}
	for id := range byNode {
		if !want[id] {
			unexpected++
		}
	}
	var out []string
	if missing+repeated+unexpected > 0 {
		out = append(out, fmt.Sprintf("github:pr:conflicts: %d of %d implied PRs never emitted, %d emitted more than once, %d emitted for PRs the dataset never made conflicting",
			missing, len(want), repeated, unexpected))
	}
	return out
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

// countersSum is a + b, as a − (−b).
func countersSum(a, b Counters) Counters {
	neg := countersDelta(b, Counters{})
	return countersDelta(neg, a)
}

func countersDelta(before, after Counters) Counters {
	return Counters{
		Total:                 after.Total - before.Total,
		PullsList:             after.PullsList - before.PullsList,
		PullsPages:            after.PullsPages - before.PullsPages,
		Pulls304:              after.Pulls304 - before.Pulls304,
		GraphQL:               after.GraphQL - before.GraphQL,
		Points:                after.Points - before.Points,
		Teams:                 after.Teams - before.Teams,
		Refs:                  after.Refs - before.Refs,
		Refs304:               after.Refs304 - before.Refs304,
		Compares:              after.Compares - before.Compares,
		PathReads:             after.PathReads - before.PathReads,
		FileLists:             after.FileLists - before.FileLists,
		RateLimited:           after.RateLimited - before.RateLimited,
		Other:                 after.Other - before.Other,
		TestMerges:            after.TestMerges - before.TestMerges,
		TestMergesHeadPushed:  after.TestMergesHeadPushed - before.TestMergesHeadPushed,
		TestMergesOverlap:     after.TestMergesOverlap - before.TestMergesOverlap,
		TestMergesReread:      after.TestMergesReread - before.TestMergesReread,
		TestMergesUnaccounted: after.TestMergesUnaccounted - before.TestMergesUnaccounted,
	}
}
