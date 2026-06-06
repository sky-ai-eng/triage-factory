// The wizard's step registry. The shell shipped two trivial proof steps; this
// fleshes out the four ORGANIZATION steps — GitHub (mandatory), Trackers
// (optional), Poller timings, and the org max model tier — composing the
// existing shared field groups into the same step contract with no new host
// plumbing. The team section keeps its trivial proof step (delegation
// defaults) until the team-steps ticket replaces it.
//
// Reuse rule: every step composes a shared group (GitHubAccessGroup /
// GitHubAppPanel via GitHubStep, JiraAccessGroup via TrackersStep,
// PollerTimingGroup, ModelGroup, TeamSettingsGroup) — no parallel field UIs.
// Org persistence rides the single session-scoped POST /api/settings/org via
// saveOrgConfig, so there is no wizard-only persistence path to drift. Steps
// whose creds are connected mid-step (GitHub, Jira) persist at their own
// Connect action; the framework's per-step persist is then a no-op advance.

import GitHubStep from './GitHubStep'
import TrackersStep from './TrackersStep'
import ModelGroup from '../settings/ModelGroup'
import PollerTimingGroup from '../settings/PollerTimingGroup'
import TeamSettingsGroup from '../settings/TeamSettingsGroup'
import {
  emptyOrgConfig,
  fetchOrgSettings,
  orgConfigFromSettings,
  saveOrgConfig,
} from '../settings/orgConfig'
import {
  emptyTeamConfig,
  fetchTeamSettings,
  saveTeamSettings,
  teamConfigFromSettings,
} from '../settings/teamConfig'
import type { WizardState, WizardStep } from './types'

// Fresh wizard state before any load lands. Reuses the same empty-form
// factories the Settings/create pages use, so the shell starts from the
// identical baseline shape; the org-step flags default to "not connected".
export const initialWizardState = (): WizardState => ({
  org: emptyOrgConfig(),
  orgLoaded: false,
  hasGitHubPat: false,
  githubReady: false,
  jiraConnected: false,
  tracker: 'none',
  team: emptyTeamConfig(),
})

const TIER_LABELS: Record<string, string> = {
  haiku: 'Haiku',
  sonnet: 'Sonnet',
  opus: 'Opus',
}

