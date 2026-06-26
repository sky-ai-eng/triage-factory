import { useCallback, useEffect, useState } from 'react'
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

// Usage is the core spend dashboard (TFAC-479) — a single role-aware page over
// the spend-layer read API (TFAC-478). Three sections, each self-gating:
//
//   - Personal (everyone): GET /api/usage/me — your own runs + curator turns.
//   - Team (admins of their OWN teams): GET /api/usage/teams/{id}, chosen via a
//     TeamSwitch over the teams you admin. Team detail is team-admin-only, so the
//     dropdown is sourced from useTeams() filtered to role==='admin' — NOT from
//     the org rollup's by_team (drilling into a team you don't admin would 403).
//   - Org rollup (org admins): GET /api/usage/org — every team + curator +
//     system overhead. by_team here is a display, not a drill-in control.
//
// All three reads are session-org-scoped (org from the session, like
// /api/dashboard/*), so we hit the literal /api/usage/* paths with no org prefix.
// Safe in both modes: useOrgRole/useTeams return mode-correct values without an
// AuthProvider, so local mode (N=1) shows personal + its single team and no org
// section (the local user isn't an org admin in the role sense, and the rollup
// would just mirror the team view).

// --- query window ---

type RangeKey = 'month' | '30d' | '90d'

const RANGES: { key: RangeKey; label: string }[] = [
  { key: 'month', label: 'This month' },
  { key: '30d', label: 'Last 30 days' },
  { key: '90d', label: 'Last 90 days' },
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

function withWindow(path: string, since: string): string {
  return since ? `${path}?since=${encodeURIComponent(since)}` : path
}

// --- fetch hook ---

interface UsageFetch<T> {
  data: T | null
  loading: boolean
  error: string | null
}

// useUsageFetch loads one usage endpoint, re-running when the url (window/team)
// changes or reloadToken is bumped (the page's Refresh). A null url means "don't
// fetch" (e.g. the team section before a team resolves) and clears to an idle,
// non-loading state. The actual setState calls live in the `load` callback (not
// the effect body) — mirrors PRDashboard's fetchAll pattern and keeps the
// set-state-in-effect lint rule happy.
function useUsageFetch<T>(url: string | null, reloadToken = 0): UsageFetch<T> {
  const [data, setData] = useState<T | null>(null)
  const [loading, setLoading] = useState<boolean>(url !== null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (signal: { cancelled: boolean }) => {
      if (!url) {
        setData(null)
        setLoading(false)
        setError(null)
        return
      }
      setLoading(true)
      setError(null)
      try {
        const res = await fetch(url)
        if (!res.ok) throw new Error(await readError(res, 'Failed to load usage'))
        const body = (await res.json()) as T
        if (!signal.cancelled) {
          setData(body)
          setLoading(false)
        }
      } catch (err) {
        if (!signal.cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      }
    },
    [url],
  )

  useEffect(() => {
    const signal = { cancelled: false }
    void load(signal)
    return () => {
      signal.cancelled = true
    }
  }, [load, reloadToken])

  return { data, loading, error }
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
// color per category reused across the donuts.
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

function tokenTitle(b: UsageCategoryBucket): string {
  const total = b.input_tokens + b.output_tokens + b.cache_read_tokens + b.cache_creation_tokens
  return `${total.toLocaleString()} tokens (${b.input_tokens.toLocaleString()} in / ${b.output_tokens.toLocaleString()} out)`
}

// --- bar-list adapters: each labeled breakdown → a common {key,label,value} ---

interface BarDatum {
  key: string
  label: string
  value: number
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

function teamBars(rows: UsageTeamBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((t) => ({
    key: t.team_id,
    label: t.team_name || t.team_id,
    value: t.cost,
  }))
}

function orgLevelBars(rows: UsageOrgLevelBucket[] | undefined): BarDatum[] {
  return (rows ?? []).map((o) => ({
    key: o.category,
    label: categoryLabel(o.category),
    value: o.cost,
  }))
}

// --- chart + layout primitives ---

const tooltipStyle = {
  background: 'rgba(255,255,255,0.85)',
  backdropFilter: 'blur(12px)',
  border: '1px solid rgba(255,255,255,0.7)',
  borderRadius: '8px',
  fontSize: '11px',
  boxShadow: '0 4px 12px rgba(0,0,0,0.05)',
} as const

function Card({
  title,
  children,
  className = '',
}: {
  title: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={`bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl p-4 shadow-sm shadow-black/[0.03] ${className}`}
    >
      <h3 className="text-[11px] font-medium text-text-tertiary mb-3">{title}</h3>
      {children}
    </div>
  )
}

function ZeroMini({ label = 'No data' }: { label?: string }) {
  return <p className="text-[12px] text-text-tertiary text-center py-6">{label}</p>
}

// CategoryDonut — the by-category split (donut + a text legend). The legend rows
// are real DOM text (cost + share), so the breakdown is readable even where the
// SVG donut can't size itself (and assertable in tests).
function CategoryDonut({ data }: { data: UsageCategoryBucket[] }) {
  const slices = data
    .map((d) => ({
      key: d.category,
      label: categoryLabel(d.category),
      value: d.cost,
      color: categoryColor(d.category),
      bucket: d,
    }))
    .filter((d) => d.value > 0)
    .sort((a, b) => b.value - a.value)
  const total = slices.reduce((sum, d) => sum + d.value, 0)
  if (total === 0) return <ZeroMini label="No spend" />

  return (
    <div className="flex items-center gap-4">
      <div className="w-20 h-20 shrink-0">
        <ResponsiveContainer>
          <PieChart>
            <Pie
              data={slices}
              cx="50%"
              cy="50%"
              innerRadius={22}
              outerRadius={38}
              dataKey="value"
              stroke="none"
            >
              {slices.map((d) => (
                <Cell key={d.key} fill={d.color} />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
      </div>
      <ul className="space-y-1 min-w-0 flex-1">
        {slices.map((d) => (
          <li key={d.key} className="flex items-center justify-between gap-2 text-[12px]">
            <span className="flex items-center gap-1.5 min-w-0">
              <span className="w-2 h-2 rounded-full shrink-0" style={{ background: d.color }} />
              <span className="text-text-secondary truncate">{d.label}</span>
            </span>
            <span className="text-text-tertiary tabular-nums shrink-0" title={tokenTitle(d.bucket)}>
              {fmtUSD(d.value)} · {pct(d.value, total)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

// CostBars — a horizontal bar list for the labeled breakdowns (by team / user /
// rule / model). Sorted cost-desc and capped to `limit` with a "+N more" note so
// a long tail doesn't blow out the card (and the cap is never silent).
function CostBars({
  data,
  emptyLabel = 'No data',
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
      <ul className="space-y-2">
        {shown.map((d) => (
          <li key={d.key} className="space-y-1">
            <div className="flex items-baseline justify-between gap-2 text-[12px]">
              <span className="text-text-secondary truncate" title={d.label}>
                {d.label}
              </span>
              <span className="text-text-tertiary tabular-nums shrink-0">{fmtUSD(d.value)}</span>
            </div>
            <div className="h-1.5 rounded-full bg-black/[0.04] overflow-hidden">
              <div
                className="h-full rounded-full"
                style={{
                  width: `${Math.max((d.value / max) * 100, 2)}%`,
                  background: 'var(--color-accent)',
                }}
              />
            </div>
          </li>
        ))}
      </ul>
      {hidden > 0 && <p className="text-[11px] text-text-tertiary mt-2">+{hidden} more</p>}
    </div>
  )
}

// SpendArea — daily cost over the window.
function SpendArea({ data }: { data: UsageDayBucket[] }) {
  if (data.length === 0) return <ZeroMini label="No activity" />
  const formatted = data.map((d) => ({
    ...d,
    label: new Date(d.date + 'T00:00:00').toLocaleDateString([], {
      month: 'short',
      day: 'numeric',
    }),
  }))

  return (
    <div className="h-24">
      <ResponsiveContainer>
        <AreaChart data={formatted} margin={{ top: 4, right: 4, bottom: 0, left: 4 }}>
          <defs>
            <linearGradient id="usageSpendGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.3} />
              <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="label" hide />
          <YAxis hide />
          <Tooltip
            contentStyle={tooltipStyle}
            formatter={(value) => [fmtUSD(Number(value)), 'Spend']}
            labelFormatter={(label) => String(label)}
          />
          <Area
            type="monotone"
            dataKey="cost"
            stroke="var(--color-accent)"
            strokeWidth={2}
            fill="url(#usageSpendGrad)"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

const shimmer = 'animate-pulse bg-black/[0.04] rounded'

function SkeletonGrid() {
  return (
    <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
      {[0, 1, 2].map((i) => (
        <div key={i} className="bg-surface-raised border border-border-glass rounded-2xl p-4">
          <div className={`h-2.5 w-20 mb-4 ${shimmer}`} />
          <div className="space-y-2">
            <div className={`h-2.5 w-full ${shimmer}`} />
            <div className={`h-2.5 w-4/5 ${shimmer}`} />
            <div className={`h-2.5 w-3/5 ${shimmer}`} />
          </div>
        </div>
      ))}
    </div>
  )
}

function ErrorState({ msg }: { msg: string }) {
  return (
    <div className="rounded-2xl border border-dismiss/30 bg-dismiss/5 py-8 px-4 text-center">
      <p className="text-sm text-dismiss">{msg}</p>
    </div>
  )
}

function ZeroState() {
  return (
    <div className="rounded-2xl border border-border-subtle bg-black/[0.01] py-12 text-center">
      <p className="text-sm text-text-secondary">No spend in this window</p>
      <p className="text-[12px] text-text-tertiary mt-1">
        Spend appears here once runs or curator turns settle.
      </p>
    </div>
  )
}

function TotalPill({ value, loading }: { value: number; loading: boolean }) {
  return (
    <div className="text-right">
      <div className="text-[11px] text-text-tertiary">Total spend</div>
      <div className="text-2xl font-semibold text-text-primary tabular-nums">
        {loading ? '—' : fmtUSD(value)}
      </div>
    </div>
  )
}

function SectionShell({
  title,
  subtitle,
  right,
  children,
}: {
  title: string
  subtitle?: string
  right?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="mb-10">
      <div className="flex items-end justify-between gap-4 mb-4">
        <div className="min-w-0">
          <h2 className="text-lg font-semibold text-text-primary">{title}</h2>
          {subtitle && <p className="text-[12px] text-text-tertiary mt-0.5 truncate">{subtitle}</p>}
        </div>
        {right && <div className="shrink-0">{right}</div>}
      </div>
      {children}
    </section>
  )
}

// SectionBody picks the right state — error / loading skeleton / zero state /
// content — so every section handles an empty window the same way (a zero state,
// never a crash). `empty` is keyed off the total: total === 0 means no settled
// spend in the window.
function SectionBody({
  loading,
  error,
  empty,
  children,
}: {
  loading: boolean
  error: string | null
  empty: boolean
  children: React.ReactNode
}) {
  if (error) return <ErrorState msg={error} />
  if (loading) return <SkeletonGrid />
  if (empty) return <ZeroState />
  return <>{children}</>
}

// --- sections ---

function PersonalSection({ since, reload }: { since: string; reload: number }) {
  const { data, loading, error } = useUsageFetch<UsageMeResponse>(
    withWindow('/api/usage/me', since),
    reload,
  )
  const total = data?.total_cost_usd ?? 0

  return (
    <SectionShell
      title="Your usage"
      subtitle="Your delegated runs and curator turns."
      right={<TotalPill value={total} loading={loading} />}
    >
      <SectionBody loading={loading} error={error} empty={total === 0}>
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
          <Card title="By category">
            <CategoryDonut data={data?.by_category ?? []} />
          </Card>
          <Card title="By model">
            <CostBars data={modelBars(data?.by_model)} emptyLabel="No model spend" />
          </Card>
          <Card title="Over time">
            <SpendArea data={data?.by_day ?? []} />
          </Card>
        </div>
      </SectionBody>
    </SectionShell>
  )
}

function TeamSection({
  adminTeams,
  since,
  reload,
}: {
  adminTeams: TeamSummary[]
  since: string
  reload: number
}) {
  // The picked team, validated against the teams we currently admin every
  // render — if the held id is no longer one of them (org switch, a late
  // /api/teams load that changed the set), fall back to the first. Deriving the
  // effective id this way avoids storing an invalid selection (no effect needed).
  const [picked, setPicked] = useState('')
  const teamId =
    picked && adminTeams.some((t) => t.id === picked) ? picked : (adminTeams[0]?.id ?? '')

  const { data, loading, error } = useUsageFetch<UsageTeamResponse>(
    teamId ? withWindow(`/api/usage/teams/${teamId}`, since) : null,
    reload,
  )
  const total = data?.total_cost_usd ?? 0
  const teamName = data?.team_name || adminTeams.find((t) => t.id === teamId)?.name || 'your team'

  return (
    <SectionShell
      title="Team usage"
      subtitle={`Spend across ${teamName} — by member, rule, model, and category.`}
      right={
        <div className="flex items-center gap-4">
          {/* The switcher renders only at ≥2 admin teams; with one, the section
              still shows that team's breakdown (name is in the subtitle). */}
          <TeamSwitch value={teamId} onChange={setPicked} teams={adminTeams} />
          <TotalPill value={total} loading={loading} />
        </div>
      }
    >
      <SectionBody loading={loading} error={error} empty={total === 0}>
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          <Card title="By member">
            <CostBars data={userBars(data?.by_user)} emptyLabel="No member spend" />
          </Card>
          <Card title="By rule">
            <CostBars data={ruleBars(data?.by_rule)} emptyLabel="No automated runs" />
          </Card>
          <Card title="By category">
            <CategoryDonut data={data?.by_category ?? []} />
          </Card>
          <Card title="By model">
            <CostBars data={modelBars(data?.by_model)} emptyLabel="No model spend" />
          </Card>
          <Card title="Over time" className="md:col-span-2 lg:col-span-1">
            <SpendArea data={data?.by_day ?? []} />
          </Card>
        </div>
      </SectionBody>
    </SectionShell>
  )
}

function OrgSection({ since, reload }: { since: string; reload: number }) {
  const { data, loading, error } = useUsageFetch<UsageOrgResponse>(
    withWindow('/api/usage/org', since),
    reload,
  )
  const total = data?.total_cost_usd ?? 0

  return (
    <SectionShell
      title="Organization"
      subtitle="Every team, plus curator and system overhead across the org."
      right={<TotalPill value={total} loading={loading} />}
    >
      <SectionBody loading={loading} error={error} empty={total === 0}>
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          <Card title="By team">
            <CostBars data={teamBars(data?.by_team)} emptyLabel="No team spend" />
          </Card>
          <Card title="By user">
            <CostBars data={userBars(data?.by_user)} emptyLabel="No user spend" />
          </Card>
          <Card title="By category">
            <CategoryDonut data={data?.by_category ?? []} />
          </Card>
          <Card title="Org-level">
            <CostBars data={orgLevelBars(data?.org_level)} emptyLabel="No org-level spend" />
          </Card>
          <Card title="By model">
            <CostBars data={modelBars(data?.by_model)} emptyLabel="No model spend" />
          </Card>
          <Card title="Over time">
            <SpendArea data={data?.by_day ?? []} />
          </Card>
        </div>
      </SectionBody>
    </SectionShell>
  )
}

function RangePicker({ value, onChange }: { value: RangeKey; onChange: (r: RangeKey) => void }) {
  return (
    <div className="inline-flex rounded-lg border border-border-subtle bg-white/60 p-0.5">
      {RANGES.map((r) => (
        <button
          key={r.key}
          type="button"
          onClick={() => onChange(r.key)}
          className={`px-2.5 py-1 text-[12px] rounded-md transition-colors ${
            value === r.key
              ? 'bg-accent-soft text-accent font-medium'
              : 'text-text-tertiary hover:text-text-secondary'
          }`}
        >
          {r.label}
        </button>
      ))}
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
  // the personal and team views by design; without it, a local user's system
  // spend would be invisible. The backend agrees: RequireOrgAdminRole short-
  // circuits to allowed in local mode.
  const { isAdmin } = useOrgRole()
  const isLocal = useOptionalAuth() === null
  const showOrg = isAdmin || isLocal
  const { teams } = useTeams()
  const adminTeams = teams.filter((t) => t.role === 'admin')

  const [range, setRange] = useState<RangeKey>('month')
  const [reload, setReload] = useState(0)
  const since = sinceParam(range)

  return (
    <div className="max-w-6xl mx-auto">
      <div className="flex items-center justify-between gap-4 mb-8">
        <div>
          <h1 className="text-xl font-semibold text-text-primary">Usage</h1>
          <p className="text-[12px] text-text-tertiary mt-0.5">
            Settled LLM spend, in real dollars.
          </p>
        </div>
        <div className="flex items-center gap-3">
          <RangePicker value={range} onChange={setRange} />
          <button
            type="button"
            onClick={() => setReload((n) => n + 1)}
            className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
          >
            Refresh
          </button>
        </div>
      </div>

      <PersonalSection since={since} reload={reload} />
      {adminTeams.length > 0 && (
        <TeamSection adminTeams={adminTeams} since={since} reload={reload} />
      )}
      {showOrg && <OrgSection since={since} reload={reload} />}
    </div>
  )
}
