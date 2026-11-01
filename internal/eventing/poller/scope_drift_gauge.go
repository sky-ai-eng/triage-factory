package poller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// DefaultScopeDriftInterval is how often the scope drift observer re-reads
// the finding. The finding changes when the GitHub cycle refreshes the grant
// mirror, once per poll interval, or when a team's tracked set changes, and
// the alert on it waits an hour; a minute keeps the reads, several per App
// org, few.
const DefaultScopeDriftInterval = time.Minute

// ScopeDriftSource is what the scope drift observer reads: the active orgs,
// each org's credential class and GitHub host, its live installations on that
// host, whether its grant mirror has been refreshed, and the scope drift
// finding's count. ScopeDriftStores composes it from the stores.
type ScopeDriftSource interface {
	ListActiveSystem(ctx context.Context) ([]string, error)
	GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error)
	ListInstallationsOnHostSystem(ctx context.Context, orgID, githubHost string) ([]domain.OrgGitHubAppInstallation, error)
	ReachableStateSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass) (domain.ReachableCacheState, error)
	ListScopeDriftSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, opts db.ListOpts) ([]domain.ScopeDriftRepository, int, error)
}

type scopeDriftStores struct {
	db.OrgsStore
	db.GitHubAppsStore
	db.ReachableReposStore
}

// ScopeDriftStores is the ScopeDriftSource the stores make together.
func ScopeDriftStores(orgs db.OrgsStore, apps db.GitHubAppsStore, reachable db.ReachableReposStore) ScopeDriftSource {
	return scopeDriftStores{OrgsStore: orgs, GitHubAppsStore: apps, ReachableReposStore: reachable}
}

// ScopeDriftObserver exports each org's scope drift finding as
// tf_github_scope_drift_repos: the number of repositories some team tracks
// that the org's GitHub App grant does not contain, which the poll therefore
// skips. The count is the one the GitHub access settings panel shows, read
// through the same store method, so the two cannot disagree. It reads on a
// ticker and reports at scrape time, the same shape and lifecycle as
// ConnectionObserver, and for the same reason runs on the background brain:
// one process exports it.
//
// Only an org whose answer can be trusted gets a series: one whose credential
// class is an App's, with a GitHub host and a grant mirror refreshed at least
// once. A PAT org holds no grant, and an unrefreshed mirror has not been
// looked at. A count above 0 is then always reported, since every repository
// it counts is outside the grant. A 0 is reported only when no live
// installation's grant is of unknown width: the finding does not report the
// repositories on such an account, so a 0 there could hide drift. An org
// whose installations all grant every repository is reported like any other,
// because the finding still counts what it tracks on an account no
// installation covers, such as one the App was uninstalled from. So a 0 is
// always an all-clear.
type ScopeDriftObserver struct {
	src ScopeDriftSource

	mu     sync.Mutex
	counts map[string]int64
	reg    metric.Registration
}

// NewScopeDriftObserver creates the gauge against a provider and registers
// the callback that reports it. An instrument-creation error can only be a
// programmer error; the observer then reports nothing and its ticks still
// run.
func NewScopeDriftObserver(provider metric.MeterProvider, src ScopeDriftSource) *ScopeDriftObserver {
	c := &ScopeDriftObserver{src: src, counts: map[string]int64{}}
	m := provider.Meter(meterName)
	gauge, err := m.Int64ObservableGauge("github.scope_drift.repos",
		metric.WithDescription("Repositories a team in the org tracks that its GitHub App grant does not contain, so they are not polled. No series for an org connected with a token, before its grant mirror is first refreshed, or for a 0 while one of its grants is of unknown width."))
	if err != nil {
		pollerLog.Error("scope drift gauge setup failed", "instrument", "github.scope_drift.repos", "error", err)
		return c
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for orgID, n := range c.counts {
			o.ObserveInt64(gauge, n, metric.WithAttributes(telemetry.OrgID(orgID)))
		}
		return nil
	}, gauge)
	if err != nil {
		pollerLog.Error("scope drift gauge callback registration failed", "error", err)
		return c
	}
	c.reg = reg
	return c
}

// Close unregisters the gauge callback, so a stopped observer stops
// reporting. See ConnectionObserver.Close.
func (c *ScopeDriftObserver) Close() {
	c.mu.Lock()
	reg := c.reg
	c.reg = nil
	c.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			pollerLog.Error("scope drift gauge callback unregister failed", "error", err)
		}
	}
}

// Tick reads every active org's finding once. The result replaces the
// previous one whole, so an org that stops qualifying (it moved to a PAT, or
// was deleted) stops reporting. A failed read keeps the previous value: all
// of them when the org list cannot be read, and the one org's when its own
// reads fail.
func (c *ScopeDriftObserver) Tick(ctx context.Context) {
	if c.src == nil {
		return
	}
	orgIDs, err := c.src.ListActiveSystem(ctx)
	if err != nil {
		pollerLog.Error("scope drift read failed; keeping the previous values", "error", err)
		return
	}
	c.mu.Lock()
	prev := c.counts
	c.mu.Unlock()
	counts := make(map[string]int64, len(prev))
	for _, orgID := range orgIDs {
		n, reported, err := c.readOrg(ctx, orgID)
		if err != nil {
			pollerLog.Error("scope drift read failed; keeping the org's previous value", "org", orgID, "error", err)
			if v, ok := prev[orgID]; ok {
				counts[orgID] = v
			}
			continue
		}
		if reported {
			counts[orgID] = int64(n)
		}
	}
	c.mu.Lock()
	c.counts = counts
	c.mu.Unlock()
}

// readOrg returns one org's scope drift count, and whether the org gets a
// series at all (see ScopeDriftObserver).
func (c *ScopeDriftObserver) readOrg(ctx context.Context, orgID string) (int, bool, error) {
	set, err := c.src.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return 0, false, fmt.Errorf("read org settings: %w", err)
	}
	class := set.GitHubCredentialClass
	if !class.AppTier() {
		return 0, false, nil
	}
	host := db.EffectiveGitHubHost(set.GitHubBaseURL)
	if host == "" {
		return 0, false, nil
	}
	state, err := c.src.ReachableStateSystem(ctx, orgID, host, class)
	if err != nil {
		return 0, false, fmt.Errorf("read grant mirror state: %w", err)
	}
	if !state.Known() {
		return 0, false, nil
	}
	_, total, err := c.src.ListScopeDriftSystem(ctx, orgID, host, class, db.ListOpts{CountOnly: true})
	if err != nil {
		return 0, false, fmt.Errorf("count scope drift: %w", err)
	}
	if total > 0 {
		return total, true, nil
	}
	insts, err := c.src.ListInstallationsOnHostSystem(ctx, orgID, host)
	if err != nil {
		return 0, false, fmt.Errorf("list installations: %w", err)
	}
	for _, inst := range insts {
		if inst.RepositorySelection == "" {
			return 0, false, nil
		}
	}
	return 0, true, nil
}

// Run ticks until ctx is cancelled, then unregisters the gauge for a caller
// that did not already. The first read happens on the first tick rather than
// at start, for ConnectionObserver.Run's reason.
func (c *ScopeDriftObserver) Run(ctx context.Context, interval time.Duration) {
	defer c.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Tick(ctx)
		}
	}
}
