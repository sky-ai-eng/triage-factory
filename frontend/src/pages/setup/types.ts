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
import type { OrgConfigForm } from '../settings/orgConfig'
import type { TeamConfigForm } from '../settings/teamConfig'

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
  // Which tracker the user selected in the Trackers step. Seeded from
  // jiraConnected on load (a connected org resumes on "Jira").
  tracker: TrackerKind
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
  // Whether one-click Connect is offerable — false until Cloud OAuth lands
  // (DC = paste-a-PAT), so the step offers only the token path for now.
  jiraUserConnectAvailable: boolean
  // The draft token the user pastes when binding Jira access. Captured
  // (validated → stored → account derived) on the Jira user step's Continue,
  // then cleared.
  jiraUserPat: string
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
