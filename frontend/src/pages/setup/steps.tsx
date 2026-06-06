// The shell's trivial-but-real proof steps. They exist to exercise the whole
// step contract end to end — load() seeds from the server, isComplete() drives
// resume, persist() round-trips a real settings endpoint, collapsedSummary()
// fills the confirmation bar — using the *simplest* existing shared field
// groups, one per divider section. The real organization steps (GitHub,
// Trackers, poller) and team steps (repos, GitHub teams, Jira projects) drop
// into this same array in later tickets; they compose the heavier shared
// groups but need no new host plumbing.
//
// Reuse rule: these compose ModelGroup / TeamSettingsGroup — no parallel
// field UIs. Persistence rides the existing session-scoped settings endpoints
// via the orgConfig / teamConfig helpers, so there is no new wizard-only
// persistence path to drift.

import ModelGroup from '../settings/ModelGroup'
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
// identical baseline shape.
export const initialWizardState = (): WizardState => ({
  org: emptyOrgConfig(),
  hasGitHubPat: false,
  team: emptyTeamConfig(),
})

const ORG_TIER_LABELS: Record<string, string> = {
  haiku: 'Haiku',
  sonnet: 'Sonnet',
  opus: 'Opus',
}

const TEAM_MODEL_LABELS: Record<string, string> = {
  haiku: 'Haiku',
  sonnet: 'Sonnet',
  opus: 'Opus',
}

// Organization settings · workspace model cap. The simplest org-scoped field
// group: an optional ceiling over every team's model. isComplete is always
// true (no cap is a legitimate end state), so this is the "optional,
// always-satisfiable" archetype — it never blocks the stack. Loading the full
// org form (not just the tier) is deliberate: persist round-trips the same
// org_settings shape, so seeding from the GET keeps base URL / intervals /
// the stored PAT untouched when only the tier changes.
const orgModelStep: WizardStep = {
  id: 'org-model',
  section: 'org',
  title: 'Workspace model cap',
  load: async () => {
    const org = await fetchOrgSettings()
    if (!org) throw new Error('Could not load organization settings')
    return { org: orgConfigFromSettings(org), hasGitHubPat: org.has_github_pat }
  },
  isComplete: () => true,
  persist: async ({ state }) => {
    const result = await saveOrgConfig(state.org)
    if (!result.ok) throw new Error(result.error)
  },
  collapsedSummary: (s) =>
    s.org.max_llm_model_tier
      ? `Capped at ${ORG_TIER_LABELS[s.org.max_llm_model_tier] ?? s.org.max_llm_model_tier}`
      : 'No model cap',
  render: ({ state, patch }) => (
    <ModelGroup
      value={{ max_llm_model_tier: state.org.max_llm_model_tier }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
    />
  ),
}

// Team settings · delegation defaults. The team mirror: the first team's
// default delegation model + auto-delegation toggle, via the shared
// TeamSettingsGroup. isComplete requires a chosen model (the seeded default is
// "sonnet", so a returning team reads complete). persist writes only the
// team-settings slice, leaving tracked repos / GitHub-team mappings — which
// the real team steps own — untouched.
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
    `${TEAM_MODEL_LABELS[s.team.default_model] ?? s.team.default_model} · auto-delegate ${
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
export const WIZARD_STEPS: WizardStep[] = [orgModelStep, teamModelStep]
