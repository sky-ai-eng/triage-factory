package poller

import (
	"context"
	"errors"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The installation mirror used to be refreshed by pull in LOCAL MODE ONLY — the
// backfill call sat behind an isLocal gate, on the reasoning that webhooks
// cannot reach a laptop. That left multi mode converging on deliveries alone,
// and GitHub does not auto-retry a delivery it failed to make. An org that
// missed its `created` delivery then read as installed on no accounts
// permanently: degraded, cycle skipped, repository picker blank, with nothing
// on a timer to correct it.
//
// This pins the two properties that removes. The pass runs in BOTH modes, and
// it runs before the tracked-set gate — an org that tracks nothing returns
// before any other GitHub work, and is exactly the org where "the App can reach
// repositories nobody asked for" is largest, so skipping the pass there would
// blind the finding in the case that needs it most.
//
// Leader gating needs no test of its own here: the pass has one call site, and
// it is inside a poll cycle. In multi mode the poll scheduler runs only on the
// brain-lease holder (internal/app's startBrain / stopBrain drive it) and in
// local mode it always runs, so the pass inherits that gate rather than
// introducing a second one.
func TestRunGitHubCycleForOrg_ReconcilesTheGrantBeforeTheTrackedSetGate(t *testing.T) {
	for _, mode := range []runmode.Mode{runmode.ModeLocal, runmode.ModeMulti} {
		t.Run(string(mode), func(t *testing.T) {
			runmode.SetForTest(t, mode)

			ctx := context.Background()
			database := newMigratedSQLiteForPoller(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID

			bus := eventbus.New()
			t.Cleanup(bus.Close)

			var reconciled []string
			m := &Manager{
				database:       database,
				pub:            busPublisher{bus: bus},
				tasks:          stores.Tasks,
				entities:       stores.Entities,
				repos:          stores.Repos,
				orgs:           stores.Orgs,
				users:          stores.Users,
				apps:           &fakeInstallsStore{},
				resolver:       &fakeResolver{},
				ReconcileGrant: func(_ context.Context, orgID string) error { reconciled = append(reconciled, orgID); return nil },
			}

			// No repos configured: the cycle returns at the tracked-set gate, so
			// anything that ran did so ahead of it.
			m.runGitHubCycleForOrg(ctx, org)

			if len(reconciled) != 1 || reconciled[0] != org {
				t.Fatalf("grant reconcile ran %v; want exactly [%s] in %s mode", reconciled, org, mode)
			}
		})
	}
}

func TestRunGitHubCycleForOrg_GrantReconcileFailureDoesNotSkipThePoll(t *testing.T) {
	// Best-effort: a mirror TF fails to refresh is one it refreshes next cycle,
	// and never a reason to skip the poll the caller actually came for. The
	// reconcile leaves the previous answer in place on failure, so a stale
	// mirror is the worst outcome.
	runmode.SetForTest(t, runmode.ModeMulti)

	srv := pollerTestServer(t)
	ctx := context.Background()
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	m := &Manager{
		database:       database,
		pub:            busPublisher{bus: bus},
		tasks:          stores.Tasks,
		entities:       stores.Entities,
		repos:          stores.Repos,
		orgs:           stores.Orgs,
		users:          stores.Users,
		apps:           &fakeInstallsStore{},
		resolver:       &fakeResolver{client: ghclient.NewClient(srv.URL, "pat")},
		ReconcileGrant: func(context.Context, string) error { return errors.New("github unreachable") },
	}

	m.runGitHubCycleForOrg(ctx, org)

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entities; want 1 — a failed mirror refresh must not cost the poll", len(ents))
	}
}
