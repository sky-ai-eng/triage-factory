import { useCallback, useEffect, useState } from 'react'
import { ResponsiveContainer, AreaChart, Area, XAxis, YAxis, Tooltip } from 'recharts'
import TeamSwitch from '../components/TeamSwitch'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeams } from '../hooks/useTeams'
import { readError } from '../lib/api'
import type {
  TeamSummary,
  UsageCategoryBucket,
  UsageMeResponse,
  UsageModelBucket,
  UsageOrgResponse,
  UsageRuleBucket,
  UsageTeamBucket,
  UsageTeamResponse,
  UsageUserBucket,
  UsageOrgLevelBucket,
  UsageDayBucket,
} from '../types'

// Usage is the core spend dashboard (TFAC-479) over the spend-layer read API
// (TFAC-478) — styled as a "burn console" rather than a grid of cards, taking the
// Board / Run Station design language: borderless bands that melt into the page,
// etched monospace section labels, thin glowing telemetry gauges, and a dark HMI
// "screen" for each time-series. Three role-gated sections, each self-gating:
//
//   - Personal (everyone): GET /api/usage/me — your own runs + curator turns.
//   - Team (admins of their OWN teams): GET /api/usage/teams/{id}, chosen via a
//     TeamSwitch over the teams you admin. Team detail is team-admin-only, so the
//     dropdown is sourced from useTeams() filtered to role==='admin' — NOT from
//     the org rollup's by_team (drilling into a team you don't admin would 403).
//   - Org rollup (org admins + local N=1): GET /api/usage/org — every team +
//     curator + system overhead. The org rollup is the ONLY place system-overhead
//     spend surfaces, so local mode shows it too (see Usage() below).
//
// All three reads are session-org-scoped (org from the session, like
// /api/dashboard/*), so we hit the literal /api/usage/* paths with no org prefix.

// --- query window ---

type RangeKey = 'month' | '30d' | '90d'

const RANGES: { key: RangeKey; label: string }[] = [
  { key: 'month', label: 'MTD' },
  { key: '30d', label: '30D' },
  { key: '90d', label: '90D' },
]

// sinceParam maps a range preset to the `since` query value. 'month' returns ''
// so we send no param and the backend defaults to the current UTC month → now;
// the rolling presets send a YYYY-MM-DD date (the backend leaves `until` open at
// now). The window is half-open [since, until).
function sinceParam(range: RangeKey): string {
  if (range === 'month') return ''
  const days = range === '30d' ? 30 : 90
  return new Date(Date.now() - days * 86_400_000).toISOString().slice(0, 10)
}

// windowDays is the calendar span of the active window, used only for the "avg
// $/day" burn-rate readout in each band header. Month → days elapsed so far this
// month (month-to-date); the rolling presets → their nominal length.
function windowDays(range: RangeKey): number {
  if (range === '30d') return 30
  if (range === '90d') return 90
  return new Date().getDate()
}

function withWindow(path: string, since: string): string {
  return since ? `${path}?since=${encodeURIComponent(since)}` : path
}

// --- fetch hook ---

// Spend settles sporadically — a run / curator turn / system job finishing every
// few seconds-to-minutes — so the page POLLS rather than streams; a websocket
// spend feed would be mostly idle chatter for a once-in-a-while update. 15s feels
// live for a cost view without hammering the aggregation reads (the /org rollup
// is a cross-RLS scan). It's the single knob: polling much faster is the point a
// short-TTL server-side cache would start to earn its keep, but at this cadence
// the per-org/-month volumes are modest enough to read straight through (matching
// the backend's deliberate "materialize nothing" stance).
const POLL_INTERVAL_MS = 15_000

interface UsageFetch<T> {
  data: T | null
  /** True ONLY on the cold load (no data yet). A refetch (window switch, team
   *  switch, poll) keeps the last data on screen instead of flashing. */
  loading: boolean
  error: string | null
}

