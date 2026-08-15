import { useState, useEffect, useMemo, useCallback } from 'react'
import { Link } from 'react-router'
import { Check, X } from 'lucide-react'
import { readError } from '../lib/api'
import { useOrgHref } from '../hooks/useOrgHref'
import { toast } from './Toast/toastStore'

interface Props {
  value: string[]
  onChange: (next: string[]) => void
  // The team whose tracked repos the picker offers. Empty falls back to
  // the org's default team (the "default" alias the repos endpoint
  // resolves) — the solo case where no write-time picker renders. When
  // the create modal's TeamPicker is shown (≥2 teams), it threads the
  // chosen team here so the offered repos match the team the project is
  // created under, and server-side validatePinnedRepos agrees.
  teamId?: string
  // When true the picker waits — no fetch, no interaction — until the
  // acting team resolves. The create modal sets this so a multi-team user
  // can't pick the *default* team's repos in the cold-load window before
  // the real acting team is known; those repos would 400 on submit against
  // the resolved team.
  disabled?: boolean
}

// RepoMultiSelect is the project create modal's pinned-repos picker. It
// reads the acting team's tracked repos and exposes those slugs as
// toggleable chips. Mirroring the server-side validation contract: the
// user can only pick from the team's tracked set, so the chip strip
// already enforces exactly what validatePinnedRepos enforces server-side.
// Sourcing from the org-wide union (GET /api/repos) instead would offer
// sibling-team repos the team doesn't track, so submitting would 400.
//
// Chosen slugs render up top; the popover below holds the remaining
// tracked options + a search filter. Empty tracked list shows a hint
// pointing at /repos rather than an awkward empty popover.
export default function RepoMultiSelect({ value, onChange, teamId, disabled = false }: Props) {
  // "default" is the alias resolveTeamID maps to the org's default team;
  // a real team id is used verbatim once the picker supplies one.
  const teamReposPath = `/api/settings/team/${teamId || 'default'}/repos`
  const [available, setAvailable] = useState<string[]>([])
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
        const res = await fetch(teamReposPath, { signal })
        if (signal.aborted) return
        if (!res.ok) {
          const msg = await readError(res, 'Failed to load repos')
          setError(msg)
          toast.error(msg)
          return
        }
        const data: { repos?: string[] } = await res.json()
        if (signal.aborted) return
        setAvailable(data.repos ?? [])
      } catch (err) {
        if (signal.aborted) return
        const msg = `Failed to load repos: ${err instanceof Error ? err.message : String(err)}`
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
    return available.filter((slug) => slug.toLowerCase().includes(q))
  }, [available, search])

  const toggle = useCallback(
    (slug: string) => {
      const next = new Set(value)
      if (next.has(slug)) {
        next.delete(slug)
      } else {
        next.add(slug)
      }
      onChange(Array.from(next).sort())
    },
    [value, onChange],
  )

  // Waiting on the acting team — shown before the team resolves so the
  // user doesn't pick repos that won't match the team they submit under.
  if (disabled) {
    return <div className="text-ui text-ink-3 py-2">Select a team to choose its repos.</div>
  }

  if (loading) {
    return <div className="text-ui text-ink-3 py-2">Loading repos…</div>
  }

  // Distinct from "no repos configured" — a transient failure to
  // load shouldn't redirect the user to a setup screen they may
  // have already completed.
  if (error) {
    return (
      <div className="text-ui text-ink-3 py-2">
        Couldn&rsquo;t load configured repos.{' '}
        <button
          type="button"
          onClick={() => {
            setLoading(true)
            const controller = new AbortController()
            loadRepos(controller.signal)
          }}
          className="text-warm hover:underline"
        >
          Try again
        </button>
        .
      </div>
    )
  }

  if (available.length === 0) {
    return (
      <div className="text-ui text-ink-3 py-2">
        No repos configured.{' '}
        <Link to={orgHref('/repos')} className="text-warm hover:underline">
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
          {value.map((slug) => (
            <button
              key={slug}
              type="button"
              onClick={() => toggle(slug)}
              className="
                inline-flex items-center gap-1 rounded-full
                bg-warm-2 text-warm px-2.5 py-0.5 text-reported
                hover:bg-warm hover:text-warm-ink transition-colors
                group
              "
            >
              {slug}
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
          w-full rounded-lg border border-line-1
          bg-raised px-3 py-1.5 text-ui text-ink-1
          placeholder:text-ink-3 mb-1.5
          focus:outline-none focus:border-warm focus:bg-raised
        "
      />
      <div className="max-h-40 overflow-y-auto rounded-lg border border-line-1 bg-raised">
        {filtered.length === 0 ? (
          <div className="text-ui text-ink-3 py-2 px-3">No matches.</div>
        ) : (
          filtered.map((slug) => {
            const isSelected = selected.has(slug)
            return (
              <button
                key={slug}
                type="button"
                onClick={() => toggle(slug)}
                className="
                  w-full flex items-center justify-between gap-2
                  px-3 py-1.5 text-ui text-left
                  hover:bg-tint-2 transition-colors
                "
              >
                <span className="text-ink-1 truncate">{slug}</span>
                {isSelected && <Check size={12} className="shrink-0 text-warm" />}
              </button>
            )
          })
        )}
      </div>
    </div>
  )
}
