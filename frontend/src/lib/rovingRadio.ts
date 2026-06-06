// Keyboard navigation for a WAI-ARIA radiogroup rendered as a row of cards.
// Shared by the model-tier selector and the trackers picker so both get the
// same arrow-key behaviour from one definition rather than two parallel
// handlers. The components own their own rendering + roving tabIndex; this
// owns only the "which index does an arrow key move to" rule.

const NEXT_KEYS = new Set(['ArrowRight', 'ArrowDown'])
const PREV_KEYS = new Set(['ArrowLeft', 'ArrowUp'])

// nextRadioIndex returns the index selection+focus should move to for an arrow
// key — the nearest enabled option in the key's direction, wrapping — or null
// when the key isn't an arrow (or no other option is enabled). `isEnabled`
// lets a group skip disabled cards (e.g. the "coming soon" tracker).
export function nextRadioIndex(
  key: string,
  current: number,
  count: number,
  isEnabled: (i: number) => boolean = () => true,
): number | null {
  const forward = NEXT_KEYS.has(key)
  const backward = PREV_KEYS.has(key)
  if ((!forward && !backward) || count === 0) return null
  const delta = forward ? 1 : -1
  // No-selection (current = -1) is unreachable via both current callers — each
  // default-selects an item — so this is just a defensive origin: walk from 0,
  // and since the `i !== current` guard compares against -1 (not start), every
  // index including 0 is eligible. The exact landing in that case is incidental
  // and unreached; documented only so a future caller without a default knows
  // the start is a fallback, not a meaningful "selected" position.
  const start = current < 0 ? 0 : current
  // Walk one full loop in the chosen direction, returning the first enabled
  // landing spot. Bounded by count so an all-disabled group can't spin.
  for (let step = 1; step <= count; step++) {
    const i = (((start + delta * step) % count) + count) % count
    if (i !== current && isEnabled(i)) return i
  }
  return null
}
