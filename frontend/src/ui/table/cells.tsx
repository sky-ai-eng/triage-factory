import Checkbox from '../checkbox/Checkbox'
import type { TableColumn } from './Table'

// The table's shared vocabulary: how a page window is computed, how elapsed
// time is printed, and the registry of cell types.
//
// Separate from Table.tsx because none of it is the component. A caller
// registers a cell type once and every table gets it, which is the opposite of
// something tied to one component's render — and keeping them together made
// the module export both a component and a mutable registry, which is what
// react-refresh objects to and it is right to.

/**
 * Which page numbers to draw: all of them up to seven, otherwise a window of
 * five around the current page.
 */
export function tablePages(at: number, pages: number): number[] {
  if (pages <= 7) return Array.from({ length: pages }, (_, i) => i)
  const from = Math.max(0, Math.min(at - 2, pages - 5))
  return Array.from({ length: 5 }, (_, i) => from + i)
}

/**
 * Elapsed time reads as a measurement, never as a word: "yesterday" is both
 * vague and three times the width of the column it sits in.
 */
export function ago(min: number): string {
  if (min < 60) return Math.max(1, Math.round(min)) + 'm'
  if (min < 60 * 24) return Math.round(min / 60) + 'h'
  if (min < 60 * 24 * 7) return Math.round(min / (60 * 24)) + 'd'
  return Math.round(min / (60 * 24 * 7)) + 'w'
}

/** A spark bar: a fraction the cell scales, or one the caller has sized itself. */
export type SparkBar = number | { h: string; bg?: string }

// Cell types. A column names one with `type`, and the table asks the registry
// how to draw it, how wide it wants to be and what it sorts by. Callers extend
// the registry rather than the component:
//
//   import { tableCells } from './Table'
//   tableCells.badge = { align: 'end', render: (r, c) => <Badge …/> }
//
// A column's own keys always win over its type's, so a type is a default set,
// not a straitjacket. Anything a type cannot express is still a plain `render`
// returning a node — the table treats any non-primitive value as a cell it does
// not style, measure or clip.
export const tableCells: Record<string, Partial<TableColumn>> = {}
const CELL = tableCells

CELL.text = {}
// Elapsed minutes, printed as a measurement and sorted as a number.
CELL.ago = { align: 'end', render: (r, c) => ago(r[c.key]), sortValue: (r) => r.lastMin }
// A person: avatar, then name. Sized and sorted by the name alone.
CELL.identity = {
  mono: false,
  measure: (r, c) => '    ' + r[c.key],
  sortValue: (r, c) => r[c.key],
  render: (r, c) => (
    <span className="tb-identity">
      <span
        className="tb-avatar"
        style={{ background: r[c.avatarKey || 'avatar'] || 'transparent' }}
      />
      <span className="tb-identity-name">{r[c.key]}</span>
    </span>
  ),
}
// A short trail of bars: numbers 0–1, or {h, bg} for a caller that has already
// decided its own scale. Not sortable — a shape is not an order.
CELL.spark = {
  sortable: false,
  render: (r, c) => (
    <span className="tb-spark">
      {(r[c.key] || []).map((d: SparkBar, i: number) => (
        <i
          key={i}
          style={{
            height: typeof d === 'number' ? Math.max(2, Math.round(d * 14)) + 'px' : d.h,
            background: (typeof d === 'number' ? null : d.bg) || undefined,
          }}
        />
      ))}
    </span>
  ),
}

// A row toggle: the cell IS the control, applied on click. `onToggle` gets the
// row; `locked` refuses the click for a row that cannot be switched off.
CELL.toggle = {
  sortValue: (r, c) => (r[c.key] ? 0 : 1),
  render: (r, c) => {
    const fixed = c.locked ? c.locked(r) : false
    return (
      <span className="tb-toggle" data-locked={fixed || undefined}>
        <Checkbox
          checked={!!r[c.key]}
          size={13}
          title={c.label}
          onChange={(_next, e) => {
            e.stopPropagation()
            if (!fixed && c.onToggle) c.onToggle(r)
          }}
        />
      </span>
    )
  },
}
// A single-select mark: exactly one row in the column carries it, and it can be
// moved but not cleared — an unset default has to fall back to something.
CELL.mark = {
  width: '26px',
  sortable: false,
  render: (r, c) => {
    const set = !!(c.isSet && c.isSet(r))
    return (
      <button
        type="button"
        className="tb-mark"
        data-align={c.align}
        data-set={set || undefined}
        aria-pressed={set}
        aria-label={c.label ? c.label + ' — ' + (r[c.nameKey || 'name'] || '') : undefined}
        onClick={(e) => {
          e.stopPropagation()
          if (c.onPick) c.onPick(r)
        }}
      >
        {/* Drawn rather than typed. A text star inherits its font's metrics — and
            on a stack whose first family carries no star glyph, a fallback's — so
            it never lands where the row's other marks do. The path is centred on
            its own ink: a five-pointed star reaches further above its centre than
            below, so the geometry is offset rather than the element. */}
        <svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true">
          <path d="M12 3.4 14.13 9.96 21.04 9.96 15.45 14.02 17.58 20.59 12 16.53 6.42 20.59 8.55 14.02 2.96 9.96 9.87 9.96Z" />
        </svg>
      </button>
    )
  },
}
// A share: hatched track, filled to the value, with the number at the right so
// the column reads as a ranking and as a measurement at once.
CELL.bar = {
  align: 'end',
  sortValue: (r, c) => -(r[c.key] || 0),
  render: (r, c) => {
    const v = r[c.key] || 0
    const pct = Math.round((v / (c.max || 100)) * 100)
    return (
      <span className="tb-share">
        <span className="tb-share-track">
          <i style={{ width: pct + '%', background: c.color ? c.color(r) : undefined }} />
        </span>
        <span
          className="tb-share-value"
          style={{ width: c.valueWidth || 34, color: c.valueColor ? c.valueColor(r) : undefined }}
        >
          {c.format ? c.format(v, r) : v + '%'}
        </span>
      </span>
    )
  },
}
