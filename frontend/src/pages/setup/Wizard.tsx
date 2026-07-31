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
// The step owns its own Back / Continue — they are part of the card, not a
// toolbar bolted to the viewport. What holds still is where they LAND: the page
// scrolls itself so the active step's action row rests on the same line every
// time (ACTIONS_REST below), and the content flows around that. Left to itself
// the row walks down the page as you progress — the header, every collapsed bar
// that has receded above the active step, and the body's own height all stack
// up in front of it — until it is off the bottom of the viewport and you have
// to go looking for it.
//
// Accessibility is first-class, not a follow-up: Esc goes back, focus follows
// the active step as it expands, the collapsed stack stays a navigable list of
// buttons, an aria-live region announces step changes, and prefers-reduced-
// motion swaps the recede animation for an instant collapse.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { Check } from 'lucide-react'
import { AnimatePresence, cubicBezier, motion, useReducedMotion } from 'motion/react'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { WIZARD_SECTIONS, type WizardState } from './types'
import { WIZARD_STEPS, initialWizardState, jiraActive } from './steps'
import { useWizard } from './useWizard'
import { isStepVisible } from './resume'
import { CollapsedStepBar, SectionDivider } from './parts'
import { GlassBackdrop } from './glass'
import { bodyDurationMs, bodyEase, bodyEasePoints } from './glassStyle'

