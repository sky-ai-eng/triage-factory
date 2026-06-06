// The setup-wizard controller: owns step state, runs each step's load() on
// mount to seed from the server, resumes on the first incomplete step, and
// runs validate → persist → advance when the user continues. Kept as a hook
// so the host component stays declarative (it renders the view model this
// returns) and the stack/keyboard/focus concerns live there, not here.

import { useCallback, useEffect, useRef, useState } from 'react'
import {
  allComplete,
  isStepVisible,
  nextVisibleIndex,
  prevVisibleIndex,
  resumeIndex,
} from './resume'
import type { WizardIdentity, WizardState, WizardStep } from './types'

export interface WizardController {
  // 'loading' until every step's load() settles; then 'ready'.
  phase: 'loading' | 'ready'
  state: WizardState
  patch: (patch: Partial<WizardState>) => void
  activeIndex: number
  steps: WizardStep[]
  // Whether step i is satisfied by current state (drives the bar's check —
  // independent of position, so a completed step the user navigated back past
  // still shows a check).
  isStepComplete: (index: number) => boolean
  // A persist is in flight (Continue/Finish disabled).
  busy: boolean
  // Inline validate/persist error for the active step, or null.
  error: string | null
  // The active step's load() failed — the host shows a retry instead of the
  // fields, so we never persist over state we couldn't read.
  activeLoadFailed: boolean
  // Every step is satisfied — the active step's primary action becomes Finish.
  canFinish: boolean
  isLastStep: boolean
  // There is a visible step before the active one — drives the Back / Esc
  // affordance's enabled state. Keyed on "is there a previous visible step",
  // not the raw index, so a future omitted step 0 can't leave an
  // enabled-but-dead Back button (back() is a no-op with no prior visible step).
  canGoBack: boolean
  // Whether step i can be reopened by clicking its collapsed bar.
  canEdit: (index: number) => boolean
  // validate → persist the active step, then advance (or finish on the last).
  advance: () => void
  // Re-expand the previous step (the Back / Esc affordance).
  back: () => void
  // Reopen an already-done (or earlier) step to edit it.
  goTo: (index: number) => void
  // Re-run loads after a failure.
  retry: () => void
}

