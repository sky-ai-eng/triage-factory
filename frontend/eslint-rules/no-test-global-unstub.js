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
//
// The call is matched on the property it names rather than on how it is
// spelled, so dot and bracket access are one finding — `vi.unstubAllGlobals()`
// and `vi['unstubAllGlobals']()` are the same call, and optional chaining in
// either is a third spelling that changes nothing. What it cannot see is a
// binding pulled off `vi` first (`const { unstubAllGlobals } = vi`), which is
// where a rule guarding a convention stops: nothing in the suite is written
// that way, and a reader who takes that route has already read this file.

const TEST_FILE = /\.test\.[cm]?[jt]sx?$/

/**
 * The property a member expression names, when it can be read statically — the
 * identifier after a dot, or a plain string key in brackets.
 */
function staticPropertyName(node) {
  const key = node.property
  if (!node.computed) return key.type === 'Identifier' ? key.name : null
  if (key.type === 'Literal') return typeof key.value === 'string' ? key.value : null
  if (key.type === 'TemplateLiteral' && key.expressions.length === 0) {
    return key.quasis[0].value.cooked
  }
  return null
}

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
        if (callee.type !== 'MemberExpression') return
        if (callee.object.type !== 'Identifier' || callee.object.name !== 'vi') return
        if (staticPropertyName(callee) !== 'unstubAllGlobals') return
        context.report({ node, messageId: 'ownHook' })
      },
    }
  },
}

export default rule
