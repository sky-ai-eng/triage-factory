// Shared types + persistence helpers for the team-level configuration
// surface (tracked repos, GitHub-team mappings, Jira project rules, team
// settings). The team mirror of orgConfig.ts: the same helpers back the
// Settings team tab and the create-time setup wizard (both modes), so
// no surface grows its own parallel persistence path.
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
import type { SlackChannelsResponse } from '../../types'

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
  // Branch-name template suggested (not enforced) to delegated agents when they
  // create a branch (TFAC-498). The `<ticket-id>` literal is replaced with the
  // ticket id at run time. Same key on the GET and POST wire.
  branch_template: string
  // How this team's delegated reviews reach GitHub (TFAC-680): 'identity' |
  // 'draft' | 'auto' | 'auto_unless_blocking'. GET returns it as ReviewPosture;
  // the POST key is review_posture.
  review_posture: string
  // Carried for round-trip fidelity (they're part of the team_settings the
  // GET returns and the POST accepts) even though no surface exposes an
  // input for them yet — seeded from the GET and written back unchanged, so
  // a save can't silently reset them. A future ticket adds the controls.
  ai_reprioritize_threshold: number
  ai_preference_update_interval: number
  // Presence-gated absent auto-deny (TFAC-392). The grace is held in SECONDS on
  // the form (the UI input is seconds); teamConfig maps it to/from the stored ms.
  permission_absent_autodeny_enabled: boolean
  permission_absent_grace_seconds: number
  jira_projects: JiraProjectConfig[]
  // repos and github_groups load from their own endpoints (separate from the
  // team-settings GET), so they carry a third state: `undefined` means "not
  // loaded yet / load failed" — distinct from `[]` ("loaded, genuinely
  // empty"). saveTeamConfig skips an `undefined` slice rather than PUTting an
  // empty set over data it never read, so a flaky load can't wipe a team's
  // repos or mappings. This makes the no-wipe invariant hold by construction
  // for every consumer instead of each one re-implementing a load guard.
  repos?: string[]
  github_groups?: GitHubGroup[]
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
    BranchTemplate: string
    ReviewPosture: string
    PermissionAbsentGraceMS: number
    PermissionAbsentAutodenyEnabled: boolean
  }
  jira_projects: JiraProjectConfig[]
  member_count: number
  role: string
  // Honored bounds of the unattended-prompt grace window (whole seconds),
  // surfaced by the backend so the slider's range tracks permTimeout() instead
  // of hardcoding it. Optional for forward-compat with an older server that
  // doesn't emit them — the UI falls back to its own GRACE_* defaults.
  permission_absent_grace_min_seconds?: number
  permission_absent_grace_max_seconds?: number
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
  // True when the team tracks repos but no owner had a resolvable GitHub
  // credential — an empty `candidates` then means "reconnect GitHub", not
  // "these orgs have no teams." Omitted (false) in the healthy case. The
  // GitHub-teams step surfaces it as a reconnect prompt rather than a silent
  // empty checklist.
  credentials_missing?: boolean
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
// block a save. Zero tracked projects is a valid choice — a Jira-connected
// org can still have a team that tracks no Jira project — so it does NOT
// block; only a partially-filled project (a key with incomplete rules) does,
// since that can't be persisted. Disconnected Jira never blocks (no rules).
export function teamProjectsBlocked(projects: JiraProjectConfig[], connected: boolean): boolean {
  if (!connected) return false
  return projects.some((p) => p.key.trim() !== '' && !projectIsComplete(p))
}

// emptyTeamConfig leaves repos/github_groups undefined (unloaded) — a save
// from this state writes neither, rather than wiping them with [].
export const emptyTeamConfig = (): TeamConfigForm => ({
  default_model: 'sonnet',
  auto_delegate_enabled: true,
  branch_template: 'tfac/<ticket-id>',
  review_posture: 'identity',
  ai_reprioritize_threshold: 0,
  ai_preference_update_interval: 0,
  permission_absent_autodeny_enabled: true,
  permission_absent_grace_seconds: 15,
  jira_projects: [],
})

