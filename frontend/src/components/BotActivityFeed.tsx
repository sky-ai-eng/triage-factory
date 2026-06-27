import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ActivityArtifact, ArtifactKind } from '../types'
import { readError } from '../lib/api'
import { ArtifactRow } from './ArtifactRow'
import {
  ARTIFACT_KINDS,
  ARTIFACT_PROVIDERS,
  ARTIFACT_STATES,
  KIND_META,
  PROVIDER_LABEL,
} from './artifactMeta'

// BotActivityFeed is the EE bot-activity audit feed (TFAC-483): a time-ordered,
// filterable history of every external action the org's bot took with org
// credentials — branches, PRs, reviews, comments, issues — newest first,
// terminal rows included. It is a LOG, not a "what's open" worklist (that's the
// board / PRs page). It backs two Usage-page bands, both behind FeatureGovernance
// and rendered only by the caller once entitlement + role gate pass:
//
//   - team: baseUrl=/api/usage/teams/{id}/activity (team admin or org admin).
//   - org : baseUrl=/api/usage/org/activity, showTeam (org admin) — rows carry
//     the owning team, and a client-side team-scope filter narrows the view.
//
// Rows reuse ArtifactRow (link-out only, no approval overlay). provider/kind/
// state/time filters drive server-side query params + limit/offset paging
// ("load more"); the org team-scope filter is in-memory over the loaded page
// (the org endpoint is org-wide by design and takes no team param). Filter state
// is in-memory (v1) — not persisted like the board's per-user localStorage.

const PAGE_SIZE = 50

type RangeKey = 'all' | '7d' | '30d' | '90d'

const RANGE_TABS: { key: RangeKey; label: string }[] = [
  { key: 'all', label: 'ALL' },
  { key: '7d', label: '7D' },
  { key: '30d', label: '30D' },
  { key: '90d', label: '90D' },
]

// sinceFor maps a range preset to the `since` query value (YYYY-MM-DD, UTC).
// 'all' sends nothing — the activity endpoints default to the full history (no
// month window, unlike the spend reads).
function sinceFor(range: RangeKey): string {
  if (range === 'all') return ''
  const days = range === '7d' ? 7 : range === '30d' ? 30 : 90
  return new Date(Date.now() - days * 86_400_000).toISOString().slice(0, 10)
}

interface Filters {
  provider: string
  kind: string
  state: string
  range: RangeKey
}

const EMPTY_FILTERS: Filters = { provider: 'all', kind: 'all', state: 'all', range: 'all' }

// buildUrl composes the activity request: the server-side filters (provider /
// kind / state / since) + limit/offset paging. 'all' / '' selections are omitted
// so the backend leaves that column unfiltered.
function buildUrl(base: string, f: Filters, offset: number): string {
  const q = new URLSearchParams()
  if (f.provider !== 'all') q.set('provider', f.provider)
  if (f.kind !== 'all') q.set('kind', f.kind)
  if (f.state !== 'all') q.set('state', f.state)
  const since = sinceFor(f.range)
  if (since) q.set('since', since)
  q.set('limit', String(PAGE_SIZE))
  if (offset > 0) q.set('offset', String(offset))
  return `${base}?${q.toString()}`
}

