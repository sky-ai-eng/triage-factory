// Shared types + persistence helpers for the team-level configuration
// surface (tracked repos, GitHub-team mappings, Jira project rules, team
// settings). The team mirror of orgConfig.ts: the same helpers back the
// Settings team tab and the create-time setup wizard (both modes), so
// no surface grows its own parallel persistence path.
//
// Key difference from org: team config spans MULTIPLE endpoints, not the
// single org-settings PATCH. The team settings row has its own PATCH; the
// Jira project rules, the tracked repos and the GitHub-team mappings are each
// a child collection with its own replace-set PUT. saveTeamConfig sequences
// all four and surfaces partial failure (so the user never silently lands on a
// half-saved team), while the per-slice savers let a surface that only touched
// one slice (e.g. Settings' change-aware save) write just that one.

import { apiFetch, apiJSON, httpErrorMessage } from '../../lib/apiClient'
import type { JiraStatusRef, JiraStatusRuleValue } from '../../components/JiraStatusRule'
import type { GitHubTeamCandidate } from '../../lib/githubTeams'
import type { SlackChannelsResponse } from '../../types'

// JiraProjectConfig mirrors the backend per-project rule wire shape (key +
// the four status rules). One project's tracking config; a team can carry
// several with independent workflows.
export interface JiraProjectConfig {
  key: string
  pickup: JiraStatusRuleValue
  in_progress: JiraStatusRuleValue
  // Optional: the status naming work that awaits human review. No Jira status
  // is ever read back into TF's in-review board column, so this is a write
  // target only and an empty rule is a complete configuration.
  in_review: JiraStatusRuleValue
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
  // Claude Code SDK permission posture. Exposed only on the local Settings
  // page; multi mode is always auto and native conversations ignore it.
  auto_mode_enabled: boolean
  // Branch-name template suggested (not enforced) to delegated agents when they
  // create a branch (TFAC-498). The `<ticket-id>` literal is replaced with the
  // ticket id at run time. Same key on the GET and POST wire.
  branch_template: string
  // How this team's delegated reviews reach GitHub: 'identity' |
  // 'draft' | 'auto' | 'auto_unless_blocking'. GET returns it as ReviewPosture;
  // the POST key is review_posture.
  review_posture: string
  // Whether delegated agents may push to a repo's base/default branch:
  // 'never' | 'manual_only' | 'always'. GET returns it as
  // BaseBranchPushPolicy; the POST key is base_branch_push_policy.
  base_branch_push_policy: string
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

// TeamSettingsData mirrors the GET /api/teams/{id}/settings response.
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
    AutoModeEnabled: boolean
    BranchTemplate: string
    ReviewPosture: string
    BaseBranchPushPolicy: string
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

// TeamReposData mirrors GET /api/teams/{id}/github-repos.
export interface TeamReposData {
  repos: string[]
  role: string
}

// TeamGitHubGroupsData mirrors GET /api/teams/{id}/github-groups —
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
  in_review: { members: [] },
  done: { members: [] },
})

/** ruleIsIdentified reports whether every status in a rule carries an id.
 *
 *  Only an identified rule can be WRITTEN: the PUT takes ids, so a rule stored
 *  before statuses were identified — names, no ids — has nothing to send. Such
 *  a rule is left out of the body instead, and the server keeps what it has.
 *  It becomes identified the first time someone edits it, because editing means
 *  picking from a freshly fetched status list. */
export const ruleIsIdentified = (r: JiraStatusRuleValue): boolean =>
  r.members.every((m) => !!m.id) && (!r.canonical || !!r.canonical.id)

// projectIsArmed reports whether TF can actually poll this project: a
// non-empty key and fully-populated pickup/in-progress/done rules (in-progress
// and done also need a canonical write target). Mirrors the backend's
// JiraProjectStatusRules.Armed.
//
// Armed is NOT the same question as valid. A project is WATCHED the moment it
// is in the tracked set, and mapping its workflow's statuses is the step after
// that — so an unarmed project saves fine and simply contributes nothing to
// the discovery JQL until it is mapped.
//
// in_review is deliberately not part of armed: it is optional, and it gates
// nothing the poller asks.
export const projectIsArmed = (p: JiraProjectConfig): boolean =>
  p.key.trim() !== '' &&
  p.pickup.members.length > 0 &&
  p.in_progress.members.length > 0 &&
  !!p.in_progress.canonical &&
  p.done.members.length > 0 &&
  !!p.done.canonical

