import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { motion, useReducedMotion } from 'motion/react'
import {
  ArrowLeft,
  Check,
  ExternalLink,
  Pause,
  Send,
  ShieldQuestion,
  Square,
  X,
} from 'lucide-react'
import type { AgentMessage, AgentRun, Task } from '../../types'
import type { PendingPermission, PermissionDecisionInput } from '../../hooks/useRunDetail'
import {
  isActiveStatus,
  isFailedStatus,
  formatDurationMs,
  formatElapsed,
  isActiveRun,
} from '../../lib/runStatus'
import { EventTag, SourceTag } from '../board/cardChrome'
import { STEP_VAR, type StepState } from '../board/cardStyle'
import { stripWorktree } from '../../lib/worktree'
import ScreenTranscript from './ScreenTranscript'
import { TelemetryRail } from './StationInstruments'
import { liveHeat, stationState, tint } from './stationStyle'
import { useOrgHref } from '../../hooks/useOrgHref'

export interface StationActions {
  onBack: () => void
  onCancel?: () => void
  onRequeue?: () => void
  onReview?: () => void
  /** Steer the run with a free-form message (live process or `open` resume). */
  onMessage?: (text: string) => void
  /** Pause the current turn (run → open), leaving the process warm. */
  onInterrupt?: () => void
  /** Answer a pending tool-permission prompt. The promise settles when the
   *  resolve POST finishes — PermissionPrompt awaits it to hold its
   *  single-flight guard. */
  onResolvePermission?: (requestID: string, decision: PermissionDecisionInput) => Promise<void>
  interruptPending?: boolean
}

// A blueprint chain segment: its lifecycle state, plus the run page to open when
// the step has actually run (null for a not-yet-spawned step, or for the run
// you're already viewing).
interface ChainStep {
  state: StepState
  href: string | null
}

interface Props {
  run: AgentRun
  task: Task | null
  messages: AgentMessage[]
  now: number
  chainSteps?: AgentRun[] | null
  actions: StationActions
  /** Unanswered tool-permission prompts, head-first. The dock renders the head
   *  with priority and surfaces an "N more" affordance for the rest. */
  pendingPermissions?: PendingPermission[]
}

// RunStation — the full-screen agent run as a station on the factory line. The
// page is the machine's interface, seen head-on: a warm housing cradling the
// screen (the transcript), lit by a single state-colored emissive frame, with
// the work running hot through it — vent heat across the top, a conveyor idling
// at the intake edge. All flat; the depth is light, motion, and texture.
export default function RunStation({
  run,
  task,
  messages,
  now,
  chainSteps,
  actions,
  pendingPermissions,
}: Props) {
  const reduce = !!useReducedMotion()
  const orgHref = useOrgHref()
  const st = stationState(run)
  const active = isActiveRun(run)

  const lastMessageAt = useMemo(() => {
    if (messages.length === 0) return null
    const last = messages[messages.length - 1]
    return new Date(last.CreatedAt).getTime()
  }, [messages])
  const heat = st.live ? liveHeat(st.heat, lastMessageAt, now) : st.heat

  const elapsed =
    !active && run.DurationMs != null
      ? formatDurationMs(run.DurationMs)
      : formatElapsed(run.StartedAt, now)

  const chain = useMemo<ChainStep[]>(() => {
    if (!chainSteps || chainSteps.length <= 1) return []
    const currentStep = run.blueprint_step_index ?? undefined
    return chainSteps.map((s, i) => {
      // A synthetic placeholder (step not spawned yet) or the run you're already
      // viewing isn't a navigable trace.
      const navigable = !s.ID.startsWith('__pending-') && s.ID !== run.ID
      return {
        state: stepStateOf(s, i, run.ID, currentStep),
        href: navigable ? orgHref(`/runs/${s.ID}`) : null,
      }
    })
  }, [chainSteps, run.ID, run.blueprint_step_index, orgHref])

  return (
    <motion.div
      initial={reduce ? false : { opacity: 0, y: 8 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.28, ease: [0.22, 1, 0.36, 1] }}
      className="relative flex h-full min-h-[34rem] flex-col overflow-hidden rounded-[6px] border border-border-subtle bg-surface-overlay backdrop-blur-xl"
    >
      <StateFrame light={st.light} live={st.live} />
      <CornerBrackets light={st.light} />

      <LabelPlate
        task={task}
        state={st}
        elapsed={elapsed}
        active={active}
        chain={chain}
        onBack={actions.onBack}
      />

      {/* Body — conveyor intake gutter · the screen · the instrument rail. */}
      <div className="relative flex min-h-0 flex-1">
        <ConveyorLane speed={st.belt} />
        <div className="relative flex min-h-0 flex-1 flex-col p-3">
          <ScreenTranscript messages={messages} run={run} />
          <VentHeat heat={heat} light={st.light} live={st.live} />
        </div>
        <TelemetryRail run={run} messages={messages} state={st} now={now} />
      </div>

      <IntakeDock
        run={run}
        state={st}
        active={active}
        actions={actions}
        pending={pendingPermissions ?? []}
      />
    </motion.div>
  )
}