export default function BotActivityFeed({
  baseUrl,
  showTeam = false,
}: {
  baseUrl: string
  /** Org feed: show the owning team per row + a client-side team-scope filter. */
  showTeam?: boolean
}) {
  const [filters, setFilters] = useState<Filters>(EMPTY_FILTERS)
  const [teamFilter, setTeamFilter] = useState('') // org feed only; '' = all teams
  const [rows, setRows] = useState<ActivityArtifact[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [hasMore, setHasMore] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)

  const setFilter = useCallback(<K extends keyof Filters>(key: K, value: Filters[K]) => {
    setFilters((prev) => ({ ...prev, [key]: value }))
  }, [])

  // Page 0 (replace) on mount and whenever the server-side filters or the base
  // url (team switch) change. teamFilter is in-memory, so it's deliberately NOT
  // a dependency — narrowing by team never refetches.
  useEffect(() => {
    let cancelled = false
    setRows(null)
    setError(null)
    setHasMore(false)
    ;(async () => {
      try {
        const res = await fetch(buildUrl(baseUrl, filters, 0))
        if (!res.ok) throw new Error(await readError(res, "Couldn't load bot activity"))
        const data = ((await res.json()) as ActivityArtifact[] | null) ?? []
        if (!cancelled) {
          setRows(data)
          setHasMore(data.length === PAGE_SIZE)
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [baseUrl, filters])

  const loadMore = useCallback(async () => {
    if (rows === null || loadingMore) return
    setLoadingMore(true)
    try {
      // Offset is the count actually fetched from the server (rows accumulates
      // server pages; the team filter only narrows the render), so paging stays
      // aligned with the server's newest-first cursor.
      const res = await fetch(buildUrl(baseUrl, filters, rows.length))
      if (!res.ok) throw new Error(await readError(res, "Couldn't load bot activity"))
      const data = ((await res.json()) as ActivityArtifact[] | null) ?? []
      setRows((prev) => [...(prev ?? []), ...data])
      setHasMore(data.length === PAGE_SIZE)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoadingMore(false)
    }
  }, [baseUrl, filters, rows, loadingMore])

  // Distinct teams present in the loaded rows — the org feed's team-scope
  // options. In-memory because the org endpoint is org-wide (no team param).
  const teamOptions = useMemo(() => {
    if (!showTeam || rows === null) return []
    const seen = new Map<string, string>()
    for (const r of rows) {
      if (r.team_id) seen.set(r.team_id, r.team_name || r.team_id)
    }
    return [...seen.entries()]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [showTeam, rows])

  const visible = useMemo(() => {
    if (rows === null) return null
    if (!showTeam || !teamFilter) return rows
    return rows.filter((r) => r.team_id === teamFilter)
  }, [rows, showTeam, teamFilter])

  const count = visible?.length ?? 0

  return (
    <section className="mb-12 last:mb-0">
      {/* Etched header — mono label + hairline + the loaded-row count, matching
          the spend bands' chrome but counting actions, not dollars. */}
      <div className="flex items-end gap-4">
        <div className="flex items-baseline gap-2.5 pb-1.5">
          <span className="font-mono text-[12px] font-semibold uppercase tracking-[0.2em] text-text-secondary">
            Bot activity
          </span>
          <span className="font-mono text-[10px] tracking-[0.06em] text-text-tertiary/70">
            external actions · org credentials
          </span>
        </div>
        <span className="mb-[9px] h-px flex-1 bg-border-subtle" />
        <div className="pb-0.5 text-right leading-none">
          <span className="font-mono text-sm tabular-nums text-text-tertiary">
            {visible === null
              ? '—'
              : `${count}${hasMore ? '+' : ''} action${count === 1 ? '' : 's'}`}
          </span>
        </div>
      </div>

      {/* Filter bar — provider / kind / state selects, the time-range tabs, and
          (org feed) the team scope. In-memory filter state, v1. */}
      <div className="mt-5 flex flex-wrap items-center gap-x-5 gap-y-2.5">
        <FeedSelect
          label="provider"
          value={filters.provider}
          onChange={(v) => setFilter('provider', v)}
          options={[
            { value: 'all', label: 'All' },
            ...ARTIFACT_PROVIDERS.map((p) => ({ value: p, label: PROVIDER_LABEL[p] ?? p })),
          ]}
        />
        <FeedSelect
          label="kind"
          value={filters.kind}
          onChange={(v) => setFilter('kind', v)}
          options={[
            { value: 'all', label: 'All' },
            ...ARTIFACT_KINDS.map((k: ArtifactKind) => ({ value: k, label: KIND_META[k].label })),
          ]}
        />
        <FeedSelect
          label="state"
          value={filters.state}
          onChange={(v) => setFilter('state', v)}
          options={[
            { value: 'all', label: 'All' },
            ...ARTIFACT_STATES.map((s) => ({ value: s, label: s })),
          ]}
        />
        {showTeam && teamOptions.length > 0 && (
          <FeedSelect
            label="team"
            value={teamFilter}
            onChange={setTeamFilter}
            options={[
              { value: '', label: 'All teams' },
              ...teamOptions.map((t) => ({ value: t.id, label: t.name })),
            ]}
          />
        )}
        <span className="ml-auto">
          <RangeTabs value={filters.range} onChange={(r) => setFilter('range', r)} />
        </span>
      </div>

      {/* Body — skeleton on first load, error, zero state, or the rows. */}
      <div className="mt-5">
        {visible === null ? (
          error ? (
            <FeedNote msg={error} tone="error" />
          ) : (
            <FeedSkeleton />
          )
        ) : count === 0 ? (
          <FeedNote msg="no bot activity · this view" />
        ) : (
          <>
            <ul className="space-y-1.5">
              {visible.map((a) => (
                <ArtifactRow
                  key={a.id}
                  artifact={a}
                  note={showTeam && a.team_name ? <TeamChip name={a.team_name} /> : undefined}
                />
              ))}
            </ul>
            {/* "Load more" pages the server feed; the count above carries the '+' */}
            {hasMore && (
              <div className="mt-3 flex justify-center">
                <button
                  type="button"
                  onClick={loadMore}
                  disabled={loadingMore}
                  className="rounded-[4px] border border-border-subtle bg-white/60 px-3 py-1 font-mono text-[10px] uppercase tracking-[0.12em] text-text-tertiary transition-colors hover:bg-white hover:text-text-secondary disabled:opacity-50"
                >
                  {loadingMore ? 'loading…' : 'load more'}
                </button>
              </div>
            )}
            {/* A non-fatal paging error sits under the rows we already have. */}
            {error && <FeedNote msg={error} tone="error" />}
          </>
        )}
      </div>
    </section>
  )
}

// FeedSelect — one labeled dropdown in the filter bar, in the console's
// monospace language (Board's filter affordances + the Usage chrome).
function FeedSelect({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string }[]
}) {
  return (
    <label className="flex items-center gap-1.5">
      <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.14em] text-text-tertiary/70">
        {label}
      </span>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={label}
        className="rounded-[4px] border border-border-subtle bg-white/60 px-2 py-1 font-mono text-[11px] text-text-secondary transition-colors hover:bg-white focus:outline-none focus:ring-1 focus:ring-accent/40"
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  )
}

// RangeTabs — the time-range selector, styled as the Usage Channel tabs (bare
// monospace labels with a rust underline-tick on the active one).
function RangeTabs({ value, onChange }: { value: RangeKey; onChange: (r: RangeKey) => void }) {
  return (
    <div className="flex items-center gap-3">
      {RANGE_TABS.map((t) => {
        const active = t.key === value
        return (
          <button
            key={t.key}
            type="button"
            onClick={() => onChange(t.key)}
            className={`relative font-mono text-[10px] tracking-[0.12em] transition-colors ${
              active ? 'text-accent' : 'text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {t.label}
            {active && <span className="absolute -bottom-1 left-0 right-0 h-px bg-accent" />}
          </button>
        )
      })}
    </div>
  )
}

// TeamChip — the owning-team tag on an org-feed row (which team's bot acted).
function TeamChip({ name }: { name: string }) {
  return (
    <span
      className="max-w-[8rem] shrink-0 truncate rounded-[3px] bg-black/[0.04] px-1.5 py-0.5 font-mono text-[9px] text-text-tertiary"
      title={name}
    >
      {name}
    </span>
  )
}

function FeedSkeleton() {
  return (
    <ul className="space-y-1.5">
      {[0, 1, 2, 3].map((i) => (
        <li
          key={i}
          className="h-8 animate-pulse rounded-[4px] border border-border-subtle bg-black/[0.03]"
        />
      ))}
    </ul>
  )
}

// FeedNote — the zero / error state, a single etched monospace line with
// hairline rules (mirrors the Usage page's EtchedNote so the feed reads as part
// of the same console).
function FeedNote({ msg, tone = 'muted' }: { msg: string; tone?: 'muted' | 'error' }) {
  const isErr = tone === 'error'
  return (
    <div
      className={`flex items-center gap-3 py-4 font-mono text-[10px] ${
        isErr
          ? 'tracking-[0.04em] text-dismiss'
          : 'uppercase tracking-[0.16em] text-text-tertiary/60'
      }`}
    >
      <span className={`h-px w-6 ${isErr ? 'bg-dismiss/40' : 'bg-border-subtle'}`} />
      {msg}
      <span className={`h-px flex-1 ${isErr ? 'bg-dismiss/20' : 'bg-border-subtle'}`} />
    </div>
  )
}