// Where the active step's action row comes to rest, as a fraction of viewport
// height measured to the row's bottom edge. The two spacers rendered around the
// flow exist purely so this line is reachable at both ends: an early step can't
// scroll UP to the line without room above it, and the last step can't scroll
// DOWN to it without room below.
//
// The value is a trade, not a taste: raising the line means a short window
// scrolls less to reach it (scrolling is what pushes the page header out of
// view on the first step) and a tall window needs more lead room above the
// flow. 0.78 keeps the row clear of the bottom edge while holding the first
// step's scroll to a few dozen pixels at laptop heights.
const ACTIONS_REST = 0.78
// How much of the active step's heading is protected when its body is too tall
// to honour the line — past this the step would open already scrolled past its
// own title.
const HEADING_GAP = 16
// How far a step body is offset along its direction of travel as it enters, and
// the reverse as it leaves. Small on purpose — it is a direction cue, not a
// slide; anything larger reads as the content being thrown around.
const BODY_TRAVEL = 8
// Frames past the end of the body animation to keep re-anchoring, covering the
// last layout the browser settles after the final animated frame.
const SETTLE_TAIL_MS = 80
// Fallback window for closing the self-scroll bracket when `scrollend` never
// arrives — an older browser, or a scrollTo that lands on the current offset and
// so fires nothing at all.
const SELF_SCROLL_MS = 150
// How long the action row takes to fade. Short: it leads the collapse out and
// follows the expand in, so it should be gone before the fold is underway and
// back promptly once everything has settled.
const ACTIONS_FADE_MS = 160
// The body animation's curve, for the scroll that tracks it on a backward move.
// Same points as bodyEase, so the two finish on the same frame.
const anchorEase = cubicBezier(...bodyEasePoints)

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

  const { back, advance, goTo, activeIndex, busy, canGoBack, steps, activeLoadFailed } = wiz

  // Every route between steps goes through these three, so `navigated` can't
  // miss one. It records that the user has moved — an event, not something to
  // re-derive in an effect — and that is what tells a newly mounted step body
  // whether to animate open (every step the user opens) or simply be there (the
  // step the wizard resumes on at first paint). Setting it true when it already
  // is bails out of the render, so it costs one extra render, once.
  const [navigated, setNavigated] = useState(false)
  // Which way the user is travelling, so a body can enter from the side it is
  // coming from — rising into place going forward, settling down from above
  // going back. Without it both directions play the identical expand and Back
  // reads as another step forward.
  const [travel, setTravel] = useState<'forward' | 'back'>('forward')
  // The step being left behind on a backward move. Steps render up to
  // `activeIndex`, so without this the departing <li> — and the AnimatePresence
  // inside it that owns the exit — unmount in the same frame, cutting the old
  // step instead of collapsing it. Held until its body has finished exiting.
  const [departing, setDeparting] = useState<number | null>(null)
  // A backward move runs in two beats rather than one. 'collapse' folds the
  // newer step away and brings the flow to where the returning step will sit
  // once it is open; 'expand' then opens it into the room already made. Null
  // outside a backward move, and once both beats are done.
  const [backPhase, setBackPhase] = useState<'collapse' | 'expand' | null>(null)
  // The action row belongs to the step at rest, not to the move between steps.
  // Raised by the navigation handlers so it leads the fold out, and lowered when
  // the transition loop finishes so it follows the expand back in. Opacity only:
  // the row's box is what the anchor measures against, so it has to keep its
  // place in the layout even while invisible.
  const [actionsHidden, setActionsHidden] = useState(false)

  const goBack = useCallback(() => {
    setNavigated(true)
    setTravel('back')
    setDeparting(activeIndex)
    setBackPhase('collapse')
    setActionsHidden(true)
    back()
  }, [back, activeIndex])
  const goNext = useCallback(() => {
    setNavigated(true)
    setTravel('forward')
    setDeparting(null)
    setBackPhase(null)
    setActionsHidden(true)
    advance()
  }, [advance])
  const goToStep = useCallback(
    (index: number) => {
      setNavigated(true)
      const backwards = index < activeIndex
      setTravel(backwards ? 'back' : 'forward')
      setDeparting(backwards ? activeIndex : null)
      setBackPhase(backwards ? 'collapse' : null)
      setActionsHidden(true)
      goTo(index)
    },
    [goTo, activeIndex],
  )

  // Keyboard mirrors of the footer buttons. Esc re-expands the previous step
  // (gated on canGoBack so it stays in lockstep with the Back button). Enter
  // triggers Continue, but only on a step that opts in (advanceOnEnter) and only
  // from a text input — so pressing it in the URL field probes + advances, while
  // it never hijacks the access steps' own Connect / Register buttons.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Mirror the Continue button's disabled condition (busy || activeLoadFailed)
      // so the keyboard path can't fire while a save is in flight or the active
      // step's load failed (showing the Retry UI in place of the fields).
      if (busy || activeLoadFailed) return
      if (e.key === 'Escape' && canGoBack) {
        e.preventDefault()
        goBack()
        return
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        const step = steps[activeIndex]
        const target = e.target as HTMLElement | null
        const inTextInput =
          target instanceof HTMLInputElement && target.type !== 'button' && target.type !== 'submit'
        if (step?.advanceOnEnter && inTextInput) {
          e.preventDefault()
          goNext()
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [goBack, goNext, canGoBack, busy, activeLoadFailed, steps, activeIndex])

  const headingRef = useRef<HTMLHeadingElement | null>(null)
  // The active step's Back / Continue row — what the page positions itself
  // around — and the lead spacer that buys room above the flow when the stack
  // is still too short to reach the line by scrolling alone.
  const actionsRef = useRef<HTMLDivElement | null>(null)
  // The active body's content, measured so the open animation has a target that
  // stays true. `height: 'auto'` is resolved to pixels ONCE, when the animation
  // starts, so content that lands during those few hundred milliseconds — a
  // status fetch returning, a list arriving — is not in the number the wrapper
  // is animating to, and overflow:hidden cuts it off until the animation ends
  // and height reverts to auto. Feeding a measured height instead re-targets
  // the animation in flight, so late content is simply included.
  const bodyInnerRef = useRef<HTMLDivElement | null>(null)
  const [openHeight, setOpenHeight] = useState<number | 'auto'>('auto')
  const leadRef = useRef<HTMLDivElement | null>(null)
  const contentRef = useRef<HTMLDivElement | null>(null)
  // Set once the user has taken the scroll over, cleared when the step changes:
  // from then on the re-anchoring below stops for this step, because yanking the
  // page out from under someone mid-read is worse than a slightly off line.
  const userScrolledRef = useRef(false)
  // How that is detected: bracket OUR scrolls rather than trying to enumerate
  // the user's. Every window.scrollTo below raises this flag, and any `scroll`
  // event arriving while it is down came from someone else. Listing input
  // devices instead would answer the wrong question — it catches wheel and
  // touch, and misses PageDown, space, Home/End, a scrollbar drag, and a screen
  // reader moving the viewport, all of which are exactly the reader this is
  // meant to protect.
  const selfScrollingRef = useRef(false)
  const selfScrollTimer = useRef(0)
  // True while a step change is interpolating the anchor. The content observer
  // stands down for the duration: it applies the anchor outright, which would
  // jump the flow to the destination the interpolation is still travelling to.
  const transitioningRef = useRef(false)

  // Where the flow wants to be right now: the lead spacer's height, and the
  // scroll offset that puts the action row on the line. Measuring is separate
  // from applying because a step change eases into this rather than snapping to
  // it. The spacer is zeroed for the measurement, so what we read is the flow's
  // natural position rather than the position the last pass produced — that is
  // what keeps this idempotent instead of creeping down the page.
  const measureAnchor = useCallback((lineShift = 0) => {
    const lead = leadRef.current
    // Always the active step's action row. Splitting a backward move into two
    // beats is what makes that work in both directions: nothing else is growing
    // while the newer step folds away, so there is no competing motion for the
    // anchor to cancel.
    const bottomOf = actionsRef.current
    if (!bottomOf) return null
    // lineShift is the room a not-yet-opened body will take. Aiming the row at
    // the line MINUS that room leaves exactly enough space below it for the
    // body to grow into, so the expand needs no scrolling of its own and the
    // row arrives on the line by itself.
    const line = window.innerHeight * ACTIONS_REST - lineShift

    const held = lead?.style.height ?? '0px'
    if (lead) lead.style.height = '0px'
    // Both reads happen inside the zeroed window so they share one frame of
    // reference; zeroing can clamp scrollY, and rect + scrollY cancel that out.
    const naturalBottom = bottomOf.getBoundingClientRect().bottom + window.scrollY
    const naturalHeadingTop = headingRef.current
      ? headingRef.current.getBoundingClientRect().top + window.scrollY
      : null
    if (lead) lead.style.height = held

    // Only the early steps need lead: until enough completed bars have stacked
    // up, the row sits above the line with nothing above it to scroll away.
    const leadHeight = Math.max(0, line - naturalBottom)
    let top = naturalBottom + leadHeight - line
    // A step too tall to honour the line would otherwise open already scrolled
    // past its own heading. Showing the heading wins; the row sits low and the
    // user scrolls the last stretch, which is the honest outcome for a step
    // that cannot fit.
    if (naturalHeadingTop !== null) {
      top = Math.min(top, naturalHeadingTop + leadHeight - HEADING_GAP)
    }
    return { top: Math.max(0, top), leadHeight }
  }, [])

  const applyAnchor = useCallback((top: number, leadHeight: number) => {
    if (leadRef.current) leadRef.current.style.height = `${leadHeight}px`
    // Raise the self-scroll flag before moving, so the `scroll` this produces
    // isn't mistaken for the user's. `scrollend` lowers it; the timer is the
    // fallback for browsers without it, and for a scrollTo that lands on the
    // current offset and so fires no event at all.
    selfScrollingRef.current = true
    window.clearTimeout(selfScrollTimer.current)
    selfScrollTimer.current = window.setTimeout(() => {
      selfScrollingRef.current = false
    }, SELF_SCROLL_MS)
    // Instant. Every caller either wants the flow put right immediately
    // (content arrived, viewport changed) or is already interpolating this
    // itself, frame by frame, against the body animation.
    window.scrollTo({ top: Math.max(0, top), behavior: 'auto' })
  }, [])

  const settle = useCallback(() => {
    const anchor = measureAnchor()
    if (anchor) applyAnchor(anchor.top, anchor.leadHeight)
  }, [measureAnchor, applyAnchor])

  // Whether the stack has painted once. This is what AnimatePresence's `initial`
  // wants: false for the resumed step on first paint (it should already be open,
  // not animate in from nothing), true for every step opened afterwards. It
  // cannot be the constant `false` it used to be — each step's AnimatePresence
  // lives inside that step's <li>, and forward navigation mounts that <li>
  // fresh, so its body is present on its own AnimatePresence's first render,
  // which is exactly the case a constant false suppresses. The result was an
  // expand that played going Back and never going forward: the arriving step
  // appeared at full height in one frame and the outgoing one drained out from
  // under it.
  // As a step becomes active, move focus to its heading and bring its actions to
  // the line. preventScroll matters: focus() scrolls on its own, and that scroll
  // would race the settle immediately after it.
  // Then hold it there for the length of the transition. A size observer alone
  // cannot do this: through a step change the two bodies' curves cancel, so the
  // document height barely moves while the row slides the full height of the
  // expanding body — most visibly on a backward move, where the step that grows
  // is ABOVE the row and the one that shrinks is below it, so nothing the
  // observer watches changes size at all. Re-anchoring every frame is what
  // actually pins the row; forward moves only looked right because their
  // geometry happened to cancel.
  useEffect(() => {
    if (wiz.phase !== 'ready') return
    userScrolledRef.current = false
    headingRef.current?.focus({ preventScroll: true })

    // Forward applies the anchor outright: the row is already where it belongs,
    // so there is nothing to ease and easing would only add travel.
    //
    // Back runs in two beats. The first folds the newer step away while easing
    // the flow to where the returning step's row will land once open — aimed at
    // the line minus the room its body still needs. The second lets the body
    // expand into exactly that room, with the scroll held still, so the row
    // descends onto the line under its own steam. Nothing has to be cancelled
    // against anything else, which is what made the single-beat version so
    // difficult to settle.
    const backward = travel === 'back'
    const startY = window.scrollY
    const startLead = leadRef.current?.offsetHeight ?? 0
    const t0 = performance.now()
    transitioningRef.current = true

    let frame = 0
    const tick = () => {
      if (userScrolledRef.current) {
        // The user has taken the scroll over, so stop moving it — but the step
        // still has to end up in a usable state. Leaving these set would strand
        // the flow mid-transition: the action row invisible and unclickable for
        // good, and the body's height frozen at a measured pixel value.
        transitioningRef.current = false
        setBackPhase(null)
        setActionsHidden(false)
        setOpenHeight('auto')
        return
      }
      const elapsed = performance.now() - t0
      // Re-measured every frame. Costs nothing when nothing changes — the inner
      // content is not itself animating, so this settles to one value and React
      // bails out on the rest.
      const inner = bodyInnerRef.current?.offsetHeight ?? 0
      if (inner) setOpenHeight(inner)

      const folding = backward && elapsed < bodyDurationMs
      if (folding) {
        // Beat one. Aim past the closed row by the room its body will need.
        const anchor = measureAnchor(inner)
        if (anchor) {
          const eased = reduce ? 1 : anchorEase(Math.min(1, elapsed / bodyDurationMs))
          applyAnchor(
            startY + (anchor.top - startY) * eased,
            startLead + (anchor.leadHeight - startLead) * eased,
          )
        }
      } else if (backward) {
        // Beat two. The room is already made; holding the scroll is what lets
        // the row descend into it rather than the page sliding out from under.
        // Re-applied rather than simply left alone: holding still is an action
        // here, because it keeps the self-scroll bracket alive. Without it the
        // bracket lapses mid-beat and the scroll events the expanding body
        // provokes get read as the user taking over — which stops the loop and
        // strands the row hidden.
        applyAnchor(window.scrollY, leadRef.current?.offsetHeight ?? 0)
        // Setting the same value bails out of the render, so calling this every
        // frame of the beat costs one.
        setBackPhase('expand')
      } else {
        const anchor = measureAnchor()
        if (anchor) applyAnchor(anchor.top, anchor.leadHeight)
      }

      const total = backward ? bodyDurationMs * 2 : bodyDurationMs
      if (elapsed < total + SETTLE_TAIL_MS) {
        frame = requestAnimationFrame(tick)
      } else {
        transitioningRef.current = false
        setBackPhase(null)
        setActionsHidden(false)
        // Correct any drift once everything has come to rest.
        settle()
        // Done animating — hand the height back to the browser so ordinary
        // reflows need no JS at all.
        setOpenHeight('auto')
      }
    }
    tick()
    return () => {
      cancelAnimationFrame(frame)
      transitioningRef.current = false
    }
  }, [wiz.phase, activeIndex, measureAnchor, applyAnchor, settle, reduce, travel])

  // Re-settle for as long as the flow's height is still moving: the outgoing
  // body collapsing, the incoming one expanding, and content that lands after
  // the fact (a repo list, an App install status, an error line). The one-shot
  // settle above necessarily measures a card that hasn't finished growing.
  // Observes the content only — the spacers are siblings, so resizing them
  // can't feed back in. Coalesced through rAF, so a burst of callbacks costs
  // one retarget.
  useEffect(() => {
    const content = contentRef.current
    if (wiz.phase !== 'ready' || !content) return
    let frame = 0
    const observer = new ResizeObserver(() => {
      if (userScrolledRef.current || transitioningRef.current) return
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(settle)
    })
    observer.observe(content)
    return () => {
      cancelAnimationFrame(frame)
      observer.disconnect()
    }
  }, [wiz.phase, settle])

  // Any scroll we did not cause is the user taking over — whatever moved it.
  // `scrollend` closes our bracket as soon as the browser reports the scroll
  // finished; the timer in settle() covers browsers that don't fire it. The
  // listeners are on the window because the page itself scrolls; the flow is
  // not inside a scroll container.
  useEffect(() => {
    const onScroll = () => {
      if (!selfScrollingRef.current) userScrolledRef.current = true
    }
    const onScrollEnd = () => {
      selfScrollingRef.current = false
    }
    window.addEventListener('scroll', onScroll, { passive: true })
    window.addEventListener('scrollend', onScrollEnd, { passive: true })
    return () => {
      window.removeEventListener('scroll', onScroll)
      window.removeEventListener('scrollend', onScrollEnd)
    }
  }, [])

  // A viewport change moves the line itself — it is a fraction of viewport
  // height — so the row has to be re-anchored against the new one. Without this
  // a phone rotated mid-step, or a window resized, holds the row against a line
  // that no longer exists until the next step change. Unlike the content-driven
  // re-anchors this one ignores the takeover flag and clears it: a resize
  // reflows the page out from under any reading position anyway, so putting the
  // row back on the line is the more useful answer than preserving an offset
  // that no longer means anything.
  useEffect(() => {
    if (wiz.phase !== 'ready') return
    let frame = 0
    const onResize = () => {
      userScrolledRef.current = false
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(settle)
    }
    window.addEventListener('resize', onResize)
    window.visualViewport?.addEventListener('resize', onResize)
    return () => {
      cancelAnimationFrame(frame)
      window.removeEventListener('resize', onResize)
      window.visualViewport?.removeEventListener('resize', onResize)
    }
  }, [wiz.phase, settle])

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
  // How far down the stack to render. Normally the active step; on a backward
  // move it also covers the step being left, so that step stays mounted long
  // enough to collapse rather than being cut.
  const renderThrough = Math.max(activeIndex, departing ?? -1)
  // How far a body is offset when it enters, and the opposite on the way out —
  // the cue that makes Back read as Back. Content rises into place when moving
  // forward and settles down from above when moving back.
  const enterOffset = travel === 'back' ? -BODY_TRAVEL : BODY_TRAVEL

  return (
    <div className="relative min-h-screen px-4 py-16">
      <GlassBackdrop />
      <div className="mx-auto max-w-xl">
        {/* Lead spacer — height is owned by settle(), not by CSS. Zero once the
            stack is tall enough to reach the line by scrolling. */}
        <div aria-hidden ref={leadRef} />
        <div ref={contentRef}>
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
              // below the active step is shown (no "road ahead"), so going Back
              // re-hides the forward steps. The one exception is the step being
              // left on a backward move, which renderThrough keeps mounted until
              // it has collapsed; dropping it on the same frame cut it instead. A
              // section with no reached steps yet (the team section while still in
              // org config) collapses away entirely.
              const entries = wiz.steps
                .map((step, index) => ({ step, index }))
                .filter(
                  ({ step, index }) =>
                    step.section === section.id &&
                    index <= renderThrough &&
                    isStepVisible(step, wiz.state),
                )
              if (entries.length === 0) return null
              return (
                <section key={section.id} aria-labelledby={`setup-section-${section.id}`}>
                  <SectionDivider id={`setup-section-${section.id}`} title={section.title} />
                  <ol className="relative space-y-5 pl-9">
                    {/* The faint thread the steps flow down; markers sit on it.
                      An aria-hidden, list-none <li> so the <ol> keeps only <li>
                      children (valid list semantics) while staying decorative. */}
                    <li
                      aria-hidden
                      className="pointer-events-none absolute bottom-3 left-[10px] top-2 w-px list-none bg-[var(--color-border-subtle)]"
                    />
                    {entries.map(({ step, index }) => {
                      const isActive = index === activeIndex
                      const complete = wiz.isStepComplete(index)
                      const n = String(displayNumber(step)).padStart(2, '0')
                      // Rendered past the active step, so this is the step being
                      // left on a backward move — on its way out, not staying as
                      // a bar. Its whole row goes with the body rather than
                      // bottoming out at the height of its own title and then
                      // being unmounted out from under the eye.
                      const isLeaving = index > activeIndex

                      // One <li> per step, persisting across the active↔collapsed
                      // transition so the always-mounted AnimatePresence can play
                      // the body's recede (exit) and expand (enter). The header
                      // swaps between the active heading and the collapsed bar; the
                      // body only mounts while active. Actions live OUTSIDE the
                      // height-animated body so the Continue glow is never clipped;
                      // the body can't move its inputs out, so instead it pads
                      // horizontally (px-1.5) and cancels the pad with -mx-1.5 — the
                      // fields stay aligned while their focus ring clears the
                      // overflow-hidden clip edge the height animation requires.
                      return (
                        <motion.li
                          key={step.id}
                          className="relative"
                          initial={false}
                          // Fading the <li> takes the gutter marker with it, which
                          // sits outside the flow and so cannot be collapsed; the
                          // margin goes too, or the stack's own spacing would be
                          // left behind to pop at unmount.
                          animate={isLeaving ? { opacity: 0, marginTop: 0 } : {}}
                          transition={reduce ? { duration: 0 } : bodyEase}
                        >
                          {/* Marker on the thread, in the left gutter (bg masks the
                            line behind the glyph). */}
                          <span
                            aria-hidden
                            className="absolute -left-9 top-px flex h-5 w-[21px] items-center justify-center bg-surface"
                          >
                            {complete && !isActive ? (
                              <Check
                                size={13}
                                strokeWidth={3}
                                className="text-[var(--color-claim)]"
                              />
                            ) : (
                              <span
                                className={`text-[11px] font-semibold tabular-nums ${
                                  isActive ? 'text-accent' : 'text-text-tertiary'
                                }`}
                              >
                                {n}
                              </span>
                            )}
                          </span>

                          {/* The title collapses with the body rather than
                              outliving it. Going back that is what stops the
                              departing step bottoming out at the height of its
                              own name; going forward it means the arriving step
                              unfolds whole, title and all, instead of the title
                              appearing a beat before its content. Pads and
                              cancels horizontally like the body, so the bar's
                              focus ring clears the clip the height needs. */}
                          <motion.div
                            initial={navigated && !reduce ? { height: 0, opacity: 0 } : false}
                            animate={
                              isLeaving ? { height: 0, opacity: 0 } : { height: 'auto', opacity: 1 }
                            }
                            transition={reduce ? { duration: 0 } : bodyEase}
                            className="-mx-1.5 px-1.5"
                            style={{ overflow: 'hidden' }}
                          >
                            {isActive ? (
                              <h3
                                ref={headingRef}
                                tabIndex={-1}
                                aria-current="step"
                                className="text-[12px] font-medium uppercase tracking-[0.12em] text-text-tertiary outline-none"
                              >
                                {step.title}
                              </h3>
                            ) : (
                              <CollapsedStepBar
                                title={step.title}
                                summary={step.collapsedSummary(wiz.state)}
                                complete={complete}
                                onEdit={() => goToStep(index)}
                              />
                            )}
                          </motion.div>

                          <AnimatePresence
                            initial={navigated}
                            onExitComplete={() => setDeparting(null)}
                          >
                            {isActive && (
                              <motion.div
                                key="body"
                                initial={
                                  reduce
                                    ? false
                                    : {
                                        height: 0,
                                        opacity: 0,
                                        filter: 'blur(10px)',
                                        y: enterOffset,
                                      }
                                }
                                // Shut for the first beat of a backward move —
                                // the newer step is still folding away and the
                                // flow is still travelling to meet this one.
                                // Opens on the second, into room already made.
                                animate={
                                  backPhase === 'collapse'
                                    ? {
                                        height: 0,
                                        opacity: 0,
                                        filter: 'blur(10px)',
                                        y: enterOffset,
                                      }
                                    : {
                                        height: openHeight,
                                        opacity: 1,
                                        filter: 'blur(0px)',
                                        y: 0,
                                      }
                                }
                                // No blur on the way out. It buys nothing on
                                // content that is already collapsing and fading,
                                // and blurring a layer whose height changes every
                                // frame repaints it every frame — the same cost
                                // the ambient backdrop is careful to avoid.
                                exit={
                                  reduce
                                    ? { opacity: 0 }
                                    : { height: 0, opacity: 0, y: -enterOffset }
                                }
                                transition={reduce ? { duration: 0 } : bodyEase}
                                className="-mx-1.5 px-1.5"
                                style={{ overflow: 'hidden' }}
                              >
                                <div ref={bodyInnerRef} className="space-y-6 pt-4">
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
                                      advance: goNext,
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
                                </div>
                              </motion.div>
                            )}
                          </AnimatePresence>

                          {isActive && (
                            <motion.div
                              ref={actionsRef}
                              initial={false}
                              animate={{ opacity: actionsHidden ? 0 : 1 }}
                              transition={
                                reduce ? { duration: 0 } : { duration: ACTIONS_FADE_MS / 1000 }
                              }
                              // Invisible must also mean unclickable. The row
                              // keeps its box either way — that box is what the
                              // anchor measures against.
                              style={{ pointerEvents: actionsHidden ? 'none' : 'auto' }}
                              aria-hidden={actionsHidden || undefined}
                              className="flex items-center justify-between gap-3 pt-5"
                            >
                              <button
                                type="button"
                                onClick={goBack}
                                disabled={!canGoBack || busy}
                                className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:cursor-not-allowed disabled:opacity-40"
                              >
                                Back
                              </button>
                              {/* A self-advancing step (the mode picker) advances
                                from its own in-body action — no Continue. */}
                              {!step.selfAdvancing && (
                                <button
                                  type="button"
                                  onClick={goNext}
                                  disabled={busy || wiz.activeLoadFailed}
                                  className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 hover:shadow-[0_12px_32px_-8px_var(--color-accent)] disabled:opacity-40 disabled:shadow-none"
                                >
                                  {busy ? 'Saving…' : wiz.isLastStep ? 'Finish setup' : 'Continue'}
                                </button>
                              )}
                            </motion.div>
                          )}
                        </motion.li>
                      )
                    })}
                  </ol>
                </section>
              )
            })}
          </div>
        </div>
        {/* Trailing spacer — the room the last step needs to scroll its action
            row up to the line. Sized as the slice of viewport that sits below
            the line, so it exactly covers the deepest scroll settle() asks for. */}
        <div aria-hidden style={{ height: `${Math.round((1 - ACTIONS_REST) * 100)}dvh` }} />
      </div>
    </div>
  )
}