// ── State frame ────────────────────────────────────────────────────────────
// The single status light. An emissive inner ring + outward bloom in the run's
// tone, breathing while a turn executes, steady otherwise. The machine wears
// exactly one color so the board's "lit by state, not chrome" reads full-bleed.
function StateFrame({ light, live }: { light: string; live: boolean }) {
  return (
    <>
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 z-20 rounded-[6px]"
        style={{ boxShadow: `inset 0 0 0 1px ${tint(light, 55)}` }}
      />
      <div
        aria-hidden
        className={`pointer-events-none absolute inset-0 z-20 rounded-[6px] ${live ? 'hmi-anim' : ''}`}
        style={{
          boxShadow: `inset 0 0 0 1px ${light}, inset 0 0 28px -6px ${tint(light, 70)}, 0 0 46px -22px ${light}`,
          opacity: live ? undefined : 0.5,
          animation: live ? 'hmi-breathe 4.5s ease-in-out infinite' : undefined,
        }}
      />
    </>
  )
}

// CornerBrackets — the crate's reinforced corners, in the state tone (the
// column/card L-bracket DNA, scaled to the whole machine).
function CornerBrackets({ light }: { light: string }) {
  const base = 'pointer-events-none absolute z-20 h-4 w-4'
  const s = { borderColor: light }
  return (
    <>
      <span className={`${base} left-0 top-0 rounded-tl-[6px] border-l-2 border-t-2`} style={s} />
      <span className={`${base} right-0 top-0 rounded-tr-[6px] border-r-2 border-t-2`} style={s} />
      <span
        className={`${base} bottom-0 left-0 rounded-bl-[6px] border-b-2 border-l-2`}
        style={s}
      />
      <span
        className={`${base} bottom-0 right-0 rounded-br-[6px] border-b-2 border-r-2`}
        style={s}
      />
    </>
  )
}

// ── Label plate ────────────────────────────────────────────────────────────
// The recessed identity strip at the top of the machine: source/event tags +
// the etched task title on the left, the state readout + elapsed + view toggle
// on the right, the chain progress riding the bottom edge.
function LabelPlate({
  task,
  state,
  elapsed,
  active,
  chain,
  onBack,
}: {
  task: Task | null
  state: ReturnType<typeof stationState>
  elapsed: string
  active: boolean
  chain: ChainStep[]
  onBack: () => void
}) {
  const hasChain = chain.length > 1
  return (
    <div
      className="relative z-10 shrink-0 px-5 pb-3.5 pt-3"
      style={{ background: 'linear-gradient(to bottom, rgba(0,0,0,0.03), transparent)' }}
    >
      {/* Tags + controls */}
      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <button
            type="button"
            onClick={onBack}
            aria-label="Back to floor"
            title="Back to floor (Esc)"
            className="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[4px] text-text-tertiary transition-colors hover:bg-black/[0.04] hover:text-text-primary"
          >
            <ArrowLeft size={15} />
          </button>
          {task && <SourceTag task={task} />}
          {task && <EventTag eventType={task.event_type} />}
          {task?.source_id && (
            <span className="truncate font-mono text-[11px] text-text-tertiary/80">
              {task.source_id}
            </span>
          )}
        </div>

        <div className="flex shrink-0 items-center gap-3">
          <StateReadout state={state} elapsed={elapsed} active={active} />
          {task?.source_url && (
            <>
              <PlateDivider />
              <a
                href={task.source_url}
                target="_blank"
                rel="noopener noreferrer"
                title="Open source"
                className="inline-flex items-center gap-1 text-[11px] font-semibold text-accent transition-colors hover:text-accent/70"
              >
                Open
                <ExternalLink size={11} />
              </a>
            </>
          )}
        </div>
      </div>

      {/* Etched title — a registration tick, then the task name in heavy type. */}
      <div className="mt-2 flex items-baseline gap-2.5">
        <span
          aria-hidden
          className="mt-1 inline-block h-3 w-[3px] shrink-0 rounded-full"
          style={{ background: state.light, boxShadow: `0 0 8px ${state.light}` }}
        />
        <h1 className="truncate text-[20px] font-semibold leading-tight tracking-tight text-text-primary">
          {task?.title || 'Untitled run'}
        </h1>
      </div>

      {hasChain && <ChainTrack steps={chain} />}

      {/* The plate's closing hairline. */}
      <span aria-hidden className="absolute inset-x-0 bottom-0 h-px bg-border-subtle" />
      <span
        aria-hidden
        className="absolute bottom-0 left-5 h-px w-10"
        style={{ background: state.light, opacity: 0.6 }}
      />
    </div>
  )
}

