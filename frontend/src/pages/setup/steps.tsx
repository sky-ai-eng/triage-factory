// The wizard's step registry — one atomic action per stack entry. The
// ORGANIZATION steps (GitHub URL → GitHub access method → [App: account type →
// register | PAT: token → clone protocol (local only)] → GitHub poll interval →
// Trackers → [Jira URL → Jira access → Jira poll interval, shown only when Jira
// is the chosen/connected tracker] → org max model tier → Claude credentials
// [source (local only) → Anthropic key (multi always; local when BYOK)]) and
// the four TEAM steps for
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
// JiraProjectRulesGroup, and the shared ModelPicker) — no parallel field
// UIs. Org persistence rides the single PATCH /api/orgs/{org}/settings; team
// persistence rides the existing per-team repos / github-groups / jira-projects
// PUTs and the team-settings PATCH — no wizard-only persistence path to drift. The URL
// steps persist on Continue (reachability probe + base-URL save); the access
// steps connect on Continue (PAT) or via an external launch (App register), so
// their per-step persist either performs the connect or is a no-op advance.

import {
  GitHubUrlStep,
  GitHubModeStep,
  GitHubAppSourceStep,
  GitHubAccountTypeStep,
  GitHubAppStep,
  GitHubAppImportStep,
  GitHubAppInstallStep,
  GitHubPatStep,
  GitHubCloneStep,
  DEFAULT_GITHUB_URL,
} from './GitHubStep'
import TrackersStep from './TrackersStep'
import { JiraUrlStep, JiraModeStep, JiraAccessStep } from './JiraStep'
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
import { OrgBackgroundJobsModelStep, TeamModelStep } from './ModelStep'
import { OrgClaudeSourceStep, OrgClaudeKeyStep } from './ClaudeStep'
import { UserIdentityStep } from './UserIdentityStep'
import { JiraUserAccessStep } from './JiraUserAccessStep'
import { captureGitHubIdentityPat } from '../../lib/githubIdentity'
import { captureJiraIdentityPat, captureJiraIdentityApiToken } from '../../lib/jiraIdentity'
import {
  getGitHubAppStatus,
  refreshGitHubAppInstallations,
  cutoverToApp,
  switchToPat,
  discardStagedApp,
} from '../../lib/githubApp'
import { apiJSON } from '../../lib/apiClient'
import { modelCatalogEntry, modelDisplayName } from '../../hooks/useModelCatalog'
import { gateModelSave, offerSweepAfterConnect } from '../../lib/modelGate'
import type { GitHubIdentityStatus, JiraIdentityStatus } from '../../types'
import PollerTimingGroup from '../settings/PollerTimingGroup'
import GitHubTeamGroup from '../settings/GitHubTeamGroup'
import JiraProjectRulesGroup from '../settings/JiraProjectRulesGroup'
import {
  emptyOrgConfig,
  fetchOrgSettings,
  orgConfigFromSettings,
  patchOrgSettings,
  type OrgSettingsData,
  type OrgSettingsPatch,
} from '../settings/orgConfig'
import { connectJira, type JiraDeployment } from '../settings/jiraConnect'
import { connectGitHubPAT } from '../settings/orgCredentials'
import { connectAnthropic, disconnectLLM } from '../settings/anthropicConnect'
import { connectBedrock, bedrockPayloadFromForm } from '../settings/bedrockConnect'
import {
  emptyTeamConfig,
  fetchTeamRepos,
  fetchTeamSettings,
  saveTeamGitHubGroups,
  saveTeamRepos,
  saveTeamJiraProjects,
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
  githubPatLogin: '',
  githubPatEnvProvided: false,
  jiraCredentialEnvProvided: false,
  githubReady: false,
  githubUrlConfirmed: false,
  githubAccessTab: null,
  githubAppOwnerType: 'user',
  githubAppSource: null,
  githubAppRegistered: false,
  githubAppStaged: false,
  githubAppSlug: '',
  githubAppStatus: null,
  githubAppInstalled: false,
  githubAppInstallCount: 0,
  isLocal: false,
  jiraConnected: false,
  jiraUrlConfirmed: false,
  jiraDeployment: null,
  tracker: 'none',
  anthropicKeySource: null,
  anthropicConnected: false,
  claudeProvider: 'anthropic',
  bedrockConnected: false,
  bedrockStoredMethod: null,
  team: emptyTeamConfig(),
  teamLoaded: false,
  userIdentityConnected: false,
  userIdentityLogin: '',
  userIdentityHost: '',
  userConnectAvailable: false,
  userGitHubPat: '',
  jiraUserConnected: false,
  jiraUserAccount: '',
  jiraUserHost: '',
  jiraUserConnectAvailable: false,
  jiraUserPat: '',
  jiraUserEmail: '',
  jiraUserApiToken: '',
  duplicateGitHubToUser: false,
  duplicateJiraToUser: false,
})

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
async function fetchIntegrationsState(): Promise<{
  githubReady: boolean
  jiraConnected: boolean
  jiraDeployment: JiraDeployment | null
}> {
  const empty = { githubReady: false, jiraConnected: false, jiraDeployment: null }
  try {
    const data = await apiJSON<{
      github_ready?: boolean
      jira?: boolean
      jira_url?: string
      jira_deployment?: string
    }>('/api/integrations/status')
    return {
      githubReady: !!data.github_ready,
      jiraConnected: !!data.jira && !!data.jira_url,
      // The backend's authoritative deployment (from the auth-method marker);
      // null when not connected or an unexpected value.
      jiraDeployment:
        data.jira_deployment === 'cloud'
          ? 'cloud'
          : data.jira_deployment === 'data_center'
            ? 'data_center'
            : null,
    }
  } catch {
    return empty
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
export async function loadOrg(ctx: LoadContext): Promise<Partial<WizardState>> {
  if (!ctx.orgId) throw new Error('Could not load organization settings')
  const [org, integrations] = await Promise.all([
    fetchOrgSettings(ctx.orgId),
    fetchIntegrationsState(),
  ])
  if (!org) throw new Error('Could not load organization settings')
  const orgForm = orgConfigFromSettings(org)
  if (!orgForm.github_url) orgForm.github_url = DEFAULT_GITHUB_URL
  return {
    org: orgForm,
    orgLoaded: true,
    isLocal: ctx.isLocal,
    // Default the local-mode "use this token as my own identity too" reuse
    // checkboxes to checked: in local mode the org and personal token are almost
    // always the same, so opt the operator into the streamline by default. The
    // checkboxes (shown only in local mode) let them opt out. Always false in
    // multi, where they never render anyway.
    duplicateGitHubToUser: ctx.isLocal,
    duplicateJiraToUser: ctx.isLocal,
    hasGitHubPat: org.has_github_pat,
    githubPatLogin: org.github_pat_login ?? '',
    githubPatEnvProvided: org.github_pat_env_provided ?? false,
    jiraCredentialEnvProvided: org.jira_credential_env_provided ?? false,
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
    // A returning connected org resumes past the deployment picker with its
    // backend taken from the authoritative auth-method marker (surfaced by the
    // status endpoint), NOT re-guessed from the host shape — so a Cloud org on a
    // custom domain still labels as Cloud. A fresh/unconnected org starts
    // unselected so the picker opens and the choice is made explicitly. This
    // only labels the picker; the credential the org authenticates with is the
    // stored one.
    jiraDeployment: integrations.jiraConnected ? integrations.jiraDeployment : null,
    tracker: integrations.jiraConnected ? 'jira' : 'none',
    // Claude credentials: the source resumes from the org's STORED selection,
    // not from whether a credential happens to be bound — an org that chose to
    // bring its own key and has not bound one yet is a different state from one
    // running on the machine's, and re-guessing it here is what made the two
    // indistinguishable. The GET already resolves the mode, so multi arrives as
    // 'byok' and a fresh local install as 'system' (the column default), which
    // is what lets the source step auto-resolve with nothing asked. The
    // provider radio resumes on whichever provider is stored.
    anthropicConnected: org.has_anthropic_api_key,
    anthropicKeySource: org.llm_auth_method,
    claudeProvider: org.has_bedrock_credentials ? 'bedrock' : 'anthropic',
    bedrockConnected: org.has_bedrock_credentials,
    bedrockStoredMethod:
      org.bedrock_auth_method === 'bearer' ||
      org.bedrock_auth_method === 'access_keys' ||
      org.bedrock_auth_method === 'role'
        ? org.bedrock_auth_method
        : null,
  }
}

// saveOrgSlice is the low-level save behind every org step that writes through
// the settings PATCH: it sends ONLY the named fields, at the concurrency token
// wizard state holds, and returns the post-save row. It refuses to write when
// the single org load (the GitHub step's) failed — `state.org` would be the
// empty default form, and even a one-field save would write a default the user
// never chose. The GitHub step shows a retry when its load fails, so this
// guard only trips if the user reaches a later org step while that load is
// still broken.
//
// On a version conflict the row is re-read and the fresh token patched into
// wizard state before the error surfaces — the message tells the user to
// re-apply their edit, and with the token refreshed their immediate retry can
// succeed instead of needing a page reload.
async function saveOrgSlice(
  ctx: { state: WizardState; patch: (p: Partial<WizardState>) => void; orgId: string | null },
  slice: OrgSettingsPatch,
): Promise<OrgSettingsData> {
  if (!ctx.state.orgLoaded || !ctx.orgId) {
    throw new Error('Settings didn’t load — reopen the GitHub step and retry before saving.')
  }
  const result = await patchOrgSettings(ctx.orgId, ctx.state.org.version, slice)
  if (!result.ok) {
    if (result.conflict) {
      const fresh = await fetchOrgSettings(ctx.orgId)
      if (fresh) ctx.patch({ org: { ...ctx.state.org, version: fresh.version } })
    }
    throw new Error(result.error)
  }
  return result.settings
}

// persistOrgFields builds the persist for an org step that saves through the
// settings PATCH (clone protocol, the poll intervals, the model cap): only the
// named fields ride the wire, so a step can never carry — or clobber — a value
// its card doesn't show. A lingering in-memory org PAT is therefore not a
// hazard by construction: credentials aren't representable in the patch at all.
//
// The save carries the row's concurrency token and the response returns the
// next one, so the version is patched back into wizard state: without that, the
// second org step's save would conflict with the first step's own write.
export function persistOrgFields(...fields: (keyof OrgSettingsPatch)[]) {
  return async (ctx: {
    state: WizardState
    patch: (p: Partial<WizardState>) => void
    orgId: string | null
  }): Promise<void> => {
    const slice = Object.fromEntries(fields.map((f) => [f, ctx.state.org[f]])) as OrgSettingsPatch
    const settings = await saveOrgSlice(ctx, slice)
    ctx.patch({ org: { ...ctx.state.org, version: settings.version } })
  }
}

// freshOrgVersion re-reads the settings row's concurrency token after a write
// that lands OUTSIDE the settings PATCH: the credential binds and unbinds
// (GitHub PAT, Jira, the LLM providers) persist their host / key-ref columns
// on the same row server-side, which bumps the token — so the one wizard state
// holds goes stale the moment a connect succeeds, and the next field save
// would 409 against the connect's own write. Best-effort: on a failed re-read
// the held token stands, and the save path's conflict recovery covers it.
async function freshOrgVersion(orgId: string, held: number): Promise<number> {
  const fresh = await fetchOrgSettings(orgId)
  return fresh?.version ?? held
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
export async function loadTeam(teamId: string): Promise<Partial<WizardState>> {
  const [settings, repos] = await Promise.all([fetchTeamSettings(teamId), fetchTeamRepos(teamId)])
  if (!settings) throw new Error('Could not load team settings')
  return {
    team: { ...teamConfigFromSettings(settings), repos: repos ?? undefined },
    teamLoaded: true,
  }
}

// persistTeamSettings is the shared save for the team steps that round-trip the
// settings row via PATCH /api/teams/{id}/settings (the team's default model).
// It refuses to write when the team load (the Repositories step's)
// failed — `state.team` would be the empty default form, and saving it would
// clobber the stored model / thresholds. Returns the backend's model-cap clamp
// notice (if any) so the caller can surface it. The Jira project rules, the
// repos and the GitHub-team mappings each ride their own replace-set PUT,
// persisted by their own steps; this writes only the settings row.
async function persistTeamSettings(
  teamId: string,
  state: WizardState,
): Promise<string | undefined> {
  if (!state.teamLoaded) {
    throw new Error(
      'Team settings didn’t load — reopen the Repositories step and retry before saving.',
    )
  }
  const result = await saveTeamSettings(teamId, state.team, false)
  if (!result.ok) throw new Error(result.error)
  return result.warning
}

// gateModelPick holds a step's Continue to the same save gate every other model
// choice passes through: a model nothing has established this org's credentials
// can run is tested first, with the person's consent, and the step persists only
// on green. Refusing throws, which is how a step reports "not saved" — the host
// shows the message on the step it belongs to.
//
// Two things are not a gate. A blank key is the absence of a model, and the
// step's own isComplete is what refuses that. And a model the catalog read
// could not resolve proceeds: the client has learned nothing about it, and the
// save it would be gating is refused server-side anyway if the org cannot run
// it.
async function gateModelPick(orgId: string | null, key: string): Promise<void> {
  if (!orgId || key === '') return
  const entry = modelCatalogEntry(orgId, key)
  if (!entry) return
  if (await gateModelSave(orgId, entry)) return
  throw new Error(
    `${entry.display_name} hasn’t been verified against your credentials — test it to continue.`,
  )
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
  persist: async ({ state, patch, orgId }) => {
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
    // Persist the base URL now: it is what both access paths read. The App
    // registration derives its host from the stored value, and the PAT bind
    // validates the token against it — neither carries a host of its own, so
    // this step is where the workspace commits to one. The save names only the
    // URL, so the intervals / model cap are untouched by construction.
    if (!orgId) throw new Error('Settings didn’t load — retry before saving.')
    const settings = await saveOrgSlice({ state, patch, orgId }, { github_url: url })
    patch({
      org: { ...state.org, github_url: url, version: settings.version },
      githubUrlConfirmed: true,
    })
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

// Step · App source (visible when App is the chosen method, before the account
// type). Create a new App vs connect an existing one (bring-your-own), as a
// flush two-panel picker, action-on-click (selfAdvancing). The choice gates the
// account-type/register steps (create) vs the import step (existing). Complete
// once a source is chosen, OR once an App is already registered — so a returning
// org with a live/staged App (whose source value resets to null on reload, since
// it isn't persisted) resumes past it showing the connection rather than
// re-prompting.
const githubAppSourceStep: WizardStep = {
  id: 'org-github-app-source',
  section: 'org',
  title: 'App source',
  visible: (s) => s.githubAccessTab === 'app',
  selfAdvancing: true,
  isComplete: (s) => s.githubAppSource !== null || s.githubAppRegistered,
  persist: async () => {},
  collapsedSummary: (s) =>
    s.githubAppRegistered
      ? `Connected${s.githubAppSlug ? ` (${s.githubAppSlug})` : ''}`
      : s.githubAppSource === 'existing'
        ? 'Connect an existing App'
        : s.githubAppSource === 'create'
          ? 'Create a new App'
          : 'Not chosen',
  render: (ctx) => <GitHubAppSourceStep {...ctx} />,
}

// Step · App account type (visible when App is the chosen method AND the create
// source, before the registration step). Personal vs Organization — which GitHub
// account the App is registered under — as a flush two-panel picker,
// action-on-click (selfAdvancing). Hidden on the connect-existing branch
// (owner_type comes from GET /app), but kept visible once an App is registered
// (source resets to null on reload) so resume shows coherent collapsed history. The choice is seeded from a registered App's persisted
// owner_type by the shared App-status loader (loadGitHubAppInstall), so it
// survives a reload without a fetch of its own; absent an App it stays at the
// Personal default. Always complete; the value rides state.githubAppOwnerType
// into the registration step.
const githubAccountStep: WizardStep = {
  id: 'org-github-account',
  section: 'org',
  title: 'App account type',
  visible: (s) => s.githubAccessTab === 'app' && s.githubAppSource !== 'existing',
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
// App is registered (github_ready, which registration alone flips), which
// validate gates. Installation is the NEXT step (githubAppInstallStep), split
// out so a registered-but-uninstalled App can't sail past into the repo picker.
// The render passes returnTo='setup' so the post-registration callback lands
// back on /setup, resuming on the install step rather than teleporting ahead.
const githubAppStep: WizardStep = {
  id: 'org-github-app',
  section: 'org',
  title: 'GitHub App',
  visible: (s) => s.githubAccessTab === 'app' && s.githubAppSource !== 'existing',
  // Fetch the App status up front, with every other step's load, so the panel
  // has it before the step is ever opened. Best-effort: a failure just leaves
  // the panel to load it itself, exactly as it does in Settings.
  load: async ({ orgId }) =>
    orgId
      ? getGitHubAppStatus(orgId)
          .then((githubAppStatus) => ({ githubAppStatus }))
          .catch(() => ({}))
      : {},
  isComplete: (s) => s.githubReady,
  validate: (s) => (s.githubReady ? null : 'Register your GitHub App to continue.'),
  persist: async () => {},
  collapsedSummary: connectedSummary,
  render: (ctx) => <GitHubAppStep {...ctx} returnTo="setup" />,
}

// Step · Connect an existing App (visible when App is the chosen method, the
// connect-existing source, and no App is registered yet). The bring-your-own-App
// import form: App ID + private key, optional OAuth client secret + webhook
// secret. The form owns its own submit (selfAdvancing — no wizard Continue);
// validating + importing happens there, and on success it patches the same state
// the register path patches and advances to the install step. Disappears once an
// App is registered (the install step takes over). A 422 (bad pair, permission
// gap) stays inline in the form; the wizard never leaves the step until success.
const githubAppImportStep: WizardStep = {
  id: 'org-github-app-import',
  section: 'org',
  title: 'Connect your App',
  visible: (s) =>
    s.githubAccessTab === 'app' && s.githubAppSource === 'existing' && !s.githubAppRegistered,
  selfAdvancing: true,
  isComplete: (s) => s.githubAppRegistered,
  persist: async () => {},
  collapsedSummary: (s) =>
    s.githubAppRegistered
      ? `Connected${s.githubAppSlug ? ` (${s.githubAppSlug})` : ''}`
      : 'Not connected',
  render: (ctx) => <GitHubAppImportStep {...ctx} />,
}

// loadGitHubAppInstall seeds the App-presence + install gate from the live App
// status: githubAppInstalled / githubAppInstallCount drive the install step's
// resume, while githubAppRegistered / githubAppStaged / githubAppSlug drive the
// mode-card status lines, the cross-pick confirm, and the install step's
// cutover-vs-refresh branch. Best-effort and self-contained — a missing orgId
// or a failed read resolves to {} (the step keeps its defaults), since its
// persist re-verifies against the refresh/cutover endpoint regardless.
//
// It also OWNS the staged-window tab override: an org with ANY registered App
// (staged or live) resolves the access tab to 'app'. This corrects loadOrg's
// naive `has_github_pat ? 'pat'` derivation (steps.tsx), which would otherwise
// dump a mid-switch user (staged App + still-live PAT) back onto the PAT path.
// No registered App ⇒ the tab key is omitted, so loadOrg's PAT/null derivation
// stands. This relies on loadGitHubAppInstall merging AFTER loadOrg, which it
// does — both are org-section steps and the install step is the later of the
// two in WIZARD_STEPS.
//
// It is the SINGLE App-status loader: it also seeds githubAppOwnerType (the
// account-type picker's value), so the account step needs no load of its own
// and Settings needs only this one fetch — every step's load() fans out in
// parallel on mount, so a separate owner-type loader would just duplicate this
// GET /github/app.
export async function loadGitHubAppInstall(ctx: LoadContext): Promise<Partial<WizardState>> {
  if (!ctx.orgId) return {}
  try {
    const status = await getGitHubAppStatus(ctx.orgId)
    const app = status.app
    const n = status.installations.length
    const slice: Partial<WizardState> = {
      githubAppRegistered: !!app,
      githubAppStaged: !!app && !app.active,
      githubAppSlug: app?.slug ?? '',
      githubAppInstalled: n > 0,
      githubAppInstallCount: n,
    }
    if (app) {
      // Registered (staged → resume the switch; live → it's the live credential)
      // ⇒ App tab. Absent ⇒ leave the tab to loadOrg's PAT/null derivation.
      slice.githubAccessTab = 'app'
      // owner_type is a free string on the wire; narrow to the picker's union,
      // folding anything that isn't 'org' to 'user'. Absent an App, the default
      // 'user' stands (key omitted).
      slice.githubAppOwnerType = app.owner_type === 'org' ? 'org' : 'user'
    }
    return slice
  } catch {
    return {}
  }
}

// Step · Install the App (mandatory, visible when App is the chosen method).
// Splits "the App is registered" (the prior step) from "the App is actually
// installed somewhere" — the gate that was missing, which let users sail past a
// registered-but-uninstalled App into the repo picker that then dead-ends.
//
// Two Continue behaviours, on whether the App is staged (mid PAT→App switch):
//   - Fresh App (not staged): refresh-then-verify — reconcile the installation
//     mirror (the authoritative discovery path in local mode where webhooks
//     never arrive) and advance once ≥1 installation comes back.
//   - Staged App (the user had a PAT): CUTOVER — one call that reconciles,
//     verifies ≥1 installation, activates the App, and deletes the PAT
//     atomically. Its 409 ("install the App before switching") surfaces inline
//     exactly like the refresh path's "no installation yet" throw.
// Either way a no-installation state throws and the wizard stays on the step. A
// done step (installed AND not staged) short-circuits, mirroring the PAT/Jira
// access steps; a staged-but-installed App is NOT done until the cutover runs.
const githubAppInstallStep: WizardStep = {
  id: 'org-github-app-install',
  section: 'org',
  title: 'Install the App',
  visible: (s) => s.githubAccessTab === 'app',
  load: loadGitHubAppInstall,
  isComplete: (s) => s.githubAppInstalled && !s.githubAppStaged,
  persist: async ({ state, orgId, patch }) => {
    if (state.githubAppInstalled && !state.githubAppStaged) return
    if (!orgId) throw new Error('No organization context.')
    if (state.githubAppStaged) {
      // Commit the switch: cutover does refresh → verify → activate → delete
      // PAT in one call, throwing the backend's 409 if nothing's installed yet.
      await cutoverToApp(orgId)
      // The App is now the live credential and the PAT is gone. Re-read status
      // for an accurate install count (the cutover body doesn't carry it);
      // degrade to the loaded count (≥1, since cutover succeeded) on a read slip.
      const fresh = await getGitHubAppStatus(orgId).catch(() => null)
      const n = fresh?.installations.length ?? Math.max(state.githubAppInstallCount, 1)
      patch({
        githubAppStaged: false,
        githubAppInstalled: true,
        githubAppInstallCount: n,
        githubReady: true,
        hasGitHubPat: false,
      })
      return
    }
    const status = await refreshGitHubAppInstallations(orgId)
    const n = status.installations.length
    if (n === 0) {
      throw new Error(
        "We can't see the App installed on any account yet — install it on GitHub, then continue.",
      )
    }
    patch({ githubAppInstalled: true, githubAppInstallCount: n })
  },
  collapsedSummary: (s) =>
    s.githubAppInstalled
      ? `Installed on ${s.githubAppInstallCount} account${s.githubAppInstallCount === 1 ? '' : 's'}`
      : 'Not installed',
  render: (ctx) => <GitHubAppInstallStep {...ctx} />,
}

// Step · Personal access token (mandatory, visible when PAT is the chosen
// method). No separate Connect button — Continue performs the connect: a PUT on
// the org's GitHub credential resource, which validates the PAT against the
// base URL and 422s on a bad token. A freshly-entered token is trusted on a
// successful save; an empty field relied on a *stored* token, which the save
// does NOT re-validate, so confirm against the server's github_ready signal
// rather than optimistically marking connected.
//
// Cross-pick (an App is registered, user picked PAT to switch): Continue tears
// the App down instead of the plain settings save (which 409s while an App
// exists anyway). Two sub-cases, because in a STAGED switch the PAT is still the
// live credential:
//   - Staged App + live PAT, field blank → DISCARD the staged App (the PAT
//     stays). This is the "changed my mind mid-switch" exit — it must NOT force
//     re-typing a token the user may not have on hand.
//   - Otherwise (a live App, which by XOR has no PAT; or the user typed a new
//     token to rotate) → switch-to-pat, a full teardown that validates + stores
//     the supplied token.
// The body warns about the teardown; nothing is destroyed until Continue.
// `githubReady` alone can't gate completion here (an App makes it true), so the
// step is complete only once a PAT is live AND no App remains.
const githubPatStep: WizardStep = {
  id: 'org-github-pat',
  section: 'org',
  title: 'Personal access token',
  visible: (s) => s.githubAccessTab === 'pat',
  isComplete: (s) => s.githubReady && !s.githubAppRegistered,
  validate: (s) => {
    if (s.githubReady && !s.githubAppRegistered) return null
    if (s.githubAppRegistered) {
      // A staged App keeps the live PAT, so a blank field is valid — it discards
      // the staged App and keeps the current token. A live App has no PAT (XOR),
      // so switching off it requires a typed token.
      if (s.githubAppStaged && s.hasGitHubPat) return null
      return s.org.github_pat.trim() === ''
        ? 'Enter a personal access token to switch to PAT access.'
        : null
    }
    return !s.hasGitHubPat && s.org.github_pat.trim() === ''
      ? 'Enter a personal access token to connect.'
      : null
  },
  persist: async ({ state, orgId, patch }) => {
    if (state.githubReady && !state.githubAppRegistered) return
    // Cross-pick: tear down the App. Pre-repos in the wizard, so no reachability
    // diff — confirm-and-go.
    if (state.githubAppRegistered) {
      if (!orgId) throw new Error('No organization context.')
      const typed = state.org.github_pat.trim()
      // Blank is only valid for a staged App (validate blocks a live App + blank),
      // where the live PAT stays — so discard the staged App rather than re-enter
      // a token. A typed token rotates via the full teardown.
      if (typed === '') {
        await discardStagedApp(orgId)
      } else {
        await switchToPat(orgId, typed)
      }
      patch({
        githubReady: true,
        hasGitHubPat: true,
        githubAppRegistered: false,
        githubAppStaged: false,
        githubAppInstalled: false,
        githubAppInstallCount: 0,
        githubAppSlug: '',
        org: { ...state.org, github_pat: '' },
      })
      return
    }
    const typedPat = state.org.github_pat.trim()
    // A typed token is bound through the credential resource, which validates
    // it against the base URL and 422s on a bad one. A BLANK field means the
    // user is relying on an already-stored token — there's nothing to write, so
    // we don't write: confirm the stored credential still resolves and move on.
    // (Under the old bulk save, blank rode a "leave blank to keep current"
    // contract through the same request that could have rotated it.)
    if (typedPat === '') {
      const { githubReady } = await fetchIntegrationsState()
      if (!githubReady) {
        throw new Error('Couldn’t verify your stored GitHub token. Re-enter it to reconnect.')
      }
    } else {
      if (!orgId) throw new Error('No organization context.')
      // No host goes with the token: the GitHub URL step above already probed
      // and saved it, and the bind validates against that saved value. So this
      // writes no settings column and the concurrency token stays where the URL
      // step left it.
      const result = await connectGitHubPAT(orgId, typedPat)
      if (!result.ok) throw new Error(result.error)
    }
    // Local-mode convenience (the "use this token as my own GitHub identity too"
    // checkbox): reuse the just-connected org PAT to also bind the operator's own
    // identity (PAT_2), so a solo local operator doesn't paste the same token
    // again on the User step. Only when a token was actually typed this session.
    //
    // This marks ONLY the User-section step complete — it never touches org/team
    // step state, and wizard navigation is linear (advance → next visible step;
    // resume opens the first incomplete step; Finish needs every visible step), so
    // org/team onboarding is never short-circuited by it. Best-effort: a capture
    // failure must not fail the org connect (which already succeeded), so fall
    // back to carrying the token into the User step's draft and let that step
    // surface the real error.
    if (state.isLocal && state.duplicateGitHubToUser && typedPat !== '' && orgId) {
      try {
        const id = await captureGitHubIdentityPat(orgId, typedPat)
        patch({
          githubReady: true,
          hasGitHubPat: true,
          userIdentityConnected: true,
          userIdentityLogin: id.login,
          userIdentityHost: id.host,
          userGitHubPat: '',
          org: { ...state.org, github_pat: '' },
        })
        return
      } catch {
        toast.info("GitHub connected. We've pre-filled it on the last step — just hit Continue.")
        patch({
          githubReady: true,
          hasGitHubPat: true,
          userGitHubPat: typedPat,
          org: { ...state.org, github_pat: '' },
        })
        return
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
// to SSH); persist saves just the protocol. Switching to SSH triggers the
// backend's SSH preflight, which a default-SSH org never hits.
const githubCloneStep: WizardStep = {
  id: 'org-github-clone',
  section: 'org',
  title: 'Clone protocol',
  visible: (s) => s.githubAccessTab === 'pat' && s.isLocal,
  selfAdvancing: true,
  isComplete: () => true,
  persist: persistOrgFields('github_clone_protocol'),
  collapsedSummary: (s) => `Clone via ${s.org.github_clone_protocol.toUpperCase()}`,
  render: (ctx) => <GitHubCloneStep {...ctx} />,
}

// Step · GitHub poll interval. Sits right after the GitHub steps so each
// integration's connect + cadence stay together. Always satisfiable (a default
// exists), so it never blocks the stack; saveOrgSlice guards against saving
// when the org load failed. Renders the GitHub control alone (showJira=false).
const githubPollerStep: WizardStep = {
  id: 'org-github-poller',
  section: 'org',
  title: 'GitHub poll interval',
  isComplete: () => true,
  persist: persistOrgFields('github_poll_interval'),
  collapsedSummary: (s) => `GitHub every ${intervalLabel(s.org.github_poll_interval)}`,
  render: ({ state, patch }) => (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          How often should we poll GitHub?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          More frequent polling surfaces new PRs and reviews sooner; less frequent is lighter on
          rate limits.
        </p>
      </div>
      <PollerTimingGroup
        value={{
          github_poll_interval: state.org.github_poll_interval,
          jira_poll_interval: state.org.jira_poll_interval,
        }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        showJira={false}
        bare
      />
    </div>
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

// Step · Jira deployment (visible only when Jira is the chosen tracker). The
// Jira mirror of the GitHub mode picker: a self-advancing Cloud-vs-Data-Center
// choice that gates which credential the access step asks for. Made explicit
// (not sniffed from the URL) so the fields the user fills always match the
// scheme the connect sends. Complete once chosen — OR once connected, so a
// returning org (whose deployment loadOrg pre-resolves from the host) resumes
// past it.
const jiraModeStep: WizardStep = {
  id: 'org-jira-mode',
  section: 'org',
  title: 'Jira deployment',
  selfAdvancing: true,
  visible: (s) => s.tracker === 'jira',
  isComplete: (s) => s.jiraDeployment !== null || s.jiraConnected,
  persist: async () => {},
  collapsedSummary: (s) =>
    s.jiraDeployment === 'cloud'
      ? 'Atlassian Cloud'
      : s.jiraDeployment === 'data_center'
        ? 'Data Center / Server'
        : 'Not chosen',
  render: (ctx) => <JiraModeStep {...ctx} />,
}

// Step · Jira access (visible only when Jira is the chosen tracker). The Jira
// mirror of the GitHub PAT step: no separate Connect button — Continue performs
// the connect via the shared connectJira helper (PUT on the Jira credential
// resource, which
// validates the credential server-side and 4xxs on a bad one). The credential
// shape follows the deployment chosen in the prior step: Cloud sends an email +
// API token, Data Center a PAT. On success the step marks connected and
// advances; the disconnect half stays inside JiraAccessGroup. Mandatory while
// Jira is selected — its isComplete blocks finishing a half-connected Jira —
// but invisible (and so non-blocking) once the tracker is None.
const jiraAccessStep: WizardStep = {
  id: 'org-jira-access',
  section: 'org',
  title: 'Jira access',
  visible: (s) => s.tracker === 'jira',
  isComplete: (s) => s.jiraConnected,
  validate: (s) => {
    if (s.jiraConnected) return null
    // A disconnect (JiraAccessStep onDisconnected) clears jiraUrlConfirmed, so
    // reconnecting must re-probe the URL first — bounce Continue back to the
    // (now-incomplete) URL bar above rather than connecting against an
    // unconfirmed host.
    if (!s.jiraUrlConfirmed) return 'Re-confirm your Jira URL above to reconnect.'
    if (s.jiraDeployment === null) return 'Choose your Jira deployment above to continue.'
    if (s.jiraDeployment === 'cloud') {
      if (s.org.jira_email.trim() === '') return 'Enter your Atlassian account email to connect.'
      return s.org.jira_api_token.trim() === '' ? 'Enter an API token to connect.' : null
    }
    return s.org.jira_pat.trim() === '' ? 'Enter a personal access token to connect.' : null
  },
  persist: async ({ state, orgId, patch }) => {
    if (state.jiraConnected) return
    // jiraDeployment is non-null here (the mode step gates this one), but fall
    // back defensively so the connect never sends an undefined scheme.
    const deployment = state.jiraDeployment ?? 'data_center'
    // The just-typed org credential, captured before connectJira clears it, for
    // the local-mode reuse branch below. The shape follows the deployment: Cloud
    // reuses the email + API token, DC the PAT.
    const typedPat = state.org.jira_pat.trim()
    const typedEmail = state.org.jira_email.trim()
    const typedToken = state.org.jira_api_token.trim()
    if (!orgId) throw new Error('No organization context.')
    const result = await connectJira(orgId, state.org.jira_url, deployment, state.org)
    if (!result.ok) throw new Error(result.error)
    // The bind also persisted the Jira URL onto the settings row, so the
    // concurrency token moved — pick up the fresh one, or the very next org
    // save (the Jira poll interval) conflicts with this connect.
    const version = await freshOrgVersion(orgId, state.org.version)
    // Local-mode convenience — the Jira sibling of the GitHub PAT step's reuse
    // (see there for the rationale + the navigation-safety note). Reuse the
    // just-connected org Jira credential as the operator's own STORED Jira
    // credential, so they don't paste it again on the User step. Marks only the
    // User step; org/team flow is untouched. Best-effort: a capture failure never
    // fails the org connect — it pre-fills the User step drafts instead.
    const cloudReuse = deployment === 'cloud' && typedEmail !== '' && typedToken !== ''
    const dcReuse = deployment === 'data_center' && typedPat !== ''
    if (state.isLocal && state.duplicateJiraToUser && orgId && (cloudReuse || dcReuse)) {
      try {
        const id = cloudReuse
          ? await captureJiraIdentityApiToken(orgId, typedEmail, typedToken)
          : await captureJiraIdentityPat(orgId, typedPat)
        patch({
          jiraConnected: true,
          jiraUrlConfirmed: true,
          jiraUserConnected: true,
          jiraUserAccount: id.account,
          jiraUserHost: id.host,
          jiraUserPat: '',
          jiraUserEmail: '',
          jiraUserApiToken: '',
          org: { ...state.org, jira_pat: '', jira_email: '', jira_api_token: '', version },
        })
        return
      } catch {
        toast.info("Jira connected. We've pre-filled it on the last step — just hit Continue.")
        patch({
          jiraConnected: true,
          jiraUrlConfirmed: true,
          // Pre-fill the User step drafts in the deployment's shape so Continue
          // captures them there.
          ...(cloudReuse
            ? { jiraUserEmail: typedEmail, jiraUserApiToken: typedToken }
            : { jiraUserPat: typedPat }),
          org: { ...state.org, jira_pat: '', jira_email: '', jira_api_token: '', version },
        })
        return
      }
    }
    patch({
      jiraConnected: true,
      jiraUrlConfirmed: true,
      org: { ...state.org, jira_pat: '', jira_email: '', jira_api_token: '', version },
    })
  },
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
  persist: persistOrgFields('jira_poll_interval'),
  collapsedSummary: (s) => `Jira every ${intervalLabel(s.org.jira_poll_interval)}`,
  render: ({ state, patch }) => (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          How often should we poll Jira?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          The cadence for the Jira tracker — independent of the GitHub poll interval.
        </p>
      </div>
      <PollerTimingGroup
        value={{
          github_poll_interval: state.org.github_poll_interval,
          jira_poll_interval: state.org.jira_poll_interval,
        }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        showGitHub={false}
        bare
      />
    </div>
  ),
}

// Step · Background jobs model — the one model scoring, project classification
// and repo profiling all run on. Blocking, and that is the whole point in multi:
// nothing falls back, so an org that skips this has three background jobs that
// silently never run. It is not felt in local, where the setting arrives
// pre-filled and the step is complete on arrival — the mode difference travels
// as the seeded value, not as a branch here.
const orgBackgroundJobsModelStep: WizardStep = {
  id: 'org-background-jobs-model',
  section: 'org',
  title: 'Background jobs model',
  isComplete: (s) => s.org.background_jobs_model !== '',
  persist: async (ctx) => {
    await gateModelPick(ctx.orgId, ctx.state.org.background_jobs_model)
    await persistOrgFields('background_jobs_model')(ctx)
  },
  collapsedSummary: (s) =>
    s.org.background_jobs_model
      ? modelDisplayName(s.org.background_jobs_model)
      : 'No background jobs model',
  render: (ctx) => <OrgBackgroundJobsModelStep {...ctx} />,
}

// Step · Claude credential source (local only). The system-vs-BYOK picker, a
// self-advancing ChoiceCards (no Continue): "system" records the choice and its
// persist clears any stored key so the resolver falls back to the
// subscription/env path; "byok" advances to the key step. Hidden in multi —
// multi has no system-creds option (cross-tenant bleed), so it jumps straight to
// the key step. Always complete once a source is set (loadOrg seeds it), so it
// never blocks; the mandatory gate (in multi / local-BYOK) lives on the key
// step. Mirrors the github-mode picker's self-advancing shape.
const orgClaudeSourceStep: WizardStep = {
  id: 'org-claude-source',
  section: 'org',
  title: 'Claude credentials',
  selfAdvancing: true,
  visible: (s) => s.isLocal,
  isComplete: (s) => s.anthropicKeySource !== null,
  persist: async ({ state, patch, orgId }) => {
    // "byok" defers everything to the key step: the bind records the source
    // itself, because an org holding its own key is not running on the
    // machine's and nothing should ask it to say so twice.
    if (state.anthropicKeySource !== 'system') return
    if (!orgId) throw new Error('Settings didn’t load — retry before saving.')

    // "system" is a stored selection with a precondition. The removals come
    // first — two DELETEs, because they are two credentials and no single write
    // wipes both any more, and both idempotent when nothing is stored — because
    // the settings write refuses while any provider material is still bound.
    const removed = await disconnectLLM(orgId)
    if (!removed.ok) throw new Error(removed.error)
    // The removals cleared the key refs on the settings row, moving its
    // concurrency token — pick up the fresh one so this write, and a revisited
    // org step's save, don't conflict with them.
    const version = await freshOrgVersion(orgId, state.org.version)
    const saved = await patchOrgSettings(orgId, version, { llm_auth_method: 'system' })
    if (!saved.ok) throw new Error(saved.error)
    patch({
      anthropicConnected: false,
      bedrockConnected: false,
      bedrockStoredMethod: null,
      org: { ...state.org, anthropic_api_key: '', version: saved.settings.version },
    })
  },
  collapsedSummary: (s) =>
    s.anthropicKeySource === 'byok'
      ? 'Your Anthropic API key'
      : s.anthropicKeySource === 'system'
        ? 'System Claude Code credentials'
        : 'Not chosen',
  render: (ctx) => <OrgClaudeSourceStep {...ctx} />,
}

// bedrockFormError is the input-layer validation for the Bedrock credential
// form, shared by the wizard step's validate and the Settings section's Save
// gate so both surfaces block on the same rules (mirroring the backend's 400
// shapes): the region is always required, and so is the selected shape's own
// credential.
//
// There is no "leave blank to keep current" any more. Each shape's bind route
// REPLACES the credential resource, so a save that only means to change the
// region still re-enters the secret — the price of a blank field no longer
// meaning two different things on two neighbouring routes.
export function bedrockFormError(s: WizardState): string | null {
  const f = s.org
  if (f.bedrock_region.trim() === '') return 'Enter the AWS region (e.g. us-east-1).'
  // Role mode carries no secret — the ARN is its credential, and it must look
  // like an IAM role ARN. (The server does the real assume-role check on bind;
  // this is the cheap shape gate.)
  if (f.bedrock_auth_method === 'role') {
    const arn = f.bedrock_role_arn.trim()
    if (arn === '') return 'Enter the IAM role ARN Triage Factory should assume.'
    if (!/^arn:aws[a-z-]*:iam::\d{12}:role\/.+/.test(arn)) {
      return 'Enter a valid IAM role ARN (arn:aws:iam::123456789012:role/…).'
    }
    return null
  }
  if (f.bedrock_auth_method === 'bearer') {
    if (f.bedrock_bearer_token.trim() === '') {
      return 'Paste your Bedrock API key to continue.'
    }
    return null
  }
  const id = f.aws_access_key_id.trim()
  const secret = f.aws_secret_access_key.trim()
  if ((id === '') !== (secret === '')) {
    return 'Enter both the access key ID and the secret access key.'
  }
  if (id === '') {
    return 'Enter your AWS access key ID and secret access key.'
  }
  return null
}

// Step · Claude provider + credentials. Visible in multi always; in local only
// when BYOK was chosen (so "system" hides it). Continue drives the selected
// provider's validated connect endpoint (connectAnthropic / connectBedrock) and
// blocks advancing on failure (the error reddens the fields and shows in the
// host error line). Mandatory while visible: a run can't execute without a
// credential, so isComplete requires a validated+stored credential for the
// SELECTED provider — which blocks Finish in multi and local-BYOK until one is
// set. The picker chooses which credential to bind, not which one the org may
// hold: a connect leaves the other provider's stored material alone, so the
// summary names every provider that is connected. Mirrors the org-jira-access
// persist shape.
// connectedProviderLabels names every LLM provider the org currently holds a
// credential for. Both can be bound at once — binding one does not disturb the
// other — so a summary that named only the last one bound would be wrong.
function connectedProviderLabels(s: WizardState): string[] {
  return [
    s.anthropicConnected ? 'Anthropic' : '',
    s.bedrockConnected ? 'Amazon Bedrock' : '',
  ].filter(Boolean)
}

const orgClaudeKeyStep: WizardStep = {
  id: 'org-claude-key',
  section: 'org',
  title: 'Claude provider',
  visible: (s) => !s.isLocal || s.anthropicKeySource === 'byok',
  isComplete: (s) => (s.claudeProvider === 'bedrock' ? s.bedrockConnected : s.anthropicConnected),
  validate: (s) => {
    if (s.claudeProvider === 'bedrock') return bedrockFormError(s)
    // Already connected (resuming) with no new entry: valid — the stored key
    // stays ("leave blank to keep current").
    if (s.anthropicConnected && s.org.anthropic_api_key.trim() === '') return null
    return s.org.anthropic_api_key.trim() === ''
      ? 'Paste your Anthropic API key to continue.'
      : null
  },
  persist: async ({ state, patch, orgId }) => {
    if (!orgId) throw new Error('Settings didn’t load — retry before saving.')
    if (state.claudeProvider === 'bedrock') {
      const r = await connectBedrock(orgId, bedrockPayloadFromForm(state.org))
      if (!r.ok) throw new Error(r.error)
      // The credential is bound, so its models can now be tested — this is the
      // one moment the eager pass is offered. Declining is fine; the save gate
      // catches each model individually later.
      await offerSweepAfterConnect(orgId, 'bedrock', 'Amazon Bedrock')
      // Both binds persist their key ref onto the settings row, moving its
      // concurrency token — pick up the fresh one so a revisited org step's
      // save doesn't conflict with this write.
      const version = await freshOrgVersion(orgId, state.org.version)
      // An Anthropic key the org already holds is untouched by this bind: the
      // two providers coexist, and a run resolves whichever serves its model.
      patch({
        bedrockConnected: true,
        bedrockStoredMethod: state.org.bedrock_auth_method,
        org: {
          ...state.org,
          bedrock_bearer_token: '',
          aws_access_key_id: '',
          aws_secret_access_key: '',
          aws_session_token: '',
          version,
        },
      })
      return
    }
    const typed = state.org.anthropic_api_key.trim()
    // Resuming with a stored key and no new entry: nothing to do. The bind
    // requires a key, so there is no blank call to accidentally make.
    if (state.anthropicConnected && typed === '') return
    const r = await connectAnthropic(orgId, typed)
    if (!r.ok) throw new Error(r.error)
    await offerSweepAfterConnect(orgId, 'anthropic', 'Anthropic')
    const version = await freshOrgVersion(orgId, state.org.version)
    patch({
      anthropicConnected: true,
      org: { ...state.org, anthropic_api_key: '', version },
    })
  },
  collapsedSummary: (s) =>
    connectedProviderLabels(s).length
      ? `Connected · ${connectedProviderLabels(s).join(' + ')}`
      : 'Not connected',
  render: (ctx) => <OrgClaudeKeyStep {...ctx} />,
}

// Step 5 · Repositories (first team step). Embeds the shared RepoPickerModal
// inline (footer hidden — the wizard owns Continue/Back) and runs the single
// team load. Persisting the repo set writes `repositories`, which is BOTH half
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
        <p className="text-ui text-ink-3">
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
      bare
    />
  ),
}

// Step 7 · Jira projects. The shared JiraProjectRulesGroup — the watch picker
// plus the per-project pickup/in-progress/done status rules. Gated: omitted
// entirely unless a Jira tracker was configured (step 2). No load of its own —
// the rules came in with the team load. Optional twice over: zero watched
// projects is valid, and so is a watched project nobody has mapped yet, since
// mapping is the step after watching. Only a rule the server would reject
// blocks — members with no write target — which validate also catches.
const jiraProjectsStep: WizardStep = {
  id: 'team-jira-projects',
  section: 'team',
  title: 'Jira projects',
  visible: (s) => jiraActive(s),
  isComplete: (s) => !teamProjectsBlocked(s.team.jira_projects, s.jiraConnected),
  validate: (s) =>
    teamProjectsBlocked(s.team.jira_projects, s.jiraConnected)
      ? 'Finish or clear the half-mapped Jira status rule before continuing.'
      : null,
  persist: async ({ state, teamId }) => {
    // The rules are their own replace-set resource now, not a key inside the
    // team-settings body — so this step writes only what it owns, and a Jira
    // rule edit no longer re-saves the team's model and thresholds alongside it.
    if (!state.teamLoaded) {
      throw new Error(
        'Team settings didn’t load — reopen the Repositories step and retry before saving.',
      )
    }
    const result = await saveTeamJiraProjects(teamId, state.team.jira_projects)
    if (!result.ok) throw new Error(result.error)
  },
  collapsedSummary: (s) =>
    `Tracked Jira projects: ${s.team.jira_projects.filter((p) => p.key.trim() !== '').length}`,
  render: ({ state, patch }) => (
    <JiraProjectRulesGroup
      value={state.team.jira_projects}
      onChange={(jira_projects) => patch({ team: { ...state.team, jira_projects } })}
      connected={state.jiraConnected}
      bare
    />
  ),
}

// Step · Team default model. The shared ModelPicker, team-scoped — the model
// this team delegates with by default. No load of its own — reads the repos
// step's seeded team form; persistTeamSettings guards against saving when that
// load failed. isComplete requires a chosen model.
const teamModelStep: WizardStep = {
  id: 'team-model',
  section: 'team',
  title: 'Team default model',
  isComplete: (s) => s.team.default_model.trim() !== '',
  validate: (s) => (s.team.default_model.trim() === '' ? 'Choose a default model.' : null),
  persist: async ({ state, teamId, orgId }) => {
    await gateModelPick(orgId, state.team.default_model)
    const warning = await persistTeamSettings(teamId, state)
    if (warning) toast.info(warning)
  },
  collapsedSummary: (s) =>
    `Default model: ${s.team.default_model ? modelDisplayName(s.team.default_model) : 'Not chosen'}`,
  render: (ctx) => <TeamModelStep {...ctx} />,
}

// loadUserIdentity reads the caller's GitHub identity-binding status for the
// active org's host (the same endpoint the RequireGitHubIdentity gate polls).
// It seeds the User step: `connected` resumes it complete (a github.com login
// already has an identity via login_claim — zero extra clicks), `login`/`host`
// drive the copy, and `connect_available` chooses Connect vs. the PAT_2 paste.
// Throws on a hard read failure so the host shows a retry rather than wrongly
// prompting a connected user to reconnect.
export async function loadUserIdentity(ctx: LoadContext): Promise<Partial<WizardState>> {
  if (!ctx.orgId) throw new Error('No organization context for the GitHub identity check.')
  // Through apiClient so a stale-session 401 is routed to AuthContext; a hard
  // failure (HttpError / network) throws, and the host shows a retry rather
  // than wrongly prompting a connected user to reconnect.
  const data = await apiJSON<GitHubIdentityStatus>(
    `/api/orgs/${encodeURIComponent(ctx.orgId)}/github/identity`,
  )
  return {
    userIdentityConnected: data.connected,
    userIdentityLogin: data.login ?? '',
    userIdentityHost: data.host ?? '',
    userConnectAvailable: data.connect_available,
  }
}

// Step · Your GitHub identity (the sole User-section step). Captures the
// signed-in user's own @login on the org's host — PAT_2, independent of the
// org's PAT_1 credential. Connect (one click) when an App is registered, else a
// PAT_2 paste; either way the end state is a verified user_github_identities
// row with no token retained. The Connect button (in the body) is an external
// OAuth redirect that returns to /setup; the PAT path is this step's Continue,
// which validates the token, records the login, and discards it. Mandatory: the
// wizard can't finish until a host-verified identity exists — the in-flow mirror
// of the RequireGitHubIdentity gate.
const userIdentityStep: WizardStep = {
  id: 'user-github-identity',
  section: 'user',
  title: 'Your GitHub identity',
  advanceOnEnter: true,
  load: loadUserIdentity,
  isComplete: (s) => s.userIdentityConnected,
  validate: (s) =>
    s.userIdentityConnected || s.userGitHubPat.trim() !== ''
      ? null
      : 'Connect your GitHub, or paste a personal access token, to finish.',
  persist: async ({ state, orgId, patch }) => {
    // Already bound (Connect return, or a github.com login_claim) — nothing to
    // do; Continue just finishes.
    if (state.userIdentityConnected) return
    if (!orgId) throw new Error('No organization context.')
    const pat = state.userGitHubPat.trim()
    if (pat === '') {
      throw new Error('Connect your GitHub, or paste a personal access token, to finish.')
    }
    // Capture-and-discard: validates the token, reads the login, stores the
    // identity row, drops the token. On success mark connected + clear the draft.
    const result = await captureGitHubIdentityPat(orgId, pat)
    patch({
      userIdentityConnected: true,
      userIdentityLogin: result.login,
      userIdentityHost: result.host,
      userGitHubPat: '',
    })
  },
  collapsedSummary: (s) =>
    s.userIdentityConnected ? `GitHub: @${s.userIdentityLogin}` : 'Not connected',
  render: (ctx) => <UserIdentityStep {...ctx} />,
}

// loadJiraUserAccess reads the caller's Jira access-binding status for the
// active org's Jira host (the same endpoint the gate will poll). It seeds the
// Jira user step: `connected` resumes it complete (a stored credential from a
// prior session), `account`/`host` drive the copy. Throws on a hard read
// failure so the host shows a retry rather than wrongly prompting a connected
// user to reconnect.
export async function loadJiraUserAccess(ctx: LoadContext): Promise<Partial<WizardState>> {
  if (!ctx.orgId) throw new Error('No organization context for the Jira access check.')
  // Through apiClient so a stale-session 401 is routed to AuthContext; a hard
  // failure (HttpError / network) throws.
  const data = await apiJSON<JiraIdentityStatus>(
    `/api/orgs/${encodeURIComponent(ctx.orgId)}/jira/identity`,
  )
  return {
    jiraUserConnected: data.connected,
    jiraUserAccount: data.account ?? '',
    jiraUserHost: data.host ?? '',
    jiraUserConnectAvailable: data.connect_available,
    // Seed the deployment from the identity endpoint — the canonical source for
    // THIS step (it owns the user-access load). The org-connect load
    // (fetchIntegrationsState) seeds jiraDeployment too, but it fires in
    // parallel and can be null if the org isn't fully resolved; keying the
    // Cloud-vs-DC field choice off this step's own fetch removes that implicit
    // ordering dependency. Only overwrite on a recognized value.
    ...(data.deployment === 'cloud' || data.deployment === 'data_center'
      ? { jiraDeployment: data.deployment }
      : {}),
  }
}

// Step · Your Jira access (User section, shown only when Jira is the connected
// tracker). The Jira sibling of the GitHub identity step, with the structural
// difference that the token is STORED (Jira's user level holds access, not just
// identity). DC = paste-a-PAT; Cloud orgs with an Atlassian OAuth app configured
// get a one-click Connect button instead (see JiraUserAccessStep.tsx). The PAT
// path is this step's Continue, which validates the token, stores it, and
// derives the account. Mandatory while visible: the wizard can't finish a
// Jira-tracked workspace until the user has bound their own access.
const jiraUserAccessStep: WizardStep = {
  id: 'user-jira-access',
  section: 'user',
  title: 'Your Jira access',
  advanceOnEnter: true,
  visible: (s) => jiraActive(s),
  load: loadJiraUserAccess,
  isComplete: (s) => s.jiraUserConnected,
  validate: (s) => {
    if (s.jiraUserConnected) return null
    // The required fields follow the org's deployment: Cloud needs the email +
    // API token pair, Data Center the single PAT.
    if (s.jiraDeployment === 'cloud') {
      return s.jiraUserEmail.trim() !== '' && s.jiraUserApiToken.trim() !== ''
        ? null
        : 'Enter your Atlassian account email and API token to finish.'
    }
    return s.jiraUserPat.trim() !== '' ? null : 'Paste your personal Jira token to finish.'
  },
  persist: async ({ state, orgId, patch }) => {
    // Already bound (a stored credential) — nothing to do; Continue finishes.
    if (state.jiraUserConnected) return
    if (!orgId) throw new Error('No organization context.')
    // Capture-and-store: validates the credential, derives the account, persists
    // it. On success mark connected + clear the drafts. The credential shape
    // follows the org's deployment (Cloud = email + API token, DC = PAT).
    let result
    if (state.jiraDeployment === 'cloud') {
      const email = state.jiraUserEmail.trim()
      const token = state.jiraUserApiToken.trim()
      if (email === '' || token === '') {
        throw new Error('Enter your Atlassian account email and API token to finish.')
      }
      result = await captureJiraIdentityApiToken(orgId, email, token)
    } else {
      const pat = state.jiraUserPat.trim()
      if (pat === '') {
        throw new Error('Paste your personal Jira token to finish.')
      }
      result = await captureJiraIdentityPat(orgId, pat)
    }
    patch({
      jiraUserConnected: true,
      jiraUserAccount: result.account,
      jiraUserHost: result.host,
      jiraUserPat: '',
      jiraUserEmail: '',
      jiraUserApiToken: '',
    })
  },
  collapsedSummary: (s) => (s.jiraUserConnected ? `Jira: ${s.jiraUserAccount}` : 'Not connected'),
  render: (ctx) => <JiraUserAccessStep {...ctx} />,
}

// Ordered registry. Section grouping is derived from each step's `section`;
// the host inserts a divider above the first step of each section. The
// Jira-projects step is conditionally omitted via its `visible` predicate.
export const WIZARD_STEPS: WizardStep[] = [
  githubUrlStep,
  githubModeStep,
  githubAppSourceStep,
  githubAccountStep,
  githubAppStep,
  githubAppImportStep,
  githubAppInstallStep,
  githubPatStep,
  githubCloneStep,
  githubPollerStep,
  trackersStep,
  jiraUrlStep,
  jiraModeStep,
  jiraAccessStep,
  jiraPollerStep,
  // The credential steps come BEFORE the model picks, and that order is the
  // point: a picker asked first can only offer models the org may turn out to
  // have no way of running, and its availability badges have no credential to
  // be about. Connect first, then choose from what this organization can
  // actually reach.
  orgClaudeSourceStep,
  orgClaudeKeyStep,
  orgBackgroundJobsModelStep,
  reposStep,
  githubTeamsStep,
  jiraProjectsStep,
  teamModelStep,
  userIdentityStep,
  jiraUserAccessStep,
]