// useUsageFetch loads one usage endpoint and keeps it fresh: it refetches when
// the url (window / team) changes, polls every POLL_INTERVAL_MS while the tab is
// visible, and refetches immediately when the tab regains focus. Crucially it
// HOLDS the previous response while a new one is in flight (never clears `data`
// mid-fetch), so switching the range/team or a background poll updates the
// numbers in place rather than flashing the whole section. A null url means
// "don't fetch" (the team section before a team resolves). setState lives in the
// `load` callback (not the effect body) — mirrors PRDashboard and keeps the
// set-state-in-effect lint rule happy.
function useUsageFetch<T>(url: string | null): UsageFetch<T> {
  const [data, setData] = useState<T | null>(null)
  const [fetching, setFetching] = useState<boolean>(url !== null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (signal: { cancelled: boolean }) => {
      if (!url) {
        setData(null)
        setFetching(false)
        setError(null)
        return
      }
      setFetching(true)
      // Deliberately do NOT clear data/error here — the last good data stays on
      // screen until the new response lands, so a refetch never flashes.
      try {
        const res = await fetch(url)
        if (!res.ok) throw new Error(await readError(res, 'Failed to load usage'))
        const body = (await res.json()) as T
        if (!signal.cancelled) {
          setData(body)
          setError(null)
          setFetching(false)
        }
      } catch (err) {
        if (!signal.cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setFetching(false)
        }
      }
    },
    [url],
  )

  useEffect(() => {
    const signal = { cancelled: false }
    void load(signal) // immediate on mount + whenever the url (window/team) changes
    // Poll only while visible — a backgrounded tab shouldn't churn the DB — and
    // refetch the instant it regains focus so it's never stale on return.
    const poll = () => {
      if (document.visibilityState === 'visible') void load(signal)
    }
    const interval = setInterval(poll, POLL_INTERVAL_MS)
    document.addEventListener('visibilitychange', poll)
    return () => {
      signal.cancelled = true
      clearInterval(interval)
      document.removeEventListener('visibilitychange', poll)
    }
  }, [load])

  // loading = cold load only (no data yet). Once we have data, a refetch keeps it
  // visible, so callers render the data, not a skeleton.
  return { data, loading: data === null && fetching, error }
}

// --- formatting + category mapping ---

// fmtUSD renders settled spend as real money (TFAC-449: total_cost_usd is exact,
// not an estimate). Sub-cent-but-nonzero collapses to "<$0.01" so a tiny curator
// turn doesn't render as a misleading "$0.00".
function fmtUSD(n: number): string {
  if (!n) return '$0.00'
  if (n > 0 && n < 0.01) return '<$0.01'
  return n.toLocaleString('en-US', { style: 'currency', currency: 'USD' })
}

function pct(value: number, total: number): string {
  if (total <= 0) return '0%'
  return `${Math.round((value / total) * 100)}%`
}

// glow is the emissive shadow on a lit gauge fill — the "status via light" move
// from the board cards / telemetry rail, tinted to the gauge's own color.
function glow(color: string): string {
  return `0 0 8px color-mix(in srgb, ${color} 55%, transparent)`
}

// Category axis (domain.SpendCategory*) → the labels the epic specifies for the
// org by-category split (automated / delegated / curator / system) and a stable
// tone per category, reusing the board's semantic color vocabulary.
const CATEGORY_LABEL: Record<string, string> = {
  manual: 'Delegated',
  autonomous: 'Automated',
  curator: 'Curator',
  system_overhead: 'System',
}

const CATEGORY_COLOR: Record<string, string> = {
  manual: 'var(--color-delegate)',
  autonomous: 'var(--color-accent)',
  curator: 'var(--color-claim)',
  system_overhead: 'var(--color-text-tertiary)',
}

function categoryLabel(category: string): string {
  return CATEGORY_LABEL[category] ?? category
}

function categoryColor(category: string): string {
  return CATEGORY_COLOR[category] ?? 'var(--color-snooze)'
}

// tokenTitle is the burn-bar legend hover detail. The parenthetical parts must
// sum to the stated total, so the breakdown lists every component that goes into
// it (input + output + the two cache classes, folded into one "cached" figure),
// omitting any that are zero.
function tokenTitle(b: UsageCategoryBucket): string {
  const cached = b.cache_read_tokens + b.cache_creation_tokens
  const total = b.input_tokens + b.output_tokens + cached
  const parts: string[] = []
  if (b.input_tokens) parts.push(`${b.input_tokens.toLocaleString()} in`)
  if (b.output_tokens) parts.push(`${b.output_tokens.toLocaleString()} out`)
  if (cached) parts.push(`${cached.toLocaleString()} cached`)
  const breakdown = parts.length ? ` (${parts.join(' · ')})` : ''
  return `${total.toLocaleString()} tokens${breakdown}`
}

