import { useLiveText } from './useLiveText'

// The leaf that makes useLiveText's scoping automatic: mount one of these
// where the ticking text goes and the once-a-second re-render — which only
// happens when the text visibly changes — is confined to a fragment, never
// the surface around it. Renders bare text with no element of its own, so it
// inherits whatever its host styles (a run row's age cell, a card caption).
//
//   <LiveText compute={(now) => compactAge(startedAt, now)} />
//
// Calling useLiveText directly from a page component works but subscribes the
// whole page; prefer this leaf unless the caller is already leaf-sized.

export function LiveText({ compute }: { compute: (now: number) => string }) {
  return <>{useLiveText(compute)}</>
}

export default LiveText
