import { useEffect, useState } from 'react'

// Whether the viewer is somewhere else: the tab hidden or the window blurred.
// A clock that measures how long something has been in front of someone (a
// toast's lifetime, a notification's arrival) holds while this is true, so
// what lands while they are elsewhere waits for them.

function attending(): boolean {
  if (typeof document === 'undefined') return true
  return document.visibilityState === 'visible' && document.hasFocus()
}

export function useAway(): boolean {
  const [away, setAway] = useState(() => !attending())
  useEffect(() => {
    const sync = () => setAway(!attending())
    const blur = () => setAway(true)
    document.addEventListener('visibilitychange', sync)
    window.addEventListener('focus', sync)
    window.addEventListener('blur', blur)
    return () => {
      document.removeEventListener('visibilitychange', sync)
      window.removeEventListener('focus', sync)
      window.removeEventListener('blur', blur)
    }
  }, [])
  return away
}

export default useAway