// projectRulesValid mirrors the backend's validateProjectRules: each rule is
// independently complete-or-empty, and in-progress/done bind their members and
// their canonical together — the canonical is the status TF transitions a
// ticket INTO, so members with no write target is a rule that cannot be
// executed, and a write target that isn't one of them is a rule that would
// fail at transition time. This is what decides whether a save can go through.
export const projectRulesValid = (p: JiraProjectConfig): boolean =>
  [p.in_progress, p.in_review, p.done].every(
    (r) =>
      r.members.length > 0 === !!r.canonical &&
      (!r.canonical ||
        r.members.some((m) =>
          m.id && r.canonical?.id ? m.id === r.canonical.id : m.name === r.canonical?.name,
        )),
  )

/** sameStatus compares two refs the way the server does: ids decide when both
 *  carry one, names otherwise. */
const sameStatus = (a: JiraStatusRef, b: JiraStatusRef): boolean =>
  a.id && b.id ? a.id === b.id : !!a.name && a.name === b.name

/** unresolvableStatuses returns the statuses this project's rules name that
 *  its live workflow no longer has — a status deleted or retired in Jira since
 *  the rule was armed.
 *
 *  Nothing repairs these automatically. A stored rule is the team's, and a
 *  status vanishing upstream is a decision someone made in Jira that TF has no
 *  standing to reinterpret: dropping the member silently would change what
 *  work the team sees, and re-pointing it at a same-named status would rewrite
 *  their configuration on a string match. So they are reported here and
 *  removed by hand.
 *
 *  Returns [] when the workflow has not been fetched, since "no statuses
 *  loaded" is not evidence that any of them are gone. */
export function unresolvableStatuses(
  p: JiraProjectConfig,
  allStatuses: JiraStatusRef[],
): JiraStatusRef[] {
  if (allStatuses.length === 0) return []
  const out: JiraStatusRef[] = []
  const seen = new Set<string>()
  for (const rule of [p.pickup, p.in_progress, p.in_review, p.done]) {
    for (const ref of [...rule.members, ...(rule.canonical ? [rule.canonical] : [])]) {
      const dedup = ref.id || ref.name
      if (!dedup || seen.has(dedup)) continue
      if (!allStatuses.some((s) => sameStatus(s, ref))) {
        seen.add(dedup)
        out.push(ref)
      }
    }
  }
  return out
}

/** dropStatus removes one status from every rule of a project, clearing a
 *  canonical that pointed at it. Clearing the canonical leaves the rule
 *  incomplete on purpose — a write target is a decision, and picking a
 *  replacement is the team's, not a default this can guess. */
export function dropStatus(p: JiraProjectConfig, status: JiraStatusRef): JiraProjectConfig {
  const strip = (r: JiraStatusRuleValue): JiraStatusRuleValue => ({
    members: r.members.filter((m) => !sameStatus(m, status)),
    canonical: r.canonical && sameStatus(r.canonical, status) ? null : r.canonical,
  })
  return {
    ...p,
    pickup: strip(p.pickup),
    in_progress: strip(p.in_progress),
    in_review: strip(p.in_review),
    done: strip(p.done),
  }
}

// teamProjectsBlocked reports whether the team's Jira project rules should
// block a save. Zero tracked projects is a valid choice — a Jira-connected
// org can still have a team that tracks no Jira project — and so is a watched
// project nobody has mapped yet, so neither blocks. Only a rule the server
// would reject does: members with no write target, or a write target that is
// not one of them. Disconnected Jira never blocks (no rules).
export function teamProjectsBlocked(projects: JiraProjectConfig[], connected: boolean): boolean {
  if (!connected) return false
  return projects.some((p) => p.key.trim() !== '' && !projectRulesValid(p))
}

