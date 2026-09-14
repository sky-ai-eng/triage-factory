import { useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { Tooltip } from '../../ui/tooltip/Tooltip'
import { ActivityRow } from './ActivityRow'
import { PermissionRow } from './PermissionRow'
import { ArtifactStrip } from './ArtifactStrip'
import { Glyph, type GlyphKind } from './Glyph'
import { CrateMark } from './CrateMark'
import {
  pendingCount,
  type ArtifactTotals,
  type EventTone,
  type Lifecycle,
  type PendingTotals,
} from './cardModel'
import './task-card.css'

// TaskCard — the board's atom, and the only card on it.
//
// A task is the thing that persists; a run is one attempt at it. There is no
// second component for a queued card and no kind on the data: what a card
// shows follows from what it HAS (cardModel.ts derives that). No elapsed, no
// activity and no result is what queued MEANS, and every run-shaped element is
// absent for exactly the reason its data is.
//
// The SUMMARY belongs to neither. It says what the work is, in the scorer's
// own words, and it shows in every lifecycle, beside whatever the run is
// producing right now: the reader scanning a lane of working cards wants to
// know what each one is about, and the live line under it says what the
// agent is doing about it. Two paragraphs on a card is the accepted cost.
//
// The EVENT opens that sentence. A title names the entity; it does not say
// what happened to it, and a lane of PR titles reads as a list of pull
// requests rather than a list of things that need something. The label is
// mono because the system classified it and the sentence stays sans because
// it's prose, not a category — one line, two registers, no extra row. The
// slot always says something: with no summary the event's own DESCRIPTION
// takes the whole line, so a task the scorer left without one still says
// what it is about.
//
// Tone spends hue only where the event is a problem or an ask. `eventTone`
// also has a `good`, and the language has no success colour; a green
// "CI PASSED" would be the one celebration on the board, so good and neutral
// both draw ink. Two hues on the line, both meaning act.
//
// Attention comes in two shapes that look alike and behave nothing alike:
//
//   PERMISSION  blocks. The agent has stopped. It takes the activity row's
//               slot, because a stopped agent has no current command, and the
//               two therefore never appear together.
//   APPROVAL    does not block. Finished artifacts wait on a verdict while the
//               run carries on, so it spends no room at all: the strip warms
//               the kinds involved and the corner brackets brighten. It can
//               therefore coexist with a permission request without the card
//               carrying two asks in the same column.
//
// The footer is elapsed on the left and the artifact strip on the right —
// and on a run that has stopped, the two collapse into one line with the
// ending on it. No Pause, no Cancel: stopping a run is a decision taken where
// the transcript is, not from a lane. No spend either — it changes while you
// read it and is never the reason to act on a card. No failure reason: the
// mark says it failed and the footer says when, and why is a run-page fact.
//
// Where the run IS lives in the STATUS MARK and nowhere else. Not in the
// ground, not in a spine, and not in the activity row's words — that row is a
// destination, and it reads "View run".
//
// A card is a container of targets, never a target itself. The title goes to
// the entity, the activity row and the strip to the run, the assignee mark to
// its picker — and the card is inert between them.

export interface TaskCardProps {
  title: string
  /** The source id. Shown on hover rather than spending a line. */
  entity?: string
  /** The source entity the title names — the issue, ticket or thread. Makes
   *  the title a link with an out-arrow beside it; without it the title is
   *  text. */
  entityHref?: string
  lifecycle: Lifecycle
  summary?: string
  /** Draw the summary's line boxes in hairlines while it is generated. */
  summaryPending?: boolean
  /** What made the task, opening the summary line. Resolved by `cardModel`
   *  from `task.event_type` rather than looked up here: the card reads what
   *  it is handed, and every other derivation on it lives in that one file. */
  event?: { label: string; description: string; tone: EventTone }
  /** The agent's live command. Scans. Ignored unless `working`. */
  command?: string
  /** A blocked run. Its presence removes the activity row. */
  permission?: { command: string; count: number; onAllow?: () => void; onDeny?: () => void }
  artifacts?: ArtifactTotals
  pending?: PendingTotals
  /** How long the run has been going. Runs only. */
  elapsed?: string
  /** How long the task has been waiting. Queued only. Never both. */
  age?: string
  ageTitle?: string
  /** The run page. The activity row, a permission row's head and the strip go here. */
  href?: string
  chain?: { done: number; total: number }
  /** The working mark's state: a queued run holds the pile built. */
  liveState?: 'running' | 'idle'
  /** The task is waiting on the memory its last conversation owes. */
  waiting?: boolean
  /** A delegation the bot took that never fired, and the way to try again. */
  retry?: { message: string; onRetry: () => void }
  /** False removes the verbs a reader may not use — Allow, Deny. */
  interactive?: boolean
  assigneeSlot?: ReactNode
  className?: string
  style?: CSSProperties
}

// The corner brackets. A card's own hairline says where it ENDS; the brackets
// say it is a thing you can pick up, and they brighten together on hover so
// the whole card reads as one target rather than four.
//
// They also carry the ONE ask that does not block: artifacts waiting on a
// verdict take the frame from 35% warm to 90%. No element is added, nothing
// has to find room on a card that is already dense, and four arms at the
// card's corners are legible at a lane's scanning distance in a way a 14px
// disc in the footer is not.
function Brackets({ lit, asking }: { lit: boolean; asking: boolean }) {
  const pct = asking ? (lit ? 100 : 90) : lit ? 75 : 35
  const c = `color-mix(in srgb, var(--color-warm) ${pct}%, transparent)`
  const arm: CSSProperties = {
    position: 'absolute',
    width: 10,
    height: 10,
    pointerEvents: 'none',
    transition: 'border-color var(--dur-hover)',
  }
  const r = 'var(--radius-inflow)'
  return (
    <>
      <span
        aria-hidden
        style={{
          ...arm,
          left: 0,
          top: 0,
          borderRadius: `${r} 0 0 0`,
          borderLeft: `1.5px solid ${c}`,
          borderTop: `1.5px solid ${c}`,
        }}
      />
      <span
        aria-hidden
        style={{
          ...arm,
          right: 0,
          top: 0,
          borderRadius: `0 ${r} 0 0`,
          borderRight: `1.5px solid ${c}`,
          borderTop: `1.5px solid ${c}`,
        }}
      />
      <span
        aria-hidden
        style={{
          ...arm,
          left: 0,
          bottom: 0,
          borderRadius: `0 0 0 ${r}`,
          borderLeft: `1.5px solid ${c}`,
          borderBottom: `1.5px solid ${c}`,
        }}
      />
      <span
        aria-hidden
        style={{
          ...arm,
          right: 0,
          bottom: 0,
          borderRadius: `0 0 ${r} 0`,
          borderRight: `1.5px solid ${c}`,
          borderBottom: `1.5px solid ${c}`,
        }}
      />
    </>
  )
}

// A pending summary draws its two line boxes in hairlines — never gray blobs.
// A bar sits inside the LINE BOX it stands for, not on its own height: that
// is the whole trick, since reserving 8px where the text will take 16 makes
// the lane jump when it lands.
const LINE_BODY = 'calc(var(--text-secondary) * var(--leading-body))'
function Bar({ w, i = 0 }: { w: string; i?: number }) {
  return (
    <span aria-hidden style={{ display: 'flex', alignItems: 'center', height: LINE_BODY }}>
      <span
        style={{
          display: 'block',
          width: w,
          height: 8,
          borderRadius: 2,
          boxShadow: 'inset 0 0 0 1px var(--color-line-2)',
          animation: 'tf-draw 1.9s var(--ease-default) infinite',
          animationDelay: `${i * 0.12}s`,
        }}
      />
    </span>
  )
}

// The footer's sentence, per ending. "Ran for" is the only one that measures a
// span rather than naming a stop — everything else happened AFTER a duration,
// which is the distinction between a run that finished and a run that ended.
const SETTLED: Record<Lifecycle, string> = {
  done: 'Ran for ',
  failed: 'Failed after ',
  canceled: 'Canceled after ',
  // The system's own word for a run that stopped mid-turn and can be picked
  // back up.
  idle: 'Idle after ',
  queued: '',
  working: '',
}

// The status mark. A card says where the run IS here and nowhere else.
//
// An 11px slot whatever the state, so the title's first line never shifts:
// queued centres a 7px dot in it, the settled states draw a glyph, and a
// working run carries the crate pile — 20px wide and 19 tall, so it is the
// only mark that also needs vertical room: the slot is 11 and the title's cap
// height sits at the top of it, so the pile is pulled up to hang off that line
// rather than centred on it. A card carrying it has its title pushed right of
// its neighbours'. That is a real cost of the mark, not a bug to pad around.
const SLOT: CSSProperties = {
  width: 11,
  height: 11,
  marginTop: 2,
  flexShrink: 0,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
}
const LIVE_SIZE = 20

function StatusMark({
  lifecycle,
  liveState,
}: {
  lifecycle: Lifecycle
  liveState: 'running' | 'idle'
}) {
  if (lifecycle === 'working')
    return (
      <span
        aria-hidden
        data-mark="crates"
        style={{
          ...SLOT,
          width: LIVE_SIZE,
          height: Math.round((LIVE_SIZE * 21) / 22),
          marginTop: -2,
        }}
      >
        <CrateMark size={LIVE_SIZE} state={liveState} />
      </span>
    )
  if (lifecycle === 'queued')
    return (
      <span aria-hidden data-mark="queued" style={SLOT}>
        <span
          style={{
            width: 7,
            height: 7,
            borderRadius: 'var(--radius-pill)',
            boxShadow: 'inset 0 0 0 1.5px var(--color-ink-4)',
          }}
        />
      </span>
    )
  // Settled. Only failure spends a hue — the rest are ink, because "it ended"
  // is not news.
  const [kind, color]: [GlyphKind, string] =
    lifecycle === 'done'
      ? ['check', 'var(--color-ink-3)']
      : lifecycle === 'failed'
        ? ['cross', 'var(--color-alarm)']
        : lifecycle === 'canceled'
          ? ['dash', 'var(--color-ink-4)']
          : ['pause', 'var(--color-ink-4)']
  return (
    <span aria-hidden data-mark={lifecycle} style={{ ...SLOT, color }}>
      <Glyph kind={kind} size={11} />
    </span>
  )
}

// Chain — the turns a conversation has taken, one segment each. It reports
// SHAPE, not progress: an agent conversation has no known length, so the
// segments say "four turns, two of them finished, this one running" rather
// than a fraction of anything. One cool segment at most, and only while the
// run is working — cool means emission, an agent acting, and a stopped run
// emits nothing. Everything else is ink, because a turn that has happened is
// a fact rather than a signal.
function Chain({ chain, working }: { chain?: { done: number; total: number }; working: boolean }) {
  if (!chain || !chain.total) return null
  const total = chain.total
  const done = Math.max(0, Math.min(total, chain.done))
  const live = working && done < total
  return (
    <div aria-hidden style={{ display: 'flex', gap: 3 }}>
      {Array.from({ length: total }, (_, i) => (
        <span
          key={i}
          style={{
            height: 2,
            flex: 1,
            borderRadius: 999,
            background:
              i < done
                ? 'var(--color-ink-2)'
                : live && i === done
                  ? 'var(--color-cool)'
                  : 'var(--color-ink-3)',
            opacity: i < done || (i === done && live) ? 1 : 0.3,
            animation:
              live && i === done ? 'tf-breathe var(--dur-breath) ease-in-out infinite' : 'none',
          }}
        />
      ))}
    </div>
  )
}

export function TaskCard({
  title,
  entity,
  entityHref,
  lifecycle,
  summary,
  summaryPending = false,
  event,
  command,
  permission,
  artifacts,
  pending,
  elapsed,
  age,
  ageTitle,
  href,
  chain,
  liveState = 'running',
  waiting = false,
  retry,
  interactive = true,
  assigneeSlot,
  className = '',
  style,
}: TaskCardProps) {
  const [hover, setHover] = useState(false)
  // Whether the title overran its two lines, so task-card.css can paint the
  // ellipsis the engine does not. Re-checked on every resize of the title
  // box, since the lane's width is what decides where the lines break.
  const titleRef = useRef<HTMLElement>(null)
  const [clipped, setClipped] = useState(false)
  useLayoutEffect(() => {
    const el = titleRef.current
    if (!el) return
    const check = () => {
      const over = el.scrollHeight > el.clientHeight + 1
      setClipped(over)
      if (!over) return
      // Where line two's text actually ends, so the glyph follows the last
      // word rather than sitting at the box edge with a gap before it. Screen
      // px from the range, divided back to CSS px for the style.
      const box = el.getBoundingClientRect()
      const s = el.offsetWidth ? box.width / el.offsetWidth : 1
      const r = document.createRange()
      r.selectNodeContents(el)
      const visible = Array.from(r.getClientRects()).filter(
        (x) => x.top < box.top + el.clientHeight * s - 1,
      )
      const last = visible[visible.length - 1]
      // The rect includes the trailing space before a soft wrap, so the
      // glyph is pulled back over it; otherwise it floats a word-space off.
      const end = last ? (last.right - box.left) / s - 3 : el.clientWidth
      el.style.setProperty('--tc-ell', Math.max(0, Math.min(end, el.clientWidth - 14)) + 'px')
    }
    check()
    // The font swap moves where the lines break without changing the span's
    // box, so a ResizeObserver on the span alone leaves the glyph where the
    // fallback font's line ended. Not fonts.ready: that promise may already
    // be resolved when this runs, and the title's own font is requested
    // lazily AFTER it, so it never fires again. The loadingdone event fires
    // for every batch.
    const fonts = typeof document !== 'undefined' ? document.fonts : undefined
    fonts?.addEventListener?.('loadingdone', check)
    let ro: ResizeObserver | undefined
    if (typeof ResizeObserver !== 'undefined') {
      ro = new ResizeObserver(check)
      ro.observe(el)
      // A lane resize re-breaks the lines without resizing the span.
      const card = el.closest('.tc')
      if (card) ro.observe(card)
    }
    return () => {
      fonts?.removeEventListener?.('loadingdone', check)
      ro?.disconnect()
    }
  }, [title])

  const queued = lifecycle === 'queued'
  const working = lifecycle === 'working'
  const blocked = !!permission
  // Any run that is not currently producing. Idle is included even though it
  // is resumable: there is no command to report either way, so a row of its
  // own would hold two words, and the footer can say the same thing on a line
  // it was already spending.
  const settled = !queued && !working && !blocked
  const asking = pendingCount(pending) > 0
  // Liveness and blocking are INDEPENDENT. A run waiting on a permission is
  // still claimed and current, so its status mark stays the live mark; the
  // plate is what says it has stopped.
  const time = elapsed || (queued ? age : '')

  const titleNode = entityHref ? (
    <a
      ref={titleRef as React.RefObject<HTMLAnchorElement>}
      className="tc-clamp tc-link"
      href={entityHref}
      target="_blank"
      rel="noreferrer"
      data-clipped={clipped || undefined}
    >
      {title}
    </a>
  ) : (
    <span
      ref={titleRef as React.RefObject<HTMLSpanElement>}
      className="tc-clamp"
      data-clipped={clipped || undefined}
    >
      {title}
    </span>
  )

  // The activity slot: a blocked run's plate, the memory wait, a delegation
  // that never fired, or the working run's live line. Nothing on a queued or
  // settled card — the settled footer carries its own row.
  let slot: ReactNode = null
  if (blocked && permission) {
    slot = (
      <PermissionRow
        command={permission.command}
        count={permission.count}
        href={href}
        onAllow={permission.onAllow}
        onDeny={permission.onDeny}
        interactive={interactive}
      />
    )
  } else if (waiting) {
    // The run page is where the wait is explained; the card only names it.
    slot = href ? (
      <ActivityRow label="Waiting on memory" href={href} />
    ) : (
      <span className="tc-readout">Waiting on memory</span>
    )
  } else if (retry) {
    slot = (
      <button type="button" className="tc-retry" onClick={retry.onRetry} title={retry.message}>
        <Glyph kind="alert" size={11} />
        <span>Delegation didn’t fire</span>
        <span className="tc-retry-verb">Retry</span>
      </button>
    )
  } else if (working) {
    slot = <ActivityRow command={command} label="View run" href={href} />
  }

  return (
    <div
      className={`tc ${className}`.trim()}
      style={style}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
    >
      <Brackets lit={hover} asking={asking} />
      <div className="tc-head">
        <StatusMark lifecycle={lifecycle} liveState={liveState} />
        {/* The source id is a definition of the title, not a datum of its own,
            so it is a hint rather than a line — that is the whole reason the
            card can afford a two-line title. Tooltip stays inert when there is
            no entity, focus included, so this needs no branch. The title is
            the link to the thing it names — the issue, the ticket, the thread
            — and the tooltip is that thing's id. A link when there is
            somewhere to go; otherwise plain text, because a pointer over a
            title that opens nothing is a promise the card cannot keep. */}
        <Tooltip content={entity ?? ''} side="top" className="tc-title" focusable={!entityHref}>
          {titleNode}
        </Tooltip>
        {/* The out-arrow has its own slot on the row, aligned to the first
            line, so it lands in the same place whether the title is one line,
            two, or truncated — the clamp can never cut it. It lights with the
            title: one affordance, two parts. */}
        {entityHref && (
          <a
            className="tc-out"
            href={entityHref}
            target="_blank"
            rel="noreferrer"
            tabIndex={-1}
            aria-hidden
          >
            <svg
              width="11"
              height="11"
              viewBox="0 0 12 12"
              fill="none"
              style={{ display: 'block', transform: 'rotate(-45deg)' }}
            >
              <path
                d="M2 6h7M6.5 3l3 3-3 3"
                stroke="currentColor"
                strokeWidth="1.3"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </a>
        )}
        {assigneeSlot}
      </div>

      <Chain chain={chain} working={working} />

      {/* What the work IS, in the scorer's own words rather than the run's,
          opened by the event that made the task. Shown in every state: the
          caller decides — pass it or do not.

          Pending keeps the lead-in. The event is known the moment the row
          arrives and the summary is not, so the label lands first and the
          bars fill beside and under it; the first bar takes the rest of the
          label's own line, which is why it is the one that flexes. */}
      {summaryPending ? (
        <span style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', columnGap: 7 }}>
          {event && (
            <span className="tc-event" data-tone={event.tone}>
              {event.label}
            </span>
          )}
          <span style={{ flex: 1, minWidth: 90 }}>
            <Bar w="100%" i={0} />
          </span>
          {/* The wrapper carries the width, not the bar: Bar's own percentage
              resolves against its parent, and a bare flex item is
              content-sized — so `w="64%"` inside one is 64% of nothing. */}
          <span style={{ width: '64%' }}>
            <Bar w="100%" i={1} />
          </span>
        </span>
      ) : summary ? (
        <div className="tc-summary">
          {/* The label ends in a real space, INSIDE the span: the gap the eye
              sees is the rule's margin, but a reader that does not see —
              assistive technology, a copy of the text — needs a word
              separator in the DOM, or the label and the first word of the
              summary are one word. task-card.css makes the span an
              inline-block so the trailing space collapses at the end of its
              own line and the visible gap stays exactly the margin. */}
          {event && (
            <span className="tc-event" data-tone={event.tone}>
              {event.label}{' '}
            </span>
          )}
          {summary}
        </div>
      ) : (
        event && (
          <div className="tc-event-line" data-tone={event.tone}>
            {event.description}
          </div>
        )
      )}

      {slot}

      {settled ? (
        // A finished run has nothing to report and one duration to state, so
        // the row and the footer collapse into each other: artifacts take the
        // left, and "Ran for" carries the arrow at the right. One line where
        // there were two, and the elapsed clock stops being a live figure and
        // becomes the sentence's own subject. A task ended with no run to view
        // states its age instead.
        <div className="tc-foot">
          <span style={{ display: 'flex', minWidth: 0 }}>
            <ArtifactStrip artifacts={artifacts} pending={pending} href={href} />
          </span>
          {href ? (
            <ActivityRow
              variant="foot"
              label={elapsed ? SETTLED[lifecycle] + elapsed : 'View run'}
              href={href}
            />
          ) : (
            age && (
              <span className="tc-readout" title={ageTitle}>
                {age}
              </span>
            )
          )}
        </div>
      ) : (
        (time || artifacts) && (
          <div className="tc-foot">
            <span className="tc-readout" title={queued ? ageTitle : undefined}>
              {time}
            </span>
            <ArtifactStrip artifacts={artifacts} pending={pending} href={href} />
          </div>
        )
      )}
    </div>
  )
}

export default TaskCard
