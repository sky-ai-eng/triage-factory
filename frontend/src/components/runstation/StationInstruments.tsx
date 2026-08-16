import { useMemo } from 'react'
import type { Message, Conversation } from '../../types'
import {
  completionGloss,
  formatDurationMs,
  formatElapsed,
  QUEUE_DWELL_VISIBLE_MS,
  queueDwellMs,
  parkReasonLabel,
  workStartedAt,
} from '../../lib/runStatus'
import { artifactSetKey } from '../../lib/approval'
import { compactNum, tint, type StationState } from './stationStyle'
import ArtifactList from '../ArtifactList'
import ActionList from '../ActionList'

interface Props {
  run: Conversation
  messages: Message[]
  state: StationState
  now: number
  /** Open an artifact's approval overlay (PR / review) — wired up the page to
   *  RunDetail's overlay state. */
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
  /** Re-pull the run projection after an in-place dismiss in the Artifacts
   *  list (the list refetches its own rows). */
  onArtifactResolved?: () => void
}

// TelemetryRail — the instruments etched into the machine's housing, flanking
// the screen. Unlike the dark screen, this is part of the warm housing: light,
// quiet, all monospace readouts and thin gauges. It carries only data the run
// actually has — no faked context meter (that arrives with P4's telemetry).
export function TelemetryRail({
  run,
  messages,
  state,
  now,
  onOpenArtifact,
  onArtifactResolved,
}: Props) {
  // The token rollups ride the run row (the run read SUMs them per
  // conversation), so the rail shows the same authoritative numbers the usage
  // dashboard does rather than re-summing the transcript — useRunDetail keeps
  // them advancing mid-engagement by folding each streamed row's usage on top.
  const inputTokens = run.input_tokens ?? 0
  const outputTokens = run.output_tokens ?? 0
  const cacheRead = run.cache_read_tokens ?? 0
  const cacheWrite = run.cache_creation_tokens ?? 0
  // The context scan does walk the transcript — it wants the last
  // usage-bearing row, not a sum — and the rail re-renders every second for
  // the elapsed readout (`now`), so memoize it on the message array.
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
        <TokenGauge input={inputTokens} output={outputTokens} light={state.light} />
        {(cacheRead > 0 || cacheWrite > 0) && (
          <div className="mt-2 flex items-center gap-3 font-mono text-[10px] tabular-nums text-text-tertiary/70">
            {cacheRead > 0 && <span>cache·r {compactNum(cacheRead)}</span>}
            {cacheWrite > 0 && <span>cache·w {compactNum(cacheWrite)}</span>}
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
        {dwellMs != null && (isQueued || dwellMs >= QUEUE_DWELL_VISIBLE_MS) && (
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
        {run.ParkReason && <Readout k="stop" v={parkReasonLabel(run.ParkReason)} />}
        {/* The stored token, plus what it meant for the task. On its own
            `continue` is an orchestration term the viewer has no way to read —
            and the one that decides whether anything runs after this. */}
        {run.Outcome && (
          <Readout k="outcome" v={run.Outcome} accent={state.light} note={completionGloss(run)} />
        )}
      </Section>

      {/* Artifacts — everything the run produced (branch / PR / review / issue /
          comment), with the run's unresolved rows sorted first and dismissable
          in place; PR/review rows open their approval overlays, the rest link
          out. Gated on artifact_count (free on the run) so a run that produced
          nothing skips both the section and its fetch — matching the board
          card's affordance, which hides at 0. */}
      {(run.artifact_count ?? 0) > 0 && (
        <Section label="Artifacts">
          <ArtifactList
            runId={run.ID}
            pendingArtifactIds={run.pending_artifact_ids}
            refreshKey={artifactSetKey(run)}
            onOpenApproval={onOpenArtifact}
            onResolved={onArtifactResolved}
          />
        </Section>
      )}

      {/* Actions — every external write this run made, artifact-bearing or not.
          Unlike Artifacts above, this section is NOT gated on a count: an empty
          Actions list is an answer ("this run touched nothing outside the box"),
          where a hidden one is indistinguishable from a surface that was never
          built — which is how the reply that started this went unnoticed.
          Refetched when the run's status or turn count moves, so a live run's
          list fills in without a request per streamed row. */}
      <Section label="Actions">
        <ActionList runId={run.ID} refreshKey={`${run.Status}:${run.NumTurns ?? 0}`} />
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

// A readout is a key/value line; `note` adds a plain-language second line
// under it, for a value whose stored spelling doesn't speak for itself.
function Readout({
  k,
  v,
  title,
  accent,
  note,
}: {
  k: string
  v: string
  title?: string
  accent?: string
  note?: string
}) {
  return (
    <div title={title}>
      <div className="flex items-baseline justify-between gap-3">
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
      {note && <p className="mt-0.5 text-[10.5px] leading-snug text-text-tertiary/80">{note}</p>}
    </div>
  )
}

// latestContextSize returns the prompt size (input + cache read + cache
// creation) of the most recent message carrying token usage — the size the
// model last processed. Trailing usage-less rows (tool results, the user's own
// message) are skipped; null when no message has usage yet (render nothing).
function latestContextSize(messages: Message[]): number | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i]
    const size = (m.input_tokens ?? 0) + (m.cache_read_tokens ?? 0) + (m.cache_creation_tokens ?? 0)
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
