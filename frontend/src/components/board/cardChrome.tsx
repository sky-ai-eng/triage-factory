import { motion, useReducedMotion } from 'motion/react'
import { Tooltip } from '../../ui/tooltip/Tooltip'
import type { Task } from '../../types'
import { eventDisplay, eventTone } from '../../lib/eventDisplay'
import { STEP_VAR, TONE_TEXT, TONE_VAR, type Glow, type StepState, type Tone } from './cardStyle'

// cardChrome — the shared visual chrome for the board's cards. Every export
// here is a component (Fast Refresh rule); the tokens + logic live in
// cardStyle.ts. TaskCard and AgentCard compose these.
//
// The idiom is a factory crate / HUD module: against the columns' pure
// emptiness, each card is a defined panel — a machined beveled edge, rust
// corner-reinforcement brackets (the column L-bracket DNA, now hugging the
// card), a recessed header "label plate" with a registration tick, and
// monospace readout data. Status is carried by light: a left-edge spine in the
// conversation's tone plus, for live work, a breathing glow. (The lane's own glow was
// retired — the light rides the work, not the column around it.)

// CardPlane is the crate body: a translucent frosted panel with a beveled
// machined edge (inner top highlight + bottom shadow), a faint hairline border
// for panel definition, rust corner brackets, the status spine (+ optional
// chain notches), and — when a conversation is live — a status glow.
export function CardPlane({
  glow,
  dim,
  dragging,
  children,
}: {
  glow?: Glow | null
  dim?: boolean
  dragging?: boolean
  children: React.ReactNode
}) {
  const reduce = !!useReducedMotion()
  return (
    <div
      className={`group/plane relative transition-transform duration-300 ${
        dim ? 'opacity-70' : ''
      } ${dragging ? 'scale-[1.015]' : ''}`}
    >
      {/* The crate body — flat frosted fill + a hairline border + the corner
          brackets. No resting shadow / bevel / sheen: those bloomed past the
          card and fought the column's bracket edges. A lift shadow returns only
          while dragging. No overflow clip, so the glow bloom spills past the
          panel; the header plate rounds its own top corners. */}
      <div
        className={`relative rounded-[5px] border border-line-1 bg-raised backdrop-blur-xl ${
          dragging ? 'shadow-[0_22px_55px_-20px_rgba(0,0,0,0.5)]' : ''
        }`}
      >
        {children}
      </div>

      {/* Status glow: a 1px inner ring + outward bloom in the conversation's tone.
          Breathing for a live conversation; steady for a state that wants attention.
          A wrapper-level overlay so the bloom spills past the unclipped panel. */}
      {glow && (
        <motion.div
          aria-hidden
          className="pointer-events-none absolute inset-0 rounded-[5px]"
          initial={false}
          animate={{
            opacity: glow.breathing && !reduce ? [0.3, 0.85, 0.3] : glow.breathing ? 0.6 : 0.5,
          }}
          transition={
            glow.breathing && !reduce
              ? { duration: 4.5, repeat: Infinity, ease: 'easeInOut' }
              : { duration: 0.4 }
          }
          style={{
            boxShadow: `inset 0 0 0 1px ${TONE_VAR[glow.tone]}, 0 0 42px -16px ${TONE_VAR[glow.tone]}`,
          }}
        />
      )}

      <CornerBrackets />
    </div>
  )
}

// CornerBrackets are the crate's corner reinforcements — short rust L-ticks
// hugging each corner (the column bracket, scaled down to the card). Faint at
// rest, they brighten when the card is hovered, so the panel reads as a
// targetable HUD module.
function CornerBrackets() {
  const base =
    'pointer-events-none absolute h-2.5 w-2.5 border-warm/35 transition-colors duration-200 group-hover/plane:border-warm/75'
  return (
    <>
      <span className={`${base} left-0 top-0 rounded-tl-[5px] border-l-[1.5px] border-t-[1.5px]`} />
      <span
        className={`${base} right-0 top-0 rounded-tr-[5px] border-r-[1.5px] border-t-[1.5px]`}
      />
      <span
        className={`${base} bottom-0 left-0 rounded-bl-[5px] border-b-[1.5px] border-l-[1.5px]`}
      />
      <span
        className={`${base} bottom-0 right-0 rounded-br-[5px] border-b-[1.5px] border-r-[1.5px]`}
      />
    </>
  )
}

