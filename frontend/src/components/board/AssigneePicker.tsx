import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import type { CSSProperties, KeyboardEvent as ReactKeyboardEvent } from 'react'
import type { Task, TeamBot, TeamMember } from '../../types'
import './assignee-picker.css'

// AssigneePicker — the design language document's picker, on the board's
// claim model. The interaction pattern is settled; treat every constant here
// as fixed.
//
// Assignment is a scanning fact, so the mark is always visible, never
// hover-only. A person is a round mark with initials, an agent a squared one,
// nobody a dash: distinguished by SHAPE, never by color.
//
// The picker DRAWS ITSELF rather than appearing: the mark retires, a terminus
// takes its place, a stem grows out of it, ticks extend and labels arrive
// along them. The page recedes behind it, and the card in hand lifts above
// the recede (task-card.css).
//
// It never scrolls. Past the browse threshold it filters: five rows, always,
// just a different five. A scroll container needs a bounded height, which is
// a rectangle, which is the panel this treatment exists to avoid.
//
// One measurement is taken from the DOM rather than hardcoded, and it is
// load-bearing: the stem terminates exactly at the FARTHEST option's tick, so
// the join reads as a corner rather than a T. Measured, so the option count
// is free rather than baked into a magic height.
//
// What a row DOES follows from who it names. Picking the agent delegates
// (the parent opens the blueprint picker); picking yourself claims — a
// self-claim, or a takeover from the bot or another person, which the server
// judges; picking a teammate reassigns, which the server also judges; picking
// Unassign returns the task to the queue, through the same door the drag
// takes. A row that names the current holder is marked and inert.

// Only one picker is open at a time. Opening one announces itself and every
// other instance stands down.
//
// An event rather than a shared store or a context: pickers mount on cards
// across independent trees, and none of them owns the others. The outside-
// click handler cannot do this job, because a mark's own onClick stops
// propagation before the document sees it, which is exactly how two ever
// ended up open.
const OPENED = 'tf-assign-opened'

const BROWSE_MAX = 6
const SHOWN = 5

export type AssigneeKind = 'agent' | 'me' | 'user' | 'none'

interface Entry {
  key: string
  name: string
  kind: AssigneeKind
}

const UNASSIGN: Entry = { key: 'none', name: 'Unassign', kind: 'none' }

function rank(
  roster: Entry[],
  q: string,
  defaults: Entry[],
): { list: Entry[]; rest: number; browse: boolean } {
  // Small roster: show everyone, no filter, nothing hidden.
  if (roster.length <= BROWSE_MAX) return { list: roster, rest: 0, browse: true }
  // Empty query is not "the first five alphabetically" — it is the five you
  // actually pick: the agent, yourself, the first of the rest, and the escape
  // hatch.
  if (!q.trim()) return { list: defaults, rest: roster.length - defaults.length, browse: false }
  const n = q.trim().toLowerCase()
  const starts: Entry[] = []
  const has: Entry[] = []
  for (const e of roster) {
    const l = e.name.toLowerCase()
    if (l.startsWith(n) || l.split(/[\s·]+/).some((w) => w.startsWith(n))) starts.push(e)
    else if (l.includes(n)) has.push(e)
  }
  const all = starts.concat(has)
  return { list: all.slice(0, SHOWN), rest: Math.max(0, all.length - SHOWN), browse: false }
}

function initials(e: Entry | null): string {
  if (!e || e.kind === 'none') return '—'
  const name = e.kind === 'agent' ? e.name.replace(/^Agent · /, '') : e.name
  return name
    .split(/\s+/)
    .filter(Boolean)
    .map((w) => w[0])
    .join('')
    .slice(0, 2)
    .toUpperCase()
}

function markClass(e: Entry | null): string {
  return 'who' + (e?.kind === 'agent' ? ' bot' : !e || e.kind === 'none' ? ' none' : '')
}

