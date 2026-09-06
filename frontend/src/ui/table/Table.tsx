import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from 'react'
import Checkbox from '../checkbox/Checkbox'
import SelectionBar from '../selectionbar/SelectionBar'
import type { PickerOption, SelectionBarProps } from '../selectionbar/SelectionBar'
import { tableCells, tablePages } from './cells'
import './table.css'

/**
 * A row is whatever the caller puts in it. `any` rather than `unknown` on
 * purpose: this is a generic data table, and `unknown` would push a cast into
 * every renderer a caller writes — which is the opposite of what the cell
 * registry exists for. The narrowing belongs in the caller's column
 * definitions, where the shape is actually known.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type TableRow = Record<string, any> & { id: string | number }

/** Someone the draft page could add — the shape `add.options` supplies. */
export type TableCandidate = {
  id?: string
  name: string
  disabled?: boolean
  disabledNote?: string
}

export type TableColumn = {
  key: string
  label?: string
  /** A registry type supplying defaults: text | ago | identity | spark | toggle | mark | bar. */
  type?: string
  align?: 'start' | 'end' | 'center'
  /** false draws the cell in sans at 12.5px instead of mono at 11px. */
  mono?: boolean
  width?: string
  /** The width below which a flexible column stops being readable. Default 150. */
  floor?: number
  /** Drop order when the table runs out of room. 1 goes first; no drop never goes. */
  drop?: number
  sortable?: boolean
  render?: (row: TableRow, col: TableColumn) => ReactNode
  measure?: (row: TableRow, col: TableColumn) => string | number
  sortValue?: (row: TableRow, col: TableColumn) => string | number
  color?: (row: TableRow) => string
  /**
   * The registry escape hatch: a column may override anything a cell type
   * supplies, and a caller may register a type of its own with keys this
   * component has never heard of.
   */
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  [k: string]: any
}

export type TableAction = {
  id: string
  label: string
  tone?: 'bad' | string
  holdMs?: number
  message?: (n: number, pick?: PickerOption) => string
  enabledFor?: (rows: TableRow[]) => boolean
}

export type TableProps = {
  columns?: TableColumn[]
  rows?: TableRow[]
  actions?: TableAction[]
  label?: string
  headerAction?: ReactNode
  /**
   * A "+ add teammate" affordance, immediately left of the filter. Off unless
   * the table's estate is one the reader can grow from here.
   */
  add?: {
    label: string
    placeholder?: string
    hint?: string
    options?: TableCandidate[]
    fields?: Array<{ key: string; options?: string[]; value?: string }>
    onAdd?: (option: TableCandidate, values: Record<string, string>) => void
    onSelect?: () => void
  } | null
  footer?: ReactNode
  selectable?: boolean
  mutate?: ((row: TableRow, actionId: string, pick?: PickerOption) => TableRow | null) | null
  /**
   * The only place a request belongs. Called once, when the undo window closes
   * — never when it opens, because the whole point is that an undone action is
   * never sent.
   *
   * `pick` is the option a picker verb resolved to, because a caller that can
   * APPLY a role change has to be able to SEND it; without it the table shows a
   * change the server never hears about.
   *
   * `reason` says why the window closed, because the answer changes the
   * transport. On 'unload' an ordinary fetch is killed with the page: use
   * `keepalive: true` (not sendBeacon, which is POST-only and cannot express a
   * PATCH or a DELETE).
   */
  onCommit?:
    | ((
        actionId: string,
        ids: Array<string | number>,
        ctx: { pick?: PickerOption; reason: TableCommitReason },
      ) => void)
    | null
  emptyLabel?: string
  /**
   * Pages, not a scroller: a long estate read in fixed-height screens, so the
   * row under your cursor never moves and the count below is always true.
   * 'auto' reads the height the table has been given and shows as many rows as
   * fit — the same fixed frame, sized by the window instead of by hand.
   */
  pageSize?: number | 'auto'
  /** Selection verbs, in the floating bar the rest of the system uses.
   *
   *  Every verb here is gated by its action's `enabledFor`, so a selection a
   *  verb cannot apply to is a selection that is not offered it. A picker is
   *  gated one choice at a time instead: give `options` a function and it is
   *  resolved against the selected rows, so a choice that would not apply
   *  cleanly is not drawn while the ones that would still are. */
  bar?: {
    actions?: TableAction[]
    picker?: Omit<NonNullable<SelectionBarProps['picker']>, 'options'> & {
      action?: TableAction
      options?: PickerOption[] | ((rows: TableRow[]) => PickerOption[])
    }
    danger?: NonNullable<SelectionBarProps['danger']> & { action?: TableAction }
  } | null
  /** 'absolute' pins the bar in the nearest positioned ancestor; 'fixed' to the viewport. */
  barPosition?: 'fixed' | 'absolute' | 'inline'
  sortKey?: string
  sortDir?: 1 | -1
  /** Height reserved for an inline bar. Must still clear the bar itself (~38px). */
  barSlot?: number
  headerRight?: ReactNode
  headerLeft?: ReactNode
  showHeader?: boolean
  /** A row that is the one — the default model, the current release. */
  rowBg?: ((row: TableRow) => string | null) | null
  /** The table arriving as a scan: a beam runs down the rows and each resolves in its wake. */
  build?: boolean
  /**
   * A row that OPENS. When present, a click on the row is this rather than a
   * selection toggle, and the checkbox alone selects — for a table whose rows
   * lead somewhere (a token's sheet) while the bulk verbs still want a
   * selection. Absent, the row click toggles as it always has.
   */
  onRowOpen?: ((row: TableRow) => void) | null
}

// Table — a selectable, sortable table with an undo window on bulk actions.
// Generic: it knows about columns, rows, selection and time. It knows nothing
// about people; a people table is this component with people columns.
//
// The undo model is the one Gmail, Slack and Linear use: the mutation is applied
// to what you see IMMEDIATELY and the request is held client-side for the length
// of the countdown. Undo means the request is never sent, so there is no
// compensating write and no window where the server disagrees with the screen.
// (The alternative — send now, ask the server to reverse it — needs every
// mutation to be invertible server-side, which role changes and removals from a
// team are not, cleanly.)

