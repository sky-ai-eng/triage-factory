import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { ease } from '../shared/ease'
import { useAway } from '../shared/useAway'
import { useReducedMotion } from '../shared/useReducedMotion'
import './notify-strip.css'

export type StripItem = {
  id: string
  /** The audience, preformatted: 'ORG', 'TEAM INFRA', 'YOU'. */
  scope: string
  title: string
  /** How long the condition has been true, preformatted ('2d'). A node, so the caller can keep it live. */
  since?: ReactNode
  /** This viewer's read mark. An unread urgent item draws the full 40px band. */
  read: boolean
  /** The item's first action this viewer may use, if any. */
  action?: { label: string; onSelect: () => void }
}

export type NotifyStripProps = {
  /** Open, undismissed urgent items for this viewer, newest first. */
  urgent: StripItem[]
  /** One high arrival at a time. The caller queues the rest. */
  arrival?: StripItem | null
  /** The arrival has finished leaving: its 8s ran out, or it was opened. Clear `arrival` here. */
  onArrivalDone?: (id: string) => void
  /** A title, or an arrival's `open`, was chosen. The caller marks the item read and navigates. */
  onOpen: (id: string) => void
  /** false: titles, actions and the stepper's arrows are text, and only an inactive tab holds the clock. */
  interactive?: boolean
}

/** How long a high arrival is announced, while the viewer is attending. */
export const ARRIVAL_MS = 8000

const OPEN = 120
const ENTER_FADE = 300
const FADE = 120
const CLOSE = 160

// NotifyStrip — the strip in Shell's banner slot. Urgent notifications stay
// until they resolve; a high one is announced for 8s when it arrives. See
// NotifyStrip.md.
export function NotifyStrip({
  urgent,
  arrival = null,
  onArrivalDone,
  onOpen,
  interactive = true,
}: NotifyStripProps) {
  const reduced = useReducedMotion()
  const away = useAway()
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)

  // The urgent items the line last showed, so a line that closes because its
  // last condition resolved leaves showing what it was rather than going blank.
  const [lastUrgent, setLastUrgent] = useState(urgent)
  if (urgent.length > 0 && urgent !== lastUrgent) setLastUrgent(urgent)

  // An emptied strip collapses from under a pointer that has not moved, and
  // nothing fires pointerleave for it. Without this the next arrival would
  // start held and never run out.
  const empty = urgent.length === 0 && arrival === null
  const [wasEmpty, setWasEmpty] = useState(empty)
  if (empty !== wasEmpty) {
    setWasEmpty(empty)
    if (empty) {
      setHovered(false)
      setFocused(false)
    }
  }

  const paused = away || (interactive && (hovered || focused))

  // The strip stays mounted while empty, at no height, so it is already a live
  // region when its first line arrives: a region inserted together with its
  // content is often not announced at all.
  return (
    <div
      className="ns"
      role={empty ? undefined : 'region'}
      aria-label={empty ? undefined : 'Notifications'}
      aria-live="polite"
      aria-relevant="additions"
      onPointerEnter={interactive ? () => setHovered(true) : undefined}
      onPointerLeave={interactive ? () => setHovered(false) : undefined}
      onFocus={interactive ? () => setFocused(true) : undefined}
      onBlur={
        interactive
          ? (e) => {
              if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setFocused(false)
            }
          : undefined
      }
    >
      <Fold open={urgent.length > 0} reduced={reduced}>
        {lastUrgent.length > 0 && (
          <Urgent items={lastUrgent} onOpen={onOpen} interactive={interactive} />
        )}
      </Fold>
      {arrival && (
        <Arrival
          key={arrival.id}
          item={arrival}
          paused={paused}
          reduced={reduced}
          interactive={interactive}
          onOpen={onOpen}
          onDone={(id) => onArrivalDone?.(id)}
        />
      )}
    </div>
  )
}

type FoldProps = {
  open: boolean
  reduced: boolean
  /** The line has finished closing. */
  onClosed?: () => void
  children: ReactNode
}

