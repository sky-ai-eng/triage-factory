package poller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestManager_RunGitHubCycle_IteratesActiveOrgs pins the outer-loop
// contract on the poller: a single tick enumerates every active org
// via OrgsStore.ListActiveSystem and dispatches per-org work.
//
// To keep the test free of GitHub network round-trips, every fake
// org's RepositoryStore returns an empty configured-names list — that path
// short-circuits before the tracker is invoked, so the assertion is
// strictly "per-org loop visited every active org," not "tracker did
// the right thing." Tracker behavior is covered by the tracker
// package's own per-org tests.
func TestManager_RunGitHubCycle_IteratesActiveOrgs(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	repos := &recordingRepositoryStore{}
	users := &emptyUsersStore{} // GetGitHubLoginSystem unused — repo path exits first

	m := &Manager{orgs: orgs, repos: repos, users: users}
	m.runGitHubCycle(nil)

	if got := orgs.callCount(); got != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 per cycle", got)
	}
	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != len(orgs.ids) {
		t.Fatalf("ListTrackedNamesSystem visited %d orgs (%v); want %d (%v)", len(repos.visited), repos.visited, len(orgs.ids), orgs.ids)
	}
	for i, got := range repos.visited {
		if got != orgs.ids[i] {
			t.Errorf("visit[%d] = %s; want %s (per-org iteration must preserve ListActiveSystem order)", i, got, orgs.ids[i])
		}
	}
}

// TestManager_RunGitHubCycle_StopHaltsMidCycle pins D4: a closed stop
// channel (a lease demotion / RestartAll) keeps the per-org loop from
// starting new batches, rather than polling the whole fleet after this
// holder has been torn down.
func TestManager_RunGitHubCycle_StopHaltsMidCycle(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	repos := &recordingRepositoryStore{}
	users := &emptyUsersStore{}
	m := &Manager{orgs: orgs, repos: repos, users: users}

	stop := make(chan struct{})
	close(stop) // torn down before the loop starts
	m.runGitHubCycle(stop)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 0 {
		t.Errorf("polled %d orgs (%v) after stop was closed; want 0 — a torn-down holder must not start new per-org batches", len(repos.visited), repos.visited)
	}
}

// TestManager_RunJiraCycle_OrgsStoreError mirrors the GitHub-side
// test for the Jira path. The Jira loop is thinner than GitHub
// (no per-org repo gate), so we record orgIDs via a tracker
// substitute — but tracker.Tracker is a concrete type, so instead we
// record the OrgsStore call and rely on the fact that each visited
// org will at least try to call client.RefreshJira (nil client →
// panic-rescue path is not tested here; that's a real-call concern).
//
// Practically: this test only verifies that ListActiveSystem is
// invoked once per cycle. The per-org dispatch inside runJiraCycle
// is a flat for-loop over the returned slice with no early-exit
// branches that could skip an org — a regression that broke that
// loop would also break the GitHub test above (both share the
// orgs.ListActiveSystem contract).
func TestManager_RunJiraCycle_OrgsStoreError(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	m := &Manager{orgs: orgs}
	m.runJiraCycle(nil)

	if got := orgs.callCount(); got != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 even on error", got)
	}
}

// TestManager_RunGitHubCycle_OrgsStoreErrorAbortsCycle pins: if the
// orgs lookup fails, the cycle ends rather than falling back to the
// hardcoded sentinel — silent fallback would mask multi-mode failures
// as local-mode behavior.
func TestManager_RunGitHubCycle_OrgsStoreErrorAbortsCycle(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos}
	m.runGitHubCycle(nil)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 0 {
		t.Errorf("ListTrackedNamesSystem called %d times despite ListActiveSystem error; want 0", len(repos.visited))
	}
}

// TestManager_StartGitHub_StartsInMultiMode pins that the old local-only
// gate is lifted: multi-mode GitHub polling is now the per-org App
// path, so startGitHub spawns the poll goroutine (ghStop != nil) in multi
// mode too. The initial poll fans out per active org; with an empty
// active-org list the cycle does nothing, so this test asserts only the
// lifecycle contract (goroutine spawned, then cleanly stopped).
func TestManager_StartGitHub_StartsInMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	m := &Manager{orgs: &fakeOrgsStore{}}

	m.startGitHub()

	m.mu.Lock()
	started := m.ghStop != nil
	m.mu.Unlock()
	if !started {
		t.Errorf("multi-mode startGitHub did not spawn a poll goroutine (ghStop == nil); want the per-org App path to run")
	}
	m.StopAll()
}