const UNDO_MS = 10000

// Height reserved under an inline bar. The slot is drawn whether or not a bar
// is in it, so selecting a row cannot move the table it belongs to.
// No measured column may take more than this; past it a value is long enough
// that clipping it costs less than the left column loses.
const CEILING = 240

const BAR_SLOT = 54
// One row: 17px of line between 9px of padding. A short last page holds the
// difference open so the table does not change height as you page through it.
const ROW_H = 35

/** One chip in the pager: a numbered page, an arrow, or the draft page's `+`. */
type PagerButton = {
  k: string
  glyph: string
  to?: number
  live: boolean
  on?: boolean
  draft?: boolean
  label?: string
}

/**
 * Why the undo window closed.
 *
 *   window     — it ran out, or a second action cut it short. The normal path.
 *   navigation — the table unmounted with a window still open. The action was
 *                applied on screen, so dropping it here would silently discard
 *                a change the reader watched happen.
 *   unload     — the page is going away. Same obligation, less time: an
 *                ordinary fetch dies with the document.
 */
export type TableCommitReason = 'window' | 'navigation' | 'unload'

/** The held mutation: applied to what you see, not yet sent. */
type TablePending = {
  actionId: string
  ids: Array<string | number>
  snapshot: TableRow[]
  label: string
  left: number
  /** The picked option, carried so onCommit can send what mutate applied. */
  pick?: PickerOption
}

// A type supplies defaults; the column overrides them.
function columnDef(c: TableColumn): TableColumn {
  const t = c.type && tableCells[c.type]
  if (!t) return c
  const out: TableColumn = { ...(t as TableColumn) }
  for (const k in c) if (c[k] !== undefined) out[k] = c[k]
  return out
}

