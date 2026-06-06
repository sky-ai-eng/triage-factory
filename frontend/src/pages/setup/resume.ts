// Pure resume logic for the setup wizard. Kept free of React so the rule —
// "open on the first incomplete step" — is a single readable function the
// host calls once loads settle.

import type { WizardState, WizardStep } from './types'

// resumeIndex picks the step the wizard opens on after its initial load: the
// first step whose isComplete() is false against the loaded state, with the
// rest pre-collapsed above it. When every step is already satisfied (a
// returning, fully-configured user), it opens on the LAST step — expanded,
// with the earlier steps collapsed above and Finish enabled — rather than a
// contentless terminal screen, so there is always exactly one active step to
// land focus on.
export function resumeIndex(steps: WizardStep[], state: WizardState): number {
  if (steps.length === 0) return 0
  const firstIncomplete = steps.findIndex((step) => !step.isComplete(state))
  return firstIncomplete === -1 ? steps.length - 1 : firstIncomplete
}

// allComplete reports whether every step is satisfied. Drives the active
// step's primary action (Finish vs. Continue) — the wizard's own notion of
// "done", distinct from the backend setup_complete gate, which the real
// GitHub + repo steps satisfy in later tickets.
export function allComplete(steps: WizardStep[], state: WizardState): boolean {
  return steps.every((step) => step.isComplete(state))
}
