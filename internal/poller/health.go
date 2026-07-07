package poller

import (
	"context"
	"time"
)

// heartbeatStaleFactor is the multiple of basePollInterval past which a
// source's base-tick loop is considered dead by the /readyz hard check
// (TFAC-573). 3x basePollInterval = 90s: generous enough that one slow
// cycle doesn't false-positive, tight enough to surface a genuinely
// stuck/crashed loop within a couple of minutes.
const heartbeatStaleFactor = 3

// HealthSnapshot is the poller state GET /readyz needs, gathered fresh on
// every call so the endpoint never serves cached liveness — Health() is
// cheap (one ListActiveSystem + settings read per active org).
type HealthSnapshot struct {
	GitHub SourceHealth
	Jira   SourceHealth
}

// SourceHealth is one poll source's (github/jira) liveness state.
type SourceHealth struct {
	// Alive reports whether the base-tick loop woke within
	// heartbeatStaleFactor*basePollInterval of now — the /readyz hard
	// check. False both for a stalled/crashed loop and for the brief
	// window right after boot before the loop's first tick lands.
	Alive bool
	// LastTick is the wall-clock time of the last base-tick heartbeat; the
	// zero Time if the loop has never woken.
	LastTick time.Time
	// Orgs is keyed by org ID, one entry per org ListActiveSystem returned
	// at call time — the /readyz soft per-org poll-staleness signal.
	Orgs map[string]OrgPollHealth
}

// OrgPollHealth is one active org's last-successful-poll + configured
// interval for one source.
type OrgPollHealth struct {
	// LastSuccess is the zero Time if this org has never completed a poll
	// of this source since process start (a freshly-added org, an org not
	// configured for this source, or one whose polls have only ever
	// errored) — /readyz treats a zero LastSuccess as "no signal yet"
	// rather than stale, so an unconfigured or brand-new org doesn't read
	// as degraded.
	LastSuccess time.Time
	// IntervalSeconds is the org's configured poll interval for this
	// source, clamped to basePollInterval — the denominator the operator
	// (or /readyz's own default) divides age_seconds by to judge
	// staleness.
	IntervalSeconds int
}

// Health returns a fresh liveness snapshot: each source's base-tick-alive
// hard check plus, for every currently active org, that source's
// last-successful-poll + configured interval. Reads m.orgs at call
// time — the active-org roster can change between calls, and re-listing
// it is cheap even at multi-mode scale — so repeated /readyz polls always
// see the current set.
func (m *Manager) Health(ctx context.Context) HealthSnapshot {
	return HealthSnapshot{
		GitHub: m.sourceHealth(ctx, "github"),
		Jira:   m.sourceHealth(ctx, "jira"),
	}
}

func (m *Manager) sourceHealth(ctx context.Context, source string) SourceHealth {
	lastTick := m.heartbeat(source)
	alive := !lastTick.IsZero() && time.Since(lastTick) <= heartbeatStaleFactor*basePollInterval

	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		pollerLog.Warn("readyz: list active orgs failed", "source", source, "error", err)
		return SourceHealth{Alive: alive, LastTick: lastTick, Orgs: map[string]OrgPollHealth{}}
	}

	successMap := m.successSnapshot(source)
	orgs := make(map[string]OrgPollHealth, len(orgIDs))
	for _, orgID := range orgIDs {
		interval := m.loadOrgSettings(ctx, orgID).GitHubPollInterval
		if source == "jira" {
			interval = m.loadOrgSettings(ctx, orgID).JiraPollInterval
		}
		orgs[orgID] = OrgPollHealth{
			LastSuccess:     successMap[orgID],
			IntervalSeconds: int(clampPollInterval(interval).Seconds()),
		}
	}
	return SourceHealth{Alive: alive, LastTick: lastTick, Orgs: orgs}
}

func (m *Manager) heartbeat(source string) time.Time {
	m.heartbeatMu.Lock()
	defer m.heartbeatMu.Unlock()
	if source == "jira" {
		return m.lastJiraTick
	}
	return m.lastGithubTick
}

// successSnapshot returns a shallow copy of the per-org last-success map
// for source, so the caller can read it without holding pollSuccessMu.
func (m *Manager) successSnapshot(source string) map[string]time.Time {
	m.pollSuccessMu.Lock()
	defer m.pollSuccessMu.Unlock()
	src := m.lastGithubSuccess
	if source == "jira" {
		src = m.lastJiraSuccess
	}
	out := make(map[string]time.Time, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (m *Manager) stampGitHubHeartbeat() {
	m.heartbeatMu.Lock()
	m.lastGithubTick = time.Now()
	m.heartbeatMu.Unlock()
}

func (m *Manager) stampJiraHeartbeat() {
	m.heartbeatMu.Lock()
	m.lastJiraTick = time.Now()
	m.heartbeatMu.Unlock()
}

func (m *Manager) stampGitHubSuccess(orgID string) {
	m.pollSuccessMu.Lock()
	if m.lastGithubSuccess == nil {
		m.lastGithubSuccess = make(map[string]time.Time)
	}
	m.lastGithubSuccess[orgID] = time.Now()
	m.pollSuccessMu.Unlock()
}

func (m *Manager) stampJiraSuccess(orgID string) {
	m.pollSuccessMu.Lock()
	if m.lastJiraSuccess == nil {
		m.lastJiraSuccess = make(map[string]time.Time)
	}
	m.lastJiraSuccess[orgID] = time.Now()
	m.pollSuccessMu.Unlock()
}
