import { Scan } from '../ui/scan/Scan'

// The Scan harness. The thing to review is the sweep's meaning and its
// restraint: the crest crosses in 2.2s, then holds for the rest of a 4.8s
// cycle — a heartbeat, not a loading bar. Toggle the gallery's ground: the
// base/crest pair inverts on light (see scan.css for the measured luminance
// argument), and the resting text must stay readable on both.

const LINE = 'Replaying 6 commits onto origin/main'

export function ScanCard() {
  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Scan</span>
      </div>

      <p className="gal-note">
        Emission applied to type — the same statement <code>readout/Emission</code> makes with a
        dot, made by the words themselves. One meaning: an agent is writing the answer. Never a
        loading state, and nothing else in the product may use the motion. Under reduced motion the
        text relights instead of going invisible — the glyphs are painted by a gradient clipped to
        them, so stopping the animation alone would leave a transparent line.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">ink (default)</span>
          <Scan style={{ font: '400 12.5px/1.3 var(--font-sans)' }}>{LINE}</Scan>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">cool — the accent, at a contrast cost</span>
          <Scan tone="cool" style={{ font: '400 12.5px/1.3 var(--font-sans)' }}>
            {LINE}
          </Scan>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">inactive — plain text, no machinery</span>
          <Scan active={false} style={{ font: '400 12.5px/1.3 var(--font-sans)' }}>
            {LINE}
          </Scan>
        </div>
      </div>
    </div>
  )
}

export default ScanCard
