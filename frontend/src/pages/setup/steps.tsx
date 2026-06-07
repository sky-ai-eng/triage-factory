// The wizard's step registry — one atomic action per stack entry. The
// ORGANIZATION steps (GitHub URL → GitHub access method → [App: account type →
// register | PAT: token → clone protocol (local only)] → GitHub poll interval →
// Trackers → [Jira URL → Jira access → Jira poll interval, shown only when Jira
// is the chosen/connected tracker] → org max model tier) and the four TEAM
// steps for
// the first team (Repositories, GitHub teams, Jira projects, team default
// model), composing the existing shared field groups into the same step
// contract with no new host plumbing.
//
// The split is deliberate: each integration's URL, access, and cadence are
// separate entries (a URL step's Continue runs the reachability probe; an
// access step's Continue runs the connect — no separate Verify/Connect
// buttons), so the stack reads as one decision per card. The Jira steps are
// gated via `visible` on the tracker choice; the team steps are ordered
// repos-first so persisting the tracked repos makes the GitHub-teams candidate
// list enumerable (the structural fix for the always-empty-teams bug), and the
// Jira-projects step is gated on a connected Jira tracker.
//
// Reuse rule: every step composes a shared group (GitHubAccessGroup /
// GitHubAppPanel via the GitHub bodies, JiraAccessGroup via the Jira bodies,
// PollerTimingGroup, ModelGroup, the RepoPickerModal, GitHubTeamGroup,
// JiraProjectRulesGroup, and the shared ModelTierSelector) — no parallel field
// UIs. Org persistence rides the single session-scoped POST /api/settings/org;
// team persistence rides the existing per-team repos / github-groups PUTs and
// the team-settings POST — no wizard-only persistence path to drift. The URL
// steps persist on Continue (reachability probe + base-URL save); the access
// steps connect on Continue (PAT) or via an external launch (App register), so
// their per-step persist either performs the connect or is a no-op advance.

import {
  GitHubUrlStep,
  GitHubModeStep,
  GitHubAccountTypeStep,
  GitHubAppStep,
  GitHubPatStep,
  GitHubCloneStep,
  DEFAULT_GITHUB_URL,
} from './GitHubStep'
import TrackersStep from './TrackersStep'
import { JiraUrlStep, JiraAccessStep } from './JiraStep'
import {
  hostOf,
  isHttpUrl,
  normalizeBaseUrl,
  checkGitHubReachability,
  checkJiraReachability,
  reachabilityMessage,
} from '../../lib/reachability'
import { toast } from '../../components/Toast/toastStore'
import RepoPickerModal from '../../components/RepoPickerModal'
import { OrgModelStep, TeamModelStep } from './ModelStep'
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
import type { LoadContext, WizardState, WizardStep } from './types'

