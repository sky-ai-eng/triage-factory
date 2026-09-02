// The setup-wizard step framework: the contract every step implements and
// the shared state the host threads through them. This is the *shell* — it
// defines the host/framework and ships a couple of trivial real steps to
// prove it; the real organization and team steps compose the existing shared
// field groups into this same contract in later tickets, with no bespoke
// per-step host plumbing.
//
// One continuous vertical stack, one route. The host owns step state, runs
// each step's load() on mount to seed from the server, computes the first
// incomplete step to resume on, and persists a step before advancing so a
// downstream step can read upstream state.

import type { ReactNode } from 'react'
import type { ClaudeCredentialSource, OrgConfigForm } from '../settings/orgConfig'
import type { JiraDeployment } from '../settings/jiraConnect'
import type { TeamConfigForm } from '../settings/teamConfig'
import type { GitHubAppStatus } from '../../lib/githubApp'

// The three divider sections the stack groups steps under, in order:
// org-wide config, then the first team's settings, then the signed-in user's
// own settings (their GitHub identity). Setup reads Org → Team → User.
export type SectionId = 'org' | 'team' | 'user'

export interface WizardSection {
  id: SectionId
  title: string
}

// Section order + labels. A divider renders above the first step of each
// section; a step declares which section it belongs to via WizardStep.section.
export const WIZARD_SECTIONS: WizardSection[] = [
  { id: 'org', title: 'Organization settings' },
  { id: 'team', title: 'Team settings (first team)' },
  { id: 'user', title: 'User settings' },
]

// The shared, mutable wizard state. Deliberately thin in the shell: the
// trivial steps only touch the model fields of the org/team config forms.
// Later tickets widen *usage* of these same slices (GitHub access, trackers,
// repos, …) rather than inventing parallel shapes, so the wizard keeps
// round-tripping the identical org_settings / team_settings forms the
// Settings page and the create-time pages already use.
// The tracker a workspace opts into at the org level — one at a time. "none"
// is a legitimate end state (GitHub-only); "linear" is a disabled
// "coming soon" placeholder (no poller, no credentials) so the value can be
// selected for display but never persists a connection.
export type TrackerKind = 'none' | 'jira' | 'linear'

// The GitHub access method an org connects with — its own picker step, gating
// the App vs PAT config steps. App is the default in both modes.
export type GitHubAccessMode = 'app' | 'pat'

// Which way into App mode the user takes: 'create' registers a brand-new App via
// GitHub's manifest flow; 'existing' connects an App that already exists by App
// ID + private key (bring-your-own-App). Gates the account-type/register steps
// vs the import step. Null until chosen.
export type GitHubAppSource = 'create' | 'existing'

// The LLM provider a BYOK org connects with — Anthropic direct or Amazon
// Bedrock. The picker chooses which credential this flow binds; an org may hold
// both at once, and which one a run authenticates with is decided by the run's
// model.
export type ClaudeProvider = 'anthropic' | 'bedrock'

