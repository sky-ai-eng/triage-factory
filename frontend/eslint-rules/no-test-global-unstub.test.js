// @vitest-environment node
//
// Node, not the suite's default jsdom: the rule keys on the file's path, and
// RuleTester's `filename` option is only meaningful outside the browser
// environment.
import { RuleTester } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, it } from 'vitest'
import rule from './no-test-global-unstub.js'

// RuleTester runs its assertions inline when no global describe/it exists
// (vitest runs with globals off), so each run() call throws inside its own it().
const ruleTester = new RuleTester({
  languageOptions: {
    parser: tseslint.parser,
    ecmaVersion: 2022,
    sourceType: 'module',
  },
})

const TEST = '/repo/frontend/src/pages/team/SourcePages.test.tsx'
const SETUP = '/repo/frontend/src/test/setup.ts'
const APP = '/repo/frontend/src/pages/team/SourcePages.tsx'

describe('no-test-global-unstub', () => {
  it('leaves stubbing, the setup file, and non-test files alone', () => {
    ruleTester.run('no-test-global-unstub', rule, {
      valid: [
        // Stubbing is the point; only the restore is owned elsewhere.
        { code: "vi.stubGlobal('fetch', vi.fn())", filename: TEST },
        // The other teardown verbs are a file's own business.
        { code: 'afterEach(() => vi.restoreAllMocks())', filename: TEST },
        { code: 'afterEach(() => vi.useRealTimers())', filename: TEST },
        // The one file that may call it — it is the hook that owns the order.
        { code: 'afterEach(() => { cleanup(); vi.unstubAllGlobals() })', filename: SETUP },
        // Not a test file at all.
        { code: 'vi.unstubAllGlobals()', filename: APP },
        // A same-named method on something that is not vitest's `vi`.
        { code: 'sandbox.unstubAllGlobals()', filename: TEST },
      ],
      invalid: [],
    })
  })

  it('rejects every shape of a per-file restore hook', () => {
    ruleTester.run('no-test-global-unstub', rule, {
      valid: [],
      invalid: [
        {
          code: 'afterEach(() => vi.unstubAllGlobals())',
          filename: TEST,
          errors: [{ messageId: 'ownHook' }],
        },
        {
          code: 'afterEach(() => {\n  vi.unstubAllGlobals()\n})',
          filename: TEST,
          errors: [{ messageId: 'ownHook' }],
        },
        // Alongside other teardown, which is how it usually appears.
        {
          code: 'afterEach(() => {\n  vi.useRealTimers()\n  vi.unstubAllGlobals()\n})',
          filename: TEST,
          errors: [{ messageId: 'ownHook' }],
        },
        // Anywhere else in the file is the same hazard with worse timing.
        {
          code: 'afterAll(() => vi.unstubAllGlobals())',
          filename: TEST,
          errors: [{ messageId: 'ownHook' }],
        },
        {
          code: "it('x', () => { vi.unstubAllGlobals() })",
          filename: TEST,
          errors: [{ messageId: 'ownHook' }],
        },
      ],
    })
  })
})
