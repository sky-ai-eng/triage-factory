import { Section, Field, inputClass, POLL_INTERVAL_OPTIONS } from './primitives'

interface PollerTimingValue {
  github_poll_interval: string
  jira_poll_interval: string
}

/**
 * PollerTimingGroup is the org-level poller-cadence field group — how often
 * the GitHub and Jira pollers run. Both intervals are org_settings columns,
 * so they round-trip via POST /api/settings/org like the rest of the org
 * config. The Jira interval is suppressed (showJira false) on surfaces where
 * Jira isn't connected yet, since the cadence is meaningless without it.
 */
export default function PollerTimingGroup({
  value,
  onChange,
  showJira = true,
}: {
  value: PollerTimingValue
  onChange: (patch: Partial<PollerTimingValue>) => void
  showJira?: boolean
}) {
  return (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">Poller timing</h2>
      <div className="space-y-3">
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
