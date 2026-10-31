package poller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
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

func (c *countingResolver) ClientForInstallation(ctx context.Context, orgID string, inst domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return c.ClientFor(ctx, orgID, inst.AccountLogin)
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

	rec := notifytest.Record(t)
	m.runGitHubCycleForOrg(ctx, org)

	rec.Active(t, org, kinds.GitHubAppUnusable, "1")
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
// unusable stops nothing, and nobody is notified.
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

	rec := notifytest.Record(t)
	m.runGitHubCycleForOrg(ctx, org)

	rec.Inactive(t, org, kinds.GitHubAppUnusable)
	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entities; want 1 — a staged App's state must not cost the PAT's poll", len(ents))
	}
}

// appsStore is the registration a reconcile leaves, readable or not.
type appsStore struct {
	fakeInstallsStore
	readErr error
}

func (s *appsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.fakeInstallsStore.GetForOrgSystem(ctx, orgID)
}

// newAppHeadManager builds a Manager for an org that tracks nothing, so each
// GitHub cycle stops after the reconcile and the registration reads on either
// side of it. The reconcile answers for whatever registration apps holds, as
// the backfill would.
func newAppHeadManager(t *testing.T) (*Manager, *appsStore, db.Stores) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	seedBYOAppCredentialClass(t, stores, runmode.LocalDefaultOrgID)
	bus := eventbus.New()
	t.Cleanup(bus.Close)

	apps := &appsStore{}
	m := &Manager{
		database: database,
		pub:      busPublisher{bus: bus},
		tasks:    stores.Tasks,
		entities: stores.Entities, eventQueue: stores.EventQueue,
		repos: stores.Repos,
		orgs:  stores.Orgs,
		apps:  apps,
		ReconcileGrant: func(context.Context, string) error {
			if a := apps.app; a != nil && a.Unusable() {
				return &db.GitHubAppUnusableError{Reason: a.UnusableReason, Err: errors.New("githubapp: list installations: status 404")}
			}
			return nil
		},
	}
	return m, apps, stores
}

func unusableApp(appID string, reason domain.GitHubAppUnusableReason) *domain.OrgGitHubApp {
	return &domain.OrgGitHubApp{
		OrgID: runmode.LocalDefaultOrgID, AppID: appID, Slug: "tf", Active: true,
		UnusableReason: reason, UnusableSince: time.Now(),
	}
}

// TestRunGitHubCycleForOrg_UnusableAppNotifiesUntilAccepted: the notification
// is raised on every reconcile that finds the active App unusable, not only on
// the change, and resolves on the first one where GitHub accepts it again.
func TestRunGitHubCycleForOrg_UnusableAppNotifiesUntilAccepted(t *testing.T) {
	m, apps, _ := newAppHeadManager(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	rec := notifytest.Record(t)

	apps.app = unusableApp("1", domain.GitHubAppKeyRejected)
	m.runGitHubCycleForOrg(ctx, org)
	m.runGitHubCycleForOrg(ctx, org)
	open := rec.Open(org, kinds.GitHubAppUnusable)
	if len(open) != 1 || open[0].Key != "1" || open[0].Seen != 2 {
		t.Fatalf("open after two cycles = %+v; want App 1, raised by both", open)
	}
	if got := open[0].Params.(kinds.GitHubApp).Reason; got != domain.GitHubAppKeyRejected {
		t.Errorf("reason = %q; want %q", got, domain.GitHubAppKeyRejected)
	}

	accepted := *apps.app
	accepted.UnusableReason, accepted.UnusableSince = "", time.Time{}
	apps.app = &accepted
	m.runGitHubCycleForOrg(ctx, org)
	rec.Inactive(t, org, kinds.GitHubAppUnusable)
}

// TestRunGitHubCycleForOrg_AppUnusableFollowsTheOrgsApp: an occurrence is
// about one App, so it resolves when the org stops holding that App, by
// replacing it or by moving to the deployment App, which leaves no
// registration at all.
func TestRunGitHubCycleForOrg_AppUnusableFollowsTheOrgsApp(t *testing.T) {
	cases := map[string]struct {
		next     *domain.OrgGitHubApp
		class    domain.GitHubCredentialClass
		wantOpen []string
	}{
		"replaced by an App GitHub accepts": {
			next: &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "2", Slug: "tf2", Active: true},
		},
		"replaced by an App GitHub also refuses": {
			next:     unusableApp("2", domain.GitHubAppKeyRejected),
			wantOpen: []string{"2"},
		},
		"deployment App takes over": {
			class: domain.GitHubCredentialClassManagedApp,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, apps, stores := newAppHeadManager(t)
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID
			rec := notifytest.Record(t)

			apps.app = unusableApp("1", domain.GitHubAppMissing)
			m.runGitHubCycleForOrg(ctx, org)
			rec.Active(t, org, kinds.GitHubAppUnusable, "1")

			apps.app = tc.next
			if tc.class != "" {
				if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, org, tc.class); err != nil {
					t.Fatalf("SetGitHubCredentialClass: %v", err)
				}
			}
			m.runGitHubCycleForOrg(ctx, org)
			var keys []string
			for _, occ := range rec.Open(org, kinds.GitHubAppUnusable) {
				keys = append(keys, occ.Key)
			}
			if !slices.Equal(keys, tc.wantOpen) {
				t.Errorf("open = %v; want %v", keys, tc.wantOpen)
			}
		})
	}
}

// TestRunGitHubCycleForOrg_UnreadableRegistrationLeavesTheNotification: a
// cycle that cannot read the registration does not know whether the App is
// still refused, so it leaves the occurrence as it was.
func TestRunGitHubCycleForOrg_UnreadableRegistrationLeavesTheNotification(t *testing.T) {
	m, apps, _ := newAppHeadManager(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	rec := notifytest.Record(t)

	apps.app = unusableApp("1", domain.GitHubAppMissing)
	m.runGitHubCycleForOrg(ctx, org)
	apps.readErr = errors.New("database is locked")
	m.runGitHubCycleForOrg(ctx, org)
	rec.Active(t, org, kinds.GitHubAppUnusable, "1")
}
