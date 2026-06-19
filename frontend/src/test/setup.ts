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

afterEach(() => {
  cleanup()
})
