package poller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// DefaultSourceOrgsInterval is how often the source orgs observer re-reads
// every org's source states. A state changes only when an org's admin
// connects, disconnects or turns off a source, or installs or removes its
// GitHub App, and the gauge feeds a dashboard rather than an alert. Each
// org's read loads its whole credential bundle, about a dozen small queries,
// so the interval is longer than the other connection gauges'.
const DefaultSourceOrgsInterval = 5 * time.Minute

// sourceStateNoInstallations is the gauge's one state outside the
// event-source vocabulary: an org whose GitHub is available through an active
// App of its own that is installed on no account on the org's host.
const sourceStateNoInstallations = "no_installations"

// sourceOrgStates is every (source, state) pair tf_source_orgs reports, a 0
// included when no org is in it: the event-source states each core source can
// take, with GitHub's available split by no_installations. GitHub is
// required, so it is never disabled. A source outside this list, such as one
// registered from outside core, is not counted, since its state set is not
// fixed here.
var sourceOrgStates = []struct {
	source string
	states []string
}{
	{eventsource.KindGitHub, []string{string(eventsource.StateAvailable), sourceStateNoInstallations, string(eventsource.StateUnconfigured)}},
	{eventsource.KindJira, []string{string(eventsource.StateAvailable), string(eventsource.StateUnconfigured), string(eventsource.StateDisabled)}},
	{eventsource.KindLinear, []string{string(eventsource.StateAvailable), string(eventsource.StateUnconfigured), string(eventsource.StateDisabled)}},
}

// SourceOrgsSource is what the source orgs observer reads: the active orgs,
// each org's resolved event-source states, and, for an org whose GitHub is
// available, the settings, App registration and installations that decide
// whether it is no_installations. SourceOrgsStores composes it from the
// stores.
type SourceOrgsSource interface {
	ListActiveSystem(ctx context.Context) ([]string, error)
	ResolveSystem(ctx context.Context, orgID string) (eventsource.Availability, error)
	GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error)
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error)
	ListInstallationsOnHostSystem(ctx context.Context, orgID, githubHost string) ([]domain.OrgGitHubAppInstallation, error)
}

type sourceOrgsStores struct {
	db.OrgsStore
	db.GitHubAppsStore
	stores db.Stores
}

func (s sourceOrgsStores) ResolveSystem(ctx context.Context, orgID string) (eventsource.Availability, error) {
	return eventsource.ResolveSystem(ctx, s.stores, orgID)
}

// SourceOrgsStores is the SourceOrgsSource the stores make together.
func SourceOrgsStores(stores db.Stores) SourceOrgsSource {
	return sourceOrgsStores{OrgsStore: stores.Orgs, GitHubAppsStore: stores.GitHubApps, stores: stores}
}

// SourceOrgsObserver exports tf_source_orgs{source, state}: the number of
// active orgs in each state of each core source. An org that has not
// connected a source, or whose admin turned it off, is skipped by that
// source's poll before it makes a request, so it has no tf_upstream_up series
// for it; this is where it is counted. The states are the ones
// eventsource.ResolveSystem answers, the same derivation the org's sources
// route reads, so the two cannot disagree. The one refinement is GitHub's
// no_installations (see noInstallations).
//
// It reads on a ticker and reports at scrape time, the same shape and
// lifecycle as ConnectionObserver, and for the same reason runs on the
// background brain: one process exports it.
type SourceOrgsObserver struct {
	src SourceOrgsSource

	mu sync.Mutex
	// states is each org's state per source, from its last successful read.
	states map[string]map[string]string
	// sampled is set by the first read of the org list that succeeds. Until
	// then nothing is reported, since a 0 for orgs no read has counted would
	// read as a deployment with none.
	sampled bool
	reg     metric.Registration
}

type sourceStateKey struct{ source, state string }