// Opens a line from no height and closes it back, the toast log's Fade: the
// space arrives, then the words; the words leave, then the space. The content
// stays on screen while it closes, so it leaves showing what it was.
function Fold({ open, reduced, onClosed, children }: FoldProps) {
  const outer = useRef<HTMLDivElement>(null)
  const inner = useRef<HTMLDivElement>(null)
  const [shown, setShown] = useState(open)
  if (open && !shown) setShown(true)

  // Read through a ref so a parent re-rendering mid-close, with a new closure,
  // does not restart the close.
  const closedRef = useRef(onClosed)
  useEffect(() => {
    closedRef.current = onClosed
  })

  useLayoutEffect(() => {
    if (!open) return
    const o = outer.current!
    const i = inner.current!
    const enter = reduced
      ? [i.animate([{ opacity: 0 }, { opacity: 1 }], { duration: FADE })]
      : [
          o.animate([{ height: '0px' }, { height: i.offsetHeight + 'px' }], {
            duration: OPEN,
            easing: ease(o, '--ease-default'),
          }),
          i.animate([{ opacity: 0 }, { opacity: 0, offset: 0.4 }, { opacity: 1 }], {
            duration: ENTER_FADE,
          }),
        ]
    return () => {
      for (const a of enter) a.cancel()
    }
    // Once per opening. Re-running on a reduced-motion change would replay
    // the entrance on a line that is already showing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  useEffect(() => {
    if (open || !shown) return
    const o = outer.current
    const i = inner.current
    if (!o || !i) return
    // A line that closes holding focus gives it up first: unmounting a
    // focused element fires no focusout, and the strip would stay held.
    const active = document.activeElement
    if (active instanceof HTMLElement && o.contains(active)) active.blur()
    let live = true
    const exit = [i.animate([{ opacity: 1 }, { opacity: 0 }], { duration: FADE, fill: 'forwards' })]
    exit[0].finished
      .then(() => {
        if (reduced) return
        const close = o.animate([{ height: o.offsetHeight + 'px' }, { height: '0px' }], {
          duration: CLOSE,
          easing: ease(o, '--ease-fold'),
          fill: 'forwards',
        })
        exit.push(close)
        return close.finished
      })
      .then(
        () => {
          if (!live) return
          setShown(false)
          closedRef.current?.()
        },
        () => {},
      )
    // Cancelled when the line opens again before it has closed, so the close's
    // held opacity and height do not outlive it.
    return () => {
      live = false
      for (const a of exit) a.cancel()
    }
  }, [open, shown, reduced])

  if (!shown) return null
  return (
    <div ref={outer} className="ns-slide">
      <div ref={inner}>{children}</div>
    </div>
  )
}

type UrgentProps = {
  items: StripItem[]
  onOpen: (id: string) => void
  interactive: boolean
}

function Urgent({ items, onOpen, interactive }: UrgentProps) {
  // Starts on the first unread item, so an unread condition is never hidden
  // behind a read one's "+N urgent".
  const [at, setAt] = useState<number | null>(null)
  const k = Math.min(
    at ??
      Math.max(
        0,
        items.findIndex((x) => !x.read),
      ),
    items.length - 1,
  )
  const it = items[k]
  const full = !it.read
  const many = items.length > 1

  return (
    <div className="ns-line ns-urgent" data-full={full || undefined}>
      <span className="ns-label">URGENT · {it.scope}</span>
      {interactive ? (
        <button type="button" className="ns-title" onClick={() => onOpen(it.id)}>
          {it.title}
        </button>
      ) : (
        <span className="ns-title">{it.title}</span>
      )}
      {it.since != null && <span className="ns-since">{it.since}</span>}
      {!full && many && <span className="ns-more">· +{items.length - 1} urgent</span>}
      <span className="ns-end">
        {interactive && it.action && (
          <button type="button" className="ns-act" onClick={it.action.onSelect}>
            {it.action.label}
          </button>
        )}
        {full && many && (
          <span className="ns-step">
            {interactive && (
              <button
                type="button"
                aria-label="Previous urgent notification"
                disabled={k === 0}
                onClick={() => setAt(k - 1)}
              >
                ‹
              </button>
            )}
            <span>
              {k + 1} of {items.length}
            </span>
            {interactive && (
              <button
                type="button"
                aria-label="Next urgent notification"
                disabled={k === items.length - 1}
                onClick={() => setAt(k + 1)}
              >
                ›
              </button>
            )}
          </span>
        )}
      </span>
    </div>
  )
}

type ArrivalProps = {
  item: StripItem
  paused: boolean
  reduced: boolean
  interactive: boolean
  onOpen: (id: string) => void
  onDone: (id: string) => void
}

function Arrival({ item, paused, reduced, interactive, onOpen, onDone }: ArrivalProps) {
  const fill = useRef<HTMLSpanElement>(null)
  const clock = useRef<Animation | null>(null)
  const [leaving, setLeaving] = useState(false)

  // The clock IS the timer: its `finished` starts the close, so pausing the
  // animation pauses the announcement with no second setTimeout to keep in step.
  useLayoutEffect(() => {
    const c = fill.current!.animate([{ transform: 'scaleX(1)' }, { transform: 'scaleX(0)' }], {
      duration: ARRIVAL_MS,
      easing: reduced ? `steps(${ARRIVAL_MS / 1000})` : 'linear',
      fill: 'forwards',
    })
    clock.current = c
    c.finished.then(
      () => setLeaving(true),
      () => {},
    )
    return () => c.cancel()
    // Once per arrival. Re-running on a reduced-motion change would restart
    // the clock from full, losing the time already spent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const c = clock.current
    // play() on a finished animation rewinds it, which would refill the line
    // in the moment between the clock running out and the close starting.
    if (!c || c.playState === 'finished') return
    if (leaving || paused) c.pause()
    else c.play()
  }, [paused, leaving])

  const open = () => {
    onOpen(item.id)
    setLeaving(true)
  }

  return (
    <Fold open={!leaving} reduced={reduced} onClosed={() => onDone(item.id)}>
      <div className="ns-line ns-high">
        <span className="ns-label">HIGH · {item.scope}</span>
        {interactive ? (
          <button type="button" className="ns-title" onClick={open}>
            {item.title}
          </button>
        ) : (
          <span className="ns-title">{item.title}</span>
        )}
        <span className="ns-end">
          {interactive && (
            <button type="button" className="ns-link" onClick={open}>
              open
            </button>
          )}
        </span>
        <span className="ns-clock" aria-hidden="true">
          <span ref={fill} />
        </span>
      </div>
    </Fold>
  )
}

export default NotifyStrip