// TestManager_RestartAll_StartsBothInMultiMode pins the boot/config-change
// entry point main.go uses: RestartAll must start the process-global GitHub
// poller in multi mode (nothing ever started a poll loop in multi, so
// no entities were discovered) AND the Jira poller. Jira polling now reads
// service creds through the claims-free system door (integrations.LoadSystem,
// resolver.ForSystem), so it no longer needs a request JWT and runs in multi
// just like local — RestartAll starts both loops in both modes.
func TestManager_RestartAll_StartsBothInMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	m := &Manager{orgs: &fakeOrgsStore{}}

	m.RestartAll()
	defer m.StopAll()

	m.mu.Lock()
	gh, jira := m.ghStop != nil, m.jiraStop != nil
	m.mu.Unlock()
	if !gh {
		t.Errorf("RestartAll in multi mode did not start the GitHub poller (ghStop == nil) — SKY-386 regression")
	}
	if !jira {
		t.Errorf("RestartAll in multi mode did not start the Jira poller (jiraStop == nil); multi-mode Jira polling reads creds via the system door")
	}
}

// TestManager_RestartAll_StartsBothInLocalMode pins that the lifecycle is
// behavior-preserving in local mode: RestartAll starts both loops.
func TestManager_RestartAll_StartsBothInLocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	m := &Manager{orgs: &fakeOrgsStore{}}

	m.RestartAll()
	defer m.StopAll()

	m.mu.Lock()
	gh, jira := m.ghStop != nil, m.jiraStop != nil
	m.mu.Unlock()
	if !gh || !jira {
		t.Errorf("RestartAll in local mode: ghStop!=nil=%v jiraStop!=nil=%v; want both started", gh, jira)
	}
}

// TestManager_RunGitHubCycle_SkipsOrgNotYetDue pins the per-org cadence gate:
// once an org is polled, a second cycle fired before its interval elapses must
// skip it. (DefaultOrgSettings is 5m, far longer than the gap between the two
// synchronous cycles here.)
func TestManager_RunGitHubCycle_SkipsOrgNotYetDue(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	orgs := &fakeOrgsStore{ids: []string{"org-a"}}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos, users: &emptyUsersStore{}}

	m.runGitHubCycle(nil) // org-a due → polled, scheduled ~5m out
	m.runGitHubCycle(nil) // immediately again → not due → skipped

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 1 {
		t.Fatalf("org polled %d times across two back-to-back cycles; want 1 (second cycle must skip a not-yet-due org). visited=%v", len(repos.visited), repos.visited)
	}
}

// TestManager_RunGitHubCycle_RepollsAfterSlotElapses pins the other half of
// the gate: once an org's reserved slot is in the past, the next cycle polls
// it again. Backdating via schedulePoll keeps the test deterministic (no
// sleep, no real interval wait).
func TestManager_RunGitHubCycle_RepollsAfterSlotElapses(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	orgs := &fakeOrgsStore{ids: []string{"org-a"}}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos, users: &emptyUsersStore{}}

	m.runGitHubCycle(nil)
	m.schedulePoll("github", "org-a", time.Now().Add(-time.Second)) // pretend the interval elapsed
	m.runGitHubCycle(nil)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 2 {
		t.Fatalf("org polled %d times; want 2 (re-poll once its slot elapsed). visited=%v", len(repos.visited), repos.visited)
	}
}

// TestManager_PollSoon_ReduesOnlyTargetOrg is the load-safety regression for
// the multi-mode GitHub-config-change path: a config change on ONE org must
// re-due only that org, never the whole fleet. Two orgs are polled and put on
// their (long, default) cadence; PollSoon on org-a alone must make the next
// cycle re-poll org-a but NOT org-b — otherwise one tenant saving a setting
// would stampede every tenant's poll against shared GHES/GHEC API budgets.
func TestManager_PollSoon_ReduesOnlyTargetOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b"}}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos, users: &emptyUsersStore{}}

	m.runGitHubCycle(nil) // both due → polled once each, both scheduled ~5m out
	m.PollSoon("github", "org-a")
	m.runGitHubCycle(nil) // org-a re-due; org-b still on cadence → skipped

	repos.mu.Lock()
	defer repos.mu.Unlock()
	got := map[string]int{}
	for _, v := range repos.visited {
		got[v]++
	}
	if got["org-a"] != 2 {
		t.Errorf("org-a polled %d times; want 2 (initial + PollSoon re-due). visited=%v", got["org-a"], repos.visited)
	}
	if got["org-b"] != 1 {
		t.Errorf("org-b polled %d times; want 1 (PollSoon('org-a') must NOT re-due org-b — fleet stampede). visited=%v", got["org-b"], repos.visited)
	}
}