// ONE scrim for the page, created on first use and shared by every instance.
//
// Not one per picker, and not a child of the card: only one menu is ever open,
// and an overlay rendered inside the card would paint over that card's own
// background — the one surface the treatment exists to keep crisp. It lives on
// the body so the open card can lift above it (see task-card.css), which
// requires the two to be siblings.
//
// A shared node written by per-instance effects needs a single source of truth,
// or whichever effect ran last wins. Reassigning from one card to another is
// the common path on a board, and React runs sibling cleanups and effects in
// TREE order rather than paired per instance — so the closing card's
// `recedeTo(false)` lands after the opening card's `recedeTo(true)` and the page
// quietly stops receding. Hence a set of open instances, and one writer that
// reads it once per microtask, after every effect in the commit has had its say.
const SCRIM = 'tf-assign-scrim'
const RECEDING = new Set<string>()
let scrimQueued = false

function scrimNode(): HTMLElement {
  let el = document.getElementById(SCRIM)
  if (!el) {
    el = document.createElement('div')
    el.id = SCRIM
    el.setAttribute('aria-hidden', 'true')
    document.body.appendChild(el)
    // Force a style resolution so the node has a settled opacity of 0 to
    // interpolate FROM. Without it the first fade of the session is a cut.
    void el.offsetHeight
  }
  return el
}

function flushScrim() {
  scrimQueued = false
  const el = document.getElementById(SCRIM)
  if (el) el.classList.toggle('on', RECEDING.size > 0)
}

function recedeTo(key: string, on: boolean) {
  if (typeof document === 'undefined') return
  if (on) RECEDING.add(key)
  else RECEDING.delete(key)
  // The node is created synchronously, because it needs its base style settled
  // before any class lands on it. The class itself waits for a MICROTASK: that
  // is late enough to see the final state of the commit, and unlike a frame it
  // cannot be throttled — an offscreen or background tab defers
  // requestAnimationFrame indefinitely, which showed up as a scrim carrying
  // .on with an opacity that had not started moving.
  if (RECEDING.size > 0) scrimNode()
  if (scrimQueued) return
  scrimQueued = true
  Promise.resolve().then(flushScrim)
}

// Whether the menu can be lifted into the browser's top layer. There it is
// painted outside every ancestor's overflow and above every stacking context,
// while the node itself stays under its host — so the card's :has() lift, the
// outside-click containment check, keyboard routing and aria all keep working
// unchanged. The same route Tooltip takes, for the same reason. Without it the
// menu stays an ordinary child and flips downward when the column has no room.
const TOP_LAYER = typeof HTMLElement !== 'undefined' && 'showPopover' in HTMLElement.prototype

// Screen px per CSS px. getBoundingClientRect reports the former and every
// value written back to a style is the latter, so anything that scales the
// document needs this divided out or the measurement overshoots by exactly
// the zoom factor.
const scaleOf = (a: HTMLElement) =>
  (a.offsetWidth ? a.getBoundingClientRect().width / a.offsetWidth : 1) || 1

interface Props {
  task: Task
  currentUserID: string
  members: TeamMember[]
  bot: TeamBot | null
  onClaim: (task: Task) => Promise<void>
  onUnclaim: (task: Task) => Promise<void>
  onDelegate: (task: Task) => void
  onReassign: (task: Task, targetUserID: string) => Promise<void>
  // Read-only keeps the MARK and drops the picker. Assignment is a scanning
  // fact — a lane is read by running down the marks — so a reader who cannot
  // reassign still needs to see who owns what.
  readOnly?: boolean
  style?: CSSProperties
}

