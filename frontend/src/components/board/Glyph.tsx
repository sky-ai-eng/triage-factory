import type { CSSProperties } from 'react'
import { GLYPH_PATHS, type GlyphName } from '../../ui/shell/glyphPaths'

// Glyph — the board's marks, read from the shell's own glyph table
// (`ui/shell/glyphPaths.ts`) so a shape the product already has is never
// drawn twice: `shield` is the table's `gov`, the mark the product already
// uses for that shape. Never Lucide on a card — the grids differ and the
// weights will not match.
//
// One deliberate deviation from `Ico`. That component fixes stroke at 1.3 and
// lets the RENDERED weight follow size, which is right in a 15px rail and
// wrong in a footer: a 15px mark is larger than the 11px numeral it labels.
// Here the rendered weight is held at 1.22px — the activity row's arrow, and
// `Ico` at 15 — and size is free. Stroke is solved for, never set.

export type GlyphKind =
  | 'branch'
  | 'pulls'
  | 'review'
  | 'comment'
  | 'alert'
  | 'shield'
  | 'check'
  | 'cross'
  | 'pause'
  | 'dash'

// The board's kind names onto the table's. `pulls` is the artifact strip's
// word for a pull request, and `shield` is the product's `gov`.
const NAME: Record<GlyphKind, GlyphName> = {
  branch: 'branch',
  pulls: 'pulls',
  review: 'review',
  comment: 'comment',
  alert: 'alert',
  shield: 'gov',
  check: 'check',
  cross: 'cross',
  pause: 'pause',
  dash: 'dash',
}

// The rendered hairline every mark on a card shares.
const WEIGHT = 1.22

export function Glyph({
  kind,
  size = 13,
  style,
}: {
  kind: GlyphKind
  /** Rendered stroke is held at 1.22px whatever this is. Default 13. */
  size?: number
  style?: CSSProperties
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden
      style={{ flexShrink: 0, display: 'block', ...style }}
    >
      <path
        d={GLYPH_PATHS[NAME[kind]]}
        stroke="currentColor"
        strokeWidth={(WEIGHT * 16) / size}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

export default Glyph
