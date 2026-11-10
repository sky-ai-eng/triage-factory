package poller

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// markedRig is an org on an active App with two installations, tok-1 granting
// acme/one and tok-2 granting beta/two, both tracked, with the refresh marks
// wired and events captured.
type markedRig struct {
	t        *testing.T
	database *sql.DB
	stores   db.Stores
	gh       *reachGitHub
	m        *Manager
	pub      *capturingPublisher
	scored   atomic.Int32
}

func newMarkedRig(t *testing.T) *markedRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"acme/one", "beta/two"})
	seedBYOAppCredentialClass(t, stores, org)
	grants := map[string][]string{"tok-1": {"acme/one"}, "tok-2": {"beta/two"}}
	gh := newReachGitHub(t, reachRepos, grants, grants)
	r := &markedRig{t: t, database: database, stores: stores, gh: gh, pub: &capturingPublisher{}}
	r.m = newReachManager(t, database, stores, gh, []domain.OrgGitHubAppInstallation{
		{InstallationID: "1", AccountLogin: "acme"},
		{InstallationID: "2", AccountLogin: "beta"},
	})
	r.m.pub = r.pub
	r.m.RefreshMarks = stores.GitHubRefreshMarks
	r.m.TriggerScorer = func(string) { r.scored.Add(1) }
	return r
}

// mark writes a refresh mark on repo, as a receiver would.
func (r *markedRig) mark(repo string, target domain.GitHubRefreshTarget) {
	r.t.Helper()
	owner, name, _ := strings.Cut(repo, "/")
	row, err := r.stores.Repos.FindOrCreateByRefSystem(context.Background(), runmode.LocalDefaultOrgID, domain.RepoRef{Host: "https://github.com", Owner: owner, Repo: name})
	if err != nil {
		r.t.Fatalf("register %s: %v", repo, err)
	}
	if _, err := r.stores.GitHubRefreshMarks.MarkSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID, target); err != nil {
		r.t.Fatalf("mark %s %v: %v", repo, target, err)
	}
}

func (r *markedRig) pending() []domain.GitHubRefreshMark {
	r.t.Helper()
	marks, err := r.stores.GitHubRefreshMarks.ListSystem(context.Background(), runmode.LocalDefaultOrgID, 100)
	if err != nil {
		r.t.Fatalf("list marks: %v", err)
	}
	return marks
}

// settle runs the scheduled cycle until both pull requests are refreshed and
// both listings answer 304, then forgets what it sent.
func (r *markedRig) settle() {
	r.t.Helper()
	for range 2 {
		r.m.runGitHubCycleForOrg(context.Background(), runmode.LocalDefaultOrgID)
	}
	r.gh.takeSent()
	r.gh.takePaths()
	r.pub.mu.Lock()
	r.pub.evts = nil
	r.pub.mu.Unlock()
}

// A narrowed pass refreshes the marked pull request under the installation
// whose grant the last scheduled cycle read, lists only the marked
// repository, makes none of the cycle's org-wide reads, clears the marks it
// honored, kicks the scorer when it emitted something, and reports no
// completed poll.
func TestRefreshMarkedGitHub_ReadsOnlyTheMarkedRepository(t *testing.T) {
	r := newMarkedRig(t)
	org := runmode.LocalDefaultOrgID
	r.settle()
	r.m.setGitHubCursor(org, "beta/two")

	r.gh.merge("PR_one")
	r.mark("acme/one", domain.PullRequestRefreshTarget(1))
	r.m.RefreshMarkedGitHubOnce(context.Background(), org)

	paths := r.gh.takePaths()
	for _, p := range paths {
		if !strings.HasPrefix(p, "tok-1 ") || strings.Contains(p, "/installation/repositories") || strings.Contains(p, "beta/two") {
			t.Errorf("request %q: want only tok-1's listing of acme/one and its refresh", p)
		}
	}
	if sent := r.gh.takeSent(); !slices.Equal(sent["tok-1"], []string{"PR_one"}) || len(sent) != 1 {
		t.Errorf("refreshes sent %v, want only PR_one under tok-1", sent)
	}
	if got := r.pending(); len(got) != 0 {
		t.Errorf("marks after the pass = %+v, want the honored mark cleared", got)
	}
	if n := len(r.pub.ofType(domain.EventGitHubPRMerged)); n != 1 {
		t.Errorf("merged events = %d, want 1", n)
	}
	if n := len(r.pub.ofType(domain.EventSystemPollCompleted)); n != 0 {
		t.Errorf("poll-complete events = %d, want none from a narrowed pass", n)
	}
	if n := r.scored.Load(); n != 1 {
		t.Errorf("scorer kicks = %d, want 1 after a pass that emitted", n)
	}
	if got := r.m.githubCursor(org); got != "beta/two" {
		t.Errorf("cursor = %q, want beta/two: a narrowed pass leaves it", got)
	}
}

