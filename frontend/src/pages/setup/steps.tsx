// The wizard's step registry — all eight real steps. The four ORGANIZATION
// steps (GitHub mandatory, Trackers optional, Poller timings, org max model
// tier) and the four TEAM steps for the first team (Repositories, GitHub teams,
// Jira projects, team default model), composing the existing shared field
// groups into the same step contract with no new host plumbing.
//
// The team steps are ordered repos-first on purpose: persisting the tracked
// repos is what makes the GitHub-teams candidate list enumerable (candidates
// are built from the org's persisted repo-owners), the structural fix for the
// always-empty-teams bug. The Jira-projects step is gated via its `visible`
// predicate — omitted entirely unless a Jira tracker was configured in step 2.
//
// Reuse rule: every step composes a shared group (GitHubAccessGroup /
// GitHubAppPanel via GitHubStep, JiraAccessGroup via TrackersStep,
// PollerTimingGroup, ModelGroup, the RepoPickerModal, GitHubTeamGroup,
// JiraProjectRulesGroup, and the shared ModelTierSelector) — no parallel field
// UIs. Org persistence rides the single session-scoped POST /api/settings/org;
// team persistence rides the existing per-team repos / github-groups PUTs and
// the team-settings POST — no wizard-only persistence path to drift. Steps
// whose creds are connected mid-step (GitHub, Jira) persist at their own
// Connect action; the framework's per-step persist is then a no-op advance.

import GitHubStep from './GitHubStep'
import TrackersStep from './TrackersStep'
import { hostOf } from '../../lib/reachability'
import { toast } from '../../components/Toast/toastStore'
import RepoPickerModal from '../../components/RepoPickerModal'
import ModelGroup from '../settings/ModelGroup'
import ModelTierSelector, { type ModelTierOption } from '../settings/ModelTierSelector'
import PollerTimingGroup from '../settings/PollerTimingGroup'
import GitHubTeamGroup from '../settings/GitHubTeamGroup'
import JiraProjectRulesGroup from '../settings/JiraProjectRulesGroup'
import {
  emptyOrgConfig,
  fetchOrgSettings,
  orgConfigFromSettings,
  saveOrgConfig,
} from '../settings/orgConfig'
import {
  emptyTeamConfig,
  fetchTeamRepos,
  fetchTeamSettings,
  saveTeamGitHubGroups,
  saveTeamRepos,
  saveTeamSettings,
  teamConfigFromSettings,
  teamProjectsBlocked,
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
  teamLoaded: false,
})

