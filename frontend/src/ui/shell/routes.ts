import type { GlyphName } from './glyphs'

// The route table. ONE source: the rail, the palette and the tooltips all read
// this array. That is the whole reason the palette lives in this component
// rather than in its own file — two copies of a permission rule eventually
// disagree, and the failure mode is a palette that offers a page the rail is
// hiding.
//
// Availability is a function of deployment MODE and GRANT, never of role name.
// Local mode has no teams, no organisation and no marketplace, so those rows do
// not exist. They are absent rather than disabled, because a greyed row is an
// advertisement for a feature the reader cannot buy.

export type Grant = 'org-admin' | 'team-admin' | 'fleet'

export type Flags = {
  marketplace?: boolean
  /**
   * Governance has a rail row, a warm alert tail and no page yet. Off until one
   * exists — the same absent-not-disabled rule, applied to ourselves.
   */
  governance?: boolean
}

export type RailCounts = {
  repos?: number
  pulls?: number
  usage?: string
  fleet?: number
  gov?: number
}

export type Row = {
  id: string
  name: string
  icon: GlyphName
  /** Renders the two live counts (needs + running) instead of a tail. */
  counts?: boolean
  /** A fact about the destination — never a notification about the app. */
  tail?: string
  /** A thing that happened and needs somebody. Warm, and the rail's only other warm mark. */
  alert?: number
  children?: Row[]
}

export type Section = { id: string; name: string; rows: Row[] }

export function routes({
  mode,
  grants,
  flags,
  n,
}: {
  mode: 'local' | 'multi'
  grants: Grant[]
  flags: Flags
  n: RailCounts
}): Section[] {
  const multi = mode === 'multi'
  const g = (k: Grant) => grants.indexOf(k) !== -1
  const num = (v: number | undefined) => (v === undefined ? undefined : String(v))

  const secs: Section[] = [
    {
      id: 'work',
      name: 'WORK',
      rows: [
        // First, because it is where the eye starts and the least frequent
        // destination — you land on your last page, not here.
        { id: 'overview', name: 'Overview', icon: 'overview' },
        { id: 'board', name: 'Board', icon: 'board', counts: true },
        { id: 'factory', name: 'Factory', icon: 'factory' },
      ],
    },
    {
      id: 'sources',
      name: 'SOURCES',
      rows: [
        { id: 'repos', name: 'Repositories', icon: 'repos', tail: num(n.repos) },
        { id: 'pulls', name: 'Pull requests', icon: 'pulls', tail: num(n.pulls) },
        {
          id: 'prompts',
          name: 'Prompts',
          icon: 'prompts',
          children: [
            { id: 'prompts.library', name: 'Library', icon: 'library' },
            ...(flags.marketplace && multi
              ? [{ id: 'prompts.market', name: 'Marketplace', icon: 'market' as GlyphName }]
              : []),
            { id: 'prompts.bindings', name: 'Bindings', icon: 'bindings' },
          ],
        },
      ],
    },
  ]

  // Usage and the management rows share one heading: two headings for four rows
  // spends more height on labels than on destinations. The name follows what is
  // actually in it — for a member holding no admin grant the section is Usage
  // alone, and calling that Administration would be a lie.
  const admin = multi && (g('org-admin') || g('team-admin'))
  secs.push({
    id: 'admin',
    name: admin ? 'ADMINISTRATION' : 'CAPACITY',
    rows: [
      { id: 'usage', name: 'Usage', icon: 'usage', tail: n.usage },
      ...(admin ? [{ id: 'team', name: 'Team', icon: 'team' as GlyphName }] : []),
      ...(admin && g('org-admin')
        ? [{ id: 'org', name: 'Organization', icon: 'org' as GlyphName }]
        : []),
      ...(admin && g('org-admin') && flags.governance
        ? [{ id: 'gov', name: 'Governance', icon: 'gov' as GlyphName, alert: n.gov || 0 }]
        : []),
    ],
  })

  if (g('fleet')) {
    secs.push({
      id: 'deploy',
      name: 'DEPLOYMENT',
      rows: [{ id: 'fleet', name: 'Fleet', icon: 'fleet', tail: num(n.fleet) }],
    })
  }

  return secs
}

// The rail hides whole sections by grant, so a member and an admin see
// different products with no explanation anywhere else. This line is where
// "why can't I see Fleet" gets its answer.
export const grantLabel: Record<Grant, string> = {
  'org-admin': 'org admin',
  'team-admin': 'team admin',
  fleet: 'fleet operator',
}