function StateReadout({
  state,
  elapsed,
  active,
}: {
  state: ReturnType<typeof stationState>
  elapsed: string
  active: boolean
}) {
  return (
    <span className="inline-flex items-center gap-2">
      <span
        className={`inline-block h-2 w-2 rounded-full ${state.live ? 'hmi-anim' : ''}`}
        style={{
          background: state.light,
          boxShadow: `0 0 10px ${state.light}`,
          animation: state.live ? 'hmi-core 1.6s ease-in-out infinite' : undefined,
        }}
      />
      <span
        className="font-mono text-[11px] font-semibold uppercase tracking-[0.16em]"
        style={{ color: state.light }}
      >
        {state.label}
      </span>
      <span className="font-mono text-[11px] tabular-nums text-text-tertiary/80">{elapsed}</span>
      {active && <span className="sr-only">live</span>}
    </span>
  )
}

function PlateDivider() {
  return <span aria-hidden className="h-3.5 w-px shrink-0 bg-text-tertiary/20" />
}

// ChainTrack — the blueprint chain as a segmented bar riding the plate's bottom
// edge (done/active/current/pending). Each already-run step links to that step's
// full trace (/runs/{id}); the current step and not-yet-spawned steps aren't
// navigable. A generous invisible hit area sits around each 3px bar. No "step N
// of M" text.
function ChainTrack({ steps }: { steps: ChainStep[] }) {
  return (
    <div className="mt-2.5 flex items-center gap-1">
      {steps.map((s, i) => {
        const bar = (
          <span
            className={`block h-[3px] w-full rounded-full transition-[filter] ${
              s.state === 'active' ? 'hmi-anim' : ''
            } ${s.href ? 'group-hover/step:brightness-125' : ''}`}
            style={{
              background: STEP_VAR[s.state],
              opacity: s.state === 'pending' ? 0.3 : 1,
              animation: s.state === 'active' ? 'hmi-breathe 1.8s ease-in-out infinite' : undefined,
            }}
          />
        )
        return s.href ? (
          <Link
            key={i}
            to={s.href}
            className="group/step flex-1 py-1.5"
            title="Open this step's run"
            aria-label={`Open blueprint step ${i + 1}`}
          >
            {bar}
          </Link>
        ) : (
          <span key={i} aria-hidden className="flex-1 py-1.5">
            {bar}
          </span>
        )
      })}
    </div>
  )
}

// ── Conveyor lane ──────────────────────────────────────────────────────────
// A thin chevron belt running down the machine's intake edge — ambient transit.
// Speed tracks the run state (fast while working, idling while open, parked when
// settled). Motion is the point, not literal item counts.
function ConveyorLane({ speed }: { speed: number }) {
  const moving = speed > 0.01
  const duration = moving ? Math.max(0.6, 2.4 / speed) : 0
  return (
    <div
      aria-hidden
      className="relative w-3 shrink-0 overflow-hidden border-r border-border-subtle/70"
      style={{ background: 'rgba(0,0,0,0.025)' }}
    >
      <div
        className={`absolute inset-0 ${moving ? 'hmi-anim' : ''}`}
        style={{
          backgroundImage:
            'repeating-linear-gradient(180deg, transparent 0 12px, color-mix(in srgb, var(--hmi-belt) 55%, transparent) 12px 14px, transparent 14px 17px, color-mix(in srgb, var(--hmi-belt) 30%, transparent) 17px 18px)',
          animation: moving ? `hmi-belt ${duration}s linear infinite` : undefined,
        }}
      />
    </div>
  )
}