// emptyTeamConfig leaves repos/github_groups undefined (unloaded) — a save
// from this state writes neither, rather than wiping them with [].
//
// default_model is blank rather than a guessed model: the accepted set is the
// org's catalog, which this module cannot see, and a name invented here would
// be one the save rejects. Every real form is seeded from the team GET, whose
// settings row always carries one.
export const emptyTeamConfig = (): TeamConfigForm => ({
  default_model: '',
  auto_delegate_enabled: true,
  auto_mode_enabled: true,
  branch_template: 'tfac/<ticket-id>',
  review_posture: 'identity',
  base_branch_push_policy: 'never',
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
    default_model: data.team_settings.DefaultModel,
    auto_delegate_enabled: data.team_settings.AutoDelegateEnabled,
    auto_mode_enabled: data.team_settings.AutoModeEnabled ?? true,
    branch_template: data.team_settings.BranchTemplate || 'tfac/<ticket-id>',
    review_posture: data.team_settings.ReviewPosture || 'identity',
    base_branch_push_policy: data.team_settings.BaseBranchPushPolicy || 'never',
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

// The team resource. Its settings row and its three child collections — the
// Jira project rules, the tracked GitHub repos, the GitHub-team mappings — all
// hang off it.
const teamPath = (teamId: string) => `/api/teams/${encodeURIComponent(teamId)}`

export async function fetchTeamSettings(teamId: string): Promise<TeamSettingsData | null> {
  return apiJSON<TeamSettingsData>(`${teamPath(teamId)}/settings`).catch(() => null)
}

// fetchTeamRepos returns the team's tracked-repo slugs, or null on failure.
// The null (vs. empty []) distinction matters: a caller that seeds a picker
// from this must not treat a failed load as "tracks nothing" and then write
// [] back, wiping the team's repos (the Repos page guards the same way).
export async function fetchTeamRepos(teamId: string): Promise<string[] | null> {
  return apiJSON<TeamReposData>(`${teamPath(teamId)}/github-repos`)
    .then((data) => data.repos ?? [])
    .catch(() => null)
}

// fetchTeamGitHubGroups returns just the team's saved GitHub-team mappings (no
// candidate list), or null on failure — for a container that needs the saved
// set up front (e.g. a collapsed section's count + change-detection baseline)
// without mounting the full GitHubTeamGroup checklist. Same null-vs-[] contract
// as fetchTeamRepos: a failed load must not read as "maps nothing." (The GET
// also re-triggers the server's deletion reconcile, same as the group's fetch.)
export async function fetchTeamGitHubGroups(teamId: string): Promise<GitHubGroup[] | null> {
  return apiJSON<TeamGitHubGroupsData>(`${teamPath(teamId)}/github-groups`)
    .then((data) => data.groups ?? [])
    .catch(() => null)
}

export type SaveResult = { ok: true; warning?: string } | { ok: false; error: string }

// JiraProjectsSaveResult carries the stored set back, because the PUT resolves
// status display names server-side — so what came back is not necessarily what
// was sent, and the caller should render the former.
export type JiraProjectsSaveResult =
  | { ok: true; projects: JiraProjectConfig[] }
  | { ok: false; error: string }

// saveTeamSettings persists the team settings row via
// PATCH /api/teams/{id}/settings. `warning` carries the backend's model-cap
// clamp notice on an otherwise-successful save.
//
// Every field is sent because every field is edited somewhere on this form and
// the caller passes the whole live form; a surface that edits one field can
// still send only that key, since absent means keep. The Jira project rules are
// NOT part of this body — they have their own replace-set write below.
export async function saveTeamSettings(
  teamId: string,
  form: TeamConfigForm,
  isLocal: boolean,
): Promise<SaveResult> {
  try {
    const body = await apiJSON<{ warning?: string } | null>(`${teamPath(teamId)}/settings`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ai_model: form.default_model,
        ai_auto_delegate_enabled: form.auto_delegate_enabled,
        // Multi mode always uses auto and must not grow a hidden
        // setup/settings write for this local-only field.
        ...(isLocal ? { auto_mode_enabled: form.auto_mode_enabled } : {}),
        branch_template: form.branch_template,
        review_posture: form.review_posture,
        base_branch_push_policy: form.base_branch_push_policy,
        ai_reprioritize_threshold: form.ai_reprioritize_threshold,
        ai_preference_update_interval: form.ai_preference_update_interval,
        permission_absent_autodeny_enabled: form.permission_absent_autodeny_enabled,
        permission_absent_grace_seconds: form.permission_absent_grace_seconds,
      }),
    })
    return { ok: true, warning: body?.warning }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the team settings.') }
  }
}

// jiraRuleWrite renders one rule for the PUT, which takes status IDS and
// resolves the display names itself. It returns undefined for a rule that
// cannot be expressed in ids — one stored before statuses were identified — and
// the caller omits the key entirely, which is how the server is told to keep
// what it already has rather than to clear it.
function jiraRuleWrite(
  r: JiraStatusRuleValue,
  withCanonical: boolean,
): Record<string, unknown> | undefined {
  if (!ruleIsIdentified(r)) return undefined
  const member_ids = r.members.map((m: JiraStatusRef) => m.id)
  return withCanonical ? { member_ids, canonical_id: r.canonical?.id ?? '' } : { member_ids }
}

