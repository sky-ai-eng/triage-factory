// glass.tsx — the Liquid Glass components for the setup flow: the
// ambient backdrop the flush flow sits on. The shared material class strings +
// motion easing live in glassStyle.ts (a non-component module, so this stays
// component-only for react-refresh).
//
// Material is built from the existing theme tokens: --color-raised
// (the translucent glass fill, already light/dark-aware) over --color-border-
// glass edges. We paint the ambient orbs with the --color-warm / --color-
// snooze tokens directly via inline radial-gradients rather than bg-gradient-*
// utilities — index.css strips those in dark mode, and we want the warm glow in
// both themes.

// GlassBackdrop is the fixed, full-bleed ambient layer behind the wizard: two
// large warm orbs (clay + faint gold) over the surface, so the frosted panels
// have something to refract — glass over a flat fill reads as mud.
//
// The orbs are deliberately STATIC. They used to drift on infinite
// framer-motion loops, but a moving layer underneath the app's many
// backdrop-blur surfaces (every board card, the run station panel) forces the
// GPU to re-sample and re-blur those backdrops every frame, forever — a
// constant battery drain on pages that are otherwise idle. At 7–16% opacity
// behind 150px+ of blur the drift was imperceptible anyway; holding the orbs
// still lets the browser compute each backdrop-filter once and composite the
// cached result.
export function GlassBackdrop() {
  return (
    <div aria-hidden className="pointer-events-none fixed inset-0 -z-10 overflow-hidden bg-ground">
      <div
        className="absolute -left-40 -top-48 h-[46rem] w-[46rem] rounded-full blur-[150px]"
        style={{
          background: 'radial-gradient(circle, var(--color-warm) 0%, transparent 70%)',
          opacity: 0.16,
        }}
      />
      <div
        className="absolute -bottom-56 -right-32 h-[42rem] w-[42rem] rounded-full blur-[160px]"
        style={{
          background: 'radial-gradient(circle, var(--color-snooze) 0%, transparent 70%)',
          opacity: 0.1,
        }}
      />
      {/* A cool counterpoint to the two warm orbs — keeps the field from going
          one-note amber and gives the glass a warm→cool gradient to refract
          (the Halo/Transcendence depth). Very low opacity. */}
      <div
        className="absolute left-1/2 top-1/4 h-[38rem] w-[38rem] -translate-x-1/2 rounded-full blur-[170px]"
        style={{
          background: 'radial-gradient(circle, rgba(99,130,175,1) 0%, transparent 70%)',
          opacity: 0.07,
        }}
      />
    </div>
  )
}