// ── Vent heat ──────────────────────────────────────────────────────────────
// The warm glow across the top of the screen — the machine venting hot as it
// works. Intensity tracks activity (flaring as the agent emits); it breathes
// while live, sits as a faint ember when settled, goes cold on failure.
function VentHeat({ heat, light, live }: { heat: number; light: string; live: boolean }) {
  if (heat <= 0.001) return null
  return (
    <div
      aria-hidden
      className={`pointer-events-none absolute inset-x-3 top-3 z-10 h-24 origin-top ${live ? 'hmi-anim' : ''}`}
      style={{
        background: `radial-gradient(120% 100% at 50% 0%, ${tint('var(--hmi-vent)', Math.round(heat * 42))}, transparent 72%)`,
        opacity: 0.5 + heat * 0.5,
        animation: live ? 'hmi-heat 3.4s ease-in-out infinite' : undefined,
        // A faint state-tinted core under the orange keeps failure red, not warm.
        boxShadow: live ? `inset 0 20px 30px -24px ${light}` : undefined,
      }}
    />
  )
}

// ── Intake dock ────────────────────────────────────────────────────────────
// The station's intake port at the base — the machine's control panel and the
// steering surface. Three stacked regions: a permission prompt (priority, when
// the turn is parked on a tool approval), the honest run status + contextual
// actions, and the steering composer (for a live or `open` run). The composer is
// suppressed while a prompt is up — you answer the prompt first.
function IntakeDock({
  run,
  state,
  active,
  actions,
  pending,
}: {
  run: AgentRun
  state: ReturnType<typeof stationState>
  active: boolean
  actions: StationActions
  pending: PendingPermission[]
}) {
  const isPending = run.Status === 'pending_approval'
  const isTerminal =
    run.Status === 'failed' ||
    run.Status === 'cancelled' ||
    run.Status === 'task_unsolvable' ||
    run.Status === 'completed'
  // A run takes steering when a turn is executing or it's idle-resumable.
  const steerable = active || run.Status === 'open'
  const hasPrompt = pending.length > 0

  const sublabel = isPending
    ? run.pending_kind === 'pr'
      ? 'a pull request is staged for your approval'
      : 'a review is staged for your approval'
    : active
      ? 'agent is executing — streaming live'
      : run.Status === 'open'
        ? 'open — idle, resumable'
        : run.Status === 'completed'
          ? 'work complete'
          : run.Status === 'failed'
            ? 'run failed'
            : run.Status === 'cancelled'
              ? 'run cancelled'
              : run.Status === 'task_unsolvable'
                ? 'agent could not finish'
                : run.Status

  return (
    <div className="relative z-10 shrink-0 border-t border-border-subtle px-4 py-2.5">
      {active && <StreamShimmer light={state.light} />}

      {/* Permission prompt — priority: it's blocking the agent's turn. */}
      {hasPrompt && (
        <PermissionPrompt
          key={pending[0].request_id}
          prompt={pending[0]}
          remaining={pending.length - 1}
          worktree={run.WorktreePath}
          onResolve={actions.onResolvePermission}
        />
      )}

      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <IntakePort light={state.light} />
          <span
            className={`inline-block h-1.5 w-1.5 shrink-0 rounded-full ${state.live ? 'hmi-anim' : ''}`}
            style={{
              background: state.light,
              boxShadow: `0 0 8px ${state.light}`,
              animation: state.live ? 'hmi-core 1.6s ease-in-out infinite' : undefined,
            }}
          />
          <span
            className="shrink-0 font-mono text-[10px] font-semibold uppercase leading-none tracking-[0.14em]"
            style={{ color: state.light }}
          >
            {state.label}
          </span>
          <span className="truncate text-[12px] leading-none text-text-tertiary">{sublabel}</span>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          {isPending && actions.onReview && (
            <DockButton tone={state.light} solid onClick={actions.onReview}>
              {run.pending_kind === 'pr' ? 'Open PR' : 'Review'} →
            </DockButton>
          )}
          {/* Interrupt pauses the turn (→ open); distinct from Cancel's abandon. */}
          {active && actions.onInterrupt && (
            <DockButton
              tone="var(--hmi-cyan)"
              onClick={actions.onInterrupt}
              disabled={actions.interruptPending}
              icon={<Pause size={11} />}
            >
              {actions.interruptPending ? 'Pausing…' : 'Pause'}
            </DockButton>
          )}
          {active && actions.onCancel && (
            <DockButton
              tone="var(--color-dismiss)"
              onClick={actions.onCancel}
              icon={<Square size={11} />}
            >
              Cancel
            </DockButton>
          )}
          {(isTerminal || isPending) && actions.onRequeue && (
            <DockButton tone="var(--color-text-tertiary)" onClick={actions.onRequeue}>
              Return to queue
            </DockButton>
          )}
        </div>
      </div>

      {/* Steering composer — live + `open` runs, hidden behind a pending prompt. */}
      {steerable && !hasPrompt && actions.onMessage && (
        <DockComposer
          state={state}
          placeholder={active ? 'Steer the agent…' : 'Message to resume…'}
          onSend={actions.onMessage}
        />
      )}
    </div>
  )
}

