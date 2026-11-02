// WAAPI takes an easing as a string, not a var(), so a JS-timed animation
// reads its curve off the token layer when it starts rather than restating the
// cubic-bezier. Undefined, which WAAPI reads as linear, only where the tokens
// are not loaded: a test environment.
export function ease(el: Element, token: '--ease-default' | '--ease-fold'): string | undefined {
  return getComputedStyle(el).getPropertyValue(token).trim() || undefined
}
