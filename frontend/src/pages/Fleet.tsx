import { Fragment, useCallback, useEffect, useId, useMemo, useState } from 'react'
import { Navigate } from 'react-router'
import { Area, AreaChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

import { useOptionalAuth } from '../contexts/AuthContext'
import { FeatureFleet, useEntitlements } from '../hooks/useEntitlements'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { averageCores, coreMinutes, cpuRateSeries, memSeries } from '../lib/sandboxSeries'
import type { SandboxPoint } from '../lib/sandboxSeries'
import type {
  FleetInstance,
  FleetInstances,
  FleetOverview,
  FleetBacklog,
  FleetSandboxClaim,
  FleetSandboxes,
  FleetSandboxSeries,
  FleetTimeseries,
  RunStatusValue,
} from '../types'
import { isActiveStatus } from '../lib/runStatus'

// Fleet — the sandbox-fleet administration console (TFAC-589). Operator-gated,
// EE-licensed (FeatureFleet). A DB-backed live view over the instances registry,
// the instance_stats telemetry samples, the runs timing columns, and llm_spend —
// no Prometheus dependency. Design mirrors the Usage "burn console": borderless
// bands, etched monospace labels, thin glowing meters. 10s poll (the
// factory-snapshot cadence), visibility-gated, holding last-good data so the
// panel never flashes.

const POLL_INTERVAL_MS = 10_000

type WindowKey = '24h' | '7d'
const WINDOWS: { key: WindowKey; label: string; hours: number }[] = [
  { key: '24h', label: '24H', hours: 24 },
  { key: '7d', label: '7D', hours: 168 },
]

// ---------- fetch hook ----------

interface Fetch<T> {
  data: T | null
  error: string | null
}

function useFleetFetch<T>(url: string | null): Fetch<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (signal: { cancelled: boolean }) => {
      if (!url) {
        setData(null)
        setError(null)
        return
      }
      // Hold last-good data across a refetch (no flash); only replace on success.
      try {
        const body = await apiJSON<T>(url)
        if (!signal.cancelled) {
          setData(body)
          setError(null)
        }
      } catch (err) {
        if (!signal.cancelled) setError(httpErrorMessage(err, 'Could not load the fleet data.'))
      }
    },
    [url],
  )

  useEffect(() => {
    const signal = { cancelled: false }
    // Fetch-on-mount/url-change: load owns its own setState calls; the effect
    // just kicks it. Same safe pattern AuthContext uses.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void load(signal)
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

// ---------- formatters ----------

const fmtInt = (n: number) => n.toLocaleString('en-US')
const fmtUSD = (n: number) =>
  n === 0
    ? '$0.00'
    : n < 0.01
      ? '<$0.01'
      : `$${n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`

function fmtMs(ms: number | undefined | null): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`
  const m = s / 60
  if (m < 60) return `${m.toFixed(m < 10 ? 1 : 0)}m`
  return `${(m / 60).toFixed(1)}h`
}

function fmtDuration(seconds: number): string {
  if (seconds < 1) return '0s'
  if (seconds < 60) return `${Math.round(seconds)}s`
  const m = seconds / 60
  if (m < 60) return `${m.toFixed(m < 10 ? 1 : 0)}m`
  const h = m / 60
  if (h < 24) return `${h.toFixed(1)}h`
  return `${(h / 24).toFixed(1)}d`
}

function fmtMem(mb: number | undefined | null): string {
  if (mb == null) return '—'
  if (mb < 1024) return `${Math.round(mb)}MB`
  return `${(mb / 1024).toFixed(1)}GB`
}

function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 8)}…${id.slice(-3)}` : id
}

// ---------- primitives ----------

function Band({
  label,
  aside,
  children,
}: {
  label: string
  aside?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section>
      <div className="mb-4 flex items-center gap-3">
        <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-text-tertiary/80">
          {label}
        </span>
        <span className="h-px flex-1 bg-border-subtle/70" />
        {aside}
      </div>
      {children}
    </section>
  )
}

function Instrument({
  label,
  aside,
  children,
}: {
  label: string
  aside?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div>
      <div className="mb-2.5 flex items-center gap-2">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
          {label}
        </span>
        <span className="h-px flex-1 bg-border-subtle/60" />
        {aside}
      </div>
      {children}
    </div>
  )
}

// Meter — a single-value gauge: a left-anchored gradient fading to nothing at
// value/max. No track, only light. A floor keeps a tiny nonzero value visible.
function Meter({
  value,
  max,
  color = 'var(--color-accent)',
}: {
  value: number
  max: number
  color?: string
}) {
  const pct = max <= 0 ? 0 : value <= 0 ? 0 : Math.min(100, Math.max(5, (value / max) * 100))
  return (
    <div className="relative h-2">
      <span
        aria-hidden
        className="absolute inset-0"
        style={{ background: `linear-gradient(90deg, ${color} 0%, transparent ${pct}%)` }}
      />
    </div>
  )
}

function Stat({
  value,
  unit,
  tone = 'text-text-primary',
}: {
  value: React.ReactNode
  unit?: string
  tone?: string
}) {
  return (
    <div className={`font-mono tabular-nums ${tone}`}>
      <span className="text-[22px] font-light leading-none">{value}</span>
      {unit && <span className="ml-1 text-[11px] text-text-tertiary">{unit}</span>}
    </div>
  )
}

type ChipTone = 'rust' | 'good' | 'attention' | 'problem' | 'neutral'
const CHIP_CLASS: Record<ChipTone, string> = {
  rust: 'border-accent/30 text-accent',
  good: 'border-claim/40 text-claim',
  attention: 'border-snooze/40 text-snooze',
  problem: 'border-dismiss/40 text-dismiss',
  neutral: 'border-border-subtle text-text-tertiary',
}

function Chip({ tone = 'neutral', children }: { tone?: ChipTone; children: React.ReactNode }) {
  return (
    <span
      className={`inline-flex items-center rounded-full border px-2 py-0.5 font-mono text-[9px] font-semibold uppercase tracking-[0.12em] ${CHIP_CLASS[tone]}`}
    >
      {children}
    </span>
  )
}

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

function WindowToggle({ value, onChange }: { value: WindowKey; onChange: (w: WindowKey) => void }) {
  return (
    <div className="flex items-center gap-4">
      {WINDOWS.map((wr) => {
        const active = wr.key === value
        return (
          <button
            key={wr.key}
            type="button"
            onClick={() => onChange(wr.key)}
            className={`relative font-mono text-[11px] tracking-[0.12em] transition-colors ${
              active ? 'text-accent' : 'text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {wr.label}
            {active && <span className="absolute -bottom-1.5 left-0 right-0 h-px bg-accent" />}
          </button>
        )
      })}
    </div>
  )
}

// ---------- timeseries aggregation ----------

interface SeriesPoint {
  t: number
  active: number
  queued: number
  cpu: number
  memGB: number
}

function aggregateSeries(ts: FleetTimeseries | null): SeriesPoint[] {
  if (!ts || ts.samples.length === 0) return []
  // Bucket by sample timestamp; sum run-scoped fields, average cpu, sum mem.
  const buckets = new Map<
    number,
    { active: number; queued: number; cpuSum: number; cpuN: number; mem: number }
  >()
  for (const s of ts.samples) {
    const t = new Date(s.at).getTime()
    const b = buckets.get(t) ?? { active: 0, queued: 0, cpuSum: 0, cpuN: 0, mem: 0 }
    if (s.active_runs != null) b.active += s.active_runs
    // queued_visible is the global backlog reported by each executor; take the
    // max so N executors reporting the same depth don't multiply it.
    if (s.queued_visible != null) b.queued = Math.max(b.queued, s.queued_visible)
    if (s.cpu_pct != null) {
      b.cpuSum += s.cpu_pct
      b.cpuN += 1
    }
    if (s.mem_available_mb != null) b.mem += s.mem_available_mb
    buckets.set(t, b)
  }
  return [...buckets.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([t, b]) => ({
      t,
      active: b.active,
      queued: b.queued,
      cpu: b.cpuN > 0 ? b.cpuSum / b.cpuN : 0,
      memGB: b.mem / 1024,
    }))
}

// trace projects one field of the whole-host aggregate onto Trace's {t, v}.
function trace(series: SeriesPoint[], field: Exclude<keyof SeriesPoint, 't'>): SandboxPoint[] {
  return series.map((p) => ({ t: p.t, v: p[field] }))
}

// Trace draws one time-ordered series of {t, v}. Callers project whichever
// field they want onto `v` rather than naming a key here: recharts types
// dataKey against its own TypedDataKey, so a generic point type would have to
// be cast at the chart anyway, and a fixed shape means there is no key to
// mistype.
function Trace({
  data,
  color,
  fmt,
  height = 'h-24',
}: {
  data: SandboxPoint[]
  color: string
  fmt: (v: number) => string
  height?: string
}) {
  const gid = useId().replace(/:/g, '')
  if (data.length === 0) {
    return (
      <p className="py-8 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
        no samples yet
      </p>
    )
  }
  return (
    <div className={`${height} w-full`} data-testid="trace">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 4, right: 2, bottom: 0, left: 2 }}>
          <defs>
            <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity={0.35} />
              <stop offset="100%" stopColor={color} stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="t" type="number" domain={['dataMin', 'dataMax']} hide />
          <YAxis hide domain={[0, 'dataMax']} />
          <Tooltip
            contentStyle={{
              background: 'var(--color-surface-raised)',
              border: '1px solid var(--color-border-glass)',
              borderRadius: 8,
              fontSize: 11,
              fontFamily: 'ui-monospace, monospace',
            }}
            labelFormatter={(t) => new Date(Number(t)).toLocaleString()}
            formatter={(v) => [fmt(Number(v)), '']}
          />
          <Area
            type="monotone"
            dataKey="v"
            stroke={color}
            strokeWidth={1.5}
            fill={`url(#${gid})`}
            isAnimationActive={false}
            dot={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

// ---------- sections ----------

function OverviewRow({ ov }: { ov: FleetOverview | null }) {
  if (!ov) return <SkeletonRow n={5} />
  const { totals: t, queue: q, runs: r } = ov
  return (
    <div className="grid grid-cols-2 gap-x-8 gap-y-7 md:grid-cols-3 lg:grid-cols-5">
      <Instrument
        label="Machines"
        aside={
          <span className="font-mono text-[9px] text-text-tertiary/60">
            {t.executors}ex · {t.control}ctl
          </span>
        }
      >
        <Stat value={fmtInt(t.instances)} unit="online" />
        {t.stale > 0 && <p className="mt-1 font-mono text-[10px] text-dismiss">{t.stale} stale</p>}
      </Instrument>

      <Instrument
        label="Capacity"
        aside={
          <span className="font-mono text-[9px] tabular-nums text-text-tertiary/60">
            {t.active_runs}/{t.capacity_max}
          </span>
        }
      >
        <Stat
          value={t.capacity_max > 0 ? `${Math.round((t.active_runs / t.capacity_max) * 100)}` : '—'}
          unit="% used"
        />
        <div className="mt-2">
          <Meter value={t.active_runs} max={Math.max(1, t.capacity_max)} />
        </div>
      </Instrument>

      <Instrument
        label="Queue"
        aside={t.draining > 0 ? <Chip tone="attention">{t.draining} draining</Chip> : undefined}
      >
        <Stat
          value={fmtInt(q.depth)}
          unit="waiting"
          tone={q.depth > 0 ? 'text-snooze' : 'text-text-primary'}
        />
        <p className="mt-1 font-mono text-[10px] text-text-tertiary">
          oldest {q.depth > 0 ? fmtDuration(q.oldest_wait_seconds) : '—'}
        </p>
      </Instrument>

      <Instrument
        label="Queue wait"
        aside={<span className="font-mono text-[9px] text-text-tertiary/60">p50 · p95</span>}
      >
        <Stat value={fmtMs(q.wait_p50_ms)} />
        <p className="mt-1 font-mono text-[10px] text-text-tertiary">p95 {fmtMs(q.wait_p95_ms)}</p>
      </Instrument>

      <Instrument
        label="Run duration"
        aside={<span className="font-mono text-[9px] text-text-tertiary/60">p50 · p95</span>}
      >
        <Stat value={fmtMs(r.duration_p50_ms)} />
        <p className="mt-1 font-mono text-[10px] text-text-tertiary">
          p95 {fmtMs(r.duration_p95_ms)}
        </p>
      </Instrument>
    </div>
  )
}

function RunsBand({ ov }: { ov: FleetOverview | null }) {
  if (!ov) return null
  const { runs: r, versions, spend } = ov
  const failRate = r.total > 0 ? (r.failed / r.total) * 100 : 0
  return (
    <div className="grid grid-cols-1 gap-x-10 gap-y-7 md:grid-cols-3">
      <Instrument label={`Runs · last ${r.window_hours}h`}>
        <div className="flex items-baseline gap-6">
          <div>
            <Stat value={fmtInt(r.total)} unit="total" />
          </div>
          <div className="font-mono text-[11px] tabular-nums">
            <span className="text-claim">{fmtInt(r.completed)} done</span>
            <span className="mx-2 text-text-tertiary/40">·</span>
            <span className={r.active > 0 ? 'text-delegate' : 'text-text-tertiary'}>
              {fmtInt(r.active)} active
            </span>
          </div>
        </div>
      </Instrument>

      <Instrument
        label="Failures"
        aside={
          <span
            className={`font-mono text-[9px] tabular-nums ${failRate > 5 ? 'text-dismiss' : 'text-text-tertiary/60'}`}
          >
            {failRate.toFixed(1)}%
          </span>
        }
      >
        <Stat
          value={fmtInt(r.failed)}
          unit="failed"
          tone={r.failed > 0 ? 'text-dismiss' : 'text-text-primary'}
        />
        {r.failure_kinds.length > 0 && (
          <div className="mt-2 flex flex-wrap gap-1.5">
            {r.failure_kinds.slice(0, 4).map((f) => (
              <Chip key={f.kind} tone="problem">
                {f.kind.replace(/_/g, ' ')} {f.count}
              </Chip>
            ))}
          </div>
        )}
      </Instrument>

      <Instrument
        label="Versions"
        aside={
          versions.length > 1 ? (
            <Chip tone="attention">skew</Chip>
          ) : (
            <Chip tone="good">aligned</Chip>
          )
        }
      >
        <div className="flex flex-wrap items-center gap-2">
          {versions.map((v) => (
            <span
              key={v.version}
              className="font-mono text-[11px] tabular-nums text-text-secondary"
            >
              {v.version || '—'}
              <span className="ml-1 text-text-tertiary/60">×{v.count}</span>
            </span>
          ))}
        </div>
        {spend && (
          <p className="mt-3 font-mono text-[10px] uppercase tracking-wider text-text-tertiary/70">
            spend {r.window_hours}h ·{' '}
            <span className="text-text-secondary">{fmtUSD(spend.total_usd)}</span>
          </p>
        )}
      </Instrument>
    </div>
  )
}

function MachineCard({
  inst,
  onDrain,
  busy,
  sandboxesOpen,
  onToggleSandboxes,
}: {
  inst: FleetInstance
  onDrain: (id: string, draining: boolean) => void
  busy: boolean
  sandboxesOpen: boolean
  onToggleSandboxes: () => void
}) {
  const isExecutor = inst.role !== 'control'
  const active = inst.active_runs ?? 0
  const max = inst.max_runs ?? 0
  const meterColor = inst.stale
    ? 'var(--color-dismiss)'
    : inst.dispatch_gated
      ? 'var(--color-snooze)'
      : 'var(--color-accent)'
  return (
    <div className="rounded-lg border border-border-subtle/70 bg-surface-raised/40 p-4 transition-colors hover:border-border-subtle">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <p className="truncate font-mono text-[12px] text-text-primary" title={inst.id}>
            {shortId(inst.id)}
          </p>
          <p className="mt-0.5 font-mono text-[9px] uppercase tracking-wider text-text-tertiary/70">
            {inst.role} · {inst.version || 'unknown'} · e{inst.boot_epoch}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap justify-end gap-1">
          {inst.stale && <Chip tone="problem">stale</Chip>}
          {inst.draining && <Chip tone="attention">draining</Chip>}
          {inst.dispatch_gated && <Chip tone="attention">gated</Chip>}
          {!inst.stale && !inst.draining && !inst.dispatch_gated && <Chip tone="good">live</Chip>}
        </div>
      </div>

      {isExecutor && max > 0 && (
        <div className="mt-4">
          <div className="mb-1.5 flex items-center justify-between font-mono text-[10px] tabular-nums text-text-tertiary">
            <span>runs</span>
            <span className="text-text-secondary">
              {active}/{max}
            </span>
          </div>
          <Meter value={active} max={max} color={meterColor} />
        </div>
      )}

      <div className="mt-4 grid grid-cols-3 gap-3 font-mono text-[10px] tabular-nums">
        <Readout label="cpu" value={inst.cpu_pct != null ? `${Math.round(inst.cpu_pct)}%` : '—'} />
        <Readout label="load" value={inst.load1 != null ? inst.load1.toFixed(2) : '—'} />
        <Readout label="mem free" value={fmtMem(inst.mem_available_mb)} />
        {isExecutor && (
          <Readout
            label="claims/m"
            value={inst.claims_last_sample != null ? String(inst.claims_last_sample) : '—'}
          />
        )}
        {isExecutor && <Readout label="spawn p50" value={fmtMs(inst.spawn_p50_ms)} />}
        <Readout
          label="heartbeat"
          value={`${Math.round(inst.heartbeat_age_seconds)}s`}
          tone={inst.stale ? 'text-dismiss' : undefined}
        />
      </div>

      <div className="mt-4 flex gap-2">
        {/* Offered on every instance, not just executors: a control pod
            expands to an empty table, which is the honest answer to "what did
            this box run" rather than a missing affordance. */}
        <button
          type="button"
          aria-expanded={sandboxesOpen}
          aria-label={`sandboxes on ${inst.id}`}
          onClick={onToggleSandboxes}
          className={`flex-1 rounded-md border py-1.5 font-mono text-[10px] uppercase tracking-[0.15em] transition-colors ${
            sandboxesOpen
              ? 'border-accent/40 text-accent'
              : 'border-border-subtle text-text-tertiary hover:border-accent/40 hover:text-accent'
          }`}
        >
          {sandboxesOpen ? 'hide sandboxes' : 'sandboxes'}
        </button>
        {isExecutor && (
          <button
            type="button"
            disabled={busy}
            onClick={() => onDrain(inst.id, !inst.draining)}
            className={`flex-1 rounded-md border py-1.5 font-mono text-[10px] uppercase tracking-[0.15em] transition-colors disabled:opacity-40 ${
              inst.draining
                ? 'border-claim/40 text-claim hover:bg-claim/10'
                : 'border-border-subtle text-text-tertiary hover:border-snooze/50 hover:text-snooze'
            }`}
          >
            {inst.draining ? 'resume claims' : 'drain'}
          </button>
        )}
      </div>
    </div>
  )
}

// ---------- per-sandbox breakdown ----------
//
// The operator answer to "which sandboxes are actually eating this box".
// Whole-host instance_stats cannot answer it: a busy neighbour process is
// indistinguishable there from a heavy run. These numbers are per-cgroup, read
// from the claim that paid for them.
//
// Collapsed by default and fetched only while open — the components below are
// unmounted when collapsed, so no request fires for a row nobody expanded.

// Identifiers are DISPLAY-ONLY: this console spans orgs, and an app deep link
// into another org's run view would not authorize for the operator viewing it.
// Copyable is the useful affordance until an operator-context run view exists.
function CopyId({ id, kind }: { id: string; kind: string }) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const handle = window.setTimeout(() => setCopied(false), 1200)
    return () => window.clearTimeout(handle)
  }, [copied])
  return (
    <button
      type="button"
      title={id}
      aria-label={`copy ${kind} id ${id}`}
      onClick={() => {
        // Optional-chained: a non-secure context (or a test DOM) has no
        // clipboard, and a failed copy must not break the table.
        void navigator.clipboard?.writeText(id)
        setCopied(true)
      }}
      className="text-text-tertiary transition-colors hover:text-text-secondary"
    >
      {copied ? 'copied' : shortId(id)}
    </button>
  )
}

// The chip tone for a sandbox claim's conversation status. A switch on a
// declared conversation status rather than a status-keyed record: the record
// carried two names the backend had already stopped emitting, and a record's
// keys are invisible to the vocabulary lint — a `case` arm is not.
function statusTone(status: RunStatusValue): ChipTone {
  if (isActiveStatus(status)) return 'rust'
  switch (status) {
    case 'completed':
      return 'good'
    case 'failed':
      return 'problem'
    case 'queued':
    case 'open':
      return 'attention'
    default:
      return 'neutral'
  }
}

function SandboxState({ claim }: { claim: FleetSandboxClaim }) {
  const label = claim.failure_kind
    ? `${claim.status ?? 'failed'} · ${claim.failure_kind.replace(/_/g, ' ')}`
    : (claim.status ?? '—')
  return (
    <span className="inline-flex items-center gap-1.5">
      <Chip tone={statusTone(claim.status ?? '')}>{label}</Chip>
      {claim.live && <Chip tone="rust">live</Chip>}
    </span>
  )
}

// SandboxSeries is the second expand level: one sandbox's sampled shape. It is
// fetched only when this component mounts, i.e. only when its row is open.
function SandboxSeries({ claim }: { claim: FleetSandboxClaim }) {
  const { data, error } = useFleetFetch<FleetSandboxSeries>(
    `/api/fleet/claims/${encodeURIComponent(claim.id)}/series`,
  )
  const mem = useMemo(() => memSeries(data?.samples ?? []), [data])
  const cpu = useMemo(() => cpuRateSeries(data?.samples ?? []), [data])

  if (!data) {
    return (
      <p className="py-4 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
        {error ?? 'loading series…'}
      </p>
    )
  }
  // A real claim with no samples is ordinary, not an error: a sub-minute run
  // never met a tick, and history predating the sampler never will.
  if (data.samples.length === 0) {
    return (
      <p className="py-4 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
        no samples for this sandbox
      </p>
    )
  }
  return (
    <div className="space-y-4 py-2">
      <div className="grid grid-cols-1 gap-x-8 gap-y-4 md:grid-cols-2">
        <Instrument label="Memory · sampled">
          <Trace data={mem} color="var(--color-claim)" fmt={(v) => fmtMem(v)} height="h-20" />
        </Instrument>
        <Instrument label="CPU · derived from cumulative">
          <Trace
            data={cpu}
            color="var(--color-accent)"
            fmt={(v) => `${v.toFixed(2)} cores`}
            height="h-20"
          />
        </Instrument>
      </div>
      {/* The two numbers disagree by design and must not be reconciled: the
          sampler misses sub-minute spikes the kernel high-watermark catches,
          so the peak column legitimately sits above the chart. */}
      <p className="font-mono text-[9px] leading-relaxed text-text-tertiary/60">
        chart is the sampled shape ({data.samples.length} samples). the peak and cpu columns are the
        teardown snapshot — kernel truth, and higher than the chart whenever a spike fell between
        ticks.
      </p>
    </div>
  )
}

function SandboxRow({ claim }: { claim: FleetSandboxClaim }) {
  const [open, setOpen] = useState(false)
  const cores = claim.cpu_usec != null ? averageCores(claim.cpu_usec, claim.duration_seconds) : null
  return (
    <tbody className="border-t border-border-subtle/40">
      <tr className="hover:bg-surface-raised/30">
        <td className="py-1.5 pr-2 align-top">
          <button
            type="button"
            aria-expanded={open}
            aria-label={`usage series for sandbox ${shortId(claim.id)}`}
            onClick={() => setOpen((o) => !o)}
            className="text-text-tertiary transition-colors hover:text-accent"
          >
            {open ? '▾' : '▸'}
          </button>
        </td>
        <td className="py-1.5 pr-3 align-top">
          <CopyId id={claim.conversation_id} kind="run" />
        </td>
        <td className="py-1.5 pr-3 align-top">
          <CopyId id={claim.org_id} kind="org" />
        </td>
        <td className="py-1.5 pr-3 align-top">
          <SandboxState claim={claim} />
        </td>
        <td className="py-1.5 pr-3 text-right align-top text-text-secondary">
          {fmtDuration(claim.duration_seconds)}
        </td>
        <td className="py-1.5 pr-3 text-right align-top text-text-secondary">
          {fmtMem(claim.peak_mem_mb)}
        </td>
        <td className="py-1.5 pr-3 text-right align-top text-text-secondary">
          {claim.cpu_usec != null ? `${coreMinutes(claim.cpu_usec).toFixed(2)}` : '—'}
        </td>
        <td className="py-1.5 text-right align-top text-text-secondary">
          {cores != null ? cores.toFixed(2) : '—'}
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={8} className="pb-3 pl-6 pr-2">
            <SandboxSeries claim={claim} />
          </td>
        </tr>
      )}
    </tbody>
  )
}

function SandboxBreakdown({ instanceId }: { instanceId: string }) {
  const { data, error } = useFleetFetch<FleetSandboxes>(
    `/api/fleet/instances/${encodeURIComponent(instanceId)}/sandboxes`,
  )
  return (
    <div className="rounded-lg border border-border-subtle/50 bg-surface-raised/20 p-4">
      <Instrument
        label={`Sandboxes · ${shortId(instanceId)}`}
        aside={
          data ? (
            <span className="font-mono text-[9px] tabular-nums text-text-tertiary/60">
              {data.sandboxes.length} most recent
            </span>
          ) : undefined
        }
      >
        {!data ? (
          <p className="py-4 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
            {error ?? 'loading sandboxes…'}
          </p>
        ) : data.sandboxes.length === 0 ? (
          // A control pod never claims, and an executor that hasn't yet is a
          // normal state — an empty table, never an error.
          <p className="py-4 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
            no sandboxes on this instance
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-left font-mono text-[10px] tabular-nums">
              <thead>
                <tr className="text-[8px] uppercase tracking-wider text-text-tertiary/60">
                  <th className="w-4 py-1 pr-2 font-normal" />
                  <th className="py-1 pr-3 font-normal">run</th>
                  <th className="py-1 pr-3 font-normal">org</th>
                  <th className="py-1 pr-3 font-normal">state</th>
                  <th className="py-1 pr-3 text-right font-normal">duration</th>
                  <th className="py-1 pr-3 text-right font-normal">peak mem</th>
                  <th className="py-1 pr-3 text-right font-normal">core-min</th>
                  <th className="py-1 text-right font-normal">avg cores</th>
                </tr>
              </thead>
              {data.sandboxes.map((s) => (
                <SandboxRow key={s.id} claim={s} />
              ))}
            </table>
          </div>
        )}
      </Instrument>
    </div>
  )
}

function Readout({
  label,
  value,
  tone = 'text-text-secondary',
}: {
  label: string
  value: string
  tone?: string
}) {
  return (
    <div>
      <p className="text-[8px] uppercase tracking-wider text-text-tertiary/60">{label}</p>
      <p className={`mt-0.5 ${tone}`}>{value}</p>
    </div>
  )
}

function QueueBand({ q }: { q: FleetBacklog | null }) {
  if (!q) return null
  if (q.depth === 0) {
    return (
      <p className="py-6 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
        queue empty
      </p>
    )
  }
  const maxCount = Math.max(...q.by_org.map((o) => o.count), 1)
  return (
    <div className="space-y-3">
      {q.by_org.slice(0, 8).map((o) => (
        <div key={o.org_id} className="flex items-center gap-3">
          <span
            className="w-40 shrink-0 truncate font-mono text-[10px] text-text-tertiary"
            title={o.org_id}
          >
            {shortId(o.org_id)}
          </span>
          <div className="flex-1">
            <Meter value={o.count} max={maxCount} color="var(--color-snooze)" />
          </div>
          <span className="w-24 shrink-0 text-right font-mono text-[10px] tabular-nums text-text-secondary">
            {o.count} · {fmtDuration(o.oldest_wait_seconds)}
          </span>
        </div>
      ))}
    </div>
  )
}

function SkeletonRow({ n }: { n: number }) {
  return (
    <div className="grid grid-cols-2 gap-x-8 gap-y-7 md:grid-cols-3 lg:grid-cols-5">
      {Array.from({ length: n }).map((_, i) => (
        <div key={i} className="animate-pulse">
          <div className="mb-3 h-2 w-16 rounded bg-border-subtle/60" />
          <div className="h-6 w-20 rounded bg-border-subtle/40" />
        </div>
      ))}
    </div>
  )
}

// ---------- page ----------

export default function Fleet() {
  const { has, loaded: entLoaded } = useEntitlements()
  const auth = useOptionalAuth()
  const operator = auth?.me?.is_operator ?? false

  const [win, setWin] = useState<WindowKey>('24h')
  const hours = WINDOWS.find((w) => w.key === win)?.hours ?? 24
  const [drainBusy, setDrainBusy] = useState<string | null>(null)
  // Bump to force a refetch after a drain toggle (append to the URL as a nonce).
  const [rev, setRev] = useState(0)
  // Which machines have their sandbox breakdown open. A set rather than a
  // single id so opening one box doesn't silently collapse another an operator
  // is mid-comparison with.
  const [openSandboxes, setOpenSandboxes] = useState<ReadonlySet<string>>(() => new Set())
  const toggleSandboxes = useCallback((id: string) => {
    setOpenSandboxes((prev) => {
      const next = new Set(prev)
      if (!next.delete(id)) next.add(id)
      return next
    })
  }, [])

  const gated = entLoaded && !(operator && has(FeatureFleet))

  const overview = useFleetFetch<FleetOverview>(gated ? null : `/api/fleet/overview?hours=${hours}`)
  const instances = useFleetFetch<FleetInstances>(gated ? null : `/api/fleet/instances?_r=${rev}`)
  const timeseries = useFleetFetch<FleetTimeseries>(
    gated ? null : `/api/fleet/timeseries?hours=${hours}`,
  )
  const queue = useFleetFetch<FleetBacklog>(gated ? null : `/api/fleet/backlog?_r=${rev}`)

  const series = useMemo(() => aggregateSeries(timeseries.data), [timeseries.data])

  const onDrain = useCallback(async (id: string, draining: boolean) => {
    setDrainBusy(id)
    try {
      await apiFetch(`/api/fleet/instances/${encodeURIComponent(id)}/drain`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ draining }),
      })
      setRev((r) => r + 1) // refetch instances to reflect the new state
    } catch {
      // Surface via the next poll's error state; keep the UI responsive.
    } finally {
      setDrainBusy(null)
    }
  }, [])

  // Non-operator / unlicensed: the console does not exist for them. The nav item
  // is already hidden; a direct navigation bounces home (non-disclosure) once
  // the entitlement probe has resolved (avoid a redirect flash before it does).
  if (gated) return <Navigate to="/" replace />

  const err = overview.error ?? instances.error ?? timeseries.error ?? queue.error

  return (
    <div className="mx-auto max-w-6xl">
      <div className="mb-8 flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-light tracking-tight text-text-primary">Fleet</h1>
          <p className="mt-1 font-mono text-[11px] tracking-[0.08em] text-text-tertiary">
            sandbox fleet · live capacity &amp; queue
          </p>
        </div>
        <WindowToggle value={win} onChange={setWin} />
      </div>

      {err && !overview.data && (
        <p className="mb-6 rounded-md border border-dismiss/30 bg-dismiss/5 px-4 py-3 font-mono text-[11px] text-dismiss">
          {err}
        </p>
      )}

      <ConsoleFrame>
        <div className="space-y-12">
          <OverviewRow ov={overview.data} />

          <Band label="Throughput">
            <RunsBand ov={overview.data} />
          </Band>

          <Band
            label="Machines"
            aside={
              instances.data ? (
                <span className="font-mono text-[9px] text-text-tertiary/60">
                  {instances.data.instances.length} registered
                </span>
              ) : undefined
            }
          >
            {instances.data ? (
              instances.data.instances.length === 0 ? (
                <p className="py-6 text-center font-mono text-[10px] uppercase tracking-wider text-text-tertiary/50">
                  no registered instances
                </p>
              ) : (
                <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
                  {instances.data.instances.map((inst) => (
                    <Fragment key={inst.id}>
                      <MachineCard
                        inst={inst}
                        onDrain={onDrain}
                        busy={drainBusy === inst.id}
                        sandboxesOpen={openSandboxes.has(inst.id)}
                        onToggleSandboxes={() => toggleSandboxes(inst.id)}
                      />
                      {/* The breakdown spans the grid so a wide table isn't
                          crushed into a third of the row; it is only mounted
                          while open, which is what keeps a collapsed row from
                          fetching anything. */}
                      {openSandboxes.has(inst.id) && (
                        <div className="md:col-span-2 lg:col-span-3">
                          <SandboxBreakdown instanceId={inst.id} />
                        </div>
                      )}
                    </Fragment>
                  ))}
                </div>
              )
            ) : (
              <SkeletonRow n={3} />
            )}
          </Band>

          <Band label="History">
            <div className="grid grid-cols-1 gap-x-10 gap-y-8 md:grid-cols-2">
              <Instrument label="Active runs">
                <Trace
                  data={trace(series, 'active')}
                  color="var(--color-delegate)"
                  fmt={(v) => `${Math.round(v)} runs`}
                />
              </Instrument>
              <Instrument label="Queue depth">
                <Trace
                  data={trace(series, 'queued')}
                  color="var(--color-snooze)"
                  fmt={(v) => `${Math.round(v)} waiting`}
                />
              </Instrument>
              <Instrument label="CPU (avg)">
                <Trace
                  data={trace(series, 'cpu')}
                  color="var(--color-accent)"
                  fmt={(v) => `${Math.round(v)}%`}
                />
              </Instrument>
              <Instrument label="Memory free (total)">
                <Trace
                  data={trace(series, 'memGB')}
                  color="var(--color-claim)"
                  fmt={(v) => `${v.toFixed(1)}GB`}
                />
              </Instrument>
            </div>
          </Band>

          <Band
            label="Queue by org"
            aside={
              queue.data ? (
                <span className="font-mono text-[9px] tabular-nums text-text-tertiary/60">
                  {queue.data.depth} waiting
                </span>
              ) : undefined
            }
          >
            <QueueBand q={queue.data} />
          </Band>
        </div>
      </ConsoleFrame>
    </div>
  )
}