export function useWizard(
  steps: WizardStep[],
  identity: WizardIdentity,
  makeInitialState: () => WizardState,
  onFinish: () => void,
): WizardController {
  const { orgId, teamId, isLocal } = identity

  const [state, setState] = useState<WizardState>(makeInitialState)
  const [phase, setPhase] = useState<'loading' | 'ready'>('loading')
  const [activeIndex, setActiveIndex] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loadStatus, setLoadStatus] = useState<Record<string, 'ok' | 'error'>>({})
  // Bumped by retry() to re-run the load effect after a failure.
  const [reloadNonce, setReloadNonce] = useState(0)

  // Latest state for the stable goTo guard (it checks isComplete without
  // wanting to recreate on every keystroke).
  const stateRef = useRef(state)
  stateRef.current = state

  // Synchronous re-entrancy guard for advance(). setBusy(true) only lands on
  // the next render, so a fast double-click / Enter-repeat could fire advance()
  // twice (and start two persist() calls) before the button disables. The ref
  // blocks the second call immediately; it mirrors the `busy` state that drives
  // the disabled UI and gates back()/goTo() so navigation can't run mid-save.
  // Cleared in advance()'s finally.
  const busyRef = useRef(false)

  const patch = useCallback((p: Partial<WizardState>) => {
    setState((s) => ({ ...s, ...p }))
    // A field edit clears a stale persist/validation error for the step.
    setError(null)
  }, [])

  // Initial (and retry) load: run every step's load() in parallel, merge the
  // slices onto a fresh base, record per-step success, then resume on the
  // first incomplete step. Keyed on the resolved org/team so a switch
  // re-seeds, but NOT on every render (that would wipe in-progress edits).
  useEffect(() => {
    let cancelled = false
    setPhase('loading')
    const base = makeInitialState()
    const runs = steps.map((step) => {
      if (!step.load) {
        return Promise.resolve({ id: step.id, slice: {} as Partial<WizardState>, ok: true })
      }
      return step
        .load({ orgId, teamId, isLocal })
        .then((slice) => ({ id: step.id, slice, ok: true }))
        .catch(() => ({ id: step.id, slice: {} as Partial<WizardState>, ok: false }))
    })
    void Promise.all(runs).then((settled) => {
      if (cancelled) return
      const merged = settled.reduce<WizardState>((acc, r) => ({ ...acc, ...r.slice }), base)
      const nextStatus: Record<string, 'ok' | 'error'> = {}
      for (const r of settled) nextStatus[r.id] = r.ok ? 'ok' : 'error'
      setState(merged)
      setLoadStatus(nextStatus)
      setActiveIndex(resumeIndex(steps, merged))
      setError(null)
      setBusy(false)
      setPhase('ready')
    })
    return () => {
      cancelled = true
    }
  }, [steps, makeInitialState, orgId, teamId, isLocal, reloadNonce])

  const advance = useCallback(() => {
    const step = steps[activeIndex]
    if (!step || busyRef.current) return
    // Don't persist a step whose existing state we failed to read.
    if (loadStatus[step.id] === 'error') return
    const validationError = step.validate?.(state) ?? null
    if (validationError) {
      setError(validationError)
      return
    }
    busyRef.current = true
    setBusy(true)
    setError(null)
    void (async () => {
      try {
        await step.persist({ orgId, teamId, isLocal, state, patch })
        // Advance to the next step that applies; an omitted step (e.g. Jira
        // projects without a Jira tracker) is skipped. No visible step after
        // this one ⇒ this was the last step, so finish. Read visibility off
        // stateRef (not the captured `state`) in case persist patched something
        // that changes which steps apply — stateRef is never staler than the
        // closure, so this can't skip to or past the wrong step.
        const next = nextVisibleIndex(steps, stateRef.current, activeIndex)
        if (next === -1) {
          onFinish()
          return
        }
        setActiveIndex(next)
      } catch (e) {
        setError((e as Error)?.message || 'Could not save. Please try again.')
      } finally {
        busyRef.current = false
        setBusy(false)
      }
    })()
  }, [steps, activeIndex, loadStatus, state, orgId, teamId, isLocal, patch, onFinish])

  const back = useCallback(() => {
    // No navigating away from a step whose save is in flight.
    if (busyRef.current) return
    setError(null)
    // Re-expand the previous step that applies, skipping any omitted one.
    setActiveIndex((i) => {
      const prev = prevVisibleIndex(steps, stateRef.current, i)
      return prev === -1 ? i : prev
    })
  }, [steps])

  const goTo = useCallback(
    (i: number) => {
      // No navigating away from a step whose save is in flight.
      if (busyRef.current) return
      if (i < 0 || i >= steps.length) return
      // An omitted step has no collapsed bar to click, but guard anyway so a
      // stale index can never land on one.
      if (!isStepVisible(steps[i], stateRef.current)) return
      setError(null)
      setActiveIndex((cur) => {
        if (i === cur) return cur
        // Edit a step you've already reached (before the frontier) or one
        // that's complete; never skip ahead to an unfinished future step.
        if (i < cur) return i
        return steps[i]?.isComplete(stateRef.current) ? i : cur
      })
    },
    [steps],
  )

  const retry = useCallback(() => setReloadNonce((n) => n + 1), [])

  const isStepComplete = useCallback(
    (index: number) => !!steps[index]?.isComplete(state),
    [steps, state],
  )

  const canEdit = useCallback(
    (index: number) => index < activeIndex || !!steps[index]?.isComplete(state),
    [activeIndex, steps, state],
  )

  const activeStep = steps[activeIndex]
  return {
    phase,
    state,
    patch,
    activeIndex,
    steps,
    isStepComplete,
    busy,
    error,
    activeLoadFailed: !!activeStep && loadStatus[activeStep.id] === 'error',
    canFinish: allComplete(steps, state),
    // The last step is the one with no visible step after it — not necessarily
    // the final array slot, since the trailing region may include an omitted
    // step. This is what flips the primary action to "Finish".
    isLastStep: nextVisibleIndex(steps, state, activeIndex) === -1,
    canGoBack: prevVisibleIndex(steps, state, activeIndex) !== -1,
    canEdit,
    advance,
    back,
    goTo,
    retry,
  }
}
