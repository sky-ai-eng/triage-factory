import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import { fetchTeamTrackedJiraProjects, type TrackedJiraProject } from '../pages/settings/teamConfig'

/** JiraProjectListField is the editor for a Jira predicate's
 *  `jira_project_id_in`, and for key_changed's `old_jira_project_id_in`: a
 *  multi-select over the Jira projects the handler's team tracks (the
 *  team-settings read's `jira_projects`), submitting each one's registry
 *  `id`. The server accepts only those, since the router delivers a team's
 *  handlers only events from projects it tracks.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored entry the team no longer tracks stays in the value and renders
 *  as a removable "not tracked" chip, so opening and saving a handler never
 *  drops it without the user seeing it. Nothing the editor can read names a
 *  registry row the team does not track, so the chip shows its id.
 *
 *  Wire shape: `string[] | undefined` — undefined when nothing is selected,
 *  which is no filter on the project. */
export default function JiraProjectListField({
  teamId,
  value,
  onChange,
}: {
  teamId: string
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
}) {
  const { teams, lastActingTeamId, loaded } = useTeams()
  const team = teamId || (loaded ? pickerDefault(teams, lastActingTeamId) : '')
  // Keyed by team so a team switch reads as loading rather than showing the
  // previous team's list.
  const [result, setResult] = useState<{
    team: string
    projects: TrackedJiraProject[] | null
  } | null>(null)

  useEffect(() => {
    if (!team) return
    let cancelled = false
    fetchTeamTrackedJiraProjects(team).then((projects) => {
      if (!cancelled) setResult({ team, projects })
    })
    return () => {
      cancelled = true
    }
  }, [team])

  const selected = value ?? []
  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((p) => p !== id) : [...selected, id])

  if (!team || result?.team !== team) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }
  const tracked = result.projects ?? []
  const untracked = selected.filter((id) => !tracked.some((p) => p.id === id))

  return (
    <div className="space-y-2">
      {result.projects === null && (
        <div
          role="alert"
          className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
        >
          Could not load this team&rsquo;s Jira projects. Refresh the page to retry.
        </div>
      )}
      {result.projects !== null && tracked.length === 0 && (
        <p className="text-ui text-ink-3 italic">This team tracks no Jira projects.</p>
      )}
      {tracked.length > 0 && (
        <div className="max-h-48 overflow-y-auto rounded-lg border border-line-1 bg-raised py-1">
          {tracked.map((p) => (
            <label
              key={p.id}
              className="flex items-center gap-2 px-3 py-1.5 text-body text-ink-1 cursor-pointer hover:bg-tint-2"
            >
              <input
                type="checkbox"
                checked={selected.includes(p.id)}
                onChange={() => toggle(p.id)}
                className="accent-warm"
              />
              <span className="font-mono text-ui">{p.key}</span>{' '}
              {p.name && <span className="text-ink-2">{p.name}</span>}
            </label>
          ))}
        </div>
      )}
      {untracked.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {untracked.map((id) => (
            <span
              key={id}
              className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border bg-tint-2 text-ink-2 border-line-2"
              title="The team does not track this Jira project, so this filter never matches it."
            >
              {id} · not tracked
              <button
                type="button"
                onClick={() => set(selected.filter((p) => p !== id))}
                aria-label={`Remove ${id}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          ))}
        </div>
      )}
      {selected.length === 0 && tracked.length > 0 && (
        <p className="text-reported text-ink-3">None selected: no filter on the Jira project.</p>
      )}
    </div>
  )
}
