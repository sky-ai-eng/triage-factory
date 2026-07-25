import { useState, useEffect } from 'react'
import { Link } from 'react-router'
import { useOrgHref } from '../hooks/useOrgHref'

interface TeamSettingsResponse {
  jira_projects: { key: string }[]
}

interface Props {
  jiraKey: string
  linearKey: string
  onJiraChange: (key: string) => void
  onLinearChange: (key: string) => void
  // The team whose configured Jira projects populate the picker. A UUID
  // (the acting team in the multi-team create modal) or the literal
  // "default" — the backend's /api/settings/team/{team_id} accepts both.
  // Empty/undefined falls back to "default": the single-team detail view
  // and solo/local, where the sole team's config is the right source.
  // Must match the team the create POST validates jira_project_key
  // against, or valid team-B projects are hidden and default-team choices
  // 400. The create modal clears the selection on team change (see the
  // parent's [team] effect); the fixed-team detail view never changes it.
  teamId?: string
}

// TrackerProjectPickers renders two independent single-selects: Jira
// (sourced from config.Jira.Projects) and Linear (always disabled
// today — Linear integration is future). The two are independent on
// purpose; a project legitimately may track work in both systems.
//
// linearKey is plumbed through but disabled for now so the component
// stays forward-compatible with the Linear integration ticket without
// a future ticket needing to refactor the create modal / detail view.
//
// Important UX rule: the Jira picker is ALWAYS rendered when the
// project has a non-empty jiraKey, even if the configured-projects
// list is empty or the settings fetch failed. Hiding the picker in
// that state would leave a user with a stale link no way to clear
// it from the UI — the backend explicitly supports clearing tracker
// fields, and the UI has to give them the surface to do so.
export default function TrackerProjectPickers({
  jiraKey,
  linearKey,
  onJiraChange,
  onLinearChange,
  teamId,
}: Props) {
  const [jiraProjects, setJiraProjects] = useState<string[]>([])
  const [jiraEnabled, setJiraEnabled] = useState(false)
  const [loading, setLoading] = useState(true)
  const orgHref = useOrgHref()

  // Refetch when the team changes so the offered projects always match the
  // team the create POST will validate against. `setLoading(true)` resets
  // the picker to its loading view during the swap rather than briefly
  // showing the prior team's projects.
  const settingsTeam = teamId || 'default'
  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    const load = async () => {
      try {
        const res = await fetch(`/api/settings/team/${encodeURIComponent(settingsTeam)}`, {
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        if (!res.ok) {
          return
        }
        const data: TeamSettingsResponse = await res.json()
        if (controller.signal.aborted) return
        const keys = (data.jira_projects || []).map((p) => p.key)
        setJiraEnabled(keys.length > 0)
        setJiraProjects(keys)
      } catch (err) {
        // Network failure / abort — quietly fall through to the
        // disabled state. Without this catch, an unmount mid-flight
        // surfaces an unhandled rejection in the browser console.
        // We intentionally don't toast: the picker has a sensible
        // disabled view, and a transient settings fetch failure
        // shouldn't pop a noisy error for a UI element the user
        // may not even be looking at.
        if (controller.signal.aborted) return
        console.debug('TrackerProjectPickers: settings load failed', err)
      } finally {
        if (!controller.signal.aborted) setLoading(false)
      }
    }
    load()
    return () => controller.abort()
  }, [settingsTeam])

  // Render the Jira picker if either we have configured projects to
  // pick from, OR the project already carries a stale jiraKey we
  // need to give the user a path to clear. Without this second
  // condition, a project that linked Jira before config got cleared
  // would show no Jira UI at all — the stale value would be invisible
  // and unactionable from the UI.
  const showJiraPicker = jiraProjects.length > 0 || (!loading && jiraKey !== '')
  // Stale = we have a current value but it's no longer in the
  // configured list. Surfacing this explicitly tells the user why
  // their selection is awkward instead of leaving them to wonder.
  const jiraStale = !loading && jiraKey !== '' && !jiraProjects.includes(jiraKey)

  return (
    <div className="space-y-3">
      <div>
        <div className="flex items-center justify-between mb-1">
          <span className="text-[11px] font-medium text-text-tertiary uppercase tracking-wide">
            Jira
          </span>
          {!loading && !jiraEnabled && (
            <Link to={orgHref('/settings')} className="text-[11px] text-accent hover:underline">
              Configure Jira
            </Link>
          )}
        </div>
        {loading ? (
          <div className="text-[12px] text-text-tertiary py-1">Loading…</div>
        ) : showJiraPicker ? (
          <>
            <select
              value={jiraKey}
              onChange={(e) => onJiraChange(e.target.value)}
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/60 px-3 py-2 text-[13px] text-text-primary
                focus:outline-none focus:border-accent focus:bg-white
              "
            >
              <option value="">— None —</option>
              {/* Stale value as its own option so it remains
                  selectable until the user picks something else or
                  clears it. Without this, the <select> would render
                  with a value that has no matching <option> and the
                  display would silently fall back to the first item. */}
              {jiraStale && <option value={jiraKey}>{jiraKey} (no longer in Settings)</option>}
              {jiraProjects.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
            {jiraStale && (
              <div className="text-[11px] text-text-tertiary mt-1 italic">
                This project key isn&rsquo;t in your current Jira config. Pick a configured one or
                clear the link.
              </div>
            )}
          </>
        ) : (
          <div className="text-[12px] text-text-tertiary py-1 italic">
            {jiraEnabled
              ? 'No Jira projects in config — add them on the Settings page.'
              : 'Jira not configured.'}
          </div>
        )}
      </div>

      <div>
        <div className="flex items-center justify-between mb-1">
          <span className="text-[11px] font-medium text-text-tertiary uppercase tracking-wide">
            Linear
          </span>
        </div>
        <div className="text-[12px] text-text-tertiary py-1 italic">
          Linear integration coming soon.
        </div>
        {/* Hidden input so the parent's controlled-state plumbing doesn't
            need a special-case for "Linear is always empty" — when the
            integration ships, this swaps in a real <select> and the
            create handler keeps working unchanged. */}
        <input type="hidden" value={linearKey} readOnly />
        {/* Silence unused-prop lint for the future Linear path. */}
        <span className="hidden">{onLinearChange.length}</span>
      </div>
    </div>
  )
}
