// Shared types + persistence helpers for the team-level configuration
// surface (tracked repos, GitHub-team mappings, Jira project rules, team
// settings). The team mirror of orgConfig.ts: the same helpers back the
// Settings team tab, the create-time TeamConfigure step, and the local
// Setup wizard, so no surface grows its own parallel persistence path.
//
// Key difference from org: team config spans MULTIPLE endpoints, not the
// single POST /api/settings/org. The team-settings + Jira rules ride one
// POST; tracked repos and GitHub-team mappings each have their own PUT.
// saveTeamConfig sequences all three and surfaces partial failure (so the
// user never silently lands on a half-saved team), while the per-slice
// savers let a surface that only touched one slice (e.g. Settings'
// change-aware save) write just that one.

import { readError } from '../../lib/api'
import type { JiraStatusRuleValue } from '../../components/JiraStatusRule'
import type { GitHubTeamCandidate } from '../../lib/githubTeams'

// JiraProjectConfig mirrors the backend per-project rule wire shape (key +
// the three status rules). One project's tracking config; a team can carry
// several with independent workflows.
export interface JiraProjectConfig {
  key: string
  pickup: JiraStatusRuleValue
  in_progress: JiraStatusRuleValue
  done: JiraStatusRuleValue
}

// GitHubGroup is one stored GitHub-team → TF-team mapping row, the PUT
// /github-groups wire shape.
export interface GitHubGroup {
  org_login: string
  team_slug: string
}

// TeamConfigForm is the editable team-level field set the shared groups
// drive. A container spreads each group's patch straight into this state.
// Field names follow the GET response (default_model / auto_delegate_enabled)
// rather than the POST wire keys (ai_model / ai_auto_delegate_enabled); the
// savers below do that one mapping, exactly as orgConfig maps github_url →
// github_base_url.
export interface TeamConfigForm {
  default_model: string
  auto_delegate_enabled: boolean
  // Carried for round-trip fidelity (they're part of the team_settings the
  // GET returns and the POST accepts) even though no surface exposes an
  // input for them yet — seeded from the GET and written back unchanged, so
  // a save can't silently reset them. A future ticket adds the controls.
  ai_reprioritize_threshold: number
  ai_preference_update_interval: number
  jira_projects: JiraProjectConfig[]
  repos: string[]
  github_groups: GitHubGroup[]
}

// TeamSettingsData mirrors the GET /api/settings/team/{id} response.
// MemberCount + Role describe the caller's relationship to the team so the
// frontend can collapse to the flat N=1 layout and gate write-side fields
// without a second round trip.
export interface TeamSettingsData {
  team_settings: {
    JiraProjects: string[]
    AIReprioritizeThreshold: number
    AIPreferenceUpdateInterval: number
    DefaultModel: string
    AutoDelegateEnabled: boolean
  }
  jira_projects: JiraProjectConfig[]
  member_count: number
  role: string
}

// TeamReposData mirrors GET /api/settings/team/{id}/repos.
export interface TeamReposData {
  repos: string[]
  role: string
}

// TeamGitHubGroupsData mirrors GET /api/settings/team/{id}/github-groups —
// the saved mappings plus the live org-wide candidate list the checklist
// renders.
export interface TeamGitHubGroupsData {
  groups: GitHubGroup[]
  candidates: GitHubTeamCandidate[]
  role: string
}

export const emptyProject = (key = ''): JiraProjectConfig => ({
  key,
  pickup: { members: [] },
  in_progress: { members: [] },
  done: { members: [] },
})

// projectIsComplete is the per-project validity gate: a tracked project
// needs a non-empty key and fully-populated pickup/in-progress/done rules
// (in-progress + done also need a canonical write target). Mirrors the
// backend's validateProjectRules so the Save button blocks before a 400.
export const projectIsComplete = (p: JiraProjectConfig): boolean =>
  p.key.trim() !== '' &&
  p.pickup.members.length > 0 &&
  p.in_progress.members.length > 0 &&
  !!p.in_progress.canonical &&
  p.done.members.length > 0 &&
  !!p.done.canonical

// teamProjectsBlocked reports whether the team's Jira project rules should
// block a save: only when Jira is connected, and then only if there's no
// valid project or any keyed project is incomplete. Disconnected Jira can't
// have project rules, so it never blocks.
export function teamProjectsBlocked(projects: JiraProjectConfig[], connected: boolean): boolean {
  if (!connected) return false
  const keyed = projects.filter((p) => p.key.trim() !== '')
  const anyValid = keyed.some(projectIsComplete)
  const anyIncomplete = keyed.some((p) => !projectIsComplete(p))
  return !anyValid || anyIncomplete
}

export const emptyTeamConfig = (): TeamConfigForm => ({
  default_model: 'sonnet',
  auto_delegate_enabled: true,
  ai_reprioritize_threshold: 0,
  ai_preference_update_interval: 0,
  jira_projects: [],
  repos: [],
  github_groups: [],
})

