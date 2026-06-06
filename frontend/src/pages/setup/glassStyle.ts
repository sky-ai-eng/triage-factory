// Liquid Glass material tokens for the setup flow (SKY-457) — shared class
// strings + motion easing, kept in a non-component module so the component file
// (glass.tsx) stays component-only (react-refresh). Built from the existing
// theme tokens: --color-surface-overlay (the translucent glass fill, already
// light/dark-aware) over --color-border-glass edges.

import type { Transition } from 'motion/react'

// No per-step card/surface: the flow is flush on the ambient backdrop, content
// flowing item to item (completed steps recede into thin flush rows, the active
// step is content in space). The only frosted element is the input field —
// glass belongs on the affordance you touch, not as a container around content.

// glassField is the soft inset-glass input shared by the URL steps.
export const glassField =
  'w-full rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 ' +
  'px-4 py-3 text-[14px] text-text-primary placeholder:text-text-tertiary backdrop-blur-md outline-none ' +
  'transition-[border-color,background-color,box-shadow] focus:border-accent/40 ' +
  'focus:bg-[var(--color-surface-overlay)] focus:shadow-[0_0_0_4px_var(--color-accent-soft)]'

// bodyEase is the active step body's reveal easing — a soft, confident
// ease-out (de-blur + rise). Reduced-motion callers swap an instant transition.
export const bodyEase: Transition = { duration: 0.34, ease: [0.22, 1, 0.36, 1] }
