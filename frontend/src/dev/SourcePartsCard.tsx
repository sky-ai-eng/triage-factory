import { useEffect, useState } from 'react'
import type { CSSProperties } from 'react'
import { Odometer, Ticker } from '../ui/odometer/Odometer'
import SourceChart from '../ui/sourcechart/SourceChart'
import { figureAt, labelAt } from '../pages/team/schedule'
// The figure rows are the PAGE's layout, not the instruments' — the card
// borrows the stylesheet so the two are reviewed together.
import '../pages/team/source-pages.css'

// The instruments a source page's left column is made of, on their own stage.
//
// Both of them are about TIME, so both need a replay: a screenshot of an
// odometer is a number, and the whole argument is how it got there.

/** The prototype's own fortnight, so the drawn shape can be compared to it. */
const GH: Array<[number, number]> = [
  [27, 3],
  [26, 5],
  [10, 1],
  [11, 2],
  [18, 4],
  [20, 2],
  [34, 3],
  [23, 2],
  [18, 3],
  [12, 2],
  [10, 1],
  [27, 4],
  [20, 4],
  [18, 3],
]

const ROWS: Array<{ value: number | null; label: string; tone?: 'warm' | 'alarm' | 'faint' }> = [
  { value: 33, label: 'tracked of 48 visible' },
  { value: 164, label: 'events in 7 days' },
  { value: null, label: 'became tasks', tone: 'warm' },
  { value: 380, label: 'runs against these repositories' },
  { value: 40, label: 'since the last poll', tone: 'faint' },
]

export function SourcePartsCard() {
  const [run, setRun] = useState(0)
  const [tasks, setTasks] = useState(22)
  const [series, setSeries] = useState(true)

  // A task landing while the page is watched. The flash is the point: the
  // figure is not allowed to change quietly.
  useEffect(() => {
    const t = setInterval(() => setTasks((n) => n + 1), 5000)
    return () => clearInterval(t)
  }, [])

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Odometer · Ticker · SourceChart</span>
        <button type="button" className="gal-btn" onClick={() => setRun((r) => r + 1)}>
          replay
        </button>
      </div>

      <p className="gal-note">
        A column of figures arriving, and one of them changing while you watch. Each digit is a
        30-step ramp landing on the third pass, one beat behind the digit to its left; the labels
        follow on their own schedule, so five rows read as <strong>one movement resolving</strong>{' '}
        rather than five things happening. A figure nothing answers yet is an em dash — never a
        zero, which would be a claim about a team&rsquo;s week.
      </p>

      <div className="gal-specimens" key={run}>
        <div className="gal-spec" style={{ minWidth: 280 }}>
          <span className="gal-spec-tag">the left column</span>
          <div className="sp-figrows" style={{ width: '100%' }}>
            {ROWS.map((r, i) => (
              <div className="sp-figrow" data-tone={r.tone} key={i}>
                <span className="sp-figrow-v">
                  {r.tone === 'warm' ? (
                    <Ticker value={tasks} variant="row" />
                  ) : (
                    <Odometer
                      value={r.value}
                      at={figureAt(i)}
                      suffix={i === 4 ? 's' : undefined}
                      label={i === 4 ? '40 seconds' : undefined}
                    />
                  )}
                </span>
                <span
                  className="sp-figrow-l sp-fade"
                  style={{ '--at': labelAt(i) + 's' } as CSSProperties}
                >
                  {r.label}
                </span>
              </div>
            ))}
          </div>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">the band&rsquo;s live figure</span>
          <Ticker value={tasks} />
          <button type="button" className="gal-btn" onClick={() => setTasks((n) => n + 1)}>
            a task arrives
          </button>
        </div>
      </div>

      <p className="gal-note">
        The chart draws its baseline, then reveals the area and <strong>both</strong> lines under
        one mask wipe — two wipes would read as two charts, and the gap between the lines is the
        whole subject: events nothing was done about. Hover it to name a day. With no series it
        keeps its height and its baseline, because a page waiting on an aggregation should look like
        a chart waiting for its answer rather than a hole.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec" style={{ flex: 1, minWidth: 320 }}>
          <span className="gal-spec-tag">
            {series ? 'fourteen days' : 'nothing counts them yet'}
          </span>
          <div style={{ width: '100%' }} key={String(series) + run}>
            <SourceChart
              title="WHAT THE REPOSITORIES SENT"
              keys={['EVENTS', 'TASKS']}
              units={['events', 'tasks']}
              series={series ? GH.map(([events, t]) => ({ events, tasks: t })) : null}
            />
          </div>
          <button type="button" className="gal-btn" onClick={() => setSeries((s) => !s)}>
            {series ? 'drop the series' : 'give it a series'}
          </button>
        </div>
      </div>
    </div>
  )
}

export default SourcePartsCard
