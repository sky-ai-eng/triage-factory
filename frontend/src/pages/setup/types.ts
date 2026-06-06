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

// The two divider sections the stack groups steps under. Organization
// settings come first (org-wide config), then the first team's settings.
export type SectionId = 'org' | 'team'

export interface WizardSection {
  id: SectionId
  title: string
}

// Section order + labels. A divider renders above the first step of each
// section; a step declares which section it belongs to via WizardStep.section.
export const WIZARD_SECTIONS: WizardSection[] = [
  { id: 'org', title: 'Organization settings' },
  { id: 'team', title: 'Team settings (first team)' },
]

// The shared, mutable wizard state. Deliberately thin in the shell: the
// trivial steps only touch the model fields of the org/team config forms.
// Later tickets widen *usage* of these same slices (GitHub access, trackers,
// repos, …) rather than inventing parallel shapes, so the wizard keeps
// round-tripping the identical org_settings / team_settings forms the
// Settings page and the create-time pages already use.
export interface WizardState {
  org: OrgConfigForm
  // Carried so a re-typed-token step never clobbers a stored PAT: presence is
  // tracked separately from the (always-blank-on-load) org.github_pat field,
  // mirroring orgConfig's "leave blank to keep current" contract.
  hasGitHubPat: boolean
  team: TeamConfigForm
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

  // Is this step already satisfied by the current state? Drives both resume
  // (open on the first incomplete step) and the collapsed bar's check.
  isComplete: (state: WizardState) => boolean

  // Synchronous pre-persist validation. Return an error message to block
  // advancing and surface it inline, or null/undefined when valid.
  validate?: (state: WizardState) => string | null

  // Persist this step's slice. Rejecting blocks the advance and surfaces the
  // error inline; the user stays on the step.
  persist: (ctx: StepContext) => Promise<void>

  // Compact label for the collapsed confirmation bar (e.g. "Capped at Sonnet").
  collapsedSummary: (state: WizardState) => string

  // The expanded body — composes existing shared field groups.
  render: (ctx: StepContext) => ReactNode
}
