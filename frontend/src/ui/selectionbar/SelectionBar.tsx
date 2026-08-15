import { useState, useRef, useEffect, useLayoutEffect, useCallback } from 'react'
import type {
  CSSProperties,
  KeyboardEvent as ReactKeyboardEvent,
  MouseEvent as ReactMouseEvent,
  PointerEvent as ReactPointerEvent,
  ReactNode,
} from 'react'
import Hold from '../hold/Hold'
import Countdown from '../countdown/Countdown'
import { prefersReducedMotion } from '../shared/useReducedMotion'
import './selection-bar.css'

export type SelectionVerb = {
  label: string
  tone?: 'bad' | 'alarm' | 'default' | string
  /** 0 fires on press with the rings arriving at once; higher is leaned on. */
  holdMs?: number
  onSelect?: () => void
}

export type PickerOption = { id: string; name: string; note?: string; blocked?: boolean }

export type SelectionBarProps = {
  count?: number
  /** Overrides "<n> selected". */
  label?: ReactNode
  actions?: SelectionVerb[]
  picker?: {
    /** Must be SHORTER than "Change <label>" — the open label is painted over it. */
    label: string
    value?: string
    options?: PickerOption[]
    onPick?: (option: PickerOption) => void
    onRevert?: () => void
  }
  danger?: { label?: string; ms?: number; steps?: number; onConfirm?: () => void }
  onDismiss?: () => void
  /**
   * Controlled undo. A table owns the snapshot and the commit window, so it
   * drives the countdown and this draws it; without it the bar runs its own.
   */
  undo?: { msg: ReactNode; frac: number; onUndo: () => void } | null
  undoSeconds?: number
  undoMessage?: string | ((option: PickerOption, count: number) => string)
  className?: string
  style?: CSSProperties
}

// SelectionBar — what a multi-select table offers once rows are selected.
//
// It floats over the table rather than living in the chrome, so nothing is
// reserved when nothing is selected, and it appears near the checkboxes that
// produced it instead of across the page from them.
//
// Three ideas carry it:
//
//   THE BAR IS THE CONTROL. A verb does not get its own button inside the bar —
//   the bar itself is the Hold surface, and the measurement crosses all of it.
//   Every verb runs through that one gesture, parameterized by `holdMs`: a
//   removal is leaned on for 900ms, an ordinary verb is 0 and fires on press
//   with the rings arriving at once. Same confirmation, different pressure.
//
//   THE PICKER DRAWS ITSELF. A choice is a stem growing out of a terminus X,
//   with ticks extending and labels arriving along them, separated by a
//   soft-edged blur field. No fill, no rectangle, no shadow — a dropdown panel
//   is the one shape this system does not have.
//
//   REVERSIBLE EDITS APPLY, THEN OFFER UNDO. The change lands immediately and
//   the bar becomes a countdown you can cancel. A pending state that has not
//   applied yet makes the table lie for the length of the window, and
//   confirming a reversible edit is what trains people to dismiss the dialogs
//   that matter.
//
// See SelectionBar.md before changing any of it.

const GROW_MS = 300

