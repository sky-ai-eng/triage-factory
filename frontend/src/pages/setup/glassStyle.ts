// Liquid Glass material tokens for the setup flow — shared class
// strings + motion easing, kept in a non-component module so the component file
// (glass.tsx) stays component-only (react-refresh). Built from the existing
// theme tokens: --color-surface-overlay (the translucent glass fill, already
// light/dark-aware) over --color-border-glass edges.

import type { Transition } from 'motion/react'

// No per-step card/surface: the flow is flush on the ambient backdrop, content
// flowing item to item (completed steps recede into thin flush rows, the active
// step is content in space). The frosted field material is glassInputClass in
// settings/primitives (shared with the bare field groups), not here.

// bodyEase is the active step body's reveal easing — a soft, confident
// ease-out (de-blur + rise). Reduced-motion callers swap an instant transition.
// The duration is exported alongside because the wizard has to know how long
// the flow keeps moving after a step change: it re-anchors the action row every
// frame for exactly that long. Kept as one constant so the two can't drift.
export const bodyDurationMs = 340
export const bodyEase: Transition = {
  duration: bodyDurationMs / 1000,
  ease: [0.22, 1, 0.36, 1],
}
