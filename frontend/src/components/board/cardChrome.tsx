import * as Tooltip from '@radix-ui/react-tooltip'
import { motion, useReducedMotion } from 'motion/react'
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
// run's tone plus, for live work, a breathing glow. (The lane's own glow was
// retired — the light rides the work, not the column around it.)

// CardPlane is the crate body: a translucent frosted panel with a beveled
// machined edge (inner top highlight + bottom shadow), a faint hairline border
// for panel definition, rust corner brackets, the status spine (+ optional
// chain notches), and — when a run is live — a status glow.
export function CardPlane({
  tone = 'rust',
  steps,
  glow,
  dim,
  dragging,
  children,
}: {
  tone?: Tone
  steps?: StepState[]
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
      {/* The crate body. No overflow clip, so the glow bloom + spine notches
          spill past the panel; the header plate rounds its own top corners. */}
      <div
        className="relative rounded-[5px] border border-border-subtle bg-surface-overlay backdrop-blur-xl"
        style={{
          boxShadow: dragging
            ? '0 22px 55px -20px rgba(0,0,0,0.55), inset 0 1px 0 rgba(255,255,255,0.09), inset 0 -1px 0 rgba(0,0,0,0.16)'
            : '0 10px 28px -22px rgba(0,0,0,0.5), inset 0 1px 0 rgba(255,255,255,0.07), inset 0 -1px 0 rgba(0,0,0,0.12)',
        }}
      >
        {/* Top specular — the glass catches light along the header plate. */}
        <div
          aria-hidden
          className="pointer-events-none absolute inset-x-0 top-0 h-12 rounded-t-[5px]"
          style={{ background: 'linear-gradient(to bottom, rgba(255,255,255,0.06), transparent)' }}
        />
        {children}
      </div>

      {/* Status glow: a 1px inner ring + outward bloom in the run's tone.
          Breathing for a live run; steady for a state that wants attention.
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

      <Spine tone={tone} />
      {steps && steps.length > 0 && <SpineSteps steps={steps} />}
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
    'pointer-events-none absolute h-2.5 w-2.5 border-accent/35 transition-colors duration-200 group-hover/plane:border-accent/75'
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

// HudHeader is the recessed label plate at the top of a crate — a faint
// inset strip holding the source/event readout, closed by a hairline divider
// with a rust registration tick at the spine end. Spans the full width (its own
// padding), so it reads as a plate rather than another padded row.
export function HudHeader({ children }: { children: React.ReactNode }) {
  return (
    <div
      className="relative rounded-t-[5px] border-b border-border-subtle/80 px-4 py-2.5"
      style={{ background: 'linear-gradient(to bottom, rgba(0,0,0,0.022), transparent)' }}
    >
      {children}
      {/* Registration tick: a short rust mark where the divider meets the
          spine — a HUD readout detail, aligning the plate to the status rail. */}
      <span
        aria-hidden
        className="absolute -bottom-px left-0 h-px w-7"
        style={{ background: 'var(--color-accent)', opacity: 0.55 }}
      />
    </div>
  )
}

// Spine is the status line: a 2px bar riding the left border edge, faded to
// transparent at both ends so it never collides with the corners and reads as
// a guiding rail rather than a hard rule.
export function Spine({ tone, opacity = 0.95 }: { tone: Tone; opacity?: number }) {
  return (
    <div
      aria-hidden
      className="pointer-events-none absolute bottom-0 left-0 top-0 w-[2px]"
      style={{
        background: `linear-gradient(to bottom, transparent, ${TONE_VAR[tone]} 12%, ${TONE_VAR[tone]} 88%, transparent)`,
        opacity,
      }}
    />
  )
}

// SpineSteps overlays a chain run's steps as notches beside the spine — done,
// active (pulsing), failed, the current step (ringed), and pending — replacing
// the old horizontal circles-and-line rail. They sit on the inner rail beside
// the title, so "this card is step N of a chain" lives on the spine.
function SpineSteps({ steps }: { steps: StepState[] }) {
  const reduce = !!useReducedMotion()
  return (
    <div aria-hidden className="pointer-events-none absolute left-0 top-0 w-3">
      {steps.map((state, i) => {
        const top = 54 + i * 13
        const color = STEP_VAR[state]
        return (
          <span key={i} className="absolute" style={{ top, left: 3 }}>
            {state === 'active' && !reduce && (
              <motion.span
                className="absolute inset-0 rounded-full"
                style={{ boxShadow: `0 0 0 2px ${color}` }}
                animate={{ opacity: [0.6, 0, 0.6], scale: [1, 1.9, 1] }}
                transition={{ duration: 1.8, repeat: Infinity, ease: 'easeInOut' }}
              />
            )}
            <span
              className="block h-1.5 w-1.5 rounded-full"
              style={{
                background: state === 'current' ? 'transparent' : color,
                boxShadow: state === 'current' ? `inset 0 0 0 1.5px ${color}` : undefined,
                opacity: state === 'pending' ? 0.35 : 1,
              }}
            />
          </span>
        )
      })}
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
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span
            className={`cursor-default text-[10px] font-semibold uppercase tracking-[0.09em] ${TONE_TEXT[tone]}`}
          >
            {info.label}
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            side="top"
            sideOffset={6}
            className="z-[100] max-w-[240px] rounded-lg border border-border-glass bg-surface-raised px-3 py-2 text-[12px] leading-relaxed text-text-secondary shadow-lg shadow-black/[0.06] animate-in fade-in-0 zoom-in-95"
          >
            {info.description}
            <Tooltip.Arrow className="fill-surface-raised" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}

// SourceTag is the source marker, detuned to a monospace uppercase glyph (no
// blue Jira pill) so it sits quietly in the warm field as a HUD readout.
export function SourceTag({ task }: { task: Task }) {
  const isGitHub = task.source === 'github'
  const label = isGitHub ? (task.entity_kind === 'pr' ? 'PR' : 'GH') : 'JIRA'
  return (
    <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-text-tertiary/80">
      {label}
    </span>
  )
}

// AttentionRow is the canonical "this needs you" row — a toned kicker + a
// one-line message + a trailing verb with an arrow. Used for awaiting_input
// (Respond) and pending_approval (Review / Open PR), so every "your move"
// moment looks the same.
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
          <span className={`text-[10px] font-semibold uppercase tracking-wider ${TONE_TEXT[tone]}`}>
            {kicker}
          </span>
          {message && (
            <span className="truncate text-[12px] leading-snug text-text-primary">{message}</span>
          )}
        </span>
      </span>
      <span className={`shrink-0 text-[12px] font-semibold ${TONE_TEXT[tone]}`} aria-hidden>
        {action} →
      </span>
    </button>
  )
}
