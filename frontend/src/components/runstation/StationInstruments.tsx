import type { AgentMessage, AgentRun } from '../../types'
import { formatDurationMs, formatElapsed } from '../../lib/runStatus'
import { compactNum, tokenTotals, tint, type StationState } from './stationStyle'

interface Props {
  run: AgentRun
  messages: AgentMessage[]
  state: StationState
  now: number
}

// TelemetryRail — the instruments etched into the machine's housing, flanking
// the screen. Unlike the dark screen, this is part of the warm housing: light,
// quiet, all monospace readouts and thin gauges. It carries only data the run
// actually has — no faked context meter (that arrives with P4's telemetry).
export function TelemetryRail({ run, messages, state, now }: Props) {
  const tok = tokenTotals(messages)
  const started = run.StartedAt ? new Date(run.StartedAt) : null
  const duration =
    run.DurationMs != null && run.DurationMs > 0
      ? formatDurationMs(run.DurationMs)
      : started
        ? formatElapsed(run.StartedAt, now)
        : null

  return (
    <aside className="hidden w-[256px] shrink-0 overflow-y-auto border-l border-border-subtle bg-black/[0.012] px-4 py-4 lg:block">
      {/* Output gauge — the work product, the headline readout. */}
      <Section label="Output">
        <TokenGauge input={tok.input} output={tok.output} light={state.light} />
        {(tok.cacheRead > 0 || tok.cacheWrite > 0) && (
          <div className="mt-2 flex items-center gap-3 font-mono text-[10px] tabular-nums text-text-tertiary/70">
            {tok.cacheRead > 0 && <span>cache·r {compactNum(tok.cacheRead)}</span>}
            {tok.cacheWrite > 0 && <span>cache·w {compactNum(tok.cacheWrite)}</span>}
          </div>
        )}
      </Section>

      {/* Run figures */}
      <Section label="Run">
        {run.NumTurns != null && run.NumTurns > 0 && <Readout k="turns" v={String(run.NumTurns)} />}
        {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
          <Readout k="cost" v={`$${run.TotalCostUSD.toFixed(run.TotalCostUSD < 1 ? 4 : 2)}`} />
        )}
        {run.Model && <Readout k="model" v={shortModel(run.Model)} title={run.Model} />}
        {duration && <Readout k={run.DurationMs != null ? 'elapsed' : 'running'} v={duration} />}
        {started && (
          <Readout k="started" v={clockStamp(started)} title={started.toLocaleString()} />
        )}
        {run.StopReason && <Readout k="stop" v={run.StopReason} />}
        {run.Outcome && <Readout k="outcome" v={run.Outcome} accent={state.light} />}
      </Section>

      {run.Status === 'completed' && run.ResultSummary && (
        <Section label="Summary" last>
          <p className="whitespace-pre-line text-[11.5px] leading-relaxed text-text-secondary">
            {run.ResultSummary}
          </p>
        </Section>
      )}

      {run.OutcomeReason && (
        <Section label="Reason" last>
          <p className="text-[11.5px] leading-relaxed text-text-secondary">{run.OutcomeReason}</p>
        </Section>
      )}
    </aside>
  )
}

function Section({
  label,
  children,
  last,
}: {
  label: string
  children: React.ReactNode
  last?: boolean
}) {
  return (
    <div className={last ? '' : 'mb-5'}>
      <div className="mb-2 flex items-center gap-2">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
          {label}
        </span>
        <span className="h-px flex-1 bg-border-subtle" />
      </div>
      <div className="space-y-1.5">{children}</div>
    </div>
  )
}

// TokenGauge — a split bar: input vs output, with the output (the agent's actual
// production) lit in the state tone. The headline numbers sit above.
function TokenGauge({ input, output, light }: { input: number; output: number; light: string }) {
  const total = input + output
  const outPct = total > 0 ? (output / total) * 100 : 0
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between font-mono text-[11px] tabular-nums">
        <span className="text-text-tertiary/80">
          <span className="text-text-tertiary/50">↓</span> {compactNum(input)}
        </span>
        <span className="font-semibold" style={{ color: light }}>
          <span className="opacity-60">↑</span> {compactNum(output)}
        </span>
      </div>
      <div className="flex h-1.5 overflow-hidden rounded-full bg-black/[0.06]">
        <span
          style={{
            width: `${100 - outPct}%`,
            background: 'var(--color-text-tertiary)',
            opacity: 0.4,
          }}
        />
        <span
          style={{
            width: `${outPct}%`,
            background: light,
            boxShadow: `0 0 8px ${tint(light, 70)}`,
          }}
        />
      </div>
    </div>
  )
}

function Readout({
  k,
  v,
  title,
  accent,
}: {
  k: string
  v: string
  title?: string
  accent?: string
}) {
  return (
    <div className="flex items-baseline justify-between gap-3" title={title}>
      <span className="shrink-0 font-mono text-[10px] uppercase tracking-[0.1em] text-text-tertiary/70">
        {k}
      </span>
      <span
        className="min-w-0 truncate text-right font-mono text-[11px] tabular-nums"
        style={{ color: accent ?? 'var(--color-text-primary)' }}
      >
        {v}
      </span>
    </div>
  )
}

// shortModel trims a verbose model id to its readable family/version, e.g.
// "claude-opus-4-8-20260101" → "opus-4-8".
function shortModel(model: string): string {
  const m = model.replace(/^claude-/, '').replace(/-\d{8}$/, '')
  return m.length > 22 ? m.slice(0, 21) + '…' : m
}

function clockStamp(d: Date): string {
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}
