// Drawn structure, not an icon font: a 16px box at 1.3 stroke, every path
// authored against that grid. lucide-react is a dependency of this app and is
// deliberately NOT used here — several of these are bespoke (overview, factory,
// queue, org, offline), and the ones adapted from lucide were rescaled from its
// 24 grid, so mixing the two would put two stroke weights in one rail.

const G = {
  // A roof over a floor: the shape of a place, without the house-icon cliché.
  overview: 'M2.5 7.2L8 2.8l5.5 4.4M3.8 8.4v4.8h8.4V8.4',
  board: 'M3 3v10M8 3v10M13 3v6',
  queue: 'M3 4h10M3 8h10M3 12h6',
  factory: 'M2.5 13.5V7l4 2.4V7l4 2.4V3.5h3v10z',
  // Lucide book-marked: a repository is a book you keep, not a branch —
  // branches belong to a single repo, so the old glyph named the wrong thing.
  repos:
    'M2.7 13V3A1.7 1.7 0 014.3 1.3h8.4a.7.7 0 01.7.7v12a.7.7 0 01-.7.7H4.3a.7.7 0 010-3.3h9' +
    'M6.7 1.3v5.4l2-2 2 2V1.3',
  // Lucide git-pull-request, scaled from its 24 grid to ours.
  pulls:
    'M6 4a2 2 0 11-4 0 2 2 0 014 0M14 12a2 2 0 11-4 0 2 2 0 014 0' +
    'M8.7 4h2A1.3 1.3 0 0112 5.3V10M4 6v8',
  prompts: 'M3.5 4.5L6.5 8l-3 3.5M8.5 12h4',
  // An open book with a bookmark: what a team writes down and keeps. Kin to
  // `library` (which is also two facing pages) but marked rather than shelved —
  // the library is a catalogue you browse, knowledge is a record you consult.
  knowledge:
    'M2.6 3.4h3.6a2 2 0 011.8 1 2 2 0 011.8-1h3.6v8.2H9.8a2 2 0 00-1.8 1 2 2 0 00-1.8-1H2.6z' +
    'M8 4.4v8.2',
  usage: 'M3 12a5 5 0 1110 0M8 12V8.5',
  // Lucide server: stacked units with a status lamp each. The old 2x2 grid
  // read as a dashboard or an app launcher, not as machines.
  fleet:
    'M2.7 1.3h10.6a1.3 1.3 0 011.4 1.4v2.6a1.3 1.3 0 01-1.4 1.4H2.7a1.3 1.3 0 01-1.4-1.4V2.7a1.3 1.3 0 011.4-1.4z' +
    'M2.7 9.3h10.6a1.3 1.3 0 011.4 1.4v2.6a1.3 1.3 0 01-1.4 1.4H2.7a1.3 1.3 0 01-1.4-1.4v-2.6a1.3 1.3 0 011.4-1.4z' +
    'M4 4h.01M4 12h.01',
  team: 'M6 7.5a2 2 0 100-4 2 2 0 000 4zM2.5 13c0-2 1.6-3.2 3.5-3.2S9.5 11 9.5 13M10.5 5.2a2 2 0 011 3.6M11.5 10.2c1.3.4 2 1.4 2 2.8',
  org: 'M3 13.5V3.5h6v10M9 7.5h4v6M5 6h2M5 9h2',
  gov: 'M8 2.5l5 2v4c0 3-2.4 4.6-5 5-2.6-.4-5-2-5-5v-4z',
  // The governance tail's mark. The design bundle asked for `alert` here and
  // never drew it, so the glyph table returned undefined and the fallback fed
  // the literal string "alert" to a path's `d` — an invalid path, which renders
  // as nothing at all. Silent, and invisible until someone wonders why the one
  // warm mark in the rail has no icon beside its count.
  alert: 'M8 5.4v3.2M8 11.1h.01M8 2.2l6 10.4H2z',
  prefs:
    'M2.5 4.5h11M2.5 11.5h11' +
    'M6 4.5a1.6 1.6 0 103.2 0 1.6 1.6 0 10-3.2 0' +
    'M9.5 11.5a1.6 1.6 0 103.2 0 1.6 1.6 0 10-3.2 0',
  settings:
    'M12.6 8a4.6 4.6 0 11-9.2 0 4.6 4.6 0 019.2 0M9.9 8a1.9 1.9 0 11-3.8 0 1.9 1.9 0 013.8 0' +
    'M12.6 8h1.7M8 12.6v1.7M3.4 8H1.7M8 3.4V1.7' +
    'M11.25 11.25l1.2 1.2M4.75 11.25l-1.2 1.2M4.75 4.75l-1.2-1.2M11.25 4.75l1.2-1.2',
  wide: 'M4 3.5v9M7 8h5.5M10 5.5L12.5 8 10 10.5',
  narrow: 'M12 3.5v9M9.5 8H4M6.5 5.5L4 8l2.5 2.5',
  inbox:
    'M2.5 8.5H6l1.2 2h1.6l1.2-2h3.5M3.6 4.2L2.5 8.5v3a1.2 1.2 0 001.2 1.2h8.6a1.2 1.2 0 001.2-1.2v-3l-1.1-4.3a1.2 1.2 0 00-1.1-.8H4.8a1.2 1.2 0 00-1.2.8z',
  back: 'M9.5 3.5L5 8l4.5 4.5',
  // Prompts' three children. A drill-in column of one repeated glyph tells you
  // nothing about where you are going.
  library:
    'M8 4.6v8M8 4.6C6.8 3.7 5.3 3.4 3 3.4v7.8c2.3 0 3.8.3 5 1.2' +
    'M8 4.6c1.2-.9 2.7-1.2 5-1.2v7.8c-2.3 0-3.8.3-5 1.2',
  market: 'M3.6 6h8.8l-.7 6.1a1 1 0 01-1 .9H5.3a1 1 0 01-1-.9zM6.2 6V4.9a1.8 1.8 0 013.6 0V6',
  bindings:
    'M6.6 9.4l2.8-2.8M5.9 7.3L4.6 8.6a2.1 2.1 0 003 3l1.3-1.3' +
    'M10.1 8.7l1.3-1.3a2.1 2.1 0 00-3-3L7.1 5.7',
  chev: 'M6 3.5L10.5 8 6 12.5',
  // Signal bars with a slash through them: the conventional mark for a link
  // that is down, rather than a value that is missing.
  offline: 'M3 13.2v-2.4M6.6 13.2V8M10.2 13.2V5.2M13.8 13.2V2.8' + 'M2.6 13.6L13.4 2.8',
} as const

/** Every mark the rail can draw. Exported as a TYPE so a row cannot name
 * a glyph that was never drawn — the governance alert did exactly that,
 * and rendered nothing at all. */
export type GlyphName = keyof typeof G

export function Ico({ d, size = 16 }: { d: GlyphName; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d={G[d]}
        stroke="currentColor"
        strokeWidth={1.3}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}
