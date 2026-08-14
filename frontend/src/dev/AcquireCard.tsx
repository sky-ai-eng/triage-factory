import { useState } from 'react'
import Acquire from '../ui/acquire/Acquire'

// The Acquire harness. The sequence runs once per mount and has no replay
// method on purpose — a value that re-acquires itself on change is an ambient
// loop wearing a costume — so replaying here is a remount, driven by `key`.
// That is exactly how a caller re-runs it in a real screen.

export function AcquireCard() {
  const [run, setRun] = useState(0)
  const [late, setLate] = useState<string | null>(null)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Acquire</span>
        <button
          type="button"
          className="gal-btn"
          onClick={() => {
            setLate(null)
            setRun((r) => r + 1)
          }}
        >
          replay
        </button>
      </div>

      <p className="gal-note">
        Four beats — frame, flash, frame, cross — then the value lands. It is not a loading mask:
        the sequence takes the same time whether the answer is already in hand or still in flight,
        because its job is to point at one figure rather than to fill time. Replay remounts it.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">value in hand</span>
          <Acquire key={`a${run}`} value="164" />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">left aligned</span>
          <Acquire key={`b${run}`} value="$41.20" align="left" width={72} />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">call outlives the beats</span>
          {/* Hands off to the SHAPE OF THE ANSWER, not the reserved box carried
              over. A rectangle that persists past the sequence reads as the
              sequence stalling. */}
          <Acquire key={`c${run}`} value={null} skeleton="$--.--" width={72} />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">unreachable</span>
          {/* Inert, because nothing is still happening. Waiting breathes; this
              does not, and that difference is the only thing separating "slow"
              from "gone". */}
          <Acquire key={`d${run}`} value={null} failed skeleton="$--.--" width={72} />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">arrives late</span>
          <Acquire key={`e${run}`} value={late} skeleton="--" />
          <button type="button" className="gal-btn" onClick={() => setLate('7')}>
            deliver
          </button>
        </div>
      </div>

      <p className="gal-note">
        Under <code>prefers-reduced-motion: reduce</code> there are no beats at all — it mounts
        straight into its end state. Not a shortened sequence: a sequence at double speed is still a
        sequence. Toggle the preference in your OS and replay to see it.
      </p>
    </div>
  )
}

export default AcquireCard
