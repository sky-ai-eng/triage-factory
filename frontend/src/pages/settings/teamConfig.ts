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
import type { LinearStateOption, LinearStateRef } from '../../lib/linearTeams'
import type { SlackChannelsResponse } from '../../types'

// JiraProjectConfig mirrors the backend per-project rule wire shape (key +
// the four status rules). One project's tracking config; a team can carry
// several with independent workflows.
export interface JiraProjectConfig {
  key: string
  pickup: JiraStatusRuleValue
  in_progress: JiraStatusRuleValue
  // Optional: the status naming work that awaits human review. It has no
  // counterpart on the TF board — "awaiting a human" is a fact about a run,
  // carried on the card frame and the attention order, not a lane — so this is
  // a write target only and an empty rule is a complete configuration.
  in_review: JiraStatusRuleValue
  done: JiraStatusRuleValue
}

// LinearTeamConfig is one tracked Linear team: its id, the key and name Linear
// gave for it at the last save, and its three workflow-state rules. There is
// no in_review rule for Linear.
export interface LinearTeamConfig {
  id: string
  key: string
  name: string
  pickup: { members: LinearStateRef[] }
  in_progress: JiraStatusRuleValue<LinearStateRef>
  done: JiraStatusRuleValue<LinearStateRef>
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
  // The tracked Linear teams in display order, as PUT
  // /api/teams/{id}/linear-teams answers with them. Optional for an older
  // server that does not send it.
  linear_teams?: LinearTeamWire[]
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

// LinearTeamWire is a tracked Linear team as the server renders it. `armed` is
// the server's verdict at the last save; the form recomputes it from the rules
// as they are edited, so linearTeamFromWire leaves it behind.
export interface LinearTeamWire extends LinearTeamConfig {
  armed: boolean
}

/** linearTeamFromWire is the form's copy of a stored team: exactly the fields
 *  the form edits. Whether an edited team still matches the loaded one is
 *  linearTeamsEqual's question, not a structural one: an edit hands back the
 *  live state options, which carry more than a stored ref. */
export const linearTeamFromWire = (t: LinearTeamWire): LinearTeamConfig => ({
  id: t.id,
  key: t.key,
  name: t.name,
  pickup: { members: t.pickup.members },
  in_progress: { members: t.in_progress.members, canonical: t.in_progress.canonical ?? null },
  done: { members: t.done.members, canonical: t.done.canonical ?? null },
})

/** unmappedLinearTeam is a team that has just been watched: nothing mapped. */
export const unmappedLinearTeam = (id: string, key: string, name: string): LinearTeamConfig => ({
  id,
  key,
  name,
  pickup: { members: [] },
  in_progress: { members: [], canonical: null },
  done: { members: [], canonical: null },
})

/** linearTeamIsArmed reports whether every rule is mapped — pickup members,
 *  and members plus a canonical on in_progress and done. Mirrors the backend's
 *  LinearTeamRules.Armed. */
export const linearTeamIsArmed = (t: LinearTeamConfig): boolean =>
  t.pickup.members.length > 0 &&
  t.in_progress.members.length > 0 &&
  !!t.in_progress.canonical &&
  t.done.members.length > 0 &&
  !!t.done.canonical

/** linearTeamIsUnmapped reports whether every rule is empty: watched, and
 *  nothing mapped yet. */
export const linearTeamIsUnmapped = (t: LinearTeamConfig): boolean =>
  t.pickup.members.length === 0 &&
  t.in_progress.members.length === 0 &&
  !t.in_progress.canonical &&
  t.done.members.length === 0 &&
  !t.done.canonical

/** linearTeamRulesValid mirrors what the server stores: a Linear team is
 *  either watched with every rule empty or mapped with all three, and a
 *  write-target rule's canonical is one of its members. Half a mapping is
 *  refused there, so it blocks the save here. */
export const linearTeamRulesValid = (t: LinearTeamConfig): boolean => {
  if (linearTeamIsUnmapped(t)) return true
  if (!linearTeamIsArmed(t)) return false
  return [t.in_progress, t.done].every((r) => r.members.some((m) => m.id === r.canonical?.id))
}

/** linearTeamsBlocked reports whether the team's Linear rules would be refused
 *  by the save. Watching a team without mapping it never blocks. */
export const linearTeamsBlocked = (teams: LinearTeamConfig[]): boolean =>
  teams.some((t) => !linearTeamRulesValid(t))

/** linearDraftAfterSave is the form's set once a save of `sent` lands with
 *  `stored`. The editor stays live while the request is out, so `current` may
 *  hold edits made since — a rule changed, a team watched, a pre-fill that
 *  landed. With none, the form takes the stored set, which carries the names
 *  Linear resolved on the way in; with some, they are kept and stay unsaved
 *  against the new baseline rather than being overwritten by an older set. */
export const linearDraftAfterSave = (
  current: LinearTeamConfig[],
  sent: LinearTeamConfig[],
  stored: LinearTeamConfig[],
): LinearTeamConfig[] => (linearTeamsEqual(current, sent) ? stored : current)

/** linearTeamsEqual reports whether two sets would save as the same thing:
 *  the same teams in the same order, and per team the same state ids in each
 *  rule and the same write targets. A rule is a set — the order its members
 *  were picked in is not saved as meaning anything — and a state's name and
 *  type are refreshed from Linear on save, so neither makes a set dirty. */
export function linearTeamsEqual(a: LinearTeamConfig[], b: LinearTeamConfig[]): boolean {
  const ids = (refs: LinearStateRef[]) =>
    refs
      .map((r) => r.id)
      .sort()
      .join(',')
  const sameRule = (
    x: JiraStatusRuleValue<LinearStateRef>,
    y: JiraStatusRuleValue<LinearStateRef>,
  ) => ids(x.members) === ids(y.members) && (x.canonical?.id ?? '') === (y.canonical?.id ?? '')
  return (
    a.length === b.length &&
    a.every(
      (t, i) =>
        t.id === b[i].id &&
        sameRule(t.pickup, b[i].pickup) &&
        sameRule(t.in_progress, b[i].in_progress) &&
        sameRule(t.done, b[i].done),
    )
  )
}

// The state types each rule is pre-filled from.
const PICKUP_TYPES = new Set(['triage', 'backlog', 'unstarted'])
const DONE_TYPES = new Set(['completed', 'canceled'])

/** prefillLinearRules maps a team's rules from its workflow states' types, the
 *  thing a Linear workflow carries that a Jira one does not:
 *
 *    pickup      ← every triage, backlog and unstarted state
 *    in_progress ← every started state; canonical the lowest-positioned one
 *    done        ← every completed and canceled state; canonical the
 *                  lowest-positioned completed one
 *
 *  It is a starting point offered in the editor and nothing more: the saved
 *  rules are whatever ids the user saves. A workflow missing a state of the
 *  kind a rule needs — no started state, no completed state, nothing to pick
 *  up from — returns null, and the team is left for the user to map. */
export function prefillLinearRules(
  states: LinearStateOption[],
): Pick<LinearTeamConfig, 'pickup' | 'in_progress' | 'done'> | null {
  const byPosition = [...states].sort((a, b) => a.position - b.position)
  const ref = (s: LinearStateOption): LinearStateRef => ({ id: s.id, name: s.name, type: s.type })
  const pickup = byPosition.filter((s) => PICKUP_TYPES.has(s.type)).map(ref)
  const started = byPosition.filter((s) => s.type === 'started').map(ref)
  const done = byPosition.filter((s) => DONE_TYPES.has(s.type)).map(ref)
  const completed = done.find((s) => s.type === 'completed')
  if (pickup.length === 0 || started.length === 0 || !completed) return null
  return {
    pickup: { members: pickup },
    in_progress: { members: started, canonical: started[0] },
    done: { members: done, canonical: completed },
  }
}

/** unresolvableLinearStates returns the states a team's rules name that its
 *  live workflow no longer has. As with Jira, nothing repairs them silently:
 *  they are shown and removed by hand. Empty when the workflow has not been
 *  fetched, which is no evidence that anything is gone. */
export function unresolvableLinearStates(
  t: LinearTeamConfig,
  states: LinearStateRef[],
): LinearStateRef[] {
  if (states.length === 0) return []
  const known = new Set(states.map((s) => s.id))
  const out: LinearStateRef[] = []
  const seen = new Set<string>()
  const rules: JiraStatusRuleValue<LinearStateRef>[] = [t.pickup, t.in_progress, t.done]
  for (const rule of rules) {
    for (const ref of [...rule.members, ...(rule.canonical ? [rule.canonical] : [])]) {
      if (seen.has(ref.id) || known.has(ref.id)) continue
      seen.add(ref.id)
      out.push(ref)
    }
  }
  return out
}

/** dropLinearState removes one state from every rule of a team, clearing a
 *  canonical that pointed at it. */
export function dropLinearState(t: LinearTeamConfig, state: LinearStateRef): LinearTeamConfig {
  const strip = (r: JiraStatusRuleValue<LinearStateRef>): JiraStatusRuleValue<LinearStateRef> => ({
    members: r.members.filter((m) => m.id !== state.id),
    canonical: r.canonical && r.canonical.id === state.id ? null : r.canonical,
  })
  return {
    ...t,
    pickup: { members: t.pickup.members.filter((m) => m.id !== state.id) },
    in_progress: strip(t.in_progress),
    done: strip(t.done),
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

export type LinearTeamsSaveResult =
  | { ok: true; teams: LinearTeamConfig[] }
  | { ok: false; error: string }

// saveTeamLinearTeams persists the tracked Linear teams and their rules via
// PUT /api/teams/{id}/linear-teams — a full replace-set, in display order.
// Every rule is sent: the form always holds a team's whole rule set by id, so
// there is no rule it cannot express, and an empty rule is a real request to
// clear it.
export async function saveTeamLinearTeams(
  teamId: string,
  teams: LinearTeamConfig[],
): Promise<LinearTeamsSaveResult> {
  const ids = (refs: LinearStateRef[]) => refs.map((r) => r.id)
  const linear_teams = teams.map((t) => ({
    id: t.id,
    pickup: { member_ids: ids(t.pickup.members) },
    in_progress: {
      member_ids: ids(t.in_progress.members),
      canonical_id: t.in_progress.canonical?.id ?? '',
    },
    done: { member_ids: ids(t.done.members), canonical_id: t.done.canonical?.id ?? '' },
  }))
  try {
    // Adopt the set AS STORED: the server resolves each team's key and name
    // and each state's name and type from Linear on the way in.
    const body = await apiJSON<{ linear_teams?: LinearTeamWire[] }>(
      `${teamPath(teamId)}/linear-teams`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ linear_teams }),
      },
    )
    return { ok: true, teams: (body.linear_teams ?? []).map(linearTeamFromWire) }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the Linear teams.') }
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