export interface WizardState {
  org: OrgConfigForm
  // True once the org form has been seeded from the server (the GitHub step's
  // load). The org-form GET runs ONCE — on the GitHub step — and seeds shared
  // state for every org step; the later org steps (poller, model) read it
  // rather than re-fetching. This flag lets those steps' persist refuse to
  // write the empty default form if that single load failed, instead of
  // clobbering real settings with defaults.
  orgLoaded: boolean
  // Carried so a re-typed-token step never clobbers a stored PAT: presence is
  // tracked separately from the (always-blank-on-load) org.github_pat field,
  // mirroring orgConfig's "leave blank to keep current" contract.
  hasGitHubPat: boolean
  // The @login the stored org PAT authenticates as, when there is one — the
  // credential's own identity, not the operator's. Read-only context: Settings'
  // GitHub section names the account beside its "Replace token" control, so a
  // rotation says which bot it's swapping out. Empty in App mode (the App's bot
  // login resolves from the registration), before any bind, and whenever the
  // live token is env-supplied (see githubPatEnvProvided — the recorded login
  // describes the last token bound through a route, which isn't the one in use).
  githubPatLogin: string
  // The live GitHub token / Jira credential come from TRIAGE_FACTORY_* env vars
  // rather than the vault (local mode only). The overlay wins on read, so these
  // credentials can be reported but not managed: the surfaces that would
  // otherwise offer to replace them render a settled statement instead of a
  // control that would appear to work and then not.
  githubPatEnvProvided: boolean
  jiraCredentialEnvProvided: boolean
  // GitHub access is satisfied by ANY means — a stored/typed PAT or a
  // registered App — so the GitHub step reads the server's folded github_ready
  // signal rather than re-deriving it. Drives the step's isComplete (GitHub is
  // mandatory) independent of which access method the user picked.
  githubReady: boolean
  // The GitHub URL passed its reachability probe this session. The URL step
  // (now its own stack entry) sets it on a successful Continue; it satisfies
  // that step's isComplete so the access step can follow. A connected org
  // (githubReady) implies a previously-confirmed URL, so it's seeded from
  // githubReady on load.
  githubUrlConfirmed: boolean
  // Which access method is selected — App, PAT, or null (nothing chosen yet).
  // The mode picker step starts unselected (null) and advances on click; the
  // choice gates the App / PAT config steps' visibility, and the PAT step's
  // Continue (the connect) reads it. Seeded non-null for a returning org (PAT if
  // a token is stored, else App when already connected), null for a fresh org.
  githubAccessTab: GitHubAccessMode | null
  // The GitHub App owner type (personal vs org), chosen in its own step when App
  // is the method; fed into GitHubAppPanel's registration. Defaults to 'user'.
  githubAppOwnerType: 'user' | 'org'
  // Which way into App mode — create a new App (manifest flow) vs connect an
  // existing one (App ID + private key import). Its own picker step, gating the
  // account-type/register steps vs the import step. Null until chosen; a
  // returning org with a registered App leaves this null (its source step
  // resolves complete from the App's presence, not from this value).
  githubAppSource: GitHubAppSource | null
  // Whether the org has a registered GitHub App at all (staged OR live), from
  // the App status. Distinct from githubReady (a PAT also satisfies that) and
  // from the access tab (a registered App always resolves the tab to 'app').
  // Drives the mode-card status lines and the cross-pick confirm (picking PAT
  // while an App exists is a teardown, not a silent fork). Default false.
  githubAppRegistered: boolean
  // Whether the registered App is STAGED — registered during a PAT→App switch
  // but not yet cut over (org_github_apps.active=false), so the PAT is still the
  // live credential. Gates the install step's Continue between a plain
  // refresh-then-verify (fresh App) and a cutover (mid-switch). Default false.
  githubAppStaged: boolean
  // The registered App's slug, for the mode-card status line ("Registered
  // (slug) — …"). Empty when no App is registered. Default ''.
  githubAppSlug: string
  // The App status the panel would otherwise fetch for itself on mount. Seeded
  // by the App step's load(), with every other step's, so the panel renders its
  // final content on first paint. It matters here because the step animates
  // open: a body whose height changes partway through that is a body the
  // animation clips. Null until loaded, or if the fetch failed — the panel just
  // loads it itself in that case, as it does in Settings.
  githubAppStatus: GitHubAppStatus | null
  // Whether the org's GitHub access is the DEPLOYMENT's App — one App key the
  // operator registered for every workspace, which the org binds installations
  // to rather than registering an App of its own (multi mode only). It has no
  // App row (githubAppRegistered stays false) and never will; what it has is
  // bound installations, counted in githubAppInstallCount, of which zero is an
  // ordinary state that Settings has to render as one. Default false.
  githubAppManaged: boolean
  // Whether the org's registered App is installed on ≥1 GitHub account — the
  // gate for the "Install the App" step, distinct from githubReady (which
  // registration alone satisfies). Seeded from the installation mirror on load
  // and re-verified by the step's refresh-then-verify Continue. Default false.
  githubAppInstalled: boolean
  // How many accounts the App is installed on — drives the install step's
  // collapsed summary ("Installed on N account(s)"). Kept in lockstep with
  // githubAppInstalled. Default 0.
  githubAppInstallCount: number
  // Whether the wizard is running in local mode — seeded from the load context.
  // Gates the clone-protocol step (local-only; multi hardwires HTTPS).
  isLocal: boolean
  // Whether the org currently has a working Jira connection (PAT + base URL).
  // Gates the poller step's Jira interval and the team Jira-projects step.
  jiraConnected: boolean
  // The Jira URL passed its reachability probe this session — the Jira mirror
  // of githubUrlConfirmed, satisfying the Jira URL step so the Jira access step
  // can follow. Seeded from jiraConnected on load.
  jiraUrlConfirmed: boolean
  // Which Jira backend the org connects to — its own picker step (the Jira
  // mirror of githubAccessTab), gating which credential fields the access step
  // shows and which scheme the connect sends. Null until chosen; seeded for a
  // returning connected org from its stored host shape. Cloud authenticates
  // with an Atlassian API token (email + token), Data Center with a PAT.
  jiraDeployment: JiraDeployment | null
  // Which tracker the user selected in the Trackers step. Seeded from
  // jiraConnected on load (a connected org resumes on "Jira").
  tracker: TrackerKind