// Fresh wizard state before any load lands. Reuses the same empty-form
// factories the Settings/create pages use, so the shell starts from the
// identical baseline shape; the org-step flags default to "not connected".
export const initialWizardState = (): WizardState => ({
  org: emptyOrgConfig(),
  orgLoaded: false,
  hasGitHubPat: false,
  githubReady: false,
  githubUrlConfirmed: false,
  githubAccessTab: null,
  githubAppOwnerType: 'user',
  isLocal: false,
  jiraConnected: false,
  jiraUrlConfirmed: false,
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
// signals. It runs ONCE — as the GitHub URL step's load (the first org step) —
// and the slice it returns is merged into shared wizard state, so every later
// org step (access, poller, trackers, model) reads the same org form without
// re-fetching. Loading the whole form (not just one field) keeps each step's
// persist round-tripping the same org_settings shape, so saving the tier never
// clobbers the base URL / intervals / stored PAT. `orgLoaded: true` marks the
// seed succeeded; if this throws, the flag stays false and the persist-bearing
// steps refuse to write the empty default form over real settings.
//
// The github_url is prefilled to https://github.com when the org has no stored
// base URL, so the URL step is a one-tap confirm for the common case (free
// entry still edits it for *.ghe.com / GHES). The url/access-confirmed flags
// seed from the connection signals: a connected org has, by definition, a
// previously-confirmed URL and access, so it resumes past those steps; the
// access tab defaults to PAT for an org with a stored token, else App.
async function loadOrg(ctx: LoadContext): Promise<Partial<WizardState>> {
  const [org, integrations] = await Promise.all([fetchOrgSettings(), fetchIntegrationsState()])
  if (!org) throw new Error('Could not load organization settings')
  const orgForm = orgConfigFromSettings(org)
  if (!orgForm.github_url) orgForm.github_url = DEFAULT_GITHUB_URL
  return {
    org: orgForm,
    orgLoaded: true,
    isLocal: ctx.isLocal,
    hasGitHubPat: org.has_github_pat,
    githubReady: integrations.githubReady,
    // Seeded only from a live connection — NOT from a stored base URL. A stored
    // URL must not pre-satisfy the step, or Continue would skip the probe and
    // advance any value unchecked. An unconnected org always re-probes on
    // Continue (the URL step's whole job), connected resumes past it.
    githubUrlConfirmed: integrations.githubReady,
    // A returning org pre-selects its method (PAT if a token is stored, else App
    // when already connected); a fresh org starts unselected so the picker opens
    // with neither chosen and advances on the first click.
    githubAccessTab: org.has_github_pat ? 'pat' : integrations.githubReady ? 'app' : null,
    jiraConnected: integrations.jiraConnected,
    jiraUrlConfirmed: integrations.jiraConnected,
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
// GitHubTeamGroup's own fetch + onLoaded. `teamLoaded: true` marks the seed
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

// Step · GitHub URL (mandatory, first org step). The base-URL reachability
// gate: Continue probes the host (URL-only, no creds) and, on success, saves
// the base URL and marks it confirmed; an unreachable host rejects, bouncing
// Continue and painting the box red (the body reads ctx.error). Owns the single
// org load (loadOrg), which seeds shared state for every later org step.
// isComplete is satisfied by a confirmed URL OR an already-connected org (which
// implies one), so a returning connected org resumes past it.
const githubUrlStep: WizardStep = {
  id: 'org-github-url',
  section: 'org',
  title: 'GitHub URL',
  advanceOnEnter: true,
  load: loadOrg,
  isComplete: (s) => s.githubReady || s.githubUrlConfirmed,
  // Instant format check first (no round-trip for obviously-bad input); the
  // network probe is in persist. A connected org skips both — its URL is proven.
  validate: (s) => {
    if (s.githubReady) return null
    const url = s.org.github_url.trim()
    if (url === '') return 'Enter your GitHub URL.'
    if (!isHttpUrl(url)) return 'Enter a valid URL, including https://.'
    return null
  },
  persist: async ({ state, patch }) => {
    // Always probe on Continue (the step's whole job) unless already connected —
    // a stored/previously-confirmed URL must not let an edited value through
    // unchecked.
    if (state.githubReady) return
    // Normalize (trim + drop trailing slash) before probing AND persisting, and
    // write the clean value back to state — the backend stores the base URL
    // verbatim, so a stray space/slash that the probe tolerates would otherwise
    // be saved and break the App/PAT host derivation downstream.
    const url = normalizeBaseUrl(state.org.github_url)
    const result = await checkGitHubReachability(url)
    if (!result.reachable) throw new Error(reachabilityMessage(result))
    // Persist the base URL now (blank PAT = keep current): the App-registration
    // path reads the stored host, and the PAT path re-saves URL+creds together
    // at the access step. Round-trips the same org form loadOrg seeded, so the
    // intervals / model cap / stored PAT are untouched.
    const nextOrg = { ...state.org, github_url: url }
    const save = await saveOrgConfig(nextOrg)
    if (!save.ok) throw new Error(save.error)
    patch({ org: nextOrg, githubUrlConfirmed: true })
  },
  collapsedSummary: (s) => `GitHub URL: ${hostOf(s.org.github_url || DEFAULT_GITHUB_URL)}`,
  render: (ctx) => <GitHubUrlStep {...ctx} />,
}

// Step · GitHub access method (the mode picker). App vs PAT, as its own step;
// it starts unselected and advances on click (selfAdvancing — no Continue
// button), the in-body action both recording the choice and moving on. Complete
// once a method is chosen; the mandatory GitHub gate lives on whichever config
// step the choice makes visible.
const githubModeStep: WizardStep = {
  id: 'org-github-mode',
  section: 'org',
  title: 'GitHub access',
  selfAdvancing: true,
  isComplete: (s) => s.githubAccessTab !== null,
  persist: async () => {},
  collapsedSummary: (s) =>
    s.githubAccessTab === 'pat'
      ? 'Personal access token'
      : s.githubAccessTab === 'app'
        ? 'GitHub App'
        : 'Not chosen',
  render: (ctx) => <GitHubModeStep {...ctx} />,
}

// Step · App account type (visible when App is the chosen method, before the
// registration step). Personal vs Organization — which GitHub account the App
// is registered under — as a flush two-panel picker, action-on-click
// (selfAdvancing). Always complete (defaults to Personal); the value rides
// state.githubAppOwnerType into the registration step.
const githubAccountStep: WizardStep = {
  id: 'org-github-account',
  section: 'org',
  title: 'App account type',
  visible: (s) => s.githubAccessTab === 'app',
  selfAdvancing: true,
  isComplete: () => true,
  persist: async () => {},
  collapsedSummary: (s) =>
    s.githubAppOwnerType === 'org' ? 'Organization account' : 'Personal account',
  render: (ctx) => <GitHubAccountTypeStep {...ctx} />,
}

// connectedSummary — shared by the App / PAT config steps: the connected host,
// or "Not connected".
const connectedSummary = (s: WizardState) =>
  s.githubReady ? `Connected · ${hostOf(s.org.github_url || DEFAULT_GITHUB_URL)}` : 'Not connected'

// Step · GitHub App (mandatory, visible when App is the chosen method). The App
// registration is GitHubAppPanel's external Register launch (a redirect can't
// fold into Continue), so persist is a no-op — Continue just advances once the
// App is registered+installed (github_ready), which validate gates.
const githubAppStep: WizardStep = {
  id: 'org-github-app',
  section: 'org',
  title: 'GitHub App',
  visible: (s) => s.githubAccessTab === 'app',
  isComplete: (s) => s.githubReady,
  validate: (s) => (s.githubReady ? null : 'Register and install your GitHub App to continue.'),
  persist: async () => {},
  collapsedSummary: connectedSummary,
  render: (ctx) => <GitHubAppStep {...ctx} />,
}

// Step · Personal access token (mandatory, visible when PAT is the chosen
// method). No separate Connect button — Continue performs the connect:
// saveOrgConfig POSTs the org form; the backend validates the PAT against the
// base URL and 422s on a bad token. A freshly-entered token is trusted on a
// successful save; an empty field relied on a *stored* token, which the save
// does NOT re-validate, so confirm against the server's github_ready signal
// rather than optimistically marking connected.
const githubPatStep: WizardStep = {
  id: 'org-github-pat',
  section: 'org',
  title: 'Personal access token',
  visible: (s) => s.githubAccessTab === 'pat',
  isComplete: (s) => s.githubReady,
  validate: (s) => {
    if (s.githubReady) return null
    return !s.hasGitHubPat && s.org.github_pat.trim() === ''
      ? 'Enter a personal access token to connect.'
      : null
  },
  persist: async ({ state, patch }) => {
    if (state.githubReady) return
    const result = await saveOrgConfig(state.org)
    if (!result.ok) throw new Error(result.error)
    if (state.org.github_pat.trim() === '') {
      const { githubReady } = await fetchIntegrationsState()
      if (!githubReady) {
        throw new Error('Couldn’t verify your stored GitHub token. Re-enter it to reconnect.')
      }
    }
    patch({ githubReady: true, hasGitHubPat: true, org: { ...state.org, github_pat: '' } })
  },
  collapsedSummary: connectedSummary,
  render: (ctx) => <GitHubPatStep {...ctx} />,
}

// Step · Clone protocol (visible when PAT is the chosen method AND local — multi
// hardwires HTTPS). SSH vs HTTPS for how repos clone to the machine, as a flush
// two-panel picker, action-on-click (selfAdvancing). Always complete (defaults
// to SSH); persist saves the org form. Switching to SSH triggers the backend's
// SSH preflight, which a default-SSH org never hits.
const githubCloneStep: WizardStep = {
  id: 'org-github-clone',
  section: 'org',
  title: 'Clone protocol',
  visible: (s) => s.githubAccessTab === 'pat' && s.isLocal,
  selfAdvancing: true,
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) => `Clone via ${s.org.github_clone_protocol.toUpperCase()}`,
  render: (ctx) => <GitHubCloneStep {...ctx} />,
}

// Step · GitHub poll interval. Sits right after the GitHub steps so each
// integration's connect + cadence stay together. Always satisfiable (a default
// exists), so it never blocks the stack; persistOrg guards against saving when
// the org load failed. Renders the GitHub control alone (showJira=false).
const githubPollerStep: WizardStep = {
  id: 'org-github-poller',
  section: 'org',
  title: 'GitHub poll interval',
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) => `GitHub every ${intervalLabel(s.org.github_poll_interval)}`,
  render: ({ state, patch }) => (
    <PollerTimingGroup
      value={{
        github_poll_interval: state.org.github_poll_interval,
        jira_poll_interval: state.org.jira_poll_interval,
      }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
      showJira={false}
      showHeading={false}
    />
  ),
}

// Step · Trackers (optional). None / Jira / Linear (Linear "coming soon") — just
// the picker. When Jira is chosen, the Jira URL + access steps below own the
// connection, so any selection is a valid end state here; a Jira picked but not
// connected is blocked downstream by the Jira access step, not here.
const trackersStep: WizardStep = {
  id: 'org-trackers',
  section: 'org',
  title: 'Trackers',
  isComplete: () => true,
  persist: async () => {},
  collapsedSummary: (s) => (s.tracker === 'jira' ? 'Jira' : 'No tracker'),
  render: (ctx) => <TrackersStep {...ctx} />,
}

// jiraActive: Jira is the chosen tracker AND connected. Gates the Jira poll
// step + Jira-projects step visibility and the wizard's carry-over finish
// branch — one definition, no drift. A user who connects Jira then switches the
// picker back to None shouldn't see (or save) a Jira interval, even though the
// connection itself persists until explicitly disconnected.
export const jiraActive = (s: WizardState) => s.tracker === 'jira' && s.jiraConnected

// Step · Jira URL (visible only when Jira is the chosen tracker). The Jira
// mirror of the GitHub URL step: Continue probes reachability, then marks the
// URL confirmed; an unreachable host bounces Continue + reddens the box. No
// base-URL save here — the access step's connect persists URL+PAT together.
const jiraUrlStep: WizardStep = {
  id: 'org-jira-url',
  section: 'org',
  title: 'Jira URL',
  advanceOnEnter: true,
  visible: (s) => s.tracker === 'jira',
  isComplete: (s) => s.jiraConnected || s.jiraUrlConfirmed,
  validate: (s) => {
    if (s.jiraConnected) return null
    const url = s.org.jira_url.trim()
    if (url === '') return 'Enter your Jira URL.'
    if (!isHttpUrl(url)) return 'Enter a valid URL, including https://.'
    return null
  },
  persist: async ({ state, patch }) => {
    // Always probe on Continue unless already connected — same rule as GitHub.
    if (state.jiraConnected) return
    // Normalize + write back so the value the access step's connect sends (and
    // the collapsed summary shows) is the canonical, trimmed URL.
    const url = normalizeBaseUrl(state.org.jira_url)
    const result = await checkJiraReachability(url)
    if (!result.reachable) throw new Error(reachabilityMessage(result))
    patch({ jiraUrlConfirmed: true, org: { ...state.org, jira_url: url } })
  },
  // Jira has no default URL (unlike github.com), so guard the empty case — a
  // disconnect can leave the bar momentarily without a host — rather than
  // rendering a bare "Jira URL:".
  collapsedSummary: (s) => `Jira URL: ${s.org.jira_url.trim() ? hostOf(s.org.jira_url) : '—'}`,
  render: (ctx) => <JiraUrlStep {...ctx} />,
}

// Step · Jira access (visible only when Jira is the chosen tracker). The PAT
// connect, via the shared JiraAccessGroup which owns its Connect button +
// connect/disconnect lifecycle; the step gates on the connected flag it reports
// back. Mandatory while Jira is selected — its isComplete blocks finishing a
// half-connected Jira — but invisible (and so non-blocking) once the tracker is
// None.
const jiraAccessStep: WizardStep = {
  id: 'org-jira-access',
  section: 'org',
  title: 'Jira access',
  visible: (s) => s.tracker === 'jira',
  isComplete: (s) => s.jiraConnected,
  validate: (s) =>
    s.jiraConnected ? null : 'Connect Jira to continue, or pick a different tracker.',
  persist: async () => {},
  collapsedSummary: (s) =>
    s.jiraConnected ? `Connected · ${hostOf(s.org.jira_url)}` : 'Not connected',
  render: (ctx) => <JiraAccessStep {...ctx} />,
}

// Step · Jira poll interval (visible only when Jira is connected). The separate
// tracker cadence the GitHub-poll split implies — appears after the Jira steps,
// gated on jiraActive so it's shown (and saved) only with a live connection.
// Renders the Jira control alone (showGitHub=false).
const jiraPollerStep: WizardStep = {
  id: 'org-jira-poller',
  section: 'org',
  title: 'Jira poll interval',
  visible: (s) => jiraActive(s),
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) => `Jira every ${intervalLabel(s.org.jira_poll_interval)}`,
  render: ({ state, patch }) => (
    <PollerTimingGroup
      value={{
        github_poll_interval: state.org.github_poll_interval,
        jira_poll_interval: state.org.jira_poll_interval,
      }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
      showGitHub={false}
      showHeading={false}
    />
  ),
}

// Step 4 · Org max model tier — the ceiling ladder. Action-on-click
// (selfAdvancing): picking a cap records it and advances. Optional (no cap is a
// legitimate end state), so it never blocks. No load of its own — reads the
// GitHub step's seeded org form; persistOrg guards against saving when that load
// failed.
const orgModelStep: WizardStep = {
  id: 'org-model',
  section: 'org',
  title: 'Max model tier',
  selfAdvancing: true,
  isComplete: () => true,
  persist: ({ state }) => persistOrg(state),
  collapsedSummary: (s) =>
    s.org.max_llm_model_tier
      ? `Capped at ${TIER_LABELS[s.org.max_llm_model_tier] ?? s.org.max_llm_model_tier}`
      : 'No model cap',
  render: (ctx) => <OrgModelStep {...ctx} />,
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
  // Mappings load lazily (GitHubTeamGroup's onLoaded), so before the step is
  // visited github_groups is undefined — show a neutral dash rather than a
  // misleading "0" the user never chose, until a real count is known.
  collapsedSummary: (s) =>
    s.team.github_groups === undefined
      ? 'Bound GH teams: —'
      : `Bound GH teams: ${s.team.github_groups.length}`,
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
  // The last step: action-on-click finishes setup (selfAdvancing — no Finish
  // button), persisting the team form (and surfacing any model-cap clamp).
  selfAdvancing: true,
  isComplete: (s) => s.team.default_model.trim() !== '',
  validate: (s) => (s.team.default_model.trim() === '' ? 'Choose a default model.' : null),
  persist: async ({ state, teamId }) => {
    const warning = await persistTeamSettings(teamId, state)
    if (warning) toast.info(warning)
  },
  collapsedSummary: (s) =>
    `Default model: ${TIER_LABELS[s.team.default_model] ?? s.team.default_model}`,
  render: (ctx) => <TeamModelStep {...ctx} />,
}

// Ordered registry. Section grouping is derived from each step's `section`;
// the host inserts a divider above the first step of each section. The
// Jira-projects step is conditionally omitted via its `visible` predicate.
export const WIZARD_STEPS: WizardStep[] = [
  githubUrlStep,
  githubModeStep,
  githubAccountStep,
  githubAppStep,
  githubPatStep,
  githubCloneStep,
  githubPollerStep,
  trackersStep,
  jiraUrlStep,
  jiraAccessStep,
  jiraPollerStep,
  orgModelStep,
  reposStep,
  githubTeamsStep,
  jiraProjectsStep,
  teamModelStep,
]