// teamConfigFromSettings seeds the team-settings + Jira-rules slice of the
// form from a team GET. Repos and GitHub-team mappings come from their own
// endpoints, so they stay undefined here (unloaded) and the container patches
// them in once their own fetches land — keeping them undefined until then is
// what lets saveTeamConfig skip a slice that never loaded.
export function teamConfigFromSettings(data: TeamSettingsData): TeamConfigForm {
  return {
    default_model: data.team_settings.DefaultModel || 'sonnet',
    auto_delegate_enabled: data.team_settings.AutoDelegateEnabled,
    branch_template: data.team_settings.BranchTemplate || 'tfac/<ticket-id>',
    review_posture: data.team_settings.ReviewPosture || 'identity',
    ai_reprioritize_threshold: data.team_settings.AIReprioritizeThreshold,
    ai_preference_update_interval: data.team_settings.AIPreferenceUpdateInterval,
    permission_absent_autodeny_enabled: data.team_settings.PermissionAbsentAutodenyEnabled,
    // Stored as ms; the form (and the input) work in whole seconds.
    permission_absent_grace_seconds: Math.max(
      1,
      Math.round((data.team_settings.PermissionAbsentGraceMS ?? 15000) / 1000),
    ),
    jira_projects: data.jira_projects ?? [],
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

// fetchTeamGitHubGroups returns just the team's saved GitHub-team mappings (no
// candidate list), or null on failure — for a container that needs the saved
// set up front (e.g. a collapsed section's count + change-detection baseline)
// without mounting the full GitHubTeamGroup checklist. Same null-vs-[] contract
// as fetchTeamRepos: a failed load must not read as "maps nothing." (The GET
// also re-triggers the server's deletion reconcile, same as the group's fetch.)
export async function fetchTeamGitHubGroups(teamId: string): Promise<GitHubGroup[] | null> {
  const res = await fetch(`${teamPath(teamId)}/github-groups`)
  if (!res.ok) return null
  const data = (await res.json()) as TeamGitHubGroupsData
  return data.groups ?? []
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
      branch_template: form.branch_template,
      review_posture: form.review_posture,
      ai_reprioritize_threshold: form.ai_reprioritize_threshold,
      ai_preference_update_interval: form.ai_preference_update_interval,
      permission_absent_autodeny_enabled: form.permission_absent_autodeny_enabled,
      permission_absent_grace_seconds: form.permission_absent_grace_seconds,
      jira_projects: projects,
    }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save team settings') }
  }
  const body = (await res.json().catch(() => null)) as { warning?: string } | null
  return { ok: true, warning: body?.warning }
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

// slackChannelsPath is the ee/slack channel-tracking route — deliberately
// under /api/slack/* rather than teamPath()'s /api/settings/team/... (see
// ee/slack/channels_handler.go's package doc: ee-owned routes stay in the
// slack namespace).
const slackChannelsPath = (teamId: string) =>
  `/api/slack/teams/${encodeURIComponent(teamId)}/channels`

// fetchTeamSlackChannels returns the merged channel list (tracked + sighting
// registry + live Slack candidates, deduped by channel_id) for teamId, or
// null on failure — 404 when unentitled/local/non-member, or a transient
// error. Same null-vs-empty-array contract as fetchTeamRepos: a caller must
// not read a failed load as "tracks nothing."
export async function fetchTeamSlackChannels(
  teamId: string,
): Promise<SlackChannelsResponse | null> {
  const res = await fetch(slackChannelsPath(teamId))
  return res.ok ? ((await res.json()) as SlackChannelsResponse) : null
}

export type SlackSaveResult =
  | { ok: true; data: SlackChannelsResponse }
  | { ok: false; error: string }

// saveTeamSlackChannels persists the tracked-channel set via PUT (full-set
// replace). The response is the post-change GET shape plus any auto-join
// warnings (invite-required / join-failed), so callers refresh their rows
// straight from `data` rather than re-fetching.
export async function saveTeamSlackChannels(
  teamId: string,
  channelIds: string[],
): Promise<SlackSaveResult> {
  const res = await fetch(slackChannelsPath(teamId), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ channel_ids: channelIds }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save Slack channels') }
  }
  return { ok: true, data: (await res.json()) as SlackChannelsResponse }
}

// reassignSlackChannelPrimary makes teamId the primary tracker of channelId
// (org-admin only; the server 400s if teamId isn't already tracking it).
export async function reassignSlackChannelPrimary(
  channelId: string,
  teamId: string,
): Promise<SaveResult> {
  const res = await fetch(`/api/slack/channels/${encodeURIComponent(channelId)}/primary`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ team_id: teamId }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to reassign the primary team') }
  }
  return { ok: true }
}

// partialFailure builds a "<saved> saved, but <failed> did not — <err>"
// message from the slices that already committed, so the user knows exactly
// what landed and what to retry rather than reading a bare "failed".
function partialFailure(saved: string[], failed: string, err: string): string {
  const phrase =
    saved.length <= 1
      ? saved.join('')
      : `${saved.slice(0, -1).join(', ')} and ${saved[saved.length - 1]}`
  return `${phrase} saved, but ${failed} did not — ${err}`
}

// saveTeamConfig orchestrates the team save across the endpoints, the
// create-step's "Finish" path. Order is deliberate: team settings first
// (pure server-side validation — project rules, dup keys — with no external
// calls, so a bad rule fails before the repos PUT does any reachability work
// or profiling), then repos, then GitHub-team mappings.
//
// A slice left `undefined` (never loaded / load failed) is skipped, not
// written — that's the no-wipe guarantee. On a mid-sequence failure the
// earlier writes are already committed (separate endpoints, no spanning
// transaction), so the error names exactly which slices landed.
export async function saveTeamConfig(teamId: string, form: TeamConfigForm): Promise<SaveResult> {
  const saved: string[] = []

  const settings = await saveTeamSettings(teamId, form)
  if (!settings.ok) return settings
  saved.push('Team settings')

  if (form.repos !== undefined) {
    const repos = await saveTeamRepos(teamId, form.repos)
    if (!repos.ok) {
      return { ok: false, error: partialFailure(saved, 'tracked repositories', repos.error) }
    }
    saved.push('tracked repositories')
  }

  if (form.github_groups !== undefined) {
    const groups = await saveTeamGitHubGroups(teamId, form.github_groups)
    if (!groups.ok) {
      return { ok: false, error: partialFailure(saved, 'GitHub team mappings', groups.error) }
    }
    // Kept consistent with the slices above even though it's last and the
    // success path doesn't read `saved` — a future fourth slice's partial
    // message would otherwise silently omit it.
    saved.push('GitHub team mappings')
  }

  return { ok: true, warning: settings.warning }
}