// A narrowed pass with nothing to route sends nothing and leaves the marks to
// the scheduled cycle: before any scheduled cycle has read a grant, and while
// the org waits out a rate limit.
func TestRefreshMarkedGitHub_LeavesWhatItCannotRouteForTheScheduledCycle(t *testing.T) {
	t.Run("no grant read yet", func(t *testing.T) {
		r := newMarkedRig(t)
		r.mark("acme/one", domain.PullRequestRefreshTarget(1))
		r.m.RefreshMarkedGitHubOnce(context.Background(), runmode.LocalDefaultOrgID)
		if paths := r.gh.takePaths(); len(paths) != 0 {
			t.Errorf("requests = %v, want none", paths)
		}
		if got := r.pending(); len(got) != 1 {
			t.Errorf("marks = %+v, want the mark kept", got)
		}
	})
	t.Run("rate-limit hold", func(t *testing.T) {
		r := newMarkedRig(t)
		r.settle()
		r.m.holdPoll("github", runmode.LocalDefaultOrgID, time.Now().Add(time.Hour))
		r.mark("acme/one", domain.PullRequestRefreshTarget(1))
		r.m.RefreshMarkedGitHubOnce(context.Background(), runmode.LocalDefaultOrgID)
		if paths := r.gh.takePaths(); len(paths) != 0 {
			t.Errorf("requests = %v, want none while the org is held", paths)
		}
		if got := r.pending(); len(got) != 1 {
			t.Errorf("marks = %+v, want the mark kept", got)
		}
	})
}

// The scheduled cycle refreshes a marked pull request its gate would skip and
// clears the mark, and clears a mark on a repository no team tracks without
// refreshing anything for it.
func TestRunGitHubCycleForOrg_HonorsAndClearsRefreshMarks(t *testing.T) {
	r := newMarkedRig(t)
	r.settle()
	r.mark("beta/two", domain.PullRequestRefreshTarget(2))
	r.mark("zed/gone", domain.PullRequestRefreshTarget(9))

	r.m.runGitHubCycleForOrg(context.Background(), runmode.LocalDefaultOrgID)
	if sent := r.gh.takeSent(); !slices.Equal(sent["tok-2"], []string{"PR_two"}) || len(sent) != 1 {
		t.Errorf("refreshes sent %v, want only the marked PR_two, under tok-2", sent)
	}
	if got := r.pending(); len(got) != 0 {
		t.Errorf("marks after the cycle = %+v, want both cleared", got)
	}
}

// A narrowed pass and the scheduled cycle never overlap on one org: the
// scheduled loop passes over an org mid-pass and leaves it due, and no
// narrowed pass starts for it until the scheduled cycle has run.
func TestOrgPass_ScheduledCycleAndNarrowedPassExcludeEachOther(t *testing.T) {
	m := &Manager{}
	p := m.orgPassFor("org")
	p.mu.Lock() // a narrowed pass running
	if _, ok := m.tryBeginScheduled("org"); ok {
		t.Fatal("tryBeginScheduled succeeded while a narrowed pass held the org")
	}
	if _, ok := m.tryBeginScheduled("other"); !ok {
		t.Fatal("another org's scheduled cycle waited on this org's pass")
	}
	p.mu.Unlock()

	m.ghStop = make(chan struct{})
	m.passMu.Lock()
	p.pending = true
	m.passMu.Unlock()
	// With the scheduled cycle owed, the pass returns before reading
	// anything: this Manager has no stores, so a read would panic.
	m.runMarkedPass("org")
	if p.pending {
		t.Fatal("pending still set after the pass; want it cleared")
	}

	end, ok := m.tryBeginScheduled("org")
	if !ok {
		t.Fatal("tryBeginScheduled failed with the org free")
	}
	end()
	if p.yield {
		t.Error("yield still set after the scheduled cycle ran")
	}
}

// countingMarks counts the narrowed passes that read the org's marks.
type countingMarks struct {
	db.GitHubRefreshMarkStore
	mu    sync.Mutex
	reads int
}

func (c *countingMarks) ListSystem(ctx context.Context, orgID string, limit int) ([]domain.GitHubRefreshMark, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.GitHubRefreshMarkStore.ListSystem(ctx, orgID, limit)
}

func (c *countingMarks) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// Nudges that land before the narrowed pass reads the marks collapse into
// one pass; the sweep starts a pass for an active org with marks; a pass
// scheduled before the loop restarts runs on the restarted loop; and nothing
// runs once the loop is stopped.
func TestHintGitHub_CollapsesNudgesAndTheSweepStartsAPass(t *testing.T) {
	r := newMarkedRig(t)
	org := runmode.LocalDefaultOrgID
	counting := &countingMarks{GitHubRefreshMarkStore: r.stores.GitHubRefreshMarks}
	r.m.RefreshMarks = counting
	r.mark("acme/one", domain.PullRequestRefreshTarget(1))

	waitReads := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for counting.count() < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // and no further pass
		if got := counting.count(); got != want {
			t.Fatalf("narrowed passes = %d, want %d", got, want)
		}
	}

	r.m.scheduleMarkedPass(org, 0)
	waitReads(0) // the loop is not running

	// setLoop moves the loop to stop, closing the one it replaces as stopAll
	// does; nil is a stopped loop.
	setLoop := func(stop chan struct{}) {
		r.m.mu.Lock()
		if r.m.ghStop != nil {
			close(r.m.ghStop)
		}
		r.m.ghStop = stop
		r.m.mu.Unlock()
	}
	setLoop(make(chan struct{}))
	for range 3 {
		r.m.scheduleMarkedPass(org, 50*time.Millisecond)
	}
	waitReads(1)

	r.m.sweepMarks(context.Background())
	waitReads(2)

	r.m.scheduleMarkedPass(org, 50*time.Millisecond)
	setLoop(make(chan struct{})) // a restart before the pass is due
	waitReads(3)

	r.m.scheduleMarkedPass(org, 50*time.Millisecond)
	setLoop(nil) // a stop before the pass is due
	waitReads(3)
	r.m.sweepMarks(context.Background())
	waitReads(3)
}

