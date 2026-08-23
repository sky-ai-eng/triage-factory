/* eslint-disable react-refresh/only-export-components --
   A context+hook pair, matching AuthContext and OrgContext. The provider and
   its hooks are two halves of one contract — a page telling the frame something
   and the frame answering — and splitting them across files would trade one
   import for another while making the pair harder to read. */
import { createContext, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import type { Crumb } from '../ui/shell/Shell'

// The channel a page uses to talk to the frame around it.
//
// Two things travel over it, and they are the same kind of thing: a page
// stating an intent that only the frame can act on.
//
//   IMMERSIVE — "I want the whole screen." Cinematic mode is the factory's
//   idea, but the rail and the header are the shell's, and neither should have
//   to know about the other.
//
//   HEADER — what fills the 55px band. The band belongs to the frame; the
//   crumbs, title and actions in it belong to the page. Without this the
//   adapter has to guess a title from the route, which is the honest fallback
//   but only until a page has something better to say.
//
// Both live in the app layer, not in src/ui. The design-system Shell takes
// plain props and nothing more; wiring them to whichever page asked is the
// adapter's job, and the boundary lint would reject a ui component importing
// this.

export type PageHeader = {
  crumbs?: Crumb[]
  title?: string
  subtitle?: string
  actions?: ReactNode
  /** Passing this makes the title an inline field. */
  onTitleSave?: (name: string) => void
}

type ChromeState = {
  immersive: boolean
  requestImmersive: (on: boolean) => void
  header: PageHeader | null
  setHeader: (h: PageHeader | null) => void
}

const ChromeContext = createContext<ChromeState | null>(null)

export function ChromeProvider({
  children,
}: {
  children: (state: { immersive: boolean; header: PageHeader | null }) => ReactNode
}) {
  const [immersive, setImmersive] = useState(false)
  const [header, setHeader] = useState<PageHeader | null>(null)
  const value = useMemo(
    () => ({ immersive, requestImmersive: setImmersive, header, setHeader }),
    [immersive, header],
  )
  return (
    <ChromeContext.Provider value={value}>{children({ immersive, header })}</ChromeContext.Provider>
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
  const request = ctx?.requestImmersive
  useEffect(() => {
    if (!request) return
    request(active)
    return () => request(false)
  }, [active, request])
}

/**
 * Fill the frame's header band while this page is mounted, and hand it back on
 * unmount so the next page does not inherit the last one's title.
 *
 * `header` must be stable — memoize it, or the effect re-runs every render.
 */
export function usePageHeader(header: PageHeader | null): void {
  const ctx = useContext(ChromeContext)
  const set = ctx?.setHeader
  useEffect(() => {
    if (!set) return
    set(header)
    return () => set(null)
  }, [header, set])
}
