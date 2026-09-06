import { useState } from 'react'
import { Segmented } from '../ui/segmented/Segmented'

// The Segmented harness. The review is the mark: pick an option and watch one
// element travel, the words staying put; then arrow through with the
// keyboard, where a struck option is read but never landed on.

const EXPIRY = [
  { value: '7', label: '7d' },
  { value: '30', label: '30d' },
  { value: '60', label: '60d' },
  { value: '90', label: '90d' },
  { value: 'custom', label: 'a date' },
  { value: 'never', label: 'never', disabled: true, note: 'sky-ai-eng caps tokens at 90 days' },
]
const VIEWS = [
  { value: 'roster', label: 'Roster' },
  { value: 'sources', label: 'Sources' },
  { value: 'models', label: 'Models' },
]

export function SegmentedCard() {
  const [theme, setTheme] = useState('light')
  const [exp, setExp] = useState('30')
  const [view, setView] = useState('roster')

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Segmented</span>
        <span className="gal-route">{theme + ' · ' + exp + ' · ' + view}</span>
      </div>

      <p className="gal-note">
        One choice from a few, all of them visible, taking effect at once. The mark moves and the
        words do not — one element travelling on the content curve — so the eye reads a change of
        state rather than a redraw. Four variants, the four ways the system already marks a
        selection; <strong>spine</strong> is the default. A struck option stays in place with its
        reason on hover: a preset the policy rules out is information, not a verb to hide.
      </p>

      <div className="gal-specimens">
        {(['spine', 'tick', 'housed', 'plate'] as const).map((variant) => (
          <div className="gal-spec" key={variant}>
            <span className="gal-spec-tag">{variant}</span>
            <Segmented
              variant={variant}
              options={['light', 'dark', 'system']}
              value={theme}
              onChange={setTheme}
              label="Appearance"
            />
            <Segmented
              variant={variant}
              options={EXPIRY}
              value={exp}
              onChange={setExp}
              label="Expiry"
            />
            <Segmented
              variant={variant}
              mono={false}
              options={VIEWS}
              value={view}
              onChange={setView}
              label="View"
            />
            <Segmented
              variant={variant}
              disabled
              options={['light', 'dark', 'system']}
              value="dark"
              label="Appearance"
            />
          </div>
        ))}
      </div>
    </div>
  )
}

export default SegmentedCard
