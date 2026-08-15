import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ActivityAction, ActivityArtifact, ArtifactKind } from '../types'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'
import { ArtifactRow } from './ArtifactRow'
import { ActionRow } from './ActionRow'
import {
  ARTIFACT_KINDS,
  ARTIFACT_PROVIDERS,
  ARTIFACT_STATES,
  KIND_META,
  PROVIDER_LABEL,
} from './artifactMeta'
import { ACTION_OPTIONS, ACTION_PROVIDERS } from './actionMeta'

// ActivityFeed is the EE /usage governance surface (TFAC-483), with TWO lenses on
// the org's external footprint — toggled in the header, both behind
// FeatureGovernance and rendered only once the caller's entitlement + role gate
// pass:
//
//   - Actions (default) → the append-only external-action audit log of record:
//     one row per external WRITE TF performed under an org credential (the bot's
//     GitHub/Jira mutations, the human-authorized approval lifecycle, the Jira
//     board mirror). Rendered by ActionRow. The source of truth for "what TF did."
//   - Objects → the artifacts head: each object the bot produced and its CURRENT
//     state, reconciler-maintained (includes externally-driven transitions a
//     human made, which are NOT TF actions). Rendered by ArtifactRow. The PR #505
//     feed, kept.
//
// Two scopes, set by the caller's baseUrl:
//
//   - team: /api/usage/teams/{id}/activity (team admin or org admin).
//   - org : /api/usage/org/activity, showTeam (org admin) — rows carry the owning
//     team (+ the authorizing actor on the Actions lens), with a client-side
//     team-scope (and, on Actions, actor-scope) filter narrowing the loaded page.
//
// The lens is sent as ?view=actions|objects; the server filters + pages. Filters
// adapt per lens (Actions: provider/action/time; Objects: provider/kind/state/
// time). Filter state is in-memory (v1).

const PAGE_SIZE = 50

type Mode = 'actions' | 'objects'

const MODE_TABS: { key: Mode; label: string }[] = [
  { key: 'actions', label: 'ACTIONS' },
  { key: 'objects', label: 'OBJECTS' },
]

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
  range: RangeKey
  // Objects lens:
  kind: string
  state: string
  // Actions lens:
  action: string
}

const EMPTY_FILTERS: Filters = {
  provider: 'all',
  range: 'all',
  kind: 'all',
  state: 'all',
  action: 'all',
}

type FeedRow = ActivityArtifact | ActivityAction

// Loaded is one settled server page-set, tagged with the request that produced
// it. Keeping the lens + url ALONGSIDE the rows is what lets the feed hold its
// last good content while the next request is in flight (stale-while-revalidate,
// same stance as the Usage page's useUsageFetch): `mode` says which row
// component can render these rows, and `url` says whether they still answer the
// current lens + filters. Without the tag we'd have to blank the list on every
// toggle, which collapses the section to a 4-row skeleton and back.
interface Loaded {
  mode: Mode
  url: string
  rows: FeedRow[]
  hasMore: boolean
}

// buildUrl composes the activity request: the lens (?view=), the server-side
// filters for that lens, and limit/offset paging. 'all' / '' selections are
// omitted so the backend leaves that column unfiltered.
function buildUrl(base: string, mode: Mode, f: Filters, offset: number): string {
  const q = new URLSearchParams()
  q.set('view', mode)
  if (f.provider !== 'all') q.set('provider', f.provider)
  if (mode === 'objects') {
    if (f.kind !== 'all') q.set('kind', f.kind)
    if (f.state !== 'all') q.set('state', f.state)
  } else {
    if (f.action !== 'all') q.set('action', f.action)
  }
  const since = sinceFor(f.range)
  if (since) q.set('since', since)
  q.set('limit', String(PAGE_SIZE))
  if (offset > 0) q.set('offset', String(offset))
  return `${base}?${q.toString()}`
}

