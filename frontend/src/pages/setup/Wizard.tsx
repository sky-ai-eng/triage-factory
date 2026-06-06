// The setup wizard host: the single `/setup` route. One continuous vertical
// stack — the active step is an expanded card, completed steps recede upward
// into compact confirmation bars, upcoming steps wait as faint rows — grouped
// under two dividers ("Organization settings" / "Team settings (first team)").
//
// This is the *shell* (the framework + a couple of trivial real steps). The
// real organization/team steps compose existing shared field groups into the
// same step contract in later tickets. The mandatory-setup gate
// (RequireSetupComplete) redirects incomplete users here; the wizard derives
// per-step completion client-side from the GETs it already makes and resumes
// on the first incomplete step — no setup_step widening needed.
//
// Accessibility is first-class, not a follow-up: Esc goes back, focus follows
// the active step as it expands, the collapsed stack stays a navigable list of
// buttons, an aria-live region announces step changes, and prefers-reduced-
// motion swaps the recede animation for an instant collapse.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { WIZARD_SECTIONS, type WizardState } from './types'
import { WIZARD_STEPS, initialWizardState, jiraActive } from './steps'
import { useWizard } from './useWizard'
import { isStepVisible } from './resume'
import { CollapsedStepBar, SectionDivider } from './parts'
import { GlassBackdrop } from './glass'
import { bodyEase } from './glassStyle'

function Loading() {
  return (
    <div className="min-h-screen bg-surface flex items-center justify-center">
      <p className="text-text-tertiary text-sm">Loading…</p>
    </div>
  )
}

/**
 * `isLocal` resolves the org id (local has no OrgContext, so it uses the
 * sentinel) and the finish destination (local's app is the flat root; multi's
 * is org-scoped). The wizard is mounted outside the setup gate in both modes,
 * so an incomplete founder can actually reach it without looping.
 */
