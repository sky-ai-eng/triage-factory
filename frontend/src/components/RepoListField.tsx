import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import {
  fetchRepositorySlug,
  fetchTeamTrackedRepos,
  type TrackedRepository,
} from '../pages/settings/teamConfig'

/** RepoListField is the editor for a GitHub predicate's `repo_in`: a
 *  multi-select over the repositories the handler's team tracks
 *  (POST /api/repos/list with team_id), submitting each repository's registry
 *  `id`. The server accepts only those, since the router delivers a team's
 *  handlers only events from repositories it tracks.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored entry the team no longer tracks stays in the value and renders
 *  as a removable "not tracked" chip, named by a read of the row, so opening
 *  and saving a handler never drops it without the user seeing it.
 *
 *  Wire shape: `string[] | undefined` — undefined when nothing is selected,
 *  which matches every repository the team tracks. */
export default function RepoListField({
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
    repos: TrackedRepository[] | null
  } | null>(null)
  // Slugs of stored entries the team does not track, by id; null when the
  // row could not be read.
  const [stale, setStale] = useState<Record<string, string | null>>({})

  useEffect(() => {
    if (!team) return
    let cancelled = false
    fetchTeamTrackedRepos(team).then((repos) => {
      if (!cancelled) setResult({ team, repos })
    })
    return () => {
      cancelled = true
    }
  }, [team])

  const selected = value ?? []
  const tracked = result?.team === team ? (result.repos ?? []) : []
  const untracked =
    result?.team === team ? selected.filter((id) => !tracked.some((r) => r.id === id)) : []
  const unnamed = untracked.filter((id) => !(id in stale)).join(',')

  useEffect(() => {
    if (!unnamed) return
    let cancelled = false
    const ids = unnamed.split(',')
    Promise.all(ids.map((id) => fetchRepositorySlug(id).then((slug) => [id, slug] as const))).then(
      (pairs) => {
        if (!cancelled) setStale((prev) => ({ ...prev, ...Object.fromEntries(pairs) }))
      },
    )
    return () => {
      cancelled = true
    }
  }, [unnamed])

  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((r) => r !== id) : [...selected, id])
  const nameOf = (id: string) => stale[id] ?? id

  if (!team || result?.team !== team) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }

  return (
    <div className="space-y-2">
      {result.repos === null && (
        <div
          role="alert"
          className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
        >
          Could not load this team&rsquo;s repositories. Refresh the page to retry.
        </div>
      )}
      {result.repos !== null && tracked.length === 0 && (
        <p className="text-ui text-ink-3 italic">This team tracks no GitHub repositories.</p>
      )}
      {tracked.length > 0 && (
        <div className="max-h-48 overflow-y-auto rounded-lg border border-line-1 bg-raised py-1">
          {tracked.map((repo) => (
            <label
              key={repo.id}
              className="flex items-center gap-2 px-3 py-1.5 text-body text-ink-1 cursor-pointer hover:bg-tint-2"
            >
              <input
                type="checkbox"
                checked={selected.includes(repo.id)}
                onChange={() => toggle(repo.id)}
                className="accent-warm"
              />
              <span className="font-mono text-ui">{repo.slug}</span>
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
              title="The team does not track this repository, so this filter never matches it."
            >
              {nameOf(id)} · not tracked
              <button
                type="button"
                onClick={() => set(selected.filter((r) => r !== id))}
                aria-label={`Remove ${nameOf(id)}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          ))}
        </div>
      )}
      {selected.length === 0 && tracked.length > 0 && (
        <p className="text-reported text-ink-3">
          None selected: matches every repository this team tracks.
        </p>
      )}
    </div>
  )
}
