import { useState, useEffect, useMemo, useCallback } from 'react'
import { Link } from 'react-router'
import { Check, X } from 'lucide-react'
import { apiJSON, apiListAll, httpErrorMessage } from '../lib/apiClient'
import { useOrgHref } from '../hooks/useOrgHref'
import { toast } from './Toast/toastStore'
import type { RepoOption } from '../types'

interface Props {
  /** Selected repository ids — what `pinned_repository_ids` carries. */
  value: string[]
  onChange: (next: string[]) => void
  // The team whose tracked repos the picker offers. Empty falls back to
  // the "default" alias, which the repos endpoint resolves in local mode
  // only — the sole-team case where no write-time picker renders. Multi
  // callers must supply a uuid: the create modal holds the fetch via
  // `disabled` until the acting team resolves, then threads it here so
  // the offered repos match the team the project is created under, and
  // server-side validatePinnedRepos agrees.
  teamId?: string
  // When true the picker waits — no fetch, no interaction — until the
  // acting team resolves. The create modal sets this so a multi-team user
  // can't pick the *default* team's repos in the cold-load window before
  // the real acting team is known; those repos would 400 on submit against
  // the resolved team.
  disabled?: boolean
}

// RepoMultiSelect is the project create modal's pinned-repos picker. It
// holds repository ids and displays names: the ids are what the pinned set
// is submitted as, and a name is only ever what a chip reads.
//
// The offered set is the acting team's tracked repos, which takes two reads
// because the two facts live in different places. The team's tracked set is
// name-shaped — tracking is the ingest edge that mints a registry row, so it
// names things that may not have one yet — and the registry is what carries
// the ids. Intersecting them offers exactly what validatePinnedRepos accepts:
// offering the registry alone would include sibling teams' repos (an org admin
// reads the whole union), and every one of them would 400 on submit.
//
// Chosen repos render up top; the popover below holds the remaining
// tracked options + a search filter. Empty tracked list shows a hint
// pointing at /repos rather than an awkward empty popover.
export default function RepoMultiSelect({ value, onChange, teamId, disabled = false }: Props) {
  // "default" is the alias ResolveTeamID maps to the sole team in local
  // mode (the only mode whose callers leave teamId empty); a real team id
  // is used verbatim once the picker supplies one.
  const teamReposPath = `/api/settings/team/${teamId || 'default'}/repos`
  const [available, setAvailable] = useState<RepoOption[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const orgHref = useOrgHref()

  // Track whether the load actually succeeded vs. just returned
  // an empty list. Without this distinction, a network failure
  // (which leaves available=[]) renders the "No repos configured"
  // hint, telling users to go set up something they may already
  // have configured.
  const loadRepos = useCallback(
    async (signal: AbortSignal) => {
      try {
        setError(null)
        const [team, registry] = await Promise.all([
          apiJSON<{ repos?: string[] }>(teamReposPath, { signal }),
          apiListAll<RepoOption>('/api/repos/list', {}, { signal }),
        ])
        if (signal.aborted) return
        // Fold both sides: GitHub names are case-insensitive and the two
        // tables can hold different casings of one repository.
        const tracked = new Set((team.repos ?? []).map((slug) => slug.toLowerCase()))
        setAvailable(
          registry
            .filter((r) => tracked.has(r.slug.toLowerCase()))
            .sort((a, b) => a.slug.localeCompare(b.slug)),
        )
      } catch (err) {
        if (signal.aborted) return
        const msg = httpErrorMessage(err, 'Could not load the tracked repos.')
        setError(msg)
        toast.error(msg)
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    },
    [teamReposPath],
  )

  useEffect(() => {
    // Wait until the acting team resolves — fetching now would load the
    // default team's repos and offer them under the wrong team.
    if (disabled) return
    // Refetch when the acting team changes (the picker switched teams):
    // the offered repo set is the new team's tracked repos.
    setLoading(true)
    const controller = new AbortController()
    loadRepos(controller.signal)
    return () => controller.abort()
  }, [loadRepos, disabled])

  const selected = useMemo(() => new Set(value), [value])
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return available
    return available.filter((r) => r.slug.toLowerCase().includes(q))
  }, [available, search])

  // Chips read their name out of the offered set. An id with no entry is a
  // repo the team stopped tracking since it was pinned: it still shows, so
  // the user can see and remove it, rendered as the id because that is the
  // only thing about it this picker knows.
  const nameByID = useMemo(() => {
    const m = new Map<string, string>()
    for (const r of available) m.set(r.id, r.slug)
    return m
  }, [available])
  // Sorted by what the user reads, not by the opaque id.
  const chosen = useMemo(
    () =>
      value
        .map((id) => ({ id, slug: nameByID.get(id) ?? id }))
        .sort((a, b) => a.slug.localeCompare(b.slug)),
    [value, nameByID],
  )

  const toggle = useCallback(
    (id: string) => {
      const next = new Set(value)
      if (next.has(id)) {
        next.delete(id)
      } else {
        next.add(id)
      }
      onChange(Array.from(next))
    },
    [value, onChange],
  )

  // Waiting on the acting team — shown before the team resolves so the
  // user doesn't pick repos that won't match the team they submit under.
  if (disabled) {
    return (
      <div className="text-[12px] text-text-tertiary py-2">Select a team to choose its repos.</div>
    )
  }

  if (loading) {
    return <div className="text-[12px] text-text-tertiary py-2">Loading repos…</div>
  }

  // Distinct from "no repos configured" — a transient failure to
  // load shouldn't redirect the user to a setup screen they may
  // have already completed.
  if (error) {
    return (
      <div className="text-[12px] text-text-tertiary py-2">
        Couldn&rsquo;t load configured repos.{' '}
        <button
          type="button"
          onClick={() => {
            setLoading(true)
            const controller = new AbortController()
            loadRepos(controller.signal)
          }}
          className="text-accent hover:underline"
        >
          Try again
        </button>
        .
      </div>
    )
  }

  if (available.length === 0) {
    return (
      <div className="text-[12px] text-text-tertiary py-2">
        No repos configured.{' '}
        <Link to={orgHref('/repos')} className="text-accent hover:underline">
          Add repos
        </Link>{' '}
        first.
      </div>
    )
  }

  return (
    <div>
      {value.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-2">
          {chosen.map((r) => (
            <button
              key={r.id}
              type="button"
              onClick={() => toggle(r.id)}
              className="
                inline-flex items-center gap-1 rounded-full
                bg-accent-soft text-accent px-2.5 py-0.5 text-[11px]
                hover:bg-accent hover:text-white transition-colors
                group
              "
            >
              {r.slug}
              <X size={10} className="opacity-60 group-hover:opacity-100" />
            </button>
          ))}
        </div>
      )}
      <input
        type="text"
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search configured repos…"
        className="
          w-full rounded-lg border border-border-subtle
          bg-white/60 px-3 py-1.5 text-[12px] text-text-primary
          placeholder:text-text-tertiary mb-1.5
          focus:outline-none focus:border-accent focus:bg-white
        "
      />
      <div className="max-h-40 overflow-y-auto rounded-lg border border-border-subtle bg-white/40">
        {filtered.length === 0 ? (
          <div className="text-[12px] text-text-tertiary py-2 px-3">No matches.</div>
        ) : (
          filtered.map((r) => {
            const isSelected = selected.has(r.id)
            return (
              <button
                key={r.id}
                type="button"
                onClick={() => toggle(r.id)}
                className="
                  w-full flex items-center justify-between gap-2
                  px-3 py-1.5 text-[12px] text-left
                  hover:bg-black/[0.03] transition-colors
                "
              >
                <span className="text-text-primary truncate">{r.slug}</span>
                {isSelected && <Check size={12} className="shrink-0 text-accent" />}
              </button>
            )
          })
        )}
      </div>
    </div>
  )
}
