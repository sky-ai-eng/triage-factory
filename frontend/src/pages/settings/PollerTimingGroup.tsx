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
 *
 * showGitHub (default true) mirrors showJira for the inverse case: the setup
 * wizard splits the cadences into a GitHub poll step and a separate Jira poll
 * step (shown only once Jira is connected), so the Jira step renders this group
 * with showGitHub=false to show the Jira control alone.
 *
 * showHeading (default true) suppresses the "Poller timing" title in the wizard,
 * where each cadence is its own step already labelled "GitHub poll interval" /
 * "Jira poll interval"; Settings keeps it (multiple sections on one page).
 */
export default function PollerTimingGroup({
  value,
  onChange,
  showGitHub = true,
  showJira = true,
  showHeading = true,
}: {
  value: PollerTimingValue
  onChange: (patch: Partial<PollerTimingValue>) => void
  showGitHub?: boolean
  showJira?: boolean
  showHeading?: boolean
}) {
  return (
    <Section>
      {showHeading && (
        <h2 className="text-[13px] font-medium text-text-secondary mb-4">Poller timing</h2>
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