export default function ActivityFeed({
  baseUrl,
  showTeam = false,
}: {
  baseUrl: string
  /** Org feed: show the owning team (+ authorizing actor) per row + client-side scope filters. */
  showTeam?: boolean
}) {
  const [mode, setMode] = useState<Mode>('actions')
  const [filters, setFilters] = useState<Filters>(EMPTY_FILTERS)
  const [teamFilter, setTeamFilter] = useState('') // org feed only; '' = all teams
  const [actorFilter, setActorFilter] = useState('') // org + actions only; '' = all actors
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)

  const setFilter = useCallback(<K extends keyof Filters>(key: K, value: Filters[K]) => {
    setFilters((prev) => ({ ...prev, [key]: value }))
  }, [])

  // Switching lens is a clean slate: the filter dimensions differ per lens, so
  // reset filters + the in-memory scopes rather than carry a stale kind=/action=.
  const switchMode = useCallback((next: Mode) => {
    setMode(next)
    setFilters(EMPTY_FILTERS)
    setTeamFilter('')
    setActorFilter('')
  }, [])

  // The page-0 request for the current lens + filters. It doubles as the freshness
  // key: `loaded.url !== pageURL` means what's on screen answers a superseded
  // request. (loadMore appends later pages without changing it.)
  const pageURL = buildUrl(baseUrl, mode, filters, 0)

  // Page 0 (replace) on mount and whenever the lens, server-side filters, or the
  // base url (team switch) change. The in-memory team/actor scopes are
  // deliberately NOT dependencies — narrowing never refetches.
  //
  // Deliberately does NOT blank `loaded` first: the last good page stays on
  // screen (dimmed) until the new one lands, so toggling the lens or a filter
  // never collapses the section to a skeleton and springs back. `loaded.mode`
  // picks the row component, so holding rows across a lens switch is safe —
  // action rows keep rendering as action rows until the objects page arrives.
  useEffect(() => {
    let cancelled = false
    setError(null)
    ;(async () => {
      try {
        const data = (await apiJSON<FeedRow[] | null>(pageURL)) ?? []
        if (!cancelled) {
          setLoaded({ mode, url: pageURL, rows: data, hasMore: data.length === PAGE_SIZE })
        }
      } catch (err) {
        if (!cancelled) setError(httpErrorMessage(err, "Couldn't load activity."))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [pageURL, mode])

  // `superseded` = the rows on screen answer an older request than the current
  // lens + filters, so paging against them would splice two views together.
  // `stale` is the VISUAL hold that dims them; it clears on error so a failed
  // refetch doesn't leave the list dimmed forever — the error note renders under
  // the rows we still have instead.
  const superseded = loaded !== null && loaded.url !== pageURL
  const stale = superseded && error === null

  const loadMore = useCallback(async () => {
    if (loaded === null || loaded.url !== pageURL || loadingMore) return
    setLoadingMore(true)
    try {
      // Offset is the count actually fetched from the server (loaded.rows
      // accumulates server pages; the in-memory filters only narrow the render),
      // so paging stays aligned with the server's newest-first cursor.
      const data =
        (await apiJSON<FeedRow[] | null>(buildUrl(baseUrl, mode, filters, loaded.rows.length))) ??
        []
      // Drop the page if the lens/filters moved while it was in flight — appending
      // it would splice another request's rows into the current view.
      setLoaded((prev) =>
        prev && prev.url === pageURL
          ? { ...prev, rows: [...prev.rows, ...data], hasMore: data.length === PAGE_SIZE }
          : prev,
      )
    } catch (err) {
      setError(httpErrorMessage(err, "Couldn't load activity."))
    } finally {
      setLoadingMore(false)
    }
  }, [baseUrl, mode, filters, pageURL, loaded, loadingMore])

  // Distinct teams present in the loaded rows — the org feed's team-scope
  // options. In-memory because the org endpoint is org-wide (no team param).
  const teamOptions = useMemo(() => {
    if (!showTeam || loaded === null) return []
    const seen = new Map<string, string>()
    for (const r of loaded.rows) {
      if (r.team_id) seen.set(r.team_id, r.team_name || r.team_id)
    }
    return [...seen.entries()]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [showTeam, loaded])

  // Distinct human actors present in the loaded action rows — the org + Actions
  // feed's actor-scope options (mirrors the team scope). Empty on Objects (no
  // actor) or the team feed (names unresolved there).
  const actorOptions = useMemo(() => {
    if (!showTeam || loaded === null || loaded.mode !== 'actions') return []
    const seen = new Map<string, string>()
    for (const r of loaded.rows as ActivityAction[]) {
      if (r.actor_user_id) seen.set(r.actor_user_id, r.actor_name || r.actor_user_id)
    }
    return [...seen.entries()]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [showTeam, loaded])

  const visible = useMemo(() => {
    if (loaded === null) return null
    let out = loaded.rows
    if (showTeam && teamFilter) out = out.filter((r) => r.team_id === teamFilter)
    if (showTeam && loaded.mode === 'actions' && actorFilter) {
      out = out.filter((r) => (r as ActivityAction).actor_user_id === actorFilter)
    }
    return out
  }, [loaded, showTeam, teamFilter, actorFilter])

  const count = visible?.length ?? 0
  // The noun follows the rows actually on screen, not the selected lens — during
  // a hold the header would otherwise count "objects" over a list of actions.
  const rowMode = loaded?.mode ?? mode
  const noun = rowMode === 'actions' ? 'action' : 'object'
  const hasMore = loaded?.hasMore ?? false

  return (
    <section className="mb-12 last:mb-0">
      {/* Etched header — mono label + the lens toggle + hairline + the loaded-row
          count, matching the spend bands' chrome but counting actions/objects. */}
      <div className="flex items-end gap-4">
        <div className="flex items-baseline gap-3 pb-1.5">
          <span className="font-mono text-[12px] font-semibold uppercase tracking-[0.2em] text-text-secondary">
            Activity
          </span>
          <ModeTabs value={mode} onChange={switchMode} />
        </div>
        <span className="mb-[9px] h-px flex-1 bg-border-subtle" />
        <div className="pb-0.5 text-right leading-none">
          <span className="font-mono text-sm tabular-nums text-text-tertiary">
            {visible === null
              ? '—'
              : `${count}${hasMore ? '+' : ''} ${noun}${count === 1 ? '' : 's'}`}
          </span>
        </div>
      </div>

      {/* Sub-caption — what this lens is, so the coverage difference reads as
          intentional, not a gap. */}
      <div className="mt-1 font-mono text-[10px] tracking-[0.06em] text-text-tertiary/70">
        {mode === 'actions'
          ? 'audit log of every org-credential action TF took'
          : 'objects the bot produced + their current state'}
      </div>

      {/* Filter bar — adapts per lens. Shared: provider + the time-range tabs.
          Objects: kind/state. Actions: action (+ org actor scope). */}
      <div className="mt-5 flex flex-wrap items-center gap-x-5 gap-y-2.5">
        <FeedSelect
          label="provider"
          value={filters.provider}
          onChange={(v) => setFilter('provider', v)}
          options={[
            { value: 'all', label: 'All' },
            ...(mode === 'actions' ? ACTION_PROVIDERS : ARTIFACT_PROVIDERS).map((p) => ({
              value: p,
              label: PROVIDER_LABEL[p] ?? p,
            })),
          ]}
        />
        {mode === 'objects' ? (
          <>
            <FeedSelect
              label="kind"
              value={filters.kind}
              onChange={(v) => setFilter('kind', v)}
              options={[
                { value: 'all', label: 'All' },
                ...ARTIFACT_KINDS.map((k: ArtifactKind) => ({
                  value: k,
                  label: KIND_META[k].label,
                })),
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
          </>
        ) : (
          <FeedSelect
            label="action"
            value={filters.action}
            onChange={(v) => setFilter('action', v)}
            options={[{ value: 'all', label: 'All' }, ...ACTION_OPTIONS]}
          />
        )}
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
        {showTeam && mode === 'actions' && actorOptions.length > 0 && (
          <FeedSelect
            label="actor"
            value={actorFilter}
            onChange={setActorFilter}
            options={[
              { value: '', label: 'All actors' },
              ...actorOptions.map((a) => ({ value: a.id, label: a.name })),
            ]}
          />
        )}
        <span className="ml-auto">
          <RangeTabs value={filters.range} onChange={(r) => setFilter('range', r)} />
        </span>
      </div>

      {/* Body — skeleton on the FIRST load only; every later request keeps the
          previous rows in place (dimmed) so the section holds its height. */}
      <div
        className={`mt-5 transition-opacity duration-150 ${stale ? 'opacity-50' : ''}`}
        aria-busy={stale || undefined}
      >
        {visible === null ? (
          error ? (
            <FeedNote msg={error} tone="error" />
          ) : (
            <FeedSkeleton />
          )
        ) : count === 0 ? (
          <FeedNote msg={`no ${noun}s · this view`} />
        ) : (
          <>
            <ul className="space-y-1.5">
              {visible.map((r) =>
                rowMode === 'actions' ? (
                  <ActionRow
                    key={r.id}
                    action={r as ActivityAction}
                    note={showTeam && r.team_name ? <TeamChip name={r.team_name} /> : undefined}
                  />
                ) : (
                  <ArtifactRow
                    key={r.id}
                    artifact={r as ActivityArtifact}
                    note={showTeam && r.team_name ? <TeamChip name={r.team_name} /> : undefined}
                  />
                ),
              )}
            </ul>
            {/* "Load more" pages the server feed; the count above carries the '+' */}
            {hasMore && (
              <div className="mt-3 flex justify-center">
                <button
                  type="button"
                  onClick={loadMore}
                  disabled={loadingMore || superseded}
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

// ModeTabs — the Actions | Objects lens toggle, styled as the range tabs (bare
// monospace labels with a rust underline-tick on the active one).
function ModeTabs({ value, onChange }: { value: Mode; onChange: (m: Mode) => void }) {
  return (
    <div className="flex items-center gap-2.5" role="tablist" aria-label="activity lens">
      {MODE_TABS.map((t) => {
        const active = t.key === value
        return (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onChange(t.key)}
            className={`relative font-mono text-[10px] font-semibold tracking-[0.14em] transition-colors ${
              active ? 'text-accent' : 'text-text-tertiary/70 hover:text-text-secondary'
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

// TeamChip — the owning-team tag on an org-feed row (which team acted).
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