// --- gauge adapters: each labeled breakdown → a common {key,label,value} ---

interface BarDatum {
  key: string
  label: string
  value: number
  /** Optional per-row tone; defaults to the rust accent. Used by org-level so
   *  each category's gauge carries its own color. */
  color?: string
}

function modelBars(rows: UsageModelBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((m) => ({ key: m.model, label: m.model, value: m.cost }))
}

function userBars(rows: UsageUserBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((u) => ({
    key: u.user_id,
    label: u.display_name || u.user_id,
    value: u.cost,
  }))
}

function ruleBars(rows: UsageRuleBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((r) => ({
    key: r.trigger_id,
    label: r.rule_name || r.trigger_id,
    value: r.cost,
  }))
}

// by_team + org_level are consolidated into the org AllocationBar (which inlines
// its own segment mapping with palette colors + the overhead tag), so they need
// no flat BarDatum adapter here.

// --- instrument primitives (borderless; separated by light + hairlines) ---

const tooltipStyle = {
  background: 'rgba(255,255,255,0.9)',
  backdropFilter: 'blur(12px)',
  border: '1px solid rgba(255,255,255,0.7)',
  borderRadius: '6px',
  fontSize: '11px',
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
  boxShadow: '0 4px 12px rgba(0,0,0,0.05)',
} as const

const shimmer = 'animate-pulse bg-black/[0.04] rounded'

// Instrument is one readout in a band — a tiny etched monospace label + a
// hairline running off to the edge, then the viz. No box, no fill: the field
// shows through, the way the board columns and the telemetry rail read.
function Instrument({
  label,
  children,
  className = '',
}: {
  label: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <div className={className}>
      <div className="mb-3 flex items-center gap-2">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
          {label}
        </span>
        <span className="h-px flex-1 bg-border-subtle/70" />
      </div>
      {children}
    </div>
  )
}

function ZeroMini({ label = '—' }: { label?: string }) {
  return (
    <p className="py-6 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
      {label}
    </p>
  )
}

