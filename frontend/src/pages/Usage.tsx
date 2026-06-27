import { useCallback, useEffect, useId, useState } from 'react'
import {
  PieChart,
  Pie,
  Cell,
  ResponsiveContainer,
  AreaChart,
  Area,
  XAxis,
  YAxis,
  Tooltip,
} from 'recharts'
import * as HintTip from '@radix-ui/react-tooltip'
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

// windowDays is the calendar span of the selected window. Month → days elapsed so
// far this month (month-to-date); the rolling presets → their nominal length.
function windowDays(range: RangeKey): number {
  if (range === '30d') return 30
  if (range === '90d') return 90
  return new Date().getDate()
}

// activeDays is the denominator for every "$/day" average. It's the span from the
// FIRST day we actually have spend (by_day is oldest-first) through now, capped at
// the selected window. A short data history inside a long window — e.g. 3 days of
// spend in an MTD window — must average over those 3 days, not the whole window,
// or the daily rate reads far too low. Falls back to the window length when there
// is no data (the rate is 0 then anyway).
function activeDays(byDay: UsageDayBucket[], window: number): number {
  if (byDay.length === 0) return window
  const first = Date.parse(byDay[0].date + 'T00:00:00Z')
  if (Number.isNaN(first)) return window
  const spanned = Math.floor((Date.now() - first) / 86_400_000) + 1
  return Math.max(1, Math.min(window, spanned))
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
  error: string | null
}