export function Table({
  columns = [],
  rows = [],
  actions = [],
  label = '',
  headerAction = null,
  add = null,
  footer = null,
  selectable = true,
  mutate = null,
  onCommit = null,
  emptyLabel = 'nothing here',
  pageSize = 0,
  bar = null,
  barPosition = 'fixed',
  sortKey = '',
  sortDir = 1,
  barSlot = BAR_SLOT,
  headerRight = null,
  headerLeft = null,
  showHeader = true,
  rowBg = null,
  build = false,
  onRowOpen = null,
}: TableProps) {
  const cols = useMemo(() => columns.map(columnDef), [columns])
  const [working, setWorking] = useState(rows)
  const [sel, setSel] = useState<Record<string, boolean>>({})
  const [sort, setSort] = useState({
    k: sortKey || (columns[0] ? columns[0].key : ''),
    d: sortDir as number,
  })
  // The held mutation: applied to what you see, not yet sent. `snapshot` is
  // what Undo puts back, `left` is what the dial draws, and `onCommit` fires
  // only when the window closes.
  const [pending, setPending] = useState<TablePending | null>(null)
  const [page, setPage] = useState(0)
  const [tip, setTip] = useState<{ key: string; text: string; left: number } | null>(null)
  const [building, setBuilding] = useState(build)

  // The beam leads the rows, so the run outlasts the last row's own fill.
  // `build` flipping is adjusted during render rather than synchronised in the
  // effect: the effect owns the 900ms tail and nothing else, so the entrance
  // cannot paint one frame in the wrong state on its way in.
  const [builtFor, setBuiltFor] = useState(build)
  if (build !== builtFor) {
    setBuiltFor(build)
    setBuilding(build)
  }
  useEffect(() => {
    if (!build) return
    const t = setTimeout(() => setBuilding(false), 900)
    return () => clearTimeout(t)
  }, [build])

  const anchor = useRef<string | number | null>(null)
  const timer = useRef<ReturnType<typeof setInterval> | null>(null)

  // The pending mutation, mirrored where a cleanup function and an unload
  // listener can reach it — neither can read state, and both have to be able to
  // send what is owed.
  const pendingRef = useRef<TablePending | null>(null)
  const commitRef = useRef(onCommit)
  useEffect(() => {
    pendingRef.current = pending
    commitRef.current = onCommit
  })

  /**
   * Send what is owed, once. Everything that ends a window goes through here so
   * there is exactly one place a request is made and exactly one place the
   * pending is cleared — a second caller finds it already gone.
   */
  const commitPending = useCallback((reason: TableCommitReason) => {
    const p = pendingRef.current
    if (!p) return
    pendingRef.current = null
    if (timer.current) clearInterval(timer.current)
    commitRef.current?.(p.actionId, p.ids, { pick: p.pick, reason })
  }, [])

  // Adjusted during render, not in an effect: an effect would paint the
  // previous rows once and then replace them, which on a table that just
  // received new data is a visible flash of the old estate.
  const [rowsFor, setRowsFor] = useState(rows)
  if (rows !== rowsFor) {
    setRowsFor(rows)
    setWorking(rows)
  }

  // One sizing rule, so no table needs a hand-tuned pixel width. A
  // left-justified column runs right until it meets the next left-justified
  // column or the table edge. An end- or center-justified column takes the
  // width of its own content: the wider of its header label and its widest
  // cell, measured across EVERY row rather than the visible page, so paging
  // never shifts the grid. Both clip when the table itself runs out of room.
  // An explicit width still wins, for the rare column that must not move.
  //
  // Measured in two steps. First every column says what it needs in px; then
  // the table compares the total against the room it actually has and drops
  // columns until it fits. Dropping is opt-in per column (`drop: 1` goes
  // first, `drop: 2` next, no `drop` never goes), so a table that must keep
  // all its columns behaves exactly as before and clips instead.
  const sizes = useMemo(() => {
    // Not asserted non-null: a 2d context is absent in jsdom and in a browser
    // with canvas blocked by a privacy extension, and today that crashed the
    // whole table on its first render rather than sizing it approximately.
    const cv = document.createElement('canvas').getContext('2d')
    // canvas ignores a font it cannot parse and silently keeps the previous
    // one, so the stacks are resolved to literals here (var() is never valid)
    // and every assignment is read back before it is trusted.
    const css = getComputedStyle(document.body)
    const MONO = css.getPropertyValue('--font-mono').trim() || '"DM Mono", monospace'
    const SANS = css.getPropertyValue('--font-sans').trim() || 'Archivo, sans-serif'
    const measure = (text: string | number, font: string) => {
      // Same estimate the unparseable-font path below uses: roughly 0.6em per
      // character. Wrong in the third decimal, right about which column is
      // wider, which is all the sizing pass asks of it.
      if (!cv) return String(text).length * parseFloat(font) * 0.6
      cv.font = font
      if (cv.font.indexOf('sans-serif') === 0) return String(text).length * parseFloat(font) * 0.6
      return cv.measureText(String(text)).width
    }
    const track = (c: TableColumn) => {
      if (c.width && c.width !== 'auto') return c.width
      if (c.align !== 'end' && c.align !== 'center') return 'minmax(0,1fr)'
      // The caps label is tracked at .14em, which canvas does not apply.
      const head = c.label ? measure(c.label, '400 8px ' + MONO) + c.label.length * 1.12 + 10 : 0
      let body = 0
      for (const r of rows) {
        // A cell that draws a node says what it is as wide as through
        // `measure`; without one it falls back to its header's width.
        const v = c.measure ? c.measure(r, c) : c.render ? c.render(r, c) : r[c.key]
        if (typeof v === 'string' || typeof v === 'number') {
          body = Math.max(
            body,
            measure(v, c.mono === false ? '400 12.5px ' + SANS : '400 11px ' + MONO),
          )
        }
      }
      return Math.min(CEILING, Math.ceil(Math.max(head, body)) + 6) + 'px'
    }
    return cols.map((c) => {
      const t = track(c)
      // What the column costs the table. A flexible column costs its floor —
      // the width below which its content stops being readable — rather than
      // what it happens to be showing.
      const px = t === 'minmax(0,1fr)' ? c.floor || 150 : parseFloat(t) || 0
      return { c, track: t, px }
    })
  }, [cols, rows])

  // The table's own width, not the window's: the same table is wide in a page
  // and narrow in a column, and only the element knows which it is.
  const box = useRef<HTMLDivElement | null>(null)
  const [room, setRoom] = useState(0)
  useEffect(() => {
    if (!box.current || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver((es) => setRoom(es[0].contentRect.width))
    ro.observe(box.current)
    return () => ro.disconnect()
  }, [])

  // 'auto' pages: the rows take whatever the table's slot has left once the
  // title row, the column header and the pager have taken theirs. Measured
  // from the PARENT, whose height the layout decides — measuring the table
  // itself would feed its own growth back in and never settle.
  const pagerRef = useRef<HTMLDivElement | null>(null)
  const headRef = useRef<HTMLDivElement | null>(null)
  const titleRef = useRef<HTMLDivElement | null>(null)
  const [fit, setFit] = useState(8)
  // The draft row. A table whose estate can be grown from here grows it in the
  // shape the estate takes: a row at the top of the same grid, with the columns
  // that can be set live and the columns that are measurements still empty.
  // Nothing floats over the list, so the reader watches the roster make room.
  const draftable = !!(add && (add.options || add.fields))
  const [drafting, setDrafting] = useState(false)
  const [q, setQ] = useState('')
  const [hi, setHi] = useState(0)
  const [vals, setVals] = useState<Record<string, string>>(() => {
    const v: Record<string, string> = {}
    ;((add && add.fields) || []).forEach((f) => {
      v[f.key] = f.value !== undefined ? f.value : (f.options || [])[0]
    })
    return v
  })
  const [openField, setOpenField] = useState<string | null>(null)
  const [hov, setHov] = useState<string | null>(null)
  // Which device owns the mark. Without this, a pointer leaving the last row
  // hands the mark back to the keyboard index, which reads as a hover that
  // refused to let go.
  const [nav, setNav] = useState<'key' | 'mouse'>('key')
  // Nothing is added while the draft page is open: a name lands in `staged`,
  // where it is still a row you can click to take back. Leaving the page is what
  // commits, which makes the whole page one reversible act.
  const [staged, setStaged] = useState<
    Array<{ o: TableCandidate; values: Record<string, string> }>
  >([])
  const draftInput = useRef<HTMLInputElement | null>(null)
  const stagedRef = useRef(staged)
  const addRef = useRef(add)
  // Written in an effect, never during render: a render can be thrown away and
  // re-run, and a ref write inside one is not undone with it.
  useEffect(() => {
    stagedRef.current = staged
    addRef.current = add
  })
  const flush = () => {
    const list = stagedRef.current
    if (!list.length) return
    stagedRef.current = []
    setStaged([])
    const a = addRef.current
    if (a && a.onAdd) list.forEach((it) => a.onAdd!(it.o, it.values))
  }
  // Leaving the whole view counts as leaving the page.
  useEffect(() => () => flush(), [])
  useEffect(() => {
    if (drafting && draftInput.current) draftInput.current.focus()
  }, [drafting])
  const allOptions = (add && add.options) || []
  const key = (o: TableCandidate) => o.id || o.name
  const pool = allOptions.filter((o) => !staged.some((it) => key(it.o) === key(o)))
  // Candidates are rows, and the page shows as many as it has room for — the
  // table's own height decides, not a magic number. Two rows go to the draft row
  // and its hint, the staging strip is one more, and the candidates take the
  // rest, so a busy directory looks busy and the height still never moves.
  const budget = pageSize === 'auto' ? fit : (pageSize as number)
  const hitRoom = Math.max(0, (budget || 8) - 2 - (staged.length ? 1 : 0))
  // Three characters before anything is offered. A directory dumped as soon as
  // the row opens invites picking off a list; the point is that the reader knows
  // who they are adding and types their name.
  const MIN_Q = 3
  const asked = q.trim().length >= MIN_Q
  const hits = asked
    ? pool.filter((o) => (o.name || '').toLowerCase().indexOf(q.trim().toLowerCase()) >= 0)
    : []
  const shownHits = hits.slice(0, hitRoom)
  const takeable = shownHits.filter((o) => !o.disabled)
  const commitDraft = (o?: TableCandidate) => {
    const pick = o || takeable[Math.min(hi, takeable.length - 1)]
    if (!pick || pick.disabled) return
    // Guard against the same person landing twice — two fast presses can both
    // read a candidate list that has not re-rendered yet.
    if (stagedRef.current.some((it) => key(it.o) === key(pick))) return
    stagedRef.current = stagedRef.current.concat({ o: pick, values: { ...vals } })
    setStaged((st) => st.concat({ o: pick, values: { ...vals } }))
    // The query stays: the person you staged leaves the candidate list, so the
    // same search now shows who else it matched — clearing it would make you
    // type "cas" again to add the second Cassandra.
    setHi(0)
    if (draftInput.current) draftInput.current.focus()
  }
  const openDraft = () => {
    setSel({})
    setDrafting(true)
  }
  const closeDraft = () => {
    flush()
    setDrafting(false)
    setQ('')
    setHi(0)
    setOpenField(null)
    setHov(null)
    setNav('key')
  }

  useEffect(() => {
    if (pageSize !== 'auto' || typeof ResizeObserver === 'undefined') return
    const px = (v: string) => parseFloat(v) || 0
    // Every ancestor that clips or scrolls is a candidate frame, and the table
    // is bound by the TIGHTEST of them — not the nearest. A clipped box can
    // still grow with its content (overflow:hidden with an auto height), and
    // measuring one feeds the table's own height back in: it fits a row too
    // many, which grows the box, which is exactly the overflow. The scroller
    // further up does not move, so the minimum is the honest floor.
    const framesOf = (el: HTMLElement) => {
      const out: HTMLElement[] = []
      let p = el.parentElement
      while (p && p !== document.body) {
        const cs = getComputedStyle(p)
        // A zero-height wrapper bounds nothing; it is a host element mid-layout.
        if (
          (cs.overflowY !== 'visible' || cs.position === 'fixed') &&
          p.getBoundingClientRect().height > 1
        )
          out.push(p)
        p = p.parentElement
      }
      // The document is the last resort, not a peer: an unstretched body reports
      // the viewport's height while the page it holds is taller, so counting it
      // among the candidates would starve every table on a scrolling page.
      return out.length ? out : [document.body]
    }
    const read = () => {
      const b = box.current
      if (!b) return
      const frames = framesOf(b)
      let floor = Infinity
      for (const f of frames) {
        const fcs = getComputedStyle(f)
        const bottom =
          f.getBoundingClientRect().bottom - px(fcs.paddingBottom) - px(fcs.borderBottomWidth)
        if (bottom < floor) floor = bottom
      }
      const frame = frames[frames.length - 1]
      // Everything still owed room under the table: each ancestor's bottom
      // padding and border on the way up to the frame, and any sibling that
      // sits below it — the danger card, a footnote, a second section.
      let node: HTMLElement | null = b
      while (node && node !== frame && node.parentElement) {
        // Annotated because `node` is reassigned from `p` at the bottom of the
        // loop, so inferring one from the other is circular and lands on `any`
        // under this project's strictness. The loop guard above is what proves
        // it non-null.
        const p: HTMLElement = node.parentElement
        const cs = getComputedStyle(p)
        floor -= px(cs.paddingBottom) + px(cs.borderBottomWidth)
        const gap = px(cs.rowGap)
        for (let s = node.nextElementSibling; s; s = s.nextElementSibling) {
          const scs = getComputedStyle(s)
          // An overlay is not in the flow, so it owes the table nothing.
          if (scs.position === 'absolute' || scs.position === 'fixed') continue
          const r = s.getBoundingClientRect()
          // Beside, not below: in a row layout the next sibling is the next
          // column, and a column does not take height from its neighbour.
          if (r.top < node.getBoundingClientRect().bottom - 1) continue
          if (r.height) floor -= r.height + gap
        }
        node = p
      }
      const chrome =
        (titleRef.current ? titleRef.current.offsetHeight : 0) +
        (headRef.current ? headRef.current.offsetHeight : 0) +
        (pagerRef.current ? pagerRef.current.offsetHeight : 0) +
        6
      const avail = floor - b.getBoundingClientRect().top - chrome
      // A row within 10px of fitting is taken: the slack it eats is the
      // breathing room above the pager, and a row of real data is worth more
      // than 10px of air. Below that it is left out rather than crowded in.
      const n = Math.floor((avail + 10) / ROW_H)
      // Four is the floor: below that the pager costs more than it saves.
      setFit(Math.max(4, n))
    }
    read()
    const ro = new ResizeObserver(read)
    for (const f of framesOf(box.current!)) ro.observe(f)
    if (box.current && box.current.parentElement) ro.observe(box.current.parentElement)
    return () => ro.disconnect()
  }, [pageSize])

  const shown = useMemo(() => {
    const gap = 10
    const fixed = selectable ? 20 + gap : 0
    let keep = sizes.slice()
    const cost = () =>
      keep.reduce((n, s) => n + s.px, 0) + gap * Math.max(0, keep.length - 1) + fixed
    // Least important first, one at a time, re-measuring after each — dropping
    // one column often makes the rest fit, and dropping two when one would do
    // is how a table ends up emptier than the space it sits in.
    while (room > 0 && cost() > room) {
      const next = keep
        .filter((s) => s.c.drop)
        .sort((a, b) => (a.c.drop as number) - (b.c.drop as number))[0]
      if (!next) break
      keep = keep.filter((s) => s !== next)
    }
    return keep
  }, [sizes, room, selectable])

  // The one genuinely computed style in the component: the grid comes out of
  // measurement, so it is handed to CSS as a custom property rather than as a
  // rule this file would otherwise have to write three times.
  const grid = useMemo(
    () => (selectable ? '20px ' : '') + shown.map((s) => s.track).join(' '),
    [shown, selectable],
  )

  const sorted = useMemo(() => {
    const col = cols.find((c) => c.key === sort.k)
    const val = (r: TableRow) => (col && col.sortValue ? col.sortValue(r, col) : r[sort.k])
    const norm = (v: unknown) => (v === null || v === undefined ? '' : v)
    return working.slice().sort((a, b) => {
      const x = norm(val(a))
      const y = norm(val(b))
      // Blanks sort last in both directions: "no primary team" is the absence
      // of a value, not a value below 'a', and it should not fill the top of
      // the column you just asked to rank.
      if (x === '' || y === '') return x === y ? 0 : x === '' ? 1 : -1
      if (typeof x === 'string' && typeof y === 'string') return x.localeCompare(y) * sort.d
      return (x < y ? -1 : x > y ? 1 : 0) * sort.d
    })
  }, [working, sort, cols])

  // Everything below reads `size`, so 'auto' is resolved in exactly one place.
  // The draft row and its hint are worth about two rows, and they are paid for
  // out of the page rather than out of the frame: the table's height never moves.
  // The draft is its own PAGE — a "+" to the left of page 1 — so a row being
  // written is never mixed in with rows that exist. It costs the table no
  // height: the page it replaces was the same height.
  const size = budget
  const pages = size ? Math.max(1, Math.ceil(sorted.length / size)) : 1
  const at = Math.min(page, pages - 1)
  // On the draft page the body is the draft row, what is staged, and the
  // candidates — nothing that already exists.
  const view = drafting ? [] : size ? sorted.slice(at * size, at * size + size) : sorted
  const draftCost = 2 + (staged.length ? 1 : 0) + shownHits.length

  const selIds = Object.keys(sel).filter((k) => sel[k])
  const n = selIds.length
  const allOn = n > 0 && n >= sorted.length

  const toggle = useCallback(
    (id: string | number, shift: boolean) => {
      setSel((prev) => {
        const next = { ...prev }
        if (shift && anchor.current) {
          const order = sorted.map((r) => r.id)
          const a = order.indexOf(anchor.current)
          const b = order.indexOf(id)
          if (a > -1 && b > -1) {
            const on = !next[id]
            for (let i = Math.min(a, b); i <= Math.max(a, b); i++) {
              if (on) next[order[i]] = true
              else delete next[order[i]]
            }
            return next
          }
        }
        if (next[id]) delete next[id]
        else next[id] = true
        anchor.current = id
        return next
      })
    },
    [sorted],
  )

  // A second action arriving while the window is open commits it early, and
  // that must not leave a timer running against a snapshot nobody can reach
  // any more.
  const finish = useCallback(() => {
    commitPending('window')
    setPending(null)
  }, [commitPending])

  const undo = useCallback(() => {
    if (timer.current) clearInterval(timer.current)
    // Cleared here too, or the unmount flush would send an action the reader
    // just took back.
    pendingRef.current = null
    setPending((p) => {
      if (p) setWorking(p.snapshot)
      return null
    })
  }, [])

  const run = useCallback(
    (action: TableAction, pick?: PickerOption) => {
      if (!n) return
      if (pending) finish()
      const snapshot = working
      const ids = selIds.slice()
      const next = working
        .map((r) => (ids.indexOf(String(r.id)) < 0 ? r : mutate ? mutate(r, action.id, pick) : r))
        .filter(Boolean) as TableRow[]
      setWorking(next)
      setSel({})
      anchor.current = null
      setPending({
        actionId: action.id,
        ids,
        snapshot,
        label: action.message ? action.message(ids.length, pick) : ids.length + ' rows changed',
        left: UNDO_MS,
        pick,
      })
      if (timer.current) clearInterval(timer.current)
      const started = Date.now()
      timer.current = setInterval(() => {
        const left = UNDO_MS - (Date.now() - started)
        if (left <= 0) {
          commitPending('window')
          setPending(null)
        } else {
          setPending((p) => (p ? { ...p, left } : p))
        }
      }, 100)
    },
    [n, selIds, working, mutate, pending, finish, commitPending],
  )

  // Two ways a window can end without the timer reaching zero, and both owe the
  // same request. Dropping it would discard a change the reader watched land.
  //
  // `pagehide` rather than `beforeunload`: it fires on the bfcache path and on
  // mobile Safari, where beforeunload does not.
  useEffect(() => {
    const onHide = () => commitPending('unload')
    window.addEventListener('pagehide', onHide)
    return () => {
      window.removeEventListener('pagehide', onHide)
      if (timer.current) clearInterval(timer.current)
      commitPending('navigation')
    }
  }, [commitPending])

  // Bound to locals before the JSX. A narrowing on `bar.picker` does not
  // survive into the callbacks below — a property access could in principle
  // change between the check and the call — and a local is what proves to the
  // reader, as well as to the checker, that it is the same object throughout.
  //
  // A picker option and the hold verb are actions like any other: give either
  // an `action` and it takes the same snapshot, undo window and commit as a
  // labelled verb in the bar.
  const barPicker = bar?.picker
  const barDanger = bar?.danger
  // What the verbs are being asked to apply to, resolved once: every gate below
  // asks the same question of the same rows.
  const selectedRows = working.filter((r) => selIds.indexOf(String(r.id)) >= 0)
  const offers = (a?: TableAction) => !a?.enabledFor || a.enabledFor(selectedRows)
  // A picker narrows rather than vanishes: the choices that still apply to this
  // selection are drawn, and it is only absent once none of them do.
  const pickerOptions =
    typeof barPicker?.options === 'function'
      ? barPicker.options(selectedRows)
      : (barPicker?.options ?? [])
  const pickerProp =
    barPicker && offers(barPicker.action) && pickerOptions.length
      ? {
          ...barPicker,
          options: pickerOptions,
          onPick: (o: PickerOption) => {
            barPicker.onPick?.(o)
            if (barPicker.action) run(barPicker.action, o)
          },
        }
      : undefined
  const dangerProp =
    barDanger && offers(barDanger.action)
      ? {
          ...barDanger,
          onConfirm: () => {
            barDanger.onConfirm?.()
            if (barDanger.action) run(barDanger.action)
          },
        }
      : undefined

  const selBar = bar ? (
    <span className="tb-bar-inner">
      <SelectionBar
        count={n}
        actions={(bar.actions || actions)
          // A verb that cannot apply to every selected row is not offered
          // at all: a disabled entry in a floating bar reads as a bug.
          .filter((a) => offers(a))
          .map((a) => ({
            label: a.label,
            tone: a.tone === 'bad' ? 'alarm' : 'default',
            // A destructive verb is leaned on; an ordinary one fires on
            // press. Both take the same gesture, at different pressure.
            holdMs: a.holdMs != null ? a.holdMs : a.tone === 'bad' ? 900 : 0,
            onSelect: () => run(a),
          }))}
        // A picker option and the hold verb are actions like any other:
        // give either an `action` and it takes the same snapshot, undo
        // window and commit as a labelled verb in the bar.
        picker={pickerProp}
        danger={dangerProp}
        undo={pending ? { msg: pending.label, frac: pending.left / UNDO_MS, onUndo: undo } : null}
        onDismiss={() => {
          setSel({})
          anchor.current = null
        }}
      />
    </span>
  ) : null

  const listId = 'tb-cands'
  // An id reference cannot contain whitespace, and a candidate's key is a name.
  const domId = (o: TableCandidate) => listId + '-' + String(key(o)).replace(/\s+/g, '_')
  const activeId = (() => {
    if (!drafting || !takeable.length) return undefined
    const lead =
      nav === 'mouse'
        ? shownHits.find((o) => key(o) === hov)
        : takeable[Math.min(hi, takeable.length - 1)]
    return lead ? domId(lead) : undefined
  })()

  return (
    <div
      className="tb"
      ref={box}
      data-tbuild={building ? '1' : undefined}
      style={{ '--tb-grid': grid } as CSSProperties}
    >
      {showHeader ? (
        <div className="tb-title" ref={titleRef}>
          {n && !bar ? (
            <>
              <span className="tb-selcount">{n} SELECTED</span>
              <span className="tb-divider" />
              {actions.map((a) => (
                <button
                  type="button"
                  key={a.id}
                  className="tb-verb"
                  data-tone={a.tone}
                  onClick={() => run(a)}
                >
                  {a.label}
                </button>
              ))}
              <span className="tb-spacer" />
              <button
                type="button"
                className="tb-clear"
                onClick={() => {
                  setSel({})
                  anchor.current = null
                }}
              >
                Clear
              </button>
            </>
          ) : (
            <>
              {headerLeft && !n ? (
                headerLeft
              ) : (
                <span className="tb-title-label" data-selected={n ? '' : undefined}>
                  {n ? n + ' SELECTED' : label}
                </span>
              )}
              <span className="tb-spacer" />
              {headerAction ? <span className="tb-headaction">{headerAction}</span> : null}
            </>
          )}
          {add ? (
            <button
              type="button"
              className="tb-add"
              data-draftable={draftable || undefined}
              data-open={drafting || undefined}
              aria-expanded={draftable ? drafting : undefined}
              onClick={() => {
                if (draftable) {
                  if (drafting) closeDraft()
                  else openDraft()
                } else if (add.onSelect) add.onSelect()
              }}
            >
              + {add.label}
            </button>
          ) : null}
          {headerRight}
        </div>
      ) : null}

      <div className="tb-head" ref={headRef} role="row">
        {selectable ? (
          <Checkbox
            checked={allOn}
            indeterminate={n > 0 && !allOn}
            size={13}
            title={allOn ? 'Clear selection' : 'Select all ' + sorted.length}
            onChange={() => {
              if (allOn) setSel({})
              else {
                const next: Record<string, boolean> = {}
                for (const r of sorted) next[r.id] = true
                setSel(next)
              }
              anchor.current = null
            }}
          />
        ) : null}
        {shown.map(({ c }) => {
          const sortable = c.sortable !== false
          const active = sort.k === c.key
          const doSort = () =>
            sortable && setSort((s) => ({ k: c.key, d: s.k === c.key ? -s.d : 1 }))
          return (
            <span
              key={c.key}
              className="tb-col"
              role="columnheader"
              // A column can decline to be sorted — a sparkline is a shape, not
              // an order, and a header that does nothing when clicked is worse
              // than one that says it does nothing.
              aria-sort={
                active ? (sort.d > 0 ? 'ascending' : 'descending') : sortable ? 'none' : undefined
              }
              data-active={active || undefined}
              data-align={c.align}
              data-sortable={sortable ? '' : undefined}
              tabIndex={sortable ? 0 : undefined}
              onClick={doSort}
              onKeyDown={(e: KeyboardEvent) => {
                if (!sortable) return
                if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return
                e.preventDefault()
                doSort()
              }}
            >
              {c.label}
              {active ? (
                <span
                  className="tb-caret"
                  data-dir={sort.d > 0 ? 'up' : 'down'}
                  aria-hidden="true"
                />
              ) : null}
            </span>
          )
        })}
      </div>

      {sorted.length === 0 ? <div className="tb-empty">{emptyLabel}</div> : null}

      <div className="tb-body">
        {building ? (
          <span
            className="tb-beam"
            data-tbeam=""
            aria-hidden="true"
            style={{ '--tb-run': view.length * ROW_H + 'px' } as CSSProperties}
          />
        ) : null}

        {drafting ? (
          <div className="tb-draft">
            {/* Not a card. An outlined, raised, rounded row is what a finished
                object looks like, and its top edge lands on the header divider —
                two lines doing one job. A warm band with an edge marker in the
                gutter says "being written" without adding a second rule. */}
            <div className="tb-draft-row">
              <span className="tb-draft-edge" />
              <span className="tb-draft-sweep" />
              <span className="tb-draft-underline" />
              {selectable ? <span /> : null}
              {shown.map(({ c }, ci) => {
                if (ci === 0) {
                  return (
                    <input
                      key={c.key}
                      className="tb-draft-input"
                      data-sans={c.mono === false || c.type === 'identity' ? '' : undefined}
                      ref={draftInput}
                      value={q}
                      role="combobox"
                      aria-expanded={shownHits.length > 0}
                      aria-controls={listId}
                      aria-activedescendant={activeId}
                      aria-autocomplete="list"
                      aria-label={add!.label}
                      onChange={(e) => {
                        setQ(e.target.value)
                        setHi(0)
                        setNav('key')
                        setHov(null)
                      }}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') {
                          e.preventDefault()
                          commitDraft()
                        } else if (e.key === 'Escape') {
                          e.preventDefault()
                          closeDraft()
                        } else if (e.key === 'ArrowDown') {
                          e.preventDefault()
                          setNav('key')
                          setHov(null)
                          setHi((h) => Math.min(h + 1, Math.max(0, takeable.length - 1)))
                        } else if (e.key === 'ArrowUp') {
                          e.preventDefault()
                          setNav('key')
                          setHov(null)
                          setHi((h) => Math.max(0, h - 1))
                        }
                      }}
                      placeholder={
                        q ? '' : add!.placeholder || 'type at least ' + MIN_Q + ' characters'
                      }
                    />
                  )
                }
                const f = (add!.fields || []).find((x) => x.key === c.key)
                if (f) {
                  const opts = f.options || []
                  const open = openField === c.key
                  return (
                    <span key={c.key} className="tb-field">
                      <button
                        type="button"
                        className="tb-field-btn"
                        aria-expanded={open}
                        aria-label={c.label ? c.label + ': ' + vals[c.key] : vals[c.key]}
                        onClick={() => setOpenField(open ? null : c.key)}
                      >
                        <span className="tb-field-val">{vals[c.key]}</span>
                        <span className="tb-field-caret" aria-hidden="true" />
                      </button>
                      {open ? (
                        <span className="tb-menu" role="listbox">
                          {opts.map((o) => (
                            <button
                              type="button"
                              key={o}
                              className="tb-menu-opt"
                              role="option"
                              aria-selected={o === vals[c.key]}
                              data-on={o === vals[c.key] || undefined}
                              onClick={() => {
                                setVals((v) => ({ ...v, [c.key]: o }))
                                setOpenField(null)
                                if (draftInput.current) draftInput.current.focus()
                              }}
                            >
                              {o}
                            </button>
                          ))}
                        </span>
                      ) : null}
                    </span>
                  )
                }
                // A measurement has nothing to say about someone who has not run
                // anything yet, and an empty cell says that better than a zero.
                return (
                  <span key={c.key} className="tb-blank" data-align={c.align}>
                    —
                  </span>
                )
              })}
            </div>

            {shownHits.length ? (
              <div
                className="tb-previews"
                data-tpreviews=""
                id={listId}
                role="listbox"
                aria-label={add!.label}
              >
                {shownHits.map((o) => {
                  const rank = takeable.indexOf(o)
                  // The pointer wins while it is over a row; the keyboard owns the
                  // mark otherwise.
                  const lead =
                    nav === 'mouse'
                      ? hov === key(o)
                      : rank >= 0 && rank === Math.min(hi, takeable.length - 1)
                  return (
                    <div
                      key={o.id || o.name}
                      id={domId(o)}
                      // A candidate is drawn as the row it would become: same
                      // grid, same columns, so choosing is reading rather than
                      // picking off a list that floats over the answer.
                      className="tb-preview"
                      role="option"
                      aria-selected={lead}
                      aria-disabled={o.disabled || undefined}
                      data-lead={lead || undefined}
                      data-disabled={o.disabled || undefined}
                      onMouseEnter={() => {
                        setNav('mouse')
                        setHov(key(o))
                        if (rank >= 0) setHi(rank)
                      }}
                      onMouseLeave={() => setHov((k) => (k === key(o) ? null : k))}
                      onClick={() => commitDraft(o)}
                    >
                      {selectable ? <span /> : null}
                      {shown.map(({ c }, ci) => {
                        if (ci === 0) {
                          return (
                            <span key={c.key} className="tb-preview-id">
                              <span className="tb-preview-name">{o.name}</span>
                              {/* Only the reason a row cannot be taken. Anything
                                  else about this person needs a column before it
                                  can be printed in one. */}
                              {o.disabled ? (
                                <span className="tb-preview-note">
                                  {o.disabledNote || 'already here'}
                                </span>
                              ) : null}
                            </span>
                          )
                        }
                        const f = (add!.fields || []).find((x) => x.key === c.key)
                        return (
                          <span
                            key={c.key}
                            className="tb-preview-cell"
                            data-align={c.align}
                            data-set={f ? '' : undefined}
                          >
                            {f ? (o.disabled ? '—' : vals[c.key]) : '—'}
                          </span>
                        )
                      })}
                    </div>
                  )
                })}
              </div>
            ) : null}

            {/* The same sentence in every state: what is being written here is
                saved by leaving, and a reader should not have to have staged
                something to learn that. */}
            <div
              className="tb-hint"
              title={
                add!.hint || "pushing 'esc' or navigating to a different page will save changes"
              }
            >
              {add!.hint || "pushing 'esc' or navigating to a different page will save changes"}
            </div>
          </div>
        ) : null}

        {view.map((r, ri) => {
          const on = !!sel[r.id]
          return (
            <div
              key={r.id}
              className="tb-row"
              data-trow=""
              role="row"
              aria-selected={selectable ? on : undefined}
              data-on={on || undefined}
              data-selectable={selectable ? '' : undefined}
              data-opens={onRowOpen ? '' : undefined}
              // --tb-d is read by the build's row and hatch animations;
              // --tb-row-bg is the caller's own tint for the one row that is
              // the one. Both are runtime values, so both are properties.
              style={
                {
                  '--tb-d': (0.2 + ri * 0.022).toFixed(3) + 's',
                  '--tb-row-bg': (rowBg && rowBg(r)) || 'transparent',
                } as CSSProperties
              }
              // A row that opens takes the click; selection is then the
              // checkbox's alone, which stops its own click below.
              onClick={(e) => (onRowOpen ? onRowOpen(r) : selectable && toggle(r.id, e.shiftKey))}
            >
              {selectable ? (
                <Checkbox
                  checked={on}
                  size={13}
                  title={'Select row'}
                  onChange={(_next, e) => {
                    e.stopPropagation()
                    toggle(r.id, (e as MouseEvent).shiftKey)
                  }}
                />
              ) : null}
              {shown.map(({ c }) => {
                const value = c.render ? c.render(r, c) : r[c.key]
                // A cell that renders a node owns its own layout: no ellipsis, no
                // hover tooltip, no type treatment. Text cells keep all three.
                if (value !== null && typeof value === 'object') {
                  return (
                    <span key={c.key} className="tb-cell-node" data-align={c.align} role="cell">
                      {value}
                    </span>
                  )
                }
                return (
                  <span
                    key={c.key}
                    className="tb-cell"
                    role="cell"
                    data-align={c.align}
                    data-sans={c.mono === false ? '' : undefined}
                    style={c.color ? { color: c.color(r) } : undefined}
                    // A name too long for its column ends in an ellipsis. Flagged
                    // per cell so only a clipped name offers its full self on hover.
                    ref={(el) => {
                      if (!el) return
                      el.dataset.clip = el.scrollWidth > el.clientWidth + 1 ? '1' : ''
                    }}
                    onMouseEnter={(e) => {
                      const el = e.currentTarget
                      if (!el.dataset.clip) return
                      setTip({
                        key: r.id + '/' + c.key,
                        text: el.textContent || '',
                        left: el.offsetLeft,
                      })
                    }}
                    onMouseLeave={() =>
                      setTip((p) => (p && p.key === r.id + '/' + c.key ? null : p))
                    }
                  >
                    {value}
                  </span>
                )
              })}
              {tip && tip.key.indexOf(r.id + '/') === 0 ? (
                <span className="tb-tip" style={{ left: tip.left }} aria-hidden="true">
                  {tip.text}
                </span>
              ) : null}
            </div>
          )
        })}
      </div>

      {/* A short LAST page holds the difference open so the table does not
          change height as you page through it. When every row already fits
          there is no next page to hold height for, and the spacer would be a
          hole. */}
      {drafting ? (
        size ? (
          <div className="tb-fill" style={{ height: Math.max(0, (size - draftCost) * ROW_H) }} />
        ) : null
      ) : size && view.length < Math.min(size, sorted.length) ? (
        <div
          className="tb-fill"
          style={{ height: (Math.min(size, sorted.length) - view.length) * ROW_H }}
        />
      ) : null}

      {staged.length ? (
        <div className="tb-staged">
          {staged.map((it, si) => (
            <button
              type="button"
              key={'s' + (it.o.id || it.o.name)}
              className="tb-pill"
              title="click to remove"
              aria-label={'Remove ' + it.o.name}
              onClick={() => setStaged((st) => st.filter((_, i) => i !== si))}
            >
              {it.o.name}
              {(add!.fields || []).length ? (
                <span className="tb-pill-val">{it.values[(add!.fields || [])[0].key]}</span>
              ) : null}
              <span className="tb-pill-x" aria-hidden="true">
                ×
              </span>
            </button>
          ))}
        </div>
      ) : null}

      {size && sorted.length ? (
        <div className="tb-pager" ref={pagerRef}>
          <span className="tb-pager-footer">{footer}</span>
          <span className="tb-pager-count">
            {drafting
              ? staged.length
                ? staged.length + ' to add · ' + pool.length + ' more in the org'
                : asked
                  ? hits.length + ' of ' + pool.length + ' in the org'
                  : pool.length + ' in the org'
              : at * size +
                1 +
                '–' +
                Math.min(sorted.length, at * size + size) +
                ' of ' +
                sorted.length}
          </span>
          <span className="tb-pages">
            {[
              // The draft page appears when there IS one: the pager reports where
              // you can go, and there is nowhere to go until the row is open.
              ...(draftable && drafting
                ? [{ k: 'new', glyph: '+', draft: true, live: true, on: true }]
                : []),
              { k: 'prev', glyph: '\u2039', to: at - 1, live: at > 0, label: 'Previous page' },
              ...tablePages(at, pages).map((p) => ({
                k: 'p' + p,
                glyph: String(p + 1),
                to: p,
                live: true,
                on: p === at && !drafting,
                label: 'Page ' + (p + 1),
              })),
              { k: 'next', glyph: '\u203a', to: at + 1, live: at < pages - 1, label: 'Next page' },
            ].map((b: PagerButton) => (
              <button
                type="button"
                key={b.k}
                className="tb-page"
                data-on={b.on || undefined}
                data-draft={b.draft || undefined}
                data-live={b.live ? '' : undefined}
                aria-current={b.on && !b.draft ? 'page' : undefined}
                aria-label={b.draft ? (add && add.label ? add.label : 'add') : b.label}
                title={b.draft ? (add && add.label ? add.label : 'add') : undefined}
                disabled={!b.live && !b.draft}
                onClick={() => {
                  // The draft page and the data pages are one control: leaving
                  // for a real page closes the draft, and returning re-opens it.
                  if (b.draft) {
                    if (drafting) closeDraft()
                    else openDraft()
                    return
                  }
                  if (!b.live) return
                  if (drafting) closeDraft()
                  // Only a chip that names a destination moves the page. The
                  // draft `+` has none — it returned above — and neither arrow
                  // is drawn past its end.
                  if (b.to !== undefined && b.to !== at) setPage(b.to)
                }}
              >
                {b.glyph}
              </button>
            ))}
          </span>
        </div>
      ) : null}

      {size && sorted.length ? null : footer}

      {/* The bar's slot is always in the flow when it is inline, so a selection
          cannot resize the table it belongs to. Floating placements keep the
          old behaviour and reserve nothing. */}
      {barPosition === 'inline' ? (
        <div className="tb-bar-slot" style={{ minHeight: barSlot }}>
          {pending || n ? selBar : null}
        </div>
      ) : (n || pending) && bar ? (
        <div className="tb-bar-float" data-pos={barPosition}>
          {selBar}
        </div>
      ) : null}
    </div>
  )
}

export default Table
