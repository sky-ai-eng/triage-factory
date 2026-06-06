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

import { useCallback, useEffect, useMemo, useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { WIZARD_SECTIONS, type WizardState } from './types'
import { WIZARD_STEPS, initialWizardState, jiraActive } from './steps'
import { useWizard } from './useWizard'
import { isStepVisible } from './resume'
import { CollapsedStepBar, SectionDivider } from './parts'

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

  // Esc re-expands the previous step — the keyboard mirror of the Back button.
  const { back, advance, activeIndex, busy } = wiz
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && activeIndex > 0 && !busy) {
        e.preventDefault()
        back()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [back, activeIndex, busy])

  // As a step becomes active, move focus to its heading and bring the card to
  // center. scrollIntoView honors reduced motion (instant vs. smooth).
  const headingRef = useRef<HTMLHeadingElement | null>(null)
  const cardRef = useRef<HTMLLIElement | null>(null)
  useEffect(() => {
    if (wiz.phase !== 'ready') return
    headingRef.current?.focus()
    cardRef.current?.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'center' })
  }, [wiz.phase, activeIndex, reduce])

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
    <div className="min-h-screen bg-surface px-4 py-12">
      <div className="mx-auto max-w-2xl">
        <header className="mb-8 space-y-1.5">
          <h1 className="text-[22px] font-semibold tracking-tight text-text-primary">
            Set up Triage Factory
          </h1>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            A few steps to get your workspace ready. Each step saves as you go — you can change any
            of it later in Settings.
          </p>
        </header>

        <div aria-live="polite" className="sr-only">
          {announcement}
        </div>

        <div className="space-y-6">
          {WIZARD_SECTIONS.map((section) => {
            const entries = wiz.steps
              .map((step, index) => ({ step, index }))
              .filter(({ step }) => step.section === section.id && isStepVisible(step, wiz.state))
            if (entries.length === 0) return null
            return (
              <section key={section.id} aria-labelledby={`setup-section-${section.id}`}>
                <SectionDivider id={`setup-section-${section.id}`} title={section.title} />
                <ol className="space-y-2">
                  {entries.map(({ step, index }) => {
                    const isActive = index === activeIndex
                    const complete = wiz.isStepComplete(index)

                    // One <li> per step, persisting across the active↔collapsed
                    // transition so the always-mounted AnimatePresence can play
                    // the body's recede (exit) and expand (enter). The header
                    // swaps between the active heading and the collapsed bar; the
                    // body only mounts while active.
                    return (
                      <li
                        key={step.id}
                        ref={isActive ? cardRef : undefined}
                        className={
                          isActive
                            ? 'rounded-2xl border border-accent/30 bg-surface-raised shadow-sm shadow-black/[0.04]'
                            : undefined
                        }
                      >
                        {isActive ? (
                          <div className="flex items-center gap-2.5 px-5 pt-4">
                            <span className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-accent text-[10px] font-semibold text-white">
                              {displayNumber(step)}
                            </span>
                            <h3
                              ref={headingRef}
                              tabIndex={-1}
                              aria-current="step"
                              className="text-[14px] font-semibold text-text-primary outline-none"
                            >
                              {step.title}
                            </h3>
                          </div>
                        ) : (
                          <CollapsedStepBar
                            number={displayNumber(step)}
                            title={step.title}
                            summary={step.collapsedSummary(wiz.state)}
                            complete={complete}
                            editable={wiz.canEdit(index)}
                            onEdit={() => wiz.goTo(index)}
                          />
                        )}

                        <AnimatePresence initial={false}>
                          {isActive && (
                            <motion.div
                              key="body"
                              initial={reduce ? false : { height: 0, opacity: 0 }}
                              animate={{ height: 'auto', opacity: 1 }}
                              exit={reduce ? { opacity: 0 } : { height: 0, opacity: 0 }}
                              transition={{ duration: reduce ? 0 : 0.25, ease: 'easeOut' }}
                              style={{ overflow: 'hidden' }}
                            >
                              <div className="space-y-4 px-5 pb-5 pt-3">
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
                                  step.render({ ...identity, state: wiz.state, patch: wiz.patch })
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
                                    disabled={activeIndex === 0 || busy}
                                    className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:cursor-not-allowed disabled:opacity-40"
                                  >
                                    Back
                                  </button>
                                  <button
                                    type="button"
                                    onClick={advance}
                                    disabled={busy || wiz.activeLoadFailed}
                                    className="rounded-xl bg-accent px-5 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:opacity-40"
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
