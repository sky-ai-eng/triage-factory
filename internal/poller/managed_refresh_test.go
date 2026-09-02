package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// The managed installation-set refresh is a DEPLOYMENT singleton on the GitHub
// cycle, not a per-org pass: under a shared App key GET /app/installations is
// the same answer whoever asks, so it runs at most once per cycle — ahead of
// the first due org's poll, so the per-org grant pass reads rows the listing
// has already corrected — and not at all on a wake where nothing is due.
//
// Leader gating needs no test of its own here, for the same reason as the
// per-org grant pass: the hook has one call site, inside the poll cycle, and
// in multi mode the scheduler that runs the cycle runs only on the brain-lease
// holder. A standby never enters runGitHubCycle, so it never lists.
func TestRunGitHubCycle_RefreshesManagedInstallationsOnceAheadOfTheFirstPoll(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	repos := &recordingRepositoryStore{}

	var mu sync.Mutex
	var sequence []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		sequence = append(sequence, step)
	}
	m := &Manager{
		orgs:  orgs,
		repos: repos,
		users: &emptyUsersStore{},
		RefreshManagedInstallations: func(context.Context) error {
			record("managed")
			return nil
		},
		ReconcileGrant: func(_ context.Context, orgID string) error {
			record("grant:" + orgID)
			return nil
		},
	}

	m.runGitHubCycle(nil)

	want := []string{"managed", "grant:org-a", "grant:org-b", "grant:org-c"}
	if len(sequence) != len(want) {
		t.Fatalf("cycle ran %v; want %v — one managed refresh, before the first org's grant pass", sequence, want)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("cycle ran %v; want %v", sequence, want)
		}
	}
}

func TestRunGitHubCycle_NoDueOrgSpendsNoManagedRefresh(t *testing.T) {
	// The second wake finds every org still on its cadence, so nothing is
	// polled — and the listing is not spent either. The refresh rides the polls
	// it precedes; it does not add a wake of its own.
	orgs := &fakeOrgsStore{ids: []string{"org-a"}}
	repos := &recordingRepositoryStore{}

	var refreshes int
	m := &Manager{
		orgs:  orgs,
		repos: repos,
		users: &emptyUsersStore{},
		RefreshManagedInstallations: func(context.Context) error {
			refreshes++
			return nil
		},
	}

	m.runGitHubCycle(nil) // cold boot: org-a due → polled, refresh spent once
	m.runGitHubCycle(nil) // org-a still on cadence → nothing polled, nothing listed

	if refreshes != 1 {
		t.Errorf("managed refresh ran %d times across a polling wake and an idle one; want 1", refreshes)
	}
}

func TestRunGitHubCycle_ManagedRefreshFailureDoesNotSkipThePolls(t *testing.T) {
	// Best effort, like the per-org grant pass: a listing that fails leaves every
	// managed row as it was, which is the worst outcome, and never a reason to
	// skip the polls the cycle came for.
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b"}}
	repos := &recordingRepositoryStore{}
	m := &Manager{
		orgs:  orgs,
		repos: repos,
		users: &emptyUsersStore{},
		RefreshManagedInstallations: func(context.Context) error {
			return errors.New("github unreachable")
		},
	}

	m.runGitHubCycle(nil)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 2 {
		t.Errorf("polled %d orgs (%v) after the managed refresh failed; want both", len(repos.visited), repos.visited)
	}
}
