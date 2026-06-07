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
  onFinish: (state: WizardState) => void,
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

  // The latest wizard state, readable synchronously — for the goTo/back guards
  // and for advance() after an await, where the render-bound `state` closure
  // can be stale. Kept current in two places: here on every render, AND
  // synchronously inside patch() (below). The patch path is what matters for
  // advance(): a useState setter doesn't run its updater synchronously at the
  // call site, so without the in-patch write a persist-time patch wouldn't be
  // visible until a render that hasn't happened yet.
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
    // Advance stateRef synchronously (not only on the next render) so a patch
    // made inside a step's persist() is observable the instant that await
    // resolves — see the stateRef note above. Merging off stateRef.current
    // (not React's `s`) chains correctly across batched patches, and feeding
    // the same object to setState keeps the ref and the rendered state
    // identical. Done here at the call site, not in a setState updater, since
    // updaters run during render (and double-run under StrictMode).
    const next = { ...stateRef.current, ...p }
    stateRef.current = next
    setState(next)
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
    // Read off stateRef, not the render-bound `state` closure: a selfAdvancing
    // step calls patch() then advance() in the same handler, so `state` here is
    // still pre-patch while stateRef.current already carries the choice. (For a
    // Continue-driven step the two are identical — stateRef is never staler.)
    const validationError = step.validate?.(stateRef.current) ?? null
    if (validationError) {
      setError(validationError)
      return
    }
    busyRef.current = true
    setBusy(true)
    setError(null)
    void (async () => {
      try {
        await step.persist({ orgId, teamId, isLocal, state: stateRef.current, patch })
        // Advance to the next step that applies; an omitted step (e.g. Jira
        // projects without a Jira tracker) is skipped. No visible step after
        // this one ⇒ this was the last step, so finish. Read visibility (and
        // the finish state below) off stateRef, which patch() keeps in sync
        // synchronously, so a persist that patched visibility- or finish-
        // affecting state (e.g. jiraConnected) is reflected here — the closure
        // `state` and a not-yet-flushed render would both still be stale.
        const next = nextVisibleIndex(steps, stateRef.current, activeIndex)
        if (next === -1) {
          // Pass the fresh ref so onFinish's finish branch (the local Jira
          // carry-over hand-off) sees the same post-persist state.
          onFinish(stateRef.current)
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
    // advance reads stateRef.current (kept in sync on every render + in patch),
    // not the `state` closure, so it needn't recreate on every keystroke.
  }, [steps, activeIndex, loadStatus, orgId, teamId, isLocal, patch, onFinish])

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
    advance,
    back,
    goTo,
    retry,
  }
}