// HudHeader is the recessed label plate at the top of a crate — a faint inset
// strip holding the source/event readout, closed by a hairline divider. Spans
// the full width (its own padding), so it reads as a plate rather than another
// padded row. When a chain `steps` array is passed, the divider becomes a
// segmented progress track (the chain's done/active/current/pending), so step
// progress lives in the plate edge instead of a separate widget; otherwise the
// divider gets a short rust registration tick at the left.
export function HudHeader({ children, steps }: { children: React.ReactNode; steps?: StepState[] }) {
  const hasChain = !!steps && steps.length > 1
  return (
    <div
      className="relative rounded-t-[5px] border-b border-line-1/80 px-4 py-2.5"
      style={{ background: 'linear-gradient(to bottom, rgba(0,0,0,0.022), transparent)' }}
    >
      {children}
      {hasChain ? (
        <ChainTrack steps={steps!} />
      ) : (
        // Registration tick: a short rust mark at the divider's left end — a
        // HUD readout detail anchoring the plate.
        <span
          aria-hidden
          className="absolute -bottom-px left-0 h-px w-7"
          style={{ background: 'var(--color-warm)', opacity: 0.55 }}
        />
      )}
    </div>
  )
}

// ChainTrack draws the blueprint chain as a segmented bar riding the plate's
// bottom edge — done segments solid, the active one pulsing, the current
// (queued-next) one accent, pending faint. Reads as a progress bar without any
// "step N of M" text.
function ChainTrack({ steps }: { steps: StepState[] }) {
  return (
    <div aria-hidden className="absolute inset-x-0 -bottom-[1.5px] flex gap-[3px] px-3">
      {steps.map((state, i) => (
        <span
          key={i}
          className={`h-[2px] flex-1 rounded-full ${state === 'active' ? 'animate-pulse' : ''}`}
          style={{
            background: STEP_VAR[state],
            opacity: state === 'pending' ? 0.3 : 1,
          }}
        />
      ))}
    </div>
  )
}

// EventTag is the detuned event-type label — uppercase tracked text in one of
// the four warm tones (no pastel pill). Keeps EventBadge's tooltip so the full
// description is a hover away.
export function EventTag({ eventType }: { eventType?: string }) {
  if (!eventType) return null
  const info = eventDisplay(eventType)
  const tone = eventTone(eventType)
  return (
    <Tooltip content={info.description} wrap>
      <span
        className={`cursor-default text-label font-semibold uppercase tracking-[0.09em] ${TONE_TEXT[tone]}`}
      >
        {info.label}
      </span>
    </Tooltip>
  )
}

// SourceTag is the source marker, detuned to a monospace uppercase glyph (no
// blue Jira pill) so it sits quietly in the warm field as a HUD readout.
export function SourceTag({ task }: { task: Task }) {
  const label =
    task.source === 'github'
      ? task.entity_kind === 'pr'
        ? 'PR'
        : 'GH'
      : task.source === 'slack'
        ? 'SLACK'
        : 'JIRA'
  return (
    <span className="font-mono text-label font-semibold uppercase tracking-[0.12em] text-ink-3/80">
      {label}
    </span>
  )
}

// AttentionRow is the canonical "this needs you" row — a toned kicker + a
// one-line message + a trailing verb with an arrow. Used for the derived
// approval surface (unresolved draft PRs / ready reviews — "Open PR" / "Review N
// items"); kept generic so any future "your move" moment looks the same.
export function AttentionRow({
  tone = 'attention',
  kicker,
  message,
  action,
  onClick,
  pulse = true,
}: {
  tone?: Tone
  kicker: string
  message?: string
  action: string
  onClick: () => void
  pulse?: boolean
}) {
  const reduce = !!useReducedMotion()
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={`${kicker}: ${action}`}
      className="flex w-full items-center justify-between gap-3 rounded-[4px] px-3 py-2 text-left transition-colors"
      style={{
        background: `color-mix(in srgb, ${TONE_VAR[tone]} 10%, transparent)`,
        boxShadow: `inset 0 0 0 1px color-mix(in srgb, ${TONE_VAR[tone]} 22%, transparent)`,
      }}
    >
      <span className="flex min-w-0 items-start gap-2">
        <motion.span
          aria-hidden
          className="mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full"
          style={{ background: TONE_VAR[tone] }}
          animate={pulse && !reduce ? { opacity: [1, 0.3, 1] } : { opacity: 1 }}
          transition={
            pulse && !reduce ? { duration: 1.8, repeat: Infinity, ease: 'easeInOut' } : undefined
          }
        />
        <span className="flex min-w-0 flex-col">
          <span className={`text-label font-semibold uppercase tracking-wider ${TONE_TEXT[tone]}`}>
            {kicker}
          </span>
          {message && <span className="truncate text-ui leading-snug text-ink-1">{message}</span>}
        </span>
      </span>
      <span className={`shrink-0 text-ui font-semibold ${TONE_TEXT[tone]}`} aria-hidden>
        {action} →
      </span>
    </button>
  )
}