// teamConfigFromSettings seeds the team-settings + Jira-rules slice of the
// form from a team GET. Repos and GitHub-team mappings come from their own
// endpoints, so the container patches those in separately (they default to
// empty here).
export function teamConfigFromSettings(data: TeamSettingsData): TeamConfigForm {
  return {
    default_model: data.team_settings.DefaultModel || 'sonnet',
    auto_delegate_enabled: data.team_settings.AutoDelegateEnabled,
    ai_reprioritize_threshold: data.team_settings.AIReprioritizeThreshold,
    ai_preference_update_interval: data.team_settings.AIPreferenceUpdateInterval,
    jira_projects: data.jira_projects ?? [],
    repos: [],
    github_groups: [],
  }
}

const teamPath = (teamId: string) => `/api/settings/team/${encodeURIComponent(teamId)}`

export async function fetchTeamSettings(teamId: string): Promise<TeamSettingsData | null> {
  const res = await fetch(teamPath(teamId))
  return res.ok ? ((await res.json()) as TeamSettingsData) : null
}

// fetchTeamRepos returns the team's tracked-repo slugs, or null on failure.
// The null (vs. empty []) distinction matters: a caller that seeds a picker
// from this must not treat a failed load as "tracks nothing" and then write
// [] back, wiping the team's repos (the Repos page guards the same way).
export async function fetchTeamRepos(teamId: string): Promise<string[] | null> {
  const res = await fetch(`${teamPath(teamId)}/repos`)
  if (!res.ok) return null
  const data = (await res.json()) as TeamReposData
  return data.repos ?? []
}

export type SaveResult = { ok: true; warning?: string } | { ok: false; error: string }

// saveTeamSettings persists the team-settings + Jira-rules slice via POST
// /api/settings/team/{id}. Empty-keyed projects are dropped (a blank row the
// user added but never filled). `warning` carries the backend's model-cap
// clamp notice on an otherwise-successful save.
export async function saveTeamSettings(teamId: string, form: TeamConfigForm): Promise<SaveResult> {
  const projects = form.jira_projects
    .map((p) => ({ ...p, key: p.key.trim() }))
    .filter((p) => p.key !== '')
  const res = await fetch(teamPath(teamId), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      ai_model: form.default_model,
      ai_auto_delegate_enabled: form.auto_delegate_enabled,
      ai_reprioritize_threshold: form.ai_reprioritize_threshold,
      ai_preference_update_interval: form.ai_preference_update_interval,
      jira_projects: projects,
    }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save team settings') }
  }
  const body = (await res.json().catch(() => null)) as { warning?: string } | null
  return { ok: true, warning: body?.warning }
}

// saveTeamJiraProjects persists ONLY the Jira project rules via a sparse
// POST /api/settings/team/{id} (the handler treats omitted ai_* fields as
// unchanged). The Setup wizard's per-step save uses this so configuring Jira
// projects doesn't also rewrite the team's model / auto-delegate defaults the
// step never touched.
export async function saveTeamJiraProjects(
  teamId: string,
  projects: JiraProjectConfig[],
): Promise<SaveResult> {
  const normalized = projects.map((p) => ({ ...p, key: p.key.trim() })).filter((p) => p.key !== '')
  const res = await fetch(teamPath(teamId), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ jira_projects: normalized }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save Jira config') }
  }
  return { ok: true }
}

// saveTeamRepos persists the tracked-repo set via PUT /api/settings/team/
// {id}/repos. Re-PUTting the same set re-triggers profiling, so callers that
// save unconditionally (vs. only-on-change) should be aware.
export async function saveTeamRepos(teamId: string, repos: string[]): Promise<SaveResult> {
  const res = await fetch(`${teamPath(teamId)}/repos`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ repos }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save repositories') }
  }
  return { ok: true }
}

// saveTeamGitHubGroups persists the GitHub-team → TF-team mappings via PUT
// /api/settings/team/{id}/github-groups (a full replace-set).
export async function saveTeamGitHubGroups(
  teamId: string,
  groups: GitHubGroup[],
): Promise<SaveResult> {
  const res = await fetch(`${teamPath(teamId)}/github-groups`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ groups }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save GitHub teams') }
  }
  return { ok: true }
}

// saveTeamConfig orchestrates the full team save across all three endpoints,
// the create-step's "Finish" path. Order is deliberate: team settings first
// (pure server-side validation — project rules, dup keys — with no external
// calls, so a bad rule fails before the repos PUT does any reachability work
// or profiling), then repos, then GitHub-team mappings.
//
// On a mid-sequence failure the earlier writes are already committed, so the
// error names exactly what landed and what didn't rather than a bare
// "failed" that leaves the user guessing which half to redo.
export async function saveTeamConfig(teamId: string, form: TeamConfigForm): Promise<SaveResult> {
  const settings = await saveTeamSettings(teamId, form)
  if (!settings.ok) return settings

  const repos = await saveTeamRepos(teamId, form.repos)
  if (!repos.ok) {
    return {
      ok: false,
      error: `Team settings saved, but tracked repositories did not — ${repos.error}`,
    }
  }

  const groups = await saveTeamGitHubGroups(teamId, form.github_groups)
  if (!groups.ok) {
    return {
      ok: false,
      error: `Team settings and repositories saved, but GitHub team mappings did not — ${groups.error}`,
    }
  }

  return { ok: true, warning: settings.warning }
}