// saveTeamJiraProjects persists the tracked Jira projects + their status rules
// via PUT /api/teams/{id}/jira-projects — a full replace-set, like the repos
// and github-groups siblings. Empty-keyed projects are dropped first (a blank
// row the user added but never filled).
export async function saveTeamJiraProjects(
  teamId: string,
  projects: JiraProjectConfig[],
): Promise<JiraProjectsSaveResult> {
  const jira_projects = projects
    .map((p) => ({ ...p, key: p.key.trim() }))
    .filter((p) => p.key !== '')
    .map((p) => {
      // A rule that cannot be expressed in ids is OMITTED, never sent empty:
      // absent means "keep the stored one" and empty means "clear it", and only
      // the first is right for a rule this client never touched.
      const body: Record<string, unknown> = { key: p.key }
      const pickup = jiraRuleWrite(p.pickup, false)
      if (pickup) body.pickup = pickup
      const inProgress = jiraRuleWrite(p.in_progress, true)
      if (inProgress) body.in_progress = inProgress
      const inReview = jiraRuleWrite(p.in_review, true)
      if (inReview) body.in_review = inReview
      const done = jiraRuleWrite(p.done, true)
      if (done) body.done = done
      return body
    })
  try {
    // The response is the set AS STORED, and adopting it is not a nicety: the
    // server resolves every status display name from Jira on the way in, so
    // this is where a name renamed upstream since the last save refreshes.
    const body = await apiJSON<{ jira_projects?: JiraProjectConfig[] }>(
      `${teamPath(teamId)}/jira-projects`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ jira_projects }),
      },
    )
    return { ok: true, projects: body.jira_projects ?? [] }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the Jira projects.') }
  }
}

// saveTeamRepos persists the tracked-repo set via
// PUT /api/teams/{id}/github-repos. Re-PUTting the same set re-triggers
// profiling, so callers that save unconditionally (vs. only-on-change) should
// be aware.
export async function saveTeamRepos(teamId: string, repos: string[]): Promise<SaveResult> {
  try {
    await apiFetch(`${teamPath(teamId)}/github-repos`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repos }),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the repositories.') }
  }
}

// saveTeamGitHubGroups persists the GitHub-team → TF-team mappings via PUT
// /api/teams/{id}/github-groups (a full replace-set).
export async function saveTeamGitHubGroups(
  teamId: string,
  groups: GitHubGroup[],
): Promise<SaveResult> {
  try {
    await apiFetch(`${teamPath(teamId)}/github-groups`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ groups }),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the GitHub teams.') }
  }
}

// slackChannelsPath is the ee/slack channel-tracking route — deliberately
// under /api/slack/* rather than teamPath()'s /api/teams/... (see
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
  return apiJSON<SlackChannelsResponse>(slackChannelsPath(teamId)).catch(() => null)
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
  try {
    const data = await apiJSON<SlackChannelsResponse>(slackChannelsPath(teamId), {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel_ids: channelIds }),
    })
    return { ok: true, data }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the Slack channels.') }
  }
}

// reassignSlackChannelPrimary makes teamId the primary tracker of channelId
// (org-admin only; the server 400s if teamId isn't already tracking it).
export async function reassignSlackChannelPrimary(
  channelId: string,
  teamId: string,
): Promise<SaveResult> {
  try {
    await apiFetch(`/api/slack/channels/${encodeURIComponent(channelId)}/primary`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ team_id: teamId }),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not reassign the primary team.') }
  }
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
// create-step's "Finish" path. Order is deliberate: team settings and the Jira
// rules first (pure server-side validation — project rules, dup keys — with no
// external calls, so a bad rule fails before the repos PUT does any
// reachability work or profiling), then repos, then GitHub-team mappings.
//
// A slice left `undefined` (never loaded / load failed) is skipped, not
// written — that's the no-wipe guarantee. On a mid-sequence failure the
// earlier writes are already committed (separate endpoints, no spanning
// transaction), so the error names exactly which slices landed.
export async function saveTeamConfig(
  teamId: string,
  form: TeamConfigForm,
  isLocal: boolean,
): Promise<SaveResult> {
  const saved: string[] = []

  const settings = await saveTeamSettings(teamId, form, isLocal)
  if (!settings.ok) return settings
  saved.push('Team settings')

  const projects = await saveTeamJiraProjects(teamId, form.jira_projects)
  if (!projects.ok) {
    return { ok: false, error: partialFailure(saved, 'Jira projects', projects.error) }
  }
  saved.push('Jira projects')

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