// PermissionPrompt — the head of the approval queue, rendered with priority
// because it's parking the agent's turn. Tool name + a compact input summary,
// Deny / Allow, and an "N more" count when parallel tool calls stacked up.
function PermissionPrompt({
  prompt,
  remaining,
  worktree,
  onResolve,
}: {
  prompt: PendingPermission
  remaining: number
  worktree?: string
  onResolve?: (requestID: string, decision: PermissionDecisionInput) => Promise<void>
}) {
  const tone = 'var(--color-snooze)' // amber — a blocking "your move"
  // Single-flight: the first click wins. Without it a quick Deny→Allow puts
  // two resolves in flight and the broker honors whichever lands first —
  // non-deterministic from the user's view. Resolving resets on settle so a
  // transient failure (prompt stays up) remains answerable.
  const [resolving, setResolving] = useState(false)
  const resolve = async (behavior: 'allow' | 'deny') => {
    if (resolving || !onResolve) return
    setResolving(true)
    try {
      await onResolve(prompt.request_id, { behavior })
    } finally {
      setResolving(false)
    }
  }
  // Worktree-relative paths, same as the transcript — but always the real
  // command/input, never the agent's description: this is a security
  // decision, so the user approves what will run, not what the agent says.
  const summary = stripWorktree(summarizePermissionInput(prompt.tool_name, prompt.input), worktree)
  return (
    <div
      className="mb-2.5 flex items-center gap-2.5 rounded-[5px] px-3 py-2"
      style={{ background: tint(tone, 10), boxShadow: `inset 0 0 0 1px ${tint(tone, 32)}` }}
    >
      <ShieldQuestion size={15} className="shrink-0" style={{ color: tone }} />
      <span
        className="shrink-0 font-mono text-[10px] font-semibold uppercase leading-none tracking-[0.12em]"
        style={{ color: tone }}
      >
        Allow {prompt.tool_name}?
      </span>
      {summary && (
        <span
          className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-text-secondary"
          title={summary}
        >
          {summary}
        </span>
      )}
      {!summary && <span className="min-w-0 flex-1" />}
      {remaining > 0 && (
        <span
          className="shrink-0 font-mono text-[10px] tabular-nums text-text-tertiary/80"
          title={`${remaining} more prompt${remaining === 1 ? '' : 's'} queued`}
        >
          +{remaining} more
        </span>
      )}
      <div className="flex shrink-0 items-center gap-2">
        <DockButton
          tone="var(--color-dismiss)"
          onClick={() => void resolve('deny')}
          disabled={resolving}
          icon={<X size={11} />}
        >
          Deny
        </DockButton>
        <DockButton
          tone="var(--color-claim)"
          solid
          onClick={() => void resolve('allow')}
          disabled={resolving}
          icon={<Check size={11} />}
        >
          Allow
        </DockButton>
      </div>
    </div>
  )
}

