/* eslint-disable react-refresh/only-export-components --
   A context+hook pair, matching AuthContext and OrgContext. The provider and
   useImmersive are two halves of one contract — a page asking for the screen
   and the frame answering — and splitting them across files would trade one
   import for another while making the pair harder to read. */
import { createContext, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'

// The channel a page uses to ask the frame to get out of the way.
//
// Cinematic mode is the factory's idea, but the rail and the header are the
// shell's, and neither should have to know about the other: a page reaching
// into the frame's internals, or the frame special-casing a route, are the two
// ways this normally gets built and both age badly. So the page states an
// intent — "I want the whole screen" — and the frame decides what that means.
//
// This lives in the app layer, not in src/ui. The design-system Shell takes an
// `immersive` prop and nothing more; wiring that prop to whichever page asked
// for it is the adapter's job, and the boundary lint would reject a ui
// component importing this.

type ChromeState = {
  immersive: boolean
  request: (on: boolean) => void
}

const ChromeContext = createContext<ChromeState | null>(null)

export function ChromeProvider({ children }: { children: (immersive: boolean) => ReactNode }) {
  const [immersive, setImmersive] = useState(false)
  return (
    <ChromeContext.Provider value={{ immersive, request: setImmersive }}>
      {children(immersive)}
    </ChromeContext.Provider>
  )
}

/**
 * Ask the frame to fade while `active` holds, and give it back on unmount.
 *
 * Declarative on purpose. An enter/exit pair leaves the chrome hidden forever
 * if the page unmounts between them — navigating away mid-cinematic, or a
 * render that throws — and the resulting app has no navigation and no way to
 * explain why.
 */
export function useImmersive(active: boolean): void {
  const ctx = useContext(ChromeContext)
  const request = ctx?.request
  useEffect(() => {
    if (!request) return
    request(active)
    return () => request(false)
  }, [active, request])
}
