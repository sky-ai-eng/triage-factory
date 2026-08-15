import { useState } from 'react'
import Checkbox from '../ui/checkbox/Checkbox'
import Countdown from '../ui/countdown/Countdown'
import Hold from '../ui/hold/Hold'

// The three primitives the verb bar and the tables are built from. Grouped into
// one card because each is small and they are read together — a hold and the
// countdown that follows it are two halves of the same idea.

export function PrimitivesCard() {
  const [checked, setChecked] = useState(true)
  const [run, setRun] = useState(0)
  const [held, setHeld] = useState<string | null>(null)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Checkbox</span>
      </div>
      <p className="gal-note">
        Unchecked is structure — a hairline and nothing else. Checked is a value: a solid warm block
        carrying a mark painted in <code>--color-ground</code>, so it is a hole punched in the block
        rather than a white tick that only works on one theme. The mark is centred on its own ink,
        not its bounding box.
      </p>
      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">off / on</span>
          <Checkbox checked={checked} onChange={setChecked} title="Select row" />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">mixed</span>
          <Checkbox indeterminate title="Some selected" />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">disabled, still focusable</span>
          <Checkbox checked disabled label="Owned by another team" />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">11 · 13 · 20px</span>
          <span style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <Checkbox checked size={11} title="11px" />
            <Checkbox checked size={13} title="13px" />
            <Checkbox checked size={20} title="20px" />
          </span>
        </div>
      </div>
      <p className="gal-note">
        Tab to it and press space. Enter deliberately does nothing — a row checkbox sits inside a
        row with its own Enter meaning, and swallowing it would take the row&rsquo;s primary action
        away. Disabled stays in the tab order, because a row is usually disabled for a reason a
        keyboard reader should be able to find out.
      </p>

      <div className="gal-cardhead" style={{ marginTop: 'var(--space-10)' }}>
        <span>Countdown</span>
        <button type="button" className="gal-btn" onClick={() => setRun((r) => r + 1)}>
          run 10s
        </button>
      </div>
      <p className="gal-note">
        Three crates emptying top down, each draining left to right. It drains rather than fills
        because a window is time being spent, and it never ticks — a per-second jump reads as a
        stutter at fourteen pixels. Empty crates stay drawn, so the footprint never changes.
      </p>
      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">full · half · empty</span>
          <span style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <Countdown frac={1} />
            <Countdown frac={0.5} />
            <Countdown frac={0} />
          </span>
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">warm · alarm · ink</span>
          <span style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <Countdown frac={0.6} tone="warm" />
            <Countdown frac={0.6} tone="alarm" />
            <Countdown frac={0.6} tone="ink" />
          </span>
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">self-driven, 10s</span>
          <Countdown key={run} seconds={10} size={20} />
        </div>
      </div>
      <p className="gal-note">
        Under <code>prefers-reduced-motion</code> this is the one thing in the system that
        <strong> coarsens instead of stopping</strong>: it steps once a second rather than once a
        frame. Here the motion <em>is</em> the remaining time, so removing it would be information
        loss dressed as an accommodation.
      </p>

      <div className="gal-cardhead" style={{ marginTop: 'var(--space-10)' }}>
        <span>Hold</span>
        <span className="gal-route">{held ? 'confirmed: ' + held : 'nothing committed'}</span>
      </div>
      <p className="gal-note">
        A commit you have to mean, replacing a confirmation dialog. Point at it and the fill drains,
        then a cross blinks — the frame was checked and it is empty. Press and the rings grow{' '}
        <em>outward from where your finger landed</em>, because the commit is measured from you.
        They step rather than sweep: a smooth fill reads as waiting, discrete stops read as
        counting.
      </p>
      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">850ms, warm</span>
          <Hold label="Hold to save" onConfirm={() => setHeld('save')} />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">900ms, alarm</span>
          <Hold label="Hold to remove" tone="alarm" ms={900} onConfirm={() => setHeld('remove')} />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">ms 0 — the shortest form</span>
          <Hold label="Save" ms={0} onConfirm={() => setHeld('instant')} />
        </div>
      </div>
      <p className="gal-note">
        Space and Enter run the same engine, from the centre. Try starting a hold at the very edge
        and dragging off it — pointer capture means it survives, because the label changes width
        mid-gesture and the button resizes under the cursor. Zero is the gesture&rsquo;s shortest
        form, not its absence: the rings still arrive, so a cheap verb and an expensive one are the
        same shape at different prices.
      </p>
    </div>
  )
}

export default PrimitivesCard