  // ── Org Claude credentials ──
  // How the org's delegated runs (and the scorer) authenticate with Claude —
  // the in-flow mirror of the stored llm_auth_method.
  //   'system' — the credentials already on the machine: a Claude Code
  //              subscription login, an exported ANTHROPIC_API_KEY, Bedrock or
  //              Vertex vars. TF stores none of its own (local only).
  //   'byok'   — the org's own bound provider material.
  //   null     — pre-load only; loadOrg seeds it from the stored value, never
  //              from whether a credential happens to be bound.
  // The source picker step is local-only — multi has no system-creds option
  // (cross-tenant bleed), and its stored value is always 'byok'.
  anthropicKeySource: ClaudeCredentialSource | null
  // Whether a validated org Anthropic key is stored — the in-flow mirror of
  // has_anthropic_api_key. Drives the key step's isComplete (mandatory in multi
  // and local-BYOK) and the "leave blank to keep current" guard on its persist.
  anthropicConnected: boolean
  // Which BYOK provider the key step edits — Anthropic direct or Amazon
  // Bedrock. Seeded to whichever is stored (Bedrock when
  // has_bedrock_credentials, else Anthropic); the providers are mutually
  // exclusive server-side, so connecting one flips the other's connected flag
  // off.
  claudeProvider: ClaudeProvider
  // Whether a Bedrock credential is stored — the in-flow mirror of
  // has_bedrock_credentials, the Bedrock sibling of anthropicConnected.
  bedrockConnected: boolean
  // The auth method of the STORED Bedrock credential (null when none). The
  // "leave blank to keep current" masking only arms when the form's selected
  // method matches this — switching methods always requires the new method's
  // secrets, mirroring the backend's keep-current rule. ('role' stores no
  // secret, so keep-current is moot there — but it's still the stored method.)
  bedrockStoredMethod: 'role' | 'bearer' | 'access_keys' | null
  team: TeamConfigForm
  // True once the team form has been seeded from the server (the Repositories
  // step's load). The team mirror of orgLoaded: the team GET runs ONCE — on the
  // first team step — and seeds shared state for every team step (GitHub teams,
  // Jira projects, default model) that round-trips the same team form. The flag
  // lets the later team steps' persist refuse to write the empty default form
  // when that single load failed, instead of clobbering real settings.
  teamLoaded: boolean

  // ── User settings (the third section) ──
  // The signed-in user's own GitHub identity on the org's host — captured as a
  // step distinct from the org credential (PAT_1). These seed from the
  // identity-status endpoint and drive the User step.
  //
  // Whether a host-verified login is already bound for the active org's host.
  // Satisfies the User step (the in-flow mirror of the RequireGitHubIdentity
  // gate). A github.com GoTrue login already has one (login_claim), so the step
  // resumes complete in that common case.
  userIdentityConnected: boolean
  // The bound login, when connected (for the confirmation + collapsed summary).
  userIdentityLogin: string
  // The org host the identity is keyed under (for the explanatory copy).
  userIdentityHost: string
  // Whether Connect (one-click OAuth) is offerable — true once the org has a
  // registered GitHub App. When false, the PAT_2 paste is the capture path.
  userConnectAvailable: boolean
  // The draft PAT_2 the user pastes when capturing identity by token. Captured
  // (validated → whoami → discarded) on the User step's Continue, then cleared.
  userGitHubPat: string

  // ── User settings · Jira access ──
  // The signed-in user's own Jira access on the org's host — the Jira sibling of
  // the userIdentity* slices, with the difference that the credential is STORED
  // (Jira's user level holds access, not just identity). These seed from the
  // Jira identity-status endpoint and drive the Jira user step (shown only when
  // Jira is the connected tracker).
  //
  // Whether a stored Jira credential already exists for the active org's host.
  // Satisfies the Jira user step.
  jiraUserConnected: boolean
  // The bound account (display name), when connected (for the confirmation +
  // collapsed summary).
  jiraUserAccount: string
  // The org's Jira host the credential is keyed under (for the explanatory copy).
  jiraUserHost: string
  // Whether one-click Connect is offerable for this org — true when an
  // Atlassian OAuth app resolves for it; false (DC, or no OAuth app
  // configured) means the step offers only the token path.
  jiraUserConnectAvailable: boolean
  // The draft token the user pastes when binding Jira access. For a Data Center
  // org this is the personal access token; captured (validated → stored →
  // account derived) on the Jira user step's Continue, then cleared.
  jiraUserPat: string
  // The draft Cloud credential pair the user pastes when the org's Jira is
  // Cloud — the Atlassian account email + API token (Basic auth). Captured the
  // same way as jiraUserPat and cleared on success. Unused for a Data Center org.
  jiraUserEmail: string
  jiraUserApiToken: string

