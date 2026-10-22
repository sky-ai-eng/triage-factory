import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import { useApiOrgId } from '../hooks/useApiOrgId'
import { CATALOG_NOUNS, loadCatalog, type CatalogGroup, type CatalogKind } from './predicateCatalog'

/** CatalogListField is the editor for a Jira or Linear predicate field that
 *  names statuses, priorities, issue types, workflow states, labels or label
 *  groups by the provider's id: a multi-select over the values the handler's
 *  team can match on, grouped by the tracked Jira project or Linear team they
 *  belong to, submitting each one's `id`. The server accepts only those, and
 *  never a name, since a value renamed in Jira or Linear would leave a filter
 *  on its name matching nothing.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored id none of those values carries any more (a status a workflow
 *  dropped, a project the team stopped tracking) stays in the value and
 *  renders as a removable chip with the id in its tooltip, so opening and
 *  saving a handler never drops it without the user seeing it.
 *
 *  Wire shape: `string[] | undefined` — undefined when nothing is selected,
 *  which is no filter. */
export default function CatalogListField({
  kind,
  teamId,
  value,
  onChange,
}: {
  kind: CatalogKind
  teamId: string
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
}) {
  const { teams, lastActingTeamId, loaded } = useTeams()
  const team = teamId || (loaded ? pickerDefault(teams, lastActingTeamId) : '')
  const orgId = useApiOrgId()
  const noun = CATALOG_NOUNS[kind]
  // Keyed by what was read so a team or kind switch reads as loading rather
  // than showing the previous list.
  const readKey = `${kind}|${orgId}|${team}`
  const [result, setResult] = useState<{ key: string; groups: CatalogGroup[] | null } | null>(null)

  useEffect(() => {
    if (!team || !orgId) return
    let cancelled = false
    loadCatalog(kind, orgId, team).then((groups) => {
      if (!cancelled) setResult({ key: `${kind}|${orgId}|${team}`, groups })
    })
    return () => {
      cancelled = true
    }
  }, [kind, orgId, team])

  const selected = value ?? []
  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((v) => v !== id) : [...selected, id])

  if (!team || !orgId || result?.key !== readKey) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }
  const groups = (result.groups ?? []).filter((g) => g.options.length > 0)
  const offered = new Set(groups.flatMap((g) => g.options.map((o) => o.id)))
  const unknown = selected.filter((id) => !offered.has(id))
  const empty =
    noun.source === 'jira_project'
      ? `No Jira project this team tracks offers any ${noun.many}.`
      : noun.source === 'linear_team'
        ? `No Linear team this team tracks offers any ${noun.many}.`
        : `Jira offers no ${noun.many}.`

  return (
    <div className="space-y-2">
      {result.groups === null && (
        <div
          role="alert"
          className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
        >
          Could not load the {noun.many} this team can filter on. Refresh the page to retry.
        </div>
      )}
      {result.groups !== null && groups.length === 0 && (
        <p className="text-ui text-ink-3 italic">{empty}</p>
      )}
      {groups.length > 0 && (
        <div className="max-h-56 overflow-y-auto rounded-lg border border-line-1 bg-raised py-1">
          {groups.map((g) => (
            <div key={g.key} role="group" aria-label={g.label || undefined}>
              {g.label && (
                <div className="px-3 pt-1.5 pb-0.5 text-reported font-medium text-ink-3">
                  {g.label}
                </div>
              )}
              {g.options.map((o) => (
                <label
                  key={o.id}
                  className="flex items-center gap-2 px-3 py-1.5 text-body text-ink-1 cursor-pointer hover:bg-tint-2"
                >
                  <input
                    type="checkbox"
                    checked={selected.includes(o.id)}
                    onChange={() => toggle(o.id)}
                    className="accent-warm"
                  />
                  {o.name}
                </label>
              ))}
            </div>
          ))}
        </div>
      )}
      {unknown.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {unknown.map((id) => (
            <span
              key={id}
              className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border bg-tint-2 text-ink-2 border-line-2"
              title={`Nothing this team tracks offers this ${noun.one} (id ${id}) any more, so this filter never matches it.`}
            >
              Unknown {noun.one}
              <button
                type="button"
                onClick={() => set(selected.filter((v) => v !== id))}
                aria-label={`Remove unknown ${noun.one}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          ))}
        </div>
      )}
      {selected.length === 0 && groups.length > 0 && (
        <p className="text-reported text-ink-3">None selected: no filter on the {noun.one}.</p>
      )}
    </div>
  )
}
