// Drawn structure, not an icon font: a 16px box at 1.3 stroke, every path
// authored against that grid. lucide-react is a dependency of this app and is
// deliberately NOT used here — several of these are bespoke (overview, factory,
// queue, org, offline), and the ones adapted from lucide were rescaled from its
// 24 grid, so mixing the two would put two stroke weights in one rail.

import { GLYPH_PATHS, type GlyphName } from './glyphPaths'

export type { GlyphName }

export function Ico({ d, size = 16 }: { d: GlyphName; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d={GLYPH_PATHS[d]}
        stroke="currentColor"
        strokeWidth={1.3}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}
