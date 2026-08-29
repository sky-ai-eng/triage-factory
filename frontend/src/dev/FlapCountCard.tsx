import { useState } from 'react'
import { FlapCount } from '../ui/flapcount/FlapCount'

// The FlapCount harness. The review is the increment: press the buttons and
// watch which flaps turn — only the digits whose face changed, rolling with
// the sign, while everything else stays still. Compare Ticker in the shell
// card, which frames the whole figure because there the figure is the news.

const compact = (n: number) =>
  n < 1000
    ? String(n)
    : n < 10000
      ? (Math.round(n / 100) / 10).toFixed(1) + 'k'
      : Math.round(n / 1000) + 'k'

export function FlapCountCard() {
  const [v, setV] = useState(312)
  const [backlog, setBacklog] = useState(996)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>FlapCount</span>
      </div>

      <p className="gal-note">
        A running total changing — one window per digit, and only the digits whose face turns get a
        roll and a frame. Sibling of <code>Odometer</code>/<code>Ticker</code>, replacing neither:
        Ticker for an arrival, FlapCount for an increment. Nothing rolls on first paint, and a width
        change (99 → 103) flaps every column, the new lead arriving from blank.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">increments and decrements</span>
          <FlapCount value={v} size={24} label={v + ' events triaged'} />
          <span style={{ display: 'inline-flex', gap: 6 }}>
            <button type="button" className="gal-btn" onClick={() => setV((n) => n + 23)}>
              +23
            </button>
            <button
              type="button"
              className="gal-btn"
              onClick={() => setV((n) => Math.max(0, n - 4))}
            >
              −4
            </button>
          </span>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">
            format — the beat is the number, the glyphs are the string
          </span>
          <FlapCount value={backlog} size={24} format={compact} label={backlog + ' open'} />
          <button type="button" className="gal-btn" onClick={() => setBacklog((n) => n + 100)}>
            +100
          </button>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">nothing to report</span>
          <FlapCount value={null} size={24} />
        </div>
      </div>
    </div>
  )
}

export default FlapCountCard
