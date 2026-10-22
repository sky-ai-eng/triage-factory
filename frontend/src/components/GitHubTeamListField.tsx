import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import { handleOf } from '../lib/githubTeams'
import { fetchTeamGitHubGroups, type GitHubGroup } from '../pages/settings/teamConfig'

/** GitHubTeamListField is the editor for a review-request predicate's
 *  `requested_team_id_in`: a multi-select over the GitHub teams the handler's
 *  team maps (GET /api/teams/{team_id}/github-groups), submitting each one's
 *  GitHub team id. The server accepts only those: a request naming a GitHub
 *  team routes to the teams that map it.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored entry the team no longer maps stays in the value and renders as
 *  a removable "not mapped" chip, so opening and saving a handler never drops
 *  it without the user seeing it.
 *
 *  Wire shape: `string[] | undefined` — undefined when nothing is selected,
 *  which is no filter on the requested team. */
export default function GitHubTeamListField({
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
  // previous team's mappings.
  const [result, setResult] = useState<{ team: string; groups: GitHubGroup[] | null } | null>(null)

  useEffect(() => {
    if (!team) return
    let cancelled = false
    fetchTeamGitHubGroups(team).then((groups) => {
      if (!cancelled) setResult({ team, groups })
    })
    return () => {
      cancelled = true
    }
  }, [team])

  if (!team || result?.team !== team) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }

  const selected = value ?? []
  // A mapping saved before GitHub's ids were recorded has none to filter by,
  // so it is not offered until the GitHub poll identifies it.
  const mapped = (result.groups ?? []).filter((g) => g.github_team_id)
  const unmapped = selected.filter((id) => !mapped.some((g) => g.github_team_id === id))
  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((t) => t !== id) : [...selected, id])

  return (
    <div className="space-y-2">
      {result.groups === null && (
        <div
          role="alert"
          className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
        >
          Could not load this team&rsquo;s GitHub teams. Refresh the page to retry.
        </div>
      )}
      {result.groups !== null && mapped.length === 0 && (
        <p className="text-ui text-ink-3 italic">This team maps no GitHub teams.</p>
      )}
      {mapped.length > 0 && (
        <div className="max-h-48 overflow-y-auto rounded-lg border border-line-1 bg-raised py-1">
          {mapped.map((g) => (
            <label
              key={g.github_team_id}
              className="flex items-center gap-2 px-3 py-1.5 text-body text-ink-1 cursor-pointer hover:bg-tint-2"
            >
              <input
                type="checkbox"
                checked={selected.includes(g.github_team_id)}
                onChange={() => toggle(g.github_team_id)}
                className="accent-warm"
              />
              <span className="font-mono text-ui">{handleOf(g)}</span>
            </label>
          ))}
        </div>
      )}
      {unmapped.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {unmapped.map((id) => (
            <span
              key={id}
              className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border bg-tint-2 text-ink-2 border-line-2"
              title="The team does not map this GitHub team, so this filter never matches it."
            >
              {id} · not mapped
              <button
                type="button"
                onClick={() => set(selected.filter((t) => t !== id))}
                aria-label={`Remove ${id}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          ))}
        </div>
      )}
      {selected.length === 0 && mapped.length > 0 && (
        <p className="text-reported text-ink-3">None selected: no filter on the requested team.</p>
      )}
    </div>
  )
}
