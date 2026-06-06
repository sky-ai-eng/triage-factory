// Pure resume + navigation logic for the setup wizard. Kept free of React so
// the rules — "open on the first incomplete step", "skip steps that don't
// apply" — are small readable functions the host and controller call.
//
// A step may be conditionally omitted (WizardStep.visible). Resume, the
// all-complete tally, and Continue/Back navigation all operate over the
// *visible* subset so an omitted step (e.g. Jira projects without a Jira
// tracker) is never landed on, counted, or stepped through. Indices stay
// indices into the full array — visibility is a filter applied at each hop,
// not a renumbering — so a step keeps a stable identity as others appear or
// disappear around it.

import type { WizardState, WizardStep } from './types'

// isStepVisible reports whether a step applies under the current state. Absent
// predicate ⇒ always visible.
export function isStepVisible(step: WizardStep, state: WizardState): boolean {
  return step.visible ? step.visible(state) : true
}

// nextVisibleIndex / prevVisibleIndex return the nearest visible step index in
// the given direction from `from` (exclusive), or -1 when there is none.
export function nextVisibleIndex(steps: WizardStep[], state: WizardState, from: number): number {
  for (let i = from + 1; i < steps.length; i++) {
    if (isStepVisible(steps[i], state)) return i
  }
  return -1
}

export function prevVisibleIndex(steps: WizardStep[], state: WizardState, from: number): number {
  for (let i = from - 1; i >= 0; i--) {
    if (isStepVisible(steps[i], state)) return i
  }
  return -1
}

// resumeIndex picks the step the wizard opens on after its initial load: the
// first *visible* step whose isComplete() is false against the loaded state,
// with the rest pre-collapsed above it. When every visible step is already
// satisfied (a returning, fully-configured user), it opens on the LAST visible
// step — expanded, with the earlier steps collapsed above and Finish enabled —
// rather than a contentless terminal screen, so there is always exactly one
// active step to land focus on.
export function resumeIndex(steps: WizardStep[], state: WizardState): number {
  if (steps.length === 0) return 0
  for (let i = 0; i < steps.length; i++) {
    if (isStepVisible(steps[i], state) && !steps[i].isComplete(state)) return i
  }
  const last = prevVisibleIndex(steps, state, steps.length)
  return last === -1 ? 0 : last
}

// allComplete reports whether every *visible* step is satisfied. Drives the
// active step's primary action (Finish vs. Continue) — the wizard's own notion
// of "done", distinct from the backend setup_complete gate, which the real
// GitHub + repo steps satisfy. An omitted step never blocks finishing.
export function allComplete(steps: WizardStep[], state: WizardState): boolean {
  return steps.every((step) => !isStepVisible(step, state) || step.isComplete(state))
}