// DockComposer — the steering input. An auto-resizing textarea (Enter sends,
// Shift+Enter inserts a newline), with a send key lit in the run's state tone.
// State is local; the sent text round-trips back as an agent_message, so there's
// no optimistic insert here.
function DockComposer({
  state,
  placeholder,
  onSend,
}: {
  state: ReturnType<typeof stationState>
  placeholder: string
  onSend: (text: string) => void
}) {
  const [draft, setDraft] = useState('')
  const ref = useRef<HTMLTextAreaElement>(null)

  // Measure on every change, clamp to ~5 rows so a long draft can't swallow the
  // screen above the dock.
  useEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 120) + 'px'
  }, [draft])

  const submit = () => {
    const text = draft.trim()
    if (!text) return
    onSend(text)
    setDraft('')
    ref.current?.focus()
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      submit()
    }
  }

  return (
    <div className="mt-2.5 flex items-end gap-2 rounded-[5px] border border-border-subtle bg-black/[0.02] px-2.5 py-1.5 transition-colors focus-within:border-[color:var(--hmi-cyan)]">
      <textarea
        ref={ref}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={onKeyDown}
        rows={1}
        placeholder={placeholder}
        aria-label={placeholder.replace(/…$/, '')}
        className="flex-1 resize-none bg-transparent py-1 text-[13px] leading-relaxed text-text-primary placeholder:text-text-tertiary/70 focus:outline-none"
      />
      <button
        type="button"
        onClick={submit}
        disabled={!draft.trim()}
        aria-label="Send message"
        className="mb-0.5 inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[4px] transition-opacity disabled:opacity-30"
        style={{
          color: 'var(--hmi-screen)',
          background: state.light,
          boxShadow: `0 0 14px -5px ${state.light}`,
        }}
      >
        <Send size={13} />
      </button>
    </div>
  )
}

// summarizePermissionInput renders a compact one-line preview of a tool call's
// input for the permission prompt — the command for Bash, the path for file
// tools, the pattern for search, else the first short string field or a trimmed
// JSON blob. Collapsed whitespace, capped length.
function summarizePermissionInput(tool: string, input: Record<string, unknown>): string {
  const str = (k: string) => (typeof input[k] === 'string' ? (input[k] as string) : '')
  let s = ''
  if (tool === 'Bash') s = str('command')
  else if (tool === 'Read' || tool === 'Write' || tool === 'Edit') s = str('file_path')
  else if (tool === 'Glob' || tool === 'Grep') s = str('pattern')
  if (!s) {
    for (const v of Object.values(input)) {
      if (typeof v === 'string' && v) {
        s = v
        break
      }
    }
  }
  if (!s) {
    try {
      s = JSON.stringify(input)
    } catch {
      s = ''
    }
  }
  s = s.replace(/\s+/g, ' ').trim()
  return s.length > 80 ? s.slice(0, 79) + '…' : s
}

// IntakePort — a tiny socket glyph marking this as the machine's input.
function IntakePort({ light }: { light: string }) {
  return (
    <span
      aria-hidden
      className="inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-[2px] border"
      style={{ borderColor: tint(light, 45) }}
    >
      <span className="h-1.5 w-1.5 rounded-[1px]" style={{ background: tint(light, 60) }} />
    </span>
  )
}

// StreamShimmer — a thin indeterminate sweep along the dock's top edge while the
// agent streams, so the control panel reads as live without any spinner.
function StreamShimmer({ light }: { light: string }) {
  return (
    <span
      aria-hidden
      className="hmi-anim pointer-events-none absolute inset-x-0 top-0 h-px overflow-hidden"
      style={{
        background: `linear-gradient(90deg, transparent, ${light} 50%, transparent)`,
        backgroundSize: '40% 100%',
        animation: 'hmi-sweep 1.6s linear infinite',
        backgroundRepeat: 'no-repeat',
      }}
    />
  )
}

function DockButton({
  children,
  tone,
  onClick,
  disabled,
  solid,
  icon,
}: {
  children: React.ReactNode
  tone: string
  onClick: () => void
  disabled?: boolean
  solid?: boolean
  icon?: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded-[4px] px-2.5 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.1em] transition-colors disabled:cursor-wait disabled:opacity-60"
      style={
        solid
          ? { color: 'var(--hmi-screen)', background: tone, boxShadow: `0 0 16px -4px ${tone}` }
          : {
              color: tone,
              background: tint(tone, 12),
              boxShadow: `inset 0 0 0 1px ${tint(tone, 26)}`,
            }
      }
    >
      {icon}
      {children}
    </button>
  )
}

// stepStateOf maps one chain step to its progress-track state (the same mapping
// AgentCard uses, so the station's chain bar matches the board card's).
function stepStateOf(
  s: AgentRun,
  i: number,
  currentRunID: string,
  currentStepIndex?: number,
): StepState {
  if (isActiveStatus(s.Status)) return 'active'
  if (s.Status === 'completed') return 'done'
  if (isFailedStatus(s.Status)) return 'failed'
  if (s.ID === currentRunID || i === currentStepIndex) return 'current'
  return 'pending'
}
