import { useRef } from 'react'
import { Section, Field, inputClass, POLL_INTERVAL_OPTIONS } from './primitives'
import { nextRadioIndex } from '../../lib/rovingRadio'

interface PollerTimingValue {
  github_poll_interval: string
  jira_poll_interval: string
}

// IntervalScale is the flush, borderless cadence selector for the wizard: the
// intervals laid out as a segmented scale (frequent → infrequent), the chosen
// one lit on an accent rail, no dropdown chrome. radiogroup + roving tabIndex +
// arrow keys for a11y, mirroring the model-tier ladder.
function IntervalScale({
  value,
  onChange,
  ariaLabel,
}: {
  value: string
  onChange: (value: string) => void
  ariaLabel: string
}) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])
  const selectedIndex = POLL_INTERVAL_OPTIONS.findIndex((o) => o.value === value)
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, POLL_INTERVAL_OPTIONS.length)
    if (next === null) return
    e.preventDefault()
    onChange(POLL_INTERVAL_OPTIONS[next].value)
    btnRefs.current[next]?.focus()
  }
  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className="flex items-stretch"
    >
      {POLL_INTERVAL_OPTIONS.map((o, i) => {
        const selected = o.value === value
        return (
          <button
            key={o.value}
            ref={(el) => {
              btnRefs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={i === tabbable ? 0 : -1}
            onClick={() => onChange(o.value)}
            className="group relative flex-1 px-3 py-2 text-center outline-none"
          >
            <span
              className={`text-body font-medium transition-colors ${
                selected ? 'text-warm' : 'text-ink-2 group-hover:text-ink-1'
              }`}
            >
              {o.label}
            </span>
            <span
              aria-hidden
              className={`absolute inset-x-0 bottom-0 h-[2px] transition-colors ${
                selected
                  ? 'bg-warm shadow-[0_0_10px_-1px_var(--color-warm)]'
                  : 'bg-[var(--color-line-1)]'
              }`}
            />
          </button>
        )
      })}
    </div>
  )
}

/**
 * PollerTimingGroup is the org-level poller-cadence field group — how often
 * the GitHub and Jira pollers run. Both intervals are org_settings columns,
 * so they round-trip via POST /api/settings/org like the rest of the org
 * config. The Jira interval is suppressed (showJira false) on surfaces where
 * Jira isn't connected yet, since the cadence is meaningless without it.
 *
 * showGitHub (default true) mirrors showJira for the inverse case: the setup
 * wizard splits the cadences into a GitHub poll step and a separate Jira poll
 * step (shown only once Jira is connected), so the Jira step renders this group
 * with showGitHub=false to show the Jira control alone.
 *
 * showHeading (default true) suppresses the "Poller timing" title in the wizard,
 * where each cadence is its own step already labelled "GitHub poll interval" /
 * "Jira poll interval"; Settings keeps it (multiple sections on one page).
 *
 * bare (default false) renders the flush IntervalScale (no Section card, no
 * dropdown) for the wizard; Settings keeps the carded <select>.
 */
export default function PollerTimingGroup({
  value,
  onChange,
  showGitHub = true,
  showJira = true,
  showHeading = true,
  bare = false,
}: {
  value: PollerTimingValue
  onChange: (patch: Partial<PollerTimingValue>) => void
  showGitHub?: boolean
  showJira?: boolean
  showHeading?: boolean
  bare?: boolean
}) {
  if (bare) {
    return (
      <div className="space-y-5">
        {showGitHub && (
          <IntervalScale
            value={value.github_poll_interval}
            onChange={(v) => onChange({ github_poll_interval: v })}
            ariaLabel="GitHub poll interval"
          />
        )}
        {showJira && (
          <IntervalScale
            value={value.jira_poll_interval}
            onChange={(v) => onChange({ jira_poll_interval: v })}
            ariaLabel="Jira poll interval"
          />
        )}
      </div>
    )
  }

  return (
    <Section>
      {showHeading && (
        <h2 className="text-body font-medium text-ink-2 mb-4">Poller timing</h2>
      )}
      <div className="space-y-3">
        {showGitHub && (
          <Field label="GitHub poll interval">
            <select
              value={value.github_poll_interval}
              onChange={(e) => onChange({ github_poll_interval: e.target.value })}
              className={inputClass}
            >
              {POLL_INTERVAL_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </Field>
        )}
        {showJira && (
          <Field label="Jira poll interval">
            <select
              value={value.jira_poll_interval}
              onChange={(e) => onChange({ jira_poll_interval: e.target.value })}
              className={inputClass}
            >
              {POLL_INTERVAL_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </Field>
        )}
      </div>
    </Section>
  )
}