const TIER_LABELS: Record<string, string> = {
  haiku: 'Haiku',
  sonnet: 'Sonnet',
  opus: 'Opus',
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

// loadTeam seeds the team form (default model, Jira project rules, thresholds)
// plus the tracked-repo set. It runs ONCE — as the Repositories step's load,
// the first team step — and the slice merges into shared wizard state, so the
// later team steps (GitHub teams, Jira projects, default model) read the same
// form without re-fetching. GitHub-team mappings load separately, via
  // succeeded; if the team-settings GET throws, the flag stays false and the
  // persist-bearing team steps refuse to write the empty default form over real
  // settings. A failed repos GET (null) keeps repos undefined (unloaded) rather
  // than [], so callers can distinguish "unloaded" from "loaded but empty" and
  // avoid treating a fetch failure as "tracks nothing".
async function loadTeam(teamId: string): Promise<Partial<WizardState>> {
  const [settings, repos] = await Promise.all([fetchTeamSettings(teamId), fetchTeamRepos(teamId)])
  if (!settings) throw new Error('Could not load team settings')
  return {
    team: { ...teamConfigFromSettings(settings), repos: repos ?? undefined },
    teamLoaded: true,
  }
}

// persistTeamSettings is the shared save for the team steps that round-trip the
// whole team form via POST /api/settings/team/{id} (Jira projects, default
// model). It refuses to write when the team load (the Repositories step's)
// failed — `state.team` would be the empty default form, and saving it would
// clobber the stored model / project rules. Returns the backend's model-cap
// clamp notice (if any) so the caller can surface it. The repos and
// GitHub-team mappings ride their own PUT endpoints, persisted by their own
// steps; this writes only the team-settings payload.
async function persistTeamSettings(
  teamId: string,
  state: WizardState,
): Promise<string | undefined> {
  if (!state.teamLoaded) {
    throw new Error(
      'Team settings didn’t load — reopen the Repositories step and retry before saving.',
    )
  }
  const result = await saveTeamSettings(teamId, state.team)
  if (!result.ok) throw new Error(result.error)
  return result.warning
}

// The team default-model options: the three concrete tiers (no "no cap" — a
// team always delegates with *some* model). Rendered by the same shared
// ModelTierSelector the org max-tier step uses, so the two read identically.
const TEAM_MODEL_OPTIONS: ModelTierOption[] = [
  { value: 'haiku', label: 'Haiku', hint: 'Fastest, cheapest' },
  { value: 'sonnet', label: 'Sonnet', hint: 'Balanced' },
  { value: 'opus', label: 'Opus', hint: 'Most capable' },
]

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
  validate: (s) => (s.githubReady ? null : 'GitHub is required — connect it to continue.'),
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

// Step 3 · Poller timings. GitHub cadence always; the Jira cadence only when
// Jira is the chosen tracker AND connected — a user who connects Jira then
// switches the tracker back to None shouldn't see (or save) a Jira interval,
// even though the connection itself persists until explicitly disconnected.
// 30s–5m (the shared group's option set). Always satisfiable — defaults
// exist — so it never blocks the stack.
const jiraActive = (s: WizardState) => s.tracker === 'jira' && s.jiraConnected

const pollerStep: WizardStep = {
  id: 'org-poller',
  section: 'org',
  // No load of its own — it reads the org form the GitHub step seeded onto
  // shared state. persistOrg guards against saving when that load failed.
  title: 'Poller timings',
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) =>
    jiraActive(s)
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
      showJira={jiraActive(state)}
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

// Step 5 · Repositories (first team step). Embeds the shared RepoPickerModal
// inline (footer hidden — the wizard owns Continue/Back) and runs the single
// team load. Persisting the repo set writes repo_profiles, which is BOTH half
// of the backend's setup-complete gate (GitHub + ≥1 repo) AND what makes the
// next step's GitHub-team candidates enumerable — the structural fix for the
// always-empty-teams bug: candidates are built from persisted repo-owners, so
// repos must land before the teams step reads them. Mandatory: a team that
// watches nothing surfaces nothing.
const reposStep: WizardStep = {
  id: 'team-repos',
  section: 'team',
  title: 'Repositories',
  load: ({ teamId }) => loadTeam(teamId),
  isComplete: (s) => (s.team.repos ?? []).length > 0,
  validate: (s) =>
    (s.team.repos ?? []).length === 0
      ? 'Pick at least one repository for this team to watch.'
      : null,
  persist: async ({ state, teamId }) => {
    const result = await saveTeamRepos(teamId, state.team.repos ?? [])
    if (!result.ok) throw new Error(result.error)
  },
  collapsedSummary: (s) => `Tracked repos: ${(s.team.repos ?? []).length}`,
  render: ({ state, patch }) => {
    const count = (state.team.repos ?? []).length
    return (
      <div className="space-y-2">
        <RepoPickerModal
          inline
          hideFooter
          selected={state.team.repos ?? []}
          onSelectionChange={(repos) => patch({ team: { ...state.team, repos } })}
          // Footer hidden, so neither fires — the wizard's Continue persists and
          // advances. Required by the prop contract; intentional no-ops.
          onSave={() => {}}
          onClose={() => {}}
        />
        <p className="text-[12px] text-text-tertiary">
          {count} {count === 1 ? 'repository' : 'repositories'} selected.
        </p>
      </div>
    )
  },
}

// Step 6 · GitHub teams. The shared GitHubTeamGroup, now enumerable because the
// repos step persisted repo-owners first (it self-fetches candidates from the
// org's configured repos). Optional — a team can watch repos without mapping
// any GitHub team — so it never blocks the stack. No load of its own: the group
// seeds the selection up via onLoaded after its own fetch.
const githubTeamsStep: WizardStep = {
  id: 'team-github-teams',
  section: 'team',
  title: 'GitHub teams',
  isComplete: () => true,
  persist: async ({ state, teamId }) => {
    // Skip when the mapping never loaded (the group's fetch failed) so a flaky
    // load can't wipe stored mappings with []. A user edit makes the slice
    // defined, so an intentional pick still saves.
    if (state.team.github_groups === undefined) return
    const result = await saveTeamGitHubGroups(teamId, state.team.github_groups)
    if (!result.ok) throw new Error(result.error)
  },
  collapsedSummary: (s) => `Bound GH teams: ${(s.team.github_groups ?? []).length}`,
  render: ({ state, teamId, patch }) => (
    <GitHubTeamGroup
      value={state.team.github_groups ?? []}
      onChange={(github_groups) => patch({ team: { ...state.team, github_groups } })}
      teamId={teamId}
      includeMembership
      onLoaded={(github_groups) => patch({ team: { ...state.team, github_groups } })}
    />
  ),
}

// Step 7 · Jira projects. The shared JiraProjectRulesGroup — the per-project
// pickup/in-progress/done status rules. Gated: omitted entirely unless a Jira
// tracker was configured (step 2). No load of its own — the rules came in with
// the team load. Optional (zero tracked projects is valid); only a half-filled
// project (a key with incomplete rules) blocks, which validate also catches.
const jiraProjectsStep: WizardStep = {
  id: 'team-jira-projects',
  section: 'team',
  title: 'Jira projects',
  visible: (s) => jiraActive(s),
  isComplete: (s) => !teamProjectsBlocked(s.team.jira_projects, s.jiraConnected),
  validate: (s) =>
    teamProjectsBlocked(s.team.jira_projects, s.jiraConnected)
      ? 'Finish or remove the partially-configured Jira project before continuing.'
      : null,
  persist: async ({ state, teamId }) => {
    await persistTeamSettings(teamId, state)
  },
  collapsedSummary: (s) =>
    `Tracked Jira projects: ${s.team.jira_projects.filter((p) => p.key.trim() !== '').length}`,
  render: ({ state, patch }) => (
    <JiraProjectRulesGroup
      value={state.team.jira_projects}
      onChange={(jira_projects) => patch({ team: { ...state.team, jira_projects } })}
      connected={state.jiraConnected}
    />
  ),
}

// Step 8 · Team default model. The same shared ModelTierSelector the org
// max-tier step uses (step 4), team-scoped — the model this team delegates
// with by default, clamped down to the workspace cap if it exceeds it. No load
// of its own — reads the repos step's seeded team form; persistTeamSettings
// guards against saving when that load failed. isComplete requires a chosen
// model (seeded default "sonnet", so a returning team reads complete).
const teamModelStep: WizardStep = {
  id: 'team-model',
  section: 'team',
  title: 'Team default model',
  isComplete: (s) => s.team.default_model.trim() !== '',
  validate: (s) => (s.team.default_model.trim() === '' ? 'Choose a default model.' : null),
  persist: async ({ state, teamId }) => {
    const warning = await persistTeamSettings(teamId, state)
    if (warning) toast.info(warning)
  },
  collapsedSummary: (s) =>
    `Default model: ${TIER_LABELS[s.team.default_model] ?? s.team.default_model}`,
  render: ({ state, patch }) => (
    <div className="space-y-3">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        The model this team delegates with by default. Capped by the workspace max tier — a higher
        pick is clamped down to it.
      </p>
      <ModelTierSelector
        value={state.team.default_model}
        onChange={(default_model) => patch({ team: { ...state.team, default_model } })}
        options={TEAM_MODEL_OPTIONS}
        ariaLabel="Team default model"
      />
    </div>
  ),
}

// Ordered registry. Section grouping is derived from each step's `section`;
// the host inserts a divider above the first step of each section. The
// Jira-projects step is conditionally omitted via its `visible` predicate.
export const WIZARD_STEPS: WizardStep[] = [
  githubStep,
  trackersStep,
  pollerStep,
  orgModelStep,
  reposStep,
  githubTeamsStep,
  jiraProjectsStep,
  teamModelStep,
]
