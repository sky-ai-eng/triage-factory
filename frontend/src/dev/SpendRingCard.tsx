import { SpendRing } from '../ui/spendring/SpendRing'

// The SpendRing harness. The review is the hole: hover a band and the total
// swaps for that model's figure and name — no legend anywhere, which is the
// component's whole design decision. Check the zero-spend ring reads as quiet
// rather than broken, and that the small ring's caption still fits.

export function SpendRingCard() {
  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>SpendRing</span>
      </div>

      <p className="gal-note">
        A total that decomposes in its own hole on hover. No legend, ever — a permanent legend is a
        promise of content, and on a zero-spend day it renders blank rows. The hovered band thickens
        (weight is the only channel colour left free), the money rule drops precision rather than
        characters, and the ring is a link because hover is an accelerator, not the only route.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">a day with spend — hover the bands</span>
          <SpendRing
            models={[
              { name: 'claude-opus-5', v: 24.8 },
              { name: 'claude-sonnet-5', v: 13.1 },
              { name: 'claude-haiku-4-5-20251001', v: 3.3 },
            ]}
          />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">zero spend — quiet, not broken</span>
          <SpendRing models={[]} />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">small ring, big figure</span>
          <SpendRing models={[{ name: 'claude-opus-5', v: 1249.4 }]} size={104} />
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">offline</span>
          <SpendRing models={[{ name: 'claude-opus-5', v: 24.8 }]} offline />
        </div>
      </div>
    </div>
  )
}

export default SpendRingCard