// TestManager_StartGitHub_DoesNotRepollScheduledOrg pins that a restart is
// schedule-neutral: an org already on its cadence is not re-polled just because
// the loop bounced. (resetSchedule used to clear all slots on start, which —
// since multi restarts the process-global loop — re-polled the whole fleet on
// any one org's config change. That primitive is gone; PollSoon replaces it.)
func TestManager_StartGitHub_DoesNotRepollScheduledOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	orgs := &fakeOrgsStore{ids: []string{"org-a"}}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos, users: &emptyUsersStore{}}

	m.runGitHubCycle(nil) // org-a polled, scheduled ~5m out

	// Simulate the post-restart initial cycle directly (startGitHub's goroutine
	// runs exactly this); the org's existing slot must still gate it.
	m.runGitHubCycle(nil)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 1 {
		t.Fatalf("org polled %d times; want 1 (a restart must not re-poll an org still on its cadence). visited=%v", len(repos.visited), repos.visited)
	}
}

// TestManager_PollSoon_AppliesConfigChangeImmediately pins the local-mode
// config-change latency fix: a restart is schedule-neutral, so applying a
// config change immediately depends on the caller PollSoon-ing the org. Models
// the local SetOnGitHubChanged path: poll (org on cadence) → restart (no-op for
// the slot) → PollSoon → next cycle re-polls. Without the PollSoon a local user
// would wait out the interval before their saved setting took effect.
func TestManager_PollSoon_AppliesConfigChangeImmediately(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	orgs := &fakeOrgsStore{ids: []string{runmode.LocalDefaultOrgID}}
	repos := &recordingRepositoryStore{}
	m := &Manager{orgs: orgs, repos: repos, users: &emptyUsersStore{}}

	m.runGitHubCycle(nil) // polled, scheduled ~5m out
	m.runGitHubCycle(nil) // still on cadence → skipped (proves the slot gates)
	m.PollSoon("github", runmode.LocalDefaultOrgID)
	m.runGitHubCycle(nil) // PollSoon cleared the slot → re-polled now

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 2 {
		t.Fatalf("org polled %d times; want 2 (initial + post-PollSoon, with the middle cadence-gated cycle skipped). visited=%v", len(repos.visited), repos.visited)
	}
}

// --- test doubles ---

type fakeOrgsStore struct {
	db.OrgsStore // embed nil — every method except ListActiveSystem panics if reached
	ids          []string
	err          error

	// mu guards calls. RestartAll starts the GitHub and Jira loops as two
	// goroutines that each run an initial cycle calling ListActiveSystem
	// concurrently, so the counter must be safe under -race even though the
	// single-cycle tests invoke it serially.
	mu    sync.Mutex
	calls int
}

func (f *fakeOrgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.ids...), nil
}

func (f *fakeOrgsStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// GetSettingsSystem feeds the cycle's per-org interval read. Returns defaults
// — the scheduler tests assert cadence via the nextPoll clock, not the exact
// interval value.
func (f *fakeOrgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return domain.DefaultOrgSettings(), nil
}

// recordingRepositoryStore embeds db.RepositoryStore as nil and overrides only the
// methods the per-org loop reaches before exiting. Returning an empty
// configured-names list short-circuits the loop body so no real
// GitHub client work happens; any other RepositoryStore call would panic via
// the nil embedded interface, which is the loud-failure-on-regression
// posture we want.
type recordingRepositoryStore struct {
	db.RepositoryStore
	mu      sync.Mutex
	visited []string
}

func (r *recordingRepositoryStore) ListTrackedNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visited = append(r.visited, orgID)
	return nil, nil
}

type emptyUsersStore struct{ db.UsersStore }

func (emptyUsersStore) GetGitHubLoginSystem(ctx context.Context, userID, githubBaseURL string) (string, error) {
	return "", nil
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

var errOrgsDown = stubErr("simulated orgs-store outage")

var (
	_ db.OrgsStore       = (*fakeOrgsStore)(nil)
	_ db.RepositoryStore = (*recordingRepositoryStore)(nil)
	_ db.UsersStore      = emptyUsersStore{}
)
