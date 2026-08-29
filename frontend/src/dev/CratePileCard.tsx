import { useState } from 'react'
import { CratePile } from '../ui/cratepile/CratePile'

// The CratePile harness. The review is the increment: press +1 and confirm no
// existing crate blinks — the cell keying is what this card exists to check —
// then run the count past 36 and watch the drawing saturate while the figure
// keeps counting.

export function CratePileCard() {
  const [count, setCount] = useState(23)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>CratePile</span>
      </div>

      <p className="gal-note">
        A standing count as a 2.5D pile — for a quantity that accumulates rather than resets. The
        pile is texture with a floor, not a tally: it saturates at 36 crates, the figure carries the
        number alone past that, and zero draws the full dashed footprint because a pallet with
        nothing on it is the same pallet.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">standing backlog</span>
          <CratePile count={count} caption="open pull requests" captionOne="open pull request" />
          <span style={{ display: 'inline-flex', gap: 6 }}>
            <button type="button" className="gal-btn" onClick={() => setCount((n) => n + 1)}>
              +1
            </button>
            <button
              type="button"
              className="gal-btn"
              onClick={() => setCount((n) => Math.max(0, n - 1))}
            >
              −1
            </button>
            <button type="button" className="gal-btn" onClick={() => setCount(112)}>
              =112
            </button>
            <button type="button" className="gal-btn" onClick={() => setCount(0)}>
              =0
            </button>
          </span>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">offline — inert, not zero</span>
          <CratePile count={23} offline />
        </div>
      </div>
    </div>
  )
}

export default CratePileCard
