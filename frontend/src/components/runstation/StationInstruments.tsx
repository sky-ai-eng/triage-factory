import { useMemo } from 'react'
import type { AgentMessage, AgentRun } from '../../types'
import { formatDurationMs, formatElapsed, queueDwellMs, workStartedAt } from '../../lib/runStatus'
import { compactNum, tokenTotals, tint, type StationState } from './stationStyle'
import ArtifactList from '../ArtifactList'

interface Props {
  run: AgentRun
  messages: AgentMessage[]
  state: StationState
  now: number
  /** Open an artifact's approval overlay (PR / review) — wired up the page to
   *  RunDetail's overlay state. */
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
}

// TelemetryRail — the instruments etched into the machine's housing, flanking
// the screen. Unlike the dark screen, this is part of the warm housing: light,
// quiet, all monospace readouts and thin gauges. It carries only data the run
// actually has — no faked context meter (that arrives with P4's telemetry).
export function TelemetryRail({ run, messages, state, now, onOpenArtifact }: Props) {
  // Both scans are O(all messages), and the rail re-renders every second for
  // the elapsed readout (`now`) — memoize so the walk only happens when the
  // transcript actually grew.
  const tok = useMemo(() => tokenTotals(messages), [messages])
  const contextUsed = useMemo(() => latestContextSize(messages), [messages])
  // Working time and queue dwell are separate gauges: elapsed/running tick
  // from the claim stamp (queue time never inflates them), the queued readout
  // carries the wait — live while the run is in line, settled once it ran.
  const isQueued = run.Status === 'queued'
  const workStart = workStartedAt(run)
  const started = !isQueued && workStart ? new Date(workStart) : null
  const duration =
    run.DurationMs != null && run.DurationMs > 0
      ? formatDurationMs(run.DurationMs)
      : workStart
        ? formatElapsed(workStart, now)
        : null
  const dwellMs = queueDwellMs(run, now)

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

      {/* Context — how full the model's window is, from the last prompt it
          processed. Real token data (last-known for a dormant/terminal run);
          the section is omitted entirely when no message carries usage yet, so
          the rail never shows a faked bar. */}
      {contextUsed != null && (
        <Section label="Context">
          <ContextGauge used={contextUsed} limit={contextLimit(run.Model)} light={state.light} />
        </Section>
      )}

      {/* Run figures */}
      <Section label="Run">
        {run.NumTurns != null && run.NumTurns > 0 && <Readout k="turns" v={String(run.NumTurns)} />}
        {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
          <Readout k="cost" v={`$${run.TotalCostUSD.toFixed(run.TotalCostUSD < 1 ? 4 : 2)}`} />
        )}
        {run.Model && <Readout k="model" v={shortModel(run.Model)} title={run.Model} />}
        {duration && (run.DurationMs != null || !isQueued) && (
          <Readout k={run.DurationMs != null ? 'elapsed' : 'running'} v={duration} />
        )}
        {dwellMs != null && (isQueued || dwellMs >= 1000) && (
          <Readout
            k="queued"
            v={formatDurationMs(dwellMs)}
            title={
              isQueued
                ? 'Waiting for a free run slot'
                : 'Time spent waiting in the queue before this run started'
            }
          />
        )}
        {started && (
          <Readout k="started" v={clockStamp(started)} title={started.toLocaleString()} />
        )}
        {run.StopReason && <Readout k="stop" v={run.StopReason} />}
        {run.Outcome && <Readout k="outcome" v={run.Outcome} accent={state.light} />}
      </Section>

      {/* Artifacts — everything the run produced (branch / PR / review / issue /
          comment); PR/review rows open their approval overlays, the rest link
          out (TFAC-470). Gated on artifact_count (free on the run) so a run
          that produced nothing skips both the section and its fetch — matching
          the board card's affordance, which hides at 0. */}
      {(run.artifact_count ?? 0) > 0 && (
        <Section label="Artifacts">
          <ArtifactList runId={run.ID} onOpenApproval={onOpenArtifact} />
        </Section>
      )}

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

// ContextGauge — a single fill bar for how much of the model's context window
// the last processed prompt occupied. The fill shifts to the dismiss tone as it
// crowds the window, so a long run reads "getting tight" at a glance.
function ContextGauge({ used, limit, light }: { used: number; limit: number; light: string }) {
  const pct = Math.min(100, (used / limit) * 100)
  const fill = pct >= 85 ? 'var(--color-dismiss)' : light
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between font-mono text-[11px] tabular-nums">
        <span className="text-text-tertiary/80">
          {compactNum(used)} / {compactNum(limit)}
        </span>
        <span className="font-semibold" style={{ color: fill }}>
          {Math.round(pct)}%
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-black/[0.06]">
        <span
          className="block h-full rounded-full"
          style={{ width: `${pct}%`, background: fill, boxShadow: `0 0 8px ${tint(fill, 70)}` }}
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

// latestContextSize returns the prompt size (input + cache read + cache
// creation) of the most recent message carrying token usage — the size the
// model last processed. Trailing usage-less rows (tool results, the user's own
// message) are skipped; null when no message has usage yet (render nothing).
function latestContextSize(messages: AgentMessage[]): number | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i]
    const size = (m.InputTokens ?? 0) + (m.CacheReadTokens ?? 0) + (m.CacheCreationTokens ?? 0)
    if (size > 0) return size
  }
  return null
}

// contextLimit maps a model id to its context-window size in tokens.
// Current as of 2026-06 — update when new families ship: Fable 5, Opus
// 4.6/4.7/4.8, and Sonnet 4.6 are 1M-context natively (no [1m] variant id
// needed); Haiku 4.5 and the pre-4.6 families (Sonnet/Opus 4.5 and earlier)
// are 200k (haiku has no clause below — it rides the fallback; if a future
// haiku ships at 1M, add `haiku-4-[6-9]` to the pattern). Unknown families
// fall back to 200k on purpose: for a pressure gauge the safe error is
// over-reporting fullness — a false alarm draws a look — never hiding real
// exhaustion behind a too-large denominator.
function contextLimit(model: string): number {
  const m = (model || '').toLowerCase()
  if (m.includes('[1m]') || /\b1m\b/.test(m)) return 1_000_000
  if (/fable|opus-4-[678]|sonnet-4-6/.test(m)) return 1_000_000
  return 200_000
}

// shortModel trims a verbose model id to its readable family/version, e.g.
// "claude-opus-4-8-20260101" → "opus-4-8". Also strips Bedrock cross-region /
// region-less inference-profile prefixes ("us.anthropic.", "anthropic.").
function shortModel(model: string): string {
  const m = model
    .replace(/^(?:[a-z]{2,4}\.)?anthropic\./, '')
    .replace(/^claude-/, '')
    .replace(/-\d{8}$/, '')
  return m.length > 22 ? m.slice(0, 21) + '…' : m
}

function clockStamp(d: Date): string {
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}
