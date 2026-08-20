import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ActivityAction, ActivityArtifact, ArtifactKind } from '../types'
import type { ListRequest } from '../lib/apiClient'
import { usePagedList } from '../hooks/usePagedList'
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
// Two scopes, set by the caller's basePath:
//
//   - team: /api/teams/{id}/usage (team admin or org admin).
//   - org : /api/orgs/{id}/usage, showTeam (org admin) — rows carry the owning team
//     (+ the authorizing actor on the Actions lens), with a client-side
//     team-scope (and, on Actions, actor-scope) filter narrowing the loaded page.
//
// The lens is an ADDRESS, not a parameter: `${basePath}/actions/list` and
// `${basePath}/artifacts/list` are two resources with different row shapes and
// different filter vocabularies. They used to be one route selected by ?view=,
// where an Objects-only `state=` filter silently did nothing on the Actions
// lens. Filters adapt per lens (Actions: provider/action/time; Objects:
// provider/kind/state/time). Filter state is in-memory (v1).

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

/** listPathFor resolves the lens to its own route. */
function listPathFor(basePath: string, mode: Mode): string {
  return `${basePath}/${mode === 'actions' ? 'actions' : 'artifacts'}/list`
}

/** filterBodyFor builds the lens's filter half of the list body. 'all' / ''
 *  selections are omitted so the backend leaves that column unfiltered — and
 *  each lens sends only ITS OWN vocabulary, since the routes decode strictly
 *  and a stray `state` on the actions list is now a 400 rather than a filter
 *  that silently does nothing. */
function filterBodyFor(mode: Mode, f: Filters): ListRequest {
  const body: ListRequest = {}
  if (f.provider !== 'all') body.provider = f.provider
  if (mode === 'objects') {
    if (f.kind !== 'all') body.kind = f.kind
    if (f.state !== 'all') body.state = f.state
  } else if (f.action !== 'all') {
    body.action = f.action
  }
  const since = sinceFor(f.range)
  if (since) body.since = since
  return body
}

