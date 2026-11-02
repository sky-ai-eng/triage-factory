import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useReducedMotion } from '../shared/useReducedMotion'
import './toast-log.css'

export type ToastLevel = 'info' | 'success' | 'warning' | 'error'

/** Navigation only. A toast never re-runs a request; recovery lives on the surface that failed. */
export type ToastLogAction = {
  label: string
  /** Where it goes. Always a real href, so the link is a tab stop and a modified click opens a new tab. */
  href: string
  /** Handles a plain primary click in-app (the caller's router). Without it the anchor navigates. */
  onSelect?: () => void
}

export type ToastLogItem = {
  id: string
  level: ToastLevel
  title?: string
  body: string
  /** When it was pushed, ms since epoch. Drawn as HH:MM after the body. */
  at: number
  action?: ToastLogAction
}

export type ToastLogProps = {
  /** Oldest first, the store's order. Rendered newest on top. */
  items: ToastLogItem[]
  /** A row has finished leaving. Remove it from `items` here, not before, or it unmounts mid-fade. */
  onDone: (id: string) => void
  /** Rows shown at once. Past this the oldest leaves early. */
  max?: number
  /** false: no hover or focus pause and no Escape. The clocks still run, and still wait for the tab. */
  interactive?: boolean
}

// Lifetimes. See ToastLog.md before changing any timing here: the clock line
// IS the timer, so these are also the length of the line's run.
const LIFETIME: Record<ToastLevel, number> = {
  success: 3000,
  info: 3000,
  warning: 6000,
  error: 10000,
}

const ENTER_OPEN = 120
const ENTER_FADE = 300
const FADE = 120
const EXIT_CLOSE = 160

const GLYPH: Record<ToastLevel, string> = {
  success: 'M13.3 4.7L6 12l-3.3-3.3',
  info: 'M8 14.2A6.2 6.2 0 108 1.8a6.2 6.2 0 000 12.4zM8 7.4v3.6M8 5.1h.01',
  warning: 'M8 5.4v3.2M8 11.1h.01M8 2.2l6 10.4H2z',
  error: 'M12 4L4 12M4 4l8 8',
}

// No level word is drawn — the glyph carries it — so a screen reader hears one.
const SPOKEN: Record<ToastLevel, string> = {
  success: 'Done:',
  info: 'Note:',
  warning: 'Warning:',
  error: 'Error:',
}

function attending(): boolean {
  if (typeof document === 'undefined') return true
  return document.visibilityState === 'visible' && document.hasFocus()
}

function stamp(at: number): string {
  const d = new Date(at)
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0')
}

// WAAPI takes an easing as a string, not a var(), so the curve is read off the
// token layer when the animation starts rather than restated here.
function ease(el: Element, token: '--ease-default' | '--ease-fold'): string | undefined {
  return getComputedStyle(el).getPropertyValue(token).trim() || undefined
}