function Picker({
  label,
  value,
  options,
  onPick,
  onToggle,
  open,
}: {
  label: string
  value?: string
  options: PickerOption[]
  onPick: (o: PickerOption) => void
  onToggle: (next: boolean) => void
  open: boolean
}) {
  const host = useRef<HTMLSpanElement | null>(null)
  const stem = useRef<HTMLSpanElement | null>(null)
  const term = useRef<HTMLSpanElement | null>(null)
  const opts = useRef<HTMLSpanElement | null>(null)
  const haze = useRef<HTMLSpanElement | null>(null)

  // Two measurements taken from the DOM rather than hardcoded:
  //   the spine runs from the terminus X's centre to the LAST tick, so both
  //   ends are joins and the option count is free;
  //   the blur field is a union of one blob per row, so it follows the ragged
  //   text edge instead of drawing a rectangle by another name.
  const measure = useCallback(() => {
    const s = stem.current
    const t = term.current
    const o = opts.current
    const h = host.current
    if (!s || !t || !o || !h) return
    const rows = o.querySelectorAll('.selbar-opt')
    if (!rows.length) return

    // Screen pixels → CSS pixels. Gauge off the bar's WIDTH: it is hundreds of
    // pixels wide and a dozen tall, and a short gauge rounds into whole pixels
    // of error once it multiplies an eighty-pixel spine.
    const bar = (h.closest('.selbar') as HTMLElement) || h
    const gauge = bar.getBoundingClientRect().width / (bar.offsetWidth || 1)
    const k = gauge > 0.01 ? gauge : 1

    // The list grows upward, so the option the spine must reach is the topmost
    // tick, not the first in document order.
    let top = Infinity
    rows.forEach((r) => {
      const tick = r.querySelector('i')
      if (!tick) return
      const b = tick.getBoundingClientRect()
      top = Math.min(top, b.top + b.height / 2)
    })
    if (!isFinite(top)) return

    const tr = t.getBoundingClientRect()
    const termY = tr.top + tr.height / 2
    const hr = h.getBoundingClientRect()
    // Sub-pixel, not rounded: half a pixel at either end is exactly the gap
    // that stops the top join reading as a corner.
    s.style.height = (Math.max(0, termY - top) / k).toFixed(2) + 'px'

    // The blur field's BOX is measured from the options rather than guessed. A
    // fixed box clips the blobs at its own edge, and a clipped soft-edged mask
    // is a hard stop — the one thing this treatment exists to avoid. BLEED must
    // exceed the blob radii below, so every ellipse fades out inside the element.
    const z = haze.current
    if (z) {
      const BLEED = 46
      const or = o.getBoundingClientRect()
      z.style.left = ((or.left - hr.left) / k - BLEED).toFixed(2) + 'px'
      z.style.bottom = ((hr.bottom - or.bottom) / k - BLEED).toFixed(2) + 'px'
      z.style.width = (or.width / k + BLEED * 2).toFixed(2) + 'px'
      z.style.height = (or.height / k + BLEED * 2).toFixed(2) + 'px'
      const zr = z.getBoundingClientRect()
      const layers: string[] = []
      rows.forEach((r) => {
        const b = r.getBoundingClientRect()
        if (!b.width || !zr.width) return
        const cx = ((b.left + b.width / 2 - zr.left) / zr.width) * 100
        const cy = ((b.top + b.height / 2 - zr.top) / zr.height) * 100
        const rx = ((b.width / 2 + 34) / zr.width) * 100
        const ry = ((b.height / 2 + 34) / zr.height) * 100
        layers.push(
          'radial-gradient(ellipse ' +
            rx.toFixed(2) +
            '% ' +
            ry.toFixed(2) +
            '% at ' +
            cx.toFixed(2) +
            '% ' +
            cy.toFixed(2) +
            '%, #000 46%, transparent 86%)',
        )
      })
      if (layers.length) {
        const m = layers.join(',')
        const style = z.style as CSSStyleDeclaration & { webkitMaskImage?: string }
        style.webkitMaskImage = m
        style.maskImage = m
      }
    }

    if (!s.dataset.grown) {
      s.dataset.grown = '1'
      // The spine growing out of the terminus is the picker's arrival, and it is
      // a hand-stepped frame loop, so the blanket rule in tokens/motion.css
      // cannot reach it. Under the preference the spine is simply already
      // there: the structure is the information, the growth is the flourish.
      //
      // Stepped by hand because both declarative routes — a CSS keyframe and a
      // WAAPI animation — strand at their from-state on a remounted node,
      // reporting "running" with a start time that never resolves, which leaves
      // the spine invisible on every open after the first.
      if (prefersReducedMotion()) {
        s.style.transform = 'translateX(-50%) scaleY(1)'
      } else {
        const t0 = performance.now()
        const ease = (x: number) => 1 - Math.pow(1 - x, 3)
        const step = (now: number) => {
          if (!s.isConnected) return
          const p = Math.min(1, (now - t0) / GROW_MS)
          s.style.transform = 'translateX(-50%) scaleY(' + ease(p).toFixed(4) + ')'
          if (p < 1) requestAnimationFrame(step)
        }
        s.style.transform = 'translateX(-50%) scaleY(0)'
        requestAnimationFrame(step)
      }
    }
  }, [])

  useLayoutEffect(measure)

  useEffect(() => {
    if (!open) return
    // The canvas can be zoomed while the picker is already open, and a resize
    // is the only signal that gives.
    window.addEventListener('resize', measure)
    const onKey = (e: globalThis.KeyboardEvent) => e.key === 'Escape' && onToggle(false)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('resize', measure)
      window.removeEventListener('keydown', onKey)
    }
  }, [open, measure, onToggle])

  const n = options.length
  const closed = 'Change ' + label.toLowerCase()

  return (
    <span
      ref={host}
      className="selbar-picker"
      role="button"
      tabIndex={0}
      aria-haspopup="listbox"
      aria-expanded={open}
      aria-label={closed}
      onPointerDown={(e: ReactPointerEvent) => e.preventDefault()}
      onClick={(e: ReactMouseEvent) => {
        e.stopPropagation()
        onToggle(!open)
      }}
      onKeyDown={(e: ReactKeyboardEvent) => {
        if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return
        e.preventDefault()
        e.stopPropagation()
        onToggle(!open)
      }}
    >
      {/* A hidden copy of the CLOSED label holds the width open. Opening a
          picker must not resize the bar it sits in — everything to the right of
          it would shift under the pointer at the moment of the click. The open
          label is therefore required to be shorter than the closed one; it is
          painted over this sizer rather than replacing it. */}
      <span className="selbar-sizer" aria-hidden="true">
        {closed}
      </span>
      {open ? (
        <>
          <span ref={haze} className="selbar-haze" />
          {/* The word stays while the picker is open. A glyph can retire without
              consequence; a word leaves a hole where the thing you clicked was.
              The terminus rides inside the face so the pair centres together in
              the width the closed label reserved. */}
          <span className="selbar-face">
            <span ref={term} className="selbar-term">
              {/* Only the cross animates. The spine and the options are centred
                  against the terminus box, so anything transforming that box
                  transforms them too — the arrival scale would drag the whole
                  structure in with it. */}
              <span className="selbar-x">
                <i />
                <i />
              </span>
              <span ref={stem} className="selbar-stem" />
              <span ref={opts} className="selbar-opts" role="listbox">
                {options.map((o, i) => (
                  <button
                    key={o.id}
                    type="button"
                    role="option"
                    aria-selected={o.id === value}
                    className={'selbar-opt' + (o.blocked ? ' off' : o.id === value ? ' on' : '')}
                    style={
                      { '--d': (0.04 + (n - 1 - i) * 0.045).toFixed(3) + 's' } as CSSProperties
                    }
                    disabled={!!o.blocked}
                    onPointerDown={(e: ReactPointerEvent) => e.preventDefault()}
                    onClick={(e: ReactMouseEvent) => {
                      e.stopPropagation()
                      if (o.blocked) return
                      onPick(o)
                    }}
                  >
                    <i />
                    {o.name}
                    {o.note ? <em>{' · ' + o.note}</em> : null}
                  </button>
                ))}
              </span>
            </span>
            <span className="selbar-held">{label}</span>
          </span>
        </>
      ) : (
        <span className="selbar-face">{closed}</span>
      )}
    </span>
  )
}

