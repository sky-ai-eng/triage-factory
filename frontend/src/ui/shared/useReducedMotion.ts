// Reduced motion, for components whose motion is JS-timed.
//
// tokens/motion.css blankets CSS animation and transition under
// prefers-reduced-motion, but a sequence driven by setTimeout or
// requestAnimationFrame is invisible to a media query — a four-beat attention
// sequence or a hold gesture runs in full unless it reads the preference
// itself.
//
// Each component that imports this states its reduced-motion answer in its own
// .md. The rule across the system: the END STATE arrives immediately and the
// beats are skipped. Motion here always answers "what just happened?", and the
// answer is the end state — so removing the motion removes the explanation,
// never the information.
//
// Lives in src/ui/shared/ rather than src/ui/lib/ on purpose: `lib` is already
// the app's own directory, and a ui-internal folder sharing that name makes
// every import-boundary glob ambiguous.

import { useEffect, useState } from 'react'

const QUERY = '(prefers-reduced-motion: reduce)'

/** Read the preference once — for imperative code that cannot hold state. */
export function prefersReducedMotion(): boolean {
  if (typeof window === 'undefined' || !window.matchMedia) return false
  return window.matchMedia(QUERY).matches
}

/** Subscribe to the preference. Re-renders if the user changes it mid-session. */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(prefersReducedMotion)
  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return
    const mq = window.matchMedia(QUERY)
    const onChange = () => setReduced(mq.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  return reduced
}

export default useReducedMotion
