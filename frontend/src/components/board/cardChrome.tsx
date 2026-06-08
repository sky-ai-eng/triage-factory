import * as Tooltip from '@radix-ui/react-tooltip'
import { motion, useReducedMotion } from 'motion/react'
import type { Task } from '../../types'
import { eventDisplay, eventTone } from '../../lib/eventDisplay'
import { STEP_VAR, TONE_TEXT, TONE_VAR, type Glow, type StepState, type Tone } from './cardStyle'

// cardChrome — the shared visual chrome for the board's lit-plane cards. Every
// export here is a component (Fast Refresh rule); the tokens + logic live in
// cardStyle.ts. TaskCard and AgentCard compose these.

// CardPlane is the frosted borderless plane: a translucent glass fill with a
// hard top-left corner (the Blade-Runner/Transcendence move from the columns),
// a top-left specular catch (liquid glass), a whisper of depth shadow so it
// floats off the field, and — when a run is live — a status glow. No border:
// the plane separates by light, the spine carries meaning.
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
      className={`relative rounded-[13px] rounded-tl-[3px] bg-surface-overlay backdrop-blur-xl transition-[box-shadow,transform] duration-300 ${
        dragging
          ? 'scale-[1.015] shadow-[0_22px_55px_-20px_rgba(0,0,0,0.55)]'
          : 'shadow-[0_10px_30px_-22px_rgba(0,0,0,0.5)]'
      } ${dim ? 'opacity-70' : ''}`}
    >
      {/* Top-left specular — the glass catches light at the hard corner. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 rounded-[inherit]"
        style={{ background: 'linear-gradient(135deg, rgba(255,255,255,0.4), transparent 46%)' }}
      />
      {/* Status glow: a 1px inner ring + an outward bloom in the run's tone.
          Breathing for a live run; steady for a state that wants attention.
          Painted over the fill but under the content (which clears the edges
          via padding), so the ring reads at the rim and the bloom spills past
          the unclipped plane. */}
      {glow && (
        <motion.div
          aria-hidden
          className="pointer-events-none absolute inset-0 rounded-[inherit]"
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
      {children}
    </div>
  )
}

// Spine is the status line: a 2px vertical bar down the left edge, faded to
// transparent at both ends so it never collides with the rounded corners and
// reads as a guiding line rather than a hard rule.
export function Spine({ tone, opacity = 0.9 }: { tone: Tone; opacity?: number }) {
  return (
    <div
      aria-hidden
      className="pointer-events-none absolute bottom-0 left-0 top-0 w-[2px]"
      style={{
        background: `linear-gradient(to bottom, transparent, ${TONE_VAR[tone]} 14%, ${TONE_VAR[tone]} 86%, transparent)`,
        opacity,
      }}
    />
  )
}

// SpineSteps overlays a chain run's steps as notches riding the spine — done,
// active (pulsing), failed, the current step (ringed), and pending — replacing
// the old horizontal circles-and-line rail. They sit in the header zone near
// the title, so "this card is step N of a chain" lives in the spine instead of
// a separate widget.
export function SpineSteps({ steps }: { steps: StepState[] }) {
  const reduce = !!useReducedMotion()
  return (
    <div aria-hidden className="pointer-events-none absolute left-0 top-0 w-3">
      {steps.map((state, i) => {
        const top = 17 + i * 13
        const color = STEP_VAR[state]
        return (
          <span key={i} className="absolute" style={{ top, left: -2 }}>
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

// SourceTag is the source marker, detuned to a monochrome uppercase glyph (no
// blue Jira pill) so it sits quietly in the warm field next to the event tag.
export function SourceTag({ task }: { task: Task }) {
  const isGitHub = task.source === 'github'
  const label = isGitHub ? (task.entity_kind === 'pr' ? 'PR' : 'GH') : 'JIRA'
  return (
    <span className="text-[10px] font-semibold uppercase tracking-[0.09em] text-text-tertiary/70">
      {label}
    </span>
  )
}

// AttentionRow is the canonical "this needs you" row — the amber waiting-for-
// response pattern, generalized. A toned kicker + a one-line message + a
// trailing verb with an arrow. Used for awaiting_input (Respond) and
// pending_approval (Review / Open PR), so every "your move" moment looks the
// same instead of three different treatments.
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
      className="flex w-full items-center justify-between gap-3 rounded-xl px-3 py-2 text-left transition-colors"
      style={{ background: `color-mix(in srgb, ${TONE_VAR[tone]} 10%, transparent)` }}
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
