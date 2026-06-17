package poller

import (
	"context"
	"errors"
	"log"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// BackfillUserDashboard seeds the trailing-window merged/closed/reviewed
// history for one user's GitHub login into the org's entity store, so the
// personal dashboard isn't blank for history that predates tracking. It is the
// multi-mode counterpart of the local per-cycle Phase 1b backfill: an App
// installation token (and a multi-mode org PAT) has no viewer, so the org poll
// cycle can't run a "me" search; the server calls this per bound user instead —
// on identity bind, and lazily on first dashboard load (TFAC-396).
//
// Credential-agnostic: it resolves the org's GitHub client(s) exactly as the
// poll cycle does (App installations over their grant intersection, else the
// org PAT), so App and org-PAT orgs are handled identically.
//
// Once-per-(user,host): the dashboard_backfilled_at marker gates it so a
// re-poll or repeated dashboard opens never re-fire the search burst (its
// per-installation cost would otherwise scale with viewers on one shared
// installation rate budget). An in-process singleflight collapses concurrent
// kicks for the same key. On a hard failure (no usable credential, or every
// search query failed) the marker is left unset so the next dashboard load
// retries; partial per-installation failures are logged and the marker is still
// set, since organic forward tracking covers the gap.
//
// Local mode is a no-op: its per-cycle Phase 1b owns dashboard history and is
// self-healing, so there's nothing to seed here.
func (m *Manager) BackfillUserDashboard(ctx context.Context, orgID, userID, login, host string) error {
	if runmode.Current() == runmode.ModeLocal {
		return nil
	}
	if login == "" || host == "" || m.users == nil {
		return nil
	}

	key := orgID + "\x00" + userID + "\x00" + host
	if _, inflight := m.dashboardBackfillInflight.LoadOrStore(key, struct{}{}); inflight {
		return nil
	}
	defer m.dashboardBackfillInflight.Delete(key)

	// Marker gate: already backfilled this (user, host) → nothing to do.
	if at, err := m.users.DashboardBackfilledAtSystem(ctx, userID, host); err != nil {
		return err
	} else if at != nil {
		return nil
	}

	repos, err := m.repos.ListConfiguredNamesSystem(ctx, orgID)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		// Nothing configured to scope a search to — mark done so an empty org
		// doesn't retry the (no-op) backfill on every dashboard open.
		return m.users.MarkDashboardBackfilledSystem(ctx, userID, host)
	}

	if err := m.runDashboardBackfill(ctx, orgID, login, repos); err != nil {
		return err // leave the marker unset → next dashboard load retries
	}
	return m.users.MarkDashboardBackfilledSystem(ctx, userID, host)
}

// runDashboardBackfill resolves the org's GitHub client(s) like the poll cycle
// and runs the per-login history search+seed over each. Returns an error only
// when no usable credential resolved (or every search failed against the
// installations that did) so the caller can retry; per-installation failures
// and not-configured orgs are tolerated.
func (m *Manager) runDashboardBackfill(ctx context.Context, orgID, login string, repos []string) error {
	tr := m.trackerForOrg(orgID)

	// PAT path (org PAT in multi mode, or any non-App org): one client over the
	// full configured set. orgHasRegisteredApp mirrors runGitHubCycleForOrg's
	// either/or gate — a staged (inactive) App keeps the PAT live.
	if !m.orgHasRegisteredApp(ctx, orgID) {
		client, err := m.resolver.ClientFor(ctx, orgID, "")
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				return nil // not configured for GitHub — nothing to seed
			}
			return err
		}
		_, err = tr.BackfillDashboardHistory(client, login, repos)
		return err
	}

	// App path: search per installation over its grant intersection, mirroring
	// runGitHubCycleForOrg so reach matches the poll cycle's exactly.
	installs, err := m.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		return err
	}
	covered := make(map[string]bool, len(repos))
	anyFunctional := false
	ran := false
	var lastErr error
	for _, inst := range installs {
		client, cerr := m.resolver.ClientFor(ctx, orgID, inst.AccountLogin)
		if cerr != nil {
			log.Printf("[dashboard-backfill] org %s: resolve client for installation %s: %v", orgID, inst.AccountLogin, cerr)
			continue
		}
		grant, gerr := client.ListInstallationRepos()
		if gerr != nil {
			log.Printf("[dashboard-backfill] org %s: list installation repos for %s: %v", orgID, inst.AccountLogin, gerr)
			continue
		}
		anyFunctional = true
		scoped := intersectConfigured(repos, grant, covered)
		if len(scoped) == 0 {
			continue
		}
		if _, berr := tr.BackfillDashboardHistory(client, login, scoped); berr != nil {
			lastErr = berr
			log.Printf("[dashboard-backfill] org %s installation %s: %v", orgID, inst.AccountLogin, berr)
			continue
		}
		ran = true
	}
	if !anyFunctional {
		return errors.New("dashboard backfill: no functional app installation")
	}
	// Some installation had in-scope repos but every search against them failed
	// — surface it so the marker stays unset and the next load retries. When no
	// installation grants any configured repo (ran=false, lastErr=nil) the
	// backfill is a legitimate no-op and we let the caller mark it done.
	if !ran && lastErr != nil {
		return lastErr
	}
	return nil
}
