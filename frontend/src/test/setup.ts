// Vitest global setup (referenced from vite.config.ts `test.setupFiles`).
//
//  - jest-dom/vitest registers the DOM matchers (toBeInTheDocument, …) onto
//    vitest's expect AND augments its types, so test files get them without
//    per-file imports.
//  - globals are off (see vite.config.ts), so React Testing Library's
//    automatic afterEach cleanup doesn't self-register — we wire it here so
//    each test starts with a fresh DOM.
import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

// jsdom ships no ResizeObserver; Radix primitives (Popover arrow sizing, etc.)
// touch it on mount. A no-op stub is enough for the tests, which assert on
// text/roles rather than measured geometry.
if (!('ResizeObserver' in globalThis)) {
  class ResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver
}

// jsdom implements no layout, so Element.scrollIntoView is absent; list
// components call it to keep a keyboard-moved selection in view. A no-op stub
// is enough for tests that assert on text/roles rather than scroll position.
// Guarded on Element itself, not just the method: this setup file also runs
// for the eslint-rules suites, which have no DOM at all.
if (typeof Element !== 'undefined' && !Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {}
}

afterEach(() => {
  cleanup()
})
