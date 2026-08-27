// Vitest global setup (referenced from vite.config.ts `test.setupFiles`).
//
//  - jest-dom/vitest registers the DOM matchers (toBeInTheDocument, …) onto
//    vitest's expect AND augments its types, so test files get them without
//    per-file imports.
//  - globals are off (see vite.config.ts), so React Testing Library's
//    automatic afterEach cleanup doesn't self-register — we wire it here so
//    each test starts with a fresh DOM.
import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
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

// Unmount, then restore the globals — and both in ONE hook body, because the
// order is the whole point and hook registration cannot express it. Vitest's
// default `sequence.hooks: 'stack'` runs afterEach hooks in reverse
// registration order, and a setup file is always registered first, so a test
// file's own hook runs BEFORE this one. A file that restored `fetch` there
// would hand its still-mounted tree a window — one scheduler tick is enough —
// in which an in-flight effect fires against the real global. A body is read
// top to bottom whatever the hook order is, which is why the unstub lives here
// rather than in a second afterEach, and why test files do not carry one of
// their own (eslint-rules/no-test-global-unstub.js holds that).
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})