  // ── Local-mode credential reuse (the setup-only "use this token as my own
  //    identity too" checkboxes) ──
  // When the operator connects the ORG GitHub PAT (PAT_1) in local mode, reuse
  // that same token to bind their OWN GitHub identity (PAT_2) — so a solo local
  // operator, whose org and personal token are almost always the same, doesn't
  // paste it twice. The org GitHub-PAT step's persist drives the existing
  // identity capture with the typed token on a successful connect; the
  // User-section identity step then resumes already-bound. Local-only (multi
  // keeps org creds non-user), a no-op when no token is typed, and it marks ONLY
  // the User step — never org/team steps — so it can't short-circuit onboarding.
  // Default mirrors isLocal (seeded in loadOrg); the checkbox lets the operator
  // opt out.
  duplicateGitHubToUser: boolean
  // The Jira sibling: reuse the org Jira PAT as the operator's own STORED Jira
  // access credential. Same local-only reuse, same default.
  duplicateJiraToUser: boolean
}

// Identity the host resolves once and threads to every step. The org/team
// settings endpoints are session-scoped (no id in the path), so the trivial
// steps don't need orgId to persist — but the GitHub App panel (a later org
// step) and finish-navigation do, and the team endpoints take the team alias
// ("default" resolves to the org's first team).
export interface WizardIdentity {
  orgId: string | null
  teamId: string
  isLocal: boolean
}

// What a step's load() receives. Same as the identity for now; kept as its
// own alias so a later step can widen the load inputs without touching every
// step signature.
export type LoadContext = WizardIdentity

// What render()/persist() receive: the identity plus the live state and a
// patcher. A step reads its slice off `state` and writes via `patch`.
export interface StepContext extends WizardIdentity {
  state: WizardState
  patch: (patch: Partial<WizardState>) => void
  // The active step's current inline error (a failed validate/persist), or
  // null. Threaded in for render only so a field can paint itself invalid (the
  // URL box turning red on a failed reachability probe) while the message
  // itself still renders once in the host's shared error line. Absent on the
  // persist path — persist surfaces errors by rejecting, not by reading this.
  error?: string | null
  // Advance the wizard (validate → persist → next). Threaded in for render only,
  // for a self-advancing step (the GitHub mode picker) whose in-body action
  // both records the choice and moves on — no Continue button. Absent on the
  // persist path.
  advance?: () => void
}

// The step contract. Trivial or real, every step implements this so the host
// drives them uniformly — no per-step branches in the stack, the resume
// logic, or the persistence loop.
export interface WizardStep {
  // Stable identity — used as the React key and the load-status map key.
  id: string
  section: SectionId
  // Heading shown when the step is the active (expanded) card.
  title: string

  // Seed wizard state from the server on mount. Optional (a step with no
  // server-backed state omits it). Throwing marks this step's load failed:
  // the host renders a retry in its place rather than risk persisting over
  // state it never read.
  load?: (ctx: LoadContext) => Promise<Partial<WizardState>>

  // Whether this step applies at all under the current state. A step whose
  // predicate returns false is omitted entirely — no collapsed bar, skipped by
  // resume and by Continue/Back navigation, and excluded from the "all steps
  // complete" tally — rather than shown as inert. Omitted (undefined) means
  // always visible. Evaluated live, so a step gated on an upstream choice (the
  // Jira-projects step on a connected Jira tracker) appears/disappears as that
  // choice changes mid-flow, without rebuilding the step list.
  visible?: (state: WizardState) => boolean

  // Is this step already satisfied by the current state? Drives both resume
  // (open on the first incomplete step) and the collapsed bar's check.
  isComplete: (state: WizardState) => boolean

  // Synchronous pre-persist validation. Return an error message to block
  // advancing and surface it inline, or null/undefined when valid.
  validate?: (state: WizardState) => string | null

  // Persist this step's slice. Rejecting blocks the advance and surfaces the
  // error inline; the user stays on the step.
  persist: (ctx: StepContext) => Promise<void>

  // When true, pressing Enter while focus is in a text input within the active
  // step triggers Continue (advance). Enabled on the URL steps, where the field
  // is the only input and Continue (the reachability probe) is the sole action;
  // left off where the step body owns a competing submit — the access steps'
  // Connect / Register — so Enter there doesn't fire the wrong action.
  advanceOnEnter?: boolean

  // When true, the host renders no Continue button — the step's in-body action
  // advances the flow itself (the GitHub mode picker: click App/PAT → move on).
  // Back is still shown so the step is escapable.
  selfAdvancing?: boolean

  // Compact label for the collapsed confirmation bar (e.g. "Capped at Sonnet").
  collapsedSummary: (state: WizardState) => string

  // The expanded body — composes existing shared field groups.
  render: (ctx: StepContext) => ReactNode
}
