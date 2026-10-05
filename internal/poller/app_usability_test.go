package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// countingResolver records every client resolution, which is every request
// that would mint an installation token.
type countingResolver struct {
	fakeResolver
	calls int
}

func (c *countingResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	c.calls++
	return c.fakeResolver.ClientFor(ctx, orgID, target)
}

// unusableReconcile stands in for the grant reconcile on an App deleted on
// GitHub: it records the reason on the registration, as the backfill does, and
// returns the diagnosis.
func unusableReconcile(apps *fakeInstallsStore) func(context.Context, string) error {
	return func(context.Context, string) error {
		marked := *apps.app
		marked.UnusableReason = domain.GitHubAppMissing
		marked.UnusableSince = time.Now()
		apps.app = &marked
		return &db.GitHubAppUnusableError{Reason: domain.GitHubAppMissing, Err: errors.New("githubapp: list installations: status 404")}
	}
}

// TestRunGitHubCycleForOrg_UnusableAppSkipsBeforeAnyMint pins the cycle half of
// an App deleted on GitHub: once the reconcile has recorded it, the cycle
// reports degraded and returns before the groups reconcile and the
// per-installation fan-out, each of which would otherwise resolve a client and
// fail a mint against an App that no longer exists, every cycle.
func TestRunGitHubCycleForOrg_UnusableAppSkipsBeforeAnyMint(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	seedBYOAppCredentialClass(t, stores, org)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	apps := &fakeInstallsStore{
		app:      &domain.OrgGitHubApp{OrgID: org, AppID: "1", Slug: "tf", Active: true},
		installs: []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "octo"}},
	}
	resolver := &countingResolver{fakeResolver: fakeResolver{client: ghclient.NewClient(srv.URL, "pat")}}
	var reportedErr error
	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities, eventQueue: stores.EventQueue,
		repos:          stores.Repos,
		orgs:           stores.Orgs,
		apps:           apps,
		resolver:       resolver,
		githubGroups:   stores.TeamGitHubGroups,
		ReconcileGrant: unusableReconcile(apps),
		OnError:        func(_, _ string, err error) { reportedErr = err },
	}

	m.runGitHubCycleForOrg(ctx, org)

	if resolver.calls != 0 {
		t.Errorf("resolved %d clients for an App GitHub no longer accepts; want 0", resolver.calls)
	}
	if reportedErr == nil {
		t.Error("OnError was not called; an unusable App must report degraded health")
	}
	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("got %d entities; want 0 — nothing may poll under an unusable App", len(ents))
	}
}

// TestRunGitHubCycleForOrg_UnusableStagedAppStillPollsViaPAT: a staged App is
// not the org's credential yet — the PAT it would replace is — so its being
// unusable stops nothing.
func TestRunGitHubCycleForOrg_UnusableStagedAppStillPollsViaPAT(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})
	seedBYOAppCredentialClass(t, stores, org)

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	apps := &fakeInstallsStore{app: &domain.OrgGitHubApp{OrgID: org, AppID: "1", Slug: "tf", Active: false}}
	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities, eventQueue: stores.EventQueue,
		repos:          stores.Repos,
		orgs:           stores.Orgs,
		apps:           apps,
		resolver:       &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
		ReconcileGrant: unusableReconcile(apps),
	}

	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entities; want 1 — a staged App's state must not cost the PAT's poll", len(ents))
	}
}
