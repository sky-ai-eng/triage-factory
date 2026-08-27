// no-test-global-unstub — fail the lint when a test file restores the globals
// it stubbed in a hook of its own.
//
// `vi.unstubAllGlobals()` has to run AFTER the tree is unmounted, and a test
// file cannot express that. Vitest's default `sequence.hooks: 'stack'` runs
// afterEach hooks in reverse registration order, and src/test/setup.ts — which
// owns RTL's `cleanup()` — is always registered first because setup files load
// before the file under test. So a file's own afterEach runs BEFORE the
// unmount, and between the two the still-mounted tree holds the real `fetch`:
// one scheduler tick is enough for an in-flight effect to fire against it,
// which is a live request from a test that is over, an unhandled rejection,
// or a call landing on a mock nobody is looking at any more.
//
// The order that works is a single hook body, and src/test/setup.ts has it —
// `cleanup()` then `vi.unstubAllGlobals()`, top to bottom, immune to hook
// sequencing. It runs for every test in every file, so a per-file hook is
// redundant as well as wrong: the stub is already restored, one hook later.
//
// Scoped to test files because `vi` outside one is not vitest's runner API.

const TEST_FILE = /\.test\.[cm]?[jt]sx?$/

const rule = {
  meta: {
    type: 'problem',
    docs: {
      description:
        'Disallow vi.unstubAllGlobals() in test files; src/test/setup.ts restores the globals after the unmount.',
    },
    schema: [],
    messages: {
      ownHook:
        'Remove this vi.unstubAllGlobals() — src/test/setup.ts already runs it after RTL cleanup(), for every test. A hook here runs BEFORE that unmount (vitest reverses afterEach order), leaving the still-mounted tree a window in which its effects fire against the real global.',
    },
  },

  create(context) {
    const filename = context.filename ?? context.getFilename()
    if (!TEST_FILE.test(filename)) return {}

    return {
      CallExpression(node) {
        const callee = node.callee
        if (
          callee.type === 'MemberExpression' &&
          !callee.computed &&
          callee.object.type === 'Identifier' &&
          callee.object.name === 'vi' &&
          callee.property.type === 'Identifier' &&
          callee.property.name === 'unstubAllGlobals'
        ) {
          context.report({ node, messageId: 'ownHook' })
        }
      },
    }
  },
}

export default rule
