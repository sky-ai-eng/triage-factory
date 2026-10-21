package tracker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// trackedRepositoryID is the provider id the renamed repository carries in
// these tests: the identity a repository rename is detected and applied by.
const trackedRepositoryID = "1296269"

// GitHub renames octo/old to octo/new. Until TF applies the rename, the REST
// listing still answers under the tracked name, while a GraphQL search names
// the repository as GitHub does now. A pull request read under the new name
// resolves to its existing entity by node id, in discovery and in the
// dashboard backfill alike, so no second entity holds octo/new#7 and the
// repository rename applies: the entity moves onto the new key, and stays the
// one row carrying the node id.
func TestRefreshGitHub_APullRequestReadUnderItsRepositorysNewNameKeepsItsEntity(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	fx := newGitHubScopeFixture(t)
	h, client := newFakeGitHubHost(t)
	trackRepos(t, fx.stores, org, []string{"octo/old"})
	if n, err := fx.stores.Repos.FillMissingExternalIDsSystem(ctx, org, []domain.RepoRef{
		{Source: domain.RepoSourceGitHub, Host: ghOldHost, Owner: "octo", Repo: "old", ExternalID: trackedRepositoryID},
	}); err != nil || n != 1 {
		t.Fatalf("record octo/old's repository id: n=%d err=%v", n, err)
	}

	h.listPR("octo/old", 7, "PR_7", "Widget")
	fx.cycle(t, ghOldHost, client, []string{"octo/old"})
	before := fx.entity(t, ghOldHost, "octo/old#7")
	if before == nil || before.ExternalID != "PR_7" {
		t.Fatalf("octo/old#7 = %+v, want an entity carrying its node id PR_7", before)
	}

	h.searchPR("octo/new", 7, "PR_7", "Widget")
	if _, _, err := fx.tr.RefreshGitHub(ctx, ghOldHost, client, "alice", []string{"octo/old"}, nil); err != nil {
		t.Fatalf("RefreshGitHub with the dashboard search: %v", err)
	}
	if seeded, err := fx.tr.BackfillDashboardHistory(ctx, ghOldHost, client, "alice", []string{"octo/old"}); err != nil || seeded != 0 {
		t.Fatalf("BackfillDashboardHistory = %d, %v; want nothing seeded", seeded, err)
	}
	if e := fx.entity(t, ghOldHost, "octo/new#7"); e != nil {
		t.Fatalf("octo/new#7 = %+v, want no second entity for PR_7", e)
	}
	if got, err := fx.stores.Entities.GetByExternalIDSystem(ctx, org, "github", ghOldHost, "PR_7"); err != nil || got == nil || got.ID != before.ID {
		t.Fatalf("PR_7 resolves to %+v (err %v), want the existing entity %s", got, err, before.ID)
	}

	out, err := fx.stores.Repos.RenameSystem(ctx, org, domain.RepoRef{
		Source: domain.RepoSourceGitHub, Host: ghOldHost, Owner: "octo", Repo: "new", ExternalID: trackedRepositoryID,
	})
	if err != nil || !out.Renamed {
		t.Fatalf("repository rename = %+v, %v; want it applied", out, err)
	}
	after := fx.entity(t, ghOldHost, "octo/new#7")
	if after == nil || after.ID != before.ID || after.ExternalID != "PR_7" {
		t.Fatalf("octo/new#7 = %+v, want entity %s renamed onto it, still carrying PR_7", after, before.ID)
	}
	active, err := fx.stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Errorf("%d active github entities, want 1", len(active))
	}
}