// hostOf renders a base URL's host for a collapsed-bar summary.
function hostOf(url: string): string {
  try {
    return new URL(url).host || url
  } catch {
    return url.replace(/^https?:\/\//, '')
  }
}

// intervalLabel renders a Go duration like "5m0s" / "30s" compactly: trim the
// dangling "0s" only when it follows a minutes part ("5m0s" → "5m"), so a
// sub-minute value like "30s" is left intact (a naive /0s$/ would mangle it to
// "3s").
function intervalLabel(d: string): string {
  return d.replace(/m0s$/, 'm')
}

// fetchGitHubReady reads the server's folded GitHub-ready signal (PAT | env |
// registered App) + the Jira connection state from the integrations status
// endpoint. Best-effort: a failure leaves both false, and the org GET still
// seeds the URL/PAT-presence fields, so the step degrades to "not connected"
// rather than blocking the load.
async function fetchIntegrationsState(): Promise<{ githubReady: boolean; jiraConnected: boolean }> {
  try {
    const res = await fetch('/api/integrations/status')
    if (!res.ok) return { githubReady: false, jiraConnected: false }
    const data = (await res.json()) as { github_ready?: boolean; jira?: boolean; jira_url?: string }
    return {
      githubReady: !!data.github_ready,
      jiraConnected: !!data.jira && !!data.jira_url,
    }
  } catch {
    return { githubReady: false, jiraConnected: false }
  }
}

// loadOrg seeds the full org form plus the folded GitHub/Jira connection
// signals. It runs ONCE — as the GitHub step's load — and the slice it returns
// is merged into shared wizard state, so every later org step (Trackers,
// poller, model) reads the same org form without re-fetching. Loading the
// whole form (not just one field) keeps each step's persist round-tripping the
// same org_settings shape, so saving the tier never clobbers the base URL /
// intervals / stored PAT. `orgLoaded: true` marks the seed succeeded; if this
// throws, the flag stays false and the persist-bearing steps refuse to write
// the empty default form over real settings.
async function loadOrg(): Promise<Partial<WizardState>> {
  const [org, integrations] = await Promise.all([fetchOrgSettings(), fetchIntegrationsState()])
  if (!org) throw new Error('Could not load organization settings')
  return {
    org: orgConfigFromSettings(org),
    orgLoaded: true,
    hasGitHubPat: org.has_github_pat,
    githubReady: integrations.githubReady,
    jiraConnected: integrations.jiraConnected,
    tracker: integrations.jiraConnected ? 'jira' : 'none',
  }
}

// persistOrg is the shared save for the org steps that round-trip the whole
// org form (poller, model). It refuses to write when the single org load
// (the GitHub step's) failed — `state.org` would be the empty default form,
// and saving it would clobber the stored base URL / intervals / cap. The
// GitHub step shows a retry when its load fails, so this guard only trips if
// the user reaches a later org step while that load is still broken.
async function persistOrg(state: WizardState): Promise<void> {
  if (!state.orgLoaded) {
    throw new Error('Settings didn’t load — reopen the GitHub step and retry before saving.')
  }
  const result = await saveOrgConfig(state.org)
  if (!result.ok) throw new Error(result.error)
}

// Step 1 · GitHub (mandatory). The backbone: a two-stage URL → access flow
// (App default / PAT) that the GitHubStep body owns. isComplete is the server's
// github_ready signal — satisfied by a PAT or a registered App — so the
// mandatory step blocks the stack until GitHub is connected by any means.
const githubStep: WizardStep = {
  id: 'org-github',
  section: 'org',
  title: 'GitHub',
  load: loadOrg,
  isComplete: (s) => s.githubReady,
  validate: (s) => (s.githubReady ? null : 'Connect GitHub to continue — it’s required.'),
  // Creds are persisted at the step's own Connect action (PAT) or via App
  // registration; advancing is then a no-op.
  persist: async () => {},
  collapsedSummary: (s) =>
    s.githubReady
      ? `Connected · ${hostOf(s.org.github_url || 'https://github.com')}`
      : 'Not connected',
  render: (ctx) => <GitHubStep {...ctx} />,
}

// Step 2 · Trackers (optional). None / Jira / Linear (Linear "coming soon").
// Jira connects mid-step via JiraAccessGroup, so persist is a no-op; the only
// gate is "don't advance with Jira picked but not connected."
const trackersStep: WizardStep = {
  id: 'org-trackers',
  section: 'org',
  title: 'Trackers',
  // No load of its own — it reads the org/Jira state the GitHub step already
  // seeded onto shared wizard state. Optional, so None / connected-Jira are
  // complete; only a Jira picked-but-not-connected is incomplete (so it shows
  // no false check and can't be skipped past), which validate also blocks.
  isComplete: (s) => !(s.tracker === 'jira' && !s.jiraConnected),
  validate: (s) =>
    s.tracker === 'jira' && !s.jiraConnected ? 'Connect Jira to continue, or choose None.' : null,
  persist: async () => {},
  collapsedSummary: (s) => {
    if (s.tracker === 'jira') return s.jiraConnected ? 'Jira ✓' : 'Jira (not connected)'
    return 'No tracker'
  },
  render: (ctx) => <TrackersStep {...ctx} />,
}

// Step 3 · Poller timings. GitHub cadence always; Jira cadence only when a Jira
// tracker is connected. 30s–5m (the shared group's option set). Always
// satisfiable — defaults exist — so it never blocks the stack.
const pollerStep: WizardStep = {
  id: 'org-poller',
  section: 'org',
  // No load of its own — it reads the org form the GitHub step seeded onto
  // shared state. persistOrg guards against saving when that load failed.
  title: 'Poller timings',
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) =>
    s.jiraConnected
      ? `GitHub ${intervalLabel(s.org.github_poll_interval)} · Jira ${intervalLabel(
          s.org.jira_poll_interval,
        )}`
      : `GitHub every ${intervalLabel(s.org.github_poll_interval)}`,
  render: ({ state, patch }) => (
    <PollerTimingGroup
      value={{
        github_poll_interval: state.org.github_poll_interval,
        jira_poll_interval: state.org.jira_poll_interval,
      }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
      showJira={state.jiraConnected}
    />
  ),
}

// Step 4 · Org max model tier. The shared ModelTierSelector (via ModelGroup) —
// a card row, not a dropdown — that the team-default-model step reuses.
// Optional (no cap is a legitimate end state), so it never blocks the stack.
const orgModelStep: WizardStep = {
  id: 'org-model',
  section: 'org',
  // No load of its own — reads the GitHub step's seeded org form; persistOrg
  // guards against saving when that load failed.
  title: 'Max model tier',
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) =>
    s.org.max_llm_model_tier
      ? `Capped at ${TIER_LABELS[s.org.max_llm_model_tier] ?? s.org.max_llm_model_tier}`
      : 'No model cap',
  render: ({ state, patch }) => (
    <ModelGroup
      value={{ max_llm_model_tier: state.org.max_llm_model_tier }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
    />
  ),
}

// Team settings · delegation defaults. The trivial team-section proof step the
// shell shipped — the team-steps ticket replaces it with repos / GitHub teams /
// Jira projects / team default model. Kept here so the team section divider has
// a step and resume still lands somewhere. isComplete requires a chosen model
// (seeded default "sonnet", so a returning team reads complete).
const teamModelStep: WizardStep = {
  id: 'team-model',
  section: 'team',
  title: 'Delegation defaults',
  load: async ({ teamId }) => {
    const settings = await fetchTeamSettings(teamId)
    if (!settings) throw new Error('Could not load team settings')
    return { team: teamConfigFromSettings(settings) }
  },
  isComplete: (s) => s.team.default_model.trim() !== '',
  validate: (s) => (s.team.default_model.trim() === '' ? 'Choose a delegation model.' : null),
  persist: async ({ state, teamId }) => {
    const result = await saveTeamSettings(teamId, state.team)
    if (!result.ok) throw new Error(result.error)
  },
  collapsedSummary: (s) =>
    `${TIER_LABELS[s.team.default_model] ?? s.team.default_model} · auto-delegate ${
      s.team.auto_delegate_enabled ? 'on' : 'off'
    }`,
  render: ({ state, patch }) => (
    <TeamSettingsGroup
      value={{
        default_model: state.team.default_model,
        auto_delegate_enabled: state.team.auto_delegate_enabled,
      }}
      onChange={(p) => patch({ team: { ...state.team, ...p } })}
    />
  ),
}

// Ordered registry. Section grouping is derived from each step's `section`;
// the host inserts a divider above the first step of each section.
export const WIZARD_STEPS: WizardStep[] = [
  githubStep,
  trackersStep,
  pollerStep,
  orgModelStep,
  teamModelStep,
]