// The open label is painted over a sizer holding the closed label's width, so
// a longer one would be clipped rather than widening the bar. Say so loudly in
// development rather than shipping a picker whose noun is quietly cut off.
function checkLabel(label?: string) {
  if (!label) return
  const closed = 'Change ' + label.toLowerCase()
  if (label.length >= closed.length) {
    console.warn(
      '[SelectionBar] picker label "' +
        label +
        '" is not shorter than its closed form "' +
        closed +
        '". The open label must be shorter — it is painted over the closed label\'s reserved width.',
    )
  }
}

export function SelectionBar({
  count = 0,
  label,
  actions = [],
  picker,
  danger,
  onDismiss,
  undo: undoProp,
  undoSeconds = 10,
  undoMessage,
  className = '',
  style,
}: SelectionBarProps) {
  const [open, setOpen] = useState(false)
  const [undo, setUndo] = useState<{ msg: ReactNode; revert?: () => void } | null>(null)
  const [left, setLeft] = useState(0)
  const timer = useRef<ReturnType<typeof setInterval> | null>(null)
  const [tone, setTone] = useState('alarm')

  const pickerLabel = picker?.label
  useEffect(() => {
    checkLabel(pickerLabel)
  }, [pickerLabel])

  useEffect(
    () => () => {
      if (timer.current) clearInterval(timer.current)
    },
    [],
  )

  // The bar owns the clock rather than handing seconds to Countdown: the window
  // ends by committing, and the dial and the commit have to come off one timer
  // or they disagree at the last frame.
  const startUndo = (msg: ReactNode, revert?: () => void) => {
    if (timer.current) clearInterval(timer.current)
    setUndo({ msg, revert })
    const total = undoSeconds * 1000
    const t0 = Date.now()
    setLeft(total)
    timer.current = setInterval(() => {
      const rest = total - (Date.now() - t0)
      if (rest <= 0) {
        if (timer.current) clearInterval(timer.current)
        setUndo(null)
        setLeft(0)
      } else setLeft(rest)
    }, 60)
  }

  const cancelUndo = () => {
    if (timer.current) clearInterval(timer.current)
    if (undo && undo.revert) undo.revert()
    setUndo(null)
    setLeft(0)
  }

  const showUndo = undoProp || undo
  if (showUndo) {
    const frac = undoProp ? Math.max(0, Math.min(1, undoProp.frac)) : left / (undoSeconds * 1000)
    return (
      <div className={('selbar ' + className).trim()} style={style}>
        <div className="selbar-undo">
          <span className="selbar-undo-msg">{showUndo.msg}</span>
          <span className="selbar-sep" />
          {/* The dial and the verb are one object: the window is how long Undo
              is offered for, so it reads as "◔ Undo" and the whole pair is the
              target. */}
          <button
            type="button"
            className="selbar-act selbar-undo-act"
            onClick={undoProp ? undoProp.onUndo : cancelUndo}
          >
            <Countdown frac={frac} size={14} tone="warm" label="time left to undo" />
            Undo
          </button>
        </div>
      </div>
    )
  }

  // Every verb, destructive or not, is the same kind of thing: a trigger on the
  // hold surface carrying its own duration. `danger` is that list's last entry
  // with a longer one.
  const verbs = actions.map((a) => ({
    label: a.label,
    tone: a.tone === 'bad' ? 'alarm' : a.tone || 'default',
    holdMs: a.holdMs || 0,
    onSelect: a.onSelect,
    danger: false,
  }))
  if (danger) {
    verbs.push({
      label: danger.label || 'Hold to remove',
      tone: 'alarm',
      holdMs: danger.ms != null ? danger.ms : 900,
      onSelect: danger.onConfirm,
      danger: true,
    })
  }

  const body = (
    <>
      <span className="selbar-count">{label || count + ' selected'}</span>
      <span className="selbar-sep" />
      {picker ? (
        <Picker
          label={picker.label}
          value={picker.value}
          options={picker.options || []}
          open={open}
          onToggle={setOpen}
          onPick={(o: PickerOption) => {
            setOpen(false)
            picker.onPick?.(o)
            const msg = undoMessage
              ? typeof undoMessage === 'function'
                ? undoMessage(o, count)
                : undoMessage
              : count + ' ' + (count === 1 ? 'row is' : 'rows are') + ' now ' + o.name + '.'
            startUndo(msg, picker.onRevert)
          }}
        />
      ) : null}
      {verbs.map((v, i) => (
        <span
          key={'a' + i}
          className="selbar-act"
          // Focusable per verb: the bar carries several at different prices, so
          // a keypress has to land on one of them to mean anything. This is
          // also the bar's accessible naming — Hold's surface wrapper claims no
          // name, so each verb has to say both what it does and that it is held.
          tabIndex={0}
          role="button"
          aria-label={v.holdMs > 0 ? v.label + ' — press and hold to confirm' : v.label}
          data-tone={v.tone}
          data-selbar-hold=""
          data-selbar-danger={v.danger ? '' : undefined}
          data-hold-ms={v.holdMs}
          data-verb={i}
          // The rings take the tone of the verb being pressed, so a removal
          // reads alarm and an ordinary verb does not borrow its weight.
          onPointerDown={() => setTone(v.tone === 'alarm' ? 'alarm' : 'warm')}
          onKeyDown={(e: ReactKeyboardEvent) => {
            if (e.key === ' ' || e.key === 'Enter') setTone(v.tone === 'alarm' ? 'alarm' : 'warm')
          }}
        >
          {v.label}
        </span>
      ))}
      {onDismiss ? (
        <>
          <span className="selbar-sep selbar-sep-tail" />
          <button
            type="button"
            className="selbar-dismiss"
            onClick={onDismiss}
            aria-label="Clear selection"
          >
            {'×'}
          </button>
        </>
      ) : null}
    </>
  )

  return (
    <div className={('selbar ' + className).trim()} style={style}>
      {/* The bar IS the hold surface, so the rings cross everything — the commit
          is about the selection, not about the word you pressed. With no verbs
          there is nothing to measure and a plain row keeps the same rhythm. */}
      {verbs.length ? (
        <Hold
          variant="surface"
          tone={tone}
          trigger="[data-selbar-hold]"
          steps={(danger && danger.steps) || 10}
          onConfirm={(el: HTMLElement | null) => {
            const v = verbs[el ? +el.dataset.verb! : -1]
            if (v && v.onSelect) v.onSelect()
          }}
        >
          {body}
        </Hold>
      ) : (
        <div className="selbar-row">{body}</div>
      )}
    </div>
  )
}

export default SelectionBar
