import { useState } from 'react'
import { RunRows } from '../ui/runrow/RunRows'
import type { RunRowItem } from '../ui/runrow/RunRows'

// The RunRows harness. The review is the two axes (lifecycle × asks) plus the
// lead: switch it while the working rows scan — a changing action string drags
// the reference sideways in activity-first, and the anchor is what stops that.
// The extremes pane pushes both caps at once, in a wide pane and a 372px one
// where the reference has left the line for the glyph.

const ROWS: RunRowItem[] = [
  {
    id: 'r1',
    source: 'pull',
    lifecycle: 'working',
    activity: 'Replaying 6 commits onto origin/main',
    ref: 'factory-api#772',
    age: '4m',
    href: '#r1',
  },
  {
    id: 'r2',
    source: 'ticket',
    lifecycle: 'queued',
    activity: 'Triage the flaking rebalance test',
    ref: 'SKY-412',
    age: '1m',
    queue: 3,
    href: '#r2',
  },
  // A hand-started run with no upstream entity: in ref-lead the column draws
  // the em dash rather than sitting empty.
  {
    id: 'r3',
    source: 'manual',
    lifecycle: 'working',
    activity: 'Confirming the fix across 50 runs',
    age: '11m',
    href: '#r3',
  },
  {
    id: 'r4',
    source: 'pull',
    lifecycle: 'done',
    asks: true,
    activity: 'Review 4 files, 118 additions',
    ref: 'factory-api#761',
    age: '2h',
    href: '#r4',
  },
  {
    id: 'r5',
    source: 'alert',
    lifecycle: 'failed',
    asks: true,
    activity: 'Failed on the third attempt',
    ref: 'control-plane',
    age: '18h',
    href: '#r5',
  },
]

const EXTREMES: RunRowItem[] = [
  // Long on both axes at once: the reference is over the 152px cap and the
  // sentence runs past the line, so each yields in its own direction.
  {
    id: 'x1',
    source: 'pull',
    lifecycle: 'working',
    activity:
      'Replaying 34 commits onto origin/main and re-running the integration suite against the staging control plane before it opens the pull request',
    ref: 'platform-control-plane-migrations#1184',
    age: '12m',
    href: '#x1',
  },
  // Long reference, short command: the reference truncates head-first and the
  // prose does not grow into the width it gave up.
  {
    id: 'x2',
    source: 'ticket',
    lifecycle: 'working',
    activity: 'Reading the repository',
    ref: 'sky-ai-eng/payments-ledger-reconciliation#40921',
    age: '3m',
    href: '#x2',
  },
  // No `#`, so nothing to pin: one token, truncating at its end like text.
  {
    id: 'x3',
    source: 'alert',
    lifecycle: 'failed',
    asks: true,
    activity: 'Failed on the third attempt',
    ref: 'control-plane-eu-west-1-scheduler-canary',
    age: '1h',
    href: '#x3',
  },
  {
    id: 'x4',
    source: 'pull',
    lifecycle: 'queued',
    queue: 3,
    activity: 'Rebase and re-run',
    ref: 'ledger#88',
    age: '20s',
    href: '#x4',
  },
]

const MODES = [
  ['ref', 'entity first'],
  ['loose', 'entity first, loose'],
  ['activity', 'activity first'],
] as const

export function RunRowsCard() {
  const [mode, setMode] = useState<(typeof MODES)[number][0]>('ref')
  const lead = mode === 'activity' ? 'activity' : 'ref'
  const anchor = mode !== 'loose'
  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>RunRows</span>
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 4 }}>
          {MODES.map(([v, t]) => (
            <button
              key={v}
              onClick={() => setMode(v)}
              style={{
                font: '400 9.5px/1 var(--font-mono)',
                letterSpacing: '.06em',
                padding: '5px 8px',
                borderRadius: 3,
                cursor: 'pointer',
                border: '1px solid ' + (mode === v ? 'var(--color-line-2)' : 'var(--color-line-1)'),
                background: mode === v ? 'var(--color-tint-2)' : 'transparent',
                color: mode === v ? 'var(--color-ink-2)' : 'var(--color-ink-4)',
              }}
            >
              {t}
            </button>
          ))}
        </span>
      </div>

      <p className="gal-note">
        The agent-run list — one prose string per row and no title, because the work is what names a
        run. Two independent axes: <code>lifecycle</code> (queued · working · done · failed) and{' '}
        <code>asks</code> (wants a person → the warm tick, whatever the lifecycle). The lead is a
        list-level choice: <code>lead=&quot;ref&quot;</code> anchors the reference in a column of
        its own so a working row&apos;s changing action grows to the right of it instead of dragging
        the row&apos;s identity sideways — switch the toggle while the rows scan to see the twitch
        it fixes. The hand-started row keeps its slot as an em dash.
      </p>

      <div style={{ maxWidth: 560 }}>
        <RunRows
          label="NEEDS YOU"
          lead={lead}
          anchor={anchor}
          count={<span style={{ color: 'var(--color-warm)' }}>2</span>}
          rows={ROWS.filter((r) => r.asks)}
          onPick={() => {}}
          more={
            <a href="#board" onClick={(e) => e.preventDefault()}>
              +5 more on the board
            </a>
          }
        />
      </div>

      <div style={{ maxWidth: 560, marginTop: 22 }}>
        <RunRows
          label="RUNNING"
          lead={lead}
          anchor={anchor}
          count={<span style={{ color: 'var(--color-cool)' }}>3</span>}
          rows={ROWS.filter((r) => !r.asks)}
          onPick={() => {}}
        />
      </div>

      <p className="gal-note" style={{ marginTop: 22 }}>
        Extremes — a reference and a command that do not fit. The reference column is capped at
        152px and loses its head, never its number; the prose runs to the row&apos;s own time and
        dissolves there. At 372px the column is dropped outright and the reference hangs off the
        source glyph as a tooltip.
      </p>

      <div style={{ maxWidth: 560 }}>
        <RunRows lead={lead} anchor={anchor} rows={EXTREMES} onPick={() => {}} />
      </div>

      <div style={{ maxWidth: 372, marginTop: 10 }}>
        <RunRows
          lead={lead}
          anchor={anchor}
          rows={EXTREMES}
          onPick={() => {}}
          note="Same rows at 372px. Reference is on the glyph."
        />
      </div>

      <div style={{ maxWidth: 560, marginTop: 22 }}>
        <RunRows
          label="NEEDS YOU"
          count={<span style={{ color: 'var(--color-warm)' }}>0</span>}
          rows={[]}
          empty="Nothing needs you."
        />
      </div>
    </div>
  )
}

export default RunRowsCard
