import { RunRows } from '../ui/runrow/RunRows'

// The RunRows harness. The review is the two axes: lifecycle sets where the
// run is, asks sets whether it wants a person, and only their combination —
// never one enum — decides a row's face. The working row scans; hover the
// queued row's hourglass for the tooltip; the whole band is the hit area.

const ROWS = [
  {
    id: 'r1',
    source: 'pull' as const,
    lifecycle: 'working' as const,
    activity: 'Replaying 6 commits onto origin/main',
    ref: 'factory-api#772',
    age: '4m',
    href: '#r1',
  },
  {
    id: 'r2',
    source: 'ticket' as const,
    lifecycle: 'queued' as const,
    activity: 'Triage the flaking rebalance test',
    ref: 'SKY-412',
    age: '1m',
    queue: 3,
    href: '#r2',
  },
  {
    id: 'r3',
    source: 'pull' as const,
    lifecycle: 'done' as const,
    asks: true,
    activity: 'Review 4 files, 118 additions',
    ref: 'factory-api#761',
    age: '2h',
    href: '#r3',
  },
  {
    id: 'r4',
    source: 'alert' as const,
    lifecycle: 'failed' as const,
    asks: true,
    activity: 'Failed on the third attempt',
    ref: 'control-plane',
    age: '18h',
    href: '#r4',
  },
]

export function RunRowsCard() {
  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>RunRows</span>
      </div>

      <p className="gal-note">
        The agent-run list — one prose string per row and no title, because the work is what names a
        run. Two independent axes: <code>lifecycle</code> (queued · working · done · failed) and{' '}
        <code>asks</code> (wants a person → the warm tick, whatever the lifecycle). A working row
        scans its activity via <code>ui/scan</code>; a queued row wears its wait as an hourglass
        mark with <code>ui/tooltip</code>, freeing the prose to name the work.
      </p>

      <div style={{ maxWidth: 560 }}>
        <RunRows
          label="NEEDS YOU"
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
          count={<span style={{ color: 'var(--color-cool)' }}>2</span>}
          rows={ROWS.filter((r) => !r.asks)}
          onPick={() => {}}
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
