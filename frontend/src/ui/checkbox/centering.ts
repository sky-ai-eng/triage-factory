// The measurement TICK_DX/TICK_DY in Checkbox.tsx were tuned against, kept so a
// future edit to the path can be checked rather than eyeballed. Its own module
// because it is a tuning instrument, not part of the control — and a module
// exporting both a component and a helper is what react-refresh objects to.
//
// checkbox.card.html in the design bundle draws a 118px box against a crosshair
// for the same purpose.

/**
 * Reports how far the tick's inked area sits from the square's centre, in
 * viewBox units — the measurement TICK_DX/TICK_DY were tuned against, kept so
 * a future edit to the path can be checked rather than eyeballed.
 * Returns null until a checked box is on the page.
 */
export function checkboxCentering(
  root?: Element | Document | null,
): { dx: number; dy: number } | null {
  const scope = root || document
  const svg = scope.querySelector('span[role="checkbox"] svg')
  if (!svg) return null
  const path = svg.querySelector('path')
  if (!path) return null
  const box = svg.getBoundingClientRect()
  const ink = path.getBoundingClientRect()
  const k = 12 / box.width
  return {
    dx: +(((ink.left + ink.right) / 2 - (box.left + box.right) / 2) * k).toFixed(3),
    dy: +(((ink.top + ink.bottom) / 2 - (box.top + box.bottom) / 2) * k).toFixed(3),
  }
}
