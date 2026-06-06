// Liquid Glass material tokens for the setup flow (SKY-457) — shared class
// strings + motion easing, kept in a non-component module so the component file
// (glass.tsx) stays component-only (react-refresh). Built from the existing
// theme tokens: --color-surface-overlay (the translucent glass fill, already
// light/dark-aware) over --color-border-glass edges.

import type { Transition } from 'motion/react'

// glassSurface is the active step's frosted panel: translucent fill + glass
// edge + a real backdrop blur so the ambient orbs refract through it. Pair with
// <SpecularEdge/> (glass.tsx) for the light-catching top highlight.
export const glassSurface =
  'relative overflow-hidden rounded-[1.75rem] border border-[var(--color-border-glass)] ' +
  'bg-[var(--color-surface-overlay)] shadow-[0_24px_70px_-30px_rgba(0,0,0,0.32)] backdrop-blur-2xl'

// glassSliver is a completed step receded above the active one: thinner, more
// translucent, a lighter blur (perf — many can stack) than the active surface.
export const glassSliver =
  'rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/70 ' +
  'backdrop-blur-md transition-colors'

// glassField is the soft inset-glass input shared by the URL steps.
export const glassField =
  'w-full rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 ' +
  'px-4 py-3 text-[14px] text-text-primary placeholder:text-text-tertiary backdrop-blur-md outline-none ' +
  'transition-[border-color,background-color,box-shadow] focus:border-accent/40 ' +
  'focus:bg-[var(--color-surface-overlay)] focus:shadow-[0_0_0_4px_var(--color-accent-soft)]'

// bodyEase is the active step body's reveal easing — a soft, confident
// ease-out (de-blur + rise). Reduced-motion callers swap an instant transition.
export const bodyEase: Transition = { duration: 0.34, ease: [0.22, 1, 0.36, 1] }