export default function Wizard({ isLocal = false }: { isLocal?: boolean }) {
  const navigate = useNavigate()
  const ctxOrgId = useActiveOrgId()
  const orgId = isLocal ? LOCAL_DEFAULT_ORG_ID : ctxOrgId
  const reduce = !!useReducedMotion()

  // Team endpoints take the "default" alias (resolves to the org's first
  // team), so the wizard never needs the team UUID up front.
  const identity = useMemo(() => ({ orgId, teamId: 'default', isLocal }), [orgId, isLocal])

  const onFinish = useCallback(
    (state: WizardState) => {
      // Local first-run preserves the Jira carry-over step: when Jira is the
      // ACTIVE tracker (chosen in the Trackers step AND connected), finishing
      // lands on the carry-over deck (the migrated final local first-run step)
      // before the app. Keyed on jiraActive — not bare jiraConnected — so a
      // user who connects Jira then switches the picker back to None isn't sent
      // there against their choice (same test that gates the Jira-projects
      // step). A GitHub-only local install skips it, and multi has no carry-over
      // step yet — both go straight in. In local mode orgId is always the
      // sentinel, so route to it directly rather than gating on the
      // (string | null) orgId.
      if (isLocal && jiraActive(state)) {
        navigate(`/orgs/${LOCAL_DEFAULT_ORG_ID}/carry-over`, { replace: true })
        return
      }
      navigate(isLocal ? '/' : orgId ? `/orgs/${orgId}` : '/', { replace: true })
    },
    [navigate, isLocal, orgId],
  )

  const wiz = useWizard(WIZARD_STEPS, identity, initialWizardState, onFinish)

  // Keyboard mirrors of the footer buttons. Esc re-expands the previous step
  // (gated on canGoBack so it stays in lockstep with the Back button). Enter
  // triggers Continue, but only on a step that opts in (advanceOnEnter) and only
  // from a text input — so pressing it in the URL field probes + advances, while
  // it never hijacks the access steps' own Connect / Register buttons.
  const { back, advance, activeIndex, busy, canGoBack, steps, activeLoadFailed } = wiz
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Mirror the Continue button's disabled condition (busy || activeLoadFailed)
      // so the keyboard path can't fire while a save is in flight or the active
      // step's load failed (showing the Retry UI in place of the fields).
      if (busy || activeLoadFailed) return
      if (e.key === 'Escape' && canGoBack) {
        e.preventDefault()
        back()
        return
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        const step = steps[activeIndex]
        const target = e.target as HTMLElement | null
        const inTextInput =
          target instanceof HTMLInputElement && target.type !== 'button' && target.type !== 'submit'
        if (step?.advanceOnEnter && inTextInput) {
          e.preventDefault()
          advance()
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [back, advance, canGoBack, busy, activeLoadFailed, steps, activeIndex])

  // As a step becomes active, move focus to its heading and bring the card to
  // center. scrollIntoView honors reduced motion (instant vs. smooth).
  const headingRef = useRef<HTMLHeadingElement | null>(null)
  const cardRef = useRef<HTMLLIElement | null>(null)
  useEffect(() => {
    if (wiz.phase !== 'ready') return
    headingRef.current?.focus()
    cardRef.current?.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'center' })
  }, [wiz.phase, activeIndex, reduce])

  // The body wrapper needs overflow:hidden while its height animates (0 → auto),
  // but at rest that clip box hugs the flush content and crops the Continue
  // pill's glow at the bottom-right. So clip only during the expand, then let
  // overflow go visible once settled. Derived (not reset via an effect): the
  // body is settled only for the step whose enter animation last completed, so a
  // new active step reads unsettled until its own animation finishes. Reduced
  // motion has no expand animation, so overflow is visible immediately below.
  const [settledIndex, setSettledIndex] = useState<number | null>(null)
  const bodySettled = settledIndex === activeIndex

  if (wiz.phase === 'loading') return <Loading />

  // Number and count by the steps that currently apply, so an omitted step
  // (e.g. Jira projects without a Jira tracker) leaves no gap in the sequence.
  // `displayNumber` maps a full-array index to its 1-based position among the
  // visible steps.
  const visibleSteps = wiz.steps.filter((step) => isStepVisible(step, wiz.state))
  const total = visibleSteps.length
  const displayNumber = (step: (typeof wiz.steps)[number]) => visibleSteps.indexOf(step) + 1
  const activeStep = wiz.steps[activeIndex]
  const announcement = activeStep
    ? `Step ${displayNumber(activeStep)} of ${total}: ${activeStep.title}${wiz.canFinish ? ' (all steps complete)' : ''}`
    : ''

  return (
    <div className="relative min-h-screen px-4 py-16">
      <GlassBackdrop />
      <div className="mx-auto max-w-xl">
        <header className="mb-10 space-y-2">
          <h1 className="text-[27px] font-semibold tracking-tight text-text-primary">
            Set up Triage Factory
          </h1>
          <p className="text-[14px] leading-relaxed text-text-tertiary">
            A few steps to get your workspace ready. Each one saves as you go.
          </p>
        </header>

        <div aria-live="polite" className="sr-only">
          {announcement}
        </div>

        <div className="space-y-6">
          {WIZARD_SECTIONS.map((section) => {
            // Only steps up to and including the active one render — the active
            // card plus the completed bars that have receded above it. Nothing
            // below the active step is shown (no "road ahead"), and because this
            // keys on activeIndex, going Back re-hides the forward steps too. A
            // section with no reached steps yet (the team section while still in
            // org config) collapses away entirely.
            const entries = wiz.steps
              .map((step, index) => ({ step, index }))
              .filter(
                ({ step, index }) =>
                  step.section === section.id &&
                  index <= activeIndex &&
                  isStepVisible(step, wiz.state),
              )
            if (entries.length === 0) return null
            return (
              <section key={section.id} aria-labelledby={`setup-section-${section.id}`}>
                <SectionDivider id={`setup-section-${section.id}`} title={section.title} />
                <ol className="space-y-4">
                  {entries.map(({ step, index }) => {
                    const isActive = index === activeIndex
                    const complete = wiz.isStepComplete(index)

                    // One <li> per step, persisting across the active↔collapsed
                    // transition so the always-mounted AnimatePresence can play
                    // the body's recede (exit) and expand (enter). The header
                    // swaps between the active heading and the collapsed bar; the
                    // body only mounts while active.
                    return (
                      <li key={step.id} ref={isActive ? cardRef : undefined}>
                        {isActive ? (
                          <div className="flex items-center gap-2.5">
                            <span className="text-[11px] font-semibold tabular-nums text-accent">
                              {String(displayNumber(step)).padStart(2, '0')}
                            </span>
                            <motion.h3
                              layoutId={`setup-title-${step.id}`}
                              ref={headingRef}
                              tabIndex={-1}
                              aria-current="step"
                              className="text-[12px] font-medium uppercase tracking-[0.12em] text-text-tertiary outline-none"
                            >
                              {step.title}
                            </motion.h3>
                          </div>
                        ) : (
                          <CollapsedStepBar
                            id={step.id}
                            number={displayNumber(step)}
                            title={step.title}
                            summary={step.collapsedSummary(wiz.state)}
                            complete={complete}
                            onEdit={() => wiz.goTo(index)}
                          />
                        )}

                        <AnimatePresence initial={false}>
                          {isActive && (
                            <motion.div
                              key="body"
                              initial={
                                reduce ? false : { height: 0, opacity: 0, filter: 'blur(10px)' }
                              }
                              animate={{ height: 'auto', opacity: 1, filter: 'blur(0px)' }}
                              exit={
                                reduce
                                  ? { opacity: 0 }
                                  : { height: 0, opacity: 0, filter: 'blur(6px)' }
                              }
                              transition={reduce ? { duration: 0 } : bodyEase}
                              onAnimationComplete={() => setSettledIndex(activeIndex)}
                              style={{ overflow: reduce || bodySettled ? 'visible' : 'hidden' }}
                            >
                              <div className="space-y-6 pt-4">
                                {wiz.activeLoadFailed ? (
                                  <div className="space-y-3">
                                    <p className="text-[13px] text-text-secondary">
                                      We couldn&rsquo;t load your current settings for this step.
                                      Retry before saving so nothing is overwritten.
                                    </p>
                                    <button
                                      type="button"
                                      onClick={wiz.retry}
                                      className="rounded-lg border border-border-subtle bg-white/50 px-3 py-1.5 text-[12px] font-medium text-text-secondary transition-colors hover:bg-white/80"
                                    >
                                      Retry
                                    </button>
                                  </div>
                                ) : (
                                  step.render({
                                    ...identity,
                                    state: wiz.state,
                                    patch: wiz.patch,
                                    error: wiz.error,
                                  })
                                )}

                                {wiz.error && (
                                  <p
                                    role="alert"
                                    className="text-[12px] text-[var(--color-dismiss)]"
                                  >
                                    {wiz.error}
                                  </p>
                                )}

                                <div className="flex items-center justify-between gap-3 pt-1">
                                  <button
                                    type="button"
                                    onClick={back}
                                    disabled={!canGoBack || busy}
                                    className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:cursor-not-allowed disabled:opacity-40"
                                  >
                                    Back
                                  </button>
                                  <button
                                    type="button"
                                    onClick={advance}
                                    disabled={busy || wiz.activeLoadFailed}
                                    className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 hover:shadow-[0_12px_32px_-8px_var(--color-accent)] disabled:opacity-40 disabled:shadow-none"
                                  >
                                    {busy
                                      ? 'Saving…'
                                      : wiz.isLastStep
                                        ? 'Finish setup'
                                        : 'Continue'}
                                  </button>
                                </div>
                              </div>
                            </motion.div>
                          )}
                        </AnimatePresence>
                      </li>
                    )
                  })}
                </ol>
              </section>
            )
          })}
        </div>
      </div>
    </div>
  )
}