// BurnBar is the category split — a thin segmented meter (one lit segment per
// category, tones butted with a hairline gap) over a monospace legend. Replaces
// the donut: flatter, gauge-like, reads in a glance.
function BurnBar({ data }: { data: UsageCategoryBucket[] }) {
  const segs = data
    .map((d) => ({ bucket: d, label: categoryLabel(d.category), color: categoryColor(d.category) }))
    .filter((d) => d.bucket.cost > 0)
    .sort((a, b) => b.bucket.cost - a.bucket.cost)
  const total = segs.reduce((s, d) => s + d.bucket.cost, 0)
  if (total === 0) return <ZeroMini label="no spend" />

  return (
    <div>
      <div className="flex h-2 w-full gap-px overflow-hidden rounded-full bg-black/[0.06]">
        {segs.map((d) => (
          <span
            key={d.bucket.category}
            title={`${d.label} · ${fmtUSD(d.bucket.cost)}`}
            style={{
              width: `${(d.bucket.cost / total) * 100}%`,
              background: d.color,
              boxShadow: glow(d.color),
            }}
          />
        ))}
      </div>
      {/* Horizontal readout strip — chips wrap, so the bar reads as one wide
          meter rather than a stacked list of rows. */}
      <ul className="mt-3 flex flex-wrap gap-x-5 gap-y-1.5">
        {segs.map((d) => (
          <li key={d.bucket.category} className="flex items-center gap-1.5 font-mono text-[11px]">
            <span className="h-2 w-2 shrink-0 rounded-[2px]" style={{ background: d.color }} />
            <span className="text-text-secondary">{d.label}</span>
            <span className="tabular-nums text-text-tertiary" title={tokenTitle(d.bucket)}>
              {fmtUSD(d.bucket.cost)} · {pct(d.bucket.cost, total)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

// Gauges is the labeled-breakdown instrument (by team / user / rule / model) — a
// stack of telemetry gauges: monospace `label … $value` over a thin track with a
// glowing fill. Top-N by cost, with a non-silent "+N more".
function Gauges({
  data,
  emptyLabel = '—',
  limit = 8,
}: {
  data: BarDatum[]
  emptyLabel?: string
  limit?: number
}) {
  const rows = data.filter((d) => d.value > 0).sort((a, b) => b.value - a.value)
  if (rows.length === 0) return <ZeroMini label={emptyLabel} />
  const shown = rows.slice(0, limit)
  const hidden = rows.length - shown.length
  const max = shown[0].value

  return (
    <div>
      <ul className="space-y-2.5">
        {shown.map((d) => {
          const color = d.color ?? 'var(--color-accent)'
          return (
            <li key={d.key} className="space-y-1">
              <div className="flex items-baseline justify-between gap-2 font-mono text-[11px]">
                <span className="truncate text-text-secondary" title={d.label}>
                  {d.label}
                </span>
                <span className="shrink-0 tabular-nums text-text-tertiary">{fmtUSD(d.value)}</span>
              </div>
              <div className="h-1.5 overflow-hidden rounded-full bg-black/[0.06]">
                <span
                  className="block h-full rounded-full"
                  style={{
                    width: `${Math.max((d.value / max) * 100, 2)}%`,
                    background: color,
                    boxShadow: glow(color),
                  }}
                />
              </div>
            </li>
          )
        })}
      </ul>
      {hidden > 0 && (
        <p className="mt-2.5 font-mono text-[10px] text-text-tertiary/70">+{hidden} more</p>
      )}
    </div>
  )
}

// Trace is the "over time" readout, styled as a piece of equipment: a dark-glass
// HMI screen (warm backlit-paper in light mode) with faint scanlines and a cyan
// oscilloscope line — the one inset screen in the otherwise-warm console, echoing
// the Run Station.
function Trace({ data, heightClass = 'h-24' }: { data: UsageDayBucket[]; heightClass?: string }) {
  if (data.length === 0)
    return (
      <div
        className={`flex ${heightClass} items-center justify-center rounded-[4px]`}
        style={{ background: 'var(--hmi-screen)', boxShadow: 'inset 0 0 0 1px var(--hmi-line)' }}
      >
        <ZeroMini label="no activity" />
      </div>
    )
  const formatted = data.map((d) => ({
    ...d,
    label: new Date(d.date + 'T00:00:00').toLocaleDateString([], {
      month: 'short',
      day: 'numeric',
    }),
  }))

  return (
    <div
      className={`relative ${heightClass} overflow-hidden rounded-[4px]`}
      style={{ background: 'var(--hmi-screen)', boxShadow: 'inset 0 0 0 1px var(--hmi-line)' }}
    >
      {/* Scanlines — the HMI screen texture. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          backgroundImage:
            'repeating-linear-gradient(to bottom, var(--hmi-scanline) 0, var(--hmi-scanline) 1px, transparent 1px, transparent 3px)',
        }}
      />
      <ResponsiveContainer>
        <AreaChart data={formatted} margin={{ top: 8, right: 8, bottom: 4, left: 8 }}>
          <defs>
            <linearGradient id="usageTrace" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--hmi-cyan)" stopOpacity={0.35} />
              <stop offset="100%" stopColor="var(--hmi-cyan)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="label" hide />
          <YAxis hide />
          <Tooltip
            contentStyle={tooltipStyle}
            formatter={(value) => [fmtUSD(Number(value)), 'spend']}
            labelFormatter={(label) => String(label)}
          />
          <Area
            type="monotone"
            dataKey="cost"
            stroke="var(--hmi-cyan)"
            strokeWidth={1.5}
            fill="url(#usageTrace)"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

// Teams have no inherent tone, so the allocation meter cycles them through a
// palette (org-level overhead keeps its category color instead).
const TEAM_PALETTE = [
  'var(--color-accent)',
  'var(--color-delegate)',
  'var(--color-snooze)',
  'var(--color-dismiss)',
  'var(--hmi-cyan)',
  'var(--color-claim)',
]

// AllocationBar is the org hero: the WHOLE org spend in one stacked meter,
// partitioned across teams + the org-level overhead (curator-on-non-team /
// system). This is exactly the backend's partition invariant — total ==
// sum(by_team) + sum(org_level) — so it reads as "where every dollar went".
// Team segments take palette colors; overhead segments keep their category tone
// and wear an "ovh" tag in the legend so attributable vs overhead is legible.
function AllocationBar({
  byTeam,
  orgLevel,
}: {
  byTeam: UsageTeamBucket[]
  orgLevel: UsageOrgLevelBucket[]
}) {
  const teamSegs = byTeam
    .filter((t) => t.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .map((t, i) => ({
      key: `team:${t.team_id}`,
      label: t.team_name || t.team_id,
      cost: t.cost,
      color: TEAM_PALETTE[i % TEAM_PALETTE.length],
      overhead: false,
    }))
  const olSegs = orgLevel
    .filter((o) => o.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .map((o) => ({
      key: `ovh:${o.category}`,
      label: categoryLabel(o.category),
      cost: o.cost,
      color: categoryColor(o.category),
      overhead: true,
    }))
  const segs = [...teamSegs, ...olSegs]
  const total = segs.reduce((s, d) => s + d.cost, 0)
  if (total === 0) return <ZeroMini label="no spend" />

  return (
    <div>
      <div className="flex h-2.5 w-full gap-px overflow-hidden rounded-full bg-black/[0.06]">
        {segs.map((d) => (
          <span
            key={d.key}
            title={`${d.label} · ${fmtUSD(d.cost)}`}
            style={{
              width: `${(d.cost / total) * 100}%`,
              background: d.color,
              boxShadow: glow(d.color),
            }}
          />
        ))}
      </div>
      <ul className="mt-3 grid grid-cols-2 gap-x-6 gap-y-1.5 sm:grid-cols-3">
        {segs.map((d) => (
          <li key={d.key} className="flex items-center justify-between gap-2 font-mono text-[11px]">
            <span className="flex min-w-0 items-center gap-1.5">
              <span className="h-2 w-2 shrink-0 rounded-[2px]" style={{ background: d.color }} />
              <span className="truncate text-text-secondary">{d.label}</span>
              {d.overhead && (
                <span className="shrink-0 text-[9px] uppercase tracking-wider text-text-tertiary/50">
                  ovh
                </span>
              )}
            </span>
            <span className="shrink-0 tabular-nums text-text-tertiary">
              {fmtUSD(d.cost)} · {pct(d.cost, total)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

// ThroughputInstrument fuses "over time" + "by model" into one readout shaped
// like the Run Station: a wide HMI trace screen with a model telemetry rail
// etched into the housing beside it (stacks below on narrow viewports).
function ThroughputInstrument({
  byDay,
  byModel,
}: {
  byDay: UsageDayBucket[]
  byModel: UsageModelBucket[]
}) {
  return (
    <Instrument label="Over time" className="md:col-span-2 lg:col-span-3">
      <div className="flex flex-col gap-5 lg:flex-row">
        <div className="min-w-0 flex-1">
          <Trace data={byDay} heightClass="h-32" />
        </div>
        <div className="lg:w-[200px] lg:shrink-0">
          <div className="mb-3 flex items-center gap-2">
            <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
              by model
            </span>
            <span className="h-px flex-1 bg-border-subtle/70" />
          </div>
          <Gauges data={modelBars(byModel)} emptyLabel="no model spend" limit={5} />
        </div>
      </div>
    </Instrument>
  )
}

function SkeletonBand() {
  return (
    <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-3">
      {[0, 1, 2].map((i) => (
        <div key={i}>
          <div className={`mb-3 h-2 w-20 ${shimmer}`} />
          <div className="space-y-2.5">
            <div className={`h-1.5 w-full ${shimmer}`} />
            <div className={`h-1.5 w-4/5 ${shimmer}`} />
            <div className={`h-1.5 w-3/5 ${shimmer}`} />
          </div>
        </div>
      ))}
    </div>
  )
}

// EtchedNote is the empty / error state — a single etched monospace line with
// hairline rules, not a boxed panel (a zero window must read as "quiet", not as
// a missing card).
function EtchedNote({ msg, tone = 'muted' }: { msg: string; tone?: 'muted' | 'error' }) {
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

// Band is one section of the console: an etched header (mono label + hairline +
// the section total and avg burn-rate riding the right end) over its body of
// instruments. The skeleton / error / zero-note shows only before the first load
// — once we have data it stays on screen across refetches (no flash).
function Band({
  label,
  right,
  total,
  rate,
  hasData,
  error,
  empty,
  children,
}: {
  label: string
  right?: React.ReactNode
  total: number
  rate: number
  hasData: boolean
  error: string | null
  empty: boolean
  children: React.ReactNode
}) {
  const loading = !hasData && !error
  return (
    <section className="mb-12 last:mb-0">
      <div className="flex items-end gap-4">
        <div className="flex items-baseline gap-2.5 pb-1.5">
          <span className="font-mono text-[12px] font-semibold uppercase tracking-[0.2em] text-text-secondary">
            {label}
          </span>
          {right}
        </div>
        <span className="mb-[9px] h-px flex-1 bg-border-subtle" />
        <div className="pb-0.5 text-right leading-none">
          <span className="font-mono text-lg font-semibold tabular-nums text-text-primary">
            {loading ? '—' : fmtUSD(total)}
          </span>
          {!loading && !empty && (
            <span className="ml-3 font-mono text-[10px] tabular-nums text-text-tertiary/80">
              {fmtUSD(rate)}/day
            </span>
          )}
        </div>
      </div>
      <div className="mt-6">
        {!hasData ? (
          error ? (
            <EtchedNote msg={error} tone="error" />
          ) : (
            <SkeletonBand />
          )
        ) : empty ? (
          <EtchedNote msg="no settled burn · this window" />
        ) : (
          children
        )}
      </div>
    </section>
  )
}

// --- sections ---

function PersonalSection({ since, days }: { since: string; days: number }) {
  const { data, error } = useUsageFetch<UsageMeResponse>(withWindow('/api/usage/me', since))
  const total = data?.total_cost_usd ?? 0
  return (
    <Band
      label="Personal"
      total={total}
      rate={total / days}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      <div className="grid grid-cols-1 gap-x-10 gap-y-8 lg:grid-cols-3">
        <Instrument label="Category" className="lg:col-span-3">
          <BurnBar data={data?.by_category ?? []} />
        </Instrument>
        <ThroughputInstrument byDay={data?.by_day ?? []} byModel={data?.by_model ?? []} />
      </div>
    </Band>
  )
}

function TeamSection({
  adminTeams,
  since,
  days,
}: {
  adminTeams: TeamSummary[]
  since: string
  days: number
}) {
  // The picked team, validated against the teams we currently admin every
  // render — if the held id is no longer one of them (org switch, a late
  // /api/teams load that changed the set), fall back to the first. Deriving the
  // effective id this way avoids storing an invalid selection (no effect needed).
  const [picked, setPicked] = useState('')
  const teamId =
    picked && adminTeams.some((t) => t.id === picked) ? picked : (adminTeams[0]?.id ?? '')

  const { data, error } = useUsageFetch<UsageTeamResponse>(
    teamId ? withWindow(`/api/usage/teams/${teamId}`, since) : null,
  )
  const total = data?.total_cost_usd ?? 0
  // Name from the locally-known selection FIRST, not the response: while a switch
  // is in flight we still hold the previous team's data, so data.team_name would
  // momentarily label the section with the wrong (old) team.
  const teamName = adminTeams.find((t) => t.id === teamId)?.name || data?.team_name || 'team'
  // The switcher renders only at ≥2 admin teams; with one, the header shows the
  // team's name inline so the band is still labeled.
  const right =
    adminTeams.length > 1 ? (
      <TeamSwitch value={teamId} onChange={setPicked} teams={adminTeams} />
    ) : (
      <span className="font-mono text-[11px] tracking-[0.06em] text-text-tertiary/80">
        / {teamName}
      </span>
    )

  return (
    <Band
      label="Team"
      right={right}
      total={total}
      rate={total / days}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-2 lg:grid-cols-3">
        <Instrument label="By member">
          <Gauges data={userBars(data?.by_user)} emptyLabel="no member spend" />
        </Instrument>
        <Instrument label="By rule">
          <Gauges data={ruleBars(data?.by_rule)} emptyLabel="no automated runs" />
        </Instrument>
        <Instrument label="Category">
          <BurnBar data={data?.by_category ?? []} />
        </Instrument>
        <ThroughputInstrument byDay={data?.by_day ?? []} byModel={data?.by_model ?? []} />
      </div>
    </Band>
  )
}

function OrgSection({ since, days }: { since: string; days: number }) {
  const { data, error } = useUsageFetch<UsageOrgResponse>(withWindow('/api/usage/org', since))
  const total = data?.total_cost_usd ?? 0
  return (
    <Band
      label="Org"
      total={total}
      rate={total / days}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-2 lg:grid-cols-3">
        {/* Allocation hero — the whole org spend partitioned across teams +
            overhead (merges the old "by team" + "org-level" instruments). */}
        <Instrument label="Allocation" className="md:col-span-2 lg:col-span-3">
          <AllocationBar byTeam={data?.by_team ?? []} orgLevel={data?.org_level ?? []} />
        </Instrument>
        <Instrument label="By user">
          <Gauges data={userBars(data?.by_user)} emptyLabel="no user spend" />
        </Instrument>
        <Instrument label="Category" className="lg:col-span-2">
          <BurnBar data={data?.by_category ?? []} />
        </Instrument>
        <ThroughputInstrument byDay={data?.by_day ?? []} byModel={data?.by_model ?? []} />
      </div>
    </Band>
  )
}

// --- console chrome ---

// Channel is the window selector, styled as an industrial channel toggle: bare
// monospace labels (MTD / 30D / 90D) with a rust underline-tick on the active
// one — no pill, no fill.
function Channel({ value, onChange }: { value: RangeKey; onChange: (r: RangeKey) => void }) {
  return (
    <div className="flex items-center gap-4">
      {RANGES.map((r) => {
        const active = r.key === value
        return (
          <button
            key={r.key}
            type="button"
            onClick={() => onChange(r.key)}
            className={`relative font-mono text-[11px] tracking-[0.12em] transition-colors ${
              active ? 'text-accent' : 'text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {r.label}
            {active && <span className="absolute -bottom-1.5 left-0 right-0 h-px bg-accent" />}
          </button>
        )
      })}
    </div>
  )
}

// ConsoleFrame wraps the bands in the wireframe-industrial framing: rust corner
// registration ticks (the board's L-bracket DNA, at page scale).
function ConsoleFrame({ children }: { children: React.ReactNode }) {
  const tick = 'pointer-events-none absolute h-3 w-3 border-accent/40'
  return (
    <div className="relative">
      <span aria-hidden className={`${tick} -left-3 -top-3 border-l-[1.5px] border-t-[1.5px]`} />
      <span aria-hidden className={`${tick} -right-3 -top-3 border-r-[1.5px] border-t-[1.5px]`} />
      <span aria-hidden className={`${tick} -bottom-3 -left-3 border-b-[1.5px] border-l-[1.5px]`} />
      <span
        aria-hidden
        className={`${tick} -bottom-3 -right-3 border-b-[1.5px] border-r-[1.5px]`}
      />
      <div className="relative">{children}</div>
    </div>
  )
}

export default function Usage() {
  // The org rollup gates on org-admin; the team section on the teams the viewer
  // admins (filtered from useTeams, NOT the org rollup — drilling into a non-
  // admin team would 403). Local mode (no AuthProvider → useOptionalAuth null)
  // is N=1: the single user owns the whole org and is effectively its admin, so
  // the org section shows there too. That matters — the org rollup is the ONLY
  // section that surfaces system-overhead spend (scorer / repo-profiler /
  // classifier), which carries a NULL creator + NULL team and is excluded from
  // the personal and team views by design. The backend agrees: RequireOrgAdminRole
  // short-circuits to allowed in local mode.
  const { isAdmin } = useOrgRole()
  const isLocal = useOptionalAuth() === null
  const showOrg = isAdmin || isLocal
  const { teams } = useTeams()
  const adminTeams = teams.filter((t) => t.role === 'admin')

  const [range, setRange] = useState<RangeKey>('month')
  const since = sinceParam(range)
  const days = Math.max(1, windowDays(range))

  return (
    <div className="mx-auto max-w-6xl">
      <div className="mb-10 flex items-end justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight text-text-primary">Usage</h1>
          <p className="mt-1 font-mono text-[10px] uppercase tracking-[0.18em] text-text-tertiary/70">
            settled llm spend · real dollars
          </p>
        </div>
        <Channel value={range} onChange={setRange} />
      </div>

      <ConsoleFrame>
        <PersonalSection since={since} days={days} />
        {adminTeams.length > 0 && <TeamSection adminTeams={adminTeams} since={since} days={days} />}
        {showOrg && <OrgSection since={since} days={days} />}
      </ConsoleFrame>
    </div>
  )
}
