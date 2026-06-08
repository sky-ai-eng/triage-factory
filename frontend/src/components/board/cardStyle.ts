import type { AgentRun } from '../../types'
import { isActiveRun } from '../../lib/runStatus'

// cardStyle — the design tokens + status→tone logic for the board's lit-plane
// cards. Pure module (no JSX) so the shared chrome components live in
// cardChrome.tsx and Fast Refresh stays happy.
//
// The thesis: cards are borderless frosted planes that separate from the field
// by depth and light, not by a frame. Status is carried by LIGHT — a single
// vertical "spine" line on the left edge, colored by state, plus (for live work)
// a breathing glow. This is the same grammar as the columns' rust L-bracket,
// pulled onto the cards. The column ambient glow was retired when this landed:
// the work-is-live signal now lives on the card that's actually working, not
// the lane around it.

// Tone is the card's semantic color vocabulary, mapped to theme tokens so it
// tracks light/dark automatically. `rust` is the resting accent (a plain task);
// the rest encode run state.
export type Tone = 'rust' | 'active' | 'good' | 'attention' | 'problem' | 'neutral'

export const TONE_VAR: Record<Tone, string> = {
  rust: 'var(--color-accent)',
  active: 'var(--color-delegate)',
  good: 'var(--color-claim)',
  attention: 'var(--color-snooze)',
  problem: 'var(--color-dismiss)',
  neutral: 'var(--color-text-tertiary)',
}

export const TONE_TEXT: Record<Tone, string> = {
  rust: 'text-accent',
  active: 'text-delegate',
  good: 'text-claim',
  attention: 'text-snooze',
  problem: 'text-dismiss',
  neutral: 'text-text-tertiary',
}

export interface Glow {
  tone: Tone
  // Breathing = an actively-turning run (the lane is alive). Steady = a state
  // that wants attention but isn't moving (waiting on you, or it failed).
  breathing: boolean
}

// runGlow decides whether the card itself lights up. Only a LIVE run glows — a
// breathing "work is alive here" bloom. Everything else returns null: a steady
// colored glow on a settled/cancelled/waiting card just reads as a tinted
// drop-shadow under the card (and those states already announce themselves via
// the attention row / result verdict).
export function runGlow(run: AgentRun): Glow | null {
  if (isActiveRun(run)) return { tone: 'active', breathing: true }
  return null
}

// StepState is a chain step's lifecycle, rendered as a notch on the spine.
export type StepState = 'done' | 'active' | 'failed' | 'current' | 'pending'

export const STEP_VAR: Record<StepState, string> = {
  done: 'var(--color-claim)',
  active: 'var(--color-delegate)',
  failed: 'var(--color-dismiss)',
  current: 'var(--color-accent)',
  pending: 'var(--color-text-tertiary)',
}

// formatAge — coarse "just now / 4h / 3d" for the card's age caption.
export function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

export function compactNum(n: number): string {
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
}