// The scheduled cycle drops a mark nothing can refresh: one on a tracked
// repository no installation's grant holds, once every grant was read, and
// every mark of an org with no GitHub credential at all.
func TestRunGitHubCycleForOrg_DropsMarksNoCredentialCanRefresh(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	t.Run("no grant holds the repository", func(t *testing.T) {
		r := newMarkedRig(t)
		trackRepos(t, r.stores, org, []string{"acme/one", "beta/two", "zed/three"})
		r.settle()
		r.mark("zed/three", domain.PullRequestRefreshTarget(3))
		r.mark("acme/one", domain.PullRequestRefreshTarget(1))
		r.m.runGitHubCycleForOrg(ctx, org)
		if got := r.pending(); len(got) != 0 {
			t.Errorf("marks after the cycle = %+v, want both cleared: one honored, one in no grant", got)
		}
	})
	t.Run("no credential", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		database := newMigratedSQLiteForPoller(t)
		stores := sqlitestore.New(database)
		trackRepos(t, stores, org, []string{"acme/one"})
		m := &Manager{
			database: database, tasks: stores.Tasks, entities: stores.Entities, eventQueue: stores.EventQueue,
			mergeability: stores.PRMergeability, repos: stores.Repos, orgs: stores.Orgs,
			resolver:     &fakeResolver{err: ghclient.ErrNoGitHubCredentials},
			RefreshMarks: stores.GitHubRefreshMarks,
		}
		row, err := stores.Repos.FindOrCreateByRefSystem(ctx, org, domain.RepoRef{Host: "https://github.com", Owner: "acme", Repo: "one"})
		if err != nil {
			t.Fatalf("register acme/one: %v", err)
		}
		if _, err := stores.GitHubRefreshMarks.MarkSystem(ctx, org, row.ID, domain.PullRequestRefreshTarget(1)); err != nil {
			t.Fatalf("mark: %v", err)
		}
		m.runGitHubCycleForOrg(ctx, org)
		if got, err := stores.GitHubRefreshMarks.ListSystem(ctx, org, 10); err != nil || len(got) != 0 {
			t.Errorf("marks after the cycle = %+v, %v; want the mark dropped", got, err)
		}
	})
}

// In local mode a narrowed pass on the personal token runs only once a
// scheduled cycle has listed the user's teams, which the reviewer resolver
// needs to tell a review requested from one of them.
func TestMarkedPATPass_LocalWaitsForTheUsersTeams(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	m := &Manager{orgs: stores.Orgs, users: stores.Users, githubGroups: stores.TeamGitHubGroups,
		resolver: &fakeResolver{client: ghclient.NewClient("https://github.com", "tok")}}
	org := runmode.LocalDefaultOrgID
	set := domain.OrgSettings{GitHubBaseURL: "https://github.com"}
	if _, _, _, ok := m.markedPATPass(context.Background(), org, set); ok {
		t.Fatal("markedPATPass ran before any scheduled cycle listed the user's teams")
	}
	m.rememberLocalTeams(org, nil)
	if _, _, _, ok := m.markedPATPass(context.Background(), org, set); !ok {
		t.Fatal("markedPATPass refused once the teams were listed, as none")
	}
}

// A GitHub rate limit holds the org's polls until its reset: a config save's
// PollSoon does not make the org due sooner, and a narrowed pass sees the hold.
func TestRecordGitHubCursor_RateLimitHoldsThePolls(t *testing.T) {
	m := &Manager{}
	now := time.Now()
	reset := now.Add(10 * time.Minute)
	m.recordGitHubCursor("org", "acme/one", &ghclient.ErrRateLimited{ResumeAt: reset})
	m.PollSoon("github", "org")
	if m.pollDue("github", "org", now) {
		t.Error("PollSoon made a rate-limited org due before its reset")
	}
	if !m.pollHeld("github", "org", now) {
		t.Error("pollHeld = false before the reset; a narrowed pass would poll into the limit")
	}
	if !m.pollDue("github", "org", reset) || m.pollHeld("github", "org", reset) {
		t.Error("the hold outlasted its reset")
	}
}
