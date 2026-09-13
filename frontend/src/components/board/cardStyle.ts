// cardStyle — the semantic colour vocabulary the run station and the action
// and artifact rows share, mapped to theme tokens so it tracks light/dark
// automatically. Pure module (no JSX), so the components that read it
// (cardChrome.tsx and the rows) keep Fast Refresh happy.

// Tone is the semantic colour vocabulary. `rust` is the resting accent; the
// rest encode a conversation's or an artifact's state.
export type Tone = 'rust' | 'active' | 'good' | 'attention' | 'problem' | 'neutral'

export const TONE_VAR: Record<Tone, string> = {
  rust: 'var(--color-warm)',
  active: 'var(--color-cool)',
  good: 'var(--color-ink-1)',
  attention: 'var(--color-warm)',
  problem: 'var(--color-alarm)',
  neutral: 'var(--color-ink-3)',
}

export const TONE_TEXT: Record<Tone, string> = {
  rust: 'text-warm',
  active: 'text-cool',
  good: 'text-ink-1',
  attention: 'text-warm',
  problem: 'text-alarm',
  neutral: 'text-ink-3',
}

// StepState is a chain step's lifecycle, as the run station's chain bar
// draws it.
export type StepState = 'done' | 'active' | 'failed' | 'current' | 'pending'

export const STEP_VAR: Record<StepState, string> = {
  done: 'var(--color-ink-2)',
  active: 'var(--color-cool)',
  failed: 'var(--color-alarm)',
  current: 'var(--color-warm)',
  pending: 'var(--color-ink-3)',
}