export default function AssigneePicker({
  task,
  currentUserID,
  members,
  bot,
  onClaim,
  onUnclaim,
  onDelegate,
  onReassign,
  readOnly = false,
  style,
}: Props) {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState('')
  // -1 is not "row 0" — it is the filter, which is a real position in this
  // structure rather than the absence of one. Arrowing down off the bottom row
  // returns here, which is where typing goes.
  const [cursor, setCursor] = useState(-1)
  // The close has a TAIL. z-index cannot be transitioned, so the card's lift
  // above the scrim is boolean — and if it drops when .open does, the card
  // spends the scrim's whole fade sitting behind it: it dims hard, then
  // brightens as the scrim goes. That pop is what this exists to prevent. The
  // class outlives the open state by exactly the scrim's fade.
  const [closing, setClosing] = useState(false)
  // The tail begins the moment the menu closes, decided at render rather than
  // in an effect so no frame paints the card dropped behind a scrim still at
  // full strength.
  const [prevOpen, setPrevOpen] = useState(open)
  if (open !== prevOpen) {
    setPrevOpen(open)
    if (!open) setClosing(true)
  }
  // Which way the structure grows. Not a preference: a lane is a scroll
  // container, so a menu drawn upward out of a card near the lane's top is
  // clipped away entirely. In the top layer nothing clips, so it always grows
  // upward — the language's resting form, and the one that never lands on
  // the card.
  const [side, setSide] = useState<'up' | 'down'>('up')
  const closeTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const id = useId()
  const host = useRef<HTMLSpanElement>(null)
  const stem = useRef<HTMLSpanElement>(null)
  const input = useRef<HTMLInputElement>(null)
  const picker = useRef<HTMLSpanElement>(null)
  const lifted = TOP_LAYER

  // The roster, in the order the empty query shows it: the agent, yourself,
  // the rest of the team, and the escape hatch.
  const me = members.find((m) => m.is_current_user || m.user_id === currentUserID)
  const meName = me?.display_name || me?.github_username || 'Me'
  const roster: Entry[] = []
  if (bot)
    roster.push({ key: 'agent', name: `Agent · ${bot.display_name || 'bot'}`, kind: 'agent' })
  roster.push({ key: 'me', name: meName, kind: 'me' })
  for (const m of members) {
    if (m.is_current_user || m.user_id === currentUserID) continue
    roster.push({
      key: m.user_id,
      name: m.display_name || m.github_username || m.user_id,
      kind: 'user',
    })
  }
  // Who holds it, as the mark and the marked row. The holder must be a
  // roster entry, not just a value compared against one: an agent claim with
  // no bot configured for this team, or a user who has left it, names
  // someone the loops above never added, and a fallback entry that isn't in
  // `roster` can't appear in `list` — the holder's row would be unmarked or
  // simply absent rather than inert.
  let holder: Entry | null = null
  if (task.claimed_by_agent_id) {
    holder = roster.find((e) => e.kind === 'agent') ?? null
    if (!holder) {
      holder = { key: 'agent', name: 'Agent', kind: 'agent' }
      roster.unshift(holder)
    }
  } else if (task.claimed_by_user_id) {
    if (task.claimed_by_user_id === currentUserID && currentUserID !== '') {
      holder = roster.find((e) => e.kind === 'me')!
    } else {
      holder = roster.find((e) => e.key === task.claimed_by_user_id) ?? null
      if (!holder) {
        holder = {
          key: task.claimed_by_user_id,
          name: 'User ' + task.claimed_by_user_id.slice(0, 8),
          kind: 'user',
        }
        roster.push(holder)
      }
    }
  }

  roster.push(UNASSIGN)
  const defaults = [...roster.slice(0, SHOWN - 1), UNASSIGN]

  const { list, rest, browse } = rank(roster, q, defaults)

  // A popover is positioned against the viewport, so the picker's box — which
  // everything inside it is drawn relative to — is placed over the mark's
  // rect. Sized in the mark's own CSS px and scaled about the bottom-right
  // corner by whatever the document is scaled by.
  const placePicker = useCallback(() => {
    const a = host.current
    const p = picker.current
    if (!a || !p || !lifted) return
    const r = a.getBoundingClientRect()
    const s = scaleOf(a)
    p.style.width = a.offsetWidth + 'px'
    p.style.height = a.offsetHeight + 'px'
    p.style.left = r.right - a.offsetWidth * s + 'px'
    p.style.top = r.bottom - a.offsetHeight * s + 'px'
    p.style.transformOrigin = 'right bottom'
    p.style.transform = Math.abs(s - 1) > 0.001 ? 'scale(' + s + ')' : ''
  }, [lifted])

  // The frame is the nearest ancestor that clips, or the viewport. Whichever
  // side has room wins; ties go up, which is the language's resting form.
  const chooseSide = useCallback(() => {
    if (lifted) return setSide('up')
    const a = host.current
    if (!a) return
    const opts = a.querySelector<HTMLElement>('.opts')
    if (!opts) return
    const s = scaleOf(a)
    const r = a.getBoundingClientRect()
    let top = 0
    let bottom = window.innerHeight
    let p = a.parentElement
    while (p && p !== document.body) {
      const cs = getComputedStyle(p)
      if (cs.overflow !== 'visible' || cs.overflowY !== 'visible') {
        const pr = p.getBoundingClientRect()
        top = Math.max(top, pr.top)
        bottom = Math.min(bottom, pr.bottom)
        break
      }
      p = p.parentElement
    }
    // The block, plus the stem's foot at the terminus.
    const need = opts.offsetHeight + 26
    const above = (r.top - top) / s
    const below = (bottom - r.bottom) / s
    setSide(above >= need || above >= below ? 'up' : 'down')
  }, [lifted])

  // Shown for the whole of its life, not per open. A hidden popover is
  // display:none, and an element that goes from none to shown in the same
  // commit that adds .open has no previous style to transition FROM — the
  // structure would appear rather than draw itself. Held visible, the picker
  // keeps its visibility gate and its transitions; the top layer only changes
  // where it is painted. Declared before the measurement effect, because a
  // hidden popover measures as nothing.
  useLayoutEffect(() => {
    const el = picker.current
    if (!el || !lifted) return
    try {
      if (!el.matches(':popover-open')) el.showPopover()
    } catch {
      /* already shown, or not connected yet */
    }
    return () => {
      try {
        el.hidePopover()
      } catch {
        /* already hidden */
      }
    }
  }, [lifted])

  const sizeStem = useCallback(() => {
    const a = host.current
    if (!a || !stem.current) return
    const rows = a.querySelectorAll<HTMLElement>('.opt, .frow')
    if (!rows.length) return
    const aR = a.getBoundingClientRect()
    const s = scaleOf(a)
    const terminusY = aR.bottom - 9 * s // ring centre
    // The FARTHEST ticked row, not the first one: which row that is depends on
    // which way the block grew, and the stem's job is to reach the end of it
    // either way. Ticks live on the options and the filter row; the count has
    // none, so it is not a terminus for anything.
    let far = 0
    rows.forEach((o) => {
      const r = o.getBoundingClientRect()
      far = Math.max(far, Math.abs(terminusY - (r.top + r.height / 2)))
    })
    stem.current.style.height = Math.round(far / s) + 'px'
  }, [])

  useLayoutEffect(() => {
    placePicker()
    // Which side has room is a fact of layout, which only an effect can read.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- measured from layout
    chooseSide()
    sizeStem()
  })

  useEffect(() => {
    // A card's picker only needs to know about the outside world while its
    // own menu is up — closed, there is nothing on screen to keep aligned.
    // Wiring these unconditionally means one resize listener and one
    // ResizeObserver per card on the board, all firing on every scroll,
    // whether or not that card's picker has ever been opened.
    if (!open) return
    const remeasure = () => {
      placePicker()
      chooseSide()
      sizeStem()
    }
    window.addEventListener('resize', remeasure)
    // A lane scrolling under an open menu changes which side has room, and
    // capture catches it on the lane rather than only on the window.
    document.addEventListener('scroll', remeasure, true)
    // The stem is stale the moment anything reflows, and the render pass that
    // took it cannot know that. A web font arriving after first paint is the
    // common case; a resize listener never sees it, because the window never
    // resized.
    if (document.fonts?.ready) document.fonts.ready.then(remeasure)
    let ro: ResizeObserver | undefined
    if (typeof ResizeObserver !== 'undefined') {
      ro = new ResizeObserver(remeasure)
      ro.observe(document.documentElement)
      const opts = host.current?.querySelector('.opts')
      if (opts) ro.observe(opts)
    }
    return () => {
      window.removeEventListener('resize', remeasure)
      document.removeEventListener('scroll', remeasure, true)
      ro?.disconnect()
    }
  }, [open, sizeStem, chooseSide, placePicker])

  // Closing returns the focus it took. Without this, dismissing with Escape
  // leaves the tab ring on a button that is now invisible, and the next Tab
  // starts from wherever that was rather than from the card.
  const close = useCallback((refocus?: boolean) => {
    setOpen(false)
    setCursor(-1)
    if (refocus && host.current) {
      const mark = host.current.querySelector<HTMLElement>('.who')
      if (mark) mark.focus()
    }
  }, [])

  useEffect(() => {
    if (!open) return
    const onOther = (e: Event) => {
      if ((e as CustomEvent<string>).detail !== id) setOpen(false)
    }
    document.addEventListener(OPENED, onOther)
    return () => document.removeEventListener(OPENED, onOther)
  }, [open, id])

  useEffect(() => {
    if (!open) return
    // A click outside CLOSES, and does nothing else. Capture phase on the
    // document, and the event is stopped there: React binds its own handlers
    // on the root container, which is a descendant, so stopping here means the
    // click never reaches anything underneath. Without it, clicking a second
    // mark closed this menu and opened that one in the same gesture, and any
    // control under the receded page stayed live while the page said it was
    // not. Reassigning to a different card is therefore two clicks: one to
    // dismiss, one to open. That is the intended cost.
    //
    // Both events, because either one alone leaks: click is what activates a
    // button, pointerdown is what moves focus and starts a drag.
    const swallow = (e: Event) => {
      if (host.current && host.current.contains(e.target as Node)) return
      e.preventDefault()
      e.stopPropagation()
      close()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(true)
    }
    document.addEventListener('pointerdown', swallow, true)
    document.addEventListener('click', swallow, true)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('pointerdown', swallow, true)
      document.removeEventListener('click', swallow, true)
      window.removeEventListener('keydown', onKey)
    }
  }, [open, close])

  const pick = (e: Entry) => {
    close()
    setQ('')
    // The row naming the current holder is inert: there is nothing to move.
    if (holder && holder.key === e.key) return
    switch (e.kind) {
      case 'agent':
        onDelegate(task)
        return
      case 'me':
        void onClaim(task)
        return
      case 'user':
        void onReassign(task, e.key)
        return
      case 'none':
        if (holder) void onUnclaim(task)
        return
    }
  }

  // Ended by the scrim's own transitionend rather than a matching timer. Two
  // durations that have to agree are two durations that will disagree: the
  // fade does not begin at the click but at the next commit, so a hand-tuned
  // duration drops the lift with the scrim still at a fifth of its strength —
  // the same flash, quieter. The event knows exactly when the page is back.
  useEffect(() => {
    if (!closing) return
    const done = () => setClosing(false)
    const el = document.getElementById(SCRIM)
    if (el) el.addEventListener('transitionend', done, { once: true })
    // A scrim that is absent, already clear, or under reduced motion may fire
    // nothing at all, so the tail still needs a floor.
    const tk =
      parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--tk-assign')) || 1
    clearTimeout(closeTimer.current)
    closeTimer.current = setTimeout(done, 700 * tk)
    return () => {
      if (el) el.removeEventListener('transitionend', done)
      clearTimeout(closeTimer.current)
    }
  }, [closing])
  useEffect(() => () => clearTimeout(closeTimer.current), [])

  // The page recedes while a menu is open. Owned here rather than by the board:
  // the picker is what knows, and one shared element means the state cannot
  // disagree with itself across lanes.
  useEffect(() => {
    recedeTo(id, open)
    return () => recedeTo(id, false)
  }, [open, id])

  // Arrow keys walk the visible rows. The list is drawn bottom-up, so Up moves
  // toward index 0 and entering from the filter starts at the bottom row — the
  // one nearest the cursor's own position.
  const onKeys = (e: ReactKeyboardEvent) => {
    if (!open) return
    const last = list.length - 1
    if (e.key === 'Escape') {
      e.preventDefault()
      close(true)
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((c) => (c === -1 ? last : Math.max(0, c - 1)))
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((c) => (c === -1 || c === last ? -1 : c + 1))
      return
    }
    if (e.key === 'Home') {
      e.preventDefault()
      setCursor(0)
      return
    }
    if (e.key === 'End') {
      e.preventDefault()
      setCursor(last)
      return
    }
    if (e.key === 'Enter' && list.length) {
      e.preventDefault()
      // The cursor if one is set, otherwise the best match — which is the top
      // row, because ranking puts prefix hits first.
      pick(cursor >= 0 ? list[cursor] : list[0])
    }
  }

  const title = holder ? holder.name : 'Unassigned'

  if (readOnly)
    return (
      <span className="assign" style={style}>
        <span className={markClass(holder) + ' static'} title={title}>
          {initials(holder)}
        </span>
      </span>
    )

  return (
    <span
      ref={host}
      className={'assign' + (open ? ' open' : '') + (closing ? ' closing' : '')}
      data-side={side}
      style={style}
      onClick={(e) => e.stopPropagation()}
      onPointerDown={(e) => e.stopPropagation()}
      onKeyDown={onKeys}
    >
      <button
        type="button"
        className={markClass(holder)}
        title={title}
        aria-label={holder ? `Assigned to ${holder.name}` : 'Unassigned'}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={(e) => {
          e.stopPropagation()
          const was = open
          setOpen(!was)
          setCursor(-1)
          if (!was) {
            document.dispatchEvent(new CustomEvent(OPENED, { detail: id }))
            setQ('')
            // The filter takes focus when there is one. A browsable roster has
            // none, so focus goes to the bottom row instead — the structure
            // still has a first position, and it is the one by the mark.
            setTimeout(() => {
              if (input.current) return input.current.focus()
              const opts = host.current?.querySelectorAll<HTMLElement>('.opt')
              if (opts && opts.length) opts[opts.length - 1].focus()
            }, 60)
          }
        }}
      >
        {initials(holder)}
      </button>

      <span className="picker" ref={picker} popover={lifted ? 'manual' : undefined}>
        {/* The terminus is the close: while open it stands exactly where the
            mark was, so the control opens and shuts under one cursor. The mark
            beneath it is pointer-events:none, which is why this needs its own
            handler rather than inheriting one. */}
        <button
          type="button"
          className="term"
          aria-label="Close"
          tabIndex={open ? 0 : -1}
          onClick={(e) => {
            e.stopPropagation()
            close(true)
            setQ('')
          }}
        />
        <span className="stem" ref={stem} />
        <span className="opts" role="listbox" id={id + '-list'} aria-label="Assignee">
          {/* Newest-to-oldest visually: the list grows upward away from the filter. */}
          {list.map((e, i) => (
            <button
              key={e.key}
              type="button"
              role="option"
              id={id + '-o' + i}
              aria-selected={holder?.key === e.key}
              className={
                'opt' + (holder?.key === e.key ? ' on' : '') + (cursor === i ? ' cursor' : '')
              }
              style={{ '--i': list.length - 1 - i, '--j': i } as CSSProperties}
              tabIndex={open ? 0 : -1}
              onMouseEnter={() => setCursor(i)}
              onClick={(ev) => {
                ev.stopPropagation()
                pick(e)
              }}
            >
              {e.name}
            </button>
          ))}
          {/* The count stays SHORTER than a name: as the longest string in the
              block it would define the left edge and read as misaligned
              against the spine. */}
          {rest > 0 && <span className="rest">+{rest} more</span>}
          {!browse && (
            <span className="frow">
              <input
                ref={input}
                className="filter"
                type="text"
                value={q}
                placeholder="Assign to…"
                aria-label="Filter people"
                role="combobox"
                aria-expanded={open}
                aria-controls={id + '-list'}
                aria-activedescendant={cursor >= 0 ? id + '-o' + cursor : undefined}
                tabIndex={open ? 0 : -1}
                onClick={(e) => e.stopPropagation()}
                onChange={(e) => {
                  setQ(e.target.value)
                  // A new query is a new list; a held cursor would point at a
                  // different name than the one it was put on.
                  setCursor(-1)
                }}
              />
            </span>
          )}
        </span>
      </span>
    </span>
  )
}