// Every way the poller meets a pull request leaves its entity carrying the
// node id: discovery creates a row with it, a row created before node ids
// were recorded learns it from the listing that finds it by its key, a stub
// learns it from the read that resolves it, and the dashboard backfill seeds
// a row with it.
func TestRefreshGitHub_EveryPullRequestEntityCarriesItsNodeID(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	fx := newGitHubScopeFixture(t)
	h, client := newFakeGitHubHost(t)
	trackRepos(t, fx.stores, org, []string{"octo/repo"})

	legacy := fx.seedPR(t, ghOldHost, "octo/repo#1", &domain.PRSnapshot{NodeID: "PR_1", Number: 1, Repo: "octo/repo", State: "OPEN"})
	stub := fx.seedPR(t, ghOldHost, "octo/repo#2", nil)
	if legacy.ExternalID != "" || stub.ExternalID != "" {
		t.Fatalf("seeded rows carry ids %q and %q, want none", legacy.ExternalID, stub.ExternalID)
	}
	h.listPR("octo/repo", 1, "PR_1", "Legacy")
	h.listPR("octo/repo", 3, "PR_3", "New")
	h.basicPR("octo/repo#2", "PR_2")

	fx.cycle(t, ghOldHost, client, []string{"octo/repo"})

	h.searchPR("octo/repo", 4, "PR_4", "History")
	if seeded, err := fx.tr.BackfillDashboardHistory(ctx, ghOldHost, client, "alice", []string{"octo/repo"}); err != nil || seeded != 1 {
		t.Fatalf("BackfillDashboardHistory = %d, %v; want octo/repo#4 seeded", seeded, err)
	}

	for key, want := range map[string]string{
		"octo/repo#1": "PR_1",
		"octo/repo#2": "PR_2",
		"octo/repo#3": "PR_3",
		"octo/repo#4": "PR_4",
	} {
		e := fx.entity(t, ghOldHost, key)
		if e == nil || e.ExternalID != want {
			t.Errorf("%s = %+v, want an entity carrying %s", key, e, want)
		}
	}
	if e := fx.entity(t, ghOldHost, "octo/repo#1"); e == nil || e.ID != legacy.ID {
		t.Errorf("octo/repo#1 = %+v, want the row created before node ids were recorded", e)
	}
	if e := fx.entity(t, ghOldHost, "octo/repo#2"); e == nil || e.ID != stub.ID {
		t.Errorf("octo/repo#2 = %+v, want the stub", e)
	}
}

// A listing can name a pull request under a key whose active entity carries
// another pull request's node id: the repository was deleted and recreated
// under its name, or renamed away and its name taken. The entity is left as
// it is, no second row is created, and the repository's listing cursor is not
// written, so the next cycle lists it again rather than answering 304.
func TestRefreshGitHub_AKeyHeldByAnotherPullRequestIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	fx := newGitHubScopeFixture(t)
	h, client := newFakeGitHubHost(t)
	trackRepos(t, fx.stores, org, []string{"octo/repo"})

	held, _, err := fx.stores.Entities.FindOrCreateSystem(ctx, org, "github", ghOldHost, "octo/repo#5", "PR_earlier5", "pr", "Earlier", "")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := json.Marshal(domain.PRSnapshot{NodeID: "PR_earlier5", Number: 5, Repo: "octo/repo", State: "OPEN", Title: "Earlier"})
	if ok, err := fx.stores.Entities.UpdateSnapshotCASSystem(ctx, org, held.ID, string(snap), held.PollSeq); err != nil || !ok {
		t.Fatalf("seed the earlier pull request's snapshot: ok=%v err=%v", ok, err)
	}
	h.listPR("octo/repo", 5, "PR_later5", "Later")

	fx.cycle(t, ghOldHost, client, []string{"octo/repo"})
	fx.cycle(t, ghOldHost, client, []string{"octo/repo"})

	e := fx.entity(t, ghOldHost, "octo/repo#5")
	if e == nil || e.ID != held.ID || e.ExternalID != "PR_earlier5" || e.Title != "Earlier" {
		t.Errorf("octo/repo#5 = %+v, want the earlier pull request's entity untouched", e)
	}
	if got, err := fx.stores.Entities.GetByExternalIDSystem(ctx, org, "github", ghOldHost, "PR_later5"); err != nil || got != nil {
		t.Errorf("PR_later5 resolves to %+v (err %v), want no entity", got, err)
	}
	if n := h.notModified(); n != 0 {
		t.Errorf("%d listings answered 304, want 0: the cursor must not move past a pull request TF did not store", n)
	}
}