// NewSourceOrgsObserver creates the gauge against a provider and registers the
// callback that reports it. An instrument-creation error can only be a
// programmer error; the observer then reports nothing and its ticks still
// run.
func NewSourceOrgsObserver(provider metric.MeterProvider, src SourceOrgsSource) *SourceOrgsObserver {
	c := &SourceOrgsObserver{src: src, states: map[string]map[string]string{}}
	m := provider.Meter(meterName)
	gauge, err := m.Int64ObservableGauge("source.orgs",
		metric.WithDescription("Active orgs in each state of each source: available, unconfigured (not connected) or disabled (turned off by the org's admin). GitHub has no disabled, and counts an org whose own GitHub App is installed on no account on its host as no_installations rather than available."))
	if err != nil {
		pollerLog.Error("source orgs gauge setup failed", "instrument", "source.orgs", "error", err)
		return c
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.sampled {
			return nil
		}
		counts := make(map[sourceStateKey]int64)
		for _, decl := range sourceOrgStates {
			for _, state := range decl.states {
				counts[sourceStateKey{decl.source, state}] = 0
			}
		}
		for _, states := range c.states {
			for source, state := range states {
				counts[sourceStateKey{source, state}]++
			}
		}
		for k, n := range counts {
			o.ObserveInt64(gauge, n, metric.WithAttributes(telemetry.Source(k.source), attribute.String("state", k.state)))
		}
		return nil
	}, gauge)
	if err != nil {
		pollerLog.Error("source orgs gauge callback registration failed", "error", err)
		return c
	}
	c.reg = reg
	return c
}

// Close unregisters the gauge callback, so a stopped observer stops
// reporting. See ConnectionObserver.Close.
func (c *SourceOrgsObserver) Close() {
	c.mu.Lock()
	reg := c.reg
	c.reg = nil
	c.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			pollerLog.Error("source orgs gauge callback unregister failed", "error", err)
		}
	}
}

// Tick reads every active org's states once. The result replaces the
// previous one whole, so a deleted org stops being counted. A failed read
// keeps the previous states: all of them when the org list cannot be read,
// and the one org's when its own reads fail, so one bad org neither freezes
// the gauge nor drops out of it.
func (c *SourceOrgsObserver) Tick(ctx context.Context) {
	if c.src == nil {
		return
	}
	orgIDs, err := c.src.ListActiveSystem(ctx)
	if err != nil {
		pollerLog.Error("source state read failed; keeping the previous values", "error", err)
		return
	}
	c.mu.Lock()
	prev := c.states
	c.mu.Unlock()
	states := make(map[string]map[string]string, len(orgIDs))
	for _, orgID := range orgIDs {
		// A brain demoting mid-tick cancels ctx, which would fail every read
		// left in the pass. Stop instead, keeping the previous values.
		if ctx.Err() != nil {
			return
		}
		st, err := c.readOrg(ctx, orgID)
		if err != nil {
			pollerLog.Error("source state read failed; keeping the org's previous states", "org", orgID, "error", err)
			if p, ok := prev[orgID]; ok {
				states[orgID] = p
			}
			continue
		}
		states[orgID] = st
	}
	c.mu.Lock()
	c.states, c.sampled = states, true
	c.mu.Unlock()
}

// readOrg returns one org's state for each core source.
func (c *SourceOrgsObserver) readOrg(ctx context.Context, orgID string) (map[string]string, error) {
	avail, err := c.src.ResolveSystem(ctx, orgID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(sourceOrgStates))
	for _, decl := range sourceOrgStates {
		if st, ok := avail.State(decl.source); ok {
			states[decl.source] = string(st)
		}
	}
	if states[eventsource.KindGitHub] == string(eventsource.StateAvailable) {
		none, err := c.noInstallations(ctx, orgID)
		if err != nil {
			return nil, err
		}
		if none {
			states[eventsource.KindGitHub] = sourceStateNoInstallations
		}
	}
	return states, nil
}

// noInstallations reports whether the org's GitHub credential is an active App
// of its own with no live installation on the org's host. Each GitHub poll
// skips such an org as installed on no accounts, while availability reports
// it available, because for an org's own App it reads the registration and
// not its installations. A staged App is not the org's credential: the PAT it
// would replace still polls, with no installation needed.
func (c *SourceOrgsObserver) noInstallations(ctx context.Context, orgID string) (bool, error) {
	set, err := c.src.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read org settings: %w", err)
	}
	if set.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
		return false, nil
	}
	app, err := c.src.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read org github app: %w", err)
	}
	if app == nil || !app.Active {
		return false, nil
	}
	insts, err := c.src.ListInstallationsOnHostSystem(ctx, orgID, db.EffectiveGitHubHost(set.GitHubBaseURL))
	if err != nil {
		return false, fmt.Errorf("list installations: %w", err)
	}
	return len(insts) == 0, nil
}

// Run ticks until ctx is cancelled, then unregisters the gauge for a caller
// that did not already. The first read happens on the first tick rather than
// at start, for ConnectionObserver.Run's reason.
func (c *SourceOrgsObserver) Run(ctx context.Context, interval time.Duration) {
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