// useUsageFetch loads one usage endpoint and keeps it fresh: it refetches when
// the url (window / team) changes, polls every POLL_INTERVAL_MS while the tab is
// visible, and refetches immediately when the tab regains focus. Crucially it
// HOLDS the previous response while a new one is in flight (never clears `data`
// mid-fetch), so switching the range/team or a background poll updates the
// numbers in place rather than flashing the whole section. Sections branch on
// `data !== null` (skeleton vs content) — there's no separate loading flag.
// A null url means "don't fetch" (the team section before a team resolves).
// setState lives in the `load` callback (not the effect body) — mirrors
// PRDashboard and keeps the set-state-in-effect lint rule happy.
function useUsageFetch<T>(url: string | null): UsageFetch<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (signal: { cancelled: boolean }) => {
      if (!url) {
        setData(null)
        setError(null)
        return
      }
      // Deliberately do NOT clear data/error here — the last good data stays on
      // screen until the new response lands, so a refetch never flashes.
      try {
        const res = await fetch(url)
        if (!res.ok) throw new Error(await readError(res, 'Failed to load usage'))
        const body = (await res.json()) as T
        if (!signal.cancelled) {
          setData(body)
          setError(null)
        }
      } catch (err) {
        if (!signal.cancelled) {
          setError(err instanceof Error ? err.message : String(err))
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

  return { data, error }
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

// by_user has its own roster instrument (avatars + names), so it needs no flat
// BarDatum adapter.

function ruleBars(rows: UsageRuleBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((r) => ({
    key: r.trigger_id,
    label: r.rule_name || r.trigger_id,
    value: r.cost,
  }))
}

// by_team + org_level are consolidated into the org AllocationDonut (which builds
// its own DonutSeg list with palette colors + the overhead tag), so they need no
// flat BarDatum adapter here.

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
  aside,
}: {
  label: string
  children: React.ReactNode
  className?: string
  /** Optional subtle stat that rides the right end of the header rule (e.g. the
   *  roster's per-person average), mirroring the Band header's total + $/day. */
  aside?: React.ReactNode
}) {
  return (
    <div className={className}>
      <div className="mb-3 flex items-center gap-2">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
          {label}
        </span>
        <span className="h-px flex-1 bg-border-subtle/70" />
        {aside}
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

// Meter is a single-value gauge: just a left-anchored gradient that fades to
// nothing at value/max — no track, no frame, only the light. A small floor keeps
// a nonzero-but-tiny value visible.
function Meter({
  value,
  max,
  color = 'var(--color-accent)',
}: {
  value: number
  max: number
  color?: string
}) {
  const fill = value <= 0 ? 0 : Math.min(100, Math.max(6, (value / max) * 100))
  return (
    <div className="relative h-2.5">
      <span
        aria-hidden
        className="absolute inset-0"
        style={{ background: `linear-gradient(90deg, ${color} 0%, transparent ${fill}%)` }}
      />
    </div>
  )
}

// InfoHint is a small "?" affordance whose hover popup explains a term — used in
// the allocation legend so a non-team slice can say what it is without a cryptic
// inline tag. Styled to match the board/run-detail tooltips.
function InfoHint({ text }: { text: string }) {
  return (
    <HintTip.Provider delayDuration={150}>
      <HintTip.Root>
        <HintTip.Trigger asChild>
          <button
            type="button"
            aria-label="What's this?"
            className="inline-flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded-full border border-text-tertiary/30 font-mono text-[8px] leading-none text-text-tertiary/70 transition-colors hover:border-accent hover:text-accent"
          >
            ?
          </button>
        </HintTip.Trigger>
        <HintTip.Portal>
          <HintTip.Content
            side="top"
            sideOffset={6}
            className="z-[100] max-w-[240px] rounded-lg border border-border-glass bg-surface-raised px-3 py-2 text-[12px] leading-relaxed text-text-secondary shadow-lg shadow-black/[0.06]"
          >
            {text}
            <HintTip.Arrow className="fill-surface-raised" />
          </HintTip.Content>
        </HintTip.Portal>
      </HintTip.Root>
    </HintTip.Provider>
  )
}

// Donut is the composition instrument — a thin wireframe ring (a stroke, not a
// bulbous pie) with gapped segments and a monospace legend that carries the
// numbers. Used for both the category split and the org allocation. The ring is
// decorative (renders nothing in jsdom); the legend is the readable record.
interface DonutSeg {
  key: string
  label: string
  color: string
  value: number
  /** Optional explainer — renders a "?" after the label whose hover popup shows
   *  this text. Used by the org allocation's non-team slices (system jobs /
   *  team-less curator). */
  hint?: string
  title?: string
}
function Donut({
  segments,
  ringClass = 'h-24 w-24',
  legendScroll = false,
}: {
  segments: DonutSeg[]
  ringClass?: string
  /** Cap the legend height and let it scroll — for the larger org dials whose
   *  legends can run long (many teams / users). */
  legendScroll?: boolean
}) {
  // Linked hover: hovering a legend row (or a chart slice) highlights its slice
  // and dims the rest, so reading "which wedge is this team" is one glance.
  const [active, setActive] = useState<string | null>(null)
  const data = segments.filter((d) => d.value > 0).sort((a, b) => b.value - a.value)
  const total = data.reduce((s, d) => s + d.value, 0)
  if (total === 0) return <ZeroMini label="no spend" />

  return (
    <div className="flex items-center gap-5">
      <div className={`${ringClass} shrink-0`}>
        <ResponsiveContainer>
          <PieChart>
            <Pie
              data={data}
              dataKey="value"
              nameKey="label"
              cx="50%"
              cy="50%"
              innerRadius="70%"
              outerRadius="100%"
              paddingAngle={2}
              startAngle={90}
              endAngle={-270}
              stroke="none"
              onMouseEnter={(_, i) => setActive(data[i]?.key ?? null)}
              onMouseLeave={() => setActive(null)}
            >
              {data.map((d) => (
                <Cell
                  key={d.key}
                  fill={d.color}
                  fillOpacity={active && active !== d.key ? 0.25 : 1}
                  style={{ transition: 'fill-opacity 0.15s ease' }}
                />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
      </div>
      <ul
        className={`min-w-0 flex-1 space-y-1.5 ${legendScroll ? 'max-h-44 overflow-y-auto pr-1' : ''}`}
      >
        {data.map((d) => (
          <li
            key={d.key}
            onMouseEnter={() => setActive(d.key)}
            onMouseLeave={() => setActive(null)}
            className={`-mx-1 flex items-center justify-between gap-2 rounded-[3px] px-1 font-mono text-[11px] transition-colors ${
              active === d.key ? 'bg-black/[0.04]' : ''
            }`}
          >
            <span className="flex min-w-0 items-center gap-1.5">
              <span className="h-2 w-2 shrink-0 rounded-[2px]" style={{ background: d.color }} />
              <span className="truncate text-text-secondary">{d.label}</span>
              {d.hint && <InfoHint text={d.hint} />}
            </span>
            <span className="shrink-0 tabular-nums text-text-tertiary" title={d.title}>
              {fmtUSD(d.value)} · {pct(d.value, total)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

// CategoryDonut renders the spend-by-category split as a Donut; legend values
// carry the token-breakdown tooltip. `large` gives the org's half-width dial a
// bigger ring + scrollable legend (Team's compact 1/3 cell keeps the default).
function CategoryDonut({ data, large = false }: { data: UsageCategoryBucket[]; large?: boolean }) {
  return (
    <Donut
      segments={data.map((d) => ({
        key: d.category,
        label: categoryLabel(d.category),
        color: categoryColor(d.category),
        value: d.cost,
        title: tokenTitle(d),
      }))}
      ringClass={large ? 'h-32 w-32' : 'h-24 w-24'}
      legendScroll={large}
    />
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
              <Meter value={d.value} max={max} color={color} />
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

// initialsOf builds a 1–2 char monogram from a display name (first + last word,
// or the first two letters of a single word).
function initialsOf(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return '?'
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase()
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
}

// Avatar is a square-ish identity chip — a real picture when we have a url, else
// a rust monogram placeholder. A load failure (404, or — until the CSP img-src
// allows OAuth-CDN avatars, TFAC-51 follow-up — a CSP block) flips to the
// monogram via onError, so a blocked picture degrades gracefully instead of
// rendering broken.
function Avatar({ name, url }: { name: string; url?: string }) {
  const [failed, setFailed] = useState(false)
  if (url && !failed) {
    return (
      <img
        src={url}
        alt=""
        onError={() => setFailed(true)}
        className="h-7 w-7 shrink-0 rounded-[3px] object-cover"
      />
    )
  }
  return (
    <span
      aria-hidden
      className="flex h-7 w-7 shrink-0 items-center justify-center rounded-[3px] bg-accent/10 font-mono text-[10px] font-semibold text-accent"
    >
      {initialsOf(name)}
    </span>
  )
}

// RosterAvg is the subtle per-PERSON baseline for a roster's header — mean spend
// across the people in it (total / head count). Deliberately NOT a per-day rate:
// the section header already carries the daily burn; when looking at individuals
// we want a baseline ("the typical person costs ~$X"), not a rate. Null when empty.
function RosterAvg({ data }: { data: UsageUserBucket[] }) {
  const rows = data.filter((d) => d.cost > 0)
  if (rows.length === 0) return null
  const avg = rows.reduce((s, d) => s + d.cost, 0) / rows.length
  return (
    <span className="shrink-0 font-mono text-[9px] tabular-nums text-text-tertiary/60">
      avg {fmtUSD(avg)}
    </span>
  )
}

// UserRoster is the per-person breakdown (team "by member" / org "by user") given
// real room: a fixed-height, vertically-scrollable column of avatar + name +
// spend-gauge rows, showing everyone (not a top-N), in the same gradient-gauge
// language as the rest of the console.
function UserRoster({ data, emptyLabel = '—' }: { data: UsageUserBucket[]; emptyLabel?: string }) {
  const rows = data.filter((d) => d.cost > 0).sort((a, b) => b.cost - a.cost)
  if (rows.length === 0) return <ZeroMini label={emptyLabel} />
  const max = rows[0].cost
  return (
    <div className="h-56 space-y-3 overflow-y-auto pr-1">
      {rows.map((d) => {
        const name = d.display_name || d.user_id
        return (
          <div key={d.user_id} className="flex items-center gap-2.5">
            <Avatar name={name} url={d.avatar_url} />
            <div className="min-w-0 flex-1">
              <div className="flex items-baseline justify-between gap-2 font-mono text-[11px]">
                <span className="truncate text-text-secondary" title={name}>
                  {name}
                </span>
                <span className="shrink-0 tabular-nums text-text-tertiary">{fmtUSD(d.cost)}</span>
              </div>
              <Meter value={d.cost} max={max} />
            </div>
          </div>
        )
      })}
    </div>
  )
}

// Trace is the "over time" readout: a borderless accent line with a soft area
// fade, sitting directly on the warm console over a single faint baseline — no
// panel, no screen, no scanlines. It matches the gauges' gradient language and
// the "everything melts into the page" ethos.
function Trace({ data, heightClass = 'h-24' }: { data: UsageDayBucket[]; heightClass?: string }) {
  // Unique per instance — three traces render at once, and a duplicated gradient
  // id would be invalid HTML. Strip useId's colons so it's a clean SVG fragment ref.
  const gradId = `usageTrace-${useId().replace(/:/g, '')}`
  if (data.length === 0)
    return (
      <div className={`flex ${heightClass} items-center justify-center`}>
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
    <div className={`relative ${heightClass}`}>
      <ResponsiveContainer>
        <AreaChart data={formatted} margin={{ top: 6, right: 2, bottom: 0, left: 2 }}>
          <defs>
            <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.22} />
              <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0} />
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
            stroke="var(--color-accent)"
            strokeWidth={1.5}
            fill={`url(#${gradId})`}
          />
        </AreaChart>
      </ResponsiveContainer>
      {/* A single faint baseline grounds the plot without a panel. */}
      <span
        aria-hidden
        className="pointer-events-none absolute inset-x-0 bottom-0 h-px bg-border-subtle"
      />
    </div>
  )
}

// Teams have no inherent tone, so the allocation ring cycles them through a
// palette (org-level slices keep their category color instead).
const TEAM_PALETTE = [
  'var(--color-accent)',
  'var(--color-delegate)',
  'var(--color-snooze)',
  'var(--color-dismiss)',
  'var(--hmi-cyan)',
  'var(--color-claim)',
]

// Explainers for the org-level (non-team) slices. These stay distinct categories
// — the backend's org_level is grouped by category, so team-less curator comes
// back separate from the system jobs — and each gets a "?" rather than a shared
// "overhead" tag.
const ORG_LEVEL_HINT: Record<string, string> = {
  system_overhead:
    'Org-wide system jobs, not tied to any team — the scorer, repo-profiler, and project classifier.',
  curator: 'Curator turns on projects that aren’t attached to a team.',
}

// AllocationDonut is the org hero: the WHOLE org spend as one ring, partitioned
// across teams + the org-level (non-team) slices. This is exactly the backend's
// partition invariant — total == sum(by_team) + sum(org_level) — so it reads as
// "where every dollar went". Team slices take palette colors; the org-level
// slices keep their category tone + label (System / Curator) and carry a "?"
// explainer.
function AllocationDonut({
  byTeam,
  orgLevel,
}: {
  byTeam: UsageTeamBucket[]
  orgLevel: UsageOrgLevelBucket[]
}) {
  const teamSegs: DonutSeg[] = byTeam
    .filter((t) => t.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .map((t, i) => ({
      key: `team:${t.team_id}`,
      label: t.team_name || t.team_id,
      value: t.cost,
      color: TEAM_PALETTE[i % TEAM_PALETTE.length],
      title: `${t.team_name || t.team_id} · ${fmtUSD(t.cost)}`,
    }))
  const olSegs: DonutSeg[] = orgLevel
    .filter((o) => o.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .map((o) => ({
      key: `org:${o.category}`,
      label: categoryLabel(o.category),
      value: o.cost,
      color: categoryColor(o.category),
      hint: ORG_LEVEL_HINT[o.category],
      title: `${categoryLabel(o.category)} · ${fmtUSD(o.cost)}`,
    }))
  // A bigger ring + a scrollable legend, since the org dial can carry many teams.
  return <Donut segments={[...teamSegs, ...olSegs]} ringClass="h-32 w-32" legendScroll />
}

// ThroughputInstrument fuses "over time" + "by model" into one readout: a wide
// trace plot with a model telemetry rail beside it (stacks below on narrow
// viewports).
function ThroughputInstrument({
  byDay,
  byModel,
  tall = false,
  spanClass = 'md:col-span-2 lg:col-span-3',
}: {
  byDay: UsageDayBucket[]
  byModel: UsageModelBucket[]
  /** Hero mode — a taller plot. Used by Personal, where the throughput is the
   *  section's only instrument. */
  tall?: boolean
  /** Grid span override (Team gives it 2/3 so a Category dial can share its row). */
  spanClass?: string
}) {
  return (
    <Instrument label="Over time" className={spanClass}>
      <div className="flex flex-col gap-5 lg:flex-row">
        <div className="min-w-0 flex-1">
          <Trace data={byDay} heightClass={tall ? 'h-44' : 'h-32'} />
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
  const active = activeDays(data?.by_day ?? [], days)
  return (
    <Band
      label="Personal"
      total={total}
      rate={total / active}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      {/* Personal is intentionally the leanest scope: just your spend trend +
          the models behind it. (Category — manual vs curator — added little at
          the personal level, so it's dropped here; it still lives in Team/Org.) */}
      <ThroughputInstrument byDay={data?.by_day ?? []} byModel={data?.by_model ?? []} tall />
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
  const active = activeDays(data?.by_day ?? [], days)
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
      rate={total / active}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      {/* By member is the wide, tall, scrollable roster (2/3); by rule rides
          beside it, then category + throughput share the next row. */}
      <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-2 lg:grid-cols-3">
        <Instrument
          label="By member"
          className="md:col-span-2 lg:col-span-2"
          aside={<RosterAvg data={data?.by_user ?? []} />}
        >
          <UserRoster data={data?.by_user ?? []} emptyLabel="no member spend" />
        </Instrument>
        <Instrument label="By rule">
          <Gauges data={ruleBars(data?.by_rule)} emptyLabel="no automated runs" />
        </Instrument>
        <Instrument label="Category">
          <CategoryDonut data={data?.by_category ?? []} />
        </Instrument>
        <ThroughputInstrument
          byDay={data?.by_day ?? []}
          byModel={data?.by_model ?? []}
          spanClass="md:col-span-2 lg:col-span-2"
        />
      </div>
    </Band>
  )
}

function OrgSection({ since, days }: { since: string; days: number }) {
  const { data, error } = useUsageFetch<UsageOrgResponse>(withWindow('/api/usage/org', since))
  const total = data?.total_cost_usd ?? 0
  const active = activeDays(data?.by_day ?? [], days)
  return (
    <Band
      label="Org"
      total={total}
      rate={total / active}
      hasData={data !== null}
      error={error}
      empty={total === 0}
    >
      {/* Allocation + Category as two half-width dials — larger rings with
          scrollable, hover-linked side legends — then the wide by-user roster and
          the full-width throughput. */}
      <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-2">
        {/* Allocation — the whole org spend partitioned across teams + the org-
            level (non-team) slices (merges the old "by team" + "org-level"). */}
        <Instrument label="Allocation">
          <AllocationDonut byTeam={data?.by_team ?? []} orgLevel={data?.org_level ?? []} />
        </Instrument>
        <Instrument label="Category">
          <CategoryDonut data={data?.by_category ?? []} large />
        </Instrument>
        <Instrument
          label="By user"
          className="md:col-span-2"
          aside={<RosterAvg data={data?.by_user ?? []} />}
        >
          <UserRoster data={data?.by_user ?? []} emptyLabel="no user spend" />
        </Instrument>
        <ThroughputInstrument
          byDay={data?.by_day ?? []}
          byModel={data?.by_model ?? []}
          spanClass="md:col-span-2"
        />
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
