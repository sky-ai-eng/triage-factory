import { useState, useEffect, useMemo, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { Check, X } from 'lucide-react'
import { readError } from '../lib/api'
import { useOrgHref } from '../hooks/useOrgHref'
import { toast } from './Toast/toastStore'

interface Props {
  value: string[]
  onChange: (next: string[]) => void
}

// The default team's tracked repos — the create modal builds projects
// under the default team, and pinned_repos is validated server-side
// against that team's team_github_repos. Sourcing the picker from the
// org-wide union (GET /api/repos) instead would offer sibling-team repos
// the default team doesn't track, so submitting would 400.
//
// TODO(SKY-294): "default" is hardcoded because there is no team
// creation/selection UX yet — every project is created under the org's
// single default team, so this is correct *today*. When the write-time
// team picker lands, thread the acting team in as a prop here (and have
// the create handler stamp it) instead of assuming default. One of
// several call sites that hardcode the default team; the server-side
// create handlers carry the matching fallback.
const TEAM_REPOS_PATH = '/api/settings/team/default/repos'

// RepoMultiSelect is the project create modal's pinned-repos picker. It
// reads the default team's tracked repos and exposes those slugs as
// toggleable chips. Mirroring the server-side validation contract: the
// user can only pick from the team's tracked set, so the chip strip
// already enforces exactly what validatePinnedRepos enforces server-side.
//
// Chosen slugs render up top; the popover below holds the remaining
// tracked options + a search filter. Empty tracked list shows a hint
// pointing at /repos rather than an awkward empty popover.
export default function RepoMultiSelect({ value, onChange }: Props) {
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
  const loadRepos = useCallback(async (signal: AbortSignal) => {
    try {
      setError(null)
      const res = await fetch(TEAM_REPOS_PATH, { signal })
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
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    loadRepos(controller.signal)
    return () => controller.abort()
  }, [loadRepos])

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
          {value.map((slug) => (
            <button
              key={slug}
              type="button"
              onClick={() => toggle(slug)}
              className="
                inline-flex items-center gap-1 rounded-full
                bg-accent-soft text-accent px-2.5 py-0.5 text-[11px]
                hover:bg-accent hover:text-white transition-colors
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
          filtered.map((slug) => {
            const isSelected = selected.has(slug)
            return (
              <button
                key={slug}
                type="button"
                onClick={() => toggle(slug)}
                className="
                  w-full flex items-center justify-between gap-2
                  px-3 py-1.5 text-[12px] text-left
                  hover:bg-black/[0.03] transition-colors
                "
              >
                <span className="text-text-primary truncate">{slug}</span>
                {isSelected && <Check size={12} className="shrink-0 text-accent" />}
              </button>
            )
          })
        )}
      </div>
    </div>
  )
}
