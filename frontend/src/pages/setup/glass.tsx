// glass.tsx — the Liquid Glass components for the setup flow (SKY-457): the
// ambient backdrop the flush flow sits on. The shared material class strings +
// motion easing live in glassStyle.ts (a non-component module, so this stays
// component-only for react-refresh).
//
// Material is built from the existing theme tokens: --color-surface-overlay
// (the translucent glass fill, already light/dark-aware) over --color-border-
// glass edges. We paint the ambient orbs with the --color-accent / --color-
// snooze tokens directly via inline radial-gradients rather than bg-gradient-*
// utilities — index.css strips those in dark mode, and we want the warm glow in
// both themes.

import { motion, useReducedMotion } from 'motion/react'

// GlassBackdrop is the fixed, full-bleed ambient layer behind the wizard: two
// large, slowly drifting warm orbs (clay + faint gold) over the surface, so the
// frosted panels have something to refract — glass over a flat fill reads as
// mud. Honors prefers-reduced-motion by holding the orbs still.
export function GlassBackdrop() {
  const reduce = useReducedMotion()
  const drift = (x: number[], y: number[], duration: number) =>
    reduce
      ? {}
      : {
          animate: { x, y },
          transition: {
            duration,
            repeat: Infinity,
            repeatType: 'mirror' as const,
            ease: 'easeInOut' as const,
          },
        }
  return (
    <div aria-hidden className="pointer-events-none fixed inset-0 -z-10 overflow-hidden bg-surface">
      <motion.div
        className="absolute -left-40 -top-48 h-[46rem] w-[46rem] rounded-full blur-[150px]"
        style={{
          background: 'radial-gradient(circle, var(--color-accent) 0%, transparent 70%)',
          opacity: 0.16,
        }}
        {...drift([0, 44, 0], [0, 32, 0], 30)}
      />
      <motion.div
        className="absolute -bottom-56 -right-32 h-[42rem] w-[42rem] rounded-full blur-[160px]"
        style={{
          background: 'radial-gradient(circle, var(--color-snooze) 0%, transparent 70%)',
          opacity: 0.1,
        }}
        {...drift([0, -38, 0], [0, -30, 0], 36)}
      />
    </div>
  )
}
