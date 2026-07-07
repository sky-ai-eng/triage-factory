package poller

import "time"

// SetGitHubHeartbeatForTest backdates the GitHub base-tick heartbeat,
// bypassing the real ~30s ticker. /readyz's "poller heartbeat aged past
// 3x basePollInterval" acceptance criterion (TFAC-573) needs to trip the
// hard check deterministically without a 90s+ sleep; this is that seam.
//
// Lives in a non-_test.go file (mirrors internal/runmode's SetForTest) so
// internal/server's readyz tests — a different package — can call it; a
// _test.go file here would only be visible to package poller's own tests.
func (m *Manager) SetGitHubHeartbeatForTest(at time.Time) {
	m.heartbeatMu.Lock()
	m.lastGithubTick = at
	m.heartbeatMu.Unlock()
}

// SetJiraHeartbeatForTest is SetGitHubHeartbeatForTest's Jira twin.
func (m *Manager) SetJiraHeartbeatForTest(at time.Time) {
	m.heartbeatMu.Lock()
	m.lastJiraTick = at
	m.heartbeatMu.Unlock()
}

// SetGitHubSuccessForTest backdates orgID's last-successful-GitHub-poll
// timestamp — the /readyz seam for exercising the soft "stale
// last_successful_poll" signal (top-level status "degraded") without
// running a real poll cycle.
func (m *Manager) SetGitHubSuccessForTest(orgID string, at time.Time) {
	m.stampSuccessForTest(&m.lastGithubSuccess, orgID, at)
}

// SetJiraSuccessForTest is SetGitHubSuccessForTest's Jira twin.
func (m *Manager) SetJiraSuccessForTest(orgID string, at time.Time) {
	m.stampSuccessForTest(&m.lastJiraSuccess, orgID, at)
}

func (m *Manager) stampSuccessForTest(dst *map[string]time.Time, orgID string, at time.Time) {
	m.pollSuccessMu.Lock()
	if *dst == nil {
		*dst = make(map[string]time.Time)
	}
	(*dst)[orgID] = at
	m.pollSuccessMu.Unlock()
}
