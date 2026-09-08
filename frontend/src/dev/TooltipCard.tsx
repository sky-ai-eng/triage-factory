import { Tooltip, TOOLTIP_DELAY } from '../ui/tooltip/Tooltip'

// The Tooltip harness. The states worth reviewing are the two accessibility
// modes and the tap-inside-a-link case — hover any specimen, then tab to the
// focusable ones (focus opens with no delay), then click the in-link mark and
// confirm the page does not navigate.

export function TooltipCard() {
  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Tooltip</span>
      </div>

      <p className="gal-note">
        The project&rsquo;s one tooltip — the shipped rail&rsquo;s treatment, no arrow, one{' '}
        <code>TOOLTIP_DELAY</code> ({TOOLTIP_DELAY}ms) everywhere. It carries the definition of a
        label, never the value of a datum; a value the reader needs belongs on the page.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">focusable (default)</span>
          <Tooltip content="A review was requested from you on this pull request.">
            <span
              style={{
                font: '400 10.5px/1 var(--font-mono)',
                color: 'var(--color-warm)',
                border: '1px solid var(--color-warm-2)',
                borderRadius: 3,
                padding: '3px 7px',
              }}
            >
              Review requested
            </span>
          </Tooltip>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">four sides</span>
          <span style={{ display: 'inline-flex', gap: 14 }}>
            {(['top', 'bottom', 'left', 'right'] as const).map((s) => (
              <Tooltip key={s} content={s} side={s}>
                <span style={{ font: '400 10px/1 var(--font-mono)', color: 'var(--color-ink-3)' }}>
                  {s.toUpperCase()}
                </span>
              </Tooltip>
            ))}
          </span>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">inside a link — focusable=false</span>
          {/* The mark is scenery: the row is the interactive thing, the words
              reach assistive technology through the mark's aria-label, and a
              tap on the mark toggles the hint instead of navigating. */}
          <a
            href="#nowhere"
            onClick={(e) => e.preventDefault()}
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: 8,
              font: '400 11.5px/1 var(--font-sans)',
              color: 'var(--color-ink-2)',
              textDecoration: 'none',
            }}
          >
            the row is the anchor
            <Tooltip content="3 runs ahead of this one" focusable={false}>
              <span
                role="img"
                aria-label="3 runs ahead of this one"
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  minWidth: 15,
                  height: 15,
                  padding: '0 4px',
                  borderRadius: 8,
                  background: 'var(--color-warm)',
                  color: 'var(--color-warm-ink)',
                  font: '500 9.5px/1 var(--font-mono)',
                }}
              >
                3
              </span>
            </Tooltip>
          </a>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">clipping, transformed scroller</span>
          {/* The case a portal or a bare `position: fixed` would fail: an
              ancestor that clips AND carries a transform, which makes it the
              containing block for anything fixed inside it. The hint must
              escape it whole, and close as soon as this box scrolls. */}
          <div
            style={{
              overflow: 'auto',
              transform: 'translateZ(0)',
              width: 96,
              height: 28,
              border: '1px dashed var(--color-line-1)',
              borderRadius: 3,
              padding: '6px 8px 40px',
              boxSizing: 'border-box',
              font: '400 10px/1 var(--font-mono)',
              color: 'var(--color-ink-3)',
            }}
          >
            <Tooltip content="Escapes the clip; closes when this box scrolls." side="right">
              <span>CLIPPED</span>
            </Tooltip>
          </div>
        </div>

        <div className="gal-spec">
          <span className="gal-spec-tag">wrap — two lines, capped</span>
          <Tooltip
            content="Runs are claimed in queue order; a claim that fails returns its slot."
            wrap
          >
            <span style={{ font: '400 10px/1 var(--font-mono)', color: 'var(--color-ink-3)' }}>
              QUEUE
            </span>
          </Tooltip>
        </div>
      </div>
    </div>
  )
}

export default TooltipCard