// ToastLog — one panel of short replies to what the viewer just did, newest on
// top. Each row's lifetime is the 1px clock line at its foot, and the panel
// holds every clock while the viewer is reading it or is somewhere else.
export function ToastLog({ items, onDone, max = 5, interactive = true }: ToastLogProps) {
  const reduced = useReducedMotion()
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const [away, setAway] = useState(() => !attending())
  const [escaped, setEscaped] = useState<ReadonlySet<string>>(() => new Set())
  // Rows whose exit has begun, for any reason. Escape skips them, so each
  // press takes a row that is still showing.
  const leavingRef = useRef(new Set<string>())

  // An emptied panel collapses from under a pointer that has not moved, and a
  // row removed without its exit takes focus with it; neither fires a leave
  // event. Without this the next toast would arrive held and never run out.
  const empty = items.length === 0
  const [wasEmpty, setWasEmpty] = useState(empty)
  if (empty !== wasEmpty) {
    setWasEmpty(empty)
    if (empty) {
      setHovered(false)
      setFocused(false)
    }
  }

  useEffect(() => {
    const sync = () => setAway(!attending())
    const blur = () => setAway(true)
    document.addEventListener('visibilitychange', sync)
    window.addEventListener('focus', sync)
    window.addEventListener('blur', blur)
    return () => {
      document.removeEventListener('visibilitychange', sync)
      window.removeEventListener('focus', sync)
      window.removeEventListener('blur', blur)
    }
  }, [])

  // Escape dismisses the newest row still showing. It is listened for on the
  // window so a row with no link (no tab stop) can still be dismissed, but it
  // only claims the key when nothing nearer did: a dialog, menu or the shell
  // palette that handles Escape marks it defaultPrevented first, and a press
  // typed in a field is left alone. Claiming it marks it, so one press never
  // does two things.
  useEffect(() => {
    if (!interactive) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      const t = e.target
      if (t instanceof HTMLElement && (t.isContentEditable || t.matches('input, textarea, select')))
        return
      const top = [...items].reverse().find((x) => !leavingRef.current.has(x.id))
      if (!top) return
      e.preventDefault()
      leavingRef.current.add(top.id)
      setEscaped((s) => new Set(s).add(top.id))
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [interactive, items])

  const paused = away || (interactive && (hovered || focused))
  // An escaped row already counts as gone, so it does not push out an older one.
  const showing = items.filter((t) => !escaped.has(t.id))
  const evicted = new Set(showing.slice(0, Math.max(0, showing.length - max)).map((t) => t.id))
  const rows = [...items].reverse()

  // The panel stays mounted while empty, so its list is already a live region
  // when the first row arrives: a region inserted together with its content is
  // often not announced at all.
  return (
    <div
      className="tl"
      role={empty ? undefined : 'region'}
      aria-label={empty ? undefined : 'Messages'}
      data-empty={empty || undefined}
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
      <ol className="tl-list" aria-live="polite" aria-relevant="additions">
        {rows.map((t) => (
          <Row
            key={t.id}
            item={t}
            paused={paused}
            reduced={reduced}
            leaving={evicted.has(t.id) || escaped.has(t.id)}
            onLeave={(id) => leavingRef.current.add(id)}
            onDone={(id) => {
              leavingRef.current.delete(id)
              setEscaped((s) => {
                if (!s.has(id)) return s
                const next = new Set(s)
                next.delete(id)
                return next
              })
              onDone(id)
            }}
          />
        ))}
      </ol>
    </div>
  )
}

type RowProps = {
  item: ToastLogItem
  paused: boolean
  reduced: boolean
  leaving: boolean
  onLeave: (id: string) => void
  onDone: (id: string) => void
}

function Row({ item, paused, reduced, leaving, onLeave, onDone }: RowProps) {
  const outer = useRef<HTMLLIElement>(null)
  const inner = useRef<HTMLDivElement>(null)
  const fill = useRef<HTMLSpanElement>(null)
  const clock = useRef<Animation | null>(null)
  // Latched the moment the exit starts. `leaving` can fall back to false
  // mid-exit — an evicted row stops being the sixth once a newer one is
  // removed — and nothing may restart the clock or the exit after that.
  const exiting = useRef(false)
  const [expired, setExpired] = useState(false)
  const out = expired || leaving

  // Enter, and start the clock. The clock IS the timer: its `finished` starts
  // the exit, so pausing the animation pauses the lifetime with no second
  // setTimeout to keep in step.
  useLayoutEffect(() => {
    const o = outer.current!
    const i = inner.current!
    const enter = reduced
      ? [i.animate([{ opacity: 0 }, { opacity: 1 }], { duration: FADE })]
      : [
          o.animate([{ height: '0px' }, { height: i.offsetHeight + 'px' }], {
            duration: ENTER_OPEN,
            easing: ease(o, '--ease-default'),
          }),
          i.animate([{ opacity: 0 }, { opacity: 0, offset: 0.4 }, { opacity: 1 }], {
            duration: ENTER_FADE,
          }),
        ]
    const ms = LIFETIME[item.level]
    const c = fill.current!.animate([{ transform: 'scaleX(1)' }, { transform: 'scaleX(0)' }], {
      duration: ms,
      easing: reduced ? `steps(${Math.round(ms / 1000)})` : 'linear',
      fill: 'forwards',
    })
    clock.current = c
    c.finished.then(
      () => setExpired(true),
      () => {},
    )
    return () => {
      for (const a of enter) a.cancel()
      c.cancel()
    }
    // Once per row. Re-running on a reduced-motion change would replay the
    // entrance and restart the clock from full, losing the time already spent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const c = clock.current
    // play() on a finished animation rewinds it, which would refill the line
    // in the moment between the clock running out and the exit starting.
    if (!c || exiting.current || c.playState === 'finished') return
    if (paused) c.pause()
    else c.play()
  }, [paused])

  useEffect(() => {
    if (!out || exiting.current) return
    exiting.current = true
    onLeave(item.id)
    clock.current?.pause()
    const o = outer.current!
    const i = inner.current!
    // A row that leaves holding focus gives it up first. Unmounting a focused
    // element fires no focusout, so the panel would go on believing focus is
    // inside it and hold every other row's clock indefinitely.
    const active = document.activeElement
    if (active instanceof HTMLElement && o.contains(active)) active.blur()
    let gone = false
    const done = () => {
      if (!gone) {
        gone = true
        onDone(item.id)
      }
    }
    i.animate([{ opacity: 1 }, { opacity: 0 }], { duration: FADE, fill: 'forwards' })
      .finished.then(() => {
        if (reduced) return done()
        return o
          .animate([{ height: o.offsetHeight + 'px' }, { height: '0px' }], {
            duration: EXIT_CLOSE,
            easing: ease(o, '--ease-fold'),
            fill: 'forwards',
          })
          .finished.then(done)
      })
      .catch(done)
  }, [out, item.id, onLeave, onDone, reduced])

  const action = item.action

  return (
    <li ref={outer} className="tl-row" data-level={item.level}>
      {/* The alert role sits inside the item rather than on it: an <li>
          carrying another role is no longer a list item. */}
      <div ref={inner} className="tl-in" role={item.level === 'error' ? 'alert' : undefined}>
        <span className="tl-ico" aria-hidden="true">
          <svg viewBox="0 0 16 16">
            <path d={GLYPH[item.level]} />
          </svg>
        </span>
        <div className="tl-text">
          <span className="tl-sr">{SPOKEN[item.level]} </span>
          {item.title && <span className="tl-title">{item.title}</span>}
          <span className="tl-body">
            {item.body}
            <time className="tl-time" dateTime={new Date(item.at).toISOString()}>
              {stamp(item.at)}
            </time>
          </span>
          {action && (
            <a
              className="tl-act"
              href={action.href}
              onClick={(e) => {
                // A plain primary click stays in the app; a modified click or
                // another button keeps the anchor's own behavior (a new tab).
                if (!action.onSelect) return
                if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
                e.preventDefault()
                action.onSelect()
              }}
            >
              {action.label}
            </a>
          )}
        </div>
        <span className="tl-bar" aria-hidden="true">
          <span ref={fill} className="tl-fill" />
        </span>
      </div>
    </li>
  )
}

export default ToastLog