export default function ActivityFeed({
  basePath,
  showTeam = false,
}: {
  /** The scope prefix — `/api/orgs/{id}/usage` or `/api/teams/{id}/usage`. The
   *  lens appends its own resource segment. */
  basePath: string
  /** Org feed: show the owning team (+ authorizing actor) per row + client-side scope filters. */
  showTeam?: boolean
}) {
  const [mode, setMode] = useState<Mode>('actions')
  const [filters, setFilters] = useState<Filters>(EMPTY_FILTERS)
  const [teamFilter, setTeamFilter] = useState('') // org feed only; '' = all teams
  const [actorFilter, setActorFilter] = useState('') // org + actions only; '' = all actors

  // The request the rows on screen answer. Held alongside the hook's items so
  // the feed can keep its last good content while the next request is in flight
  // (stale-while-revalidate, same stance as the Usage page's useUsageFetch):
  // `mode` says which row component can render these rows, and `key` says
  // whether they still answer the current lens + filters. Without it we'd have
  // to blank the list on every toggle, which collapses the section to a 4-row
  // skeleton and back.
  const [loaded, setLoaded] = useState<{ mode: Mode; key: string } | null>(null)

  const listPath = listPathFor(basePath, mode)
  const filterBody = filterBodyFor(mode, filters)
  // The freshness key for the current lens + filters. (loadMore appends later
  // pages without changing it.)
  const pageKey = `${listPath}|${JSON.stringify(filterBody)}`

  const feed = usePagedList<FeedRow>(listPath, "Couldn't load activity.")
  const { items, load, loadMore, hasMore, loading, error } = feed

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

  // Page 1 (replace) on mount and whenever the lens, server-side filters, or the
  // base path (team switch) change. The in-memory team/actor scopes are
  // deliberately NOT dependencies — narrowing never refetches.
  //
  // Deliberately does NOT blank the items first: the last good page stays on
  // screen (dimmed) until the new one lands, so toggling the lens or a filter
  // never collapses the section to a skeleton and springs back. `loaded.mode`
  // picks the row component, so holding rows across a lens switch is safe —
  // action rows keep rendering as action rows until the objects page arrives.
  //
  // pageKey is the only dependency: it is a string derived from the path and
  // the filters, so a re-render that rebuilds an equal filter object doesn't
  // refetch. `load` is stable per path, and mode/filters are what pageKey is
  // made of.
  useEffect(() => {
    let cancelled = false
    load({ ...filterBody, page_size: PAGE_SIZE }).then((page) => {
      if (page && !cancelled) setLoaded({ mode, key: pageKey })
    })
    return () => {
      cancelled = true
    }
  }, [pageKey]) // eslint-disable-line react-hooks/exhaustive-deps

  // `superseded` = the rows on screen answer an older request than the current
  // lens + filters, so paging against them would splice two views together —
  // and the hook's held page token belongs to that older request, which the
  // server would reject outright.
  // `stale` is the VISUAL hold that dims them; it clears on error so a failed
  // refetch doesn't leave the list dimmed forever — the error note renders under
  // the rows we still have instead.
  const superseded = loaded !== null && loaded.key !== pageKey
  const stale = superseded && error === ''

  // Distinct teams present in the loaded rows — the org feed's team-scope
  // options. In-memory because the org endpoint is org-wide (no team param).
  const teamOptions = useMemo(() => {
    if (!showTeam || loaded === null) return []
    const seen = new Map<string, string>()
    for (const r of items) {
      if (r.team_id) seen.set(r.team_id, r.team_name || r.team_id)
    }
    return [...seen.entries()]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [showTeam, loaded, items])

  // Distinct human actors present in the loaded action rows — the org + Actions
  // feed's actor-scope options (mirrors the team scope). Empty on Objects (no
  // actor) or the team feed (names unresolved there).
  const actorOptions = useMemo(() => {
    if (!showTeam || loaded === null || loaded.mode !== 'actions') return []
    const seen = new Map<string, string>()
    for (const r of items as ActivityAction[]) {
      if (r.actor_user_id) seen.set(r.actor_user_id, r.actor_name || r.actor_user_id)
    }
    return [...seen.entries()]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [showTeam, loaded, items])

  const visible = useMemo(() => {
    if (loaded === null) return null
    let out = items
    if (showTeam && teamFilter) out = out.filter((r) => r.team_id === teamFilter)
    if (showTeam && loaded.mode === 'actions' && actorFilter) {
      out = out.filter((r) => (r as ActivityAction).actor_user_id === actorFilter)
    }
    return out
  }, [loaded, items, showTeam, teamFilter, actorFilter])

  const count = visible?.length ?? 0
  // The noun follows the rows actually on screen, not the selected lens — during
  // a hold the header would otherwise count "objects" over a list of actions.
  const rowMode = loaded?.mode ?? mode
  const noun = rowMode === 'actions' ? 'action' : 'object'
  const loadingMore = loading && loaded !== null

  return (
    <section className="mb-12 last:mb-0">
      {/* Etched header — mono label + the lens toggle + hairline + the loaded-row
          count, matching the spend bands' chrome but counting actions/objects. */}
      <div className="flex items-end gap-4">
        <div className="flex items-baseline gap-3 pb-1.5">
          <span className="font-mono text-ui font-semibold uppercase tracking-[0.2em] text-ink-2">
            Activity
          </span>
          <ModeTabs value={mode} onChange={switchMode} />
        </div>
        <span className="mb-[9px] h-px flex-1 bg-line-1" />
        <div className="pb-0.5 text-right leading-none">
          <span className="font-mono text-sm tabular-nums text-ink-3">
            {visible === null
              ? '—'
              : `${count}${hasMore ? '+' : ''} ${noun}${count === 1 ? '' : 's'}`}
          </span>
        </div>
      </div>

      {/* Sub-caption — what this lens is, so the coverage difference reads as
          intentional, not a gap. */}
      <div className="mt-1 font-mono text-label tracking-[0.06em] text-ink-3/70">
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
                  className="rounded-[4px] border border-line-1 bg-raised px-3 py-1 font-mono text-label uppercase tracking-[0.12em] text-ink-3 transition-colors hover:bg-raised hover:text-ink-2 disabled:opacity-50"
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
            className={`relative font-mono text-label font-semibold tracking-[0.14em] transition-colors ${
              active ? 'text-warm' : 'text-ink-3/70 hover:text-ink-2'
            }`}
          >
            {t.label}
            {active && <span className="absolute -bottom-1 left-0 right-0 h-px bg-warm" />}
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
      <span className="font-mono text-label-sm font-semibold uppercase tracking-[0.14em] text-ink-3/70">
        {label}
      </span>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={label}
        className="rounded-[4px] border border-line-1 bg-raised px-2 py-1 font-mono text-reported text-ink-2 transition-colors hover:bg-raised focus:outline-none focus:ring-1 focus:ring-warm/40"
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
            className={`relative font-mono text-label tracking-[0.12em] transition-colors ${
              active ? 'text-warm' : 'text-ink-3 hover:text-ink-2'
            }`}
          >
            {t.label}
            {active && <span className="absolute -bottom-1 left-0 right-0 h-px bg-warm" />}
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
      className="max-w-[8rem] shrink-0 truncate rounded-[3px] bg-tint-3 px-1.5 py-0.5 font-mono text-label-sm text-ink-3"
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
        <li key={i} className="h-8 animate-pulse rounded-[4px] border border-line-1 bg-tint-2" />
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
      className={`flex items-center gap-3 py-4 font-mono text-label ${
        isErr ? 'tracking-[0.04em] text-alarm' : 'uppercase tracking-[0.16em] text-ink-3/60'
      }`}
    >
      <span className={`h-px w-6 ${isErr ? 'bg-alarm/40' : 'bg-line-1'}`} />
      {msg}
      <span className={`h-px flex-1 ${isErr ? 'bg-alarm/20' : 'bg-line-1'}`} />
    </div>
  )
}
